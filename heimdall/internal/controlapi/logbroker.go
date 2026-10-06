package controlapi

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/heimdall-dev/heimdall/internal/controlapi/gen"
	"github.com/heimdall-dev/heimdall/internal/domain"
	"github.com/redis/go-redis/v9"
)

// ErrLogLimit indicates that the bounded ephemeral queue is full.
var ErrLogLimit = errors.New("ephemeral log queue limit exceeded")

// LogBroker carries short redacted log tails across API replicas. It must expire
// every record within the supplied TTL and must never persist application logs.
type LogBroker interface {
	Create(context.Context, LogRecord, time.Duration) error
	Get(context.Context, string) (LogRecord, error)
	Pending(context.Context, string, string) ([]LogRecord, error)
	Complete(context.Context, LogRecord) error
}

// MemoryLogBroker is a bounded, process-local broker for development.
type MemoryLogBroker struct {
	mu      sync.Mutex
	records map[string]LogRecord
}

func NewMemoryLogBroker() *MemoryLogBroker {
	return &MemoryLogBroker{records: map[string]LogRecord{}}
}

func (b *MemoryLogBroker) expire() {
	for id, r := range b.records {
		if !r.Request.ExpiresAt.After(time.Now()) {
			delete(b.records, id)
		}
	}
}

func (b *MemoryLogBroker) Create(_ context.Context, r LogRecord, ttl time.Duration) error {
	if ttl <= 0 || ttl > 2*time.Minute || r.Request.ExpiresAt.After(time.Now().Add(ttl+time.Second)) {
		return domain.ErrForbidden
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.expire()
	if _, exists := b.records[r.Request.Id]; exists {
		return domain.ErrConflict
	}
	tenant, environment := 0, 0
	for _, existing := range b.records {
		if existing.TenantID == r.TenantID {
			tenant++
			if existing.Request.EnvironmentID == r.Request.EnvironmentID {
				environment++
			}
		}
	}
	if len(b.records) >= 1024 || tenant >= 64 || environment >= 4 {
		return ErrLogLimit
	}
	b.records[r.Request.Id] = r
	return nil
}

func (b *MemoryLogBroker) Get(_ context.Context, id string) (LogRecord, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.expire()
	r, exists := b.records[id]
	if !exists {
		return r, domain.ErrNotFound
	}
	return r, nil
}

func (b *MemoryLogBroker) Pending(_ context.Context, tenant, cluster string) ([]LogRecord, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.expire()
	out := []LogRecord{}
	for _, r := range b.records {
		if r.TenantID == tenant && r.ClusterID == cluster && r.Request.State == gen.LogRequestStatePending {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Request.Id < out[j].Request.Id })
	return out, nil
}

func (b *MemoryLogBroker) Complete(_ context.Context, r LogRecord) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.expire()
	current, exists := b.records[r.Request.Id]
	if !exists {
		return domain.ErrNotFound
	}
	if current.TenantID != r.TenantID || current.ClusterID != r.ClusterID || current.ActorID != r.ActorID || current.Request.EnvironmentID != r.Request.EnvironmentID || current.Request.Generation != r.Request.Generation {
		return domain.ErrStaleGeneration
	}
	if current.Request.State == gen.LogRequestStatePending {
		r.Request.ExpiresAt = current.Request.ExpiresAt
		b.records[r.Request.Id] = r
	}
	return nil
}

// RedisLogBroker uses volatile Redis storage with TTLs. Redis must be operated
// with persistence disabled for this queue; no log bytes enter PostgreSQL.
// One hash slot keeps bounds and completion atomic even in Redis Cluster.
type RedisLogBroker struct{ client redis.UniversalClient }

func NewRedisLogBroker(client redis.UniversalClient) *RedisLogBroker {
	return &RedisLogBroker{client: client}
}

// Ready refuses a log broker whose bytes could be persisted to RDB or AOF.
// A TTL alone is insufficient: a snapshot can otherwise retain an expired tail.
func (b *RedisLogBroker) Ready(ctx context.Context) error {
	if b.client == nil {
		return errors.New("ephemeral log broker is unavailable")
	}
	appendOnly, err := b.client.ConfigGet(ctx, "appendonly").Result()
	if err != nil || appendOnly["appendonly"] != "no" {
		return errors.New("ephemeral log broker append-only persistence must be disabled")
	}
	save, err := b.client.ConfigGet(ctx, "save").Result()
	if err != nil || save["save"] != "" {
		return errors.New("ephemeral log broker snapshot persistence must be disabled")
	}
	return b.client.Ping(ctx).Err()
}

const logPrefix = "heimdall:{ephemeral-logs}:"

var createLogScript = redis.NewScript(`
for i=2,5 do redis.call('ZREMRANGEBYSCORE', KEYS[i], '-inf', ARGV[2]) end
if redis.call('ZCARD', KEYS[2]) >= 1024 or redis.call('ZCARD', KEYS[3]) >= 64 or redis.call('ZCARD', KEYS[4]) >= 4 then return 0 end
if not redis.call('SET', KEYS[1], ARGV[1], 'PX', ARGV[4], 'NX') then return -1 end
for i=2,5 do
  redis.call('ZADD', KEYS[i], ARGV[3], ARGV[5])
  redis.call('PEXPIRE', KEYS[i], 180000)
end
return 1
`)

func (b *RedisLogBroker) Create(ctx context.Context, r LogRecord, ttl time.Duration) error {
	if ttl <= 0 || ttl > 2*time.Minute || r.Request.ExpiresAt.After(time.Now().Add(ttl+time.Second)) {
		return domain.ErrForbidden
	}
	remaining := time.Until(r.Request.ExpiresAt)
	if remaining <= 0 || remaining > ttl {
		return domain.ErrForbidden
	}
	raw, err := json.Marshal(r)
	if err != nil {
		return err
	}
	keys := []string{logPrefix + "record:" + r.Request.Id, logPrefix + "all", logPrefix + "tenant:" + r.TenantID, logPrefix + "env:" + r.TenantID + ":" + r.Request.EnvironmentID, logPrefix + "cluster:" + r.TenantID + ":" + r.ClusterID}
	result, err := createLogScript.Run(ctx, b.client, keys, raw, time.Now().UnixMilli(), r.Request.ExpiresAt.UnixMilli(), remaining.Milliseconds(), r.Request.Id).Int()
	if err != nil {
		return err
	}
	switch result {
	case 1:
		return nil
	case 0:
		return ErrLogLimit
	default:
		return domain.ErrConflict
	}
}

func (b *RedisLogBroker) Get(ctx context.Context, id string) (LogRecord, error) {
	var record LogRecord
	raw, err := b.client.Get(ctx, logPrefix+"record:"+id).Bytes()
	if errors.Is(err, redis.Nil) {
		return record, domain.ErrNotFound
	}
	if err != nil {
		return record, err
	}
	// JSON escaping can expand a bounded 16 KiB tail sixfold.
	if len(raw) > 128<<10 || json.Unmarshal(raw, &record) != nil || record.Request.Id != id || !record.Request.ExpiresAt.After(time.Now()) {
		return record, domain.ErrNotFound
	}
	return record, nil
}

func (b *RedisLogBroker) Pending(ctx context.Context, tenant, cluster string) ([]LogRecord, error) {
	key := logPrefix + "cluster:" + tenant + ":" + cluster
	ids, err := b.client.ZRangeByScore(ctx, key, &redis.ZRangeBy{Min: "(" + strconv.FormatInt(time.Now().UnixMilli(), 10), Max: "+inf", Offset: 0, Count: 64}).Result()
	if err != nil {
		return nil, err
	}
	out := []LogRecord{}
	for _, id := range ids {
		r, err := b.Get(ctx, id)
		if errors.Is(err, domain.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if r.TenantID != tenant || r.ClusterID != cluster {
			return nil, domain.ErrForbidden
		}
		if r.Request.State == gen.LogRequestStatePending {
			out = append(out, r)
		}
	}
	return out, nil
}

var completeLogScript = redis.NewScript(`
local raw = redis.call('GET', KEYS[1])
if not raw then return 0 end
local old = cjson.decode(raw)
local new = cjson.decode(ARGV[1])
if old.tenantID ~= new.tenantID or old.clusterID ~= new.clusterID or old.actorID ~= new.actorID or old.request.environmentID ~= new.request.environmentID or old.request.generation ~= new.request.generation then return -1 end
if old.request.state ~= 'pending' then return 1 end
local ttl = redis.call('PTTL', KEYS[1])
if ttl <= 0 then return 0 end
new.request.expiresAt = old.request.expiresAt
redis.call('SET', KEYS[1], cjson.encode(new), 'PX', ttl)
return 1
`)

func (b *RedisLogBroker) Complete(ctx context.Context, r LogRecord) error {
	raw, err := json.Marshal(r)
	if err != nil {
		return err
	}
	result, err := completeLogScript.Run(ctx, b.client, []string{logPrefix + "record:" + r.Request.Id}, raw).Int()
	if err != nil {
		return err
	}
	if result == 0 {
		return domain.ErrNotFound
	}
	if result < 0 {
		return domain.ErrStaleGeneration
	}
	return nil
}

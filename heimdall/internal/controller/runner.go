package controller

import (
	"context"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/types"

	"github.com/heimdall-dev/heimdall/internal/api/v1alpha1"
	"github.com/heimdall-dev/heimdall/internal/engine"
)

// Operator executes engine operations; *engine.Engine implements it. Tests
// substitute a scripted fake so controller logic is testable against a real
// API server (envtest) without workloads that never become ready there.
type Operator interface {
	Prepare(ctx context.Context, spec engine.Spec) (string, error)
	Apply(ctx context.Context, spec engine.Spec) (*engine.Result, error)
	Reset(ctx context.Context, spec engine.Spec, nonce int64) (*engine.Result, error)
	Destroy(ctx context.Context, spec engine.Spec) (*engine.Result, error)
	Status(ctx context.Context, spec engine.Spec) (*engine.Status, error)
}

// OperatorFactory returns an Operator that reports each step to observe.
type OperatorFactory func(observe engine.Observer) Operator

// EngineOperators adapts the engine: one cheap Engine per operation, so each
// operation gets its own progress observer.
func EngineOperators(cluster engine.Cluster, stepTimeout time.Duration) OperatorFactory {
	return func(observe engine.Observer) Operator { return engine.New(cluster, stepTimeout, observe) }
}

// opKey identifies the work an operation does. Two operations with the same
// key are interchangeable; any other key supersedes a running one.
type opKey struct {
	Type       v1alpha1.OperationType
	Generation int64
	Nonce      int64
}

const maxSteps = 64

// operation is one asynchronous engine call.
type operation struct {
	key     opKey
	started time.Time
	cancel  context.CancelFunc
	done    chan struct{}

	mu        sync.Mutex
	running   bool // holds a concurrency slot
	cancelled bool
	steps     []engine.Event
	result    *engine.Result
	err       error
	finished  time.Time
}

func (o *operation) record(ev engine.Event) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.steps = append(o.steps, ev)
	if len(o.steps) > maxSteps*2 {
		o.steps = o.steps[len(o.steps)-maxSteps*2:]
	}
}

// state is a consistent copy of an operation's progress.
type state struct {
	running, cancelled bool
	steps              []engine.Event
	result             *engine.Result
	err                error
	finished           time.Time
}

func (o *operation) snapshot() state {
	o.mu.Lock()
	defer o.mu.Unlock()
	return state{o.running, o.cancelled, append([]engine.Event(nil), o.steps...), o.result, o.err, o.finished}
}

// Done reports whether the operation has returned.
func (o *operation) Done() bool {
	select {
	case <-o.done:
		return true
	default:
		return false
	}
}

// Cancel asks the operation to stop. Engine steps observe the context; the
// operation still reports when it has actually stopped.
func (o *operation) Cancel() {
	o.mu.Lock()
	o.cancelled = true
	o.mu.Unlock()
	o.cancel()
}

// Runner executes at most one operation per environment, and at most a
// configured number overall. It is a manager Runnable that only runs on the
// leader, so operations never outlive leadership.
type Runner struct {
	factory OperatorFactory
	notify  func(types.NamespacedName)
	slots   chan struct{}

	mu   sync.Mutex
	base context.Context
	wg   sync.WaitGroup
	ops  map[types.NamespacedName]*operation
}

// NewRunner returns a Runner. notify must enqueue a reconcile for the name and
// must not block for long.
func NewRunner(factory OperatorFactory, maxConcurrent int, notify func(types.NamespacedName)) *Runner {
	return &Runner{
		factory: factory,
		notify:  notify,
		slots:   make(chan struct{}, maxConcurrent),
		ops:     map[types.NamespacedName]*operation{},
	}
}

// Start implements manager.Runnable. On shutdown every operation is
// cancelled and awaited, so engine journal leases are released promptly.
func (r *Runner) Start(ctx context.Context) error {
	r.mu.Lock()
	r.base = ctx
	r.mu.Unlock()
	<-ctx.Done()
	r.mu.Lock()
	for _, op := range r.ops {
		op.Cancel()
	}
	r.mu.Unlock()
	r.wg.Wait()
	return nil
}

// NeedLeaderElection implements manager.LeaderElectionRunnable.
func (r *Runner) NeedLeaderElection() bool { return true }

// Get returns the environment's current or finished operation, or nil.
func (r *Runner) Get(name types.NamespacedName) *operation {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ops[name]
}

// Forget cancels and drops an environment's operation (its object is gone).
func (r *Runner) Forget(name types.NamespacedName) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if op := r.ops[name]; op != nil {
		op.Cancel()
		delete(r.ops, name)
	}
}

// Remove drops a finished operation once its result has been recorded.
func (r *Runner) Remove(name types.NamespacedName, op *operation) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ops[name] == op && op.Done() {
		delete(r.ops, name)
	}
}

// Operator returns an Operator without progress reporting, for quick
// synchronous reads (health checks).
func (r *Runner) Operator() Operator { return r.factory(nil) }

// Launch starts fn for the environment. The caller must have checked that no
// operation is in flight for it.
func (r *Runner) Launch(name types.NamespacedName, key opKey, now time.Time, fn func(context.Context, Operator) (*engine.Result, error)) *operation {
	r.mu.Lock()
	defer r.mu.Unlock()
	base := r.base
	if base == nil {
		base = context.Background()
	}
	ctx, cancel := context.WithCancel(base)
	op := &operation{key: key, started: now, cancel: cancel, done: make(chan struct{})}
	r.ops[name] = op
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		defer r.notify(name)
		defer close(op.done)
		defer cancel()
		select {
		case r.slots <- struct{}{}:
		case <-ctx.Done():
			op.mu.Lock()
			op.err, op.finished = ctx.Err(), time.Now()
			op.mu.Unlock()
			return
		}
		defer func() { <-r.slots }()
		op.mu.Lock()
		op.running = true
		op.mu.Unlock()
		r.notify(name)
		operator := r.factory(func(ev engine.Event) {
			op.record(ev)
			r.notify(name)
		})
		result, err := fn(ctx, operator)
		op.mu.Lock()
		op.result, op.err, op.finished = result, err, time.Now()
		op.mu.Unlock()
	}()
	return op
}

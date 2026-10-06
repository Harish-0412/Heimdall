// Package controlapi implements the outbound-only, tenant-scoped HTTP contract.
package controlapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"go.opentelemetry.io/otel"
	"github.com/heimdall-dev/heimdall/internal/config"
	"github.com/heimdall-dev/heimdall/internal/controlapi/gen"
	"github.com/heimdall-dev/heimdall/internal/domain"
)

const MaxBodyBytes = 1 << 20

// Repository exposes only tenant-scoped operations. An implementation must
// derive SQL tenant context from Principal and atomically persist event/audit
// records alongside every desired-state mutation.
type Repository interface {
	Ping(context.Context) error
	Authenticate(context.Context, string) (domain.Principal, error)
	CreateEnvironment(context.Context, domain.Principal, domain.CreateEnvironment, string) (domain.Environment, error)
	GetEnvironment(context.Context, domain.Principal, string) (domain.Environment, error)
	ListEnvironments(context.Context, domain.Principal, domain.Page) ([]domain.Environment, error)
	ActEnvironment(context.Context, domain.Principal, string, int64, domain.Action, string) (domain.Environment, error)
	ReportStatus(context.Context, domain.Principal, string, domain.StatusUpdate) (domain.Environment, error)
	Timeline(context.Context, domain.Principal, string, int64, int) ([]domain.Event, error)
	Snapshot(context.Context, domain.Principal) (domain.DesiredSnapshot, error)
	GetPolicy(context.Context, domain.Principal) (config.Policy, error)
	SetPolicy(context.Context, domain.Principal, config.Policy) error
	GetRepository(context.Context, domain.Principal, string) (domain.Repository, error)
	GetCluster(context.Context, domain.Principal, string) (domain.Cluster, error)
	ListClusters(context.Context, domain.Principal) ([]domain.Cluster, error)
	CreateCluster(context.Context, domain.Principal, domain.Cluster) (domain.Cluster, error)
	Heartbeat(context.Context, domain.Principal, string) (domain.Cluster, error)
	IssueCredential(context.Context, domain.Principal, domain.CredentialInput) (domain.IssuedCredential, error)
	ExchangeCredential(context.Context, string, string, string) (domain.CredentialPair, error)
	RevokeClusterCredentials(context.Context, domain.Principal, string) error
	AppendAudit(context.Context, domain.Principal, string, string, json.RawMessage) error
}

type Options struct {
	Logger *slog.Logger
	// GitHub handlers authenticate HMAC and Actions OIDC independently.
	GitHubWebhook http.Handler
	BuildCallback http.Handler
	// SSE sessions periodically re-authenticate and end at this duration.
	StreamDuration time.Duration
	PollInterval time.Duration
	LogTTL time.Duration
}

type Server struct {
	repo Repository
	opts Options
	mu sync.Mutex
	logs map[string]logRecord
}

var _ gen.ServerInterface = (*Server)(nil)

func New(repo Repository, opts Options) *Server {
	if opts.Logger == nil { opts.Logger = slog.Default() }
	if opts.StreamDuration <= 0 { opts.StreamDuration = 5 * time.Minute }
	if opts.PollInterval <= 0 { opts.PollInterval = time.Second }
	if opts.LogTTL <= 0 || opts.LogTTL > 5 * time.Minute { opts.LogTTL = 2 * time.Minute }
	return &Server{repo: repo, opts: opts, logs: make(map[string]logRecord)}
}

func (s *Server) Handler() http.Handler {
	r := chi.NewRouter()
	r.Use(s.middleware)
	r.NotFound(func(w http.ResponseWriter, r *http.Request) { s.fail(w, r, http.StatusNotFound, "api.not_found", "Endpoint not found") })
	r.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) { s.fail(w,r,http.StatusMethodNotAllowed,"api.method_not_allowed","Method not allowed") })
	return gen.HandlerWithOptions(s, gen.ChiServerOptions{BaseRouter:r, ErrorHandlerFunc: func(w http.ResponseWriter,r *http.Request,_ error) { s.fail(w,r,http.StatusBadRequest,"api.invalid_request","Invalid path, query or header parameter") }})
}

type contextKey uint8
const ( principalKey contextKey = iota; requestIDKey )

var safeID = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)
var idemID = regexp.MustCompile(`^[A-Za-z0-9._:-]{8,128}$`)
var dnsID = regexp.MustCompile(`^[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?$`)

func randomID() string { b := make([]byte,16); if _,err:=rand.Read(b);err!=nil { panic("cryptographic random unavailable") };return hex.EncodeToString(b) }
func requestID(r *http.Request) string { v,_:=r.Context().Value(requestIDKey).(string);return v }
func principal(r *http.Request) domain.Principal { p,_:=r.Context().Value(principalKey).(domain.Principal);return p }
func bearer(r *http.Request) string { v:=r.Header.Values("Authorization");if len(v)!=1{return ""};p:=strings.Fields(v[0]);if len(p)!=2 || !strings.EqualFold(p[0],"Bearer") || len(p[1])>1024{return ""};return p[1] }

func (s *Server) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){
		id:=r.Header.Get("X-Request-ID");if !safeID.MatchString(id){id=randomID()}
		ctx:=context.WithValue(r.Context(),requestIDKey,id)
		ctx,span:=otel.Tracer("heimdall/controlapi").Start(ctx,"HTTP "+r.Method);defer span.End()
		r=r.WithContext(ctx);w.Header().Set("X-Request-ID",id);w.Header().Set("Cache-Control","no-store");w.Header().Set("X-Content-Type-Options","nosniff")
		defer func(){if recover()!=nil{s.opts.Logger.ErrorContext(ctx,"API panic", "request_id",id);s.fail(w,r,500,"api.internal","An internal error occurred")}}()
		r.Body=http.MaxBytesReader(w,r.Body,MaxBodyBytes)
		public:=r.URL.Path=="/healthz" || r.URL.Path=="/readyz" || r.URL.Path=="/v1/github/webhooks" || r.URL.Path=="/v1/github/builds" || r.URL.Path=="/v1/agent/register" || r.URL.Path=="/v1/agent/refresh"
		if !public {
			token:=bearer(r);if token==""{s.fail(w,r,401,"auth.required","A bearer credential is required");return}
			p,err:=s.repo.Authenticate(ctx,token);if err!=nil || p.TenantID=="" || p.ActorID=="" || p.Role=="" {s.fail(w,r,401,"auth.invalid","The credential is invalid, expired or revoked");return}
			if p.Kind!="access" && p.Kind!="user" {s.fail(w,r,403,"auth.forbidden","This credential cannot access this endpoint");return}
			r=r.WithContext(context.WithValue(ctx,principalKey,p))
		}
		next.ServeHTTP(w,r)
	})
}

func (s *Server) allow(w http.ResponseWriter,r *http.Request,roles ...string) bool {
	p:=principal(r);for _,role:=range roles {if p.Role==role {if role=="agent" && (p.ClusterID=="" || p.Kind!="access"){break};return true}}
	s.fail(w,r,403,"auth.forbidden","This credential is not authorized for this operation");return false
}

func (s *Server) fail(w http.ResponseWriter,r *http.Request,status int,code,message string){s.json(w,status,gen.Error{Code:code,Message:message,RequestID:requestID(r)})}
func (s *Server) json(w http.ResponseWriter,status int,v any){w.Header().Set("Content-Type","application/json");w.WriteHeader(status);if status!=204{_ = json.NewEncoder(w).Encode(v)}}
func (s *Server) err(w http.ResponseWriter,r *http.Request,err error) {
	switch {
	case errors.Is(err,domain.ErrUnauthorized):s.fail(w,r,401,"auth.invalid","The credential is invalid, expired or revoked")
	case errors.Is(err,domain.ErrForbidden):s.fail(w,r,403,"auth.forbidden","This operation is not authorized")
	case errors.Is(err,domain.ErrNotFound):s.fail(w,r,404,"api.not_found","The requested resource was not found")
	case errors.Is(err,domain.ErrStaleGeneration):s.fail(w,r,409,"state.stale_generation","The status generation is not current; pull desired state again")
	case errors.Is(err,domain.ErrConflict):s.fail(w,r,409,"state.version_conflict","The environment changed; read the current version and retry")
	case errors.Is(err,domain.ErrIdempotencyConflict):s.fail(w,r,409,"api.idempotency_conflict","The idempotency key was already used for a different request")
	case errors.Is(err,domain.ErrInvalidTransition):s.fail(w,r,409,"state.invalid_transition","This action is not valid in the environment's current state")
	case errors.Is(err,domain.ErrQuotaExceeded):s.fail(w,r,409,"policy.quota_exceeded","The tenant environment quota was exceeded")
	default:s.opts.Logger.ErrorContext(r.Context(),"API storage operation failed","request_id",requestID(r),"error",err);s.fail(w,r,503,"api.unavailable","The control plane is temporarily unavailable; retry with the same idempotency key")
	}
}

// StrictJSON rejects unknown fields, duplicate object keys, excessive nesting,
// trailing values and malformed UTF-8. It is shared by nested wire documents.
func StrictJSON(data []byte,v any) error {
	if !utf8.Valid(data){return errors.New("JSON must be UTF-8")}
	d:=json.NewDecoder(bytes.NewReader(data));if err:=checkJSON(d,0);err!=nil{return err};if _,err:=d.Token();err!=io.EOF{return errors.New("JSON must contain one value")}
	d=json.NewDecoder(bytes.NewReader(data));d.DisallowUnknownFields();if err:=d.Decode(v);err!=nil{return err};return nil
}
func checkJSON(d *json.Decoder,depth int) error {
	if depth>64{return errors.New("JSON nesting exceeds 64")};tok,err:=d.Token();if err!=nil{return err};delim,ok:=tok.(json.Delim);if !ok{return nil};switch delim {
	case '{': seen:=map[string]bool{};for d.More(){k,err:=d.Token();if err!=nil{return err};name,ok:=k.(string);if !ok || seen[name]{return errors.New("duplicate or invalid JSON object key")};seen[name]=true;if err:=checkJSON(d,depth+1);err!=nil{return err}};end,err:=d.Token();if err!=nil || end!=json.Delim('}'){return errors.New("invalid JSON object")}
	case '[':for d.More(){if err:=checkJSON(d,depth+1);err!=nil{return err}};end,err:=d.Token();if err!=nil || end!=json.Delim(']'){return errors.New("invalid JSON array")}
	default:return errors.New("invalid JSON delimiter")
	};return nil
}
func (s *Server) decode(w http.ResponseWriter,r *http.Request,v any) bool {
	media,_,err:=mime.ParseMediaType(r.Header.Get("Content-Type"));if err!=nil || media!="application/json"{s.fail(w,r,415,"api.content_type","Content-Type must be application/json");return false}
	b,err:=io.ReadAll(r.Body);if err!=nil{var large *http.MaxBytesError;if errors.As(err,&large){s.fail(w,r,413,"api.body_too_large","The request body exceeds the size limit")}else{s.fail(w,r,400,"api.invalid_request","Cannot read request body")};return false}
	if len(bytes.TrimSpace(b))==0 || bytes.TrimSpace(b)[0]!='{' || StrictJSON(b,v)!=nil{s.fail(w,r,400,"api.invalid_request","Body must be a single JSON object with only supported fields");return false};return true
}
func pageLimit(v *int) (int,error) { if v==nil{return 50,nil};if *v<1 || *v>100{return 0,errors.New("limit must be 1 through 100")};return *v,nil }

func (s *Server) Health(w http.ResponseWriter,r *http.Request){s.json(w,200,map[string]string{"status":"ok"})}
func (s *Server) Ready(w http.ResponseWriter,r *http.Request){ctx,cancel:=context.WithTimeout(r.Context(),3*time.Second);defer cancel();if err:=s.repo.Ping(ctx);err!=nil{s.fail(w,r,503,"api.unavailable","Database is unavailable");return};s.json(w,200,map[string]string{"status":"ready"})}
func (s *Server) GithubWebhook(w http.ResponseWriter,r *http.Request){if s.opts.GitHubWebhook==nil{s.fail(w,r,503,"github.unconfigured","GitHub webhook reception is not configured");return};s.opts.GitHubWebhook.ServeHTTP(w,r)}
func (s *Server) GithubBuild(w http.ResponseWriter,r *http.Request){if s.opts.BuildCallback==nil{s.fail(w,r,503,"github.unconfigured","GitHub build reception is not configured");return};s.opts.BuildCallback.ServeHTTP(w,r)}

func toEnvironment(e domain.Environment) gen.Environment {status:=e.Status;if len(status)==0 {status=json.RawMessage(`{}`)};return gen.Environment{Id:e.ID,TenantID:e.TenantID,RepositoryID:e.RepositoryID,ClusterID:e.ClusterID,Name:e.Name,Version:e.Version,Generation:e.Generation,ResetNonce:e.ResetNonce,DesiredState:gen.EnvironmentDesiredState(e.DesiredState),Phase:e.Phase,Spec:e.Spec,Status:status,ExpiresAt:e.ExpiresAt,CreatedAt:e.CreatedAt,UpdatedAt:e.UpdatedAt}}
func toEvent(e domain.Event) gen.Event {data:=e.Payload;return gen.Event{Id:e.ID,EnvironmentID:e.EnvironmentID,Generation:e.Generation,Type:e.Kind,Message:e.Kind,Data:&data,CreatedAt:e.CreatedAt}}

func (s *Server) ListEnvironments(w http.ResponseWriter,r *http.Request,p gen.ListEnvironmentsParams){if !s.allow(w,r,"admin","member","viewer"){return};limit,err:=pageLimit(p.Limit);if err!=nil{s.fail(w,r,400,"api.invalid_request",err.Error());return};after:="";if p.After!=nil{after=*p.After;if len(after)>128{s.fail(w,r,400,"api.invalid_request","Invalid pagination cursor");return}};rows,err:=s.repo.ListEnvironments(r.Context(),principal(r),domain.Page{AfterID:after,Limit:limit+1});if err!=nil{s.err(w,r,err);return};page:=gen.EnvironmentPage{Items:[]gen.Environment{}};if len(rows)>limit{rows=rows[:limit];next:=rows[len(rows)-1].ID;page.Next=&next};for _,e:=range rows{page.Items=append(page.Items,toEnvironment(e))};s.json(w,200,page)}
func (s *Server) GetEnvironment(w http.ResponseWriter,r *http.Request,id string){if !s.allow(w,r,"admin","member","viewer"){return};e,err:=s.repo.GetEnvironment(r.Context(),principal(r),id);if err!=nil{s.err(w,r,err);return};s.json(w,200,toEnvironment(e))}

func (s *Server) ActEnvironment(w http.ResponseWriter,r *http.Request,id string,params gen.ActEnvironmentParams){if !s.allow(w,r,"admin","member"){return};var a gen.Action;if !s.decode(w,r,&a){return};if !idemID.MatchString(params.IdempotencyKey) || a.Version<1 || !a.Action.Valid(){s.fail(w,r,400,"api.invalid_request","A valid action, positive version and idempotency key are required");return};if a.Action==gen.Extend {policy,err:=s.repo.GetPolicy(r.Context(),principal(r));if err!=nil{s.err(w,r,err);return};if a.ExpiresAt==nil || a.ExpiresAt.Before(time.Now().Add(policy.MinTTL)) || a.ExpiresAt.After(time.Now().Add(policy.MaxTTL)){s.fail(w,r,400,"policy.ttl","Expiry must remain within tenant TTL limits");return}}else if a.ExpiresAt!=nil{s.fail(w,r,400,"api.invalid_request","expiresAt is only accepted for extend");return};e,err:=s.repo.ActEnvironment(r.Context(),principal(r),id,a.Version,domain.Action{Kind:string(a.Action),ExpiresAt:a.ExpiresAt},params.IdempotencyKey);if err!=nil{s.err(w,r,err);return};s.json(w,200,toEnvironment(e))}

func (s *Server) ListEvents(w http.ResponseWriter,r *http.Request,id string,p gen.ListEventsParams){if !s.allow(w,r,"admin","member","viewer"){return};limit,err:=pageLimit(p.Limit);if err!=nil{s.fail(w,r,400,"api.invalid_request",err.Error());return};after:=int64(0);if p.After!=nil{after=*p.After};if after<0{s.fail(w,r,400,"api.invalid_request","Invalid timeline cursor");return};rows,err:=s.repo.Timeline(r.Context(),principal(r),id,after,limit);if err!=nil{s.err(w,r,err);return};page:=gen.EventPage{Items:[]gen.Event{},Next:after};for _,e:=range rows{page.Items=append(page.Items,toEvent(e));page.Next=e.ID};s.json(w,200,page)}

func (s *Server) StreamTimeline(w http.ResponseWriter,r *http.Request,id string,p gen.StreamTimelineParams){
	if !s.allow(w,r,"admin","member","viewer"){return};after:=int64(0);if p.After!=nil{after=*p.After};if p.LastEventID!=nil{after=*p.LastEventID};if after<0{s.fail(w,r,400,"api.invalid_request","Invalid timeline cursor");return}
	if _,err:=s.repo.GetEnvironment(r.Context(),principal(r),id);err!=nil{s.err(w,r,err);return};f,ok:=w.(http.Flusher);if !ok{s.fail(w,r,500,"api.internal","Streaming is unavailable");return}
	w.Header().Set("Content-Type","text/event-stream");w.Header().Set("X-Accel-Buffering","no");w.WriteHeader(200);_,_=io.WriteString(w,"retry: 2000\n\n");f.Flush()
	ctx,cancel:=context.WithTimeout(r.Context(),s.opts.StreamDuration);defer cancel();ticker:=time.NewTicker(s.opts.PollInterval);defer ticker.Stop();token:=bearer(r)
	for { rows,err:=s.repo.Timeline(ctx,principal(r),id,after,100);if err!=nil{return};for _,e:=range rows{data,err:=json.Marshal(toEvent(e));if err!=nil{return};if _,err:=fmt.Fprintf(w,"id: %d\nevent: timeline\ndata: %s\n\n",e.ID,data);err!=nil{return};after=e.ID};if len(rows)>0 {f.Flush();if len(rows)==100{continue}};select{case <-ctx.Done():return;case <-ticker.C:p,err:=s.repo.Authenticate(ctx,token);if err!=nil || p.TenantID!=principal(r).TenantID || p.ActorID!=principal(r).ActorID || (p.Role!="admin" && p.Role!="member" && p.Role!="viewer"){return};_,_=io.WriteString(w,": heartbeat "+strconv.FormatInt(time.Now().Unix(),10)+"\n\n");f.Flush()}}
}

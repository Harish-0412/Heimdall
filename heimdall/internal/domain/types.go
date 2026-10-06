// Package domain describes control-plane intent independently of HTTP and SQL.
package domain

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/heimdall-dev/heimdall/internal/config"
)

var (
	ErrNotFound            = errors.New("not found")
	ErrConflict            = errors.New("version conflict")
	ErrStaleGeneration     = errors.New("stale generation")
	ErrInvalidTransition   = errors.New("invalid transition")
	ErrUnauthorized        = errors.New("unauthorized")
	ErrForbidden           = errors.New("forbidden")
	ErrIdempotencyConflict = errors.New("idempotency key reused with a different request")
	ErrLeaseLost           = errors.New("worker lease lost")
	ErrQuotaExceeded       = errors.New("tenant environment quota exceeded")
)

type Principal struct{ TenantID, TenantSlug, ActorID, ClusterID, Role, Kind, CredentialID string }

type Tenant struct {
	ID   string `json:"id"`
	Slug string `json:"slug"`
	Name string `json:"name"`
}
type Cluster struct {
	ID            string     `json:"id"`
	TenantID      string     `json:"tenantId"`
	Name          string     `json:"name"`
	LastHeartbeat *time.Time `json:"lastHeartbeat,omitempty"`
	AgentVersion  string     `json:"agentVersion,omitempty"`
	CreatedAt     time.Time  `json:"createdAt"`
	Tier          int        `json:"tier"`
}
type Installation struct {
	ID        int64  `json:"id"`
	TenantID  string `json:"tenantId"`
	Account   string `json:"account"`
	Suspended bool   `json:"suspended"`
}
type Repository struct {
	ID                 string `json:"id"`
	TenantID           string `json:"tenantId"`
	GitHubID           int64  `json:"githubId"`
	InstallationID     int64  `json:"installationId"`
	ClusterID          string `json:"clusterId"`
	FullName           string `json:"fullName"`
	DefaultBranch      string `json:"defaultBranch"`
	Enabled            bool   `json:"enabled"`
	TrustedWorkflowRef string `json:"trustedWorkflowRef"`
	TrustedWorkflowSHA string `json:"trustedWorkflowSha"`
}
type Environment struct {
	ID           string          `json:"id"`
	TenantID     string          `json:"tenantId"`
	RepositoryID string          `json:"repositoryId"`
	ClusterID    string          `json:"clusterId"`
	Name         string          `json:"name"`
	PullRequest  int64           `json:"pullRequest"`
	Owner        string          `json:"owner"`
	Commit       string          `json:"commit"`
	Version      int64           `json:"version"`
	Generation   int64           `json:"generation"`
	ResetNonce   int64           `json:"resetNonce"`
	DesiredState string          `json:"desiredState"`
	Phase        string          `json:"phase"`
	BuildState   string          `json:"buildState"`
	Spec         json.RawMessage `json:"spec"`
	Status       json.RawMessage `json:"status"`
	ExpiresAt    time.Time       `json:"expiresAt"`
	CreatedAt    time.Time       `json:"createdAt"`
	UpdatedAt    time.Time       `json:"updatedAt"`
}
type CreateEnvironment struct {
	ID, RepositoryID, ClusterID, Name, Owner, Commit string
	PullRequest                                      int64
	Spec                                             json.RawMessage
	ExpiresAt                                        time.Time
}
type Deployment struct {
	ID            string          `json:"id"`
	EnvironmentID string          `json:"environmentId"`
	Generation    int64           `json:"generation"`
	Commit        string          `json:"commit"`
	Spec          json.RawMessage `json:"spec"`
	CreatedAt     time.Time       `json:"createdAt"`
}
type DeployInput struct {
	Spec                   json.RawMessage
	Commit                 string
	ExpiresAt              time.Time
	DeliveryID, LeaseToken string
}
type Action struct {
	Kind       string     `json:"kind"`
	Reason     string     `json:"reason,omitempty"`
	ExpiresAt  *time.Time `json:"expiresAt,omitempty"`
	DeliveryID string     `json:"-"`
	LeaseToken string     `json:"-"`
}
type DataAttestation struct {
	RepositoryID string    `json:"repositoryId"`
	PullRequest  int64     `json:"pullRequest"`
	ConfigSHA256 string    `json:"configSha256"`
	SeedSHA256   string    `json:"seedSha256"`
	ApprovedBy   string    `json:"approvedBy"`
	Reason       string    `json:"reason"`
	Sanitised    bool      `json:"sanitised"`
	ExpiresAt    time.Time `json:"expiresAt"`
}
type StatusUpdate struct {
	Generation int64           `json:"generation"`
	Version    int64           `json:"version"`
	Phase      string          `json:"phase"`
	Status     json.RawMessage `json:"status"`
	EventID    string          `json:"eventId"`
	Step       string          `json:"step,omitempty"`
	Diagnoses  []Diagnosis     `json:"diagnoses,omitempty"`
	SmokeRuns  []SmokeRun      `json:"smokeRuns,omitempty"`
}
type Event struct {
	ID            int64           `json:"id"`
	EnvironmentID string          `json:"environmentId"`
	Generation    int64           `json:"generation"`
	Kind          string          `json:"kind"`
	Payload       json.RawMessage `json:"payload"`
	CreatedAt     time.Time       `json:"createdAt"`
}
type Diagnosis struct {
	Code       string   `json:"code"`
	Summary    string   `json:"summary"`
	Suggestion string   `json:"suggestion"`
	Evidence   []string `json:"evidence,omitempty"`
	Subject    string   `json:"subject,omitempty"`
	Stage      string   `json:"stage,omitempty"`
}
type SmokeRun struct {
	Name       string `json:"name"`
	Passed     bool   `json:"passed"`
	DurationMS int64  `json:"durationMs"`
	Summary    string `json:"summary,omitempty"`
}
type Page struct {
	AfterID      string
	Limit        int
	RepositoryID string
	ClusterID    string
}
type Quota struct {
	MaxEnvironments int `json:"maxEnvironments"`
	MaxCPUMilli     int `json:"maxCpuMilli"`
	MaxMemoryMi     int `json:"maxMemoryMi"`
	MaxStorageMi    int `json:"maxStorageMi"`
}
type UsageSample struct {
	ID            int64     `json:"id"`
	EnvironmentID string    `json:"environmentId"`
	CPUMilli      int       `json:"cpuMilli"`
	MemoryMi      int       `json:"memoryMi"`
	StorageMi     int       `json:"storageMi"`
	At            time.Time `json:"at"`
}
type Audit struct {
	ID       int64           `json:"id"`
	ActorID  string          `json:"actorId"`
	Action   string          `json:"action"`
	Resource string          `json:"resource"`
	Metadata json.RawMessage `json:"metadata"`
	At       time.Time       `json:"at"`
}
type CredentialInput struct {
	ActorID, ClusterID, Role, Kind string
	TTL                            time.Duration
}
type Credential struct {
	ID        string     `json:"id"`
	ActorID   string     `json:"actorId"`
	ClusterID string     `json:"clusterId,omitempty"`
	Role      string     `json:"role"`
	Kind      string     `json:"kind"`
	ExpiresAt time.Time  `json:"expiresAt"`
	RevokedAt *time.Time `json:"revokedAt,omitempty"`
}
type IssuedCredential struct {
	Credential
	Token string `json:"token"`
}
type CredentialPair struct {
	Access  IssuedCredential `json:"access"`
	Refresh IssuedCredential `json:"refresh"`
}
type WebhookDelivery struct {
	DeliveryID   string          `json:"deliveryId"`
	TenantID     string          `json:"tenantId"`
	Event        string          `json:"event"`
	RepositoryID string          `json:"repositoryId"`
	PullRequest  int64           `json:"pullRequest"`
	Payload      json.RawMessage `json:"payload"`
	PayloadHash  string          `json:"payloadHash"`
	LeaseToken   string          `json:"leaseToken,omitempty"`
	Attempts     int             `json:"attempts"`
	ReceivedAt   time.Time       `json:"receivedAt"`
}
type Outbox struct {
	ID            int64           `json:"id"`
	TenantID      string          `json:"tenantId"`
	EnvironmentID string          `json:"environmentId,omitempty"`
	Generation    int64           `json:"generation"`
	Kind          string          `json:"kind"`
	Payload       json.RawMessage `json:"payload"`
	LeaseToken    string          `json:"leaseToken,omitempty"`
	Attempts      int             `json:"attempts"`
}
type GitHubDelivery struct {
	EnvironmentID string `json:"environmentId"`
	Generation    int64  `json:"generation"`
	CommentID     int64  `json:"commentId"`
	CheckID       int64  `json:"checkId"`
}
type PullRequest struct {
	RepositoryID                 string
	Number                       int64
	HeadSHA                      string
	State                        string
	Fork                         bool
	UpdatedAt                    time.Time
	ObservedAt                   time.Time
	Version                      int64
	TraceParent                  string
	ApprovedSHA                  string
	BaseSHA                      string
	NeedsApproval                bool
	ConfigDigest, BaselineDigest string
}
type PullRequestObservation struct {
	RepositoryID                                         string
	Number                                               int64
	HeadSHA, BaseSHA, State, Owner                       string
	IsFork, NeedsApproval                                bool
	ConfigDigest, BaselineDigest, DeliveryID, LeaseToken string
	Message                                              string
	UpdatedAt                                            time.Time
	ExpectedVersion                                      int64
	TraceParent                                          string
}
type BuildInput struct {
	RepositoryID                                     string
	Number                                           int64
	HeadSHA, Bundle                                  string
	Images                                           map[string]string
	ConfigDigest, BaselineDigest, RunID, WorkflowRef string
	PolicySHA256                                     string
	RunAttempt                                       int64
	Spec                                             json.RawMessage
	ExpiresAt                                        time.Time
}
type DesiredSnapshot struct {
	Revision     int64         `json:"revision"`
	Environments []Environment `json:"environments"`
	Policy       config.Policy `json:"policy"`
}

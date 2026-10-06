package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// DesiredState is what the environment should be doing.
// +kubebuilder:validation:Enum=Running;Destroyed
type DesiredState string

const (
	DesiredRunning   DesiredState = "Running"
	DesiredDestroyed DesiredState = "Destroyed"
	// Sleeping arrives with P8 (manual sleep/wake). Until then the API rejects
	// it: a state the agent cannot honour must not be accepted.
)

// PreviewEnvironmentSpec is the desired state of one preview environment for
// this cluster. It is a projection of the control plane's record (ADR 0005):
// the agent writes it from an authoritative source, or an operator applies it
// directly when the cluster itself is the source (docs/agent.md).
//
// +kubebuilder:validation:XValidation:rule="self.generation >= oldSelf.generation",message="spec.generation must not decrease"
// +kubebuilder:validation:XValidation:rule="self.generation > oldSelf.generation || (self.commit == oldSelf.commit && self.config == oldSelf.config && (has(self.images) ? (has(oldSelf.images) && self.images == oldSelf.images) : !has(oldSelf.images)) && (has(self.data) ? (has(oldSelf.data) && self.data == oldSelf.data) : !has(oldSelf.data)))",message="changed deployment inputs (commit, config, images or data) require a higher spec.generation"
// +kubebuilder:validation:XValidation:rule="self.resetNonce >= oldSelf.resetNonce",message="spec.resetNonce must not decrease"
type PreviewEnvironmentSpec struct {
	// Tenant is the tenant's slug.
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?$`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="tenant is immutable"
	Tenant string `json:"tenant"`

	// Repository is the GitHub repository as owner/name.
	// +kubebuilder:validation:MaxLength=140
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9][A-Za-z0-9-]{0,38}/[A-Za-z0-9._-]{1,100}$`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="repository is immutable"
	Repository string `json:"repository"`

	// PullRequest is the pull request number.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=99999999
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="pullRequest is immutable"
	PullRequest int64 `json:"pullRequest"`

	// Commit is the full commit SHA being previewed.
	// +kubebuilder:validation:Pattern=`^([0-9a-f]{40}|[0-9a-f]{64})$`
	Commit string `json:"commit"`

	// Generation is the deployment generation (ADR 0005), distinct from
	// metadata.generation: it changes only when deployment inputs change, and
	// never decreases. Work for an older generation is cancelled, and its
	// results are discarded.
	// +kubebuilder:validation:Minimum=1
	Generation int64 `json:"generation"`

	// EnvironmentID identifies the environment across generations.
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="environmentID is immutable"
	EnvironmentID string `json:"environmentID"`

	// Owner is the GitHub login of the pull request's author (metadata).
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9][A-Za-z0-9-]{0,38}(\[bot\])?$`
	Owner string `json:"owner"`

	// URLSuffix makes preview hostnames hard to guess; random per environment.
	// +kubebuilder:validation:Pattern=`^[a-z0-9]{4,8}$`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="urlSuffix is immutable"
	URLSuffix string `json:"urlSuffix"`

	// ExpiresAt is when the environment expires. Recorded as metadata; TTL
	// enforcement arrives in P8.
	ExpiresAt metav1.Time `json:"expiresAt"`

	// DesiredState is Running (default) or Destroyed. Destroyed removes every
	// resource but keeps this object; deleting the object also destroys.
	// +kubebuilder:default=Running
	// +optional
	DesiredState DesiredState `json:"desiredState,omitempty"`

	// ResetNonce requests a reset of the environment's data to its baseline
	// whenever it increases. It never decreases.
	// +kubebuilder:default=0
	// +kubebuilder:validation:Minimum=0
	// +optional
	ResetNonce int64 `json:"resetNonce,omitempty"`

	// Config is the heimdall.yaml to deploy.
	Config ConfigSource `json:"config"`

	// Images maps each service and worker to its image, pinned by digest.
	// +kubebuilder:validation:MaxProperties=16
	// +optional
	Images map[string]string `json:"images,omitempty"`

	// Data is an optional operator-approved, sanitised database import
	// (ADR 0009). Synthetic data is not supported.
	// +optional
	Data *DataSource `json:"data,omitempty"`
}

// ConfigSource carries heimdall.yaml. The digest binds the content: the agent
// refuses a config whose bytes do not hash to it.
type ConfigSource struct {
	// Inline is the heimdall.yaml document, at most 256 KiB.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=262144
	Inline string `json:"inline"`

	// SHA256 is the lowercase hex SHA-256 of Inline.
	// +kubebuilder:validation:Pattern=`^[a-f0-9]{64}$`
	SHA256 string `json:"sha256"`
}

// DataSource points at an approved import held in a ConfigMap in the agent's
// own namespace.
type DataSource struct {
	// ConfigMap is the ConfigMap's name.
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`
	ConfigMap string `json:"configMap"`

	// Key is the entry holding the SQL.
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[-._a-zA-Z0-9]+$`
	Key string `json:"key"`

	// Approval is the operator's attestation, bound to the exact bytes.
	Approval DataApproval `json:"approval"`
}

// DataApproval attests that an import is sanitised (ADR 0009).
type DataApproval struct {
	// SHA256 of the approved bytes.
	// +kubebuilder:validation:Pattern=`^[a-f0-9]{64}$`
	SHA256 string `json:"sha256"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=256
	ApprovedBy string `json:"approvedBy"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1024
	Reason string `json:"reason"`
	// Sanitised must be true.
	// +kubebuilder:validation:XValidation:rule="self == true",message="only sanitised data can be imported"
	Sanitised bool `json:"sanitised"`
}

// Phase summarises the environment for humans and dashboards. Conditions are
// the detailed, machine-readable state.
// +kubebuilder:validation:Enum=Pending;Queued;Provisioning;Ready;Degraded;Resetting;Failed;Destroying;Destroyed
type Phase string

const (
	PhasePending      Phase = "Pending"
	PhaseQueued       Phase = "Queued"
	PhaseProvisioning Phase = "Provisioning"
	PhaseReady        Phase = "Ready"
	PhaseDegraded     Phase = "Degraded"
	PhaseResetting    Phase = "Resetting"
	PhaseFailed       Phase = "Failed"
	PhaseDestroying   Phase = "Destroying"
	PhaseDestroyed    Phase = "Destroyed"
)

// Condition types. Stage conditions follow the render pipeline (ADR 0007).
const (
	ConditionReady            = "Ready"
	ConditionProgressing      = "Progressing"
	ConditionGuardrails       = "GuardrailsReady"
	ConditionDependencies     = "DependenciesReady"
	ConditionBaselineDatabase = "BaselineDatabaseReady"
	ConditionApplication      = "ApplicationReady"
	ConditionSmokeTests       = "SmokeTestsPassed"
)

// OperationType is the kind of engine operation.
// +kubebuilder:validation:Enum=Apply;Reset;Destroy
type OperationType string

const (
	OperationApply   OperationType = "Apply"
	OperationReset   OperationType = "Reset"
	OperationDestroy OperationType = "Destroy"
)

// OperationResult is how an operation ended, or Running.
// +kubebuilder:validation:Enum=Queued;Running;Succeeded;Failed;Cancelled;Interrupted
type OperationResult string

const (
	ResultQueued      OperationResult = "Queued"
	ResultRunning     OperationResult = "Running"
	ResultSucceeded   OperationResult = "Succeeded"
	ResultFailed      OperationResult = "Failed"
	ResultCancelled   OperationResult = "Cancelled"
	ResultInterrupted OperationResult = "Interrupted"
)

// URL is a public service's preview URL.
type URL struct {
	Service string `json:"service"`
	URL     string `json:"url"`
	// +optional
	Primary bool `json:"primary,omitempty"`
}

// OperationStatus describes the current or most recent engine operation.
type OperationStatus struct {
	Type OperationType `json:"type"`
	// Generation is the deployment generation the operation works on.
	Generation int64 `json:"generation"`
	// +optional
	ResetNonce int64           `json:"resetNonce,omitempty"`
	Result     OperationResult `json:"result"`
	// Attempts counts consecutive failed attempts at this operation; it
	// drives exponential backoff and resets on success or new input.
	// +optional
	Attempts  int32       `json:"attempts,omitempty"`
	StartedAt metav1.Time `json:"startedAt"`
	// +optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`
	// RetryAfter is when a failed or interrupted operation may run again
	// (exponential backoff). Unset when no retry is pending.
	// +optional
	RetryAfter *metav1.Time `json:"retryAfter,omitempty"`
}

// StepStatus is one engine step of the current or last operation.
type StepStatus struct {
	Name  string `json:"name"`
	State string `json:"state"`
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`
	// +optional
	DurationSeconds int64 `json:"durationSeconds,omitempty"`
	// +optional
	Code string `json:"code,omitempty"`
}

// ErrorStatus is the last failure, with a stable public code.
type ErrorStatus struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	// +optional
	Step string `json:"step,omitempty"`
	// Generation the failure belongs to.
	Generation int64 `json:"generation"`
	// Retryable failures are retried with exponential backoff; others wait
	// for new input (a higher generation or reset nonce).
	Retryable bool        `json:"retryable"`
	At        metav1.Time `json:"at"`
}

// PreviewEnvironmentStatus is the observed state, written only by the agent.
type PreviewEnvironmentStatus struct {
	// ObservedGeneration is the metadata.generation last reconciled.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// DeployedGeneration is the deployment generation last applied
	// successfully, end to end.
	// +optional
	DeployedGeneration int64 `json:"deployedGeneration,omitempty"`
	// CompletedResetNonce is the last reset nonce that completed.
	// +optional
	CompletedResetNonce int64 `json:"completedResetNonce,omitempty"`
	// +optional
	Phase Phase `json:"phase,omitempty"`
	// Namespace holds the environment's resources.
	// +optional
	Namespace string `json:"namespace,omitempty"`
	// +optional
	URLs []URL `json:"urls,omitempty"`
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// +optional
	Operation *OperationStatus `json:"operation,omitempty"`
	// Steps of the current or last operation, in order (bounded).
	// +optional
	// +kubebuilder:validation:MaxItems=64
	Steps []StepStatus `json:"steps,omitempty"`
	// +optional
	LastError *ErrorStatus `json:"lastError,omitempty"`
	// Diagnoses explain the last failure, most likely root cause first
	// (docs/diagnostics.md). Cleared when an operation succeeds.
	// +optional
	// +kubebuilder:validation:MaxItems=5
	Diagnoses []DiagnosisStatus `json:"diagnoses,omitempty"`
}

// DiagnosisStatus is one diagnosis of a failure: a stable public code, what
// is wrong and what to do. Evidence is redacted and kept short.
type DiagnosisStatus struct {
	// +kubebuilder:validation:MaxLength=64
	Code string `json:"code"`
	// +kubebuilder:validation:MaxLength=1024
	Summary string `json:"summary"`
	// +optional
	// +kubebuilder:validation:MaxLength=253
	Subject string `json:"subject,omitempty"`
	// +optional
	// +kubebuilder:validation:MaxLength=64
	Stage string `json:"stage,omitempty"`
	// +kubebuilder:validation:MaxLength=1024
	Suggestion string `json:"suggestion"`
	// +optional
	// +kubebuilder:validation:MaxItems=10
	// +kubebuilder:validation:items:MaxLength=256
	Evidence []string `json:"evidence,omitempty"`
}

// PreviewEnvironment is one pull request's preview environment.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=pe;preview,categories=heimdall
// +kubebuilder:printcolumn:name="Repository",type=string,JSONPath=`.spec.repository`
// +kubebuilder:printcolumn:name="PR",type=integer,JSONPath=`.spec.pullRequest`
// +kubebuilder:printcolumn:name="Generation",type=integer,JSONPath=`.spec.generation`
// +kubebuilder:printcolumn:name="Deployed",type=integer,JSONPath=`.status.deployedGeneration`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="URL",type=string,JSONPath=`.status.urls[?(@.primary==true)].url`,priority=1
// +kubebuilder:printcolumn:name="Error",type=string,JSONPath=`.status.lastError.code`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type PreviewEnvironment struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec PreviewEnvironmentSpec `json:"spec"`
	// +optional
	Status PreviewEnvironmentStatus `json:"status,omitempty"`
}

// PreviewEnvironmentList is a list of PreviewEnvironments.
//
// +kubebuilder:object:root=true
type PreviewEnvironmentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PreviewEnvironment `json:"items"`
}

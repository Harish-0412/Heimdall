package config

// Policy is the set of hard rules a config must satisfy. It comes from outside
// the pull request (tenant settings held by the control plane; DefaultPolicy for
// local use), so a PR author can never edit their own ceiling. See ADR 0006.
//
// Dangerous knobs (egress rules, node selectors, security context, IAM) are
// deliberately not part of the heimdall.yaml schema at all: they cannot be
// requested, only set by the platform.
type Policy struct {
	Limits

	// MaxVisibility is the most open preview visibility a config may request.
	MaxVisibility string

	// AllowedSecrets lists the secret names workloads may reference.
	// nil means unrestricted (local validation); empty means none.
	AllowedSecrets []string

	// AllowedRegistries lists literal prefixes a prebuilt `image:` must start
	// with, for example "ghcr.io/shopflow/". nil means unrestricted; empty
	// means prebuilt images are not allowed (only `build:`).
	AllowedRegistries []string

	// AllowLargeSize approves the `large` size preset. The zero value denies
	// it, so a tenant must be granted it explicitly. Explicit cpu/memory
	// values remain bounded by the per-container Limits either way.
	AllowLargeSize bool
}

// DefaultPolicy is the permissive policy used for local validation when no
// tenant policy is supplied. Production callers pass the tenant's policy.
func DefaultPolicy() Policy {
	return Policy{Limits: DefaultLimits(), MaxVisibility: VisibilityPublic, AllowLargeSize: true}
}

// visibilityRank orders visibilities from most to least restrictive.
func visibilityRank(v string) int {
	switch v {
	case "", VisibilityPrivate:
		return 0
	case VisibilityOrg:
		return 1
	case VisibilityPublic:
		return 2
	}
	return 3 // unknown values are treated as the most open
}

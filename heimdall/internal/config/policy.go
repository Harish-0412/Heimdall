package config

import "time"

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

// BaselinePolicy parses a protected default-branch trust ceiling independently
// of a later tenant-policy reduction. It never authorizes a deployment: the PR
// must separately load under the current explicit tenant policy. These limits
// match the largest bounded policy the control API can accept.
func BaselinePolicy() Policy {
	return Policy{Limits: Limits{MaxServices: 16, MaxWorkers: 16, MaxContainerCPUMilli: 100000, MaxContainerMemoryMi: 1 << 20,
		MaxTotalCPUMilli: 1000000, MaxTotalMemoryMi: 1 << 24, MaxPostgresStorageMi: 1 << 24, MinMemoryRequestPercent: 0,
		MinTTL: time.Second, MaxTTL: 30 * 24 * time.Hour}, MaxVisibility: VisibilityPublic, AllowLargeSize: true}
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

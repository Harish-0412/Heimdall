package config

import "time"

// Limits are platform guardrails applied during validation. They are a
// parameter (not constants) because plans differ: the API will pass limits
// derived from the tenant's quota, while the CLI uses DefaultLimits.
type Limits struct {
	MaxServices int
	MaxWorkers  int

	// Per-container ceilings.
	MaxContainerCPUMilli int
	MaxContainerMemoryMi int

	// Whole-environment ceilings, including managed dependencies.
	MaxTotalCPUMilli int
	MaxTotalMemoryMi int

	MaxPostgresStorageMi int

	// MinMemoryRequestPercent is the smallest memory request allowed, as a
	// percentage of the container's memory limit. It caps memory
	// over-commitment per container (50 means limit <= 2x request), so the
	// scheduler cannot pack memory-hungry pods onto a node until they are
	// OOM-killed. Defaulted requests use the larger of this share and 50%.
	// 0 disables the check.
	MinMemoryRequestPercent int

	MinTTL time.Duration
	MaxTTL time.Duration
}

// DefaultLimits are the limits used when no tenant quota applies.
func DefaultLimits() Limits {
	return Limits{
		MaxServices:             8,
		MaxWorkers:              8,
		MaxContainerCPUMilli:    2000,
		MaxContainerMemoryMi:    4096,
		MaxTotalCPUMilli:        6000,
		MaxTotalMemoryMi:        12288,
		MaxPostgresStorageMi:    5120,
		MinMemoryRequestPercent: 50,
		MinTTL:                  time.Hour,
		MaxTTL:                  7 * 24 * time.Hour,
	}
}

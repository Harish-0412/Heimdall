package domain

import "fmt"

// Transition is the single guard for runtime phase changes. Repeating a phase
// is permitted for stage reports; starting new work is an intent action that
// separately increments generation/version under an environment row lock.
func Transition(from, to string) error {
	if from == to && validPhase(to) {
		return nil
	}
	allowed := map[string][]string{
		"Pending":      {"Queued", "Provisioning", "Failed", "Destroying"},
		"Queued":       {"Provisioning", "Failed", "Destroying"},
		"Provisioning": {"Ready", "Degraded", "Failed", "Destroying"},
		"Ready":        {"Degraded", "Failed", "Resetting", "Destroying"},
		"Degraded":     {"Ready", "Failed", "Provisioning", "Resetting", "Destroying"},
		"Resetting":    {"Ready", "Degraded", "Failed", "Destroying"},
		"Failed":       {"Provisioning", "Resetting", "Destroying"},
		"Destroying":   {"Destroyed", "Failed"},
		"Destroyed":    {},
	}
	for _, next := range allowed[from] {
		if next == to {
			return nil
		}
	}
	return fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, from, to)
}

func validPhase(phase string) bool {
	switch phase {
	case "Pending", "Queued", "Provisioning", "Ready", "Degraded", "Resetting", "Failed", "Destroying", "Destroyed":
		return true
	}
	return false
}

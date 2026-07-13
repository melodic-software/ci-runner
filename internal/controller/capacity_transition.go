package controller

import "github.com/melodic-software/ci-runner/internal/model"

// sequenceCapacityTransfer prevents a capacity slot from being visible to two
// independent scale-set listeners during a cross-pool reallocation. GitHub
// acknowledges each listener poll independently, so sending a decrease for one
// pool concurrently with an increase for another can transiently expose both
// the old and new slot. The first pass therefore sends every decrease while
// holding increases at their last known capacity. A later reconciliation can
// publish the increases after the decreased capacities are authoritative.
func sequenceCapacityTransfer(previous model.ObservedState, planned map[string]int) map[string]int {
	current := make(map[string]int, len(previous.Pools))
	for _, pool := range previous.Pools {
		capacity := pool.MaxCapacity
		if capacity < 0 {
			capacity = 0
		}
		current[pool.ID] = capacity
	}

	hasDecrease := false
	for poolID, capacity := range planned {
		if capacity < current[poolID] {
			hasDecrease = true
			break
		}
	}
	result := make(map[string]int, len(planned))
	for poolID, capacity := range planned {
		if capacity < 0 {
			capacity = 0
		}
		if hasDecrease && capacity > current[poolID] {
			capacity = current[poolID]
		}
		result[poolID] = capacity
	}
	return result
}

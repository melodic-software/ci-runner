package controller

import "github.com/melodic-software/ci-runner/internal/model"

// sequenceCapacityTransfer sends every decrease while holding increases at their last known
// capacity, so a cross-pool reallocation never exposes one slot to two independent listeners.
func sequenceCapacityTransfer(previous model.ObservedState, planned map[string]int) map[string]int {
	current := make(map[string]int, len(previous.Pools))
	for _, pool := range previous.Pools {
		current[pool.ID] = max(pool.MaxCapacity, 0)
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
		capacity = max(capacity, 0)
		if hasDecrease && capacity > current[poolID] {
			capacity = current[poolID]
		}
		result[poolID] = capacity
	}
	return result
}

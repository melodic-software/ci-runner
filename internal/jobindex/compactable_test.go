package jobindex

import (
	"testing"
	"time"
)

func TestCompactableUnderCapacityPressure(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name   string
		record Record
		want   bool
	}{
		{"open record", Record{Open: true, CompletedAt: now}, false},
		{"no terminal marker", Record{JobID: "job-1", JobStartedAt: now}, false},
		{"finalized active-job mapping", Record{JobID: "job-1", JobStartedAt: now, FinalizedAt: now}, false},
		{"completed", Record{JobID: "job-1", JobStartedAt: now, CompletedAt: now}, true},
		{"finalized without job", Record{FinalizedAt: now}, true},
		{"tombstoned", Record{TombstonedAt: &now}, true},
	}
	for _, testCase := range cases {
		if got := CompactableUnderCapacityPressure(testCase.record); got != testCase.want {
			t.Errorf("%s: CompactableUnderCapacityPressure = %v, want %v", testCase.name, got, testCase.want)
		}
	}
}

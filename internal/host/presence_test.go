package host

import (
	"testing"
	"time"
)

func TestInputIdleSinceSurvivesTickWrap(t *testing.T) {
	t.Parallel()
	tests := []struct {
		now  uint64
		last uint32
		want time.Duration
	}{
		{now: 10_000, last: 4_000, want: 6 * time.Second},
		{now: 1<<32 + 500, last: 1<<32 - 1_500, want: 2 * time.Second},
		{now: 5<<32 + 7, last: 7, want: 0},
	}
	for _, test := range tests {
		if got := inputIdleSince(test.now, test.last); got != test.want {
			t.Fatalf("inputIdleSince(%d, %d) = %s, want %s", test.now, test.last, got, test.want)
		}
	}
}

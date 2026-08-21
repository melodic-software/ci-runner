package host

import (
	"slices"
	"testing"
	"time"
)

func TestRebootArgsIncludesPlannedReasonBeforeComment(t *testing.T) {
	t.Parallel()
	args := rebootArgs(0, "ci-runner host reboot")
	if !slices.Equal(args, []string{"/r", "/t", "0", "/d", plannedOtherRestart, "/c", "ci-runner host reboot"}) {
		t.Fatalf("reboot args = %#v", args)
	}
	dIndex := slices.Index(args, "/d")
	cIndex := slices.Index(args, "/c")
	if dIndex < 0 || cIndex < 0 || dIndex > cIndex {
		t.Fatalf("/d must precede /c: %#v", args)
	}
}

func TestRebootArgsOmitsCommentAndReasonWhenEmpty(t *testing.T) {
	t.Parallel()
	args := rebootArgs(5*time.Second, "")
	if !slices.Equal(args, []string{"/r", "/t", "5"}) {
		t.Fatalf("reboot args = %#v", args)
	}
}

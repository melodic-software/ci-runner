package scaleset

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestJITConfigAlwaysFormatsRedacted(t *testing.T) {
	t.Parallel()
	jit := NewJITConfig([]byte("super-secret"))
	if got := fmt.Sprintf("%s %#v", jit, jit); got != "[REDACTED JIT CONFIG] [REDACTED JIT CONFIG]" {
		t.Fatalf("formatted JIT config = %q", got)
	}
	revealed := jit.Reveal()
	revealed[0] = 'X'
	if got := string(jit.Reveal()); got != "super-secret" {
		t.Fatalf("Reveal returned shared storage: %q", got)
	}
}

func TestRetryableClassification(t *testing.T) {
	t.Parallel()
	for _, kind := range []ErrorKind{ErrorConflict, ErrorRateLimited, ErrorServer, ErrorTransport} {
		if !Retryable(&Error{Kind: kind}) {
			t.Errorf("%s should be retryable", kind)
		}
	}
	if Retryable(&Error{Kind: ErrorUnauthorized}) || Retryable(errors.New("plain")) {
		t.Fatal("non-retryable error classified as retryable")
	}
}

func TestFakeEnsureNilPriorRotatesListenerID(t *testing.T) {
	t.Parallel()
	fake := NewFake()
	definition := Definition{TargetID: "org"}
	first, err := fake.Ensure(context.Background(), definition, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := fake.Ensure(context.Background(), definition, &first)
	if err != nil {
		t.Fatal(err)
	}
	if second.ListenerID != first.ListenerID || second.ScaleSetID != first.ScaleSetID {
		t.Fatalf("reuse with prior changed identity: first=%#v second=%#v", first, second)
	}
	rotated, err := fake.Ensure(context.Background(), definition, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rotated.ScaleSetID != first.ScaleSetID {
		t.Fatalf("nil prior deleted the scale set: %#v", rotated)
	}
	if rotated.ListenerID == first.ListenerID {
		t.Fatalf("nil prior did not open a new session: %#v", rotated)
	}
}

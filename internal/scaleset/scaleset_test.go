package scaleset

import (
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

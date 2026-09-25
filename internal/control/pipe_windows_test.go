//go:build windows

package control

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"
)

func TestCurrentUserNamedPipeRoundTrip(t *testing.T) {
	handler := &testHandler{}
	_, sddl, err := CurrentUserPipe()
	if err != nil {
		t.Fatalf("CurrentUserPipe: %v", err)
	}
	path := fmt.Sprintf(`\\.\pipe\ci-runner-test-%d-%d`, os.Getpid(), time.Now().UnixNano())
	server, err := newServerAt(path, sddl, handler)
	if err != nil {
		t.Fatalf("newServerAt: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	client, err := newClientAt(path)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	status, err := client.Status(context.Background())
	if err != nil {
		cancel()
		t.Fatalf("Status: %v", err)
	}
	if status.ProcessID != 123 || status.ActiveJobCount != 0 {
		cancel()
		t.Fatalf("unexpected status: %#v", status)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("named-pipe server did not stop after cancellation")
	}
}

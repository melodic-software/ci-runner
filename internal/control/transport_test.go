package control

import (
	"bytes"
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/melodic-software/ci-runner/internal/model"
)

type testHandler struct {
	mu        sync.Mutex
	committed string
	aborted   string
}

func (h *testHandler) Handle(_ context.Context, request Request) Response {
	return Response{OK: true, Status: &Status{
		Phase: model.PhaseDisabled, ProcessID: 123, Version: "test", ActiveJobCount: 0,
		ShuttingDown: request.Operation == OperationShutdown,
	}}
}

func (h *testHandler) CommitShutdown(requestID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.committed = requestID
}

func (h *testHandler) AbortShutdown(requestID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.aborted = requestID
}

func TestTransportStatusAndTwoPhaseShutdown(t *testing.T) {
	serverConnection, clientConnection := net.Pipe()
	handler := &testHandler{}
	server, err := NewServer(&singleConnectionListener{connection: serverConnection}, handler)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = server.Serve(ctx) }()
	client, err := NewClient(func(context.Context) (net.Conn, error) { return clientConnection, nil })
	if err != nil {
		t.Fatal(err)
	}
	status, err := client.Shutdown(context.Background(), "test restart", Status{}, true)
	if err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if !status.ShuttingDown || status.ActiveJobCount != 0 {
		t.Fatalf("unexpected status: %#v", status)
	}
	deadline := time.Now().Add(time.Second)
	for {
		handler.mu.Lock()
		committed := handler.committed
		handler.mu.Unlock()
		if committed != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("shutdown was not committed after response")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestServerCloseInterruptsClientThatNeverFinishesRequest(t *testing.T) {
	serverConnection, clientConnection := net.Pipe()
	defer clientConnection.Close()
	server, err := NewServer(&singleConnectionListener{connection: serverConnection}, &testHandler{})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve(context.Background()) }()
	time.Sleep(time.Millisecond)
	if err := server.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		t.Fatalf("Close: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("stalled current-user client prevented control server shutdown")
	}
}

func TestTransportAbortsShutdownReservationWhenAcceptanceCannotFlush(t *testing.T) {
	t.Parallel()
	handler := &testHandler{}
	server := &Server{handler: handler}
	connection := &failingWriteConnection{Reader: bytes.NewReader([]byte(`{"schemaVersion":1,"requestId":"restart-1","op":"shutdown","shutdown":{"reason":"test"}}` + "\n"))}
	server.serveConnection(context.Background(), connection)
	handler.mu.Lock()
	defer handler.mu.Unlock()
	if handler.committed != "" || handler.aborted != "restart-1" {
		t.Fatalf("committed=%q aborted=%q", handler.committed, handler.aborted)
	}
}

type singleConnectionListener struct {
	connection net.Conn
	once       sync.Once
}

func (l *singleConnectionListener) Accept() (net.Conn, error) {
	var result net.Conn
	l.once.Do(func() { result = l.connection })
	if result == nil {
		return nil, net.ErrClosed
	}
	return result, nil
}

func (*singleConnectionListener) Close() error   { return nil }
func (*singleConnectionListener) Addr() net.Addr { return testAddress("control") }

type testAddress string

func (a testAddress) Network() string { return string(a) }
func (a testAddress) String() string  { return string(a) }

type failingWriteConnection struct{ *bytes.Reader }

func (*failingWriteConnection) Write([]byte) (int, error)        { return 0, errors.New("write failed") }
func (*failingWriteConnection) Close() error                     { return nil }
func (*failingWriteConnection) LocalAddr() net.Addr              { return testAddress("local") }
func (*failingWriteConnection) RemoteAddr() net.Addr             { return testAddress("remote") }
func (*failingWriteConnection) SetDeadline(time.Time) error      { return nil }
func (*failingWriteConnection) SetReadDeadline(time.Time) error  { return nil }
func (*failingWriteConnection) SetWriteDeadline(time.Time) error { return nil }

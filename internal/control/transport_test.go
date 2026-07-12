package control

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/melodic-software/ci-runner/internal/model"
)

type testHandler struct {
	mu                   sync.Mutex
	committed            string
	aborted              string
	lastRequest          Request
	omitRestartRequestID bool
}

func (h *testHandler) Handle(_ context.Context, request Request) Response {
	h.mu.Lock()
	h.lastRequest = request
	h.mu.Unlock()
	status := &Status{
		Phase: model.PhaseDisabled, ProcessID: 123, Version: "test", ActiveJobCount: 0,
		ShuttingDown: request.Operation == OperationShutdown,
	}
	if request.Operation == OperationShutdown && request.Shutdown.RestartViaTaskScheduler && !h.omitRestartRequestID {
		status.RestartRequestID = request.RequestID
	}
	return Response{OK: true, Status: status}
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
	expected := Status{ProcessID: 123, Version: "test"}
	status, err := client.Shutdown(context.Background(), "test restart", expected, true)
	if err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if !status.ShuttingDown || status.ActiveJobCount != 0 {
		t.Fatalf("unexpected status: %#v", status)
	}
	if status.RestartRequestID == "" {
		t.Fatal("restart acceptance omitted its authenticated request ID")
	}
	deadline := time.Now().Add(time.Second)
	for {
		handler.mu.Lock()
		committed := handler.committed
		request := handler.lastRequest
		handler.mu.Unlock()
		if committed != "" {
			if request.Shutdown == nil || request.Shutdown.ExpectedProcessID != expected.ProcessID || request.Shutdown.ExpectedVersion != expected.Version {
				t.Fatalf("shutdown identity preflight = %#v, want pid=%d version=%q", request.Shutdown, expected.ProcessID, expected.Version)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("shutdown was not committed after response")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestTransportRejectsRestartAcceptanceWithoutRequestID(t *testing.T) {
	serverConnection, clientConnection := net.Pipe()
	handler := &testHandler{omitRestartRequestID: true}
	server, err := NewServer(&singleConnectionListener{connection: serverConnection}, handler)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer server.Close()
	go func() { _ = server.Serve(ctx) }()
	client, err := NewClient(func(context.Context) (net.Conn, error) { return clientConnection, nil })
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Shutdown(context.Background(), "test restart", Status{ProcessID: 123, Version: "test"}, true)
	if err == nil || !strings.Contains(err.Error(), "authenticated request ID") {
		t.Fatalf("Shutdown error = %v, want missing authenticated request ID", err)
	}
}

func TestStopForUpdateFallsBackToV017ShutdownShape(t *testing.T) {
	t.Parallel()
	type v017Shutdown struct {
		Reason                    string `json:"reason"`
		ExpectedAssignedJobCount  int    `json:"expectedAssignedJobCount"`
		ExpectedActiveJobCount    int    `json:"expectedActiveJobCount"`
		ExpectedActiveWorkerCount int    `json:"expectedActiveWorkerCount"`
		RestartViaTaskScheduler   bool   `json:"restartViaTaskScheduler"`
	}
	type v017Request struct {
		SchemaVersion int           `json:"schemaVersion"`
		RequestID     string        `json:"requestId"`
		Operation     Operation     `json:"op"`
		Shutdown      *v017Shutdown `json:"shutdown,omitempty"`
	}

	expected := Status{
		ProcessID: 123, Version: "v0.1.7", AssignedJobCount: 3,
		ActiveJobCount: 2, ActiveWorkerCount: 4,
	}
	attempts := 0
	serverResults := make(chan error, 2)
	client, err := NewClient(func(context.Context) (net.Conn, error) {
		serverConnection, clientConnection := net.Pipe()
		attempt := attempts
		attempts++
		go func() {
			defer serverConnection.Close()
			decoder := json.NewDecoder(serverConnection)
			decoder.DisallowUnknownFields()
			var request v017Request
			decodeErr := decoder.Decode(&request)
			response := Response{SchemaVersion: SchemaVersion, RequestID: request.RequestID}
			if attempt == 0 {
				if decodeErr == nil || !strings.Contains(decodeErr.Error(), `unknown field "expectedProcessId"`) {
					serverResults <- fmt.Errorf("current request was not rejected by the v0.1.7 decoder: %v", decodeErr)
					return
				}
				response.ErrorCode = "invalid-request"
				response.Error = fmt.Sprintf("decode control request: %v", decodeErr)
			} else {
				if decodeErr != nil {
					serverResults <- fmt.Errorf("legacy fallback did not decode: %w", decodeErr)
					return
				}
				if request.Operation != OperationShutdown || request.Shutdown == nil ||
					request.Shutdown.Reason != "release update" ||
					request.Shutdown.ExpectedAssignedJobCount != expected.AssignedJobCount ||
					request.Shutdown.ExpectedActiveJobCount != expected.ActiveJobCount ||
					request.Shutdown.ExpectedActiveWorkerCount != expected.ActiveWorkerCount ||
					request.Shutdown.RestartViaTaskScheduler {
					serverResults <- fmt.Errorf("unexpected v0.1.7 fallback request: %#v", request)
					return
				}
				response.OK = true
				status := expected
				status.ShuttingDown = true
				response.Status = &status
			}
			if encodeErr := json.NewEncoder(serverConnection).Encode(response); encodeErr != nil {
				serverResults <- encodeErr
				return
			}
			serverResults <- nil
		}()
		return clientConnection, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	status, err := client.Shutdown(context.Background(), "release update", expected, false)
	if err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if !status.ShuttingDown || status.ProcessID != expected.ProcessID || attempts != 2 {
		t.Fatalf("status=%#v attempts=%d, want accepted v0.1.7 fallback after one rejected current request", status, attempts)
	}
	for range 2 {
		if result := <-serverResults; result != nil {
			t.Fatal(result)
		}
	}
}

func TestRestartNeverFallsBackToLegacyShutdownShape(t *testing.T) {
	t.Parallel()
	attempts := 0
	client, err := NewClient(func(context.Context) (net.Conn, error) {
		serverConnection, clientConnection := net.Pipe()
		attempts++
		go func() {
			defer serverConnection.Close()
			var request Request
			if decodeErr := json.NewDecoder(serverConnection).Decode(&request); decodeErr != nil {
				return
			}
			_ = json.NewEncoder(serverConnection).Encode(Response{
				SchemaVersion: SchemaVersion,
				RequestID:     request.RequestID,
				ErrorCode:     "invalid-request",
				Error:         `decode control request: json: unknown field "expectedProcessId"`,
			})
		}()
		return clientConnection, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Shutdown(context.Background(), "restart", Status{ProcessID: 123, Version: "test"}, true)
	if err == nil || attempts != 1 {
		t.Fatalf("restart error=%v attempts=%d, want one rejected current request and no legacy fallback", err, attempts)
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
	connection := &failingWriteConnection{Reader: bytes.NewReader([]byte(`{"schemaVersion":1,"requestId":"restart-1","op":"shutdown","shutdown":{"reason":"test","expectedProcessId":123,"expectedVersion":"test"}}` + "\n"))}
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

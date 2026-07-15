package control

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
)

const (
	maximumMessageSize = 64 << 10
	// Release builds embed the tag without its leading "v".
	legacyShutdownControllerVersion           = "0.1.7"
	legacyShutdownIdentityFieldRejectionError = `decode control request: json: unknown field "expectedProcessId"`
)

var ErrUnavailable = errors.New("controller control plane is unavailable")

type ResponseError struct {
	Code    string
	Message string
}

func (e *ResponseError) Error() string {
	return fmt.Sprintf("controller rejected request (%s): %s", e.Code, e.Message)
}

type Server struct {
	listener net.Listener
	handler  Handler
	close    sync.Once
	wg       sync.WaitGroup

	connectionsMu sync.Mutex
	connections   map[net.Conn]struct{}
	closed        bool
}

func NewServer(listener net.Listener, handler Handler) (*Server, error) {
	if listener == nil || handler == nil {
		return nil, errors.New("control listener and handler are required")
	}
	return &Server{listener: listener, handler: handler, connections: make(map[net.Conn]struct{})}, nil
}

func (s *Server) Serve(ctx context.Context) error {
	serveContext, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		<-serveContext.Done()
		_ = s.Close()
	}()
	for {
		connection, err := s.listener.Accept()
		if err != nil {
			if serveContext.Err() != nil || errors.Is(err, net.ErrClosed) {
				s.wg.Wait()
				return nil
			}
			return fmt.Errorf("accept control connection: %w", err)
		}
		s.connectionsMu.Lock()
		if s.closed {
			s.connectionsMu.Unlock()
			_ = connection.Close()
			continue
		}
		s.connections[connection] = struct{}{}
		s.connectionsMu.Unlock()
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer func() {
				s.connectionsMu.Lock()
				delete(s.connections, connection)
				s.connectionsMu.Unlock()
				_ = connection.Close()
			}()
			s.serveConnection(serveContext, connection)
		}()
	}
}

func (s *Server) Close() error {
	var err error
	s.close.Do(func() {
		err = s.listener.Close()
		s.connectionsMu.Lock()
		s.closed = true
		for connection := range s.connections {
			err = errors.Join(err, connection.Close())
		}
		s.connectionsMu.Unlock()
	})
	return err
}

func (s *Server) serveConnection(ctx context.Context, connection net.Conn) {
	request, err := readRequest(connection)
	if err != nil {
		_ = writeResponse(connection, ErrorResponse(request.RequestID, "invalid-request", err))
		return
	}
	response := s.handler.Handle(ctx, request)
	response.SchemaVersion = SchemaVersion
	response.RequestID = request.RequestID
	if err := writeResponse(connection, response); err != nil {
		if request.Operation == OperationShutdown && response.OK {
			s.handler.AbortShutdown(request.RequestID)
		}
		return
	}
	if request.Operation == OperationShutdown && response.OK {
		s.handler.CommitShutdown(request.RequestID)
	}
}

func readRequest(reader io.Reader) (Request, error) {
	lineReader := bufio.NewReaderSize(reader, maximumMessageSize+1)
	line, err := lineReader.ReadBytes('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return Request{}, fmt.Errorf("read control request: %w", err)
	}
	if len(line) == 0 {
		return Request{}, errors.New("empty control request")
	}
	if len(line) > maximumMessageSize {
		return Request{}, fmt.Errorf("control request exceeds %d bytes", maximumMessageSize)
	}
	decoder := json.NewDecoder(bytes.NewReader(line))
	decoder.DisallowUnknownFields()
	var request Request
	if err := decoder.Decode(&request); err != nil {
		return request, fmt.Errorf("decode control request: %w", err)
	}
	if err := requireEOF(decoder); err != nil {
		return request, err
	}
	if err := request.Validate(); err != nil {
		return request, err
	}
	return request, nil
}

func writeResponse(writer io.Writer, response Response) error {
	buffered := bufio.NewWriterSize(writer, 4096)
	if err := json.NewEncoder(buffered).Encode(response); err != nil {
		return fmt.Errorf("encode control response: %w", err)
	}
	if err := buffered.Flush(); err != nil {
		return fmt.Errorf("flush control response: %w", err)
	}
	return nil
}

type Client struct {
	dial func(context.Context) (net.Conn, error)
}

// legacyShutdownEnvelope is the exact shutdown wire shape shipped through
// v0.1.7. A replacement CLI first attempts the current identity-bound request,
// then uses this shape only when an older controller explicitly rejects one of
// the added identity fields before dispatch. That preserves stop-for-update
// across the upgrade boundary without weakening current-controller preflight.
type legacyShutdownEnvelope struct {
	SchemaVersion int                   `json:"schemaVersion"`
	RequestID     string                `json:"requestId"`
	Operation     Operation             `json:"op"`
	Shutdown      legacyShutdownRequest `json:"shutdown"`
}

type legacyShutdownRequest struct {
	Reason                    string `json:"reason"`
	ExpectedAssignedJobCount  int    `json:"expectedAssignedJobCount"`
	ExpectedActiveJobCount    int    `json:"expectedActiveJobCount"`
	ExpectedActiveWorkerCount int    `json:"expectedActiveWorkerCount"`
	RestartViaTaskScheduler   bool   `json:"restartViaTaskScheduler"`
}

func NewClient(dial func(context.Context) (net.Conn, error)) (*Client, error) {
	if dial == nil {
		return nil, errors.New("control dialer is required")
	}
	return &Client{dial: dial}, nil
}

func (c *Client) Status(ctx context.Context) (Status, error) {
	requestID, err := newRequestID()
	if err != nil {
		return Status{}, err
	}
	response, err := c.roundTrip(ctx, Request{
		SchemaVersion: SchemaVersion,
		RequestID:     requestID,
		Operation:     OperationStatus,
	})
	if err != nil {
		return Status{}, err
	}
	if response.Status == nil {
		return Status{}, errors.New("controller status response is missing status")
	}
	return *response.Status, nil
}

func (c *Client) Shutdown(ctx context.Context, reason string, expected Status, restart bool) (Status, error) {
	requestID, err := newRequestID()
	if err != nil {
		return Status{}, err
	}
	request := Request{
		SchemaVersion: SchemaVersion,
		RequestID:     requestID,
		Operation:     OperationShutdown,
		Shutdown: &ShutdownRequest{
			Reason:                    reason,
			ExpectedProcessID:         expected.ProcessID,
			ExpectedVersion:           expected.Version,
			ExpectedAssignedJobCount:  expected.AssignedJobCount,
			ExpectedActiveJobCount:    expected.ActiveJobCount,
			ExpectedActiveWorkerCount: expected.ActiveWorkerCount,
			RestartViaTaskScheduler:   restart,
		},
	}
	response, err := c.roundTrip(ctx, request)
	if !restart && legacyShutdownFieldRejection(err, expected.Version) {
		response, err = c.roundTripMessage(ctx, requestID, legacyShutdownEnvelope{
			SchemaVersion: SchemaVersion,
			RequestID:     requestID,
			Operation:     OperationShutdown,
			Shutdown: legacyShutdownRequest{
				Reason:                    reason,
				ExpectedAssignedJobCount:  expected.AssignedJobCount,
				ExpectedActiveJobCount:    expected.ActiveJobCount,
				ExpectedActiveWorkerCount: expected.ActiveWorkerCount,
				RestartViaTaskScheduler:   false,
			},
		})
	}
	if err != nil {
		return Status{}, err
	}
	if response.Status == nil {
		return Status{}, errors.New("controller shutdown response is missing status")
	}
	if restart && response.Status.RestartRequestID != requestID {
		return Status{}, errors.New("controller restart response is missing the authenticated request ID")
	}
	return *response.Status, nil
}

func legacyShutdownFieldRejection(err error, expectedVersion string) bool {
	if expectedVersion != legacyShutdownControllerVersion {
		return false
	}
	var responseErr *ResponseError
	if !errors.As(err, &responseErr) || responseErr.Code != "invalid-request" {
		return false
	}
	return responseErr.Message == legacyShutdownIdentityFieldRejectionError
}

func (c *Client) ForceStopPreview(ctx context.Context) ([]ForceStopTarget, error) {
	requestID, err := newRequestID()
	if err != nil {
		return nil, err
	}
	response, err := c.roundTrip(ctx, Request{SchemaVersion: SchemaVersion, RequestID: requestID, Operation: OperationForceStopPreview})
	if err != nil {
		return nil, err
	}
	return append([]ForceStopTarget(nil), response.ForceStopTargets...), nil
}

func (c *Client) ForceStopExecute(ctx context.Context, expected []ForceStopTarget) ([]ForceStopTarget, error) {
	requestID, err := newRequestID()
	if err != nil {
		return nil, err
	}
	response, err := c.roundTrip(ctx, Request{
		SchemaVersion: SchemaVersion, RequestID: requestID, Operation: OperationForceStopExecute,
		ForceStop: &ForceStopRequest{Expected: append([]ForceStopTarget(nil), expected...)},
	})
	if err != nil {
		return nil, err
	}
	return append([]ForceStopTarget(nil), response.ForceStopTargets...), nil
}

func (c *Client) roundTrip(ctx context.Context, request Request) (Response, error) {
	if err := request.Validate(); err != nil {
		return Response{}, err
	}
	return c.roundTripMessage(ctx, request.RequestID, request)
}

func (c *Client) roundTripMessage(ctx context.Context, requestID string, request any) (_ Response, resultErr error) {
	connection, err := c.dial(ctx)
	if err != nil {
		return Response{}, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = connection.Close()
		case <-done:
		}
	}()
	defer close(done)
	defer func() {
		if closeErr := connection.Close(); closeErr != nil && !errors.Is(closeErr, net.ErrClosed) {
			resultErr = errors.Join(resultErr, fmt.Errorf("close control connection: %w", closeErr))
		}
	}()
	if err := json.NewEncoder(connection).Encode(request); err != nil {
		return Response{}, fmt.Errorf("write control request: %w", err)
	}
	lineReader := bufio.NewReaderSize(connection, maximumMessageSize+1)
	line, err := lineReader.ReadBytes('\n')
	if err != nil {
		return Response{}, fmt.Errorf("read control response: %w", err)
	}
	if len(line) > maximumMessageSize {
		return Response{}, errors.New("control response exceeds safety limit")
	}
	decoder := json.NewDecoder(bytes.NewReader(line))
	decoder.DisallowUnknownFields()
	var response Response
	if err := decoder.Decode(&response); err != nil {
		return Response{}, fmt.Errorf("decode control response: %w", err)
	}
	if err := requireEOF(decoder); err != nil {
		return Response{}, err
	}
	if response.SchemaVersion != SchemaVersion || response.RequestID != requestID {
		return Response{}, errors.New("control response schema or request ID mismatch")
	}
	if !response.OK {
		return Response{}, &ResponseError{Code: response.ErrorCode, Message: response.Error}
	}
	return response, nil
}

func requireEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("control message contains multiple JSON values")
		}
		return fmt.Errorf("decode control message trailer: %w", err)
	}
	return nil
}

func newRequestID() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", fmt.Errorf("generate control request ID: %w", err)
	}
	return hex.EncodeToString(value), nil
}

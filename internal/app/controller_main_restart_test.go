package app

import (
	"context"
	"errors"
	"testing"

	"github.com/melodic-software/ci-runner/internal/controller"
	"github.com/melodic-software/ci-runner/internal/model"
)

type recordingRestartReceiptWriter struct {
	receipts          []model.RestartReceipt
	err               error
	appendBeforeError bool
}

func (w *recordingRestartReceiptWriter) SaveRestartReceipt(_ context.Context, receipt model.RestartReceipt) error {
	if w.err != nil && !w.appendBeforeError {
		return w.err
	}
	w.receipts = append(w.receipts, receipt)
	return w.err
}

func TestCompleteControllerShutdownDoesNotAuthorizeVisiblePartialReceipt(t *testing.T) {
	t.Parallel()
	writeFailure := errors.New("directory flush failed after atomic replace")
	writer := &recordingRestartReceiptWriter{err: writeFailure, appendBeforeError: true}
	err := completeControllerShutdown(context.Background(), nil, controller.ShutdownSignal{
		RequestID: "restart-request-1", Restart: true,
	}, writer, 4242, "1.2.3")
	if !errors.Is(err, writeFailure) || errors.Is(err, ErrControllerRestartRequested) {
		t.Fatalf("error = %v, want ordinary receipt failure without restart sentinel", err)
	}
	if len(writer.receipts) != 1 {
		t.Fatalf("test did not simulate a visible partial receipt: %#v", writer.receipts)
	}
}

func TestCompleteControllerShutdownPersistsExactRestartReceipt(t *testing.T) {
	t.Parallel()
	writer := &recordingRestartReceiptWriter{}
	err := completeControllerShutdown(context.Background(), nil, controller.ShutdownSignal{
		RequestID: "restart-request-1", Reason: "test restart", Restart: true,
	}, writer, 4242, "1.2.3")
	if !errors.Is(err, ErrControllerRestartRequested) {
		t.Fatalf("error = %v, want restart sentinel", err)
	}
	if len(writer.receipts) != 1 {
		t.Fatalf("receipts = %#v, want one", writer.receipts)
	}
	receipt := writer.receipts[0]
	if receipt.SchemaVersion != 1 || receipt.RequestID != "restart-request-1" ||
		receipt.ProcessID != 4242 || receipt.Version != "1.2.3" || receipt.CompletedAt.IsZero() {
		t.Fatalf("receipt = %#v", receipt)
	}
}

func TestCompleteControllerShutdownDegradedDrainCompletesRestartOnly(t *testing.T) {
	t.Parallel()
	writer := &recordingRestartReceiptWriter{}
	err := completeControllerShutdown(context.Background(), controller.ErrShutdownDegraded, controller.ShutdownSignal{
		RequestID: "restart-request-1", Reason: "degraded restart", Restart: true,
	}, writer, 4242, "1.2.3")
	if !errors.Is(err, ErrControllerRestartRequested) {
		t.Fatalf("error = %v, want restart sentinel for a degraded drain under restart", err)
	}
	if len(writer.receipts) != 1 {
		t.Fatalf("receipts = %#v, want one", writer.receipts)
	}
}

func TestCompleteControllerShutdownDegradedDrainFailsClosedWithoutRestart(t *testing.T) {
	t.Parallel()
	writer := &recordingRestartReceiptWriter{}
	err := completeControllerShutdown(context.Background(), controller.ErrShutdownDegraded, controller.ShutdownSignal{
		Reason: "stop for update", Restart: false,
	}, writer, 4242, "1.2.3")
	if !errors.Is(err, controller.ErrShutdownDegraded) || errors.Is(err, ErrControllerRestartRequested) {
		t.Fatalf("error = %v, want the degraded drain surfaced, not a safe-stop result", err)
	}
	if len(writer.receipts) != 0 {
		t.Fatalf("receipts = %#v, want none", writer.receipts)
	}
}

func TestCompleteControllerShutdownFailsClosedWithoutReceipt(t *testing.T) {
	t.Parallel()
	shutdownFailure := errors.New("runtime close failed")
	receiptFailure := errors.New("durable write failed")
	tests := map[string]struct {
		shutdownErr error
		signal      controller.ShutdownSignal
		writer      *recordingRestartReceiptWriter
	}{
		"shutdown failure": {
			shutdownErr: shutdownFailure,
			signal:      controller.ShutdownSignal{RequestID: "restart-request-1", Restart: true},
			writer:      &recordingRestartReceiptWriter{},
		},
		"receipt failure": {
			signal: controller.ShutdownSignal{RequestID: "restart-request-1", Restart: true},
			writer: &recordingRestartReceiptWriter{err: receiptFailure},
		},
		"missing request ID": {
			signal: controller.ShutdownSignal{Restart: true},
			writer: &recordingRestartReceiptWriter{},
		},
		"ordinary stop": {
			signal: controller.ShutdownSignal{RequestID: "stop-request-1"},
			writer: &recordingRestartReceiptWriter{},
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := completeControllerShutdown(context.Background(), test.shutdownErr, test.signal, test.writer, 4242, "1.2.3")
			if errors.Is(err, ErrControllerRestartRequested) {
				t.Fatalf("ordinary failure produced restart sentinel: %v", err)
			}
			if len(test.writer.receipts) != 0 {
				t.Fatalf("failure produced receipt: %#v", test.writer.receipts)
			}
			if name == "shutdown failure" && !errors.Is(err, shutdownFailure) {
				t.Fatalf("error = %v, want shutdown failure", err)
			}
			if name == "receipt failure" && !errors.Is(err, receiptFailure) {
				t.Fatalf("error = %v, want receipt failure", err)
			}
			if name == "ordinary stop" && err != nil {
				t.Fatalf("ordinary stop error = %v", err)
			}
		})
	}
}

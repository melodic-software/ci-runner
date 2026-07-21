package control

import (
	"strings"
	"testing"
)

func TestShutdownRequestValidateRequiresExpectedControllerIdentity(t *testing.T) {
	t.Parallel()
	valid := Request{
		SchemaVersion: SchemaVersion,
		RequestID:     "shutdown-identity-1",
		Operation:     OperationShutdown,
		Shutdown: &ShutdownRequest{
			Reason: "test", ExpectedProcessID: 1234, ExpectedVersion: "1.2.3",
		},
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid shutdown request: %v", err)
	}
	tests := map[string]func(*ShutdownRequest){
		"missing process ID": func(request *ShutdownRequest) { request.ExpectedProcessID = 0 },
		"missing version":    func(request *ShutdownRequest) { request.ExpectedVersion = " " },
		"long version":       func(request *ShutdownRequest) { request.ExpectedVersion = strings.Repeat("v", 129) },
	}
	for name, invalidate := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			request := valid
			shutdown := *valid.Shutdown
			request.Shutdown = &shutdown
			invalidate(request.Shutdown)
			if err := request.Validate(); err == nil {
				t.Fatal("shutdown request without a valid expected controller identity was accepted")
			}
		})
	}
}

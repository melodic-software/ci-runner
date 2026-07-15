package control

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
)

func TestReadRequestRejectsAmbiguousWireShapes(t *testing.T) {
	tests := map[string][]byte{
		"unknown field": []byte(`{"schemaVersion":1,"requestId":"unknown-1","op":"status","unexpected":true}` + "\n"),
		"trailing JSON": []byte(`{"schemaVersion":1,"requestId":"trailing-1","op":"status"} {}` + "\n"),
		"over limit":    append(bytes.Repeat([]byte{'x'}, maximumMessageSize), '\n'),
	}
	for name, input := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := readRequest(bytes.NewReader(input)); err == nil {
				t.Fatal("readRequest accepted ambiguous or oversized wire input")
			}
		})
	}
}

func FuzzReadRequest(f *testing.F) {
	f.Add([]byte(`{"schemaVersion":1,"requestId":"status-1","op":"status"}` + "\n"))
	f.Add([]byte(`{"schemaVersion":1,"requestId":"shutdown-1","op":"shutdown","shutdown":{"reason":"release update","expectedProcessId":123,"expectedVersion":"1.2.3","expectedAssignedJobCount":0,"expectedActiveJobCount":0,"expectedActiveWorkerCount":0,"restartViaTaskScheduler":false}}`))
	f.Add([]byte(`{"schemaVersion":1,"requestId":"unknown-1","op":"status","unexpected":true}` + "\n"))
	f.Add([]byte(`{"schemaVersion":1,"requestId":"trailing-1","op":"status"} {}` + "\n"))
	f.Add(append(bytes.Repeat([]byte{'x'}, maximumMessageSize), '\n'))

	f.Fuzz(func(t *testing.T, input []byte) {
		firstMessageLength := len(input)
		if newline := bytes.IndexByte(input, '\n'); newline >= 0 {
			firstMessageLength = newline + 1
		}

		request, err := readRequest(bytes.NewReader(input))
		if firstMessageLength > maximumMessageSize {
			if err == nil {
				t.Fatalf("accepted %d-byte control message above %d-byte limit", firstMessageLength, maximumMessageSize)
			}
			return
		}
		if err != nil {
			return
		}
		if err := request.Validate(); err != nil {
			t.Fatalf("accepted request that fails validation: %v", err)
		}

		canonical, err := json.Marshal(request)
		if err != nil {
			t.Fatalf("marshal accepted request: %v", err)
		}
		canonical = append(canonical, '\n')
		roundTrip, err := readRequest(bytes.NewReader(canonical))
		if err != nil {
			t.Fatalf("re-read canonical accepted request: %v", err)
		}
		if !reflect.DeepEqual(roundTrip, request) {
			t.Fatalf("canonical request changed after round trip: got %#v, want %#v", roundTrip, request)
		}
	})
}

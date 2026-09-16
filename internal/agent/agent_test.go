package agent

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	protocol "github.com/kaeawc/spectra-protocol/protocol/v1"
)

type fakeRunner struct {
	version string
	err     error
}

func (f fakeRunner) Version(context.Context) (string, error) { return f.version, f.err }
func (f fakeRunner) Inspect(context.Context, protocol.InspectParams) (json.RawMessage, error) {
	return json.RawMessage(`{"ok":true}`), f.err
}
func (f fakeRunner) SnapshotCreate(context.Context, protocol.SnapshotCreateParams) (json.RawMessage, error) {
	return json.RawMessage(`{"id":"snapshot-1"}`), f.err
}

func TestHandleHealthAdvertisesTypedCapabilities(t *testing.T) {
	a := Agent{Runner: fakeRunner{version: "1.2.3"}, AgentVersion: "0.1.0"}
	response := a.Handle(context.Background(), protocol.Request{
		ProtocolVersion: protocol.Version,
		RequestID:       "request-1",
		Operation:       protocol.OperationHealth,
	})
	if response.Error != nil {
		t.Fatalf("Handle() error = %+v", response.Error)
	}
	var health protocol.HealthResult
	if err := json.Unmarshal(response.Result, &health); err != nil {
		t.Fatal(err)
	}
	if health.Capabilities.SpectraVersion != "1.2.3" {
		t.Fatalf("SpectraVersion = %q", health.Capabilities.SpectraVersion)
	}
	if len(health.Capabilities.Operations) != 3 {
		t.Fatalf("operations = %v", health.Capabilities.Operations)
	}
}

func TestHandleRejectsUnknownOperation(t *testing.T) {
	a := Agent{Runner: fakeRunner{version: "1.2.3"}}
	response := a.Handle(context.Background(), protocol.Request{
		ProtocolVersion: protocol.Version,
		RequestID:       "request-1",
		Operation:       "shell.exec",
	})
	if response.Error == nil || response.Error.Code != "unsupported_operation" {
		t.Fatalf("Handle() error = %+v", response.Error)
	}
}

func TestHandleRejectsUnknownParameters(t *testing.T) {
	a := Agent{Runner: fakeRunner{version: "1.2.3"}}
	response := a.Handle(context.Background(), protocol.Request{
		ProtocolVersion: protocol.Version,
		RequestID:       "request-1",
		Operation:       protocol.OperationInspect,
		Params:          json.RawMessage(`{"app_paths":["/Applications/Test.app"],"command":"rm -rf"}`),
	})
	if response.Error == nil || response.Error.Code != "invalid_request" {
		t.Fatalf("Handle() error = %+v", response.Error)
	}
}

func TestRequestTimeoutIsBoundedByAgentPolicy(t *testing.T) {
	a := Agent{MaxRunDuration: 2 * time.Second}
	if got := a.requestTimeout(protocol.Request{TimeoutMS: 500}); got != 500*time.Millisecond {
		t.Fatalf("short timeout = %s", got)
	}
	if got := a.requestTimeout(protocol.Request{TimeoutMS: 5000}); got != 2*time.Second {
		t.Fatalf("long timeout = %s", got)
	}
}

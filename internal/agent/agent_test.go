package agent

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	protocol "github.com/kaeawc/spectra-protocol/protocol/v1"
)

type recordingAuditor struct{ events []AuditEvent }

func (a *recordingAuditor) Record(event AuditEvent) error {
	a.events = append(a.events, event)
	return nil
}

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

func TestHandleAuditsOutcomeWithoutParameters(t *testing.T) {
	auditor := &recordingAuditor{}
	wantAt := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	a := Agent{Runner: fakeRunner{version: "1.2.3"}, Auditor: auditor, Now: func() time.Time { return wantAt }}
	response := a.Handle(context.Background(), protocol.Request{
		ProtocolVersion: protocol.Version,
		RequestID:       "request-1",
		Operation:       protocol.OperationInspect,
		Params:          json.RawMessage(`{"app_paths":["/Applications/Secret.app"]}`),
	})
	if response.Error != nil {
		t.Fatalf("Handle() error = %+v", response.Error)
	}
	if len(auditor.events) != 1 {
		t.Fatalf("audit events = %d", len(auditor.events))
	}
	event := auditor.events[0]
	if !event.At.Equal(wantAt) || event.RequestID != "request-1" || event.Operation != protocol.OperationInspect || event.Outcome != "succeeded" {
		t.Fatalf("audit event = %+v", event)
	}
}

func TestHandleAuditsRejectedRequestCode(t *testing.T) {
	auditor := &recordingAuditor{}
	a := Agent{Runner: fakeRunner{version: "1.2.3"}, Auditor: auditor}
	response := a.Handle(context.Background(), protocol.Request{
		ProtocolVersion: protocol.Version,
		RequestID:       "request-2",
		Operation:       "shell.exec",
	})
	if response.Error == nil {
		t.Fatal("Handle() unexpectedly succeeded")
	}
	if len(auditor.events) != 1 {
		t.Fatalf("audit events = %d", len(auditor.events))
	}
	if event := auditor.events[0]; event.Outcome != "rejected" || event.ErrorCode != "unsupported_operation" {
		t.Fatalf("audit event = %+v", event)
	}
}

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	protocol "github.com/kaeawc/spectra-protocol/protocol/v1"
)

type fakeRunner struct {
	caps     protocol.SpectraCapabilities
	capsErr  error
	inspect  func(context.Context, protocol.InspectParams) (json.RawMessage, error)
	snapshot func(context.Context, protocol.SnapshotCreateParams) (json.RawMessage, error)
}

func fixtureCaps(t *testing.T) protocol.SpectraCapabilities {
	t.Helper()
	data, err := os.ReadFile("testdata/core-capabilities.json")
	if err != nil {
		t.Fatal(err)
	}
	caps, err := protocol.DecodeSpectraCapabilities(data)
	if err != nil {
		t.Fatal(err)
	}
	return caps
}
func (f fakeRunner) Capabilities(context.Context) (protocol.SpectraCapabilities, error) {
	return f.caps, f.capsErr
}
func (f fakeRunner) Inspect(ctx context.Context, p protocol.InspectParams) (json.RawMessage, error) {
	if f.inspect != nil {
		return f.inspect(ctx, p)
	}
	return json.RawMessage(`{"ok":true}`), nil
}
func (f fakeRunner) SnapshotCreate(ctx context.Context, p protocol.SnapshotCreateParams) (json.RawMessage, error) {
	if f.snapshot != nil {
		return f.snapshot(ctx, p)
	}
	return json.RawMessage(`{"id":"snapshot-1"}`), nil
}

// identityRunner is a Runner that also implements binaryIdentifier, letting
// tests simulate `provision update` swapping the installed Spectra binary:
// both its identity and its capabilities can be changed between calls.
type identityRunner struct {
	identity string
	caps     protocol.SpectraCapabilities
	capsErr  error
}

func (r *identityRunner) BinaryIdentity() (string, error) { return r.identity, nil }
func (r *identityRunner) Capabilities(context.Context) (protocol.SpectraCapabilities, error) {
	return r.caps, r.capsErr
}
func (r *identityRunner) Inspect(context.Context, protocol.InspectParams) (json.RawMessage, error) {
	return json.RawMessage(`{"ok":true}`), nil
}
func (r *identityRunner) SnapshotCreate(context.Context, protocol.SnapshotCreateParams) (json.RawMessage, error) {
	return json.RawMessage(`{"id":"snapshot-1"}`), nil
}

type recordingAuditor struct {
	events    []AuditEvent
	failStage string
}

func (a *recordingAuditor) Record(e AuditEvent) error {
	a.events = append(a.events, e)
	if e.Stage == a.failStage {
		return errors.New("disk full")
	}
	return nil
}
func request(op protocol.Operation, params string) protocol.Request {
	return protocol.Request{ProtocolVersion: protocol.Version, RequestID: "req-1", Operation: op, Params: json.RawMessage(params)}
}
func code(t *testing.T, r protocol.Response, want protocol.ErrorCode) {
	t.Helper()
	if r.Error == nil || r.Error.Code != want {
		t.Fatalf("response error = %+v, want %s", r.Error, want)
	}
}

func TestCompatibilityErrorCode(t *testing.T) {
	coded := &protocol.CodedError{Code: protocol.CodeUnsupportedOperation, Err: errors.New("unsupported")}
	if got := compatibilityErrorCode(fmt.Errorf("compatibility check: %w", coded)); got != protocol.CodeUnsupportedOperation {
		t.Errorf("compatibilityErrorCode(wrapped coded error) = %q, want %q", got, protocol.CodeUnsupportedOperation)
	}
	if got := compatibilityErrorCode(errors.New("boom")); got != protocol.CodeIncompatibleSpectra {
		t.Errorf("compatibilityErrorCode(plain error) = %q, want %q", got, protocol.CodeIncompatibleSpectra)
	}
}

func healthManifest(t *testing.T, a *Agent) protocol.CapabilityManifest {
	t.Helper()
	resp := a.Handle(context.Background(), request(protocol.OperationHealth, ""))
	if resp.Error != nil {
		t.Fatalf("health: %+v", resp.Error)
	}
	var health protocol.HealthResult
	if err := json.Unmarshal(resp.Result, &health); err != nil {
		t.Fatal(err)
	}
	if err := health.Capabilities.Validate(); err != nil {
		t.Fatalf("manifest: %v", err)
	}
	return health.Capabilities
}
func TestHealthManifestAndDiagnosticResult(t *testing.T) {
	a := &Agent{Runner: fakeRunner{caps: fixtureCaps(t)}, AgentVersion: "agent-1", Policy: Policy{AllowSnapshot: true}}
	manifest := healthManifest(t, a)
	if manifest.SpectraVersion != "dev" || len(manifest.Operations) != 3 {
		t.Fatalf("manifest: %+v", manifest)
	}
	resp := a.Handle(context.Background(), request(protocol.OperationInspect, `{"app_paths":["/Applications/Test.app"]}`))
	if resp.Error != nil {
		t.Fatal(resp.Error)
	}
	var result protocol.DiagnosticResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatal(err)
	}
	if result.Schema != (protocol.SchemaRef{Name: protocol.SchemaInspect, Version: 1}) || result.SpectraVersion != "dev" || string(result.Data) != `{"ok":true}` {
		t.Fatalf("diagnostic result: %+v", result)
	}
	if _, err := protocol.DecodeResult(resp, protocol.OperationInspect); err != nil {
		t.Fatal(err)
	}
}
func TestSnapshotPolicyAndCapabilityCompatibility(t *testing.T) {
	caps := fixtureCaps(t)
	a := &Agent{Runner: fakeRunner{caps: caps}}
	code(t, a.Handle(context.Background(), request(protocol.OperationSnapshotCreate, `{}`)), protocol.CodePermissionDenied)
	if _, ok := healthManifest(t, a).Supports(protocol.OperationSnapshotCreate); ok {
		t.Fatal("snapshot advertised by default")
	}
	a.Policy.AllowSnapshot = true
	code(t, a.Handle(context.Background(), request(protocol.OperationSnapshotCreate, `{"include_apps":true}`)), protocol.CodePermissionDenied)
	if _, ok := healthManifest(t, a).Supports(protocol.OperationSnapshotCreate); !ok {
		t.Fatal("snapshot absent with base policy")
	}
	a.Policy.AllowSnapshotApps = true
	if r := a.Handle(context.Background(), request(protocol.OperationSnapshotCreate, `{"include_apps":true}`)); r.Error != nil {
		t.Fatal(r.Error)
	}
}
func TestIncompatibleSpectra(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*protocol.SpectraCapabilities)
		err    error
		op     protocol.Operation
	}{
		{"schema version", func(c *protocol.SpectraCapabilities) {
			for i := range c.Interfaces {
				if c.Interfaces[i].Name == "snapshot" {
					c.Interfaces[i].ResultSchema.Version = 2
				}
			}
		}, nil, protocol.OperationSnapshotCreate},
		{"missing snapshot", func(c *protocol.SpectraCapabilities) { c.Interfaces = c.Interfaces[:2] }, nil, protocol.OperationSnapshotCreate},
		{"capabilities call failed", nil, errors.New("unknown command"), protocol.OperationInspect},
	} {
		t.Run(tc.name, func(t *testing.T) {
			caps := fixtureCaps(t)
			if tc.change != nil {
				tc.change(&caps)
			}
			a := &Agent{Runner: fakeRunner{caps: caps, capsErr: tc.err}, Policy: Policy{AllowSnapshot: true}}
			params := `{}`
			if tc.op == protocol.OperationInspect {
				params = `{"app_paths":["/Applications/A.app"]}`
			}
			code(t, a.Handle(context.Background(), request(tc.op, params)), protocol.CodeIncompatibleSpectra)
			if _, ok := healthManifest(t, a).Supports(tc.op); ok {
				t.Fatal("incompatible operation advertised")
			}
		})
	}
}
func TestStaleCapabilityCacheRefreshesOnBinaryIdentityChange(t *testing.T) {
	v1 := fixtureCaps(t)
	v1.SpectraVersion = "v1.0.0"
	runner := &identityRunner{identity: "spectra-v1|100|1", caps: v1}
	a := &Agent{Runner: runner, Policy: Policy{AllowSnapshot: true}}

	inspectVersion := func() string {
		t.Helper()
		resp := a.Handle(context.Background(), request(protocol.OperationInspect, `{"app_paths":["/Applications/A.app"]}`))
		if resp.Error != nil {
			t.Fatalf("inspect: %+v", resp.Error)
		}
		var result protocol.DiagnosticResult
		if err := json.Unmarshal(resp.Result, &result); err != nil {
			t.Fatal(err)
		}
		return result.SpectraVersion
	}
	if got := inspectVersion(); got != "v1.0.0" {
		t.Fatalf("SpectraVersion = %q, want v1.0.0", got)
	}

	// `provision update` swaps the current symlink: a new binary identity and
	// capabilities, but no `health` call happens in between.
	v2 := fixtureCaps(t)
	v2.SpectraVersion = "v2.0.0"
	runner.identity = "spectra-v2|200|2"
	runner.caps = v2
	if got := inspectVersion(); got != "v2.0.0" {
		t.Fatalf("SpectraVersion after binary swap = %q, want v2.0.0 (stale cache not refreshed)", got)
	}

	// A further swap to a schema-incompatible binary must be caught on the
	// very next inspect, again without any intervening health call.
	incompatible := fixtureCaps(t)
	for i := range incompatible.Interfaces {
		if incompatible.Interfaces[i].Name == "inspect" {
			incompatible.Interfaces[i].ResultSchema.Version = 99
		}
	}
	runner.identity = "spectra-v3|300|3"
	runner.caps = incompatible
	code(t, a.Handle(context.Background(), request(protocol.OperationInspect, `{"app_paths":["/Applications/A.app"]}`)), protocol.CodeIncompatibleSpectra)
}

func TestAuditStagesPeerAndFailures(t *testing.T) {
	caps := fixtureCaps(t)
	peer := Peer{Transport: "tsnet", LoginName: "alice@example.com", NodeName: "mac", Address: "100.1.2.3:7"}
	ctx := WithPeer(context.Background(), peer)
	auditor := &recordingAuditor{failStage: "started"}
	invoked := false
	a := &Agent{Runner: fakeRunner{caps: caps, inspect: func(context.Context, protocol.InspectParams) (json.RawMessage, error) {
		invoked = true
		return nil, nil
	}}, Auditor: auditor}
	code(t, a.Handle(ctx, request(protocol.OperationInspect, `{"app_paths":["/Applications/A.app"]}`)), protocol.CodeUnavailable)
	if invoked || len(auditor.events) != 1 || auditor.events[0].Peer != peer || auditor.events[0].Stage != "started" {
		t.Fatalf("audit=%+v invoked=%t", auditor.events, invoked)
	}
	auditor = &recordingAuditor{failStage: "completed"}
	var logs []string
	a = &Agent{Runner: fakeRunner{caps: caps}, Auditor: auditor, Logf: func(format string, args ...any) { logs = append(logs, format) }}
	resp := a.Handle(ctx, request(protocol.OperationInspect, `{"app_paths":["/Applications/A.app"]}`))
	if resp.Error != nil || len(logs) != 1 || !strings.Contains(logs[0], "audit completed write failed") {
		t.Fatalf("response=%+v logs=%v", resp, logs)
	}
	if len(auditor.events) != 2 || auditor.events[0].Stage != "started" || auditor.events[1].Stage != "completed" || auditor.events[1].Peer != peer {
		t.Fatalf("audit=%+v", auditor.events)
	}
	a.Runner = fakeRunner{caps: caps, inspect: func(context.Context, protocol.InspectParams) (json.RawMessage, error) {
		return nil, errors.New("failed")
	}}
	code(t, a.Handle(ctx, request(protocol.OperationInspect, `{"app_paths":["/Applications/A.app"]}`)), protocol.CodeExecutionFailed)
	if len(logs) != 2 {
		t.Fatalf("failure audit log missing: %v", logs)
	}
}
func TestRejectedRequestAudit(t *testing.T) {
	auditor := &recordingAuditor{failStage: "completed"}
	a := &Agent{Runner: fakeRunner{caps: fixtureCaps(t)}, Auditor: auditor}
	code(t, a.Handle(context.Background(), request("future.operation", "")), protocol.CodeUnsupportedOperation)
	if len(auditor.events) != 1 || auditor.events[0].Outcome != "rejected" {
		t.Fatalf("audit=%+v", auditor.events)
	}
}
func TestSessionTimeoutCancelsRunner(t *testing.T) {
	observed := make(chan time.Time, 1)
	a := &Agent{Runner: fakeRunner{caps: fixtureCaps(t), inspect: func(ctx context.Context, _ protocol.InspectParams) (json.RawMessage, error) {
		<-ctx.Done()
		observed <- time.Now()
		return nil, ctx.Err()
	}}, MaxRunDuration: 5 * time.Second}
	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Millisecond)
	defer cancel()
	deadline, _ := ctx.Deadline()
	resp := a.Handle(ctx, request(protocol.OperationInspect, `{"app_paths":["/Applications/A.app"]}`))
	code(t, resp, protocol.CodeTimeout)
	select {
	case at := <-observed:
		if at.After(deadline.Add(200 * time.Millisecond)) {
			t.Fatalf("runner canceled too late: %s", at.Sub(deadline))
		}
	case <-time.After(time.Second):
		t.Fatal("runner not canceled")
	}
}

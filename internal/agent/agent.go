// Package agent exposes a transport-neutral, typed bridge to a locally installed Spectra binary.
package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	protocol "github.com/kaeawc/spectra-protocol/protocol/v1"
)

// Runner performs only supported local diagnostic operations.
type Runner interface {
	Capabilities(context.Context) (protocol.SpectraCapabilities, error)
	Inspect(context.Context, protocol.InspectParams) (json.RawMessage, error)
	SnapshotCreate(context.Context, protocol.SnapshotCreateParams) (json.RawMessage, error)
}

// binaryIdentifier is an optional Runner capability that reports a value
// identifying the on-disk Spectra binary currently in effect. A Runner that
// implements it lets the Agent detect that `provision update` swapped the
// binary out from under a cached capabilities probe, even though nothing
// asked for `health`. Runners that don't implement it keep the original
// refresh-on-health-only behaviour.
type binaryIdentifier interface {
	BinaryIdentity() (string, error)
}

type Policy struct {
	AllowSnapshot     bool
	AllowSnapshotApps bool
}

// DefaultMaxRunDuration is the per-request run cap used when MaxRunDuration
// is left at its zero value. A real `spectra snapshot --json` can take well
// over a minute on a used Mac, so this is sized for that rather than for a
// quick health or inspect call.
const DefaultMaxRunDuration = 3 * time.Minute

// Agent dispatches requests to one local Spectra installation.
type Agent struct {
	Runner         Runner
	AgentVersion   string
	MaxRunDuration time.Duration
	Auditor        Auditor
	Now            func() time.Time
	Policy         Policy
	Logf           func(string, ...any)
	mu             sync.Mutex
	capabilities   protocol.SpectraCapabilities
	capsErr        error
	capsLoaded     bool
	capsIdentity   string
}

type preparedRequest struct {
	inspect  protocol.InspectParams
	snapshot protocol.SnapshotCreateParams
	caps     protocol.SpectraCapabilities
	schema   protocol.SchemaRef
}

// Handle validates, authorizes, audits, and dispatches one request.
func (a *Agent) Handle(ctx context.Context, req protocol.Request) protocol.Response {
	if err := req.Validate(); err != nil {
		return a.reject(ctx, req, protocol.RequestErrorCode(err), err)
	}
	ctx, cancel := context.WithTimeout(ctx, a.requestTimeout(req))
	defer cancel()
	prepared, code, err := a.prepare(ctx, req)
	if err != nil {
		return a.reject(ctx, req, code, err)
	}
	if err := a.audit(ctx, req, "started", "", ""); err != nil {
		return failure(req.RequestID, protocol.CodeUnavailable, fmt.Errorf("audit log unavailable: %w", err))
	}
	response := a.runPrepared(ctx, req, prepared)
	a.completeAudit(ctx, req, response)
	return response
}

func (a *Agent) prepare(ctx context.Context, req protocol.Request) (preparedRequest, protocol.ErrorCode, error) {
	switch req.Operation {
	case protocol.OperationHealth:
		return preparedRequest{}, "", nil
	case protocol.OperationInspect:
		var p preparedRequest
		if err := decodeParams(req.Params, &p.inspect); err != nil {
			return p, protocol.CodeInvalidRequest, err
		}
		if len(p.inspect.AppPaths) == 0 {
			return p, protocol.CodeInvalidRequest, fmt.Errorf("inspect requires at least one app path")
		}
		return a.prepareCompatible(ctx, req.Operation, p)
	case protocol.OperationSnapshotCreate:
		return a.prepareSnapshot(ctx, req)
	default:
		return preparedRequest{}, protocol.CodeUnsupportedOperation, fmt.Errorf("operation %q is not supported", req.Operation)
	}
}

func (a *Agent) prepareSnapshot(ctx context.Context, req protocol.Request) (preparedRequest, protocol.ErrorCode, error) {
	var p preparedRequest
	// A fixed local policy denial wins over installed-Spectra incompatibility.
	if !a.Policy.AllowSnapshot {
		return p, protocol.CodePermissionDenied, fmt.Errorf("snapshots are disabled")
	}
	if err := decodeParams(req.Params, &p.snapshot); err != nil {
		return p, protocol.CodeInvalidRequest, err
	}
	if p.snapshot.IncludeApps && !a.Policy.AllowSnapshotApps {
		return p, protocol.CodePermissionDenied, fmt.Errorf("snapshot app collection is disabled")
	}
	return a.prepareCompatible(ctx, req.Operation, p)
}

func (a *Agent) prepareCompatible(ctx context.Context, op protocol.Operation, p preparedRequest) (preparedRequest, protocol.ErrorCode, error) {
	if a.Runner == nil {
		return p, protocol.CodeUnavailable, fmt.Errorf("local Spectra runner is not configured")
	}
	var err error
	p.caps, p.schema, err = a.compatible(ctx, op)
	if err != nil {
		return p, compatibilityErrorCode(err), err
	}
	return p, "", nil
}

func compatibilityErrorCode(err error) protocol.ErrorCode {
	var coded *protocol.CodedError
	if errors.As(err, &coded) {
		return protocol.CodeOf(err)
	}
	return protocol.CodeIncompatibleSpectra
}

func (a *Agent) runPrepared(ctx context.Context, req protocol.Request, p preparedRequest) protocol.Response {
	switch req.Operation {
	case protocol.OperationHealth:
		return a.health(ctx, req)
	case protocol.OperationInspect:
		result, err := a.Runner.Inspect(ctx, p.inspect)
		return a.diagnosticResponse(req.RequestID, p.schema, p.caps.SpectraVersion, result, err, ctx)
	case protocol.OperationSnapshotCreate:
		result, err := a.Runner.SnapshotCreate(ctx, p.snapshot)
		return a.diagnosticResponse(req.RequestID, p.schema, p.caps.SpectraVersion, result, err, ctx)
	}
	return failure(req.RequestID, protocol.CodeInternal, fmt.Errorf("unreachable operation %q", req.Operation))
}

func (a *Agent) completeAudit(ctx context.Context, req protocol.Request, response protocol.Response) {
	outcome, code := "succeeded", protocol.ErrorCode("")
	if response.Error != nil {
		outcome, code = "failed", response.Error.Code
	}
	if err := a.audit(ctx, req, "completed", outcome, code); err != nil && a.Logf != nil {
		a.Logf("audit completed write failed: %v", err)
	}
}

func (a *Agent) compatible(ctx context.Context, op protocol.Operation) (protocol.SpectraCapabilities, protocol.SchemaRef, error) {
	caps, err := a.loadCapabilities(ctx, false)
	if err != nil {
		return caps, protocol.SchemaRef{}, fmt.Errorf("probe Spectra capabilities: %w", err)
	}
	schema, err := caps.ResultSchemaFor(op)
	if err != nil {
		var coded *protocol.CodedError
		if errors.As(err, &coded) {
			return caps, schema, fmt.Errorf("%s: %w", coded.Code, err)
		}
		return caps, schema, fmt.Errorf("check Spectra result schema: %w", err)
	}
	return caps, schema, nil
}

func (a *Agent) loadCapabilities(ctx context.Context, refresh bool) (protocol.SpectraCapabilities, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	identity, identityErr := a.binaryIdentity()
	if a.capsLoaded && !refresh && (identityErr != nil || identity != a.capsIdentity) {
		refresh = true
	}
	if !a.capsLoaded || refresh {
		a.capabilities, a.capsErr = a.Runner.Capabilities(ctx)
		a.capsLoaded = true
		a.capsIdentity = identity
		if a.capsErr == nil {
			a.capsErr = a.capabilities.Validate()
		}
	}
	return a.capabilities, a.capsErr
}

// binaryIdentity reports the current Runner's on-disk binary identity, or
// ("", nil) when the Runner doesn't support identity checks. It never fails
// the caller: an identity error is treated as "unknown, so refresh" by
// loadCapabilities rather than surfaced here.
func (a *Agent) binaryIdentity() (string, error) {
	bi, ok := a.Runner.(binaryIdentifier)
	if !ok {
		return "", nil
	}
	return bi.BinaryIdentity()
}

func (a *Agent) health(ctx context.Context, req protocol.Request) protocol.Response {
	var caps protocol.SpectraCapabilities
	var err error
	if a.Runner != nil {
		caps, err = a.loadCapabilities(ctx, true)
	} else {
		err = fmt.Errorf("local Spectra runner is not configured")
	}
	manifest := protocol.CapabilityManifest{
		ProtocolVersions: []string{protocol.Version}, AgentVersion: a.AgentVersion,
		Operations: []protocol.OperationCapability{{Name: protocol.OperationHealth}},
		Limits:     protocol.Limits{MaxRequestBytes: protocol.MaxRequestBytes, MaxResponseBytes: protocol.MaxResponseBytes, MaxTimeoutMS: a.maxTimeoutMS()},
	}
	if err == nil {
		manifest.SpectraVersion = caps.SpectraVersion
		for _, op := range []protocol.Operation{protocol.OperationInspect, protocol.OperationSnapshotCreate} {
			if op == protocol.OperationSnapshotCreate && !a.Policy.AllowSnapshot {
				continue
			}
			if schema, schemaErr := caps.ResultSchemaFor(op); schemaErr == nil {
				manifest.Operations = append(manifest.Operations, protocol.OperationCapability{Name: op, ResultSchema: &schema})
			}
		}
	}
	return success(req.RequestID, protocol.HealthResult{Capabilities: manifest})
}

func (a *Agent) maxTimeoutMS() int {
	max := a.MaxRunDuration
	if max <= 0 {
		max = DefaultMaxRunDuration
	}
	if max > time.Duration(protocol.MaxTimeoutMS)*time.Millisecond {
		return protocol.MaxTimeoutMS
	}
	ms := int(max / time.Millisecond)
	if ms < 1 {
		return 1
	}
	return ms
}

func (a *Agent) diagnosticResponse(id string, schema protocol.SchemaRef, version string, data json.RawMessage, runErr error, ctx context.Context) protocol.Response {
	if runErr != nil {
		code := protocol.CodeExecutionFailed
		if errors.Is(runErr, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			code = protocol.CodeTimeout
		}
		return failure(id, code, runErr)
	}
	if !json.Valid(data) || bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return failure(id, protocol.CodeExecutionFailed, fmt.Errorf("Spectra returned invalid diagnostic JSON"))
	}
	return success(id, protocol.DiagnosticResult{Schema: schema, SpectraVersion: version, Data: data})
}

func (a *Agent) reject(ctx context.Context, req protocol.Request, code protocol.ErrorCode, err error) protocol.Response {
	_ = a.audit(ctx, req, "completed", "rejected", code)
	return failure(req.RequestID, code, err)
}

func (a *Agent) audit(ctx context.Context, req protocol.Request, stage, outcome string, code protocol.ErrorCode) error {
	if a.Auditor == nil {
		return nil
	}
	peer, _ := PeerFrom(ctx)
	return a.Auditor.Record(AuditEvent{At: a.now().UTC(), RequestID: req.RequestID, Operation: req.Operation, Peer: peer, Stage: stage, Outcome: outcome, ErrorCode: code})
}

func (a *Agent) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}
func (a *Agent) requestTimeout(req protocol.Request) time.Duration {
	max := time.Duration(a.maxTimeoutMS()) * time.Millisecond
	if req.TimeoutMS <= 0 {
		return max
	}
	want := time.Duration(req.TimeoutMS) * time.Millisecond
	if want < max {
		return want
	}
	return max
}

func decodeParams(raw json.RawMessage, dst any) error {
	if len(raw) == 0 {
		raw = []byte("{}")
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("invalid parameters: %w", err)
	}
	return nil
}
func success(id string, value any) protocol.Response {
	raw, err := json.Marshal(value)
	if err != nil {
		return failure(id, protocol.CodeInternal, err)
	}
	return protocol.Response{ProtocolVersion: protocol.Version, RequestID: id, Result: raw}
}
func failure(id string, code protocol.ErrorCode, err error) protocol.Response {
	return protocol.Response{ProtocolVersion: protocol.Version, RequestID: id, Error: protocol.NewError(code, err.Error())}
}

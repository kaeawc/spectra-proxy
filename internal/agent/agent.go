// Package agent exposes a transport-neutral, typed bridge to a locally
// installed Spectra binary. Transport authentication and installation are
// intentionally outside this package.
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	protocol "github.com/kaeawc/spectra-protocol/protocol/v1"
)

// Runner performs the explicitly supported local diagnostic operations.
// It is intentionally not a generic command runner.
type Runner interface {
	Version(context.Context) (string, error)
	Inspect(context.Context, protocol.InspectParams) (json.RawMessage, error)
	SnapshotCreate(context.Context, protocol.SnapshotCreateParams) (json.RawMessage, error)
}

// Agent dispatches protocol requests to one local Spectra installation.
type Agent struct {
	Runner         Runner
	AgentVersion   string
	MaxRunDuration time.Duration
}

// Handle returns a protocol response for one request.
func (a Agent) Handle(ctx context.Context, req protocol.Request) protocol.Response {
	if err := req.Validate(); err != nil {
		return failure(req.RequestID, "invalid_request", err)
	}
	if a.Runner == nil {
		return failure(req.RequestID, "unavailable", fmt.Errorf("local Spectra runner is not configured"))
	}
	ctx, cancel := context.WithTimeout(ctx, a.requestTimeout(req))
	defer cancel()
	switch req.Operation {
	case protocol.OperationHealth:
		return a.health(ctx, req)
	case protocol.OperationInspect:
		return a.inspect(ctx, req)
	case protocol.OperationSnapshotCreate:
		return a.snapshotCreate(ctx, req)
	default:
		return failure(req.RequestID, "unsupported_operation", fmt.Errorf("operation %q is not supported", req.Operation))
	}
}

func (a Agent) requestTimeout(req protocol.Request) time.Duration {
	max := a.MaxRunDuration
	if max <= 0 {
		max = 30 * time.Second
	}
	if req.TimeoutMS <= 0 {
		return max
	}
	want := time.Duration(req.TimeoutMS) * time.Millisecond
	if want < max {
		return want
	}
	return max
}

func (a Agent) health(ctx context.Context, req protocol.Request) protocol.Response {
	version, err := a.Runner.Version(ctx)
	if err != nil {
		return failure(req.RequestID, "execution_failed", err)
	}
	return success(req.RequestID, protocol.HealthResult{Capabilities: protocol.CapabilityManifest{
		ProtocolVersion: protocol.Version,
		SpectraVersion:  version,
		AgentVersion:    a.AgentVersion,
		Operations: []protocol.Operation{
			protocol.OperationHealth,
			protocol.OperationInspect,
			protocol.OperationSnapshotCreate,
		},
	}})
}

func (a Agent) inspect(ctx context.Context, req protocol.Request) protocol.Response {
	var params protocol.InspectParams
	if err := decodeParams(req.Params, &params); err != nil {
		return failure(req.RequestID, "invalid_request", err)
	}
	if len(params.AppPaths) == 0 {
		return failure(req.RequestID, "invalid_request", fmt.Errorf("inspect requires at least one app path"))
	}
	result, err := a.Runner.Inspect(ctx, params)
	if err != nil {
		return failure(req.RequestID, "execution_failed", err)
	}
	return rawSuccess(req.RequestID, result)
}

func (a Agent) snapshotCreate(ctx context.Context, req protocol.Request) protocol.Response {
	var params protocol.SnapshotCreateParams
	if err := decodeParams(req.Params, &params); err != nil {
		return failure(req.RequestID, "invalid_request", err)
	}
	result, err := a.Runner.SnapshotCreate(ctx, params)
	if err != nil {
		return failure(req.RequestID, "execution_failed", err)
	}
	return rawSuccess(req.RequestID, result)
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

func success(requestID string, value any) protocol.Response {
	raw, err := json.Marshal(value)
	if err != nil {
		return failure(requestID, "internal", err)
	}
	return rawSuccess(requestID, raw)
}

func rawSuccess(requestID string, raw json.RawMessage) protocol.Response {
	return protocol.Response{ProtocolVersion: protocol.Version, RequestID: requestID, Result: raw}
}

func failure(requestID, code string, err error) protocol.Response {
	return protocol.Response{
		ProtocolVersion: protocol.Version,
		RequestID:       requestID,
		Error:           &protocol.Error{Code: code, Message: err.Error()},
	}
}

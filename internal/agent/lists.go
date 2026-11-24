package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"time"

	protocol "github.com/kaeawc/spectra-protocol/protocol/v1"
)

// lister is an optional Runner capability for the read-only list operations,
// used when no Spectra daemon serves them. Implementations run a fixed argv
// and return data already normalized to the protocol shape.
type lister interface {
	ProcessList(context.Context) (json.RawMessage, error)
	JVMList(context.Context) (json.RawMessage, error)
}

// listOperations are the read-only list operations, in manifest order.
var listOperations = []protocol.Operation{protocol.OperationProcessList, protocol.OperationJVMList}

// daemonMethod maps a list operation to the fixed daemon method and params.
func daemonMethod(op protocol.Operation) (string, any) {
	switch op {
	case protocol.OperationProcessList:
		return "process.list", map[string]bool{"classify": true}
	case protocol.OperationJVMList:
		return "jvm.list", map[string]any{}
	}
	return "", nil
}

// listRoute is where one list operation will be served from.
type listRoute struct {
	viaDaemon      bool
	spectraVersion string
}

// daemonStatus probes the configured daemon with a short bound. ok is false
// when there is no daemon, it is unreachable, or it speaks another protocol.
func (a *Agent) daemonStatus(ctx context.Context) (DaemonStatus, bool) {
	if a.Daemon == nil {
		return DaemonStatus{}, false
	}
	ctx, cancel := context.WithTimeout(ctx, daemonProbeTimeout)
	defer cancel()
	st, err := a.Daemon.Status(ctx)
	if err != nil || st.Protocol != daemonProtocolVersion {
		return DaemonStatus{}, false
	}
	return st, true
}

// routeList prefers a daemon that lists the operation's method, and otherwise
// falls back to the local Spectra CLI when its capabilities probe succeeds.
func (a *Agent) routeList(ctx context.Context, op protocol.Operation, st DaemonStatus, daemonOK bool) (listRoute, protocol.ErrorCode, error) {
	if method, _ := daemonMethod(op); daemonOK && st.HasMethod(method) {
		return listRoute{viaDaemon: true, spectraVersion: st.Version}, "", nil
	}
	if a.Runner == nil {
		return listRoute{}, protocol.CodeUnavailable, fmt.Errorf("no Spectra daemon serves %s and the local Spectra runner is not configured", op)
	}
	if _, ok := a.Runner.(lister); !ok {
		return listRoute{}, protocol.CodeUnavailable, fmt.Errorf("no Spectra daemon serves %s and the local Spectra runner cannot list", op)
	}
	caps, err := a.loadCapabilities(ctx, false)
	if err != nil {
		err = fmt.Errorf("probe Spectra capabilities: %w", err)
		return listRoute{}, compatibilityErrorCode(err), err
	}
	return listRoute{spectraVersion: caps.SpectraVersion}, "", nil
}

func (a *Agent) prepareList(ctx context.Context, req protocol.Request) (preparedRequest, protocol.ErrorCode, error) {
	var p preparedRequest
	// A fixed local policy denial wins over availability.
	if !a.Policy.AllowProcesses {
		return p, protocol.CodePermissionDenied, fmt.Errorf("process listing is disabled")
	}
	var params any = &protocol.ProcessListParams{}
	if req.Operation == protocol.OperationJVMList {
		params = &protocol.JVMListParams{}
	}
	if err := decodeParams(req.Params, params); err != nil {
		return p, protocol.CodeInvalidRequest, err
	}
	p.schema = listSchema(req.Operation)
	st, daemonOK := a.daemonStatus(ctx)
	route, code, err := a.routeList(ctx, req.Operation, st, daemonOK)
	if err != nil {
		return p, code, err
	}
	p.list = route
	return p, "", nil
}

func listSchema(op protocol.Operation) protocol.SchemaRef {
	name, _ := protocol.ResultSchemaName(op)
	version, _ := protocol.SupportedResultSchemaVersion(name)
	return protocol.SchemaRef{Name: name, Version: version}
}

func (a *Agent) runList(ctx context.Context, op protocol.Operation, route listRoute) (json.RawMessage, error) {
	if route.viaDaemon {
		method, params := daemonMethod(op)
		raw, err := a.Daemon.Call(ctx, method, params)
		if err != nil {
			return nil, fmt.Errorf("spectra daemon %s: %w", method, err)
		}
		if op == protocol.OperationProcessList {
			return normalizeDaemonProcessList(raw)
		}
		return normalizeJVMList(raw)
	}
	l := a.Runner.(lister)
	if op == protocol.OperationProcessList {
		return l.ProcessList(ctx)
	}
	return l.JVMList(ctx)
}

// normalizeDaemonProcessList keeps sampled_at and processes from the daemon's
// process.list snapshot; other members (warning, stale) are not part of the
// protocol shape.
func normalizeDaemonProcessList(raw json.RawMessage) (json.RawMessage, error) {
	var snap struct {
		SampledAt string          `json:"sampled_at"`
		Processes json.RawMessage `json:"processes"`
	}
	if err := json.Unmarshal(raw, &snap); err != nil || !isJSONObject(raw) {
		return nil, fmt.Errorf("spectra daemon process.list returned an unexpected shape")
	}
	rows, err := rowsOf(snap.Processes, "processes")
	if err != nil {
		return nil, err
	}
	return json.Marshal(protocol.ProcessListData{SampledAt: sampledAt(snap.SampledAt), Processes: rows})
}

// normalizeCLIProcessList wraps the array `spectra process -json --classify`
// prints. It prints nothing when there are no processes.
func normalizeCLIProcessList(out []byte, at time.Time) (json.RawMessage, error) {
	rows, err := rowsOf(out, "processes")
	if err != nil {
		return nil, err
	}
	return json.Marshal(protocol.ProcessListData{SampledAt: at.UTC().Format(time.RFC3339Nano), Processes: rows})
}

// normalizeJVMList accepts an array, null, empty output, or an object with a
// jvms member, from either the daemon or `spectra jvm -json`.
func normalizeJVMList(raw []byte) (json.RawMessage, error) {
	if isJSONObject(raw) {
		var wrapped struct {
			JVMs json.RawMessage `json:"jvms"`
		}
		if err := json.Unmarshal(raw, &wrapped); err != nil {
			return nil, fmt.Errorf("decode JVM list: %w", err)
		}
		raw = wrapped.JVMs
	}
	rows, err := rowsOf(raw, "jvms")
	if err != nil {
		return nil, err
	}
	return json.Marshal(protocol.JVMListData{JVMs: rows})
}

// rowsOf decodes a JSON array of rows, treating empty input and null as no
// rows. Row contents are passed through untouched.
func rowsOf(raw []byte, field string) ([]json.RawMessage, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return []json.RawMessage{}, nil
	}
	var rows []json.RawMessage
	if err := json.Unmarshal(trimmed, &rows); err != nil {
		return nil, fmt.Errorf("Spectra returned %s that is not a JSON array", field)
	}
	return rows, nil
}

// sampledAt keeps a valid, non-zero RFC 3339 timestamp and drops anything else.
func sampledAt(s string) string {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil || t.IsZero() {
		return ""
	}
	return s
}

func isJSONObject(raw []byte) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 0 && trimmed[0] == '{'
}

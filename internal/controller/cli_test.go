package controller

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	protocol "github.com/kaeawc/spectra-protocol/protocol/v1"
	"github.com/kaeawc/spectra-proxy/internal/agent"
)

func TestParseCallFlagsUsageErrors(t *testing.T) {
	cases := [][]string{{"--operation", "health"}, {"--target", "host:1"}, {"--target", "host:1", "--operation", "health", "--timeout", "0s"}, {"--wat"}}
	for _, args := range cases {
		var stderr bytes.Buffer
		_, _, _, code := ParseCallFlags(args, &stderr, "/state")
		if code != 2 || !strings.Contains(stderr.String(), "\n") && stderr.Len() == 0 {
			t.Errorf("args %v: code=%d stderr=%q", args, code, stderr.String())
		}
	}
}

func TestParseCallFlagsDefaultTimeoutOutlastsARealSnapshot(t *testing.T) {
	var stderr bytes.Buffer
	opts, _, _, code := ParseCallFlags([]string{"--target", "host:1", "--operation", "health"}, &stderr, "/state")
	if code != 0 {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
	if opts.Timeout != DefaultCallTimeout {
		t.Fatalf("default timeout = %s, want %s", opts.Timeout, DefaultCallTimeout)
	}
	if opts.Timeout <= 30*time.Second {
		t.Fatalf("default timeout %s is not longer than the old 30s default", opts.Timeout)
	}
}

func cliOpts(op string) CallOptions {
	return CallOptions{Operation: op, Timeout: time.Second, Rand: bytes.NewReader(make([]byte, 32))}
}

func TestRunCallIncompatibleResultAndRemoteError(t *testing.T) {
	cases := []struct {
		name string
		op   string
		raw  string
		want int
	}{
		{"schema", "inspect", `{"protocol_version":"v1","request_id":"cli-0000000000000000","result":{"schema":{"name":"spectra.inspect","version":2},"spectra_version":"1","data":{}}}`, 3},
		{"remote", "inspect", `{"protocol_version":"v1","request_id":"cli-0000000000000000","error":{"code":"unavailable","message":"offline"}}`, 1},
		{"unknown operation result", "future.operation", `{"protocol_version":"v1","request_id":"cli-0000000000000000","result":{"anything":true}}`, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client, peer := net.Pipe()
			defer client.Close()
			defer peer.Close()
			go func() {
				defer peer.Close()
				_, _ = bufio.NewReader(peer).ReadBytes('\n')
				_, _ = fmt.Fprintln(peer, tc.raw)
			}()
			var out, stderr bytes.Buffer
			got := RunCall(context.Background(), client, cliOpts(tc.op), &out, &stderr)
			if got != tc.want {
				t.Fatalf("RunCall()=%d want %d; stderr=%s", got, tc.want, stderr.String())
			}
			if stderr.Len() == 0 || out.Len() != 0 {
				t.Fatalf("expected one-line error and no output; stdout=%s", out.String())
			}
		})
	}
}

func TestRunCallParamsObjectUsage(t *testing.T) {
	for _, params := range []string{"{", `[]`, `null`, `"value"`} {
		var out, stderr bytes.Buffer
		code := RunCall(context.Background(), nil, CallOptions{Operation: "inspect", Params: params}, &out, &stderr)
		if code != 2 || stderr.Len() == 0 {
			t.Errorf("params %q: code=%d stderr=%q", params, code, stderr.String())
		}
	}
}

func TestRunCallNegotiationStopsBeforeRealRequest(t *testing.T) {
	client, peer := net.Pipe()
	defer client.Close()
	defer peer.Close()
	requests := make(chan error, 1)
	go func() {
		defer peer.Close()
		reader := bufio.NewReader(peer)
		line, err := reader.ReadBytes('\n')
		if err != nil {
			requests <- err
			return
		}
		var req protocol.Request
		if err := json.Unmarshal(line, &req); err != nil {
			requests <- err
			return
		}
		manifest := protocol.CapabilityManifest{ProtocolVersions: []string{"v1"}, Operations: []protocol.OperationCapability{{Name: protocol.OperationHealth}}, Limits: protocol.Limits{MaxRequestBytes: protocol.MaxRequestBytes, MaxResponseBytes: protocol.MaxResponseBytes, MaxTimeoutMS: protocol.MaxTimeoutMS}}
		result, _ := json.Marshal(protocol.HealthResult{Capabilities: manifest})
		resp, _ := json.Marshal(protocol.Response{ProtocolVersion: protocol.Version, RequestID: req.RequestID, Result: result})
		_, _ = fmt.Fprintln(peer, string(resp))
		peer.SetReadDeadline(time.Now().Add(80 * time.Millisecond))
		if _, err := reader.ReadBytes('\n'); err == nil {
			requests <- fmt.Errorf("received unexpected second request")
		} else {
			requests <- nil
		}
	}()
	var out, stderr bytes.Buffer
	opts := cliOpts("inspect")
	opts.Negotiate = true
	code := RunCall(context.Background(), client, opts, &out, &stderr)
	if code != 1 {
		t.Fatalf("RunCall()=%d, want 1: %s", code, stderr.String())
	}
	if err := <-requests; err != nil {
		t.Fatal(err)
	}
}

func TestRandomIDFormat(t *testing.T) {
	id, err := randomID(bytes.NewReader(make([]byte, 8)))
	if err != nil {
		t.Fatal(err)
	}
	if len(id) != 20 || id[:4] != "cli-" {
		t.Fatalf("id = %q", id)
	}
	if _, err := hex.DecodeString(id[4:]); err != nil {
		t.Fatalf("id suffix: %v", err)
	}
}

// listDaemon is a fixed in-memory Spectra daemon for end-to-end list calls.
type listDaemon struct{}

func (listDaemon) Status(context.Context) (agent.DaemonStatus, error) {
	return agent.DaemonStatus{Protocol: "spectra-daemon/1", Version: "0.42.0", Methods: []string{"daemon.status", "process.list", "jvm.list"}}, nil
}
func (listDaemon) Call(_ context.Context, method string, _ any) (json.RawMessage, error) {
	switch method {
	case "process.list":
		return json.RawMessage(`{"sampled_at":"2026-11-24T10:00:00.123456Z","processes":[{"pid":1,"ppid":0,"uid":0,"user":"root","command":"launchd","full_command_line":"/sbin/launchd","rss_kib":12288,"cpu_pct":0.1,"role":"system","runtime":"native"}]}`), nil
	case "jvm.list":
		return json.RawMessage(`null`), nil
	}
	return nil, fmt.Errorf("unexpected method %q", method)
}

func TestRunCallPrintsListResults(t *testing.T) {
	cases := []struct {
		op   protocol.Operation
		data string
	}{
		{protocol.OperationProcessList, `{"sampled_at":"2026-11-24T10:00:00.123456Z","processes":[{"pid":1,"ppid":0,"uid":0,"user":"root","command":"launchd","full_command_line":"/sbin/launchd","rss_kib":12288,"cpu_pct":0.1,"role":"system","runtime":"native"}]}`},
		{protocol.OperationJVMList, `{"jvms":[]}`},
	}
	for _, tc := range cases {
		t.Run(string(tc.op), func(t *testing.T) {
			a := &agent.Agent{Daemon: listDaemon{}, Policy: agent.Policy{AllowProcesses: true}}
			client, peer := net.Pipe()
			defer client.Close()
			go func() {
				defer peer.Close()
				_ = agent.Serve(context.Background(), a, peer, peer)
			}()
			opts := cliOpts(string(tc.op))
			opts.Negotiate = true
			var out, stderr bytes.Buffer
			if got := RunCall(context.Background(), client, opts, &out, &stderr); got != 0 {
				t.Fatalf("RunCall()=%d stderr=%s", got, stderr.String())
			}
			t.Logf("stdout:\n%s", out.String())
			var resp protocol.Response
			if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
				t.Fatal(err)
			}
			outcome, err := Interpret(tc.op, resp)
			if err != nil || outcome.Diagnostic == nil {
				t.Fatalf("Interpret: %v", err)
			}
			var compact bytes.Buffer
			if err := json.Compact(&compact, outcome.Diagnostic.Data); err != nil {
				t.Fatal(err)
			}
			if compact.String() != tc.data || outcome.Diagnostic.SpectraVersion != "0.42.0" {
				t.Fatalf("diagnostic = %+v data=%s", outcome.Diagnostic, compact.String())
			}
		})
	}
}

func TestInterpretListResultShapes(t *testing.T) {
	cases := []struct {
		op     protocol.Operation
		result string
		code   protocol.ErrorCode
	}{
		{protocol.OperationProcessList, `{"schema":{"name":"spectra.process.list","version":1},"spectra_version":"1","data":{"processes":[]}}`, ""},
		{protocol.OperationJVMList, `{"schema":{"name":"spectra.jvm.list","version":1},"spectra_version":"1","data":{"jvms":[{"pid":1}]}}`, ""},
		{protocol.OperationProcessList, `{"schema":{"name":"spectra.process.list","version":1},"spectra_version":"1","data":[]}`, protocol.CodeInvalidRequest},
		{protocol.OperationJVMList, `{"schema":{"name":"spectra.process.list","version":1},"spectra_version":"1","data":{"processes":[]}}`, protocol.CodeIncompatibleSpectra},
	}
	for _, tc := range cases {
		resp := protocol.Response{ProtocolVersion: protocol.Version, RequestID: "req-1", Result: json.RawMessage(tc.result)}
		_, err := Interpret(tc.op, resp)
		if tc.code == "" {
			if err != nil {
				t.Fatalf("%s: %v", tc.op, err)
			}
			continue
		}
		if protocol.CodeOf(err) != tc.code {
			t.Fatalf("%s: code %s, want %s: %v", tc.op, protocol.CodeOf(err), tc.code, err)
		}
	}
}

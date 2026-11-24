package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	protocol "github.com/kaeawc/spectra-protocol/protocol/v1"
)

// fakeDaemon is a unix-socket JSON-RPC server shaped like `spectra daemon`.
type fakeDaemon struct {
	socket   string
	protocol string
	methods  []string
	handlers map[string]func(params json.RawMessage) (any, *DaemonRPCError)
	mu       sync.Mutex
	calls    []string
	params   map[string]string
}

func startFakeDaemon(t *testing.T, methods []string, handlers map[string]func(json.RawMessage) (any, *DaemonRPCError)) *fakeDaemon {
	t.Helper()
	// Unix socket paths are limited to ~104 bytes, so avoid t.TempDir's long names.
	dir, err := os.MkdirTemp("", "sd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	d := &fakeDaemon{socket: filepath.Join(dir, "daemon.sock"), protocol: daemonProtocolVersion, methods: methods, handlers: handlers, params: map[string]string{}}
	ln, err := net.Listen("unix", d.socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go d.serve(conn)
		}
	}()
	return d
}

func (d *fakeDaemon) serve(conn net.Conn) {
	defer conn.Close()
	r := bufio.NewReader(conn)
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			return
		}
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if json.Unmarshal(line, &req) != nil {
			return
		}
		d.mu.Lock()
		d.calls = append(d.calls, req.Method)
		d.params[req.Method] = string(req.Params)
		d.mu.Unlock()
		var result any
		var rpcErr *DaemonRPCError
		if req.Method == daemonMethodStatus {
			result = map[string]any{"protocol": d.protocol, "version": "1.2.3", "pid": 99, "methods": append([]string{daemonMethodStatus}, d.methods...)}
		} else if h := d.handlers[req.Method]; h != nil {
			result, rpcErr = h(req.Params)
		} else {
			rpcErr = &DaemonRPCError{Code: -32601, Message: "method not found"}
		}
		// A notification first proves the client skips non-replies.
		_, _ = conn.Write([]byte(`{"jsonrpc":"2.0","method":"process.update","params":{}}` + "\n"))
		reply := map[string]any{"jsonrpc": "2.0", "id": req.ID}
		if rpcErr != nil {
			reply["error"] = rpcErr
		} else {
			reply["result"] = result
		}
		data, _ := json.Marshal(reply)
		if _, err := conn.Write(append(data, '\n')); err != nil {
			return
		}
	}
}

func (d *fakeDaemon) called(method string) (bool, string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, m := range d.calls {
		if m == method {
			return true, d.params[method]
		}
	}
	return false, ""
}

func daemonProcessSnapshot(json.RawMessage) (any, *DaemonRPCError) {
	return map[string]any{
		"sampled_at": "2026-11-24T10:00:00.123456Z",
		"processes":  []any{map[string]any{"pid": 1, "command": "launchd", "role": "system"}, map[string]any{"pid": 42, "command": "java", "runtime": "jvm"}},
		"warning":    "classify: partial",
		"stale":      true,
	}, nil
}

// listerRunner is a fakeRunner that also serves the CLI list fallback.
type listerRunner struct {
	fakeRunner
	processes func(context.Context) (json.RawMessage, error)
	jvms      func(context.Context) (json.RawMessage, error)
}

func (r listerRunner) ProcessList(ctx context.Context) (json.RawMessage, error) {
	if r.processes != nil {
		return r.processes(ctx)
	}
	return json.RawMessage(`{"processes":[{"pid":7}]}`), nil
}
func (r listerRunner) JVMList(ctx context.Context) (json.RawMessage, error) {
	if r.jvms != nil {
		return r.jvms(ctx)
	}
	return json.RawMessage(`{"jvms":[]}`), nil
}

func decodeList(t *testing.T, resp protocol.Response, op protocol.Operation) protocol.DiagnosticResult {
	t.Helper()
	if resp.Error != nil {
		t.Fatalf("%s: %+v", op, resp.Error)
	}
	result, err := protocol.DecodeResult(resp, op)
	if err != nil {
		t.Fatalf("%s: %v", op, err)
	}
	return result
}

func operationNames(m protocol.CapabilityManifest) []string {
	var names []string
	for _, op := range m.Operations {
		names = append(names, string(op.Name))
	}
	return names
}

func TestProcessListViaDaemon(t *testing.T) {
	d := startFakeDaemon(t, []string{"process.list", "jvm.list"}, map[string]func(json.RawMessage) (any, *DaemonRPCError){
		"process.list": daemonProcessSnapshot,
		"jvm.list": func(json.RawMessage) (any, *DaemonRPCError) {
			return []any{map[string]any{"pid": 42, "main_class": "Main"}}, nil
		},
	})
	auditor := &recordingAuditor{}
	// The CLI runner must not be used when the daemon lists the methods.
	runner := listerRunner{processes: func(context.Context) (json.RawMessage, error) { t.Fatal("CLI fallback used"); return nil, nil }}
	a := &Agent{Runner: runner, Daemon: SpectraDaemon{Socket: d.socket}, Auditor: auditor, Policy: Policy{AllowProcesses: true}}

	result := decodeList(t, a.Handle(context.Background(), request(protocol.OperationProcessList, "")), protocol.OperationProcessList)
	if result.Schema != (protocol.SchemaRef{Name: protocol.SchemaProcessList, Version: 1}) || result.SpectraVersion != "1.2.3" {
		t.Fatalf("result: %+v", result)
	}
	if got := string(result.Data); got != `{"sampled_at":"2026-11-24T10:00:00.123456Z","processes":[{"command":"launchd","pid":1,"role":"system"},{"command":"java","pid":42,"runtime":"jvm"}]}` {
		t.Fatalf("data = %s", got)
	}
	if ok, params := d.called("process.list"); !ok || params != `{"classify":true}` {
		t.Fatalf("process.list called=%t params=%s", ok, params)
	}

	result = decodeList(t, a.Handle(context.Background(), request(protocol.OperationJVMList, "{}")), protocol.OperationJVMList)
	if got := string(result.Data); got != `{"jvms":[{"main_class":"Main","pid":42}]}` {
		t.Fatalf("jvm data = %s", got)
	}
	if ok, params := d.called("jvm.list"); !ok || params != `{}` {
		t.Fatalf("jvm.list called=%t params=%s", ok, params)
	}

	if len(auditor.events) != 4 {
		t.Fatalf("audit events: %+v", auditor.events)
	}
	for i, op := range []protocol.Operation{protocol.OperationProcessList, protocol.OperationProcessList, protocol.OperationJVMList, protocol.OperationJVMList} {
		e := auditor.events[i]
		wantStage, wantOutcome := "started", ""
		if i%2 == 1 {
			wantStage, wantOutcome = "completed", "succeeded"
		}
		if e.Operation != op || e.Stage != wantStage || e.Outcome != wantOutcome {
			t.Fatalf("audit event %d: %+v", i, e)
		}
	}
}

func TestListFallsBackToCLIWhenDaemonLacksMethod(t *testing.T) {
	d := startFakeDaemon(t, []string{"process.list"}, map[string]func(json.RawMessage) (any, *DaemonRPCError){"process.list": daemonProcessSnapshot})
	cli := listerRunner{fakeRunner: fakeRunner{caps: fixtureCaps(t)}, jvms: func(context.Context) (json.RawMessage, error) {
		return json.RawMessage(`{"jvms":[{"pid":5}]}`), nil
	}}
	a := &Agent{Runner: cli, Daemon: SpectraDaemon{Socket: d.socket}, Policy: Policy{AllowProcesses: true}}
	result := decodeList(t, a.Handle(context.Background(), request(protocol.OperationJVMList, "")), protocol.OperationJVMList)
	if string(result.Data) != `{"jvms":[{"pid":5}]}` || result.SpectraVersion != "dev" {
		t.Fatalf("result: %+v data=%s", result, result.Data)
	}
	if ok, _ := d.called("jvm.list"); ok {
		t.Fatal("called a method the daemon did not list")
	}
}

func TestListFallsBackToCLIWithoutDaemon(t *testing.T) {
	cli := listerRunner{fakeRunner: fakeRunner{caps: fixtureCaps(t)}}
	for name, daemon := range map[string]Daemon{"nil": nil, "missing socket": SpectraDaemon{Socket: "/nonexistent/spectra/daemon.sock"}} {
		t.Run(name, func(t *testing.T) {
			a := &Agent{Runner: cli, Daemon: daemon, Policy: Policy{AllowProcesses: true}}
			result := decodeList(t, a.Handle(context.Background(), request(protocol.OperationProcessList, "")), protocol.OperationProcessList)
			if string(result.Data) != `{"processes":[{"pid":7}]}` {
				t.Fatalf("data = %s", result.Data)
			}
		})
	}
}

func TestListFallsBackWhenDaemonSpeaksAnotherProtocol(t *testing.T) {
	d := startFakeDaemon(t, []string{"process.list"}, map[string]func(json.RawMessage) (any, *DaemonRPCError){"process.list": daemonProcessSnapshot})
	d.protocol = "spectra-daemon/2"
	a := &Agent{Runner: listerRunner{fakeRunner: fakeRunner{caps: fixtureCaps(t)}}, Daemon: SpectraDaemon{Socket: d.socket}, Policy: Policy{AllowProcesses: true}}
	result := decodeList(t, a.Handle(context.Background(), request(protocol.OperationProcessList, "")), protocol.OperationProcessList)
	if string(result.Data) != `{"processes":[{"pid":7}]}` {
		t.Fatalf("data = %s", result.Data)
	}
	if ok, _ := d.called("process.list"); ok {
		t.Fatal("used a daemon with an unknown protocol")
	}
}

func TestListPolicyParamsAndAvailability(t *testing.T) {
	caps := fixtureCaps(t)
	auditor := &recordingAuditor{}
	denied := &Agent{Runner: listerRunner{fakeRunner: fakeRunner{caps: caps}}, Auditor: auditor}
	// Policy denial wins over invalid parameters.
	code(t, denied.Handle(context.Background(), request(protocol.OperationProcessList, `{"argv":["rm"]}`)), protocol.CodePermissionDenied)
	if len(auditor.events) != 1 || auditor.events[0].Outcome != "rejected" || auditor.events[0].ErrorCode != protocol.CodePermissionDenied || auditor.events[0].Operation != protocol.OperationProcessList {
		t.Fatalf("audit: %+v", auditor.events)
	}

	allowed := &Agent{Runner: listerRunner{fakeRunner: fakeRunner{caps: caps}}, Policy: Policy{AllowProcesses: true}}
	for _, params := range []string{`{"argv":["rm","-rf"]}`, `{"path":"/etc"}`, `{"classify":false}`} {
		code(t, allowed.Handle(context.Background(), request(protocol.OperationProcessList, params)), protocol.CodeInvalidRequest)
		code(t, allowed.Handle(context.Background(), request(protocol.OperationJVMList, params)), protocol.CodeInvalidRequest)
	}

	code(t, (&Agent{Policy: Policy{AllowProcesses: true}}).Handle(context.Background(), request(protocol.OperationJVMList, "")), protocol.CodeUnavailable)
	code(t, (&Agent{Runner: fakeRunner{caps: caps}, Policy: Policy{AllowProcesses: true}}).Handle(context.Background(), request(protocol.OperationJVMList, "")), protocol.CodeUnavailable)
	failing := &Agent{Runner: listerRunner{fakeRunner: fakeRunner{capsErr: fmt.Errorf("exec format error")}}, Policy: Policy{AllowProcesses: true}}
	code(t, failing.Handle(context.Background(), request(protocol.OperationProcessList, "")), protocol.CodeIncompatibleSpectra)
}

func TestListExecutionFailures(t *testing.T) {
	d := startFakeDaemon(t, []string{"process.list", "jvm.list"}, map[string]func(json.RawMessage) (any, *DaemonRPCError){
		"process.list": func(json.RawMessage) (any, *DaemonRPCError) {
			return nil, &DaemonRPCError{Code: -32603, Message: "collect failed"}
		},
		"jvm.list": func(json.RawMessage) (any, *DaemonRPCError) { return map[string]any{"jvms": "nope"}, nil },
	})
	a := &Agent{Daemon: SpectraDaemon{Socket: d.socket}, Policy: Policy{AllowProcesses: true}}
	resp := a.Handle(context.Background(), request(protocol.OperationProcessList, ""))
	code(t, resp, protocol.CodeExecutionFailed)
	if !strings.Contains(resp.Error.Message, "collect failed") {
		t.Fatalf("message: %s", resp.Error.Message)
	}
	code(t, a.Handle(context.Background(), request(protocol.OperationJVMList, "")), protocol.CodeExecutionFailed)
}

func TestListDaemonTimeoutAndCancellation(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	d := startFakeDaemon(t, []string{"process.list"}, map[string]func(json.RawMessage) (any, *DaemonRPCError){
		"process.list": func(json.RawMessage) (any, *DaemonRPCError) { <-release; return nil, nil },
	})
	a := &Agent{Daemon: SpectraDaemon{Socket: d.socket}, Policy: Policy{AllowProcesses: true}}
	req := request(protocol.OperationProcessList, "")
	req.TimeoutMS = 200
	start := time.Now()
	code(t, a.Handle(context.Background(), req), protocol.CodeTimeout)
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("timeout took %s", elapsed)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(100 * time.Millisecond); cancel() }()
	if _, err := (SpectraDaemon{Socket: d.socket}).Call(ctx, "process.list", nil); err == nil || ctx.Err() == nil {
		t.Fatalf("canceled call err = %v", err)
	}
}

func TestSpectraDaemonBoundsReplies(t *testing.T) {
	big := strings.Repeat("x", maxCommandOutput)
	d := startFakeDaemon(t, []string{"jvm.list"}, map[string]func(json.RawMessage) (any, *DaemonRPCError){
		"jvm.list": func(json.RawMessage) (any, *DaemonRPCError) { return []string{big}, nil },
	})
	_, err := (SpectraDaemon{Socket: d.socket}).Call(context.Background(), "jvm.list", nil)
	if err == nil || !strings.Contains(err.Error(), "exceeded") {
		t.Fatalf("oversized reply err = %v", err)
	}
	if _, err := (SpectraDaemon{Socket: "relative.sock"}).Call(context.Background(), "jvm.list", nil); err == nil {
		t.Fatal("accepted a relative socket path")
	}
}

func TestHealthGatesListOperations(t *testing.T) {
	caps := fixtureCaps(t)
	both := startFakeDaemon(t, []string{"process.list", "jvm.list"}, nil)
	processOnly := startFakeDaemon(t, []string{"process.list"}, nil)
	cases := []struct {
		name string
		a    *Agent
		want []string
	}{
		{"policy off", &Agent{Runner: listerRunner{fakeRunner: fakeRunner{caps: caps}}, Daemon: SpectraDaemon{Socket: both.socket}}, []string{"health", "inspect"}},
		{"daemon lists both", &Agent{Runner: fakeRunner{caps: caps}, Daemon: SpectraDaemon{Socket: both.socket}, Policy: Policy{AllowProcesses: true}}, []string{"health", "inspect", "process.list", "jvm.list"}},
		{"daemon only, capabilities probe failed", &Agent{Runner: listerRunner{fakeRunner: fakeRunner{capsErr: fmt.Errorf("boom")}}, Daemon: SpectraDaemon{Socket: processOnly.socket}, Policy: Policy{AllowProcesses: true}}, []string{"health", "process.list"}},
		{"CLI fallback", &Agent{Runner: listerRunner{fakeRunner: fakeRunner{caps: caps}}, Daemon: SpectraDaemon{Socket: "/nonexistent/daemon.sock"}, Policy: Policy{AllowProcesses: true}}, []string{"health", "inspect", "process.list", "jvm.list"}},
		{"runner cannot list", &Agent{Runner: fakeRunner{caps: caps}, Policy: Policy{AllowProcesses: true}}, []string{"health", "inspect"}},
		{"nothing available", &Agent{Runner: listerRunner{fakeRunner: fakeRunner{capsErr: fmt.Errorf("boom")}}, Policy: Policy{AllowProcesses: true}}, []string{"health"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			manifest := healthManifest(t, tc.a)
			if got := strings.Join(operationNames(manifest), ","); got != strings.Join(tc.want, ",") {
				t.Fatalf("operations = %s, want %s", got, strings.Join(tc.want, ","))
			}
			for _, op := range listOperations {
				if capability, ok := manifest.Supports(op); ok {
					if _, err := protocol.Negotiate(manifest, op); err != nil || capability.ResultSchema == nil {
						t.Fatalf("%s advertised without a negotiable schema: %v", op, err)
					}
				}
			}
			if manifest.SpectraVersion == "" && len(manifest.Operations) > 1 {
				t.Fatalf("spectra_version missing: %+v", manifest)
			}
		})
	}
}

func TestNormalizeListData(t *testing.T) {
	cases := []struct {
		name string
		fn   func() (json.RawMessage, error)
		want string
	}{
		{"daemon process null rows", func() (json.RawMessage, error) {
			return normalizeDaemonProcessList(json.RawMessage(`{"sampled_at":"0001-01-01T00:00:00Z","processes":null}`))
		}, `{"processes":[]}`},
		{"daemon process bad sampled_at", func() (json.RawMessage, error) {
			return normalizeDaemonProcessList(json.RawMessage(`{"sampled_at":"soon","processes":[{"pid":1}]}`))
		}, `{"processes":[{"pid":1}]}`},
		{"daemon process array", func() (json.RawMessage, error) { return normalizeDaemonProcessList(json.RawMessage(`[{"pid":1}]`)) }, "error"},
		{"daemon process object rows", func() (json.RawMessage, error) {
			return normalizeDaemonProcessList(json.RawMessage(`{"processes":{"pid":1}}`))
		}, "error"},
		{"cli process empty output", func() (json.RawMessage, error) {
			return normalizeCLIProcessList(nil, time.Date(2026, 11, 24, 10, 0, 0, 0, time.FixedZone("x", 3600)))
		}, `{"sampled_at":"2026-11-24T09:00:00Z","processes":[]}`},
		{"cli process rows", func() (json.RawMessage, error) {
			return normalizeCLIProcessList([]byte("[\n  {\"pid\": 1}\n]\n"), time.Date(2026, 11, 24, 10, 0, 0, 5, time.UTC))
		}, `{"sampled_at":"2026-11-24T10:00:00.000000005Z","processes":[{"pid":1}]}`},
		{"cli process object", func() (json.RawMessage, error) { return normalizeCLIProcessList([]byte(`{"pid":1}`), time.Now()) }, "error"},
		{"jvm array", func() (json.RawMessage, error) { return normalizeJVMList([]byte(`[{"pid":1}]`)) }, `{"jvms":[{"pid":1}]}`},
		{"jvm null", func() (json.RawMessage, error) { return normalizeJVMList([]byte("null\n")) }, `{"jvms":[]}`},
		{"jvm empty output", func() (json.RawMessage, error) { return normalizeJVMList(nil) }, `{"jvms":[]}`},
		{"jvm wrapped", func() (json.RawMessage, error) {
			return normalizeJVMList([]byte(`{"jvms":[{"pid":2}],"jps_available":true}`))
		}, `{"jvms":[{"pid":2}]}`},
		{"jvm wrapped null", func() (json.RawMessage, error) { return normalizeJVMList([]byte(`{"jvms":null}`)) }, `{"jvms":[]}`},
		{"jvm string", func() (json.RawMessage, error) { return normalizeJVMList([]byte(`"jvms"`)) }, "error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.fn()
			if tc.want == "error" {
				if err == nil {
					t.Fatalf("accepted: %s", got)
				}
				return
			}
			if err != nil || string(got) != tc.want {
				t.Fatalf("got %s, %v; want %s", got, err, tc.want)
			}
		})
	}
}

func TestLocalSpectraListFallbackArgv(t *testing.T) {
	data, err := os.ReadFile("testdata/core-capabilities.json")
	if err != nil {
		t.Fatal(err)
	}
	fixture := filepath.Join(t.TempDir(), "caps.json")
	if err := os.WriteFile(fixture, data, 0600); err != nil {
		t.Fatal(err)
	}
	jvmOut := filepath.Join(t.TempDir(), "jvm.json")
	if err := os.WriteFile(jvmOut, []byte("[\n  {\"pid\": 4242, \"main_class\": \"GradleDaemon\"}\n]\n"), 0600); err != nil {
		t.Fatal(err)
	}
	// Only the exact fixed argv vectors succeed.
	path := makeFakeSpectra(t, fmt.Sprintf(`if [ "$*" = "capabilities --json" ]; then cat %s; exit 0; fi
if [ "$*" = "process -json --classify" ]; then printf '[{"pid":1,"role":"system"}]\n'; echo "classify: partial" >&2; exit 0; fi
if [ "$*" = "jvm -json" ]; then cat %s; exit 0; fi
echo "unexpected argv: $*" >&2; exit 2`, strconv.Quote(fixture), strconv.Quote(jvmOut)))
	auditor := &recordingAuditor{}
	a := &Agent{Runner: LocalSpectra{Path: path}, Daemon: SpectraDaemon{Socket: "/nonexistent/daemon.sock"}, Auditor: auditor, Policy: Policy{AllowProcesses: true}}
	before := time.Now().Add(-time.Second)
	result := decodeList(t, a.Handle(context.Background(), request(protocol.OperationProcessList, "")), protocol.OperationProcessList)
	var processes protocol.ProcessListData
	if err := json.Unmarshal(result.Data, &processes); err != nil {
		t.Fatal(err)
	}
	at, err := time.Parse(time.RFC3339, processes.SampledAt)
	if err != nil || at.Before(before) || len(processes.Processes) != 1 || string(processes.Processes[0]) != `{"pid":1,"role":"system"}` || result.SpectraVersion != "dev" {
		t.Fatalf("process data = %s (%v)", result.Data, err)
	}
	result = decodeList(t, a.Handle(context.Background(), request(protocol.OperationJVMList, "")), protocol.OperationJVMList)
	if string(result.Data) != `{"jvms":[{"pid":4242,"main_class":"GradleDaemon"}]}` {
		t.Fatalf("jvm data = %s", result.Data)
	}
	if len(auditor.events) != 4 || auditor.events[3].Outcome != "succeeded" || auditor.events[3].Operation != protocol.OperationJVMList {
		t.Fatalf("audit: %+v", auditor.events)
	}

	empty := makeFakeSpectra(t, `if [ "$*" = "process -json --classify" ]; then echo "no processes found" >&2; exit 0; fi; exit 2`)
	out, err := (LocalSpectra{Path: empty}).ProcessList(context.Background())
	if err != nil || !strings.Contains(string(out), `"processes":[]`) {
		t.Fatalf("empty process list = %s, %v", out, err)
	}
	failing := makeFakeSpectra(t, `echo "flag provided but not defined: -classify" >&2; exit 2`)
	if _, err := (LocalSpectra{Path: failing}).ProcessList(context.Background()); err == nil || !strings.Contains(err.Error(), "-classify") {
		t.Fatalf("failing process list err = %v", err)
	}
}

func TestDefaultDaemonSocket(t *testing.T) {
	home := func() (string, error) { return "/Users/alice", nil }
	env := func(values map[string]string) func(string) string {
		return func(k string) string { return values[k] }
	}
	cases := []struct {
		name string
		env  map[string]string
		goos string
		want string
	}{
		{"darwin default", nil, "darwin", "/Users/alice/.spectra/daemon.sock"},
		{"darwin ignores XDG", map[string]string{"XDG_RUNTIME_DIR": "/run/user/501"}, "darwin", "/Users/alice/.spectra/daemon.sock"},
		{"linux XDG", map[string]string{"XDG_RUNTIME_DIR": "/run/user/1000"}, "linux", "/run/user/1000/spectra/daemon.sock"},
		{"override", map[string]string{"SPECTRA_DAEMON_DIR": "/tmp/sd", "XDG_RUNTIME_DIR": "/run/user/1000"}, "linux", "/tmp/sd/daemon.sock"},
	}
	for _, tc := range cases {
		got, err := DefaultDaemonSocket(env(tc.env), home, tc.goos)
		if err != nil || got != tc.want {
			t.Fatalf("%s: got %q, %v; want %q", tc.name, got, err, tc.want)
		}
	}
	if _, err := DefaultDaemonSocket(env(nil), func() (string, error) { return "", fmt.Errorf("no home") }, "darwin"); err == nil {
		t.Fatal("resolved a socket without a home directory")
	}
}

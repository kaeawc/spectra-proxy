//go:build e2e

package e2e

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	protocol "github.com/kaeawc/spectra-protocol/protocol/v1"
	release "github.com/kaeawc/spectra-protocol/release/v1"
)

const calculatorApp = "/System/Applications/Calculator.app"

// step runs one dependent stage of a workflow as a subtest and stops the
// parent at the first failure.
func step(t *testing.T, name string, fn func(t *testing.T)) {
	t.Helper()
	if !t.Run(name, fn) {
		t.FailNow()
	}
}

func TestCompleteWorkflow(t *testing.T) {
	h := requireHarness(t)
	e := newEnv(t, h, newReleaseServer(t, h.release(t, v1), h.release(t, v2)))
	var a *agentProc
	var snapshotID string
	// Helpers report through whichever (sub)test is currently running.
	bind := func(t *testing.T) {
		e.t = t
		if a != nil {
			a.t = t
		}
	}

	step(t, "provision install", func(t *testing.T) {
		bind(t)
		e.mustProvision("install", "--version", v1)
		e.requireCurrent(v1, "")
	})
	// The agent belongs to the parent test so it outlives each step.
	bind(t)
	a = e.startAgent("--allow-snapshot")
	step(t, "health manifest", func(t *testing.T) {
		bind(t)
		m := a.health()
		if m.SpectraVersion != v1 {
			t.Fatalf("spectra_version = %q, want %q", m.SpectraVersion, v1)
		}
		for _, op := range []protocol.Operation{protocol.OperationInspect, protocol.OperationSnapshotCreate} {
			capability, err := protocol.Negotiate(m, op)
			if err != nil || capability.ResultSchema == nil {
				t.Fatalf("%s not advertised with a result schema: %+v %v", op, capability, err)
			}
		}
	})
	step(t, "inspect", func(t *testing.T) {
		bind(t)
		if runtime.GOOS != "darwin" {
			t.Skipf("inspect of %s requires macOS; running on %s", calculatorApp, runtime.GOOS)
		}
		_, out := a.mustCall(protocol.OperationInspect, fmt.Sprintf(`{"app_paths":[%q]}`, calculatorApp))
		d := out.Diagnostic
		if d.Schema != (protocol.SchemaRef{Name: protocol.SchemaInspect, Version: 1}) || d.SpectraVersion != v1 {
			t.Fatalf("inspect result schema=%+v spectra_version=%q", d.Schema, d.SpectraVersion)
		}
		if !strings.Contains(string(d.Data), calculatorApp) {
			t.Fatalf("inspect data does not mention %s: %.500s", calculatorApp, d.Data)
		}
	})
	step(t, "snapshot.create", func(t *testing.T) {
		bind(t)
		start := time.Now()
		id, result := a.mustCall(protocol.OperationSnapshotCreate, `{"include_apps":false}`)
		t.Logf("snapshot via agent took %s (direct warm-up %s; agent run cap 3m by default)", time.Since(start).Round(time.Millisecond), h.warmSnapshot.Round(time.Millisecond))
		snapshotID = id
		d := result.Diagnostic
		if d.Schema != (protocol.SchemaRef{Name: protocol.SchemaSnapshot, Version: 1}) || d.SpectraVersion != v1 {
			t.Fatalf("snapshot result schema=%+v spectra_version=%q", d.Schema, d.SpectraVersion)
		}
		var data map[string]json.RawMessage
		if err := json.Unmarshal(d.Data, &data); err != nil || len(data) == 0 {
			t.Fatalf("snapshot data is empty or not an object: %v", err)
		}
		a.requireRemoteError(protocol.OperationSnapshotCreate, `{"include_apps":true}`, 0, protocol.CodePermissionDenied)
	})
	step(t, "audit log", func(t *testing.T) {
		bind(t)
		var stages []string
		for _, event := range readAudit(t, e.audit) {
			if event.RequestID != snapshotID {
				continue
			}
			if event.Peer.Transport != "stdio" || event.Peer.LocalUser == "" || event.Operation != protocol.OperationSnapshotCreate {
				t.Fatalf("audit event %+v", event)
			}
			stages = append(stages, event.Stage+"/"+event.Outcome)
		}
		if !slices.Equal(stages, []string{"started/", "completed/succeeded"}) {
			t.Fatalf("audit stages for %s = %v", snapshotID, stages)
		}
	})
	step(t, "provision update", func(t *testing.T) {
		bind(t)
		e.mustProvision("update", "--version", v2)
		e.requireCurrent(v2, v1)
		// A diagnostic before health proves the running agent notices the
		// swapped binary on its own; health always re-probes capabilities.
		requireDiagnosticVersion(t, a, v2)
		if m := a.health(); m.SpectraVersion != v2 {
			t.Fatalf("after update spectra_version = %q, want %q", m.SpectraVersion, v2)
		}
	})
	step(t, "provision rollback", func(t *testing.T) {
		bind(t)
		e.mustProvision("rollback")
		e.requireCurrent(v1, v2)
		// A diagnostic before health proves the running agent notices the
		// swapped binary on its own; health always re-probes capabilities.
		requireDiagnosticVersion(t, a, v1)
		if m := a.health(); m.SpectraVersion != v1 {
			t.Fatalf("after rollback spectra_version = %q, want %q", m.SpectraVersion, v1)
		}
	})
	step(t, "provision uninstall", func(t *testing.T) {
		bind(t)
		if err := a.stop(); err != nil {
			t.Fatalf("agent exit: %v; stderr: %s", err, a.stderr.String())
		}
		e.mustProvision("uninstall")
		for _, name := range []string{"versions", "state.json", "current"} {
			if _, err := os.Lstat(filepath.Join(e.root, name)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("%s remains after uninstall: %v", name, err)
			}
		}
		r := h.run(t, h.agent, "serve-stdio", "--provision-root", e.root, "--audit-log", e.audit)
		if r.code != 2 || !strings.Contains(r.stderr, "--spectra path is required") {
			t.Fatalf("serve-stdio without provisioned spectra: %s", r)
		}
	})
}

func TestInstallRejectsTamperedArchive(t *testing.T) {
	h := requireHarness(t)
	rel := h.release(t, v1)
	rel.archive = slices.Clone(rel.archive)
	rel.archive[len(rel.archive)/2] ^= 0xff
	e := newEnv(t, h, newReleaseServer(t, rel))
	r := e.provision("install", "--version", v1)
	if r.code != 1 || !(strings.Contains(r.stderr, "digest") || strings.Contains(r.stderr, "sha256")) {
		t.Fatalf("tampered install: %s", r)
	}
	e.requireNothingInstalled()
}

func TestInstallRejectsUntrustedKey(t *testing.T) {
	h := requireHarness(t)
	e := newEnv(t, h, newReleaseServer(t, h.release(t, v1)))
	other, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	e.key = release.FormatPublicKey(other)
	r := e.provision("install", "--version", v1)
	if r.code != 1 || !strings.Contains(r.stderr, "untrusted") {
		t.Fatalf("untrusted install: %s", r)
	}
	e.requireNothingInstalled()
}

func TestInstallRejectsIncompatibleSpectra(t *testing.T) {
	h := requireHarness(t)
	const version = "v0.92.0"
	caps := fmt.Sprintf(`{"schema":{"name":"spectra.capabilities","version":2},"spectra_version":%q,"os":%q,"arch":%q,"interfaces":[]}`, version, runtime.GOOS, runtime.GOARCH)
	script := "#!/bin/sh\nif [ \"$1\" = capabilities ]; then printf '%s\\n' '" + caps + "'; exit 0; fi\nexit 1\n"
	e := newEnv(t, h, newReleaseServer(t, h.scriptRelease(t, version, script)))
	r := e.provision("install", "--version", version)
	if r.code != 1 || !strings.Contains(r.stderr, "incompatible Spectra capabilities schema version 2") {
		t.Fatalf("incompatible install: %s", r)
	}
	e.requireNothingInstalled()
}

func TestInterruptedUpdateKeepsCurrentVersion(t *testing.T) {
	h := requireHarness(t)
	src := newReleaseServer(t, h.release(t, v1), h.release(t, v2))
	e := newEnv(t, h, src)
	e.mustProvision("install", "--version", v1)
	st := src.stallArtifact(t, v2)

	cmd := h.command(t.Context(), h.agent, e.provisionArgs("update", "--version", v2)...)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	select {
	case <-st.sent:
	case err := <-waited:
		t.Fatalf("update exited before the stalled download: %v", err)
	case <-time.After(commandTimeout):
		t.Fatal("update never requested the artifact")
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-waited:
	case <-time.After(30 * time.Second):
		t.Fatal("killed update did not exit")
	}

	e.requireCurrent(v1, "")
	if _, err := os.Stat(filepath.Join(e.root, "versions", v2)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial %s installed: %v", v2, err)
	}
	leftovers := e.stagingEntries()
	e.mustProvision("update", "--version", v2)
	e.requireCurrent(v2, v1)
	for _, name := range leftovers {
		if slices.Contains(e.stagingEntries(), name) {
			t.Fatalf("staging entry %s from the killed update was not cleaned", name)
		}
	}
}

func TestDowngradeRefusedRollbackAllowed(t *testing.T) {
	h := requireHarness(t)
	e := newEnv(t, h, newReleaseServer(t, h.release(t, v1), h.release(t, v2)))
	e.mustProvision("install", "--version", v1)
	e.mustProvision("update", "--version", v2)
	r := e.provision("install", "--version", v1)
	if r.code != 1 || !strings.Contains(r.stderr, "downgrade") {
		t.Fatalf("downgrade install: %s", r)
	}
	e.requireCurrent(v2, v1)
	e.mustProvision("rollback")
	e.requireCurrent(v1, v2)
}

// The real snapshot takes seconds, so a 1 ms budget deterministically expires
// before it can finish; health beforehand caches the capabilities probe.
func TestDiagnosticTimeoutKeepsAgentUsable(t *testing.T) {
	h := requireHarness(t)
	e := newEnv(t, h, newReleaseServer(t, h.release(t, v1)))
	e.mustProvision("install", "--version", v1)
	a := e.startAgent("--allow-snapshot")
	if m := a.health(); m.SpectraVersion != v1 {
		t.Fatalf("spectra_version = %q", m.SpectraVersion)
	}
	a.requireRemoteError(protocol.OperationSnapshotCreate, `{"include_apps":false}`, 1, protocol.CodeTimeout)
	if m := a.health(); m.SpectraVersion != v1 {
		t.Fatalf("health after timeout: spectra_version = %q", m.SpectraVersion)
	}
}

// Peer denial happens in the tsnet transport before any request is read and
// needs a real tailnet identity, so it is not faked here. See
// TestDeniedConnectionIsAudited and TestAuthorizeAlwaysResolvesWhoIs in
// internal/transport/tsnet/tsnet_test.go, and TestTailscaleTwoMachine below.

// TestTailscaleTwoMachine calls a real serve-tsnet target on another tailnet
// machine; see scripts/e2e-tailscale.sh for setting up both sides.
func TestTailscaleTwoMachine(t *testing.T) {
	target := os.Getenv("SPECTRA_E2E_TAILSCALE_TARGET")
	if target == "" {
		t.Skip("SPECTRA_E2E_TAILSCALE_TARGET is unset; set host:port of a serve-tsnet agent (TS_AUTHKEY for this controller)")
	}
	b := requireProxyBinaries(t)
	health := tailscaleCall(t, b, target, "health", "")
	decoded, err := protocol.DecodeHealth(health)
	if err != nil {
		t.Fatalf("health manifest: %v", err)
	}
	t.Logf("target agent %s, spectra %s, operations %d", decoded.Capabilities.AgentVersion, decoded.Capabilities.SpectraVersion, len(decoded.Capabilities.Operations))
	app := os.Getenv("SPECTRA_E2E_TAILSCALE_APP")
	if app == "" {
		t.Log("SPECTRA_E2E_TAILSCALE_APP is unset; skipping remote inspect")
		return
	}
	inspect := tailscaleCall(t, b, target, "inspect", fmt.Sprintf(`{"app_paths":[%q]}`, app))
	if _, err := protocol.DecodeResult(inspect, protocol.OperationInspect); err != nil {
		t.Fatalf("inspect result: %v", err)
	}
}

func tailscaleCall(t *testing.T, b *proxyBinaries, target, op, params string) protocol.Response {
	t.Helper()
	args := []string{"call", "--target", target, "--operation", op, "--negotiate", "--timeout", "2m",
		"--tsnet-ephemeral", "--tsnet-hostname", "spectra-e2e-controller", "--tsnet-state-dir", t.TempDir()}
	if params != "" {
		args = append(args, "--params", params)
	}
	r := b.run(t, b.remote, args...)
	if r.code != 0 {
		t.Fatalf("spectra-remote call %s: %s", op, r)
	}
	var resp protocol.Response
	if err := json.Unmarshal([]byte(r.stdout), &resp); err != nil {
		t.Fatalf("decode %s response: %v", op, err)
	}
	return resp
}

// requireDiagnosticVersion runs the cheapest diagnostic available on this
// platform and checks which Spectra version stamped the result.
func requireDiagnosticVersion(t *testing.T, a *agentProc, want string) {
	t.Helper()
	op, params := protocol.OperationSnapshotCreate, `{"include_apps":false}`
	if runtime.GOOS == "darwin" {
		op, params = protocol.OperationInspect, fmt.Sprintf(`{"app_paths":[%q]}`, calculatorApp)
	}
	_, out := a.mustCall(op, params)
	if out.Diagnostic.SpectraVersion != want {
		t.Fatalf("%s result spectra_version = %q, want %q", op, out.Diagnostic.SpectraVersion, want)
	}
}

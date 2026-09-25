//go:build e2e

// Package e2e drives the built spectra-remote-agent and spectra-remote
// binaries against real Spectra builds served as signed releases.
package e2e

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	protocol "github.com/kaeawc/spectra-protocol/protocol/v1"
	release "github.com/kaeawc/spectra-protocol/release/v1"

	"github.com/kaeawc/spectra-proxy/internal/agent"
	"github.com/kaeawc/spectra-proxy/internal/controller"
	"github.com/kaeawc/spectra-proxy/internal/provision"
)

const (
	v1 = "v0.90.0"
	v2 = "v0.91.0"

	buildTimeout   = 10 * time.Minute
	commandTimeout = 3 * time.Minute
	callTimeout    = 2 * time.Minute
)

// proxyBinaries are this repository's commands, built once per run.
type proxyBinaries struct {
	dir, agent, remote string
	// home isolates every subprocess from the real HOME, so nothing touches
	// real install, audit, LaunchAgent, or Spectra cache locations.
	home string
}

// harness adds real Spectra builds and a release signing key.
type harness struct {
	*proxyBinaries
	spectra    map[string][]byte
	key        ed25519.PrivateKey
	trustedKey string
	// warmSnapshot is how long a direct, uncapped snapshot took while
	// populating Spectra's caches in the isolated HOME.
	warmSnapshot time.Duration
}

var (
	tempRoot   string
	binsOnce   sync.Once
	bins       *proxyBinaries
	binsErr    error
	coreOnce   sync.Once
	shared     *harness
	harnessErr error
)

func TestMain(m *testing.M) {
	code := m.Run()
	if tempRoot != "" {
		_ = os.RemoveAll(tempRoot)
	}
	os.Exit(code)
}

// requireProxyBinaries builds only this repository's commands.
func requireProxyBinaries(t *testing.T) *proxyBinaries {
	t.Helper()
	binsOnce.Do(func() { bins, binsErr = buildProxyBinaries() })
	if binsErr != nil {
		t.Fatal(binsErr)
	}
	return bins
}

// requireHarness skips unless SPECTRA_CORE_DIR names a kaeawc/spectra checkout.
func requireHarness(t *testing.T) *harness {
	t.Helper()
	core := os.Getenv("SPECTRA_CORE_DIR")
	if core == "" {
		t.Skip("SPECTRA_CORE_DIR is unset; point it at a kaeawc/spectra checkout to run the e2e suite")
	}
	b := requireProxyBinaries(t)
	coreOnce.Do(func() { shared, harnessErr = buildHarness(b, core) })
	if harnessErr != nil {
		t.Fatal(harnessErr)
	}
	return shared
}

func buildProxyBinaries() (*proxyBinaries, error) {
	var err error
	tempRoot, err = os.MkdirTemp("", "spectra-e2e-")
	if err != nil {
		return nil, fmt.Errorf("create e2e temp dir: %w", err)
	}
	b := &proxyBinaries{dir: filepath.Join(tempRoot, "bin"), home: filepath.Join(tempRoot, "home")}
	for _, dir := range []string{b.dir, b.home} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("create %s: %w", dir, err)
		}
	}
	moduleRoot, err := goOutput("", "env", "GOMOD")
	if err != nil {
		return nil, err
	}
	b.agent = filepath.Join(b.dir, "spectra-remote-agent")
	b.remote = filepath.Join(b.dir, "spectra-remote")
	for out, pkg := range map[string]string{b.agent: "./cmd/spectra-remote-agent", b.remote: "./cmd/spectra-remote"} {
		if _, err := goOutput(filepath.Dir(moduleRoot), "build", "-o", out, pkg); err != nil {
			return nil, err
		}
	}
	return b, nil
}

func buildHarness(b *proxyBinaries, core string) (*harness, error) {
	core, err := filepath.Abs(core)
	if err != nil {
		return nil, fmt.Errorf("resolve SPECTRA_CORE_DIR: %w", err)
	}
	h := &harness{proxyBinaries: b, spectra: make(map[string][]byte)}
	for _, version := range []string{v1, v2} {
		out := filepath.Join(b.dir, "spectra-"+version)
		if _, err := goOutput(core, "build", "-trimpath", "-ldflags", "-X main.version="+version, "-o", out, "./cmd/spectra"); err != nil {
			return nil, err
		}
		if err := h.verifySpectra(out, version); err != nil {
			return nil, err
		}
		if h.spectra[version], err = os.ReadFile(out); err != nil {
			return nil, fmt.Errorf("read built spectra: %w", err)
		}
	}
	if h.warmSnapshot, err = h.warmCaches(filepath.Join(b.dir, "spectra-"+v1)); err != nil {
		return nil, err
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate release key: %w", err)
	}
	h.key, h.trustedKey = priv, release.FormatPublicKey(pub)
	return h, nil
}

// warmCaches runs one snapshot outside the agent. A first snapshot in an empty
// HOME fills Spectra's caches and can approach the agent's fixed 30s run cap;
// that first-run cost is Spectra's, not the proxy workflow under test.
func (h *harness) warmCaches(binary string) (time.Duration, error) {
	ctx, cancel := context.WithTimeout(context.Background(), buildTimeout)
	defer cancel()
	start := time.Now()
	cmd := h.command(ctx, binary, "snapshot", "--json", "--no-apps")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return 0, fmt.Errorf("warm-up snapshot: %w: %s", err, stderr.String())
	}
	return time.Since(start), nil
}

func (h *harness) verifySpectra(binary, version string) error {
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	out, err := h.command(ctx, binary, "version").Output()
	if err != nil || strings.TrimSpace(string(out)) != version {
		return fmt.Errorf("built spectra version = %q, want %q: %v", out, version, err)
	}
	out, err = h.command(ctx, binary, "capabilities", "--json").Output()
	if err != nil {
		return fmt.Errorf("SPECTRA_CORE_DIR does not provide `spectra capabilities --json` (required by the proxy): %w", err)
	}
	var caps agent.SpectraCapabilities
	if err := json.Unmarshal(out, &caps); err != nil || caps.Schema.Name != "spectra.capabilities" || caps.SpectraVersion != version {
		return fmt.Errorf("unexpected capabilities output %s: %v", out, err)
	}
	return nil
}

func goOutput(dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), buildTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("go %s (in %s): %w: %s", strings.Join(args, " "), dir, err, stderr.String())
	}
	return strings.TrimSpace(string(out)), nil
}

// command builds a subprocess whose HOME and XDG directories are isolated.
func (b *proxyBinaries) command(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	isolated := map[string]string{
		"HOME":            b.home,
		"XDG_DATA_HOME":   filepath.Join(b.home, ".local", "share"),
		"XDG_CONFIG_HOME": filepath.Join(b.home, ".config"),
		"XDG_CACHE_HOME":  filepath.Join(b.home, ".cache"),
	}
	for _, kv := range os.Environ() {
		if key, _, _ := strings.Cut(kv, "="); isolated[key] == "" {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	for key, value := range isolated {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	cmd.WaitDelay = 5 * time.Second
	return cmd
}

type cmdResult struct {
	code           int
	stdout, stderr string
}

func (r cmdResult) String() string {
	return fmt.Sprintf("exit %d\nstdout: %s\nstderr: %s", r.code, r.stdout, r.stderr)
}

func (b *proxyBinaries) run(t *testing.T, name string, args ...string) cmdResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	cmd := b.command(ctx, name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		t.Fatalf("%s %v timed out after %s: %s", filepath.Base(name), args, commandTimeout, stderr.String())
	}
	result := cmdResult{stdout: stdout.String(), stderr: stderr.String()}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		result.code = exitErr.ExitCode()
	} else if err != nil {
		t.Fatalf("run %s: %v", name, err)
	}
	return result
}

// releaseFiles is one signed release as served from /<version>/<file>.
type releaseFiles struct {
	version   string
	manifest  []byte
	signature []byte
	archive   []byte
	artifact  release.Artifact
}

func artifactName(version string) string {
	return fmt.Sprintf("spectra_%s_%s_%s.tar.gz", version, runtime.GOOS, runtime.GOARCH)
}

// packageRelease mirrors the core dist layout: <top>/bin/spectra plus a README.
func packageRelease(version string, binary []byte) ([]byte, error) {
	top := strings.TrimSuffix(artifactName(version), ".tar.gz")
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	entries := []struct {
		name string
		mode int64
		body []byte
	}{
		{top + "/", 0o755, nil},
		{top + "/bin/", 0o755, nil},
		{top + "/bin/spectra", 0o755, binary},
		{top + "/README.md", 0o644, []byte("Spectra " + version + " (e2e test build)\n")},
	}
	for _, e := range entries {
		hdr := &tar.Header{Name: e.name, Mode: e.mode, Size: int64(len(e.body)), Typeflag: tar.TypeReg, ModTime: time.Unix(0, 0)}
		if strings.HasSuffix(e.name, "/") {
			hdr.Typeflag = tar.TypeDir
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return nil, fmt.Errorf("write tar header: %w", err)
		}
		if _, err := tw.Write(e.body); err != nil {
			return nil, fmt.Errorf("write tar entry: %w", err)
		}
	}
	if err := tw.Close(); err != nil {
		return nil, fmt.Errorf("close tar: %w", err)
	}
	if err := gz.Close(); err != nil {
		return nil, fmt.Errorf("close gzip: %w", err)
	}
	return buf.Bytes(), nil
}

// signRelease writes the manifest and detached signature per release/v1 FORMAT.md.
func signRelease(t *testing.T, priv ed25519.PrivateKey, version string, archive []byte) releaseFiles {
	t.Helper()
	sum := sha256.Sum256(archive)
	a := release.Artifact{OS: runtime.GOOS, Arch: runtime.GOARCH, Path: artifactName(version), SHA256: hex.EncodeToString(sum[:]), Size: int64(len(archive)), Format: "tar.gz"}
	m := release.Manifest{
		Schema: release.ManifestSchema, Product: "spectra", Version: version,
		PublishedAt: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), KeyID: release.KeyID(priv.Public().(ed25519.PublicKey)),
		CapabilitiesSchemaVersion: 1, Artifacts: []release.Artifact{a},
	}
	manifest, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	sig, err := release.Sign(manifest, priv)
	if err != nil {
		t.Fatal(err)
	}
	return releaseFiles{version: version, manifest: manifest, signature: sig, archive: archive, artifact: a}
}

// release packages and signs a real Spectra build.
func (h *harness) release(t *testing.T, version string) releaseFiles {
	t.Helper()
	archive, err := packageRelease(version, h.spectra[version])
	if err != nil {
		t.Fatal(err)
	}
	return signRelease(t, h.key, version, archive)
}

// scriptRelease packages and signs a shell script as bin/spectra.
func (h *harness) scriptRelease(t *testing.T, version, script string) releaseFiles {
	t.Helper()
	archive, err := packageRelease(version, []byte(script))
	if err != nil {
		t.Fatal(err)
	}
	return signRelease(t, h.key, version, archive)
}

// stall holds an artifact download open after half of it has been sent.
type stall struct {
	sent    chan struct{}
	release chan struct{}
}

type releaseServer struct {
	srv    *httptest.Server
	url    string
	caFile string
	mu     sync.Mutex
	files  map[string]releaseFiles
	stalls map[string]*stall
}

func newReleaseServer(t *testing.T, releases ...releaseFiles) *releaseServer {
	t.Helper()
	s := &releaseServer{files: make(map[string]releaseFiles), stalls: make(map[string]*stall)}
	for _, r := range releases {
		s.files[r.version] = r
	}
	s.srv = httptest.NewTLSServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.srv.Close)
	s.url = s.srv.URL
	s.caFile = filepath.Join(t.TempDir(), "source-ca.pem")
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: s.srv.Certificate().Raw})
	if err := os.WriteFile(s.caFile, certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	return s
}

// stallArtifact makes the next download of version's artifact stop halfway.
// The cleanup runs before the server's Close, which waits for handlers.
func (s *releaseServer) stallArtifact(t *testing.T, version string) *stall {
	st := &stall{sent: make(chan struct{}), release: make(chan struct{})}
	s.mu.Lock()
	s.stalls[version] = st
	s.mu.Unlock()
	t.Cleanup(func() { close(st.release) })
	return st
}

func (s *releaseServer) serve(w http.ResponseWriter, r *http.Request) {
	version, name, ok := strings.Cut(strings.Trim(r.URL.Path, "/"), "/")
	s.mu.Lock()
	f, found := s.files[version]
	st := s.stalls[version]
	if st != nil && found && name == f.artifact.Path {
		delete(s.stalls, version)
	} else {
		st = nil
	}
	s.mu.Unlock()
	if !ok || !found {
		http.NotFound(w, r)
		return
	}
	switch name {
	case "spectra-release.json":
		_, _ = w.Write(f.manifest)
	case "spectra-release.json.sig":
		_, _ = w.Write(f.signature)
	case f.artifact.Path:
		if st == nil {
			_, _ = w.Write(f.archive)
			return
		}
		w.Header().Set("Content-Length", fmt.Sprint(len(f.archive)))
		_, _ = w.Write(f.archive[:len(f.archive)/2])
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		close(st.sent)
		select {
		case <-st.release:
		case <-r.Context().Done():
		}
	default:
		http.NotFound(w, r)
	}
}

// env is one isolated provisioning root and its release source.
type env struct {
	t     *testing.T
	h     *harness
	root  string
	audit string
	src   *releaseServer
	key   string
}

func newEnv(t *testing.T, h *harness, src *releaseServer) *env {
	t.Helper()
	dir := t.TempDir()
	return &env{t: t, h: h, root: filepath.Join(dir, "spectra"), audit: filepath.Join(dir, "logs", "agent.audit.jsonl"), src: src, key: h.trustedKey}
}

func (e *env) provisionArgs(sub string, extra ...string) []string {
	args := []string{"provision", sub, "--root", e.root, "--source", e.src.url, "--trusted-key", e.key, "--source-ca-file", e.src.caFile}
	return append(args, extra...)
}

func (e *env) provision(sub string, extra ...string) cmdResult {
	e.t.Helper()
	return e.h.run(e.t, e.h.agent, e.provisionArgs(sub, extra...)...)
}

func (e *env) mustProvision(sub string, extra ...string) {
	e.t.Helper()
	if r := e.provision(sub, extra...); r.code != 0 {
		e.t.Fatalf("provision %s %v: %s", sub, extra, r)
	}
}

func (e *env) status() provision.StatusReport {
	e.t.Helper()
	r := e.h.run(e.t, e.h.agent, "provision", "status", "--root", e.root, "--json")
	if r.code != 0 {
		e.t.Fatalf("provision status: %s", r)
	}
	var report provision.StatusReport
	if err := json.Unmarshal([]byte(r.stdout), &report); err != nil {
		e.t.Fatalf("decode status %q: %v", r.stdout, err)
	}
	return report
}

func (e *env) requireCurrent(current, previous string) {
	e.t.Helper()
	s := e.status()
	if s.Current != current || s.Previous != previous || s.CurrentDriftDetected {
		e.t.Fatalf("status current=%q previous=%q drift=%t, want current=%q previous=%q", s.Current, s.Previous, s.CurrentDriftDetected, current, previous)
	}
}

func (e *env) requireNothingInstalled() {
	e.t.Helper()
	e.requireCurrent("", "")
	if _, err := os.Lstat(filepath.Join(e.root, "current")); !errors.Is(err, os.ErrNotExist) {
		e.t.Fatalf("current exists: %v", err)
	}
	entries, _ := os.ReadDir(filepath.Join(e.root, "versions"))
	if len(entries) != 0 {
		e.t.Fatalf("versions installed: %v", entries)
	}
}

func (e *env) stagingEntries() []string {
	e.t.Helper()
	entries, err := os.ReadDir(filepath.Join(e.root, "staging"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		e.t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

// agentProc is a running serve-stdio agent driven through internal/controller.
type agentProc struct {
	t      *testing.T
	cmd    *exec.Cmd
	conn   *pipeConn
	sess   *controller.Session
	stderr *lockedBuffer
	done   chan error
	nextID int
}

// pipeConn gives the controller deadline support so every read is bounded.
type pipeConn struct{ r, w *os.File }

func (p *pipeConn) Read(b []byte) (int, error)  { return p.r.Read(b) }
func (p *pipeConn) Write(b []byte) (int, error) { return p.w.Write(b) }
func (p *pipeConn) SetDeadline(t time.Time) error {
	if err := p.r.SetDeadline(t); err != nil {
		return err
	}
	return p.w.SetDeadline(t)
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// startAgent runs serve-stdio against the provisioned Spectra (no --spectra).
func (e *env) startAgent(extra ...string) *agentProc {
	e.t.Helper()
	args := append([]string{"serve-stdio", "--provision-root", e.root, "--allow-app-root", "/System/Applications", "--allow-app-root", "/Applications", "--audit-log", e.audit}, extra...)
	inR, inW, err := os.Pipe()
	if err != nil {
		e.t.Fatal(err)
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		e.t.Fatal(err)
	}
	cmd := e.h.command(context.Background(), e.h.agent, args...)
	stderr := &lockedBuffer{}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = inR, outW, stderr
	if err := cmd.Start(); err != nil {
		e.t.Fatalf("start agent: %v", err)
	}
	_ = inR.Close()
	_ = outW.Close()
	a := &agentProc{t: e.t, cmd: cmd, conn: &pipeConn{r: outR, w: inW}, stderr: stderr, done: make(chan error, 1)}
	a.sess = controller.NewSession(a.conn)
	go func() { a.done <- cmd.Wait() }()
	owner := e.t
	owner.Cleanup(func() {
		if err := a.stop(); err != nil {
			owner.Logf("agent stop: %v; stderr: %s", err, stderr.String())
		}
	})
	return a
}

// stop closes stdin so the agent exits cleanly, killing it after a bound.
func (a *agentProc) stop() error {
	if a.cmd == nil {
		return nil
	}
	_ = a.conn.w.Close()
	defer func() { _ = a.conn.r.Close(); a.cmd = nil }()
	select {
	case err := <-a.done:
		return err
	case <-time.After(15 * time.Second):
		_ = a.cmd.Process.Kill()
		<-a.done
		return fmt.Errorf("agent did not exit after stdin closed")
	}
}

func (a *agentProc) id() string {
	a.nextID++
	return fmt.Sprintf("e2e-%d", a.nextID)
}

// call sends one typed request and interprets the validated response.
func (a *agentProc) call(op protocol.Operation, params string, timeoutMS int) (string, controller.Outcome, error) {
	a.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	req := protocol.Request{ProtocolVersion: protocol.Version, RequestID: a.id(), Operation: op, TimeoutMS: timeoutMS}
	if params != "" {
		req.Params = json.RawMessage(params)
	}
	resp, err := controller.Call(ctx, a.sess, req)
	if err != nil {
		a.t.Fatalf("%s call failed: %v; agent stderr: %s", op, err, a.stderr.String())
	}
	out, err := controller.Interpret(op, resp)
	return req.RequestID, out, err
}

func (a *agentProc) mustCall(op protocol.Operation, params string) (string, controller.Outcome) {
	a.t.Helper()
	id, out, err := a.call(op, params, 0)
	if err != nil {
		a.t.Fatalf("%s: %v; agent stderr: %s", op, err, a.stderr.String())
	}
	return id, out
}

func (a *agentProc) requireRemoteError(op protocol.Operation, params string, timeoutMS int, want protocol.ErrorCode) {
	a.t.Helper()
	_, _, err := a.call(op, params, timeoutMS)
	var remote *controller.RemoteError
	if !errors.As(err, &remote) || remote.Code != want {
		a.t.Fatalf("%s error = %v, want remote %s", op, err, want)
	}
}

// health returns a manifest that has passed protocol validation.
func (a *agentProc) health() protocol.CapabilityManifest {
	a.t.Helper()
	_, out := a.mustCall(protocol.OperationHealth, "")
	if err := out.Health.Capabilities.Validate(); err != nil {
		a.t.Fatalf("manifest: %v", err)
	}
	return out.Health.Capabilities
}

func readAudit(t *testing.T, path string) []agent.AuditEvent {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read audit log: %v", err)
	}
	var events []agent.AuditEvent
	for _, line := range bytes.Split(bytes.TrimSpace(data), []byte("\n")) {
		var event agent.AuditEvent
		if err := json.Unmarshal(line, &event); err != nil {
			t.Fatalf("audit line %q: %v", line, err)
		}
		events = append(events, event)
	}
	return events
}

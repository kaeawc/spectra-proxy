package provision

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
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	release "github.com/kaeawc/spectra-protocol/release/v1"
)

type archiveEntry struct {
	name string
	kind byte
	body string
	link string
}
type releaseFixture struct {
	manifest, signature, archive []byte
	artifact                     release.Artifact
}

func makeArchive(t *testing.T, top string, entries []archiveEntry) []byte {
	t.Helper()
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		h := &tar.Header{Name: e.name, Typeflag: e.kind, Linkname: e.link, Mode: 0o755, Size: int64(len(e.body))}
		if e.kind != tar.TypeReg && e.kind != tar.TypeRegA {
			h.Size = 0
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Size > 0 {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func fixture(t *testing.T, version string, private ed25519.PrivateKey, entries []archiveEntry) releaseFixture {
	t.Helper()
	name := fmt.Sprintf("spectra-%s-%s-%s.tar.gz", version, runtime.GOOS, runtime.GOARCH)
	top := strings.TrimSuffix(name, ".tar.gz")
	if entries == nil {
		entries = []archiveEntry{{name: top + "/bin/spectra", kind: tar.TypeReg, body: version}}
	}
	archive := makeArchive(t, top, entries)
	sum := sha256.Sum256(archive)
	a := release.Artifact{OS: runtime.GOOS, Arch: runtime.GOARCH, Path: name, SHA256: hex.EncodeToString(sum[:]), Size: int64(len(archive)), Format: "tar.gz"}
	m := release.Manifest{Schema: release.ManifestSchema, Product: "spectra", Version: version, PublishedAt: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), KeyID: release.KeyID(private.Public().(ed25519.PublicKey)), CapabilitiesSchemaVersion: 1, Artifacts: []release.Artifact{a}}
	manifest, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	sig, err := release.Sign(manifest, private)
	if err != nil {
		t.Fatal(err)
	}
	return releaseFixture{manifest, sig, archive, a}
}

type fakeRunner struct{ change func(*capabilities) }

func (r fakeRunner) Capabilities(_ context.Context, binary string) ([]byte, error) {
	data, err := os.ReadFile(binary)
	if err != nil {
		return nil, err
	}
	c := capabilities{Schema: schemaRef{"spectra.capabilities", 1}, SpectraVersion: string(data), OS: runtime.GOOS, Arch: runtime.GOARCH, Interfaces: []capabilityInterface{{Name: "inspect", ResultSchema: schemaRef{"spectra.inspect", 1}}, {Name: "snapshot", ResultSchema: schemaRef{"spectra.snapshot", 1}}}}
	if r.change != nil {
		r.change(&c)
	}
	return json.Marshal(c)
}

type testServer struct {
	server   *httptest.Server
	releases map[string]releaseFixture
	key      string
	root     string
}

func newTestServer(t *testing.T) (*testServer, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ts := &testServer{releases: make(map[string]releaseFixture), key: release.FormatPublicKey(pub), root: t.TempDir()}
	ts.server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if len(parts) != 2 {
			http.NotFound(w, r)
			return
		}
		f, ok := ts.releases[parts[0]]
		if !ok {
			http.NotFound(w, r)
			return
		}
		switch parts[1] {
		case manifestName:
			w.Write(f.manifest)
		case signatureName:
			w.Write(f.signature)
		case f.artifact.Path:
			w.Write(f.archive)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(ts.server.Close)
	return ts, priv
}
func (s *testServer) opts() Options {
	return Options{Config: Config{Root: s.root, Sources: []string{s.server.URL}, TrustedKeys: []string{s.key}}, Client: s.server.Client(), Runner: fakeRunner{}, Now: func() time.Time { return time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC) }, allowLoopbackHTTP: true}
}

func TestInstallUpdateRollbackStatusUninstall(t *testing.T) {
	s, priv := newTestServer(t)
	s.releases["v1.0.0"] = fixture(t, "v1.0.0", priv, nil)
	s.releases["v1.1.0"] = fixture(t, "v1.1.0", priv, nil)
	o := s.opts()
	if _, err := Install(context.Background(), o, "v1.0.0"); err != nil {
		t.Fatal(err)
	}
	link, err := os.Readlink(filepath.Join(s.root, "current"))
	if err != nil || link != filepath.Join("versions", "v1.0.0") {
		t.Fatalf("link %q: %v", link, err)
	}
	stateBytes, err := os.ReadFile(filepath.Join(s.root, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(stateBytes, []byte(`"schema":"spectra-proxy.provision/1"`)) {
		t.Fatalf("state %s", stateBytes)
	}
	if _, err := Update(context.Background(), o, "v1.1.0"); err != nil {
		t.Fatal(err)
	}
	report, err := Status(o)
	if err != nil {
		t.Fatal(err)
	}
	if report.Current != "v1.1.0" || report.Previous != "v1.0.0" || len(report.Versions) != 2 {
		t.Fatalf("status %+v", report)
	}
	if _, err := Rollback(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	report, err = Status(o)
	if err != nil {
		t.Fatal(err)
	}
	if report.Current != "v1.0.0" || report.Previous != "v1.1.0" {
		t.Fatalf("rollback %+v", report)
	}
	if err := os.WriteFile(BinaryPath(s.root), []byte("tampered"), 0o755); err != nil {
		t.Fatal(err)
	}
	report, err = Status(o)
	if err != nil || !report.CurrentDriftDetected {
		t.Fatalf("drift %+v %v", report, err)
	}
	if err := Uninstall(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.root); !os.IsNotExist(err) {
		t.Fatalf("root remains: %v", err)
	}
}

func TestVersionRulesAndInterruptedCommit(t *testing.T) {
	s, priv := newTestServer(t)
	for _, v := range []string{"v1.0.0", "v1.1.0", "v1.2.0"} {
		s.releases[v] = fixture(t, v, priv, nil)
	}
	o := s.opts()
	if _, err := Install(context.Background(), o, "v1.1.0"); err != nil {
		t.Fatal(err)
	}
	if _, err := Install(context.Background(), o, "v1.0.0"); err == nil || !strings.Contains(err.Error(), "downgrade") {
		t.Fatalf("downgrade: %v", err)
	}
	if _, err := Update(context.Background(), o, "v1.1.0"); err == nil {
		t.Fatal("equal update accepted")
	}
	before, _ := os.ReadFile(filepath.Join(s.root, "state.json"))
	linkBefore, _ := os.Readlink(filepath.Join(s.root, "current"))
	o.beforeCommit = func() error { return fmt.Errorf("simulated interruption") }
	if _, err := Update(context.Background(), o, "v1.2.0"); err == nil {
		t.Fatal("interruption accepted")
	}
	after, _ := os.ReadFile(filepath.Join(s.root, "state.json"))
	linkAfter, _ := os.Readlink(filepath.Join(s.root, "current"))
	if !bytes.Equal(before, after) || linkBefore != linkAfter {
		t.Fatal("state or current changed before commit")
	}
	stale := filepath.Join(s.root, "staging", "stale")
	if err := os.WriteFile(stale, []byte("debris"), 0o600); err != nil {
		t.Fatal(err)
	}
	o.beforeCommit = nil
	if _, err := Update(context.Background(), o, "v1.2.0"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale staging remains: %v", err)
	}
}

func TestRollbackRejectsTamperedOrIncompatiblePrevious(t *testing.T) {
	s, priv := newTestServer(t)
	for _, v := range []string{"v1.0.0", "v1.1.0"} {
		s.releases[v] = fixture(t, v, priv, nil)
	}
	o := s.opts()
	if _, err := Install(context.Background(), o, "v1.0.0"); err != nil {
		t.Fatal(err)
	}
	if _, err := Update(context.Background(), o, "v1.1.0"); err != nil {
		t.Fatal(err)
	}
	stateBefore, _ := os.ReadFile(filepath.Join(s.root, "state.json"))
	linkBefore, _ := os.Readlink(filepath.Join(s.root, "current"))
	previous := versionBinary(s.root, "v1.0.0")
	if err := os.WriteFile(previous, []byte("tampered"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Rollback(context.Background(), o); err == nil || !strings.Contains(err.Error(), "tampered") {
		t.Fatalf("tampered rollback: %v", err)
	}
	if err := os.WriteFile(previous, []byte("v1.0.0"), 0o755); err != nil {
		t.Fatal(err)
	}
	o.Runner = fakeRunner{change: func(c *capabilities) { c.Schema.Version = 2 }}
	if _, err := Rollback(context.Background(), o); err == nil {
		t.Fatal("incompatible rollback accepted")
	}
	stateAfter, _ := os.ReadFile(filepath.Join(s.root, "state.json"))
	linkAfter, _ := os.Readlink(filepath.Join(s.root, "current"))
	if !bytes.Equal(stateBefore, stateAfter) || linkBefore != linkAfter {
		t.Fatal("failed rollback changed active state")
	}
	// Rollback re-verifies the recorded sha256 and re-runs the compatibility
	// check; it downloads nothing and needs no trusted release key.
	o = s.opts()
	o.Config.TrustedKeys = []string{}
	result, err := Rollback(context.Background(), o)
	if err != nil {
		t.Fatalf("rollback without trusted keys: %v", err)
	}
	if result.Version != "v1.0.0" || result.Previous != "v1.1.0" {
		t.Fatalf("rollback without trusted keys result = %+v", result)
	}
}

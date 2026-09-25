package provision

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	release "github.com/kaeawc/spectra-protocol/release/v1"
)

func TestVerificationFailuresNeverInstall(t *testing.T) {
	cases := []struct {
		name   string
		change func(*testServer, ed25519.PrivateKey, *Options)
		want   error
	}{
		{"artifact digest", func(s *testServer, _ ed25519.PrivateKey, _ *Options) {
			f := s.releases["v1.0.0"]
			f.archive[len(f.archive)/2] ^= 1
			s.releases["v1.0.0"] = f
		}, release.ErrArtifactMismatch},
		{"bad signature", func(s *testServer, _ ed25519.PrivateKey, _ *Options) {
			f := s.releases["v1.0.0"]
			var sig release.Signature
			_ = json.Unmarshal(f.signature, &sig)
			sig.Signature = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=="
			f.signature, _ = json.Marshal(sig)
			s.releases["v1.0.0"] = f
		}, release.ErrBadSignature},
		{"untrusted key", func(_ *testServer, _ ed25519.PrivateKey, o *Options) {
			pub, _, _ := ed25519.GenerateKey(rand.Reader)
			o.Config.TrustedKeys = []string{release.FormatPublicKey(pub)}
		}, release.ErrUntrustedKey},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, priv := newTestServer(t)
			s.releases["v1.0.0"] = fixture(t, "v1.0.0", priv, nil)
			o := s.opts()
			tc.change(s, priv, &o)
			_, err := Install(context.Background(), o, "v1.0.0")
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
			assertNotInstalled(t, s.root)
		})
	}
}

func TestManifestVersionMismatch(t *testing.T) {
	s, priv := newTestServer(t)
	s.releases["v1.0.0"] = fixture(t, "v1.1.0", priv, nil)
	_, err := Install(context.Background(), s.opts(), "v1.0.0")
	if err == nil || !strings.Contains(err.Error(), "does not match requested") {
		t.Fatalf("mismatch: %v", err)
	}
	assertNotInstalled(t, s.root)
}

func TestNoTrustedKeysAndHTTPSource(t *testing.T) {
	s, priv := newTestServer(t)
	s.releases["v1.0.0"] = fixture(t, "v1.0.0", priv, nil)
	o := s.opts()
	o.Config.TrustedKeys = []string{}
	if _, err := Install(context.Background(), o, "v1.0.0"); err == nil || !strings.Contains(err.Error(), "no trusted Spectra release keys") {
		t.Fatalf("empty trust: %v", err)
	}
	o = s.opts()
	o.Config.Sources = []string{"http://example.com/releases"}
	if _, err := Install(context.Background(), o, "v1.0.0"); err == nil || !strings.Contains(err.Error(), "HTTPS") {
		t.Fatalf("http accepted: %v", err)
	}
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { s.server.Config.Handler.ServeHTTP(w, r) }))
	defer httpServer.Close()
	o = s.opts()
	o.Config.Sources = []string{httpServer.URL}
	o.Client = httpServer.Client()
	o.allowLoopbackHTTP = true
	if _, err := Install(context.Background(), o, "v1.0.0"); err != nil {
		t.Fatalf("loopback test escape: %v", err)
	}
}

func TestRedirectHostPolicy(t *testing.T) {
	s, priv := newTestServer(t)
	s.releases["v1.0.0"] = fixture(t, "v1.0.0", priv, nil)
	redirect := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, s.server.URL+r.URL.Path, http.StatusFound)
	}))
	defer redirect.Close()
	o := s.opts()
	o.Config.Sources = []string{redirect.URL}
	o.Client = &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	if _, err := Install(context.Background(), o, "v1.0.0"); err == nil || !strings.Contains(err.Error(), "disallowed host") {
		t.Fatalf("disallowed redirect: %v", err)
	}
	assertNotInstalled(t, s.root)
	u, _ := url.Parse(s.server.URL)
	o.Config.AllowedRedirectHosts = []string{u.Host}
	if _, err := Install(context.Background(), o, "v1.0.0"); err != nil {
		t.Fatalf("allowed redirect: %v", err)
	}
}

func TestSourceFallbackOnlyForTransportFailures(t *testing.T) {
	s, priv := newTestServer(t)
	s.releases["v1.0.0"] = fixture(t, "v1.0.0", priv, nil)
	missing := httptest.NewTLSServer(http.NotFoundHandler())
	defer missing.Close()
	o := s.opts()
	o.Config.Sources = []string{missing.URL, s.server.URL}
	o.Client = &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	if _, err := Install(context.Background(), o, "v1.0.0"); err != nil {
		t.Fatalf("404 fallback: %v", err)
	}
	// A responsive source with bad signed metadata must stop before the good source.
	bad := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("invalid signed metadata")) }))
	defer bad.Close()
	o.Config.Root = t.TempDir()
	o.Config.Sources = []string{bad.URL, s.server.URL}
	if _, err := Install(context.Background(), o, "v1.0.0"); err == nil || !strings.Contains(err.Error(), bad.URL) {
		t.Fatalf("verification fallback: %v", err)
	}
	assertNotInstalled(t, o.Config.Root)
}

func assertNotInstalled(t *testing.T, root string) {
	t.Helper()
	if _, err := Status(Options{Config: Config{Root: root}}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"current", "state.json"} {
		if _, err := os.Stat(filepath.Join(root, name)); !os.IsNotExist(err) {
			t.Fatalf("%s exists: %v", name, err)
		}
	}
}

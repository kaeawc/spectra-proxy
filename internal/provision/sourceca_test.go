package provision

import (
	"bytes"
	"context"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeCAFile(t *testing.T, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSourceCAFileReplacesSystemRoots(t *testing.T) {
	s, priv := newTestServer(t)
	s.releases["v1.0.0"] = fixture(t, "v1.0.0", priv, nil)
	o := s.opts()
	o.Client = nil
	if _, err := Install(context.Background(), o, "v1.0.0"); err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("self-signed source accepted without CA file: %v", err)
	}
	assertNotInstalled(t, s.root)
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: s.server.Certificate().Raw})
	o.Config.SourceCAFile = writeCAFile(t, caPEM)
	if _, err := Install(context.Background(), o, "v1.0.0"); err != nil {
		t.Fatalf("install with source CA file: %v", err)
	}
}

func TestSourceCAFileIsStrict(t *testing.T) {
	s, _ := newTestServer(t)
	cert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: s.server.Certificate().Raw})
	cases := map[string][]byte{
		"empty":         nil,
		"garbage":       []byte("not a certificate"),
		"private key":   pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte{1, 2, 3}}),
		"bad der":       pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte{1, 2, 3}}),
		"trailing data": append(bytes.Clone(cert), []byte("trailing")...),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := loadSourceCAs(writeCAFile(t, data)); err == nil {
				t.Fatal("invalid CA file accepted")
			}
		})
	}
	if _, err := loadSourceCAs(filepath.Join(t.TempDir(), "absent.pem")); err == nil {
		t.Fatal("missing CA file accepted")
	}
	if _, err := loadSourceCAs(writeCAFile(t, append(bytes.Clone(cert), cert...))); err != nil {
		t.Fatalf("certificate bundle rejected: %v", err)
	}
}

func TestCLISourceCAFileFlag(t *testing.T) {
	var out, errout bytes.Buffer
	root := filepath.Join(t.TempDir(), "spectra")
	ca := writeCAFile(t, []byte("not a certificate"))
	if code := Run(context.Background(), []string{"status", "--root", root, "--source-ca-file", ca}, &out, &errout); code != 1 || !strings.Contains(errout.String(), "source CA file") {
		t.Fatalf("code %d: %s", code, errout.String())
	}
}

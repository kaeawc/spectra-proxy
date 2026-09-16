package tsnet

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNewServerRequiresIdentity(t *testing.T) {
	_, err := newServer(Config{StateDir: t.TempDir()})
	if err == nil {
		t.Fatal("newServer succeeded without hostname")
	}
}

func TestNewServerSecuresStateDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	server, err := newServer(Config{StateDir: dir, Hostname: "spectra-test"})
	if err != nil {
		t.Fatal(err)
	}
	if server.Dir != dir {
		t.Fatalf("server directory = %q, want %q", server.Dir, dir)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Fatalf("state directory mode = %o, want 700", got)
	}
}

func TestNormalize(t *testing.T) {
	if got := normalize(" Alice-Mac.Tailnet.ts.net. "); got != "alice-mac.tailnet.ts.net" {
		t.Fatalf("normalize() = %q", got)
	}
}

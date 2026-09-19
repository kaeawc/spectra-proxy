package tsnet

import (
	"os"
	"path/filepath"
	"testing"
	"time"
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

func TestConnectionLimiterBoundsConcurrentSessions(t *testing.T) {
	limiter, err := newConnectionLimiter(2)
	if err != nil {
		t.Fatal(err)
	}
	if !limiter.tryAcquire() || !limiter.tryAcquire() {
		t.Fatal("limiter rejected an available slot")
	}
	if limiter.tryAcquire() {
		t.Fatal("limiter accepted more than its configured limit")
	}
	limiter.release()
	if !limiter.tryAcquire() {
		t.Fatal("limiter did not release a slot")
	}
}

func TestConnectionLimiterDefaultsAndRejectsNegativeLimit(t *testing.T) {
	limiter, err := newConnectionLimiter(0)
	if err != nil {
		t.Fatal(err)
	}
	if cap(limiter.slots) != DefaultMaxConnections {
		t.Fatalf("default cap = %d, want %d", cap(limiter.slots), DefaultMaxConnections)
	}
	if _, err := newConnectionLimiter(-1); err == nil {
		t.Fatal("newConnectionLimiter accepted a negative limit")
	}
}

func TestSessionTimeoutDefaultsAndAcceptsConfiguredValue(t *testing.T) {
	if got := sessionTimeout(0); got != DefaultSessionTimeout {
		t.Fatalf("default session timeout = %s, want %s", got, DefaultSessionTimeout)
	}
	if got := sessionTimeout(-time.Second); got != DefaultSessionTimeout {
		t.Fatalf("negative session timeout = %s, want %s", got, DefaultSessionTimeout)
	}
	if got := sessionTimeout(15 * time.Second); got != 15*time.Second {
		t.Fatalf("configured session timeout = %s", got)
	}
}

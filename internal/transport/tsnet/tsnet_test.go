package tsnet

import (
	"context"
	"encoding/json"
	"errors"
	protocol "github.com/kaeawc/spectra-protocol/protocol/v1"
	"github.com/kaeawc/spectra-proxy/internal/agent"
	"net"
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

type fakeWhoIs struct {
	identity *peerIdentity
	err      error
	calls    int
}

func (f *fakeWhoIs) WhoIs(_ context.Context, _ string) (*peerIdentity, error) {
	f.calls++
	return f.identity, f.err
}

type fakeAuditor struct{ events []agent.AuditEvent }

func (f *fakeAuditor) Record(e agent.AuditEvent) error { f.events = append(f.events, e); return nil }
func TestAuthorizeAlwaysResolvesWhoIs(t *testing.T) {
	lookup := &fakeWhoIs{identity: &peerIdentity{login: "Alice@Example.com", node: "Mac.tailnet.ts.net."}}
	peer, err := authorize(context.Background(), lookup, Config{}, "100.1.2.3:4")
	if err != nil || lookup.calls != 1 || peer.LoginName != "Alice@Example.com" || peer.Address != "100.1.2.3:4" {
		t.Fatalf("peer=%+v calls=%d err=%v", peer, lookup.calls, err)
	}
	lookup.err = errors.New("WhoIs unavailable")
	if _, err := authorize(context.Background(), lookup, Config{}, "100.1.2.3:4"); err == nil {
		t.Fatal("authorized after WhoIs failure")
	}
	lookup.err = nil
	if _, err := authorize(context.Background(), lookup, Config{AllowLogins: []string{"bob@example.com"}}, "100.1.2.3:4"); err == nil {
		t.Fatal("authorized unlisted login")
	}
	if _, err := authorize(context.Background(), lookup, Config{AllowNodes: []string{"mac.tailnet.ts.net"}}, "100.1.2.3:4"); err != nil {
		t.Fatal(err)
	}
}
func TestDeniedConnectionIsAudited(t *testing.T) {
	server, client := net.Pipe()
	defer client.Close()
	lookup := &fakeWhoIs{identity: &peerIdentity{login: "alice@example.com", node: "alice-mac"}}
	auditor := &fakeAuditor{}
	a := &agent.Agent{Auditor: auditor}
	handleConnection(context.Background(), lookup, Config{AllowLogins: []string{"bob@example.com"}}, a, server)
	if lookup.calls != 1 || len(auditor.events) != 1 {
		t.Fatalf("calls=%d events=%+v", lookup.calls, auditor.events)
	}
	event := auditor.events[0]
	if event.Stage != "denied" || event.Outcome != "rejected" || event.ErrorCode != protocol.CodePermissionDenied || event.Peer.LoginName != "alice@example.com" || event.Peer.Address == "" {
		t.Fatalf("event=%+v", event)
	}
}

type sessionRunner struct{}

func (sessionRunner) Capabilities(context.Context) (agent.SpectraCapabilities, error) {
	return agent.SpectraCapabilities{Schema: protocol.SchemaRef{Name: "spectra.capabilities", Version: 1}, SpectraVersion: "test"}, nil
}
func (sessionRunner) Inspect(context.Context, protocol.InspectParams) (json.RawMessage, error) {
	return json.RawMessage(`{}`), nil
}
func (sessionRunner) SnapshotCreate(context.Context, protocol.SnapshotCreateParams) (json.RawMessage, error) {
	return json.RawMessage(`{}`), nil
}
func TestAuthorizedSessionAuditsAuthenticatedPeer(t *testing.T) {
	server, client := net.Pipe()
	defer client.Close()
	lookup := &fakeWhoIs{identity: &peerIdentity{login: "alice@example.com", node: "alice-mac"}}
	auditor := &fakeAuditor{}
	a := &agent.Agent{Runner: sessionRunner{}, Auditor: auditor}
	done := make(chan struct{})
	go func() {
		handleConnection(context.Background(), lookup, Config{SessionTimeout: time.Second}, a, server)
		close(done)
	}()
	if _, err := client.Write([]byte(`{"protocol_version":"v1","request_id":"r1","operation":"health"}` + "\n")); err != nil {
		t.Fatal(err)
	}
	var response protocol.Response
	if err := json.NewDecoder(client).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if response.Error != nil {
		t.Fatal(response.Error)
	}
	client.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("session did not finish")
	}
	if len(auditor.events) != 2 {
		t.Fatalf("events=%+v", auditor.events)
	}
	for _, event := range auditor.events {
		if event.Peer.Transport != "tsnet" || event.Peer.LoginName != "alice@example.com" || event.Peer.NodeName != "alice-mac" || event.Peer.Address == "" {
			t.Fatalf("event=%+v", event)
		}
	}
}

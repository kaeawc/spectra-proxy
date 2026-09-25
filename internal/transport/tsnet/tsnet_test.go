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

func (sessionRunner) Capabilities(context.Context) (protocol.SpectraCapabilities, error) {
	return protocol.SpectraCapabilities{}, nil
}
func (sessionRunner) Inspect(context.Context, protocol.InspectParams) (json.RawMessage, error) {
	return json.RawMessage(`{}`), nil
}
func (sessionRunner) SnapshotCreate(context.Context, protocol.SnapshotCreateParams) (json.RawMessage, error) {
	return json.RawMessage(`{}`), nil
}

// blockingRunner's Inspect blocks until its context is canceled, simulating a
// long-running Spectra process so tests can observe the session deadline
// (not the idle timeout) tearing it down.
type blockingRunner struct{}

func (blockingRunner) Capabilities(context.Context) (protocol.SpectraCapabilities, error) {
	inspect := protocol.SchemaRef{Name: protocol.SchemaInspect, Version: 1}
	capabilities := protocol.SchemaRef{Name: protocol.SchemaCapabilities, Version: protocol.CapabilitiesSchemaVersion}
	return protocol.SpectraCapabilities{Schema: capabilities, SpectraVersion: "test", OS: "darwin", Arch: "arm64", Interfaces: []protocol.SpectraInterface{{Name: protocol.InterfaceInspect, Output: protocol.OutputJSON, ResultSchema: &inspect}, {Name: protocol.InterfaceCapabilities, Output: protocol.OutputJSON, ResultSchema: &capabilities}}}, nil
}
func (blockingRunner) Inspect(ctx context.Context, _ protocol.InspectParams) (json.RawMessage, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
func (blockingRunner) SnapshotCreate(context.Context, protocol.SnapshotCreateParams) (json.RawMessage, error) {
	return json.RawMessage(`{}`), nil
}

const inspectRequest = `{"protocol_version":"v1","request_id":"r1","operation":"inspect","params":{"app_paths":["/Applications/A.app"]}}` + "\n"
const healthRequest = `{"protocol_version":"v1","request_id":"r1","operation":"health"}` + "\n"

func TestIdleTimeoutAllowsSequentialRequestsUnderCumulativeSum(t *testing.T) {
	server, client := net.Pipe()
	defer client.Close()
	lookup := &fakeWhoIs{identity: &peerIdentity{login: "alice@example.com", node: "alice-mac"}}
	a := &agent.Agent{Runner: sessionRunner{}}
	cfg := Config{SessionTimeout: 3 * time.Second, IdleTimeout: 200 * time.Millisecond}
	done := make(chan struct{})
	go func() {
		handleConnection(context.Background(), lookup, cfg, a, server)
		close(done)
	}()
	// Each gap between requests is under IdleTimeout, but their sum exceeds
	// it; both requests must still succeed because the idle deadline is
	// extended on every read/write, not fixed once for the whole session.
	for i := 0; i < 2; i++ {
		if _, err := client.Write([]byte(healthRequest)); err != nil {
			t.Fatal(err)
		}
		var response protocol.Response
		if err := json.NewDecoder(client).Decode(&response); err != nil {
			t.Fatal(err)
		}
		if response.Error != nil {
			t.Fatalf("request %d: %+v", i, response.Error)
		}
		time.Sleep(120 * time.Millisecond)
	}
	client.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("session did not finish")
	}
}

func TestIdleTimeoutDropsInactivePeer(t *testing.T) {
	server, client := net.Pipe()
	defer client.Close()
	lookup := &fakeWhoIs{identity: &peerIdentity{login: "alice@example.com", node: "alice-mac"}}
	a := &agent.Agent{Runner: sessionRunner{}}
	cfg := Config{SessionTimeout: 3 * time.Second, IdleTimeout: 100 * time.Millisecond}
	start := time.Now()
	done := make(chan struct{})
	go func() {
		handleConnection(context.Background(), lookup, cfg, a, server)
		close(done)
	}()
	select {
	case <-done:
		if elapsed := time.Since(start); elapsed >= cfg.SessionTimeout {
			t.Fatalf("session ended after %s: expected the idle timeout, not the session cap, to end it", elapsed)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("idle peer was not dropped")
	}
}

func TestSessionTimeoutEndsSessionWellUnderIdleTimeout(t *testing.T) {
	server, client := net.Pipe()
	defer client.Close()
	lookup := &fakeWhoIs{identity: &peerIdentity{login: "alice@example.com", node: "alice-mac"}}
	a := &agent.Agent{Runner: blockingRunner{}}
	cfg := Config{SessionTimeout: 150 * time.Millisecond, IdleTimeout: 5 * time.Second}
	start := time.Now()
	done := make(chan struct{})
	go func() {
		handleConnection(context.Background(), lookup, cfg, a, server)
		close(done)
	}()
	if _, err := client.Write([]byte(inspectRequest)); err != nil {
		t.Fatal(err)
	}
	var response protocol.Response
	if err := json.NewDecoder(client).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if response.Error == nil || response.Error.Code != protocol.CodeTimeout {
		t.Fatalf("response = %+v, want a timeout from the session deadline", response)
	}
	select {
	case <-done:
		if elapsed := time.Since(start); elapsed >= cfg.IdleTimeout {
			t.Fatalf("session ended after %s: expected the session cap, not the idle timeout, to end it", elapsed)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("session did not end at the session cap")
	}
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

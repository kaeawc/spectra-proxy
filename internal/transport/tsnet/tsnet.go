// Package tsnet owns the embedded Tailscale transport for Spectra Remote.
// Diagnostic dispatch stays in internal/agent so this is the only package
// that needs a remote-access dependency.
package tsnet

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	protocol "github.com/kaeawc/spectra-protocol/protocol/v1"
	"github.com/kaeawc/spectra-proxy/internal/agent"
	"tailscale.com/tsnet"
)

const (
	DefaultAddr           = ":7878"
	DefaultMaxConnections = 8
	// DefaultSessionTimeout bounds the whole session: with --negotiate, the
	// controller's health round trip (a capabilities exec) plus a 30s
	// diagnostic request must both fit inside it.
	DefaultSessionTimeout = 5 * time.Minute
	// DefaultIdleTimeout bounds how long the connection may go without
	// forward progress (no bytes read or written) before it is dropped.
	DefaultIdleTimeout = 60 * time.Second
)

// Config controls one managed tailnet node.
type Config struct {
	StateDir       string
	Hostname       string
	Ephemeral      bool
	AdvertiseTags  []string
	AllowLogins    []string
	AllowNodes     []string
	MaxConnections int
	// SessionTimeout is the hard cap on total session length. It bounds
	// sessionCtx, so a running Spectra process is always eventually killed
	// even if the peer keeps the connection busy.
	SessionTimeout time.Duration
	// IdleTimeout bounds each read or write: it is how long the connection
	// may go without forward progress before being dropped. It is extended
	// on every Read and Write, up to the overall SessionTimeout, so a
	// multi-request session (e.g. --negotiate's health call followed by a
	// diagnostic request) is not cut short by one fixed session-wide
	// deadline.
	IdleTimeout time.Duration
	Logf        func(format string, args ...any)
}

// DefaultStateDir returns a role-specific private state directory so an
// operator controller and a target agent do not share node identity.
func DefaultStateDir(role string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	if role == "" {
		role = "agent"
	}
	return filepath.Join(home, ".spectra-remote", "tsnet", role), nil
}

// Serve accepts authenticated tailnet connections and routes them to a typed
// local agent. Tailnet ACLs always apply; optional allowlists add a second
// target-side identity check.
func Serve(ctx context.Context, a *agent.Agent, cfg Config, addr string) error {
	limiter, err := newConnectionLimiter(cfg.MaxConnections)
	if err != nil {
		return err
	}
	server, err := newServer(cfg)
	if err != nil {
		return err
	}
	defer server.Close()
	if addr == "" {
		addr = DefaultAddr
	}
	listener, err := server.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen on tailnet %s: %w", addr, err)
	}
	defer listener.Close()

	go func() {
		<-ctx.Done()
		_ = listener.Close()
	}()

	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("accept tailnet connection: %w", err)
		}
		if !limiter.tryAcquire() {
			logf(cfg, "spectra-remote-agent rejected tailnet peer %s: connection limit reached", conn.RemoteAddr())
			_ = conn.Close()
			continue
		}
		go func() {
			defer limiter.release()
			handleConnection(ctx, serverWhoIser{server}, cfg, a, conn)
		}()
	}
}

// Dial creates a controller-side tailnet node and connects it to target. The
// caller must close both returned values.
func Dial(ctx context.Context, cfg Config, target string) (net.Conn, ioCloser, error) {
	if strings.TrimSpace(target) == "" {
		return nil, nil, fmt.Errorf("target is required")
	}
	server, err := newServer(cfg)
	if err != nil {
		return nil, nil, err
	}
	conn, err := server.Dial(ctx, "tcp", target)
	if err != nil {
		_ = server.Close()
		return nil, nil, fmt.Errorf("dial tailnet target %s: %w", target, err)
	}
	return conn, server, nil
}

type ioCloser interface {
	Close() error
}

type connectionLimiter struct {
	slots chan struct{}
}

func newConnectionLimiter(limit int) (connectionLimiter, error) {
	if limit == 0 {
		limit = DefaultMaxConnections
	}
	if limit < 1 {
		return connectionLimiter{}, fmt.Errorf("max connections must be positive")
	}
	return connectionLimiter{slots: make(chan struct{}, limit)}, nil
}

func (l connectionLimiter) tryAcquire() bool {
	select {
	case l.slots <- struct{}{}:
		return true
	default:
		return false
	}
}

func (l connectionLimiter) release() { <-l.slots }

func newServer(cfg Config) (*tsnet.Server, error) {
	stateDir := cfg.StateDir
	if stateDir == "" {
		return nil, fmt.Errorf("tsnet state directory is required")
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, fmt.Errorf("create tsnet state directory: %w", err)
	}
	if err := os.Chmod(stateDir, 0o700); err != nil {
		return nil, fmt.Errorf("secure tsnet state directory: %w", err)
	}
	hostname := strings.TrimSpace(cfg.Hostname)
	if hostname == "" {
		return nil, fmt.Errorf("tsnet hostname is required")
	}
	return &tsnet.Server{
		Dir:           stateDir,
		Hostname:      hostname,
		Ephemeral:     cfg.Ephemeral,
		AdvertiseTags: cfg.AdvertiseTags,
		UserLogf:      cfg.Logf,
	}, nil
}

type peerIdentity struct{ login, node string }
type whoIser interface {
	WhoIs(context.Context, string) (*peerIdentity, error)
}
type serverWhoIser struct{ server *tsnet.Server }

func (w serverWhoIser) WhoIs(ctx context.Context, address string) (*peerIdentity, error) {
	client, err := w.server.LocalClient()
	if err != nil {
		return nil, fmt.Errorf("open local tailscale client: %w", err)
	}
	who, err := client.WhoIs(ctx, address)
	if err != nil {
		return nil, fmt.Errorf("resolve peer identity: %w", err)
	}
	if who == nil {
		return nil, fmt.Errorf("empty peer identity")
	}
	identity := &peerIdentity{}
	if who.UserProfile != nil {
		identity.login = who.UserProfile.LoginName
	}
	if who.Node != nil {
		identity.node = who.Node.Name
	}
	return identity, nil
}

func handleConnection(ctx context.Context, lookup whoIser, cfg Config, a *agent.Agent, conn net.Conn) {
	defer conn.Close()
	address := conn.RemoteAddr().String()
	sessionCtx, cancel := context.WithTimeout(ctx, sessionTimeout(cfg.SessionTimeout))
	defer cancel()
	sessionDeadline, _ := sessionCtx.Deadline()
	wrapped := &sessionConn{Conn: conn, idleTimeout: idleTimeout(cfg.IdleTimeout), sessionDeadline: sessionDeadline}
	if err := conn.SetDeadline(wrapped.nextDeadline()); err != nil {
		logf(cfg, "spectra-remote-agent rejected tailnet peer %s: set session deadline: %v", address, err)
		auditDenial(a, agent.Peer{Transport: "tsnet", Address: address})
		return
	}
	peer, err := authorize(sessionCtx, lookup, cfg, address)
	if err != nil {
		auditDenial(a, peer)
		logf(cfg, "spectra-remote-agent rejected tailnet peer %s: %v", address, err)
		return
	}
	if err := agent.Serve(agent.WithPeer(sessionCtx, peer), a, wrapped, wrapped); err != nil {
		logf(cfg, "spectra-remote-agent session %s stopped: %v", address, err)
	}
}

// sessionConn extends a connection's read and write deadlines on every
// operation, bounded by a fixed session deadline. This gives each
// request/response an idle timeout - a peer that stops sending or reading is
// dropped after IdleTimeout - while the session deadline still caps total
// session length. Wrapping the conn this way, rather than threading the two
// timeouts through agent.Serve, keeps the deadline policy entirely in the
// transport: agent.Serve just reads and writes an io.Reader/io.Writer, and
// the sessionCtx passed to it (bounded by the same session deadline) is what
// kills a running Spectra process if the whole session runs out its clock.
type sessionConn struct {
	net.Conn
	idleTimeout     time.Duration
	sessionDeadline time.Time
}

// nextDeadline returns the earlier of "idle timeout from now" and the fixed
// session deadline. It is used for the pre-authorization deadline and for
// reads: once the session deadline has passed, a read must fail immediately
// rather than wait out a fresh idle timeout, or a hung/slow peer would keep
// the session (and its connection-limiter slot) alive well past the cap.
func (c *sessionConn) nextDeadline() time.Time {
	next := time.Now().Add(c.idleTimeout)
	if next.After(c.sessionDeadline) {
		return c.sessionDeadline
	}
	return next
}

// writeDeadline is deliberately not capped by the session deadline. The
// response for the request that hit the session deadline (e.g. a
// context.DeadlineExceeded failure) is computed and written only after that
// deadline has already passed, so a write bounded by it would fail before a
// single byte went out and the caller would get EOF instead of the failure
// it earned. An idle timeout from now is still a hard backstop against a
// peer that has stopped reading.
func (c *sessionConn) writeDeadline() time.Time {
	return time.Now().Add(c.idleTimeout)
}

func (c *sessionConn) Read(p []byte) (int, error) {
	if err := c.Conn.SetReadDeadline(c.nextDeadline()); err != nil {
		return 0, fmt.Errorf("extend read deadline: %w", err)
	}
	return c.Conn.Read(p)
}

func (c *sessionConn) Write(p []byte) (int, error) {
	if err := c.Conn.SetWriteDeadline(c.writeDeadline()); err != nil {
		return 0, fmt.Errorf("extend write deadline: %w", err)
	}
	return c.Conn.Write(p)
}

func auditDenial(a *agent.Agent, peer agent.Peer) {
	if a == nil || a.Auditor == nil {
		return
	}
	at := time.Now()
	if a.Now != nil {
		at = a.Now()
	}
	if err := a.Auditor.Record(agent.AuditEvent{At: at.UTC(), Peer: peer, Stage: "denied", Outcome: "rejected", ErrorCode: protocol.CodePermissionDenied}); err != nil && a.Logf != nil {
		a.Logf("audit denied write failed: %v", err)
	}
}

func sessionTimeout(timeout time.Duration) time.Duration {
	if timeout <= 0 {
		return DefaultSessionTimeout
	}
	return timeout
}

func idleTimeout(timeout time.Duration) time.Duration {
	if timeout <= 0 {
		return DefaultIdleTimeout
	}
	return timeout
}

func authorize(ctx context.Context, lookup whoIser, cfg Config, remoteAddr string) (agent.Peer, error) {
	peer := agent.Peer{Transport: "tsnet", Address: remoteAddr}
	lookupCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	who, err := lookup.WhoIs(lookupCtx, remoteAddr)
	if err != nil {
		return peer, fmt.Errorf("resolve peer identity: %w", err)
	}
	if who == nil || (who.login == "" && who.node == "") {
		return peer, fmt.Errorf("empty peer identity")
	}
	peer.LoginName, peer.NodeName = who.login, who.node
	if len(cfg.AllowLogins) == 0 && len(cfg.AllowNodes) == 0 {
		return peer, nil
	}
	for _, allowed := range cfg.AllowLogins {
		if peer.LoginName != "" && normalize(peer.LoginName) == normalize(allowed) {
			return peer, nil
		}
	}
	for _, allowed := range cfg.AllowNodes {
		if peer.NodeName != "" && normalize(peer.NodeName) == normalize(allowed) {
			return peer, nil
		}
	}
	return peer, fmt.Errorf("peer is not on the target allowlist")
}

func normalize(value string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(value)), ".")
}

func logf(cfg Config, format string, args ...any) {
	if cfg.Logf != nil {
		cfg.Logf(format, args...)
	}
}

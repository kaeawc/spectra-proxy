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
	DefaultSessionTimeout = time.Minute
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
	SessionTimeout time.Duration
	Logf           func(format string, args ...any)
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
	if err := conn.SetDeadline(time.Now().Add(sessionTimeout(cfg.SessionTimeout))); err != nil {
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
	if err := agent.Serve(agent.WithPeer(sessionCtx, peer), a, conn, conn); err != nil {
		logf(cfg, "spectra-remote-agent session %s stopped: %v", address, err)
	}
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

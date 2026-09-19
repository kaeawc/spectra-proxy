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

	"github.com/kaeawc/spectra-remote/internal/agent"
	"tailscale.com/tsnet"
)

const (
	DefaultAddr           = ":7878"
	DefaultMaxConnections = 8
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
func Serve(ctx context.Context, a agent.Agent, cfg Config, addr string) error {
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
			handleConnection(ctx, server, cfg, a, conn)
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

func handleConnection(ctx context.Context, server *tsnet.Server, cfg Config, a agent.Agent, conn net.Conn) {
	defer conn.Close()
	if err := authorize(ctx, server, cfg, conn.RemoteAddr().String()); err != nil {
		logf(cfg, "spectra-remote-agent rejected tailnet peer %s: %v", conn.RemoteAddr(), err)
		return
	}
	if err := agent.Serve(ctx, a, conn, conn); err != nil {
		logf(cfg, "spectra-remote-agent session %s stopped: %v", conn.RemoteAddr(), err)
	}
}

func authorize(ctx context.Context, server *tsnet.Server, cfg Config, remoteAddr string) error {
	if len(cfg.AllowLogins) == 0 && len(cfg.AllowNodes) == 0 {
		return nil
	}
	lookupCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	client, err := server.LocalClient()
	if err != nil {
		return fmt.Errorf("open local tailscale client: %w", err)
	}
	who, err := client.WhoIs(lookupCtx, remoteAddr)
	if err != nil {
		return fmt.Errorf("resolve peer identity: %w", err)
	}
	if who == nil {
		return fmt.Errorf("empty peer identity")
	}
	login, node := "", ""
	if who.UserProfile != nil {
		login = normalize(who.UserProfile.LoginName)
	}
	if who.Node != nil {
		node = normalize(who.Node.Name)
	}
	for _, allowed := range cfg.AllowLogins {
		if login != "" && login == normalize(allowed) {
			return nil
		}
	}
	for _, allowed := range cfg.AllowNodes {
		if node != "" && node == normalize(allowed) {
			return nil
		}
	}
	return fmt.Errorf("peer is not on the target allowlist")
}

func normalize(value string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(value)), ".")
}

func logf(cfg Config, format string, args ...any) {
	if cfg.Logf != nil {
		cfg.Logf(format, args...)
	}
}

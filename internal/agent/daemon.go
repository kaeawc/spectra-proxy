package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	protocol "github.com/kaeawc/spectra-protocol/protocol/v1"
)

// Spectra daemon JSON-RPC contract, as served by `spectra daemon` on its
// local unix socket: one JSON-RPC 2.0 object per newline-terminated line.
const (
	daemonProtocolVersion = "spectra-daemon/1"
	daemonMethodStatus    = "daemon.status"
	// daemonProbeTimeout bounds the daemon.status probe so a wedged daemon
	// cannot stall health or delay the CLI fallback.
	daemonProbeTimeout = 2 * time.Second
)

// Daemon is the subset of a local Spectra daemon the agent uses for the
// read-only list operations. Method names and parameters are fixed by the
// agent; a request can never choose them.
type Daemon interface {
	Status(context.Context) (DaemonStatus, error)
	Call(ctx context.Context, method string, params any) (json.RawMessage, error)
}

// DaemonStatus is the part of the daemon.status result the agent relies on.
type DaemonStatus struct {
	Protocol string   `json:"protocol"`
	Version  string   `json:"version"`
	Methods  []string `json:"methods"`
}

// HasMethod reports whether the daemon advertised method.
func (s DaemonStatus) HasMethod(method string) bool {
	for _, m := range s.Methods {
		if m == method {
			return true
		}
	}
	return false
}

// DaemonRPCError is a JSON-RPC error object returned by the daemon.
type DaemonRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *DaemonRPCError) Error() string {
	return fmt.Sprintf("spectra daemon RPC %d: %s", e.Code, e.Message)
}

// SpectraDaemon calls a local Spectra daemon over its unix socket. Each call
// opens its own short-lived connection, so no state is shared between
// requests.
type SpectraDaemon struct {
	Socket string
}

// DefaultDaemonSocket mirrors Spectra core's default daemon socket location:
// $SPECTRA_DAEMON_DIR, else $XDG_RUNTIME_DIR/spectra on Linux, else
// ~/.spectra, each holding daemon.sock.
func DefaultDaemonSocket(env func(string) string, home func() (string, error), goos string) (string, error) {
	dir := env("SPECTRA_DAEMON_DIR")
	if dir == "" && goos == "linux" && env("XDG_RUNTIME_DIR") != "" {
		dir = filepath.Join(env("XDG_RUNTIME_DIR"), "spectra")
	}
	if dir == "" {
		h, err := home()
		if err != nil {
			return "", fmt.Errorf("resolve home directory for Spectra daemon socket: %w", err)
		}
		dir = filepath.Join(h, ".spectra")
	}
	return filepath.Join(dir, "daemon.sock"), nil
}

// Status asks the daemon for its protocol, version, and method list.
func (d SpectraDaemon) Status(ctx context.Context) (DaemonStatus, error) {
	raw, err := d.Call(ctx, daemonMethodStatus, nil)
	if err != nil {
		return DaemonStatus{}, err
	}
	var st DaemonStatus
	if err := json.Unmarshal(raw, &st); err != nil {
		return DaemonStatus{}, fmt.Errorf("decode spectra daemon status: %w", err)
	}
	return st, nil
}

// Call sends one JSON-RPC request and returns its raw result. The reply is
// bounded like local Spectra output, and ctx cancellation or deadline closes
// the exchange.
func (d SpectraDaemon) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if !filepath.IsAbs(d.Socket) {
		return nil, fmt.Errorf("spectra daemon socket path must be absolute")
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", d.Socket)
	if err != nil {
		return nil, fmt.Errorf("dial spectra daemon: %w", err)
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		if err := conn.SetDeadline(deadline); err != nil {
			return nil, fmt.Errorf("set spectra daemon deadline: %w", err)
		}
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Now()) })
	defer stop()
	const id = 1
	request, err := json.Marshal(struct {
		JSONRPC string `json:"jsonrpc"`
		ID      int    `json:"id"`
		Method  string `json:"method"`
		Params  any    `json:"params,omitempty"`
	}{"2.0", id, method, params})
	if err != nil {
		return nil, fmt.Errorf("encode spectra daemon request: %w", err)
	}
	if _, err := conn.Write(append(request, '\n')); err != nil {
		return nil, d.ioError(ctx, "write", err)
	}
	reader := bufio.NewReader(conn)
	for {
		line, err := protocol.ReadMessage(reader, maxCommandOutput)
		if errors.Is(err, protocol.ErrMessageTooLarge) {
			return nil, fmt.Errorf("spectra daemon reply exceeded %d bytes", maxCommandOutput)
		}
		if err != nil {
			return nil, d.ioError(ctx, "read", err)
		}
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var reply struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Result json.RawMessage `json:"result"`
			Error  *DaemonRPCError `json:"error"`
		}
		if err := json.Unmarshal(line, &reply); err != nil {
			return nil, fmt.Errorf("decode spectra daemon reply: %w", err)
		}
		// Skip notifications and replies to anything but this request.
		if reply.Method != "" || string(bytes.TrimSpace(reply.ID)) != "1" {
			continue
		}
		if reply.Error != nil {
			return nil, reply.Error
		}
		return reply.Result, nil
	}
}

func (d SpectraDaemon) ioError(ctx context.Context, op string, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("spectra daemon %s canceled: %w", op, ctxErr)
	}
	if _, hasDeadline := ctx.Deadline(); hasDeadline && errors.Is(err, os.ErrDeadlineExceeded) {
		return fmt.Errorf("spectra daemon %s: %w", op, context.DeadlineExceeded)
	}
	return fmt.Errorf("spectra daemon %s: %w", op, err)
}

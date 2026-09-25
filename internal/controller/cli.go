package controller

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	protocol "github.com/kaeawc/spectra-protocol/protocol/v1"
)

// TSNetFlags carries transport configuration without importing the transport package.
type TSNetFlags struct {
	Hostname  string
	StateDir  string
	Ephemeral bool
	Tags      []string
}

// CallOptions contains protocol and timeout options for one CLI call.
type CallOptions struct {
	Operation string
	Params    string
	Timeout   time.Duration
	Negotiate bool
	Rand      io.Reader
}

type stringList []string

func (l *stringList) String() string         { return strings.Join(*l, ",") }
func (l *stringList) Set(value string) error { *l = append(*l, value); return nil }

// ParseCallFlags parses call arguments and returns exit code 2 for usage errors.
func ParseCallFlags(args []string, stderr io.Writer, defaultStateDir string) (opts CallOptions, target string, tsnetFlags TSNetFlags, code int) {
	fs := flag.NewFlagSet("spectra-remote call", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {}
	fs.StringVar(&target, "target", "", "Target tailnet hostname and port")
	fs.StringVar(&opts.Operation, "operation", "", "Typed protocol operation")
	fs.StringVar(&opts.Params, "params", "", "Operation parameters as JSON")
	fs.DurationVar(&opts.Timeout, "timeout", 30*time.Second, "Connection and request timeout")
	fs.StringVar(&tsnetFlags.Hostname, "tsnet-hostname", "spectra-remote-controller", "Controller tailnet node hostname")
	fs.StringVar(&tsnetFlags.StateDir, "tsnet-state-dir", defaultStateDir, "Private tsnet state directory")
	fs.BoolVar(&tsnetFlags.Ephemeral, "tsnet-ephemeral", false, "Register an ephemeral tailnet node")
	fs.BoolVar(&opts.Negotiate, "negotiate", false, "Check protocol and operation support before calling")
	fs.Var((*stringList)(&tsnetFlags.Tags), "tsnet-tag", "Tailnet tag to advertise; may be repeated")
	if err := fs.Parse(args); err != nil {
		return usageError(stderr, err)
	}
	if strings.TrimSpace(target) == "" || strings.TrimSpace(opts.Operation) == "" {
		return usageError(stderr, fmt.Errorf("call requires --target and --operation"))
	}
	if opts.Timeout <= 0 {
		return usageError(stderr, fmt.Errorf("timeout must be positive"))
	}
	return opts, target, tsnetFlags, 0
}

func usageError(stderr io.Writer, err error) (CallOptions, string, TSNetFlags, int) {
	_, _ = fmt.Fprintln(stderr, err)
	return CallOptions{}, "", TSNetFlags{}, 2
}

// RunCall performs an already parsed CLI call and writes one-line errors to stderr.
func RunCall(ctx context.Context, rw io.ReadWriter, opts CallOptions, stdout, stderr io.Writer) int {
	var params json.RawMessage
	if opts.Params != "" {
		params = json.RawMessage(opts.Params)
		probe := protocol.Request{ProtocolVersion: protocol.Version, RequestID: "params-check", Operation: protocol.Operation(opts.Operation), Params: params}
		if err := probe.Validate(); err != nil {
			return printFailure(stderr, 2, fmt.Errorf("invalid params: %w", err))
		}
	}
	id, err := randomID(opts.Rand)
	if err != nil {
		return printFailure(stderr, 1, err)
	}
	sess := NewSession(rw)
	operation := protocol.Operation(opts.Operation)
	if opts.Negotiate {
		negotiationID, genErr := randomID(opts.Rand)
		if genErr != nil {
			return printFailure(stderr, 1, genErr)
		}
		_, err := Negotiate(ctx, sess, operation, func() string { return negotiationID })
		if err != nil {
			return printFailure(stderr, exitCode(err), err)
		}
	}
	timeoutMS := int(opts.Timeout.Milliseconds())
	if timeoutMS > protocol.MaxTimeoutMS {
		timeoutMS = protocol.MaxTimeoutMS
	}
	req := protocol.Request{ProtocolVersion: protocol.Version, RequestID: id, Operation: operation, TimeoutMS: timeoutMS, Params: params}
	resp, err := Call(ctx, sess, req)
	if err != nil {
		return printFailure(stderr, exitCode(err), err)
	}
	if _, err := Interpret(operation, resp); err != nil {
		return printFailure(stderr, exitCode(err), err)
	}
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(resp); err != nil {
		return printFailure(stderr, 1, fmt.Errorf("write response: %w", err))
	}
	return 0
}

func exitCode(err error) int {
	var remote *RemoteError
	if errors.As(err, &remote) {
		return 1
	}
	var pe *ProtocolError
	if errors.As(err, &pe) {
		return 3
	}
	if protocol.CodeOf(err) == protocol.CodeIncompatibleSpectra {
		return 3
	}
	return 1
}

func printFailure(stderr io.Writer, code int, err error) int {
	_, _ = fmt.Fprintln(stderr, err)
	return code
}

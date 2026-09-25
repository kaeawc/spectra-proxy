// Package controller implements the protocol-only client for Spectra Remote.
package controller

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"time"

	protocol "github.com/kaeawc/spectra-protocol/protocol/v1"
)

// Session owns one buffered reader so sequential calls retain bytes already read ahead.
type Session struct {
	rw     io.ReadWriter
	reader *bufio.Reader
}

// NewSession creates a protocol session for one bidirectional stream.
func NewSession(rw io.ReadWriter) *Session { return &Session{rw: rw, reader: bufio.NewReader(rw)} }

// ProtocolError indicates a malformed, oversized, or mismatched wire message.
type ProtocolError struct{ Err error }

func (e *ProtocolError) Error() string { return "protocol error: " + e.Err.Error() }
func (e *ProtocolError) Unwrap() error { return e.Err }

// RemoteError is a validated error returned by the remote peer.
type RemoteError struct {
	Code    protocol.ErrorCode
	Message string
}

func (e *RemoteError) Error() string { return fmt.Sprintf("remote %s: %s", e.Code, e.Message) }

// Call validates and exchanges one request on an existing session.
func Call(ctx context.Context, sess *Session, req protocol.Request) (protocol.Response, error) {
	if err := req.Validate(); err != nil {
		return protocol.Response{}, fmt.Errorf("validate request: %w", err)
	}
	stop, err := sess.begin(ctx)
	if err != nil {
		return protocol.Response{}, err
	}
	defer stop()
	if err := protocol.WriteMessage(sess.rw, req, protocol.MaxRequestBytes); err != nil {
		if ctxErr := contextFailure(ctx); ctxErr != nil {
			return protocol.Response{}, ctxErr
		}
		if errors.Is(err, protocol.ErrMessageTooLarge) {
			return protocol.Response{}, &ProtocolError{Err: err}
		}
		return protocol.Response{}, fmt.Errorf("write request: %w", err)
	}
	line, err := protocol.ReadMessage(sess.reader, protocol.MaxResponseBytes)
	if err != nil {
		if ctxErr := contextFailure(ctx); ctxErr != nil {
			return protocol.Response{}, ctxErr
		}
		if errors.Is(err, protocol.ErrMessageTooLarge) {
			return protocol.Response{}, &ProtocolError{Err: err}
		}
		return protocol.Response{}, fmt.Errorf("read response: %w", err)
	}
	resp, err := protocol.DecodeResponse(line)
	if err != nil {
		return protocol.Response{}, &ProtocolError{Err: err}
	}
	if err := resp.ValidateFor(req); err != nil {
		return protocol.Response{}, &ProtocolError{Err: err}
	}
	return resp, nil
}

func contextFailure(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if deadline, ok := ctx.Deadline(); ok && !time.Now().Before(deadline) {
		return context.DeadlineExceeded
	}
	return nil
}

type deadlineConn interface{ SetDeadline(time.Time) error }

func (s *Session) begin(ctx context.Context) (func(), error) {
	c, supportsDeadline := s.rw.(deadlineConn)
	if supportsDeadline {
		if deadline, has := ctx.Deadline(); has {
			if err := c.SetDeadline(deadline); err != nil {
				return nil, fmt.Errorf("set connection deadline: %w", err)
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	stop := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		select {
		case <-ctx.Done():
			if supportsDeadline {
				_ = c.SetDeadline(time.Now().Add(-time.Second))
			}
		case <-stop:
		}
	}()
	return func() { close(stop); <-finished }, nil
}

// Outcome contains a raw response and any operation-specific decoded payload.
type Outcome struct {
	Response   protocol.Response
	Health     *protocol.HealthResult
	Diagnostic *protocol.DiagnosticResult
}

// Interpret turns a validated response into a typed outcome for known operations.
func Interpret(op protocol.Operation, resp protocol.Response) (Outcome, error) {
	out := Outcome{Response: resp}
	if resp.Error != nil {
		return out, &RemoteError{Code: resp.Error.Code, Message: resp.Error.Message}
	}
	switch op {
	case protocol.OperationHealth:
		health, err := protocol.DecodeHealth(resp)
		if err != nil {
			return out, &protocol.CodedError{Code: protocol.CodeOf(err), Err: fmt.Errorf("decode health: %w", err)}
		}
		out.Health = &health
	case protocol.OperationInspect, protocol.OperationSnapshotCreate:
		diagnostic, err := protocol.DecodeResult(resp, op)
		if err != nil {
			return out, err
		}
		out.Diagnostic = &diagnostic
	}
	return out, nil
}

// Negotiate requests health on the same session and checks support for op.
func Negotiate(ctx context.Context, sess *Session, op protocol.Operation, newID func() string) (protocol.CapabilityManifest, error) {
	req := protocol.Request{ProtocolVersion: protocol.Version, RequestID: newID(), Operation: protocol.OperationHealth}
	resp, err := Call(ctx, sess, req)
	if err != nil {
		return protocol.CapabilityManifest{}, err
	}
	out, err := Interpret(protocol.OperationHealth, resp)
	if err != nil {
		return protocol.CapabilityManifest{}, err
	}
	manifest := out.Health.Capabilities
	if _, err := protocol.Negotiate(manifest, op); err != nil {
		return protocol.CapabilityManifest{}, err
	}
	return manifest, nil
}

func randomID(r io.Reader) (string, error) {
	if r == nil {
		r = rand.Reader
	}
	b := make([]byte, 8)
	if _, err := io.ReadFull(r, b); err != nil {
		return "", fmt.Errorf("generate request id: %w", err)
	}
	return "cli-" + hex.EncodeToString(b), nil
}

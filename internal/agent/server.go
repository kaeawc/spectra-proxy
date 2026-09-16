package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"

	protocol "github.com/kaeawc/spectra-protocol/protocol/v1"
)

// Serve processes newline-delimited protocol requests over an already
// authenticated stream. It does not create listeners or authenticate peers.
func Serve(ctx context.Context, a Agent, input io.Reader, output io.Writer) error {
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	enc := json.NewEncoder(output)
	for scanner.Scan() {
		var req protocol.Request
		if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
			response := protocol.Response{
				ProtocolVersion: protocol.Version,
				Error:           &protocol.Error{Code: "invalid_request", Message: "invalid JSON request"},
			}
			if err := enc.Encode(response); err != nil {
				return fmt.Errorf("write invalid-request response: %w", err)
			}
			continue
		}
		if err := enc.Encode(a.Handle(ctx, req)); err != nil {
			return fmt.Errorf("write response: %w", err)
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read request: %w", err)
	}
	return nil
}

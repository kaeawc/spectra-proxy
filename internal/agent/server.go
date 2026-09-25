package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	protocol "github.com/kaeawc/spectra-protocol/protocol/v1"
)

// Serve processes bounded NDJSON requests over an authenticated stream.
func Serve(ctx context.Context, a *Agent, input io.Reader, output io.Writer) error {
	reader := bufio.NewReader(input)
	for {
		line, err := protocol.ReadMessage(reader, protocol.MaxRequestBytes)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if errors.Is(err, protocol.ErrMessageTooLarge) {
			return writeResponse(output, failure("", protocol.CodeMessageTooLarge, err))
		}
		if err != nil {
			return fmt.Errorf("read request: %w", err)
		}
		var req protocol.Request
		var resp protocol.Response
		if err := json.Unmarshal(line, &req); err != nil {
			resp = a.reject(ctx, protocol.Request{}, protocol.CodeInvalidRequest, fmt.Errorf("decode request: %w", err))
		} else {
			resp = a.Handle(ctx, req)
		}
		if err := writeResponse(output, resp); err != nil {
			return fmt.Errorf("write response: %w", err)
		}
	}
}

func writeResponse(output io.Writer, resp protocol.Response) error {
	err := protocol.WriteMessage(output, resp, protocol.MaxResponseBytes)
	if errors.Is(err, protocol.ErrMessageTooLarge) {
		return protocol.WriteMessage(output, failure(resp.RequestID, protocol.CodeMessageTooLarge, err), protocol.MaxResponseBytes)
	}
	return err
}

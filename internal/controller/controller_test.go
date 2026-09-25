package controller

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	protocol "github.com/kaeawc/spectra-protocol/protocol/v1"
)

func testRequest(op protocol.Operation) protocol.Request {
	return protocol.Request{ProtocolVersion: protocol.Version, RequestID: "req-1", Operation: op}
}

func exchange(t *testing.T, raw string, req protocol.Request) error {
	t.Helper()
	client, peer := net.Pipe()
	defer client.Close()
	defer peer.Close()
	go func() {
		defer peer.Close()
		br := bufio.NewReader(peer)
		_, _ = br.ReadBytes('\n')
		_, _ = io.WriteString(peer, raw)
	}()
	_, err := Call(context.Background(), NewSession(client), req)
	return err
}

func TestCallHealthRoundTrip(t *testing.T) {
	manifest := protocol.CapabilityManifest{ProtocolVersions: []string{"v1"}, Operations: []protocol.OperationCapability{{Name: protocol.OperationHealth}}, Limits: protocol.Limits{MaxRequestBytes: protocol.MaxRequestBytes, MaxResponseBytes: protocol.MaxResponseBytes, MaxTimeoutMS: protocol.MaxTimeoutMS}}
	result, _ := json.Marshal(protocol.HealthResult{Capabilities: manifest})
	testRoundTrip(t, protocol.OperationHealth, result)
}

func TestCallInspectRoundTrip(t *testing.T) {
	result := []byte(`{"schema":{"name":"spectra.inspect","version":1},"spectra_version":"1","data":{"checks":[]}}`)
	testRoundTrip(t, protocol.OperationInspect, result)
}

func testRoundTrip(t *testing.T, op protocol.Operation, result []byte) {
	t.Helper()
	req := testRequest(op)
	respLine, _ := json.Marshal(protocol.Response{ProtocolVersion: protocol.Version, RequestID: req.RequestID, Result: result})
	if err := exchange(t, string(respLine)+"\n", req); err != nil {
		t.Fatal(err)
	}
}

func TestCallRejectsMismatchedIDAndVersion(t *testing.T) {
	for _, tc := range []struct{ name, version, id string }{{"id", "v1", "other"}, {"version", "v2", "req-1"}} {
		t.Run(tc.name, func(t *testing.T) {
			raw := fmt.Sprintf(`{"protocol_version":%q,"request_id":%q,"result":{}}`+"\n", tc.version, tc.id)
			var pe *ProtocolError
			if err := exchange(t, raw, testRequest(protocol.OperationInspect)); !errors.As(err, &pe) {
				t.Fatalf("error = %v, want ProtocolError", err)
			}
		})
	}
}

func TestCallRejectsInvalidJSON(t *testing.T) {
	var pe *ProtocolError
	if err := exchange(t, "{nope}\n", testRequest(protocol.OperationInspect)); !errors.As(err, &pe) {
		t.Fatalf("error = %v, want ProtocolError", err)
	}
}

func TestCallRejectsOversizedFrame(t *testing.T) {
	client, peer := net.Pipe()
	defer client.Close()
	defer peer.Close()
	go func() {
		defer peer.Close()
		_, _ = bufio.NewReader(peer).ReadBytes('\n')
		chunk := strings.Repeat("x", 32*1024)
		_, _ = io.WriteString(peer, `{"padding":"`)
		for i := 0; i < protocol.MaxResponseBytes/(32*1024)+2; i++ {
			_, _ = io.WriteString(peer, chunk)
		}
	}()
	_, err := Call(context.Background(), NewSession(client), testRequest(protocol.OperationInspect))
	var pe *ProtocolError
	if !errors.As(err, &pe) || !errors.Is(err, protocol.ErrMessageTooLarge) {
		t.Fatalf("error = %v, want ProtocolError wrapping ErrMessageTooLarge", err)
	}
}

func TestCallContextDeadlineUnblocksRead(t *testing.T) {
	client, peer := net.Pipe()
	defer client.Close()
	defer peer.Close()
	go func() {
		defer peer.Close()
		_, _ = bufio.NewReader(peer).ReadBytes('\n')
		_, _ = io.Copy(io.Discard, peer)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := Call(ctx, NewSession(client), testRequest(protocol.OperationInspect))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want deadline exceeded", err)
	}
	if time.Since(start) > time.Second {
		t.Fatalf("Call took %s", time.Since(start))
	}
}

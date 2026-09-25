package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	protocol "github.com/kaeawc/spectra-protocol/protocol/v1"
)

func TestServeReturnsOneResponsePerRequest(t *testing.T) {
	input := strings.NewReader("{not json}\n{\"protocol_version\":\"v1\",\"request_id\":\"request-1\",\"operation\":\"health\"}\n")
	var output bytes.Buffer
	if err := Serve(context.Background(), &Agent{Runner: fakeRunner{caps: fixtureCaps(t)}}, input, &output); err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(&output)
	var first, second protocol.Response
	if err := dec.Decode(&first); err != nil {
		t.Fatal(err)
	}
	if err := dec.Decode(&second); err != nil {
		t.Fatal(err)
	}
	if first.Error == nil || first.Error.Code != protocol.CodeInvalidRequest || second.Error != nil || second.RequestID != "request-1" {
		t.Fatalf("responses=%+v %+v", first, second)
	}
}
func TestServeOversizedRequestStops(t *testing.T) {
	input := strings.NewReader(strings.Repeat("x", protocol.MaxRequestBytes+1) + "\n{\"protocol_version\":\"v1\",\"request_id\":\"later\",\"operation\":\"health\"}\n")
	var output bytes.Buffer
	if err := Serve(context.Background(), &Agent{Runner: fakeRunner{caps: fixtureCaps(t)}}, input, &output); err != nil {
		t.Fatal(err)
	}
	if strings.Count(output.String(), "\n") != 1 {
		t.Fatalf("responses=%q", output.String())
	}
	var resp protocol.Response
	if err := json.Unmarshal(bytes.TrimSpace(output.Bytes()), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.RequestID != "" || resp.Error == nil || resp.Error.Code != protocol.CodeMessageTooLarge {
		t.Fatalf("response=%+v", resp)
	}
}

func TestServeOversizedResponsePreservesRequestID(t *testing.T) {
	huge := json.RawMessage(`"` + strings.Repeat("x", protocol.MaxResponseBytes) + `"`)
	a := &Agent{Runner: fakeRunner{caps: fixtureCaps(t), inspect: func(context.Context, protocol.InspectParams) (json.RawMessage, error) { return huge, nil }}}
	input := strings.NewReader(`{"protocol_version":"v1","request_id":"large-result","operation":"inspect","params":{"app_paths":["/Applications/A.app"]}}` + "\n")
	var output bytes.Buffer
	if err := Serve(context.Background(), a, input, &output); err != nil {
		t.Fatal(err)
	}
	var resp protocol.Response
	if err := json.Unmarshal(bytes.TrimSpace(output.Bytes()), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.RequestID != "large-result" || resp.Error == nil || resp.Error.Code != protocol.CodeMessageTooLarge || strings.Count(output.String(), "\n") != 1 {
		t.Fatalf("response=%+v", resp)
	}
}

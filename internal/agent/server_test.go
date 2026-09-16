package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	protocol "github.com/kaeawc/spectra-protocol/protocol/v1"
)

func TestServeReturnsOneResponsePerRequest(t *testing.T) {
	input := bytes.NewBufferString("{not json}\n{\"protocol_version\":\"v1\",\"request_id\":\"request-1\",\"operation\":\"health\"}\n")
	var output bytes.Buffer
	if err := Serve(context.Background(), Agent{Runner: fakeRunner{version: "1.2.3"}}, input, &output); err != nil {
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
	if first.Error == nil || first.Error.Code != "invalid_request" {
		t.Fatalf("first response = %+v", first)
	}
	if second.Error != nil || second.RequestID != "request-1" {
		t.Fatalf("second response = %+v", second)
	}
}

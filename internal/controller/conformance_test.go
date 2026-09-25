package controller

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"testing"

	protocol "github.com/kaeawc/spectra-protocol/protocol/v1"
	"github.com/kaeawc/spectra-protocol/protocol/v1/conformance"
)

func TestProtocolConformance(t *testing.T) {
	cases, err := conformance.Cases()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range cases {
		if tc.Kind != "response" && tc.Kind != "result" && tc.Kind != "manifest" {
			continue
		}
		tc := tc
		t.Run(tc.Kind+"/"+tc.Name, func(t *testing.T) {
			switch tc.Kind {
			case "response":
				testResponseCase(t, tc)
			case "result":
				testResultCase(t, tc)
			case "manifest":
				testManifestCase(t, tc)
			default:
				t.Fatalf("unexpected fixture kind %q", tc.Kind)
			}
		})
	}
}

func testResponseCase(t *testing.T, tc conformance.Case) {
	t.Helper()
	var req protocol.Request
	if err := json.Unmarshal(tc.Request, &req); err != nil {
		t.Fatal(err)
	}
	client, peer := net.Pipe()
	defer client.Close()
	defer peer.Close()
	peerErr := make(chan error, 1)
	go func() {
		defer peer.Close()
		if _, err := bufio.NewReaderSize(peer, 1).ReadBytes('\n'); err != nil {
			peerErr <- err
			return
		}
		var compact bytes.Buffer
		if err := json.Compact(&compact, tc.Input); err != nil {
			peerErr <- err
			return
		}
		_, err := io.WriteString(peer, compact.String()+"\n")
		peerErr <- err
	}()
	_, err := Call(context.Background(), NewSession(client), req)
	if pErr := <-peerErr; pErr != nil {
		t.Fatalf("fake peer: %v", pErr)
	}
	if (err == nil) != tc.Valid {
		t.Fatalf("Call error=%v valid=%t", err, tc.Valid)
	}
}

func testResultCase(t *testing.T, tc conformance.Case) {
	t.Helper()
	var resp protocol.Response
	if err := json.Unmarshal(tc.Input, &resp); err != nil {
		t.Fatal(err)
	}
	_, err := Interpret(tc.Operation, resp)
	want := tc.Valid
	if tc.Operation != protocol.OperationInspect && tc.Operation != protocol.OperationSnapshotCreate {
		// Interpret deliberately preserves unrecognized operations as raw responses.
		want = true
	}
	if (err == nil) != want {
		t.Fatalf("Interpret error=%v valid=%t code=%s", err, tc.Valid, protocol.CodeOf(err))
	}
}

func testManifestCase(t *testing.T, tc conformance.Case) {
	t.Helper()
	result := struct {
		Capabilities json.RawMessage `json:"capabilities"`
	}{Capabilities: tc.Input}
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	resp := protocol.Response{ProtocolVersion: protocol.Version, RequestID: "req-1", Result: raw}
	out, err := Interpret(protocol.OperationHealth, resp)
	if err != nil {
		if tc.Valid {
			t.Fatal(err)
		}
		return
	}
	_, err = protocol.Negotiate(out.Health.Capabilities, protocol.OperationInspect)
	if (err == nil) != tc.Valid {
		t.Fatalf("Negotiate error=%v valid=%t", err, tc.Valid)
	}
}

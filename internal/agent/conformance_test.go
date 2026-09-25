package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"

	protocol "github.com/kaeawc/spectra-protocol/protocol/v1"
	"github.com/kaeawc/spectra-protocol/protocol/v1/conformance"
)

func TestSpectraCapabilitiesConformance(t *testing.T) {
	cases, err := conformance.CasesOf("spectra_capabilities")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range cases {
		t.Run(tc.Name, func(t *testing.T) {
			caps, err := protocol.DecodeSpectraCapabilities(tc.Input)
			if !tc.Valid && err != nil {
				var coded *protocol.CodedError
				if !errors.As(err, &coded) || coded.Code != tc.ErrorCode {
					t.Fatalf("decode error = %v, want %s", err, tc.ErrorCode)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tc.Operation == "" || tc.Operation == protocol.OperationHealth {
				return
			}
			a := &Agent{Runner: fakeRunner{caps: caps}, Policy: Policy{AllowSnapshot: true}}
			params := json.RawMessage(`{"app_paths":["/Applications/Test.app"]}`)
			if tc.Operation == protocol.OperationSnapshotCreate {
				params = json.RawMessage(`{}`)
			}
			response := a.Handle(context.Background(), protocol.Request{ProtocolVersion: protocol.Version, RequestID: "conformance", Operation: tc.Operation, Params: params})
			if tc.Valid && response.Error != nil {
				t.Fatalf("valid case rejected: %+v", response.Error)
			}
			if !tc.Valid && (response.Error == nil || response.Error.Code != tc.ErrorCode) {
				t.Fatalf("error = %+v, want %s", response.Error, tc.ErrorCode)
			}
		})
	}
}

func TestRequestConformanceThroughServe(t *testing.T) {
	cases, err := conformance.CasesOf("request")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range cases {
		t.Run(tc.Name, func(t *testing.T) {
			var output bytes.Buffer
			var compact bytes.Buffer
			if err := json.Compact(&compact, tc.Input); err != nil {
				t.Fatal(err)
			}
			input := append(compact.Bytes(), '\n')
			a := &Agent{Runner: fakeRunner{caps: fixtureCaps(t)}, Policy: Policy{AllowSnapshot: true, AllowSnapshotApps: true}}
			if err := Serve(context.Background(), a, bytes.NewReader(input), &output); err != nil {
				t.Fatal(err)
			}
			var resp protocol.Response
			if err := json.Unmarshal(bytes.TrimSpace(output.Bytes()), &resp); err != nil {
				t.Fatal(err)
			}
			if err := resp.Validate(); err != nil {
				t.Fatalf("invalid response: %v: %s", err, output.String())
			}
			var req protocol.Request
			if err := json.Unmarshal(tc.Input, &req); err != nil {
				t.Fatal(err)
			}
			if resp.RequestID != req.RequestID {
				t.Fatalf("request_id=%q want %q", resp.RequestID, req.RequestID)
			}
			if tc.ErrorCode != "" {
				if resp.Error == nil || resp.Error.Code != tc.ErrorCode {
					t.Fatalf("error=%+v want %s", resp.Error, tc.ErrorCode)
				}
			}
		})
	}
}

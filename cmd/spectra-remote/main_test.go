package main

import (
	"bytes"
	"testing"

	"github.com/kaeawc/spectra-proxy/internal/controller"
)

func TestParseCallFlagsRejectsMissingTarget(t *testing.T) {
	var stderr bytes.Buffer
	_, _, _, code := controller.ParseCallFlags([]string{"--operation", "health"}, &stderr, "/tmp/state")
	if code != 2 {
		t.Fatalf("ParseCallFlags() code = %d, want 2", code)
	}
}

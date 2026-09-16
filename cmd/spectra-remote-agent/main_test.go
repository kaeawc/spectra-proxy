package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRunRequiresAbsoluteSpectraPath(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"serve-stdio", "--spectra", "spectra"}, strings.NewReader(""), &stdout, &stderr)
	if code != 2 || !strings.Contains(stderr.String(), "absolute") {
		t.Fatalf("run() = %d, stderr = %q", code, stderr.String())
	}
}

func TestRunVersion(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"version"}, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("run() = %d, stderr = %q", code, stderr.String())
	}
	if strings.TrimSpace(stdout.String()) != version {
		t.Fatalf("stdout = %q, version = %q", stdout.String(), version)
	}
}

package provision

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func TestCLIUsageAndStatus(t *testing.T) {
	var out, errout bytes.Buffer
	for _, args := range [][]string{{}, {"nonesuch"}, {"install"}, {"install", "--version", "invalid"}, {"status", "--version", "v1.0.0"}, {"status", "--bogus"}} {
		out.Reset()
		errout.Reset()
		if code := Run(context.Background(), args, &out, &errout); code != 2 {
			t.Fatalf("%v code %d: %s", args, code, errout.String())
		}
	}
	root := filepath.Join(t.TempDir(), "spectra")
	out.Reset()
	errout.Reset()
	if code := Run(context.Background(), []string{"status", "--root", root, "--json"}, &out, &errout); code != 0 {
		t.Fatalf("status code %d: %s", code, errout.String())
	}
	var report StatusReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.Current != "" || report.BinaryPath != BinaryPath(root) {
		t.Fatalf("status %+v", report)
	}
	out.Reset()
	errout.Reset()
	if code := Run(context.Background(), []string{"install", "--root", root, "--version", "v1.0.0"}, &out, &errout); code != 1 || !strings.Contains(errout.String(), "no trusted Spectra release keys") {
		t.Fatalf("operational code %d: %s", code, errout.String())
	}
}

package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	protocol "github.com/kaeawc/spectra-protocol/protocol/v1"
)

func TestJSONLAuditorWritesPrivateMetadataOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit", "agent.jsonl")
	auditor, err := NewJSONLAuditor(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := auditor.Record(AuditEvent{At: time.Date(2026, 9, 19, 0, 0, 0, 0, time.UTC), RequestID: "request-1", Operation: protocol.OperationInspect, Stage: "completed", Outcome: "succeeded"}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"request-1", "inspect", "completed", "succeeded"} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("audit log does not include %q: %s", want, data)
		}
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("audit log mode = %o, want 600", got)
	}
}

func TestNewJSONLAuditorRejectsRelativePath(t *testing.T) {
	if _, err := NewJSONLAuditor("agent.jsonl"); err == nil {
		t.Fatal("NewJSONLAuditor accepted a relative path")
	}
}

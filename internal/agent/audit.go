package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	protocol "github.com/kaeawc/spectra-protocol/protocol/v1"
)

// AuditEvent intentionally records control-plane metadata only. Parameters
// and diagnostic results are omitted because they can include sensitive host
// information.
type AuditEvent struct {
	At        time.Time          `json:"at"`
	RequestID string             `json:"request_id"`
	Operation protocol.Operation `json:"operation"`
	Stage     string             `json:"stage"`
	Outcome   string             `json:"outcome"`
	ErrorCode string             `json:"error_code,omitempty"`
}

// Auditor persists target-agent audit metadata.
type Auditor interface {
	Record(AuditEvent) error
}

// JSONLAuditor appends durable, owner-private JSONL events.
type JSONLAuditor struct {
	path string
	mu   sync.Mutex
}

// NewJSONLAuditor creates an auditor for an absolute local path.
func NewJSONLAuditor(path string) (*JSONLAuditor, error) {
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("audit log path must be absolute")
	}
	return &JSONLAuditor{path: filepath.Clean(path)}, nil
}

// Record writes one event and synchronizes it before returning.
func (a *JSONLAuditor) Record(event AuditEvent) error {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(a.path), 0o700); err != nil {
		return fmt.Errorf("create audit log directory: %w", err)
	}
	f, err := os.OpenFile(a.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open audit log: %w", err)
	}
	defer f.Close()
	if err := f.Chmod(0o600); err != nil {
		return fmt.Errorf("secure audit log: %w", err)
	}
	line, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("encode audit event: %w", err)
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("write audit log: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync audit log: %w", err)
	}
	return nil
}

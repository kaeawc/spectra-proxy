package provision

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestUninstallRefusesUnrelatedRoots(t *testing.T) {
	for _, content := range []string{"", `{"schema":"unrelated"}`} {
		root := t.TempDir()
		marker := filepath.Join(root, "marker")
		if err := os.WriteFile(marker, []byte("keep"), 0o600); err != nil {
			t.Fatal(err)
		}
		if content != "" {
			if err := os.WriteFile(filepath.Join(root, "state.json"), []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if err := Uninstall(context.Background(), Options{Config: Config{Root: root}}); err == nil {
			t.Fatal("unrelated root removed")
		}
		if _, err := os.Stat(marker); err != nil {
			t.Fatalf("marker removed: %v", err)
		}
		if _, err := os.Stat(filepath.Join(root, "staging")); !os.IsNotExist(err) {
			t.Fatalf("staging unexpectedly touched: %v", err)
		}
	}
}

func TestPruneKeepsCurrentPreviousAndNewestExtras(t *testing.T) {
	root := t.TempDir()
	s := emptyState()
	s.Current = "v4.0.0"
	s.Previous = "v1.0.0"
	for i, v := range []string{"v1.0.0", "v2.0.0", "v3.0.0", "v4.0.0"} {
		if err := os.MkdirAll(filepath.Join(root, "versions", v), 0o700); err != nil {
			t.Fatal(err)
		}
		s.Versions[v] = VersionInfo{InstalledAt: time.Unix(int64(i), 0)}
	}
	removed := planPrune(&s, 3)
	if err := removePruned(root, removed); err != nil {
		t.Fatal(err)
	}
	if len(s.Versions) != 3 || s.Versions["v3.0.0"].InstalledAt.IsZero() {
		t.Fatalf("pruned versions %+v", s.Versions)
	}
	if _, err := os.Stat(filepath.Join(root, "versions", "v2.0.0")); !os.IsNotExist(err) {
		t.Fatalf("old extra remained: %v", err)
	}
	removed = planPrune(&s, 1)
	if err := removePruned(root, removed); err != nil {
		t.Fatal(err)
	}
	if s.Previous != "" || len(s.Versions) != 1 {
		t.Fatalf("keep one %+v", s)
	}
}

func TestStateSchemaAndFields(t *testing.T) {
	root := t.TempDir()
	s := emptyState()
	s.Current = "v1.0.0"
	s.Versions[s.Current] = VersionInfo{SHA256: "abc", InstalledAt: time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC)}
	if err := writeState(root, s); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["schema"] != stateSchema || decoded["current"] != "v1.0.0" {
		t.Fatalf("state %s", data)
	}
}

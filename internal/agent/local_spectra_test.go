package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	protocol "github.com/kaeawc/spectra-protocol/protocol/v1"
)

func TestValidateAppPath(t *testing.T) {
	s := LocalSpectra{AllowedAppRoots: []string{"/Applications", "/Users/alice/Applications"}}
	for _, path := range []string{"/Applications/Slack.app", "/Users/alice/Applications/Test.app"} {
		if err := s.validateAppPath(path); err != nil {
			t.Fatalf("validateAppPath(%q): %v", path, err)
		}
	}
	for _, path := range []string{"relative.app", "/tmp/Test.app", "/Applications/Slack"} {
		if err := s.validateAppPath(path); err == nil {
			t.Fatalf("validateAppPath(%q) succeeded", path)
		}
	}
}

func TestSnapshotCreateMapsFalseIncludeAppsToNoApps(t *testing.T) {
	path := makeFakeSpectra(t, `
if [ "$1" = "snapshot" ] && [ "$2" = "--json" ] && [ "$3" = "--no-apps" ]; then
  printf '{"id":"snapshot-1"}'
  exit 0
fi
printf 'unexpected args: %s %s %s' "$1" "$2" "$3" >&2
exit 1
`)
	s := LocalSpectra{Path: path}
	if _, err := s.SnapshotCreate(context.Background(), protocol.SnapshotCreateParams{}); err != nil {
		t.Fatalf("SnapshotCreate(): %v", err)
	}
}

func makeFakeSpectra(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "spectra")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

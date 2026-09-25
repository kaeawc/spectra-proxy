package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	protocol "github.com/kaeawc/spectra-protocol/protocol/v1"
)

func TestValidateAppPathAndSymlinks(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "allowed")
	outside := filepath.Join(base, "outside")
	for _, dir := range []string{root, outside, filepath.Join(root, "Inside.app"), filepath.Join(outside, "Escape.app")} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(root, "Link.app")
	if err := os.Symlink(filepath.Join(outside, "Escape.app"), link); err != nil {
		t.Fatal(err)
	}
	s := LocalSpectra{AllowedAppRoots: []string{root}}
	if _, err := s.validateAppPath(link); err == nil {
		t.Fatal("accepted symlink escape")
	}
	alias := filepath.Join(base, "alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	s.AllowedAppRoots = []string{alias}
	got, err := s.validateAppPath(filepath.Join(alias, "Inside.app"))
	if err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(filepath.Join(root, "Inside.app"))
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("resolved=%q", got)
	}
	for _, bad := range []string{"relative.app", filepath.Join(root, "Absent.app"), filepath.Join(outside, "Escape.app")} {
		if _, err := s.validateAppPath(bad); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
}
func TestInspectUsesResolvedPath(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real")
	if err := os.MkdirAll(filepath.Join(real, "A.app"), 0700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(base, "alias")
	if err := os.Symlink(real, alias); err != nil {
		t.Fatal(err)
	}
	path := makeFakeSpectra(t, `printf '{"path":"%s"}' "$2"`)
	s := LocalSpectra{Path: path, AllowedAppRoots: []string{alias}}
	result, err := s.Inspect(context.Background(), protocol.InspectParams{AppPaths: []string{filepath.Join(alias, "A.app")}})
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]string
	if err := json.Unmarshal(result, &decoded); err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(filepath.Join(real, "A.app"))
	if err != nil {
		t.Fatal(err)
	}
	if decoded["path"] != want {
		t.Fatalf("result=%s", result)
	}
}
func TestLocalSpectraCapabilities(t *testing.T) {
	data, err := os.ReadFile("testdata/core-capabilities.json")
	if err != nil {
		t.Fatal(err)
	}
	fixture := filepath.Join(t.TempDir(), "caps.json")
	if err := os.WriteFile(fixture, data, 0600); err != nil {
		t.Fatal(err)
	}
	path := makeFakeSpectra(t, fmt.Sprintf(`if [ "$1" = "capabilities" ] && [ "$2" = "--json" ]; then cat %s; else exit 1; fi`, strconv.Quote(fixture)))
	s := LocalSpectra{Path: path}
	caps, err := s.Capabilities(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if caps.Schema.Name != "spectra.capabilities" || len(caps.Interfaces) != 4 {
		t.Fatalf("caps=%+v", caps)
	}
	bad := []byte(`{"schema":{"name":"other","version":1}}`)
	if err := os.WriteFile(fixture, bad, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Capabilities(context.Background()); err == nil {
		t.Fatal("accepted incompatible capabilities schema")
	}
}
func TestSubprocessCancellationKillsProcessGroup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process groups require unix")
	}
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	path := makeFakeSpectra(t, `sleep 30 & echo $! > "$1"; wait`)
	s := LocalSpectra{Path: path}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := s.run(ctx, pidFile); done <- err }()
	var pid string
	until := time.Now().Add(3 * time.Second)
	for time.Now().Before(until) {
		data, err := os.ReadFile(pidFile)
		if err == nil && len(strings.TrimSpace(string(data))) > 0 {
			pid = strings.TrimSpace(string(data))
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if pid == "" {
		t.Fatal("child pid was not recorded")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("run unexpectedly succeeded")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not return within five seconds")
	}
	// On macOS a reaped process disappears; a briefly orphaned zombie has stopped running.
	until = time.Now().Add(3 * time.Second)
	for time.Now().Before(until) {
		out, err := exec.Command("ps", "-o", "stat=", "-p", pid).CombinedOutput()
		if err != nil || strings.HasPrefix(strings.TrimSpace(string(out)), "Z") {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("sleep child %s is still running", pid)
}
func TestSnapshotCreateMapsFalseIncludeAppsToNoApps(t *testing.T) {
	path := makeFakeSpectra(t, `if [ "$1" = "snapshot" ] && [ "$2" = "--json" ] && [ "$3" = "--no-apps" ]; then printf '{"id":"snapshot-1"}'; exit 0; fi; exit 1`)
	s := LocalSpectra{Path: path}
	if _, err := s.SnapshotCreate(context.Background(), protocol.SnapshotCreateParams{}); err != nil {
		t.Fatal(err)
	}
}
func makeFakeSpectra(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "spectra")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

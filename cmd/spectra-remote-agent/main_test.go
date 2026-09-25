package main

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kaeawc/spectra-proxy/internal/agentinstall"
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

func TestRunInstallRejectsNonPositiveMaxConnections(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{
		"install", "--no-load",
		"--spectra", "/opt/spectra/bin/spectra",
		"--max-connections", "0",
	}, strings.NewReader(""), &stdout, &stderr)
	if code != 2 || !strings.Contains(stderr.String(), "max-connections must be positive") {
		t.Fatalf("run() = %d, stderr = %q", code, stderr.String())
	}
}

func TestRunInstallWritesOnlyExplicitLaunchAgentConfiguration(t *testing.T) {
	oldDeps, oldExecutable := installDeps, executablePath
	t.Cleanup(func() {
		installDeps = oldDeps
		executablePath = oldExecutable
	})
	var wrote []byte
	launchctlCalls := 0
	installDeps = func() agentinstall.Deps {
		return agentinstall.Deps{
			HomeDir:  func() (string, error) { return "/Users/alice", nil },
			UserID:   func() int { return 501 },
			MkdirAll: func(string, fs.FileMode) error { return nil },
			WriteFile: func(_ string, data []byte, _ fs.FileMode) error {
				wrote = append([]byte(nil), data...)
				return nil
			},
			Remove: func(string) error { return nil },
			Run: func(string, ...string) ([]byte, error) {
				launchctlCalls++
				return nil, nil
			},
		}
	}
	executablePath = func() (string, error) { return "/opt/spectra-remote/bin/spectra-remote-agent", nil }
	var stdout, stderr bytes.Buffer
	code := run([]string{
		"install", "--no-load",
		"--spectra", "/opt/spectra/bin/spectra",
		"--tsnet-hostname", "work-mac",
		"--tsnet-state-dir", "/Users/alice/.spectra-remote/tsnet/agent",
		"--audit-log", "/Users/alice/Library/Logs/Spectra Remote/agent.audit.jsonl",
		"--max-connections", "4",
		"--allow-app-root", "/Applications",
		"--tsnet-tag", "tag:engineer",
		"--tsnet-allow-login", "engineer@example.com",
	}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run() = %d, stderr = %q", code, stderr.String())
	}
	if launchctlCalls != 0 {
		t.Fatalf("launchctl calls = %d, want 0", launchctlCalls)
	}
	for _, want := range []string{"tag:engineer", "engineer@example.com", "/Applications", "agent.audit.jsonl", ">4<"} {
		if !strings.Contains(string(wrote), want) {
			t.Fatalf("plist does not include %q:\n%s", want, wrote)
		}
	}
}

func TestSnapshotPolicyFlagDependency(t *testing.T) {
	for _, command := range []string{"serve-stdio", "serve-tsnet", "install"} {
		t.Run(command, func(t *testing.T) {
			var out, errout bytes.Buffer
			args := []string{command, "--spectra", "/opt/spectra/bin/spectra", "--allow-snapshot-apps"}
			if code := run(args, strings.NewReader(""), &out, &errout); code != 2 || !strings.Contains(errout.String(), "requires --allow-snapshot") {
				t.Fatalf("code=%d stderr=%q", code, errout.String())
			}
		})
	}
}

func TestStdioAuditIncludesLocalPeer(t *testing.T) {
	dir := t.TempDir()
	spectra := filepath.Join(dir, "spectra")
	audit := filepath.Join(dir, "audit.jsonl")
	script := `#!/bin/sh
if [ "$1" = "capabilities" ] && [ "$2" = "--json" ]; then
 printf '%s' '{"schema":{"name":"spectra.capabilities","version":1},"spectra_version":"test","interfaces":[]}'
else
 exit 1
fi
`
	if err := os.WriteFile(spectra, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	input := strings.NewReader(`{"protocol_version":"v1","request_id":"health-1","operation":"health"}` + "\n")
	code := run([]string{"serve-stdio", "--spectra", spectra, "--audit-log", audit}, input, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
	data, err := os.ReadFile(audit)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"transport":"stdio"`) || !strings.Contains(string(data), `"local_user"`) || strings.Count(string(data), "\n") != 2 {
		t.Fatalf("audit=%s", data)
	}
}

package main

import (
	"bytes"
	"io/fs"
	"strings"
	"testing"

	"github.com/kaeawc/spectra-remote/internal/agentinstall"
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
	for _, want := range []string{"tag:engineer", "engineer@example.com", "/Applications"} {
		if !strings.Contains(string(wrote), want) {
			t.Fatalf("plist does not include %q:\n%s", want, wrote)
		}
	}
}

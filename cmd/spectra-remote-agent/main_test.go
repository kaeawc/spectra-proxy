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

func writeProvisionedSpectra(t *testing.T) (root, binary string) {
	t.Helper()
	root = t.TempDir()
	version := filepath.Join(root, "versions", "v1.0.0")
	if err := os.MkdirAll(version, 0o700); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nprintf '%s' '{\"schema\":{\"name\":\"spectra.capabilities\",\"version\":1},\"spectra_version\":\"v1.0.0\",\"os\":\"darwin\",\"arch\":\"arm64\",\"interfaces\":[{\"name\":\"capabilities\",\"argv\":[\"capabilities\",\"--json\"],\"output\":\"json\",\"result_schema\":{\"name\":\"spectra.capabilities\",\"version\":1}}]}'\n"
	if err := os.WriteFile(filepath.Join(version, "spectra"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("versions", "v1.0.0"), filepath.Join(root, "current")); err != nil {
		t.Fatal(err)
	}
	return root, filepath.Join(root, "current", "spectra")
}

func TestServeStdioDefaultsToProvisionedSpectra(t *testing.T) {
	root, _ := writeProvisionedSpectra(t)
	var stdout, stderr bytes.Buffer
	input := strings.NewReader(`{"protocol_version":"v1","request_id":"health-1","operation":"health"}` + "\n")
	audit := filepath.Join(t.TempDir(), "audit.jsonl")
	if code := run([]string{"serve-stdio", "--provision-root", root, "--audit-log", audit}, input, &stdout, &stderr); code != 0 {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), `"spectra_version":"v1.0.0"`) {
		t.Fatalf("stdout=%s", stdout.String())
	}
}

func TestSpectraRequiredWithoutProvisionedBinary(t *testing.T) {
	empty := t.TempDir()
	// install keeps its existing validation error from agentinstall.
	cases := map[string]struct {
		code int
		want string
	}{
		"serve-stdio": {2, "--spectra path is required"},
		"serve-tsnet": {2, "--spectra path is required"},
		"install":     {1, "spectra executable path must be absolute"},
	}
	for command, tc := range cases {
		t.Run(command, func(t *testing.T) {
			args := []string{command, "--provision-root", empty}
			if command == "install" {
				args = append(args, "--no-load")
			}
			var stdout, stderr bytes.Buffer
			if code := run(args, strings.NewReader(""), &stdout, &stderr); code != tc.code || !strings.Contains(stderr.String(), tc.want) {
				t.Fatalf("code=%d stderr=%q", code, stderr.String())
			}
		})
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"serve-stdio", "--provision-root", "relative"}, strings.NewReader(""), &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), "--provision-root must be absolute") {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
}

func TestInstallUsesProvisionedSymlinkPath(t *testing.T) {
	oldDeps, oldExecutable := installDeps, executablePath
	t.Cleanup(func() {
		installDeps = oldDeps
		executablePath = oldExecutable
	})
	var wrote []byte
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
			Run:    func(string, ...string) ([]byte, error) { return nil, nil },
		}
	}
	executablePath = func() (string, error) { return "/opt/spectra-remote/bin/spectra-remote-agent", nil }
	root, binary := writeProvisionedSpectra(t)
	var stdout, stderr bytes.Buffer
	code := run([]string{"install", "--no-load", "--provision-root", root, "--tsnet-hostname", "work-mac"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
	if !strings.Contains(string(wrote), binary) {
		t.Fatalf("plist does not reference %s:\n%s", binary, wrote)
	}
}

func TestRunProvisionSubcommand(t *testing.T) {
	root := filepath.Join(t.TempDir(), "spectra")
	var stdout, stderr bytes.Buffer
	if code := run([]string{"provision", "status", "--root", root, "--json"}, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), `"binary_path"`) {
		t.Fatalf("stdout=%s", stdout.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"provision"}, strings.NewReader(""), &stdout, &stderr); code != 2 {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
}

func TestMaxRunDurationRejectsNonPositive(t *testing.T) {
	for _, command := range []string{"serve-stdio", "serve-tsnet", "install"} {
		t.Run(command, func(t *testing.T) {
			var out, errout bytes.Buffer
			args := []string{command, "--spectra", "/opt/spectra/bin/spectra", "--max-run-duration", "0s"}
			if command != "serve-stdio" {
				args = append(args, "--tsnet-hostname", "work-mac")
			}
			if command == "install" {
				args = append(args, "--no-load")
			}
			if code := run(args, strings.NewReader(""), &out, &errout); code != 2 || !strings.Contains(errout.String(), "--max-run-duration must be positive") {
				t.Fatalf("code=%d stderr=%q", code, errout.String())
			}
		})
	}
}

func TestMaxRunDurationRejectsAtOrOverTSNetSessionTimeout(t *testing.T) {
	for _, command := range []string{"serve-tsnet", "install"} {
		t.Run(command, func(t *testing.T) {
			var out, errout bytes.Buffer
			args := []string{command, "--spectra", "/opt/spectra/bin/spectra", "--tsnet-hostname", "work-mac", "--max-run-duration", "5m"}
			if command == "install" {
				args = append(args, "--no-load")
			}
			if code := run(args, strings.NewReader(""), &out, &errout); code != 2 || !strings.Contains(errout.String(), "tsnet session timeout") {
				t.Fatalf("code=%d stderr=%q", code, errout.String())
			}
		})
	}
}

func TestMaxRunDurationServeStdioAllowsUpToProtocolMax(t *testing.T) {
	// serve-stdio has no tsnet session cap, so a value at (but not over) the
	// protocol's own timeout ceiling must be accepted.
	var out, errout bytes.Buffer
	args := []string{"serve-stdio", "--spectra", "/opt/spectra/bin/spectra", "--max-run-duration", "10m"}
	if code := run(args, strings.NewReader(""), &out, &errout); code != 0 {
		t.Fatalf("code=%d stderr=%q", code, errout.String())
	}
}

func TestUsageListsProvision(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run(nil, strings.NewReader(""), &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), "spectra-remote-agent provision") {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
}

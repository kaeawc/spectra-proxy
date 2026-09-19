package agentinstall

import (
	"io/fs"
	"os"
	"strings"
	"testing"
)

func testOptions() Options {
	return Options{
		Program:        "/opt/spectra-remote/bin/spectra-remote-agent",
		SpectraPath:    "/opt/spectra/bin/spectra",
		ListenAddr:     ":7878",
		Hostname:       "work-mac",
		StateDir:       "/Users/alice/.spectra-remote/tsnet/agent",
		AuditLog:       "/Users/alice/Library/Logs/Spectra Remote/agent.audit.jsonl",
		MaxConnections: 8,
		AppRoots:       []string{"/Applications"},
		AllowLogins:    []string{"engineer@example.com"},
	}
}

func TestPlistIncludesOnlyConfiguredArguments(t *testing.T) {
	plist := Plist(testOptions(), "/Users/alice/Library/LaunchAgents/"+Label+".plist")
	for _, want := range []string{"serve-tsnet", "--spectra", "/opt/spectra/bin/spectra", "--audit-log", "agent.audit.jsonl", "--max-connections", ">8<", "--tsnet-allow-login", "engineer@example.com"} {
		if !strings.Contains(plist, want) {
			t.Fatalf("plist does not include %q:\n%s", want, plist)
		}
	}
	if strings.Contains(plist, "install") || strings.Contains(plist, "--remote") {
		t.Fatalf("plist contains an unexpected control capability:\n%s", plist)
	}
}

func TestInstallLoadsExpectedLaunchctlCommands(t *testing.T) {
	var writes map[string][]byte = map[string][]byte{}
	var calls [][]string
	deps := Deps{
		HomeDir:  func() (string, error) { return "/Users/alice", nil },
		UserID:   func() int { return 501 },
		MkdirAll: func(string, fs.FileMode) error { return nil },
		WriteFile: func(path string, data []byte, _ fs.FileMode) error {
			writes[path] = data
			return nil
		},
		Remove: func(string) error { return nil },
		Run: func(name string, args ...string) ([]byte, error) {
			calls = append(calls, append([]string{name}, args...))
			return nil, nil
		},
	}
	path, err := Install(testOptions(), deps)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := writes[path]; !ok {
		t.Fatalf("plist %q was not written", path)
	}
	want := [][]string{
		{"launchctl", "bootout", "gui/501", path},
		{"launchctl", "bootstrap", "gui/501", path},
		{"launchctl", "enable", "gui/501/" + Label},
		{"launchctl", "kickstart", "-k", "gui/501/" + Label},
	}
	if len(calls) != len(want) {
		t.Fatalf("calls = %#v", calls)
	}
	for i := range want {
		if strings.Join(calls[i], "\x00") != strings.Join(want[i], "\x00") {
			t.Fatalf("call %d = %#v, want %#v", i, calls[i], want[i])
		}
	}
}

func TestInstallNoLoadWritesWithoutLaunchctl(t *testing.T) {
	opts := testOptions()
	opts.NoLoad = true
	calls := 0
	deps := Deps{
		HomeDir:   func() (string, error) { return "/Users/alice", nil },
		UserID:    func() int { return 501 },
		MkdirAll:  func(string, fs.FileMode) error { return nil },
		WriteFile: func(string, []byte, fs.FileMode) error { return nil },
		Remove:    func(string) error { return nil },
		Run: func(string, ...string) ([]byte, error) {
			calls++
			return nil, nil
		},
	}
	if _, err := Install(opts, deps); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatalf("launchctl calls = %d, want 0", calls)
	}
}

func TestValidateRejectsRelativeExecutable(t *testing.T) {
	opts := testOptions()
	opts.Program = "spectra-remote-agent"
	if err := opts.Validate(); err == nil {
		t.Fatal("Validate succeeded with relative agent path")
	}
}

func TestUninstallIgnoresMissingPlist(t *testing.T) {
	deps := Deps{
		HomeDir:   func() (string, error) { return "/Users/alice", nil },
		UserID:    func() int { return 501 },
		MkdirAll:  func(string, fs.FileMode) error { return nil },
		WriteFile: func(string, []byte, fs.FileMode) error { return nil },
		Remove:    func(string) error { return os.ErrNotExist },
		Run:       func(string, ...string) ([]byte, error) { return nil, nil },
	}
	if err := Uninstall(deps); err != nil {
		t.Fatalf("Uninstall() error = %v", err)
	}
}

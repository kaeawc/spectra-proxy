// Package agentinstall installs the Spectra Remote target agent as a
// user-owned LaunchAgent. It never downloads code or accepts remote requests.
package agentinstall

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const Label = "dev.spectra-remote.agent"

// Options are the explicitly configured arguments passed to the target agent.
type Options struct {
	Program     string
	SpectraPath string
	ListenAddr  string
	Hostname    string
	StateDir    string
	Ephemeral   bool
	AppRoots    []string
	Tags        []string
	AllowLogins []string
	AllowNodes  []string
	NoLoad      bool
}

// Deps makes filesystem and launchctl behavior testable.
type Deps struct {
	HomeDir   func() (string, error)
	UserID    func() int
	MkdirAll  func(string, fs.FileMode) error
	WriteFile func(string, []byte, fs.FileMode) error
	Remove    func(string) error
	Run       func(string, ...string) ([]byte, error)
}

// DefaultDeps returns the production implementation.
func DefaultDeps() Deps {
	return Deps{
		HomeDir:   os.UserHomeDir,
		UserID:    os.Getuid,
		MkdirAll:  os.MkdirAll,
		WriteFile: writeFileAtomic,
		Remove:    os.Remove,
		Run: func(name string, args ...string) ([]byte, error) {
			return exec.Command(name, args...).CombinedOutput()
		},
	}
}

// Install writes the user LaunchAgent plist and, unless NoLoad is set, loads
// it into the current user's launchd domain.
func Install(opts Options, deps Deps) (string, error) {
	if err := opts.Validate(); err != nil {
		return "", err
	}
	plistPath, err := PlistPath(deps)
	if err != nil {
		return "", err
	}
	if err := deps.MkdirAll(filepath.Dir(plistPath), 0o755); err != nil {
		return "", fmt.Errorf("create LaunchAgents directory: %w", err)
	}
	if err := deps.MkdirAll(filepath.Dir(LogOutPath(plistPath)), 0o700); err != nil {
		return "", fmt.Errorf("create log directory: %w", err)
	}
	if err := deps.WriteFile(plistPath, []byte(Plist(opts, plistPath)), 0o600); err != nil {
		return "", fmt.Errorf("write LaunchAgent plist: %w", err)
	}
	if opts.NoLoad {
		return plistPath, nil
	}
	domain := UserDomain(deps.UserID())
	// A prior installation may not exist; bootout is intentionally best effort.
	_, _ = deps.Run("launchctl", "bootout", domain, plistPath)
	if output, err := deps.Run("launchctl", "bootstrap", domain, plistPath); err != nil {
		return "", launchctlError("bootstrap", output, err)
	}
	if output, err := deps.Run("launchctl", "enable", domain+"/"+Label); err != nil {
		return "", launchctlError("enable", output, err)
	}
	if output, err := deps.Run("launchctl", "kickstart", "-k", domain+"/"+Label); err != nil {
		return "", launchctlError("kickstart", output, err)
	}
	return plistPath, nil
}

// Uninstall unloads the current user's LaunchAgent and removes its plist.
func Uninstall(deps Deps) error {
	plistPath, err := PlistPath(deps)
	if err != nil {
		return err
	}
	_, _ = deps.Run("launchctl", "bootout", UserDomain(deps.UserID()), plistPath)
	if err := deps.Remove(plistPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove LaunchAgent plist: %w", err)
	}
	return nil
}

// Status returns launchd's current view of the user agent.
func Status(deps Deps) ([]byte, error) {
	output, err := deps.Run("launchctl", "print", UserDomain(deps.UserID())+"/"+Label)
	if err != nil {
		return nil, launchctlError("print", output, err)
	}
	return output, nil
}

// PlistPath returns the user-owned LaunchAgent plist path.
func PlistPath(deps Deps) (string, error) {
	home, err := deps.HomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, "Library", "LaunchAgents", Label+".plist"), nil
}

// LogOutPath returns the launchd stdout path associated with this plist.
func LogOutPath(plistPath string) string {
	home := filepath.Dir(filepath.Dir(filepath.Dir(plistPath)))
	return filepath.Join(home, "Library", "Logs", "Spectra Remote", "agent.out.log")
}

// UserDomain returns the per-user launchd domain for uid.
func UserDomain(uid int) string { return fmt.Sprintf("gui/%d", uid) }

// Validate rejects ambiguous executable or target-agent configuration.
func (o Options) Validate() error {
	if !filepath.IsAbs(o.Program) {
		return fmt.Errorf("agent executable path must be absolute")
	}
	if !filepath.IsAbs(o.SpectraPath) {
		return fmt.Errorf("spectra executable path must be absolute")
	}
	if strings.TrimSpace(o.Hostname) == "" {
		return fmt.Errorf("tsnet hostname is required")
	}
	if !filepath.IsAbs(o.StateDir) {
		return fmt.Errorf("tsnet state directory must be absolute")
	}
	for _, root := range o.AppRoots {
		if !filepath.IsAbs(root) {
			return fmt.Errorf("app root must be absolute: %q", root)
		}
	}
	return nil
}

// Plist returns the exact plist document to be written for opts.
func Plist(opts Options, plistPath string) string {
	args := []string{opts.Program, "serve-tsnet", "--spectra", opts.SpectraPath, "--tsnet-addr", opts.ListenAddr, "--tsnet-hostname", opts.Hostname, "--tsnet-state-dir", opts.StateDir}
	if opts.Ephemeral {
		args = append(args, "--tsnet-ephemeral")
	}
	for _, root := range opts.AppRoots {
		args = append(args, "--allow-app-root", root)
	}
	for _, tag := range opts.Tags {
		args = append(args, "--tsnet-tag", tag)
	}
	for _, login := range opts.AllowLogins {
		args = append(args, "--tsnet-allow-login", login)
	}
	for _, node := range opts.AllowNodes {
		args = append(args, "--tsnet-allow-node", node)
	}
	var b strings.Builder
	b.WriteString("<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n")
	b.WriteString("<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" \"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">\n")
	b.WriteString("<plist version=\"1.0\"><dict>\n")
	writePlistString(&b, "Label", Label)
	b.WriteString("<key>ProgramArguments</key><array>\n")
	for _, arg := range args {
		writeXMLString(&b, arg)
	}
	b.WriteString("</array>\n<key>RunAtLoad</key><true/>\n<key>KeepAlive</key><true/>\n")
	writePlistString(&b, "StandardOutPath", LogOutPath(plistPath))
	writePlistString(&b, "StandardErrorPath", strings.Replace(LogOutPath(plistPath), ".out.log", ".err.log", 1))
	b.WriteString("</dict></plist>\n")
	return b.String()
}

func writePlistString(b *strings.Builder, key, value string) {
	b.WriteString("<key>")
	b.WriteString(key)
	b.WriteString("</key>")
	writeXMLString(b, value)
	b.WriteByte('\n')
}

func writeXMLString(b *strings.Builder, value string) {
	b.WriteString("<string>")
	var escaped bytes.Buffer
	_ = xmlEscape(&escaped, []byte(value))
	b.WriteString(escaped.String())
	b.WriteString("</string>\n")
}

func xmlEscape(dst *bytes.Buffer, src []byte) error {
	for _, ch := range src {
		switch ch {
		case '&':
			dst.WriteString("&amp;")
		case '<':
			dst.WriteString("&lt;")
		case '>':
			dst.WriteString("&gt;")
		case '"':
			dst.WriteString("&quot;")
		case '\'':
			dst.WriteString("&apos;")
		default:
			dst.WriteByte(ch)
		}
	}
	return nil
}

func launchctlError(action string, output []byte, err error) error {
	message := strings.TrimSpace(string(output))
	if message == "" {
		message = err.Error()
	}
	return fmt.Errorf("launchctl %s: %s", action, message)
}

func writeFileAtomic(path string, data []byte, perm fs.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".spectra-remote-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

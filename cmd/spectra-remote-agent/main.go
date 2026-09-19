// Command spectra-remote-agent mediates authenticated remote diagnostic
// requests to a local Spectra installation.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/kaeawc/spectra-remote/internal/agent"
	"github.com/kaeawc/spectra-remote/internal/agentinstall"
	remoteTSNet "github.com/kaeawc/spectra-remote/internal/transport/tsnet"
)

var version = "dev"

var (
	installDeps    = agentinstall.DefaultDeps
	executablePath = os.Executable
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 1 && args[0] == "version" {
		fmt.Fprintln(stdout, version)
		return 0
	}
	if len(args) == 0 {
		printUsage(stderr)
		return 2
	}
	switch args[0] {
	case "serve-stdio":
		return runStdio(args[1:], stdin, stdout, stderr)
	case "serve-tsnet":
		return runTSNet(args[1:], stderr)
	case "install":
		return runInstall(args[1:], stdout, stderr)
	default:
		printUsage(stderr)
		return 2
	}
}

func runStdio(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("spectra-remote-agent serve-stdio", flag.ContinueOnError)
	fs.SetOutput(stderr)
	spectraPath := fs.String("spectra", "", "Absolute path to the local spectra executable")
	auditLog, err := defaultAuditLogPath()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	fs.StringVar(&auditLog, "audit-log", auditLog, "Owner-private JSONL audit log path")
	var appRoots pathList
	fs.Var(&appRoots, "allow-app-root", "Absolute app root allowed for inspect requests; may be repeated")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	a, ok := newAgent(*spectraPath, appRoots, auditLog, stderr)
	if !ok {
		return 2
	}
	return serveStdio(context.Background(), a, stdin, stdout, stderr)
}

func serveStdio(ctx context.Context, a agent.Agent, stdin io.Reader, stdout, stderr io.Writer) int {
	if err := agent.Serve(ctx, a, stdin, stdout); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

func runTSNet(args []string, stderr io.Writer) int {
	opts, code := parseTSNetOptions("spectra-remote-agent serve-tsnet", args, stderr, false)
	if code != 0 {
		return code
	}
	a, ok := newAgent(opts.spectraPath, opts.appRoots, opts.auditLog, stderr)
	if !ok {
		return 2
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	err := remoteTSNet.Serve(ctx, a, remoteTSNet.Config{
		StateDir:       opts.stateDir,
		Hostname:       opts.hostname,
		Ephemeral:      opts.ephemeral,
		AdvertiseTags:  opts.tags,
		AllowLogins:    opts.allowLogins,
		AllowNodes:     opts.allowNodes,
		MaxConnections: opts.maxConnections,
		Logf: func(format string, args ...any) {
			fmt.Fprintf(stderr, format+"\n", args...)
		},
	}, opts.listenAddr)
	if err != nil {
		fmt.Fprintln(stderr, "serve-tsnet:", err)
		return 1
	}
	return 0
}

func runInstall(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		switch args[0] {
		case "status":
			if len(args) != 1 {
				fmt.Fprintln(stderr, "usage: spectra-remote-agent install status")
				return 2
			}
			output, err := agentinstall.Status(installDeps())
			if err != nil {
				fmt.Fprintln(stderr, err)
				return 1
			}
			_, _ = stdout.Write(output)
			return 0
		case "uninstall":
			if len(args) != 1 {
				fmt.Fprintln(stderr, "usage: spectra-remote-agent install uninstall")
				return 2
			}
			if err := agentinstall.Uninstall(installDeps()); err != nil {
				fmt.Fprintln(stderr, err)
				return 1
			}
			fmt.Fprintln(stdout, "Spectra Remote agent LaunchAgent removed")
			return 0
		}
	}
	opts, code := parseTSNetOptions("spectra-remote-agent install", args, stderr, true)
	if code != 0 {
		return code
	}
	program, err := executablePath()
	if err != nil {
		fmt.Fprintln(stderr, "resolve agent executable:", err)
		return 1
	}
	plistPath, err := agentinstall.Install(agentinstall.Options{
		Program:        program,
		SpectraPath:    opts.spectraPath,
		ListenAddr:     opts.listenAddr,
		Hostname:       opts.hostname,
		StateDir:       opts.stateDir,
		AuditLog:       opts.auditLog,
		MaxConnections: opts.maxConnections,
		Ephemeral:      opts.ephemeral,
		AppRoots:       opts.appRoots,
		Tags:           opts.tags,
		AllowLogins:    opts.allowLogins,
		AllowNodes:     opts.allowNodes,
		NoLoad:         opts.noLoad,
	}, installDeps())
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if opts.noLoad {
		fmt.Fprintf(stdout, "Spectra Remote agent plist written to %s (not loaded)\n", plistPath)
	} else {
		fmt.Fprintf(stdout, "Spectra Remote agent installed at %s\n", plistPath)
	}
	return 0
}

type tsnetOptions struct {
	spectraPath    string
	listenAddr     string
	hostname       string
	stateDir       string
	auditLog       string
	maxConnections int
	ephemeral      bool
	appRoots       pathList
	tags           stringList
	allowLogins    stringList
	allowNodes     stringList
	noLoad         bool
}

func parseTSNetOptions(name string, args []string, stderr io.Writer, allowNoLoad bool) (tsnetOptions, int) {
	stateDir, err := remoteTSNet.DefaultStateDir("agent")
	if err != nil {
		fmt.Fprintln(stderr, err)
		return tsnetOptions{}, 1
	}
	auditLog, err := defaultAuditLogPath()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return tsnetOptions{}, 1
	}
	opts := tsnetOptions{listenAddr: remoteTSNet.DefaultAddr, hostname: "spectra-remote-agent", stateDir: stateDir, auditLog: auditLog, maxConnections: remoteTSNet.DefaultMaxConnections}
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&opts.spectraPath, "spectra", "", "Absolute path to the local spectra executable")
	fs.StringVar(&opts.listenAddr, "tsnet-addr", opts.listenAddr, "Tailnet listen address")
	fs.StringVar(&opts.hostname, "tsnet-hostname", opts.hostname, "Tailnet node hostname")
	fs.StringVar(&opts.stateDir, "tsnet-state-dir", opts.stateDir, "Private tsnet state directory")
	fs.StringVar(&opts.auditLog, "audit-log", opts.auditLog, "Owner-private JSONL audit log path")
	fs.IntVar(&opts.maxConnections, "max-connections", opts.maxConnections, "Maximum simultaneous target-agent sessions")
	fs.BoolVar(&opts.ephemeral, "tsnet-ephemeral", false, "Register an ephemeral tailnet node")
	fs.Var(&opts.appRoots, "allow-app-root", "Absolute app root allowed for inspect requests; may be repeated")
	fs.Var(&opts.tags, "tsnet-tag", "Tailnet tag to advertise; may be repeated")
	fs.Var(&opts.allowLogins, "tsnet-allow-login", "Tailnet login name allowed to connect; may be repeated")
	fs.Var(&opts.allowNodes, "tsnet-allow-node", "Tailnet node name allowed to connect; may be repeated")
	if allowNoLoad {
		fs.BoolVar(&opts.noLoad, "no-load", false, "Write the LaunchAgent plist without loading it")
	}
	if err := fs.Parse(args); err != nil {
		return tsnetOptions{}, 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "unexpected positional arguments")
		return tsnetOptions{}, 2
	}
	if opts.maxConnections < 1 {
		fmt.Fprintln(stderr, "max-connections must be positive")
		return tsnetOptions{}, 2
	}
	return opts, 0
}

func newAgent(spectraPath string, appRoots pathList, auditLog string, stderr io.Writer) (agent.Agent, bool) {
	if spectraPath == "" || !strings.HasPrefix(spectraPath, "/") {
		fmt.Fprintln(stderr, "an absolute --spectra path is required")
		return agent.Agent{}, false
	}
	auditor, err := agent.NewJSONLAuditor(auditLog)
	if err != nil {
		fmt.Fprintln(stderr, "configure audit log:", err)
		return agent.Agent{}, false
	}
	return agent.Agent{
		Runner:       agent.LocalSpectra{Path: spectraPath, AllowedAppRoots: appRoots},
		AgentVersion: version,
		Auditor:      auditor,
	}, true
}

func defaultAuditLogPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory for audit log: %w", err)
	}
	return filepath.Join(home, "Library", "Logs", "Spectra Remote", "agent.audit.jsonl"), nil
}

func printUsage(w io.Writer) {
	fmt.Fprintln(w, "usage: spectra-remote-agent serve-stdio --spectra /absolute/path [--allow-app-root /Applications]")
	fmt.Fprintln(w, "   or: spectra-remote-agent serve-tsnet --spectra /absolute/path --tsnet-hostname name [--allow-app-root /Applications]")
	fmt.Fprintln(w, "   or: spectra-remote-agent install --spectra /absolute/path --tsnet-hostname name [--no-load]")
	fmt.Fprintln(w, "   or: spectra-remote-agent install status|uninstall")
}

type stringList []string

func (l *stringList) String() string { return strings.Join(*l, ",") }
func (l *stringList) Set(value string) error {
	*l = append(*l, value)
	return nil
}

type pathList []string

func (l *pathList) String() string { return strings.Join(*l, ",") }
func (l *pathList) Set(value string) error {
	if !strings.HasPrefix(value, "/") {
		return fmt.Errorf("app root must be absolute")
	}
	*l = append(*l, value)
	return nil
}

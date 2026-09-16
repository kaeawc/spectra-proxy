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
	"strings"
	"syscall"

	"github.com/kaeawc/spectra-remote/internal/agent"
	remoteTSNet "github.com/kaeawc/spectra-remote/internal/transport/tsnet"
)

var version = "dev"

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
	default:
		printUsage(stderr)
		return 2
	}
}

func runStdio(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("spectra-remote-agent serve-stdio", flag.ContinueOnError)
	fs.SetOutput(stderr)
	spectraPath := fs.String("spectra", "", "Absolute path to the local spectra executable")
	var appRoots stringList
	fs.Var(&appRoots, "allow-app-root", "Absolute app root allowed for inspect requests; may be repeated")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	a, ok := newAgent(*spectraPath, appRoots, stderr)
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
	fs := flag.NewFlagSet("spectra-remote-agent serve-tsnet", flag.ContinueOnError)
	fs.SetOutput(stderr)
	spectraPath := fs.String("spectra", "", "Absolute path to the local spectra executable")
	listenAddr := fs.String("tsnet-addr", remoteTSNet.DefaultAddr, "Tailnet listen address")
	hostname := fs.String("tsnet-hostname", "spectra-remote-agent", "Tailnet node hostname")
	stateDir, err := remoteTSNet.DefaultStateDir("agent")
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	fs.StringVar(&stateDir, "tsnet-state-dir", stateDir, "Private tsnet state directory")
	ephemeral := fs.Bool("tsnet-ephemeral", false, "Register an ephemeral tailnet node")
	var appRoots, tags, allowLogins, allowNodes stringList
	fs.Var(&appRoots, "allow-app-root", "Absolute app root allowed for inspect requests; may be repeated")
	fs.Var(&tags, "tsnet-tag", "Tailnet tag to advertise; may be repeated")
	fs.Var(&allowLogins, "tsnet-allow-login", "Tailnet login name allowed to connect; may be repeated")
	fs.Var(&allowNodes, "tsnet-allow-node", "Tailnet node name allowed to connect; may be repeated")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	a, ok := newAgent(*spectraPath, appRoots, stderr)
	if !ok {
		return 2
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	err = remoteTSNet.Serve(ctx, a, remoteTSNet.Config{
		StateDir:      stateDir,
		Hostname:      *hostname,
		Ephemeral:     *ephemeral,
		AdvertiseTags: tags,
		AllowLogins:   allowLogins,
		AllowNodes:    allowNodes,
		Logf: func(format string, args ...any) {
			fmt.Fprintf(stderr, format+"\n", args...)
		},
	}, *listenAddr)
	if err != nil {
		fmt.Fprintln(stderr, "serve-tsnet:", err)
		return 1
	}
	return 0
}

func newAgent(spectraPath string, appRoots stringList, stderr io.Writer) (agent.Agent, bool) {
	if spectraPath == "" || !strings.HasPrefix(spectraPath, "/") {
		fmt.Fprintln(stderr, "an absolute --spectra path is required")
		return agent.Agent{}, false
	}
	return agent.Agent{
		Runner:       agent.LocalSpectra{Path: spectraPath, AllowedAppRoots: appRoots},
		AgentVersion: version,
	}, true
}

func printUsage(w io.Writer) {
	fmt.Fprintln(w, "usage: spectra-remote-agent serve-stdio --spectra /absolute/path [--allow-app-root /Applications]")
	fmt.Fprintln(w, "   or: spectra-remote-agent serve-tsnet --spectra /absolute/path --tsnet-hostname name [--allow-app-root /Applications]")
}

type stringList []string

func (l *stringList) String() string { return strings.Join(*l, ",") }
func (l *stringList) Set(value string) error {
	if !strings.HasPrefix(value, "/") {
		return fmt.Errorf("app root must be absolute")
	}
	*l = append(*l, value)
	return nil
}

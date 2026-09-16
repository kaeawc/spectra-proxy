// Command spectra-remote-agent mediates authenticated remote diagnostic
// requests to a local Spectra installation. The initial stdio mode is meant
// for a future authenticated transport supervisor; it never opens a socket.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	protocol "github.com/kaeawc/spectra-protocol/protocol/v1"
	"github.com/kaeawc/spectra-remote/internal/agent"
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
	if len(args) == 0 || args[0] != "serve-stdio" {
		fmt.Fprintln(stderr, "usage: spectra-remote-agent serve-stdio --spectra /absolute/path [--allow-app-root /Applications]")
		return 2
	}
	fs := flag.NewFlagSet("spectra-remote-agent serve-stdio", flag.ContinueOnError)
	fs.SetOutput(stderr)
	spectraPath := fs.String("spectra", "", "Absolute path to the local spectra executable")
	var appRoots stringList
	fs.Var(&appRoots, "allow-app-root", "Absolute app root allowed for inspect requests; may be repeated")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if *spectraPath == "" || !strings.HasPrefix(*spectraPath, "/") {
		fmt.Fprintln(stderr, "serve-stdio requires an absolute --spectra path")
		return 2
	}
	a := agent.Agent{
		Runner:       agent.LocalSpectra{Path: *spectraPath, AllowedAppRoots: appRoots},
		AgentVersion: version,
	}
	return serveStdio(context.Background(), a, stdin, stdout, stderr)
}

func serveStdio(ctx context.Context, a agent.Agent, stdin io.Reader, stdout, stderr io.Writer) int {
	scanner := bufio.NewScanner(stdin)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	enc := json.NewEncoder(stdout)
	for scanner.Scan() {
		var req protocol.Request
		if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
			if err := enc.Encode(protocol.Response{ProtocolVersion: protocol.Version, Error: &protocol.Error{Code: "invalid_request", Message: "invalid JSON request"}}); err != nil {
				fmt.Fprintln(stderr, "write response:", err)
				return 1
			}
			continue
		}
		if err := enc.Encode(a.Handle(ctx, req)); err != nil {
			fmt.Fprintln(stderr, "write response:", err)
			return 1
		}
	}
	if err := scanner.Err(); err != nil {
		fmt.Fprintln(stderr, "read request:", err)
		return 1
	}
	return 0
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

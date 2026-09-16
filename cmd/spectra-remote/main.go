// Command spectra-remote is the authenticated controller for Spectra Remote.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	protocol "github.com/kaeawc/spectra-protocol/protocol/v1"
	remoteTSNet "github.com/kaeawc/spectra-remote/internal/transport/tsnet"
)

var version = "dev"

func main() {
	if len(os.Args) == 2 && os.Args[1] == "version" {
		fmt.Println(version)
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "call" {
		os.Exit(runCall(os.Args[2:]))
	}
	fmt.Fprintln(os.Stderr, "usage: spectra-remote call --target host:7878 --operation health [--params '{}']")
	os.Exit(2)
}

func runCall(args []string) int {
	fs := flag.NewFlagSet("spectra-remote call", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	target := fs.String("target", "", "Target tailnet hostname and port")
	operation := fs.String("operation", "", "Typed protocol operation")
	params := fs.String("params", "", "Operation parameters as JSON")
	timeout := fs.Duration("timeout", 30*time.Second, "Connection and request timeout")
	hostname := fs.String("tsnet-hostname", "spectra-remote-controller", "Controller tailnet node hostname")
	stateDir, err := remoteTSNet.DefaultStateDir("controller")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fs.StringVar(&stateDir, "tsnet-state-dir", stateDir, "Private tsnet state directory")
	ephemeral := fs.Bool("tsnet-ephemeral", false, "Register an ephemeral tailnet node")
	var tags stringList
	fs.Var(&tags, "tsnet-tag", "Tailnet tag to advertise; may be repeated")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if strings.TrimSpace(*target) == "" || strings.TrimSpace(*operation) == "" {
		fmt.Fprintln(os.Stderr, "call requires --target and --operation")
		return 2
	}
	if *timeout <= 0 {
		fmt.Fprintln(os.Stderr, "timeout must be positive")
		return 2
	}
	request := protocol.Request{
		ProtocolVersion: protocol.Version,
		RequestID:       fmt.Sprintf("cli-%d", time.Now().UnixNano()),
		Operation:       protocol.Operation(*operation),
		TimeoutMS:       int(timeout.Milliseconds()),
	}
	if *params != "" {
		if !json.Valid([]byte(*params)) {
			fmt.Fprintln(os.Stderr, "params must be valid JSON")
			return 2
		}
		request.Params = json.RawMessage(*params)
	}
	if err := request.Validate(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	conn, node, err := remoteTSNet.Dial(ctx, remoteTSNet.Config{
		StateDir:      stateDir,
		Hostname:      *hostname,
		Ephemeral:     *ephemeral,
		AdvertiseTags: tags,
		Logf: func(format string, args ...any) {
			fmt.Fprintf(os.Stderr, format+"\n", args...)
		},
	}, *target)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer node.Close()
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(*timeout))
	if err := json.NewEncoder(conn).Encode(request); err != nil {
		fmt.Fprintln(os.Stderr, "write request:", err)
		return 1
	}
	var response protocol.Response
	if err := json.NewDecoder(conn).Decode(&response); err != nil {
		fmt.Fprintln(os.Stderr, "read response:", err)
		return 1
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(response); err != nil {
		fmt.Fprintln(os.Stderr, "write response:", err)
		return 1
	}
	if response.Error != nil {
		return 1
	}
	return 0
}

type stringList []string

func (l *stringList) String() string { return strings.Join(*l, ",") }
func (l *stringList) Set(value string) error {
	*l = append(*l, value)
	return nil
}

package provision

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strings"

	release "github.com/kaeawc/spectra-protocol/release/v1"
)

type repeated []string

func (r *repeated) String() string     { return strings.Join(*r, ",") }
func (r *repeated) Set(s string) error { *r = append(*r, s); return nil }

func Run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: provision install|update|rollback|status|uninstall")
		return 2
	}
	command := args[0]
	if command != "install" && command != "update" && command != "rollback" && command != "status" && command != "uninstall" {
		fmt.Fprintf(stderr, "unknown subcommand %q\n", command)
		return 2
	}
	parsed, code := parseCLI(command, args[1:], stderr)
	if code != 0 {
		return code
	}
	c, err := LoadConfig(parsed.configPath)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if parsed.root != "" {
		c.Root = parsed.root
	}
	if len(parsed.sources) > 0 {
		c.Sources = parsed.sources
	}
	c.TrustedKeys = append(c.TrustedKeys, parsed.keys...)
	c.AllowedRedirectHosts = append(c.AllowedRedirectHosts, parsed.redirects...)
	if err := runCommand(ctx, command, parsed, Options{Config: c}, stdout); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

type cliArgs struct {
	configPath, root, version string
	jsonOutput                bool
	sources, keys, redirects  repeated
}

func parseCLI(command string, args []string, stderr io.Writer) (cliArgs, int) {
	var parsed cliArgs
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&parsed.configPath, "config", "", "optional JSON configuration")
	fs.StringVar(&parsed.root, "root", "", "provisioning root")
	fs.StringVar(&parsed.version, "version", "", "Spectra release version")
	fs.BoolVar(&parsed.jsonOutput, "json", false, "JSON status")
	fs.Var(&parsed.sources, "source", "release source (repeatable)")
	fs.Var(&parsed.keys, "trusted-key", "trusted release key (repeatable)")
	fs.Var(&parsed.redirects, "allow-redirect-host", "allowed redirect host (repeatable)")
	if err := fs.Parse(args); err != nil {
		return parsed, 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "unexpected positional arguments")
		return parsed, 2
	}
	if (command == "install" || command == "update") && parsed.version == "" {
		fmt.Fprintln(stderr, "--version is required")
		return parsed, 2
	}
	if command != "install" && command != "update" && parsed.version != "" {
		fmt.Fprintln(stderr, "--version is only valid for install or update")
		return parsed, 2
	}
	if parsed.version != "" {
		if _, err := release.ParseVersion(parsed.version); err != nil {
			fmt.Fprintf(stderr, "invalid --version: %v\n", err)
			return parsed, 2
		}
	}
	if command != "status" && parsed.jsonOutput {
		fmt.Fprintln(stderr, "--json is only valid for status")
		return parsed, 2
	}
	return parsed, 0
}

func runCommand(ctx context.Context, command string, parsed cliArgs, opts Options, stdout io.Writer) error {
	var err error
	switch command {
	case "install", "update":
		var result Result
		if command == "install" {
			result, err = Install(ctx, opts, parsed.version)
		} else {
			result, err = Update(ctx, opts, parsed.version)
		}
		if err == nil {
			fmt.Fprintf(stdout, "Spectra %s installed (previous: %s)\n", result.Version, result.Previous)
		}
	case "rollback":
		var result Result
		result, err = Rollback(ctx, opts)
		if err == nil {
			fmt.Fprintf(stdout, "Rolled back to Spectra %s\n", result.Version)
		}
	case "status":
		var report StatusReport
		report, err = Status(opts)
		if err == nil {
			err = printStatus(stdout, report, parsed.jsonOutput)
		}
	case "uninstall":
		err = Uninstall(ctx, opts)
		if err == nil {
			fmt.Fprintln(stdout, "Spectra uninstalled")
		}
	}
	return err
}

func printStatus(w io.Writer, r StatusReport, asJSON bool) error {
	if asJSON {
		if err := json.NewEncoder(w).Encode(r); err != nil {
			return fmt.Errorf("write JSON status: %w", err)
		}
		return nil
	}
	_, err := fmt.Fprintf(w, "Current: %s\nPrevious: %s\nBinary: %s\nDrift detected: %t\nVersions: %d\n", r.Current, r.Previous, r.BinaryPath, r.CurrentDriftDetected, len(r.Versions))
	if err != nil {
		return fmt.Errorf("write status: %w", err)
	}
	return nil
}

// Command spectra-remote is the authenticated controller for Spectra Remote.
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/kaeawc/spectra-proxy/internal/controller"
	remoteTSNet "github.com/kaeawc/spectra-proxy/internal/transport/tsnet"
)

var version = "dev"

func main() {
	if len(os.Args) == 2 && os.Args[1] == "version" {
		fmt.Println(version)
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "call" {
		stateDir, err := remoteTSNet.DefaultStateDir("controller")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		opts, target, tsnetFlags, code := controller.ParseCallFlags(os.Args[2:], os.Stderr, stateDir)
		if code != 0 {
			os.Exit(code)
		}
		ctx, cancel := context.WithTimeout(context.Background(), opts.Timeout)
		defer cancel()
		conn, node, err := remoteTSNet.Dial(ctx, remoteTSNet.Config{
			StateDir: tsnetFlags.StateDir, Hostname: tsnetFlags.Hostname,
			Ephemeral: tsnetFlags.Ephemeral, AdvertiseTags: tsnetFlags.Tags,
			Logf: func(format string, args ...any) { fmt.Fprintf(os.Stderr, format+"\n", args...) },
		}, target)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		defer node.Close()
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(opts.Timeout))
		os.Exit(controller.RunCall(ctx, conn, opts, os.Stdout, os.Stderr))
	}
	fmt.Fprintln(os.Stderr, "usage: spectra-remote call --target host:7878 --operation health [--params '{}']")
	os.Exit(2)
}

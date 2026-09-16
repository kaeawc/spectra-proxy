// Command spectra-remote will be the authenticated controller. Transport
// support is intentionally withheld until target authorization and auditing
// are implemented with it.
package main

import (
	"fmt"
	"os"
)

var version = "dev"

func main() {
	if len(os.Args) == 2 && os.Args[1] == "version" {
		fmt.Println(version)
		return
	}
	fmt.Fprintln(os.Stderr, "spectra-remote: controller transport is not configured yet")
	os.Exit(2)
}

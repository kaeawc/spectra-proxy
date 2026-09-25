//go:build !unix

package provision

import "os"

// Non-Unix targets currently lack interprocess provisioning locks.
func lock(string) (func(), error)       { return func() {}, nil }
func checkConfigFile(os.FileInfo) error { return nil }

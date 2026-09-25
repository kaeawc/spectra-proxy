//go:build !unix

package agent

import "os/exec"

func configureProcessGroup(cmd *exec.Cmd) {}

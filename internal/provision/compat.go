package provision

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"runtime"
	"time"

	protocolv1 "github.com/kaeawc/spectra-protocol/protocol/v1"
)

const supportedCapabilitiesSchemaVersion = 1
const maxCapabilitiesBytes = 1 << 20

type Runner interface {
	Capabilities(context.Context, string) ([]byte, error)
}
type commandRunner struct{}

type limitedOutput struct {
	bytes.Buffer
	exceeded bool
}

func (w *limitedOutput) Write(p []byte) (int, error) {
	if w.Len()+len(p) > maxCapabilitiesBytes {
		w.exceeded = true
		return 0, fmt.Errorf("capabilities output exceeds 1 MiB")
	}
	return w.Buffer.Write(p)
}

func (commandRunner) Capabilities(ctx context.Context, binary string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "capabilities", "--json")
	var out limitedOutput
	cmd.Stdout = &out
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("run capabilities --json: %w", err)
	}
	return out.Bytes(), nil
}

type IncompatibleError struct {
	Check string
	Err   error
}

func (e *IncompatibleError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("incompatible Spectra %s: %v", e.Check, e.Err)
	}
	return "incompatible Spectra " + e.Check
}
func (e *IncompatibleError) Unwrap() error { return e.Err }

func checkCapabilities(ctx context.Context, runner Runner, binary, version string, schemaVersion int) error {
	return checkCapabilitiesForOS(ctx, runner, binary, version, schemaVersion, runtime.GOOS)
}

func checkCapabilitiesForOS(ctx context.Context, runner Runner, binary, version string, schemaVersion int, goos string) error {
	data, err := runner.Capabilities(ctx, binary)
	if err != nil {
		return &IncompatibleError{Check: "capabilities command", Err: err}
	}
	if len(data) > maxCapabilitiesBytes {
		return &IncompatibleError{Check: "capabilities output size"}
	}
	c, err := protocolv1.DecodeSpectraCapabilities(data)
	if err != nil {
		return &IncompatibleError{Check: "capabilities JSON", Err: err}
	}
	if c.Schema.Version != schemaVersion || c.Schema.Version != protocolv1.CapabilitiesSchemaVersion {
		return &IncompatibleError{Check: fmt.Sprintf("capabilities schema version %d, manifest %d, supported %d", c.Schema.Version, schemaVersion, protocolv1.CapabilitiesSchemaVersion)}
	}
	if c.SpectraVersion != version {
		return &IncompatibleError{Check: fmt.Sprintf("spectra_version %q, expected %q", c.SpectraVersion, version)}
	}
	if c.OS != goos {
		return &IncompatibleError{Check: fmt.Sprintf("os %q, expected %q", c.OS, goos)}
	}
	if c.Arch != runtime.GOARCH {
		return &IncompatibleError{Check: fmt.Sprintf("arch %q, expected %q", c.Arch, runtime.GOARCH)}
	}
	if _, err := c.ResultSchemaFor(protocolv1.OperationSnapshotCreate); err != nil {
		return &IncompatibleError{Check: "snapshot interface", Err: err}
	}
	if goos == "darwin" {
		if _, err := c.ResultSchemaFor(protocolv1.OperationInspect); err != nil {
			return &IncompatibleError{Check: "inspect interface", Err: err}
		}
	} else if _, ok := c.Interface(protocolv1.InterfaceInspect); ok {
		if _, err := c.ResultSchemaFor(protocolv1.OperationInspect); err != nil {
			return &IncompatibleError{Check: "inspect interface", Err: err}
		}
	}
	return nil
}

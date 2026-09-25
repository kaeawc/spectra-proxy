package provision

import (
	"bytes"
	"context"
	"encoding/json"
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

type schemaRef struct {
	Name    string `json:"name"`
	Version int    `json:"version"`
}
type capabilityInterface struct {
	Name         string    `json:"name"`
	ResultSchema schemaRef `json:"result_schema"`
}
type capabilities struct {
	Schema         schemaRef             `json:"schema"`
	SpectraVersion string                `json:"spectra_version"`
	OS             string                `json:"os"`
	Arch           string                `json:"arch"`
	Interfaces     []capabilityInterface `json:"interfaces"`
}

func checkCapabilities(ctx context.Context, runner Runner, binary, version string, schemaVersion int) error {
	data, err := runner.Capabilities(ctx, binary)
	if err != nil {
		return &IncompatibleError{Check: "capabilities command", Err: err}
	}
	if len(data) > maxCapabilitiesBytes {
		return &IncompatibleError{Check: "capabilities output size"}
	}
	var c capabilities
	if err := json.Unmarshal(data, &c); err != nil {
		return &IncompatibleError{Check: "capabilities JSON", Err: err}
	}
	if c.Schema.Name != "spectra.capabilities" {
		return &IncompatibleError{Check: fmt.Sprintf("capabilities schema name %q", c.Schema.Name)}
	}
	if c.Schema.Version != schemaVersion || c.Schema.Version > supportedCapabilitiesSchemaVersion {
		return &IncompatibleError{Check: fmt.Sprintf("capabilities schema version %d, manifest %d, supported max %d", c.Schema.Version, schemaVersion, supportedCapabilitiesSchemaVersion)}
	}
	if c.SpectraVersion != version {
		return &IncompatibleError{Check: fmt.Sprintf("spectra_version %q, expected %q", c.SpectraVersion, version)}
	}
	if c.OS != runtime.GOOS {
		return &IncompatibleError{Check: fmt.Sprintf("os %q, expected %q", c.OS, runtime.GOOS)}
	}
	if c.Arch != runtime.GOARCH {
		return &IncompatibleError{Check: fmt.Sprintf("arch %q, expected %q", c.Arch, runtime.GOARCH)}
	}
	for _, name := range []string{protocolv1.SchemaInspect, protocolv1.SchemaSnapshot} {
		if err := checkInterface(c.Interfaces, name); err != nil {
			return err
		}
	}
	return nil
}

func checkInterface(interfaces []capabilityInterface, schema string) error {
	name := "inspect"
	if schema == protocolv1.SchemaSnapshot {
		name = "snapshot"
	}
	want, _ := protocolv1.SupportedResultSchemaVersion(schema)
	var mismatched *capabilityInterface
	for _, iface := range interfaces {
		if iface.Name != name {
			continue
		}
		if iface.ResultSchema.Name == schema && iface.ResultSchema.Version == want {
			return nil
		}
		mismatched = &iface
	}
	if mismatched != nil {
		return &IncompatibleError{Check: fmt.Sprintf("%s result_schema %q version %d, expected %q version %d", name, mismatched.ResultSchema.Name, mismatched.ResultSchema.Version, schema, want)}
	}
	return &IncompatibleError{Check: fmt.Sprintf("missing %s interface", name)}
}

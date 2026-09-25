package agent

import (
	"fmt"

	protocol "github.com/kaeawc/spectra-protocol/protocol/v1"
)

// SpectraInterface mirrors one installed Spectra CLI interface.
type SpectraInterface struct {
	Name         string              `json:"name"`
	Argv         []string            `json:"argv"`
	Output       string              `json:"output"`
	ResultSchema *protocol.SchemaRef `json:"result_schema,omitempty"`
}

// SpectraCapabilities is the installed Spectra's own capabilities document.
type SpectraCapabilities struct {
	Schema         protocol.SchemaRef `json:"schema"`
	SpectraVersion string             `json:"spectra_version"`
	OS             string             `json:"os"`
	Arch           string             `json:"arch"`
	Interfaces     []SpectraInterface `json:"interfaces"`
}

// Protocol operations and CLI interfaces have deliberately distinct names.
var interfaceForOperation = map[protocol.Operation]string{
	protocol.OperationInspect:        "inspect",
	protocol.OperationSnapshotCreate: "snapshot",
}

func (c SpectraCapabilities) resultSchema(op protocol.Operation) (protocol.SchemaRef, error) {
	interfaceName, ok := interfaceForOperation[op]
	if !ok {
		return protocol.SchemaRef{}, fmt.Errorf("operation %q has no Spectra interface", op)
	}
	expected, _ := protocol.ResultSchemaName(op)
	version, _ := protocol.SupportedResultSchemaVersion(expected)
	for _, iface := range c.Interfaces {
		if iface.Name == interfaceName {
			if iface.ResultSchema == nil || iface.ResultSchema.Name != expected || iface.ResultSchema.Version != version {
				return protocol.SchemaRef{}, fmt.Errorf("interface %q has no supported result schema", interfaceName)
			}
			return *iface.ResultSchema, nil
		}
	}
	return protocol.SchemaRef{}, fmt.Errorf("Spectra interface %q is missing", interfaceName)
}

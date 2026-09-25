package provision

import (
	"context"
	"errors"
	"strings"
	"testing"

	protocolv1 "github.com/kaeawc/spectra-protocol/protocol/v1"
)

func TestIncompatibleCapabilities(t *testing.T) {
	cases := []struct {
		name   string
		change func(*capabilities)
	}{
		{"schema version", func(c *capabilities) { c.Schema.Version = 2 }},
		{"missing inspect", func(c *capabilities) { c.Interfaces = c.Interfaces[1:] }},
		{"wrong Spectra version", func(c *capabilities) { c.SpectraVersion = "v9.0.0" }},
		{"inspect result version", func(c *capabilities) { c.Interfaces[0].ResultSchema.Version = 2 }},
		{"snapshot result version", func(c *capabilities) { c.Interfaces[1].ResultSchema.Version = 2 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, priv := newTestServer(t)
			s.releases["v1.0.0"] = fixture(t, "v1.0.0", priv, nil)
			o := s.opts()
			o.Runner = fakeRunner{change: tc.change}
			_, err := Install(context.Background(), o, "v1.0.0")
			var incompatible *IncompatibleError
			if !errors.As(err, &incompatible) {
				t.Fatalf("expected incompatible error: %v", err)
			}
			if !strings.Contains(err.Error(), strings.Split(tc.name, " ")[0]) && tc.name != "wrong Spectra version" {
				t.Logf("check: %v", err)
			}
			assertNotInstalled(t, s.root)
		})
	}
}

func snapshotOnlyInterfaces() []capabilityInterface {
	snapshotVersion, _ := protocolv1.SupportedResultSchemaVersion(protocolv1.SchemaSnapshot)
	return []capabilityInterface{{Name: "snapshot", ResultSchema: schemaRef{Name: protocolv1.SchemaSnapshot, Version: snapshotVersion}}}
}

func TestCheckInterfacesLinuxWithoutInspectIsCompatible(t *testing.T) {
	// inspect is macOS-only: Spectra core does not advertise it on Linux, and
	// that must not be treated as an incompatibility.
	if err := checkInterfaces(snapshotOnlyInterfaces(), "linux"); err != nil {
		t.Fatalf("linux without inspect: %v", err)
	}
}

func TestCheckInterfacesDarwinWithoutInspectIsIncompatible(t *testing.T) {
	var incompatible *IncompatibleError
	if err := checkInterfaces(snapshotOnlyInterfaces(), "darwin"); !errors.As(err, &incompatible) {
		t.Fatalf("darwin without inspect: expected incompatible error, got %v", err)
	}
}

func TestCheckInterfacesInspectWrongVersionOnLinuxIsIncompatible(t *testing.T) {
	// If inspect IS present, it must still carry the supported version, even
	// on an OS where it isn't required at all.
	interfaces := append(snapshotOnlyInterfaces(), capabilityInterface{Name: "inspect", ResultSchema: schemaRef{Name: protocolv1.SchemaInspect, Version: 2}})
	var incompatible *IncompatibleError
	if err := checkInterfaces(interfaces, "linux"); !errors.As(err, &incompatible) {
		t.Fatalf("linux with mismatched inspect: expected incompatible error, got %v", err)
	}
}

func TestCompatibleInterfaceCanFollowUnsupportedDuplicate(t *testing.T) {
	s, priv := newTestServer(t)
	s.releases["v1.0.0"] = fixture(t, "v1.0.0", priv, nil)
	o := s.opts()
	o.Runner = fakeRunner{change: func(c *capabilities) {
		c.Interfaces = append([]capabilityInterface{{Name: "inspect", ResultSchema: schemaRef{Name: "spectra.inspect", Version: 2}}}, c.Interfaces...)
	}}
	if _, err := Install(context.Background(), o, "v1.0.0"); err != nil {
		t.Fatalf("valid later interface rejected: %v", err)
	}
}

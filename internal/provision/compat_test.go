package provision

import (
	"context"
	"encoding/json"
	"errors"
	"runtime"
	"strings"
	"testing"

	protocolv1 "github.com/kaeawc/spectra-protocol/protocol/v1"
)

type capabilitiesRunner struct {
	caps protocolv1.SpectraCapabilities
}

func (r capabilitiesRunner) Capabilities(context.Context, string) ([]byte, error) {
	return json.Marshal(r.caps)
}

func capabilitiesForOS(goos, version string, inspectVersion int, includeInspect, includeSnapshot bool) protocolv1.SpectraCapabilities {
	caps := protocolv1.SpectraCapabilities{
		Schema:         protocolv1.SchemaRef{Name: protocolv1.SchemaCapabilities, Version: protocolv1.CapabilitiesSchemaVersion},
		SpectraVersion: version,
		OS:             goos,
		Arch:           runtime.GOARCH,
	}
	if includeInspect {
		ref := protocolv1.SchemaRef{Name: protocolv1.SchemaInspect, Version: inspectVersion}
		caps.Interfaces = append(caps.Interfaces, protocolv1.SpectraInterface{Name: protocolv1.InterfaceInspect, Output: protocolv1.OutputJSON, ResultSchema: &ref})
	}
	if includeSnapshot {
		ref := protocolv1.SchemaRef{Name: protocolv1.SchemaSnapshot, Version: 1}
		caps.Interfaces = append(caps.Interfaces, protocolv1.SpectraInterface{Name: protocolv1.InterfaceSnapshot, Output: protocolv1.OutputJSON, ResultSchema: &ref})
	}
	ref := protocolv1.SchemaRef{Name: protocolv1.SchemaCapabilities, Version: 1}
	caps.Interfaces = append(caps.Interfaces, protocolv1.SpectraInterface{Name: protocolv1.InterfaceCapabilities, Output: protocolv1.OutputJSON, ResultSchema: &ref})
	return caps
}

func TestCheckCapabilitiesForOS(t *testing.T) {
	const version = "v1.0.0"
	cases := []struct {
		name      string
		goos      string
		caps      protocolv1.SpectraCapabilities
		schema    int
		wantError bool
	}{
		{name: "linux inspect absent", goos: "linux", caps: capabilitiesForOS("linux", version, 1, false, true)},
		{name: "darwin inspect absent", goos: "darwin", caps: capabilitiesForOS("darwin", version, 1, false, true), wantError: true},
		{name: "linux inspect wrong version", goos: "linux", caps: capabilitiesForOS("linux", version, 2, true, true), wantError: true},
		{name: "darwin inspect wrong version", goos: "darwin", caps: capabilitiesForOS("darwin", version, 2, true, true), wantError: true},
		{name: "darwin snapshot absent", goos: "darwin", caps: capabilitiesForOS("darwin", version, 1, true, false), wantError: true},
		{name: "linux snapshot absent", goos: "linux", caps: capabilitiesForOS("linux", version, 1, false, false), wantError: true},
		{name: "schema version mismatch", goos: "linux", caps: capabilitiesForOS("linux", version, 1, false, true), schema: 2, wantError: true},
		{name: "Spectra version mismatch", goos: "linux", caps: capabilitiesForOS("linux", "v2.0.0", 1, false, true), wantError: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			schema := tc.schema
			if schema == 0 {
				schema = protocolv1.CapabilitiesSchemaVersion
			}
			err := checkCapabilitiesForOS(context.Background(), capabilitiesRunner{caps: tc.caps}, "ignored", version, schema, tc.goos)
			var incompatible *IncompatibleError
			gotError := errors.As(err, &incompatible)
			if tc.wantError && !gotError {
				t.Fatalf("expected IncompatibleError, got %v", err)
			}
			if !tc.wantError && err != nil {
				t.Fatalf("expected compatibility, got %v", err)
			}
		})
	}
}

func TestIncompatibleCapabilities(t *testing.T) {
	cases := []struct {
		name   string
		change func(*protocolv1.SpectraCapabilities)
	}{
		{"schema version", func(c *protocolv1.SpectraCapabilities) { c.Schema.Version = 2 }},
		{"wrong Spectra version", func(c *protocolv1.SpectraCapabilities) { c.SpectraVersion = "v9.0.0" }},
		{"snapshot result version", func(c *protocolv1.SpectraCapabilities) {
			iface, _ := c.Interface(protocolv1.InterfaceSnapshot)
			*iface.ResultSchema = protocolv1.SchemaRef{Name: protocolv1.SchemaSnapshot, Version: 2}
			for i := range c.Interfaces {
				if c.Interfaces[i].Name == iface.Name {
					c.Interfaces[i] = iface
				}
			}
		}},
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

func TestDecodeCapabilitiesFixture(t *testing.T) {
	// inspect is macOS-only: Spectra core does not advertise it on Linux, and
	// that must not be treated as an incompatibility.
	caps := validCapabilitiesFromVersion("v1.0.0")
	if err := caps.Validate(); err != nil {
		t.Fatal(err)
	}
}

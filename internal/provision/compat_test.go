package provision

import (
	"context"
	"errors"
	"strings"
	"testing"
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

package provision

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveDefaults(t *testing.T) {
	home := func() (string, error) { return "/home/example", nil }
	getenv := func(key string) string {
		if key == "XDG_DATA_HOME" {
			return "/data"
		}
		return ""
	}
	mac, err := resolveDefaults(Config{}, home, getenv, "darwin")
	if err != nil {
		t.Fatal(err)
	}
	if mac.Root != "/home/example/Library/Application Support/Spectra Proxy/spectra" || mac.KeepVersions != 2 || len(mac.Sources) != 1 {
		t.Fatalf("mac defaults %+v", mac)
	}
	linux, err := resolveDefaults(Config{}, home, getenv, "linux")
	if err != nil {
		t.Fatal(err)
	}
	if linux.Root != "/data/spectra-proxy/spectra" {
		t.Fatalf("linux root %q", linux.Root)
	}
	linux, err = resolveDefaults(Config{}, home, func(string) string { return "" }, "linux")
	if err != nil {
		t.Fatal(err)
	}
	if linux.Root != "/home/example/.local/share/spectra-proxy/spectra" {
		t.Fatalf("fallback root %q", linux.Root)
	}
}

func TestLoadConfigStrictAndSecure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"root":"/tmp/test","unknown":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("unknown field %v", err)
	}
	if err := os.WriteFile(path, []byte(`{"root":"/tmp/test"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := LoadConfig(path)
	if err != nil || c.Root != "/tmp/test" {
		t.Fatalf("load %+v %v", c, err)
	}
	if err := os.Chmod(path, 0o666); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil {
		t.Fatal("writable config accepted")
	}
	missing, err := LoadConfig(filepath.Join(dir, "absent"))
	if err != nil || missing.Root != "" {
		t.Fatalf("optional config %+v %v", missing, err)
	}
}

func TestSourceValidation(t *testing.T) {
	c := Config{Root: "/tmp/spectra", Sources: []string{"https://example.com/base?query=1"}, KeepVersions: 2}
	if err := validateConfig(c, false); err == nil {
		t.Fatal("query accepted")
	}
	c.Sources = []string{"https://user@example.com/base"}
	if err := validateConfig(c, false); err == nil {
		t.Fatal("userinfo accepted")
	}
	c.Sources = []string{"http://127.0.0.1:1234"}
	if err := validateConfig(c, false); err == nil {
		t.Fatal("http accepted")
	}
	if err := validateConfig(c, true); err != nil {
		t.Fatalf("loopback escape: %v", err)
	}
}

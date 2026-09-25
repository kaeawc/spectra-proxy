package provision

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

type Config struct {
	Root                 string   `json:"root"`
	Sources              []string `json:"sources"`
	TrustedKeys          []string `json:"trusted_keys"`
	AllowedRedirectHosts []string `json:"allowed_redirect_hosts"`
	KeepVersions         int      `json:"keep_versions"`
}

// The Spectra release public key will be added after it is generated and distributed.
var DefaultTrustedKeys []string

func resolveDefaults(c Config, home func() (string, error), getenv func(string) string, goos string) (Config, error) {
	if c.Root == "" {
		h, err := home()
		if err != nil {
			return c, fmt.Errorf("resolve home directory: %w", err)
		}
		if goos == "darwin" {
			c.Root = filepath.Join(h, "Library", "Application Support", "Spectra Proxy", "spectra")
		} else {
			base := getenv("XDG_DATA_HOME")
			if base == "" {
				base = filepath.Join(h, ".local", "share")
			}
			c.Root = filepath.Join(base, "spectra-proxy", "spectra")
		}
	}
	if c.Sources == nil {
		c.Sources = []string{"https://github.com/kaeawc/spectra/releases/download"}
	}
	if c.AllowedRedirectHosts == nil {
		c.AllowedRedirectHosts = []string{"objects.githubusercontent.com", "release-assets.githubusercontent.com"}
	}
	if c.TrustedKeys == nil {
		c.TrustedKeys = append([]string(nil), DefaultTrustedKeys...)
	}
	if c.KeepVersions == 0 {
		c.KeepVersions = 2
	}
	return c, nil
}

func resolveConfig(c Config) (Config, error) {
	return resolveDefaults(c, os.UserHomeDir, os.Getenv, runtime.GOOS)
}

func LoadConfig(path string) (Config, error) {
	if path == "" {
		return Config{}, nil
	}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return Config{}, nil
	}
	if err != nil {
		return Config{}, fmt.Errorf("open config: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return Config{}, fmt.Errorf("stat config: %w", err)
	}
	if err := checkConfigFile(info); err != nil {
		return Config{}, err
	}
	var c Config
	d := json.NewDecoder(f)
	d.DisallowUnknownFields()
	if err := d.Decode(&c); err != nil {
		return Config{}, fmt.Errorf("decode config: %w", err)
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return Config{}, fmt.Errorf("config has trailing data: %v", err)
	}
	return c, nil
}

func validateConfig(c Config, allowLoopbackHTTP bool) error {
	if !filepath.IsAbs(c.Root) || c.Root == string(filepath.Separator) {
		return fmt.Errorf("root must be an absolute non-root directory")
	}
	if c.KeepVersions < 1 {
		return fmt.Errorf("keep_versions must be positive")
	}
	if len(c.Sources) == 0 {
		return fmt.Errorf("at least one source is required")
	}
	for _, source := range c.Sources {
		if err := validateSource(source, allowLoopbackHTTP); err != nil {
			return err
		}
	}
	for _, host := range c.AllowedRedirectHosts {
		if host == "" || strings.ContainsAny(host, "/@?#") {
			return fmt.Errorf("invalid allowed redirect host %q", host)
		}
	}
	return nil
}

func validateSource(source string, allowLoopbackHTTP bool) error {
	u, err := url.Parse(source)
	if err != nil {
		return fmt.Errorf("source %q: %w", source, err)
	}
	loopback := allowLoopbackHTTP && u.Scheme == "http" && isLoopbackHost(u.Hostname())
	if (u.Scheme != "https" && !loopback) || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return fmt.Errorf("source %q must be HTTPS with a host and no userinfo, query, or fragment", source)
	}
	return nil
}

func isLoopbackHost(host string) bool {
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback() || strings.EqualFold(host, "localhost")
}

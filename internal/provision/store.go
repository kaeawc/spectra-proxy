package provision

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	release "github.com/kaeawc/spectra-protocol/release/v1"
)

const stateSchema = "spectra-proxy.provision/1"

type VersionInfo struct {
	SHA256      string    `json:"sha256"`
	InstalledAt time.Time `json:"installed_at"`
}
type state struct {
	Schema   string                 `json:"schema"`
	Current  string                 `json:"current"`
	Previous string                 `json:"previous"`
	Versions map[string]VersionInfo `json:"versions"`
}

func emptyState() state             { return state{Schema: stateSchema, Versions: make(map[string]VersionInfo)} }
func BinaryPath(root string) string { return filepath.Join(root, "current", "spectra") }
func versionBinary(root, version string) string {
	return filepath.Join(root, "versions", version, "spectra")
}

func readState(root string) (state, error) {
	data, err := os.ReadFile(filepath.Join(root, "state.json"))
	if errors.Is(err, os.ErrNotExist) {
		return emptyState(), nil
	}
	if err != nil {
		return state{}, fmt.Errorf("read state: %w", err)
	}
	var s state
	if err := json.Unmarshal(data, &s); err != nil {
		return state{}, fmt.Errorf("parse state: %w", err)
	}
	if s.Schema != stateSchema {
		return state{}, fmt.Errorf("unrecognized provisioning state schema %q", s.Schema)
	}
	if s.Versions == nil {
		s.Versions = make(map[string]VersionInfo)
	}
	for _, version := range []string{s.Current, s.Previous} {
		if version == "" {
			continue
		}
		if _, err := release.ParseVersion(version); err != nil {
			return state{}, fmt.Errorf("invalid state version: %w", err)
		}
		if _, ok := s.Versions[version]; !ok {
			return state{}, fmt.Errorf("state version %s is missing metadata", version)
		}
	}
	for version := range s.Versions {
		if _, err := release.ParseVersion(version); err != nil {
			return state{}, fmt.Errorf("invalid retained state version: %w", err)
		}
	}
	return s, nil
}

func syncDir(path string) error {
	d, err := os.Open(path)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func writeState(root string, s state) error {
	data, err := json.Marshal(s)
	if err != nil {
		return fmt.Errorf("encode state: %w", err)
	}
	f, err := os.CreateTemp(root, "state-*")
	if err != nil {
		return fmt.Errorf("create state temp file: %w", err)
	}
	defer os.Remove(f.Name())
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return fmt.Errorf("chmod state: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return fmt.Errorf("write state: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("sync state: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close state: %w", err)
	}
	if err := os.Rename(f.Name(), filepath.Join(root, "state.json")); err != nil {
		return fmt.Errorf("rename state: %w", err)
	}
	if err := syncDir(root); err != nil {
		return fmt.Errorf("sync root after state: %w", err)
	}
	return nil
}

func activate(root, version string) error {
	next := filepath.Join(root, "current.new")
	_ = os.Remove(next)
	if err := os.Symlink(filepath.Join("versions", version), next); err != nil {
		return fmt.Errorf("create current symlink: %w", err)
	}
	if err := os.Rename(next, filepath.Join(root, "current")); err != nil {
		_ = os.Remove(next)
		return fmt.Errorf("activate current symlink: %w", err)
	}
	if err := syncDir(root); err != nil {
		return fmt.Errorf("sync root after activation: %w", err)
	}
	return nil
}

func digestFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open binary for digest: %w", err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("hash binary: %w", err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func prepareRoot(root string) (func(), error) {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("create provisioning root: %w", err)
	}
	unlock, err := lock(root)
	if err != nil {
		return nil, err
	}
	staging := filepath.Join(root, "staging")
	if err := os.RemoveAll(staging); err != nil {
		unlock()
		return nil, fmt.Errorf("clear staging: %w", err)
	}
	if err := os.Mkdir(staging, 0o700); err != nil {
		unlock()
		return nil, fmt.Errorf("create staging: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "versions"), 0o700); err != nil {
		unlock()
		return nil, fmt.Errorf("create versions: %w", err)
	}
	return unlock, nil
}

func planPrune(s *state, keep int) []string {
	if keep < 1 {
		keep = 1
	}
	wanted := map[string]bool{s.Current: true}
	if keep > 1 && s.Previous != "" {
		wanted[s.Previous] = true
	}
	type entry struct {
		name string
		at   time.Time
	}
	var extras []entry
	for name, info := range s.Versions {
		if !wanted[name] {
			extras = append(extras, entry{name, info.InstalledAt})
		}
	}
	sort.Slice(extras, func(i, j int) bool {
		if extras[i].at.Equal(extras[j].at) {
			return extras[i].name > extras[j].name
		}
		return extras[i].at.After(extras[j].at)
	})
	for i, e := range extras {
		if i < keep-len(wanted) {
			wanted[e.name] = true
		}
	}
	var removed []string
	for name := range s.Versions {
		if wanted[name] {
			continue
		}
		removed = append(removed, name)
		delete(s.Versions, name)
	}
	if keep == 1 {
		s.Previous = ""
	}
	return removed
}

func removePruned(root string, versions []string) error {
	for _, name := range versions {
		if err := os.RemoveAll(filepath.Join(root, "versions", name)); err != nil {
			return fmt.Errorf("prune version %s: %w", name, err)
		}
	}
	if len(versions) > 0 {
		if err := syncDir(filepath.Join(root, "versions")); err != nil {
			return fmt.Errorf("sync pruned versions: %w", err)
		}
	}
	return nil
}

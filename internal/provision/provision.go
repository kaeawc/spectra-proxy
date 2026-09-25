package provision

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	release "github.com/kaeawc/spectra-protocol/release/v1"
)

type Options struct {
	Config            Config
	Client            *http.Client
	Runner            Runner
	Now               func() time.Time
	Logf              func(string, ...any)
	beforeCommit      func() error
	allowLoopbackHTTP bool
	binaryLimit       int64
}

type Result struct {
	Version     string    `json:"version"`
	Previous    string    `json:"previous"`
	InstalledAt time.Time `json:"installed_at"`
}
type StatusReport struct {
	Current              string                 `json:"current"`
	Previous             string                 `json:"previous"`
	Versions             map[string]VersionInfo `json:"versions"`
	BinaryPath           string                 `json:"binary_path"`
	CurrentDriftDetected bool                   `json:"current_drift_detected"`
}

func (o Options) normalized() (Options, error) {
	c, err := resolveConfig(o.Config)
	if err != nil {
		return o, err
	}
	if err := validateConfig(c, o.allowLoopbackHTTP); err != nil {
		return o, err
	}
	o.Config = c
	if o.Runner == nil {
		o.Runner = commandRunner{}
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.binaryLimit == 0 {
		o.binaryLimit = maxBinaryBytes
	}
	return o, nil
}

func Install(ctx context.Context, opts Options, version string) (Result, error) {
	return install(ctx, opts, version, false)
}
func Update(ctx context.Context, opts Options, version string) (Result, error) {
	return install(ctx, opts, version, true)
}

func install(ctx context.Context, opts Options, version string, update bool) (Result, error) {
	o, err := opts.normalized()
	if err != nil {
		return Result{}, err
	}
	if _, err := release.ParseVersion(version); err != nil {
		return Result{}, fmt.Errorf("requested version: %w", err)
	}
	unlock, err := prepareRoot(o.Config.Root)
	if err != nil {
		return Result{}, err
	}
	defer unlock()
	s, err := readState(o.Config.Root)
	if err != nil {
		return Result{}, err
	}
	if err := checkVersionChange(s.Current, version, update); err != nil {
		return Result{}, err
	}
	got, err := fetchRelease(ctx, o, o.Config, version)
	if err != nil {
		return Result{}, err
	}
	stage := filepath.Join(o.Config.Root, "staging")
	binary, err := extractBinary(got.archive, stage, got.artifact, o.binaryLimit)
	if err != nil {
		return Result{}, fmt.Errorf("extract artifact: %w", err)
	}
	if err := checkCapabilities(ctx, o.Runner, binary, version, got.manifest.CapabilitiesSchemaVersion); err != nil {
		return Result{}, err
	}
	digest, err := digestFile(binary)
	if err != nil {
		return Result{}, err
	}
	if o.beforeCommit != nil {
		if err := o.beforeCommit(); err != nil {
			return Result{}, fmt.Errorf("before commit: %w", err)
		}
	}
	return commitInstall(o, s, version, binary, digest)
}

func checkVersionChange(current, target string, update bool) error {
	if current == "" {
		return nil
	}
	cmp, err := release.CompareVersions(target, current)
	if err != nil {
		return fmt.Errorf("compare installed version: %w", err)
	}
	if update && cmp <= 0 {
		return fmt.Errorf("update requires a version newer than %s; use install or rollback instead", current)
	}
	if cmp < 0 {
		return fmt.Errorf("install would downgrade from %s to %s; use rollback instead", current, target)
	}
	return nil
}

func commitInstall(o Options, s state, version, stagedBinary, digest string) (Result, error) {
	root := o.Config.Root
	target := filepath.Join(root, "versions", version)
	backup := filepath.Join(root, "staging", "replaced-version")
	if err := os.Mkdir(filepath.Join(root, "staging", "candidate"), 0o700); err != nil {
		return Result{}, fmt.Errorf("create candidate: %w", err)
	}
	candidate := filepath.Join(root, "staging", "candidate")
	if err := os.Rename(stagedBinary, filepath.Join(candidate, "spectra")); err != nil {
		return Result{}, fmt.Errorf("stage candidate: %w", err)
	}
	existed := false
	if _, err := os.Lstat(target); err == nil {
		existed = true
		if err := os.Rename(target, backup); err != nil {
			return Result{}, fmt.Errorf("back up existing version: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return Result{}, fmt.Errorf("inspect version directory: %w", err)
	}
	if err := os.Rename(candidate, target); err != nil {
		if existed {
			_ = os.Rename(backup, target)
		}
		return Result{}, fmt.Errorf("install version directory: %w", err)
	}
	if err := syncDir(filepath.Join(root, "versions")); err != nil {
		return Result{}, fmt.Errorf("sync versions directory: %w", err)
	}
	old := s.Current
	if err := activate(root, version); err != nil {
		return Result{}, err
	}
	if old != "" && old != version {
		s.Previous = old
	}
	s.Current = version
	installed := o.Now().UTC()
	s.Versions[version] = VersionInfo{SHA256: digest, InstalledAt: installed}
	removed := planPrune(&s, o.Config.KeepVersions)
	if err := writeState(root, s); err != nil {
		return Result{}, err
	}
	if err := removePruned(root, removed); err != nil {
		return Result{}, err
	}
	_ = os.RemoveAll(backup)
	if o.Logf != nil {
		o.Logf("installed Spectra %s", version)
	}
	return Result{Version: version, Previous: s.Previous, InstalledAt: installed}, nil
}

func Rollback(ctx context.Context, opts Options) (Result, error) {
	o, err := opts.normalized()
	if err != nil {
		return Result{}, err
	}
	// Rollback never downloads anything and never checks a signature: it
	// switches back to a previously installed binary already on disk, after
	// re-verifying its recorded sha256 and re-running the compatibility
	// check below. Trusted release keys guard fetch's signature verification
	// and are irrelevant here.
	unlock, err := prepareRoot(o.Config.Root)
	if err != nil {
		return Result{}, err
	}
	defer unlock()
	s, err := readState(o.Config.Root)
	if err != nil {
		return Result{}, err
	}
	if s.Previous == "" {
		return Result{}, fmt.Errorf("no previous Spectra version available for rollback")
	}
	info, ok := s.Versions[s.Previous]
	if !ok {
		return Result{}, fmt.Errorf("rollback target %s is absent from state", s.Previous)
	}
	binary := versionBinary(o.Config.Root, s.Previous)
	digest, err := digestFile(binary)
	if err != nil {
		return Result{}, fmt.Errorf("verify rollback target: %w", err)
	}
	if digest != info.SHA256 {
		return Result{}, fmt.Errorf("rollback target %s has been tampered with: sha256 mismatch", s.Previous)
	}
	if err := checkCapabilities(ctx, o.Runner, binary, s.Previous, supportedCapabilitiesSchemaVersion); err != nil {
		return Result{}, fmt.Errorf("rollback target: %w", err)
	}
	if o.beforeCommit != nil {
		if err := o.beforeCommit(); err != nil {
			return Result{}, fmt.Errorf("before commit: %w", err)
		}
	}
	s.Current, s.Previous = s.Previous, s.Current
	if err := activate(o.Config.Root, s.Current); err != nil {
		return Result{}, err
	}
	if err := writeState(o.Config.Root, s); err != nil {
		return Result{}, err
	}
	return Result{Version: s.Current, Previous: s.Previous, InstalledAt: info.InstalledAt}, nil
}

func Status(opts Options) (StatusReport, error) {
	o, err := opts.normalized()
	if err != nil {
		return StatusReport{}, err
	}
	s, err := readState(o.Config.Root)
	if err != nil {
		return StatusReport{}, err
	}
	r := StatusReport{Current: s.Current, Previous: s.Previous, Versions: s.Versions, BinaryPath: BinaryPath(o.Config.Root)}
	if s.Current != "" {
		digest, err := digestFile(r.BinaryPath)
		info, ok := s.Versions[s.Current]
		r.CurrentDriftDetected = err != nil || !ok || digest != info.SHA256
	}
	return r, nil
}

func Uninstall(ctx context.Context, opts Options) error {
	_ = ctx
	o, err := opts.normalized()
	if err != nil {
		return err
	}
	// Validate before any mutation, even lock-file creation or staging cleanup.
	if _, err := os.Stat(filepath.Join(o.Config.Root, "state.json")); err != nil {
		return fmt.Errorf("refuse uninstall without provisioning state: %w", err)
	}
	if _, err := readState(o.Config.Root); err != nil {
		return fmt.Errorf("refuse uninstall: %w", err)
	}
	unlock, err := prepareRoot(o.Config.Root)
	if err != nil {
		return err
	}
	defer unlock()
	if _, err := readState(o.Config.Root); err != nil {
		return fmt.Errorf("refuse uninstall: %w", err)
	}
	for _, name := range []string{"current", "versions", "staging", "state.json"} {
		if err := os.RemoveAll(filepath.Join(o.Config.Root, name)); err != nil {
			return fmt.Errorf("remove %s: %w", name, err)
		}
	}
	if err := os.Remove(filepath.Join(o.Config.Root, ".lock")); err != nil {
		return fmt.Errorf("remove lock file: %w", err)
	}
	if err := os.Remove(o.Config.Root); err != nil {
		return fmt.Errorf("remove provisioning root: %w", err)
	}
	return nil
}

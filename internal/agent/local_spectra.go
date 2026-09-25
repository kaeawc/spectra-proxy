package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	protocol "github.com/kaeawc/spectra-protocol/protocol/v1"
)

const maxCommandOutput = 8 << 20

type LocalSpectra struct {
	Path            string
	AllowedAppRoots []string
}

func (s LocalSpectra) Capabilities(ctx context.Context) (SpectraCapabilities, error) {
	out, err := s.runJSON(ctx, "capabilities", "--json")
	if err != nil {
		return SpectraCapabilities{}, fmt.Errorf("run Spectra capabilities: %w", err)
	}
	var caps SpectraCapabilities
	if err := json.Unmarshal(out, &caps); err != nil {
		return SpectraCapabilities{}, fmt.Errorf("decode Spectra capabilities: %w", err)
	}
	if caps.Schema.Name != "spectra.capabilities" || caps.Schema.Version != 1 {
		return SpectraCapabilities{}, fmt.Errorf("unsupported Spectra capabilities schema %q version %d", caps.Schema.Name, caps.Schema.Version)
	}
	return caps, nil
}

// BinaryIdentity implements the Agent's optional binaryIdentifier capability
// so a cached capabilities probe can be detected as stale after `provision
// update` swaps the `current` symlink to a new binary. It resolves through
// symlinks and combines the resolved path with size and modification time,
// which is cheap (no exec) and changes whenever the installed binary does.
func (s LocalSpectra) BinaryIdentity() (string, error) {
	resolved, err := filepath.EvalSymlinks(s.Path)
	if err != nil {
		return "", fmt.Errorf("resolve spectra binary path %q: %w", s.Path, err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("stat spectra binary %q: %w", resolved, err)
	}
	return fmt.Sprintf("%s|%d|%d", resolved, info.Size(), info.ModTime().UnixNano()), nil
}

func (s LocalSpectra) Inspect(ctx context.Context, params protocol.InspectParams) (json.RawMessage, error) {
	args := []string{"--json"}
	for _, path := range params.AppPaths {
		resolved, err := s.validateAppPath(path)
		if err != nil {
			return nil, err
		}
		args = append(args, resolved)
	}
	return s.runJSON(ctx, args...)
}

func (s LocalSpectra) SnapshotCreate(ctx context.Context, params protocol.SnapshotCreateParams) (json.RawMessage, error) {
	args := []string{"snapshot", "--json"}
	if !params.IncludeApps {
		args = append(args, "--no-apps")
	}
	return s.runJSON(ctx, args...)
}

func (s LocalSpectra) validateAppPath(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("app path must be absolute: %q", path)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("resolve app path %q: %w", path, err)
	}
	if filepath.Ext(resolved) != ".app" {
		return "", fmt.Errorf("resolved app path must end in .app: %q", resolved)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("stat app path %q: %w", resolved, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("app path is not a directory: %q", resolved)
	}
	for _, root := range s.AllowedAppRoots {
		allowed, err := filepath.EvalSymlinks(root)
		if err != nil {
			continue
		}
		rel, err := filepath.Rel(allowed, resolved)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			// Spectra may still see a replacement between this check and its own open (TOCTOU).
			return resolved, nil
		}
	}
	return "", fmt.Errorf("app path is outside configured allowed roots: %q", path)
}

func (s LocalSpectra) runJSON(ctx context.Context, args ...string) (json.RawMessage, error) {
	out, err := s.run(ctx, args...)
	if err != nil {
		return nil, err
	}
	if !json.Valid(out) {
		return nil, fmt.Errorf("local spectra returned invalid JSON")
	}
	return out, nil
}

func (s LocalSpectra) run(ctx context.Context, args ...string) ([]byte, error) {
	if !filepath.IsAbs(s.Path) {
		return nil, fmt.Errorf("spectra executable path must be absolute")
	}
	stdout, stderr := &limitedBuffer{limit: maxCommandOutput}, &limitedBuffer{limit: maxCommandOutput}
	cmd := exec.CommandContext(ctx, s.Path, args...)
	configureProcessGroup(cmd)
	cmd.WaitDelay = 2 * time.Second
	cmd.Stdout, cmd.Stderr = stdout, stderr
	err := cmd.Run()
	if stdout.exceeded || stderr.exceeded {
		return nil, fmt.Errorf("local spectra output exceeded %d bytes", maxCommandOutput)
	}
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("local spectra canceled: %w", ctx.Err())
		}
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			return nil, fmt.Errorf("run local spectra: %w", err)
		}
		return nil, fmt.Errorf("run local spectra: %w: %s", err, message)
	}
	return stdout.Bytes(), nil
}

type limitedBuffer struct {
	bytes.Buffer
	limit    int
	exceeded bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > b.limit {
		remaining := b.limit - b.Len()
		if remaining > 0 {
			_, _ = b.Buffer.Write(p[:remaining])
		}
		b.exceeded = true
		return len(p), nil
	}
	return b.Buffer.Write(p)
}

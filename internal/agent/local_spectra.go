package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	protocol "github.com/kaeawc/spectra-protocol/protocol/v1"
)

const maxCommandOutput = 8 << 20

// LocalSpectra is a typed adapter around one locally installed Spectra
// executable. It never evaluates a caller-supplied program or argument list.
type LocalSpectra struct {
	Path            string
	AllowedAppRoots []string
}

func (s LocalSpectra) Version(ctx context.Context) (string, error) {
	out, err := s.run(ctx, "version")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func (s LocalSpectra) Inspect(ctx context.Context, params protocol.InspectParams) (json.RawMessage, error) {
	args := []string{"--json"}
	for _, path := range params.AppPaths {
		if err := s.validateAppPath(path); err != nil {
			return nil, err
		}
		args = append(args, path)
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

func (s LocalSpectra) validateAppPath(path string) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("app path must be absolute: %q", path)
	}
	clean := filepath.Clean(path)
	if filepath.Ext(clean) != ".app" {
		return fmt.Errorf("app path must end in .app: %q", path)
	}
	for _, root := range s.AllowedAppRoots {
		root = filepath.Clean(root)
		if clean == root || strings.HasPrefix(clean, root+string(filepath.Separator)) {
			return nil
		}
	}
	return fmt.Errorf("app path is outside configured allowed roots: %q", path)
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
	stdout := &limitedBuffer{limit: maxCommandOutput}
	stderr := &limitedBuffer{limit: maxCommandOutput}
	cmd := exec.CommandContext(ctx, s.Path, args...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	err := cmd.Run()
	if stdout.exceeded || stderr.exceeded {
		return nil, fmt.Errorf("local spectra output exceeded %d bytes", maxCommandOutput)
	}
	if err != nil {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = err.Error()
		}
		return nil, fmt.Errorf("local spectra: %s", message)
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

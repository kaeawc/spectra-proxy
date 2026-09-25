package provision

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	release "github.com/kaeawc/spectra-protocol/release/v1"
)

const maxBinaryBytes int64 = 256 << 20
const maxExtractedBytes int64 = 1 << 30

func extractBinary(archive, stage string, artifact release.Artifact, binaryLimit int64) (string, error) {
	f, err := os.Open(archive)
	if err != nil {
		return "", fmt.Errorf("open archive: %w", err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return "", fmt.Errorf("decompress archive: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	top := strings.TrimSuffix(artifact.Path, ".tar.gz")
	want := top + "/bin/spectra"
	var total int64
	var found bool
	binary := filepath.Join(stage, "spectra")
	for {
		h, nextErr := tr.Next()
		if nextErr == io.EOF {
			break
		}
		if nextErr != nil {
			return "", fmt.Errorf("read archive: %w", nextErr)
		}
		if err := consumeEntry(h, tr, top, want, binary, binaryLimit, &total, &found); err != nil {
			return "", err
		}
	}
	if !found {
		return "", fmt.Errorf("archive is missing %s", want)
	}
	return binary, nil
}

func consumeEntry(h *tar.Header, tr *tar.Reader, top, want, binary string, binaryLimit int64, total *int64, found *bool) error {
	name := strings.TrimSuffix(h.Name, "/")
	if err := validateEntry(name, top, h.Typeflag); err != nil {
		return err
	}
	if h.Size < 0 || h.Size > maxExtractedBytes-*total {
		return fmt.Errorf("archive exceeds 1 GiB uncompressed limit")
	}
	*total += h.Size
	if name == want && h.Typeflag != tar.TypeDir {
		if *found {
			return fmt.Errorf("archive contains multiple spectra binaries")
		}
		*found = true
		if h.Size > binaryLimit {
			return fmt.Errorf("spectra binary exceeds 256 MiB limit")
		}
		return writeStagedBinary(binary, tr)
	}
	if _, err := io.Copy(io.Discard, tr); err != nil {
		return fmt.Errorf("discard archive entry: %w", err)
	}
	return nil
}

func validateEntry(name, top string, kind byte) error {
	if name == "" || path.IsAbs(name) || strings.HasPrefix(name, "/") {
		return fmt.Errorf("unsafe absolute or empty archive entry %q", name)
	}
	for _, part := range strings.Split(name, "/") {
		if part == ".." {
			return fmt.Errorf("archive entry contains ..: %q", name)
		}
	}
	if name != top && !strings.HasPrefix(name, top+"/") {
		return fmt.Errorf("archive entry outside expected top-level directory: %q", name)
	}
	if kind != tar.TypeReg && kind != tar.TypeRegA && kind != tar.TypeDir {
		return fmt.Errorf("unsupported archive entry type for %q", name)
	}
	return nil
}

func writeStagedBinary(target string, src io.Reader) error {
	f, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o755)
	if err != nil {
		return fmt.Errorf("create staged binary: %w", err)
	}
	if err := f.Chmod(0o755); err != nil {
		f.Close()
		return fmt.Errorf("chmod staged binary: %w", err)
	}
	if _, err := io.Copy(f, src); err != nil {
		f.Close()
		return fmt.Errorf("write staged binary: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("sync staged binary: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close staged binary: %w", err)
	}
	d, err := os.Open(filepath.Dir(target))
	if err != nil {
		return fmt.Errorf("open staging directory: %w", err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("sync staging directory: %w", err)
	}
	return nil
}

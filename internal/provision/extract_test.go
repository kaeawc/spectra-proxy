package provision

import (
	"archive/tar"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUnsafeArchivesRejectWholeInstall(t *testing.T) {
	cases := []struct {
		name    string
		entries func(string) []archiveEntry
	}{
		{"symlink", func(top string) []archiveEntry {
			return []archiveEntry{{name: top + "/bin/spectra", kind: tar.TypeReg, body: "v1.0.0"}, {name: top + "/bad", kind: tar.TypeSymlink, link: "/etc/passwd"}}
		}},
		{"hardlink", func(top string) []archiveEntry {
			return []archiveEntry{{name: top + "/bin/spectra", kind: tar.TypeReg, body: "v1.0.0"}, {name: top + "/bad", kind: tar.TypeLink, link: "other"}}
		}},
		{"dotdot", func(top string) []archiveEntry {
			return []archiveEntry{{name: top + "/bin/spectra", kind: tar.TypeReg, body: "v1.0.0"}, {name: top + "/../bad", kind: tar.TypeReg, body: "x"}}
		}},
		{"absolute", func(top string) []archiveEntry {
			return []archiveEntry{{name: top + "/bin/spectra", kind: tar.TypeReg, body: "v1.0.0"}, {name: "/tmp/bad", kind: tar.TypeReg, body: "x"}}
		}},
		{"duplicate", func(top string) []archiveEntry {
			return []archiveEntry{{name: top + "/bin/spectra", kind: tar.TypeReg, body: "v1.0.0"}, {name: top + "/bin/spectra", kind: tar.TypeReg, body: "v1.0.0"}}
		}},
		{"missing", func(top string) []archiveEntry {
			return []archiveEntry{{name: top + "/README", kind: tar.TypeReg, body: "x"}}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, priv := newTestServer(t)
			f := fixture(t, "v1.0.0", priv, nil)
			top := strings.TrimSuffix(f.artifact.Path, ".tar.gz")
			s.releases["v1.0.0"] = fixture(t, "v1.0.0", priv, tc.entries(top))
			if _, err := Install(context.Background(), s.opts(), "v1.0.0"); err == nil {
				t.Fatal("unsafe archive installed")
			}
			assertNotInstalled(t, s.root)
		})
	}
}

func TestBinarySizeCap(t *testing.T) {
	s, priv := newTestServer(t)
	s.releases["v1.0.0"] = fixture(t, "v1.0.0", priv, nil)
	o := s.opts()
	o.binaryLimit = 3
	if _, err := Install(context.Background(), o, "v1.0.0"); err == nil || !strings.Contains(err.Error(), "256 MiB") {
		t.Fatalf("binary limit: %v", err)
	}
	assertNotInstalled(t, s.root)
}

func TestExtractWritesExecutableMode(t *testing.T) {
	s, priv := newTestServer(t)
	s.releases["v1.0.0"] = fixture(t, "v1.0.0", priv, nil)
	if _, err := Install(context.Background(), s.opts(), "v1.0.0"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(s.root, "versions", "v1.0.0", "spectra"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("mode %v", info.Mode())
	}
}

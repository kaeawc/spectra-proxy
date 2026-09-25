package provision

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"runtime"
	"strings"
	"time"

	release "github.com/kaeawc/spectra-protocol/release/v1"
)

const manifestName = "spectra-release.json"
const signatureName = "spectra-release.json.sig"

var safeFetchName = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
var errRedirectPolicy = errors.New("release redirect rejected")

type fetched struct {
	manifest release.Manifest
	artifact release.Artifact
	archive  string
}

type transportFailure struct{ err error }

func (e *transportFailure) Error() string { return e.err.Error() }
func (e *transportFailure) Unwrap() error { return e.err }

func fetchRelease(ctx context.Context, opts Options, c Config, version string) (fetched, error) {
	if _, err := release.ParseVersion(version); err != nil {
		return fetched{}, fmt.Errorf("requested version: %w", err)
	}
	if len(c.TrustedKeys) == 0 {
		return fetched{}, fmt.Errorf("no trusted Spectra release keys configured; set trusted_keys or --trusted-key")
	}
	keys := make([]release.TrustedKey, 0, len(c.TrustedKeys))
	for _, raw := range c.TrustedKeys {
		key, err := release.ParsePublicKey(raw)
		if err != nil {
			return fetched{}, fmt.Errorf("trusted release key: %w", err)
		}
		keys = append(keys, key)
	}
	var failures []string
	for _, source := range c.Sources {
		result, err := fetchSource(ctx, opts, c, source, version, keys)
		if err == nil {
			return result, nil
		}
		var transport *transportFailure
		if !errors.As(err, &transport) {
			return fetched{}, fmt.Errorf("source %s: %w", source, err)
		}
		failures = append(failures, fmt.Sprintf("%s: %v", source, err))
	}
	return fetched{}, fmt.Errorf("all release sources failed: %s", strings.Join(failures, "; "))
}

func fetchSource(ctx context.Context, opts Options, c Config, source, version string, keys []release.TrustedKey) (fetched, error) {
	client := sourceClient(opts, c, source)
	manifestBytes, err := fetchSmall(ctx, client, source, version, manifestName)
	if err != nil {
		return fetched{}, fmt.Errorf("manifest: %w", err)
	}
	sigBytes, err := fetchSmall(ctx, client, source, version, signatureName)
	if err != nil {
		return fetched{}, fmt.Errorf("signature: %w", err)
	}
	manifest, err := release.Verify(manifestBytes, sigBytes, keys)
	if err != nil {
		return fetched{}, fmt.Errorf("verify release metadata: %w", err)
	}
	if manifest.Version != version {
		return fetched{}, fmt.Errorf("manifest version %q does not match requested version %q", manifest.Version, version)
	}
	artifact, err := manifest.ArtifactFor(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return fetched{}, fmt.Errorf("select artifact: %w", err)
	}
	archive, err := fetchArtifact(ctx, client, c.Root, source, version, artifact)
	if err != nil {
		return fetched{}, err
	}
	return fetched{manifest, artifact, archive}, nil
}

func sourceClient(opts Options, c Config, source string) *http.Client {
	client := &http.Client{Timeout: 30 * time.Second}
	if opts.Client != nil {
		*client = *opts.Client
		if client.Timeout == 0 {
			client.Timeout = 30 * time.Second
		}
	}
	origin, _ := url.Parse(source)
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) > 5 {
			return fmt.Errorf("too many release redirects: %w", errRedirectPolicy)
		}
		loopback := opts.allowLoopbackHTTP && req.URL.Scheme == "http" && isLoopbackHost(req.URL.Hostname())
		if req.URL.Scheme != "https" && !loopback {
			return fmt.Errorf("release redirect must use HTTPS: %w", errRedirectPolicy)
		}
		if strings.EqualFold(req.URL.Host, origin.Host) {
			return nil
		}
		for _, host := range c.AllowedRedirectHosts {
			if strings.EqualFold(req.URL.Host, host) || strings.EqualFold(req.URL.Hostname(), host) {
				return nil
			}
		}
		return fmt.Errorf("release redirect to disallowed host %q: %w", req.URL.Host, errRedirectPolicy)
	}
	return client
}

func releaseURL(source, version, name string) (string, error) {
	if !safeFetchName.MatchString(name) || strings.HasPrefix(name, ".") || name == ".." {
		return "", fmt.Errorf("unsafe release file name %q", name)
	}
	u, err := url.Parse(source)
	if err != nil {
		return "", fmt.Errorf("source URL: %w", err)
	}
	u.Path = path.Join(u.Path, version, name)
	return u.String(), nil
}

func get(ctx context.Context, client *http.Client, source, version, name string) (io.ReadCloser, error) {
	address, err := releaseURL(source, version, name)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, fmt.Errorf("create release request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		// Redirect policy failures are security failures and must not fall through.
		if errors.Is(err, errRedirectPolicy) {
			return nil, fmt.Errorf("request %s: %w", name, err)
		}
		return nil, &transportFailure{fmt.Errorf("request %s: %w", name, err)}
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		err := fmt.Errorf("request %s: HTTP %d", name, resp.StatusCode)
		if resp.StatusCode == http.StatusNotFound || resp.StatusCode >= 500 {
			return nil, &transportFailure{err}
		}
		return nil, err
	}
	return resp.Body, nil
}

func fetchSmall(ctx context.Context, client *http.Client, source, version, name string) ([]byte, error) {
	body, err := get(ctx, client, source, version, name)
	if err != nil {
		return nil, err
	}
	defer body.Close()
	data, err := io.ReadAll(io.LimitReader(body, release.MaxManifestBytes+1))
	if err != nil {
		return nil, &transportFailure{fmt.Errorf("read %s: %w", name, err)}
	}
	if len(data) > release.MaxManifestBytes {
		return nil, fmt.Errorf("%s exceeds release metadata size limit", name)
	}
	return data, nil
}

func fetchArtifact(ctx context.Context, client *http.Client, root, source, version string, artifact release.Artifact) (string, error) {
	body, err := get(ctx, client, source, version, artifact.Path)
	if err != nil {
		return "", fmt.Errorf("artifact: %w", err)
	}
	defer body.Close()
	f, err := os.CreateTemp(path.Join(root, "staging"), "archive-*")
	if err != nil {
		return "", fmt.Errorf("create staged archive: %w", err)
	}
	defer f.Close()
	defer func() {
		if err != nil {
			_ = os.Remove(f.Name())
		}
	}()
	n, copyErr := io.Copy(f, io.LimitReader(body, artifact.Size+1))
	if copyErr != nil {
		err = &transportFailure{fmt.Errorf("read artifact: %w", copyErr)}
		return "", err
	}
	if n > artifact.Size {
		err = fmt.Errorf("artifact exceeds declared size: %w", release.ErrArtifactMismatch)
		return "", err
	}
	if _, err = f.Seek(0, io.SeekStart); err != nil {
		return "", fmt.Errorf("rewind artifact: %w", err)
	}
	if err = release.VerifyArtifactDigest(f, artifact); err != nil {
		return "", fmt.Errorf("verify artifact: %w", err)
	}
	return f.Name(), nil
}

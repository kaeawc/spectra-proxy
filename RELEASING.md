# Releasing spectra-proxy

Before releasing:

- Ensure `main` is green in CI.
- Remove all `replace` directives from `go.mod`; verify with `make release-check`.
- Pin `github.com/kaeawc/spectra-protocol` to a tagged release. Do not use a
  `replace` directive or a pseudo-version.

Create and push an annotated semantic version tag:

```sh
git tag -a vX.Y.Z -m "spectra-proxy vX.Y.Z"
git push origin vX.Y.Z
```

Build `spectra-remote` and `spectra-remote-agent` with
`-trimpath -ldflags "-s -w -X main.version=vX.Y.Z"` for these targets:

- `darwin/arm64`
- `darwin/amd64`
- `linux/amd64`
- `linux/arm64`

Publish a `SHA256SUMS` file alongside the binaries. Never move a published
tag.

Verified provisioning of Spectra itself, including signed release metadata,
is planned and documented separately.

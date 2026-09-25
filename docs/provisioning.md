# Spectra provisioning

Provisioning is a local administrative library action. It is not a diagnostic
protocol operation, and this package is not yet wired to a command binary.

## Trust and configuration

`internal/provision.Run(ctx, args, stdout, stderr)` accepts `install`, `update`,
`rollback`, `status`, and `uninstall`. Install and update require `--version
vX.Y.Z`. Shared flags are `--config`, `--root`, repeatable `--source`,
`--trusted-key`, and `--allow-redirect-host`. `status --json` emits machine
readable status. `--source` replaces the configured list; trusted keys and
allowed redirect hosts append to it.

The optional JSON config has `root`, `sources`, `trusted_keys`,
`allowed_redirect_hosts`, and `keep_versions` fields. Unknown fields are
rejected. On Unix it must be owned by the current user and not writable by a
group or everyone else. Sources must use HTTPS. Redirects must remain on the
source host or an explicitly allowed host and must use HTTPS. The default
source is the Spectra GitHub releases download path. The default allowed
redirect hosts are `objects.githubusercontent.com` and
`release-assets.githubusercontent.com`.

The release public key has not yet been generated, so no key is built in.
Set a trusted `ed25519:<base64>` key before install, update, or rollback.
Signed metadata is verified before the artifact is selected. A bad signature,
untrusted key, wrong version, or artifact digest mismatch aborts immediately
without trying another source. Connection errors, 404s, and server errors may
advance to the next configured source.

The default root is `~/Library/Application Support/Spectra Proxy/spectra` on
macOS and `$XDG_DATA_HOME/spectra-proxy/spectra` elsewhere, falling back to
`~/.local/share/spectra-proxy/spectra`. `keep_versions` defaults to 2. When set
to 1, only the current version remains and rollback is unavailable. Larger
values retain current and previous plus the most recently installed extras.

## On disk and failure handling

The root contains `versions/<version>/spectra`, a `current` symlink to its
version directory, `state.json`, `staging/`, and a `.lock` file during normal
operation. The executable path is `<root>/current/spectra`. Root, versions,
and staging directories are private to the user. State records the current
and previous versions, the SHA-256 of each retained binary, and install times.

Downloads are bounded, signature and SHA-256 verified, and extracted without
invoking a shell. Extraction rejects links, devices, unsafe paths, duplicate
binary entries, and oversized output. The staged binary must pass
`capabilities --json` compatibility checks before activation. A nonblocking
Unix lock prevents concurrent changes; locking on non-Unix targets is not yet
implemented. Each mutating operation clears stale staging contents at start.
Status detects current binary drift. Rollback rehashes and checks compatibility
again before switching. Uninstall requires recognized provisioning state and
refuses to remove an unrelated directory.

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
Set a trusted `ed25519:<base64>` key before install or update. Signed
metadata is verified before the artifact is selected. A bad signature,
untrusted key, wrong version, or artifact digest mismatch aborts immediately
without trying another source. Connection errors, 404s, and server errors may
advance to the next configured source.

Rollback does not use trusted keys and needs none configured: it downloads
nothing. It switches back to the previous version already on disk, after
re-verifying that binary's recorded SHA-256 and re-running the same
`capabilities --json` compatibility check used by install and update.

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

Interruption before activation - anything up to and including a failed
`beforeCommit` hook, fetch, extraction, or compatibility check - leaves
`current` and `state.json` completely untouched; only staged, not-yet-active
files may be left behind, and those are cleared at the start of the next
mutating operation. Activation itself is two steps: an atomic rename of the
`current` symlink, then a `state.json` write recording the new version. A
crash between those two steps leaves `current` pointing at a version that
`state.json` does not (yet) call current. That is exactly the drift `status`
detects (it compares the binary at `current` against the SHA-256 recorded for
`state.json`'s current version) - `status` does not repair it, but re-running
`install` or `update` for the version `current` now points at will, since
that path recomputes and re-commits state from scratch. This is the only
crash window covered; no other partial-write scenario is claimed to be
detected or self-healing.

Audit writes are best-effort for a request rejected before execution (an
invalid request, a disallowed operation, an incompatible-Spectra probe): the
write is attempted but its failure does not change the rejection response.
Once a request begins executing, the "started" audit write is fail-closed -
if it cannot be written, the request is aborted with an unavailable error
before the local Spectra binary ever runs. This provisioning package does not
itself audit; see the target agent's audit log for the corresponding
protocol-side behavior.

## Remote transport timeouts

The `spectra-remote-agent serve-tsnet` transport - the protocol client of an
installed Spectra binary, in the same repo as this provisioning package -
bounds each tailnet session with two timeouts: a session timeout (default
five minutes) capping total session length, and an idle timeout (default one
minute) that is extended on every read and write. A session that goes idle
for longer than the idle timeout is dropped; a session that runs longer than
the session timeout is torn down regardless of activity, which also cancels
any Spectra process still running for it. Today neither has a CLI flag; both
take their defaults.

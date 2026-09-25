# End-to-end tests

Two opt-in suites prove the complete workflow with real binaries. Neither is
part of `make ci` or a pull-request check.

## Local suite (`make e2e`)

```bash
SPECTRA_CORE_DIR=/path/to/kaeawc/spectra make e2e
```

The suite lives in `e2e/` behind the `e2e` build tag and skips every test
when `SPECTRA_CORE_DIR` is unset. Prerequisites:

- Go from this repository's `go.mod`, and a `kaeawc/spectra` checkout whose
  `spectra` provides `capabilities --json`.
- macOS for the `inspect` step (it inspects
  `/System/Applications/Calculator.app`); on Linux that step is skipped.
- No network, Tailscale, or administrator access.

The harness builds this repository's `spectra-remote-agent` and
`spectra-remote`, then builds real Spectra from `SPECTRA_CORE_DIR` twice as
`v0.90.0` and `v0.91.0`. Each build is packaged like a core release
(`spectra_<version>_<os>_<arch>.tar.gz` containing
`spectra_<version>_<os>_<arch>/bin/spectra` and a README), described by a
`spectra-release.json` manifest signed with a throwaway Ed25519 key, and
served from a local HTTPS server. Provisioning trusts that server through
`--source-ca-file` and that key through `--trusted-key`.

The agent's `--max-run-duration` now defaults to 3 minutes (up from a fixed
30s cap), since a real `spectra snapshot --json --no-apps` can take 78-98s on
a used Mac; the harness's own timeouts and the controller's default
`--timeout` of 3 minutes are sized to comfortably outlast that.

Every test uses a fresh provisioning root, audit log, and release server.
Subprocesses run with `HOME` and the XDG directories pointed at a temporary
directory, so real install locations, LaunchAgents, audit logs, and Spectra's
own caches are never touched. Before the tests, the harness runs one direct
`spectra snapshot` to fill Spectra's caches in that temporary `HOME`.

`TestCompleteWorkflow` drives the built binaries as subprocesses:

1. `provision install --version v0.90.0`, then `provision status --json`.
2. `serve-stdio --provision-root <root>` without `--spectra`, driven over its
   stdin and stdout by `internal/controller`.
3. `health`: the capability manifest validates, reports Spectra `v0.90.0`,
   and advertises `inspect` and `snapshot.create` with result schemas.
4. `inspect` of Calculator returns a validated `spectra.inspect` v1 result.
5. `snapshot.create` without apps returns a validated `spectra.snapshot` v1
   result; with `include_apps: true` it is `permission_denied`.
6. The audit log holds `started` and `completed` events from the `stdio` peer.
7. `provision update --version v0.91.0`; the running agent's next `health`
   reports `v0.91.0`.
8. `provision rollback` returns to `v0.90.0`.
9. `provision uninstall` removes the root; `serve-stdio` without `--spectra`
   then fails with the required-path error.

Failure tests, each asserting that nothing was installed or the current
version is unchanged:

| Test | Scenario |
| --- | --- |
| `TestInstallRejectsTamperedArchive` | archive bytes differ from the signed digest |
| `TestInstallRejectsUntrustedKey` | the manifest is signed by a key that is not trusted |
| `TestInstallRejectsIncompatibleSpectra` | a signed release reports capabilities schema version 2 |
| `TestInterruptedUpdateKeepsCurrentVersion` | `update` is killed with SIGKILL mid-download; a retry succeeds |
| `TestDowngradeRefusedRollbackAllowed` | `install` of an older version is refused; rollback works |
| `TestDiagnosticTimeoutKeepsAgentUsable` | `snapshot.create` with `timeout_ms: 1` is `timeout`; `health` still works |

Peer denial is enforced by the tsnet transport before any request is read,
and is covered by the unit tests in `internal/transport/tsnet` rather than a
fake tailnet.

The `E2E` workflow (`.github/workflows/e2e.yml`) runs the suite weekly and on
demand on `macos-latest`. Its `core_ref` input picks the `kaeawc/spectra` ref
(default `main`).

## Two-machine Tailscale check

`TestTailscaleTwoMachine` is skipped unless `SPECTRA_E2E_TAILSCALE_TARGET` is
set to the `host:port` of a running `serve-tsnet` agent. It does not need
`SPECTRA_CORE_DIR`. It runs the built controller:

```bash
spectra-remote call --target "$SPECTRA_E2E_TAILSCALE_TARGET" --operation health \
  --negotiate --tsnet-ephemeral --tsnet-state-dir <temp dir>
```

and requires exit 0 and a manifest that validates. With
`SPECTRA_E2E_TAILSCALE_APP` set to an app bundle under an allowed root on the
target, it also runs a negotiated `inspect` and validates the result.

Prerequisites:

- Two machines on the same tailnet, and a tailnet policy that lets the
  controller's node reach the target on port 7878.
- An auth key for each side in `TS_AUTHKEY` (tsnet reads it from the
  environment). Ephemeral, pre-approved keys are recommended.
- On the target: `spectra-remote-agent`, a published Spectra release, and its
  trusted release key.

`scripts/e2e-tailscale.sh` automates both sides:

```bash
# Target: provision Spectra into a scratch root, then serve-tsnet from it.
SPECTRA_VERSION=v1.2.3 SPECTRA_TRUSTED_KEY=ed25519:... TS_AUTHKEY=tskey-... \
  scripts/e2e-tailscale.sh target

# Controller: run TestTailscaleTwoMachine from this repository.
SPECTRA_E2E_TAILSCALE_TARGET=spectra-e2e-target:7878 TS_AUTHKEY=tskey-... \
  scripts/e2e-tailscale.sh controller
```

The target uses the hostname `spectra-e2e-target` (override with
`E2E_HOSTNAME`), an ephemeral node, and allows only
`/System/Applications` and `/Applications` for `inspect`. Set
`E2E_ALLOW_LOGIN` to restrict connections to one tailnet login. Run
`scripts/e2e-tailscale.sh` with no arguments to list every variable.

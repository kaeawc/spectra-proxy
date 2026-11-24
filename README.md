# Spectra Proxy

See [Spectra provisioning](docs/provisioning.md) for local executable installation and rollback.
See [End-to-end tests](docs/e2e.md) for the opt-in real-binary and two-machine Tailscale suites.

Spectra Proxy is the separately installed remote-access component for
Spectra. It contains the authenticated controller, target agent, and
transport adapters. The `spectra` repository remains responsible for local
macOS diagnostics. The commands remain named `spectra-remote` (controller)
and `spectra-remote-agent` (target agent).

The target agent supports an embedded Tailscale `tsnet` listener, and the
controller joins the tailnet as its own managed node. Tailscale ACLs are the
baseline authorization control; agents can additionally allowlist login or
node identities. The agent accepts only typed diagnostic operations and
invokes a configured local Spectra executable with fixed argument mappings.

No request can supply a shell command, argv, download URL, or executable.
Remote installation and update will be a separately authenticated,
signature-verified provisioning workflow; they will not be protocol methods.

## Local development

The module path is `github.com/kaeawc/spectra-proxy` and it depends on a
`github.com/kaeawc/spectra-protocol` protocol/v1 contract. The current
worktree uses a temporary local module replace. Run `make ci` to build,
test, vet, and check formatting locally. See [RELEASING.md](RELEASING.md) for
release steps.

```bash
make ci
```

## Tailscale transport

Start a target agent with an explicit local Spectra path and permitted app
roots:

```bash
spectra-remote-agent serve-tsnet \
  --spectra /opt/spectra/bin/spectra \
  --tsnet-hostname work-mac \
  --allow-app-root /Applications \
  --tsnet-allow-login engineer@example.com
```

The agent resolves an authenticated Tailscale identity for every connection, including
when no login or node allowlist is configured. A failed identity lookup denies
the connection.

The controller uses a separate tsnet identity and sends only a typed request:

```bash
spectra-remote call --target work-mac:7878 --operation health
spectra-remote call --target work-mac:7878 --operation inspect \
  --params '{"app_paths":["/Applications/Slack.app"]}'
spectra-remote call --target work-mac:7878 --operation process.list
spectra-remote call --target work-mac:7878 --operation jvm.list
```

The operations are:

| Operation | Params | Result schema | Agent policy |
| --- | --- | --- | --- |
| `health` | none | capability manifest | always on |
| `inspect` | `{"app_paths":[...]}` | `spectra.inspect` v1 | paths must be under `--allow-app-root` |
| `snapshot.create` | `{"include_apps":bool}` | `spectra.snapshot` v1 | `--allow-snapshot` (and `--allow-snapshot-apps`) |
| `process.list` | none | `spectra.process.list` v1 | `--allow-processes` |
| `jvm.list` | none | `spectra.jvm.list` v1 | `--allow-processes` |

`process.list` data is `{"sampled_at":"<RFC 3339>","processes":[...]}`, where
each row is exactly what `spectra process --json --classify` prints. `jvm.list`
data is `{"jvms":[...]}`, where each entry is exactly what `spectra jvm --json`
prints. A successful `process.list` call prints:

```json
{
  "protocol_version": "v1",
  "request_id": "cli-0123456789abcdef",
  "result": {
    "schema": {
      "name": "spectra.process.list",
      "version": 1
    },
    "spectra_version": "0.42.0",
    "data": {
      "sampled_at": "2026-11-24T10:00:00.123456Z",
      "processes": [
        {
          "pid": 1,
          "command": "launchd",
          "full_command_line": "/sbin/launchd",
          "rss_kib": 12288,
          "role": "system"
        }
      ]
    }
  }
}
```

The controller validates the protocol version, request ID, bounded response
frame, and operation result schema before printing a response. Add
`--negotiate` to request the target's health manifest and verify that it
supports the requested operation before sending the call. CLI exit codes are
`0` for success, `1` for a remote or transport error, `2` for invalid usage or
parameters, and `3` for a protocol or incompatible schema error. A successful
result for an operation the controller has no result schema for is a protocol
error (`3`) and is not printed.

`spectra-remote call` defaults `--timeout` to 3 minutes so a default call can
outlast a real `spectra snapshot`, which can take 78-98s on a used Mac; raise
it further with `--timeout` for a slower target.

When `--spectra` is omitted, `serve-stdio`, `serve-tsnet`, and `install` use
the provisioned executable (`<root>/current/spectra`) if one is installed,
where the root is the provisioning default or `--provision-root`. Install it
with `spectra-remote-agent provision install --version vX.Y.Z --trusted-key
ed25519:...`; see [Spectra provisioning](docs/provisioning.md).

## Local agent installation

After placing `spectra-remote-agent` and `spectra` at administrator-approved
local paths, install a per-user LaunchAgent. This action writes only a local
plist and loads it for the current user; it does not download, update, or
replace either binary.

```bash
spectra-remote-agent install \
  --spectra /opt/spectra/bin/spectra \
  --tsnet-hostname work-mac \
  --allow-app-root /Applications \
  --tsnet-allow-login engineer@example.com
```

Review the exact plist without loading it:

```bash
spectra-remote-agent install --no-load \
  --spectra /opt/spectra/bin/spectra \
  --tsnet-hostname work-mac
```

Snapshots are disabled by default. Add `--allow-snapshot` to `serve-stdio`,
`serve-tsnet`, or `install` to permit `snapshot.create`; add
`--allow-snapshot-apps` as well to permit requests with `include_apps: true`.
The second flag requires the first. The installed Spectra must provide
`spectra capabilities --json` with supported `spectra.inspect` and
`spectra.snapshot` result schemas before the agent advertises or allows
`inspect` and `snapshot.create`. A failed capabilities probe leaves `health`
available and advertises only `health`.

Process listing is disabled by default too, because rows include every
visible process's full command line. Add `--allow-processes` to `serve-stdio`,
`serve-tsnet`, or `install` to permit `process.list` and `jvm.list`. Both are
read-only and take no parameters. The agent first asks the target's Spectra
daemon (`spectra daemon`) over its local unix socket: it calls `daemon.status`
and, if the daemon speaks `spectra-daemon/1` and lists the method, calls
`process.list` with `{"classify":true}` or `jvm.list` with `{}`. The socket
defaults to Spectra's own default (`$SPECTRA_DAEMON_DIR/daemon.sock`, else
`$XDG_RUNTIME_DIR/spectra/daemon.sock` on Linux, else
`~/.spectra/daemon.sock`); override it with an absolute
`--spectra-daemon-socket`. When no daemon answers or it does not list the
method, the agent runs the local Spectra with the fixed argv
`process -json --classify` or `jvm -json`. Health advertises each list
operation only when `--allow-processes` is set and one of those paths is usable:
the daemon lists the method, or the local Spectra passes the capabilities probe.

The service is `dev.spectra-remote.agent` in the current user's launchd
domain. Use `spectra-remote-agent install status` to inspect it, and
`spectra-remote-agent install uninstall` to unload and remove its plist.

Each agent request produces owner-private JSONL audit events at
`~/Library/Logs/Spectra Remote/agent.audit.jsonl` (or the absolute path passed
with `--audit-log`). Events contain the time, request ID, typed operation, authenticated peer
(`transport`, login, node, address or local user), stage (`started`,
`completed`, or connection `denied`), and outcome/error code. Parameters
and diagnostic results are never logged. A failed `started` write blocks
execution; a failed `completed` write is reported to the agent log.

The target limits concurrent sessions to eight by default. Change the local
LaunchAgent configuration deliberately with `--max-connections`; the value must
be positive.

Every target session also has a five-minute total deadline (`SessionTimeout`),
plus a one-minute idle deadline (`IdleTimeout`) that is extended on every read
and write. The idle deadline releases a session slot when an authenticated
peer connects but stops sending or reading; the session deadline is a hard
cap regardless of activity, so a still-running Spectra process (e.g. a slow
snapshot) is eventually killed even if the peer keeps the connection busy.
`--negotiate`'s extra health round trip and a subsequent diagnostic request
both fit comfortably inside the session deadline as long as each individual
step stays under the idle deadline. Neither timeout has a CLI flag yet; the
controller opens one short-lived session per call.

Each request the agent runs is separately capped by `--max-run-duration`
(default 3 minutes on `serve-stdio`, `serve-tsnet`, and `install`), sized for
a real `spectra snapshot` rather than a quick health or inspect call. On
`serve-tsnet` and `install` it must stay strictly under the five-minute
`SessionTimeout` above, or a still-running request would be killed by the
transport before the agent's own cap ever had a chance to fire.

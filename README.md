# Spectra Proxy

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
tagged `github.com/kaeawc/spectra-protocol` release. Run `make ci` to build,
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

The controller uses a separate tsnet identity and sends only a typed request:

```bash
spectra-remote call --target work-mac:7878 --operation health
spectra-remote call --target work-mac:7878 --operation inspect \
  --params '{"app_paths":["/Applications/Slack.app"]}'
```

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

The service is `dev.spectra-remote.agent` in the current user's launchd
domain. Use `spectra-remote-agent install status` to inspect it, and
`spectra-remote-agent install uninstall` to unload and remove its plist.

Each agent request also produces an owner-private JSONL audit event at
`~/Library/Logs/Spectra Remote/agent.audit.jsonl` (or the absolute path passed
with `--audit-log`). Events contain only the time, request ID, typed operation,
and outcome/error code—never request parameters or diagnostic results.

The target limits concurrent sessions to eight by default. Change the local
LaunchAgent configuration deliberately with `--max-connections`; the value must
be positive.

Every target session also has a one-minute total deadline. That releases a
session slot when an authenticated peer connects but never completes a typed
request; the controller opens one short-lived session per call.

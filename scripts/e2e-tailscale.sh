#!/usr/bin/env bash
# Two-machine Tailscale check for Spectra Proxy. See docs/e2e.md.
#
#   Target machine (macOS; provisions Spectra, then serves on the tailnet):
#     SPECTRA_VERSION=v1.2.3 SPECTRA_TRUSTED_KEY=ed25519:... \
#       TS_AUTHKEY=tskey-... scripts/e2e-tailscale.sh target
#
#   Controller machine (runs TestTailscaleTwoMachine against the target):
#     SPECTRA_E2E_TAILSCALE_TARGET=spectra-e2e-target:7878 \
#       TS_AUTHKEY=tskey-... scripts/e2e-tailscale.sh controller
set -euo pipefail

usage() {
    cat >&2 <<'EOF'
usage: scripts/e2e-tailscale.sh target|controller

target environment:
  SPECTRA_VERSION        release to provision (required, vX.Y.Z)
  SPECTRA_TRUSTED_KEY    release public key, ed25519:<base64> (required)
  SPECTRA_SOURCE         release source URL (default: provisioning default)
  SPECTRA_SOURCE_CA_FILE PEM roots for a private source (optional)
  SPECTRA_REMOTE_AGENT   agent binary (default: spectra-remote-agent on PATH)
  E2E_DIR                scratch root for provisioning, tsnet state, audit log
                         (default: a new temporary directory)
  E2E_HOSTNAME           tailnet hostname (default: spectra-e2e-target)
  E2E_ALLOW_LOGIN        tailnet login allowed to connect (optional)
  TS_AUTHKEY             auth key for the target's ephemeral tsnet node

controller environment:
  SPECTRA_E2E_TAILSCALE_TARGET  host:port of the target (required)
  SPECTRA_E2E_TAILSCALE_APP     app bundle to inspect remotely (optional)
  TS_AUTHKEY                    auth key for the controller's ephemeral node
EOF
    exit 2
}

require() {
    local name="$1"
    if [[ -z "${!name:-}" ]]; then
        echo "error: $name is required" >&2
        usage
    fi
}

run_target() {
    require SPECTRA_VERSION
    require SPECTRA_TRUSTED_KEY
    local agent="${SPECTRA_REMOTE_AGENT:-spectra-remote-agent}"
    local dir="${E2E_DIR:-$(mktemp -d "${TMPDIR:-/tmp}/spectra-e2e-tailscale.XXXXXX")}"
    local root="$dir/spectra"
    local provision_args=(--root "$root" --trusted-key "$SPECTRA_TRUSTED_KEY")
    if [[ -n "${SPECTRA_SOURCE:-}" ]]; then
        provision_args+=(--source "$SPECTRA_SOURCE")
    fi
    if [[ -n "${SPECTRA_SOURCE_CA_FILE:-}" ]]; then
        provision_args+=(--source-ca-file "$SPECTRA_SOURCE_CA_FILE")
    fi
    echo "provisioning Spectra $SPECTRA_VERSION into $root" >&2
    "$agent" provision install --version "$SPECTRA_VERSION" "${provision_args[@]}"
    "$agent" provision status --root "$root"

    local serve_args=(
        serve-tsnet
        --provision-root "$root"
        --tsnet-hostname "${E2E_HOSTNAME:-spectra-e2e-target}"
        --tsnet-state-dir "$dir/tsnet"
        --tsnet-ephemeral
        --audit-log "$dir/agent.audit.jsonl"
        --allow-app-root /System/Applications
        --allow-app-root /Applications
    )
    if [[ -n "${E2E_ALLOW_LOGIN:-}" ]]; then
        serve_args+=(--tsnet-allow-login "$E2E_ALLOW_LOGIN")
    fi
    echo "serving on the tailnet; audit log: $dir/agent.audit.jsonl (Ctrl-C to stop)" >&2
    exec "$agent" "${serve_args[@]}"
}

run_controller() {
    require SPECTRA_E2E_TAILSCALE_TARGET
    if [[ -z "${TS_AUTHKEY:-}" ]]; then
        echo "warning: TS_AUTHKEY is unset; tsnet will print a login URL instead" >&2
    fi
    cd "$(dirname "$0")/.."
    go test -tags e2e -count=1 -timeout 10m -v -run '^TestTailscaleTwoMachine$' ./e2e/...
}

case "${1:-}" in
    target) run_target ;;
    controller) run_controller ;;
    *) usage ;;
esac

# Spectra Remote

Spectra Remote is the separately installed remote-access component for
Spectra. It will contain the authenticated controller, target agent,
transport adapters, and provisioning workflow. The `spectra` repository
remains responsible for local macOS diagnostics.

This initial revision defines a transport-neutral target agent. Its
`serve-stdio` command is deliberately not a network server: a future
authenticated transport supervisor will pass it already-authorized protocol
requests. The agent accepts only typed diagnostic operations and invokes a
configured local Spectra executable with fixed argument mappings.

No request can supply a shell command, argv, download URL, or executable.
Remote installation and update will be a separately authenticated,
signature-verified provisioning workflow; they will not be protocol methods.

## Local development

The module temporarily uses a local `replace` directive for the sibling
`../spectra-protocol` checkout. Replace it with a tagged protocol version
before publishing this module.

```bash
go test ./...
go build ./cmd/spectra-remote ./cmd/spectra-remote-agent
```

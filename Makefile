.PHONY: build test vet fmt-check mod-check ci release-check e2e

build:
	go build ./cmd/spectra-remote ./cmd/spectra-remote-agent

fmt-check:
	@files="$$(gofmt -l .)"; \
	if [ -n "$$files" ]; then \
		echo "gofmt needs to format:"; \
		echo "$$files"; \
		exit 1; \
	fi

vet:
	go vet ./...

test:
	go test -race -count=1 ./...

mod-check:
	@if grep -qE '^[[:space:]]*replace[[:space:]]' go.mod; then \
		echo "go.mod contains replace directives"; \
		exit 1; \
	fi

ci: fmt-check vet test build

release-check: mod-check ci

# Opt-in end-to-end suite; requires SPECTRA_CORE_DIR (see docs/e2e.md).
e2e:
	go test -tags e2e -count=1 -timeout 20m ./e2e/...

.PHONY: build test vet

build:
	go build ./cmd/spectra-remote ./cmd/spectra-remote-agent

test:
	go test ./... -count=1

vet:
	go vet ./...

BINARY  := s3-container-runner
VERSION ?= dev
IMAGE   ?= ghcr.io/humanitec-architecture/$(BINARY):$(VERSION)

# Pinned in tools/go.mod.
GOLANGCI_LINT := go tool -modfile=tools/go.mod golangci-lint

.PHONY: build test-unit test-integration lint fmt image clean

build:
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=$(VERSION)" -o bin/$(BINARY) ./cmd/$(BINARY)

test-unit:
	go test -race -cover ./...

# Needs Docker: builds the image, then runs it against moto, an S3 emulator, through testcontainers.
test-integration: image
	RUNNER_IMAGE=$(IMAGE) go test -race -tags integration -timeout 10m ./...

lint:
	$(GOLANGCI_LINT) run

fmt:
	$(GOLANGCI_LINT) fmt

image:
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE) .

# The leading - ignores an image that is already gone.
clean:
	rm -rf bin
	-docker image rm $(IMAGE)

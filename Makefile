BIN      ?= bin/nomira-frontend
IMAGE    ?= ghcr.io/nomira-os/nomira:1-alpha
GOFLAGS  ?=

.PHONY: build test lister-test lint-example graph-example image clean

build:
	CGO_ENABLED=0 go build $(GOFLAGS) -trimpath -ldflags='-s -w' -o $(BIN) ./cmd/frontend

test:
	go vet $(GOFLAGS) ./...
	go test $(GOFLAGS) ./...
	sh hack/test_lister.sh
	sh hack/test_guard.sh

lint-example: build
	$(BIN) lint examples/recipe.yml

graph-example: build
	$(BIN) graph examples/recipe.yml

# Build and push the frontend image (podman or docker).
image:
	podman build -f Dockerfile.frontend -t $(IMAGE) .

clean:
	rm -rf bin

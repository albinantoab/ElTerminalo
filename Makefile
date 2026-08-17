APP_NAME = ElTerminalo
BINARY = elterminalo

VERSION ?= $(shell cat VERSION 2>/dev/null || echo "0.1.0")

# CGo flags for llama.cpp Metal support on macOS
export CGO_LDFLAGS = -framework Accelerate -framework Foundation -framework Metal -framework MetalKit -framework MetalPerformanceShaders

.PHONY: build run app clean dev lint test release setup-llm clean-llm

# go-llama.cpp is consumed through a local `replace` in go.mod, so whatever is
# checked out in deps/ *is* the dependency. It must be pinned.
#
# This commit is the one go.mod already names in its pseudo-version,
# v0.0.0-20260318205202-4189a5b8fd8e. Cloning an unpinned HEAD instead makes
# every build depend on whatever upstream shipped that day: a newer go.mod
# there raises the module graph above the versions our go.sum records, and all
# three Go CI jobs fail with "updates to go.mod needed; to update it: go mod
# tidy". That is invisible until the Actions cache expires, then CI turns red
# with no change on our side.
#
# To upgrade: bump this SHA, run `make setup-llm && go mod tidy`, and commit
# go.mod/go.sum together with the bump.
GO_LLAMA_REPO   = https://github.com/AshkanYarmoradi/go-llama.cpp.git
GO_LLAMA_COMMIT = 4189a5b8fd8e59afb76f30473f9d2d99700f8196

# Build the llama.cpp static library (run once, or after bumping GO_LLAMA_COMMIT)
setup-llm:
	@if [ ! -d deps/go-llama.cpp/.git ]; then \
		echo "Cloning go-llama.cpp..."; \
		rm -rf deps/go-llama.cpp; \
		git clone $(GO_LLAMA_REPO) deps/go-llama.cpp; \
	fi
	@cd deps/go-llama.cpp && \
	if [ "$$(git rev-parse HEAD)" != "$(GO_LLAMA_COMMIT)" ]; then \
		echo "Checking out pinned commit $(GO_LLAMA_COMMIT)..."; \
		git fetch --quiet origin $(GO_LLAMA_COMMIT) 2>/dev/null || git fetch --quiet origin; \
		git checkout --quiet --detach $(GO_LLAMA_COMMIT); \
		rm -f libbinding.a; \
	fi
	@cd deps/go-llama.cpp && git submodule update --init --recursive --quiet
	@if [ ! -f deps/go-llama.cpp/libbinding.a ]; then \
		echo "Building llama.cpp (Metal)..."; \
		cd deps/go-llama.cpp && BUILD_TYPE=metal make libbinding.a; \
	else \
		echo "llama.cpp already built."; \
	fi
	@echo "llama.cpp pinned at $(GO_LLAMA_COMMIT)"

dev: setup-llm
	wails dev

build: setup-llm
	wails build

run: build
	./build/bin/$(BINARY)

app:
	./scripts/build-app.sh

lint:
	golangci-lint run ./...
	cd frontend && npx tsc --noEmit

test:
	go test ./...

release:
	./scripts/release.sh $(VERSION)

clean:
	rm -rf build/bin release $(APP_NAME).app

clean-llm:
	rm -rf deps/go-llama.cpp

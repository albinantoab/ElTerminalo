APP_NAME = ElTerminalo
BINARY = elterminalo

VERSION ?= $(shell cat VERSION 2>/dev/null || echo "0.1.0")

# CGo flags for llama.cpp Metal support on macOS
export CGO_LDFLAGS = -framework Accelerate -framework Foundation -framework Metal -framework MetalKit -framework MetalPerformanceShaders

.PHONY: build run app clean dev lint test release setup-llm clean-llm

# Build the llama.cpp static library (run once, or after updating deps/go-llama.cpp)
setup-llm:
	@if [ ! -d deps/go-llama.cpp/llama.cpp ]; then \
		echo "Cloning go-llama.cpp with llama.cpp submodule..."; \
		git clone --recursive https://github.com/AshkanYarmoradi/go-llama.cpp.git deps/go-llama.cpp; \
	fi
	@if [ ! -f deps/go-llama.cpp/libbinding.a ]; then \
		echo "Building llama.cpp (Metal)..."; \
		cd deps/go-llama.cpp && BUILD_TYPE=metal make libbinding.a; \
	else \
		echo "llama.cpp already built."; \
	fi

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

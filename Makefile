APP_NAME = ElTerminalo
BINARY = elterminalo

VERSION ?= $(shell cat VERSION 2>/dev/null || echo "0.1.0")

.PHONY: build run app clean dev lint test release

dev:
	wails dev

build:
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

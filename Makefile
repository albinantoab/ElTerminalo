APP_NAME = ElTerminalo
BINARY = elterminalo

.PHONY: build run app clean dev lint test

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

clean:
	rm -rf build/bin $(APP_NAME).app

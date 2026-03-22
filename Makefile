APP_NAME = ElTerminalo
BINARY = elterminalo

.PHONY: build run app clean

build:
	go build -o $(BINARY) .

run: build
	./$(BINARY)

app: build
	./scripts/build-app.sh

clean:
	rm -rf $(BINARY) $(APP_NAME).app

.PHONY: build clean server client

build: bin/dread-server bin/dread

bin/dread-server: $(shell find . -name '*.go')
	go build -o bin/dread-server ./cmd/server

bin/dread: $(shell find . -name '*.go')
	go build -o bin/dread ./cmd/dread

server: bin/dread-server
	./bin/dread-server

run: bin/dread
	./bin/dread

clean:
	rm -rf bin/

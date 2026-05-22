.PHONY: all build etl server tidy clean

all: build

tidy:
	go mod tidy

build: tidy
	mkdir -p bin
	go build -ldflags="-s -w" -o bin/etl    ./cmd/etl
	go build -ldflags="-s -w" -o bin/server ./cmd/server
	go build -ldflags="-s -w" -o bin/stress ./cmd/stress
	go build -ldflags="-s -w" -o bin/blitz  ./cmd/blitz

# Import an NSRL SQLite DB into Pebble.
# Usage: make import DB=/path/to/NSRLFile.db
import:
	./bin/etl --db $(DB) --out data/nsrl.pebble

# Start the API server.
run:
	./bin/server --db data/nsrl.pebble --addr :8080

clean:
	rm -rf bin/ data/nsrl.pebble

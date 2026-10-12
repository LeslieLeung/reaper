.PHONY: build run docker-build docker-run test clean dry-build

# Build the gitrieve binary (CGo enabled for go-sqlite3)
build:
	go build -o gitrieve main.go

# Run the server locally
run:
	go run main.go -c config.yaml server

# Build the Docker image
docker-build:
	docker build -t wnarutou/gitrieve:latest .

# Run the server via docker-compose
docker-run:
	docker compose up

# Run all tests
test:
	go test ./...
	node --test web/main.test.cjs web/tests/*.test.js

# Quick build sanity check (builds then removes the binary)
dry-build:
	go build -o gitrieve main.go
	rm -r gitrieve

# Remove build artifacts
clean:
	rm -f gitrieve

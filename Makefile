.PHONY: build run test fmt vet tidy

build:
	go build -o bin/mux ./cmd/mux

run: build
	./bin/mux

test:
	go test ./...

fmt:
	go fmt ./...

vet:
	go vet ./...

tidy:
	go mod tidy

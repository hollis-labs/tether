.PHONY: build run test test-race fmt vet tidy check

build:
	go build -o bin/mux ./cmd/mux

run: build
	./bin/mux

# test is the fast iteration loop — no race detector so it compiles + runs
# quickly during TDD. Use test-race for the correctness gate.
test:
	go test ./...

# test-race is the correctness gate: runs the full suite with the race
# detector enabled. Sprint v002-s07 will wire this into CI.
test-race:
	go test -race ./...

fmt:
	go fmt ./...

vet:
	go vet ./...

tidy:
	go mod tidy

# check is the pre-commit full gate: build + vet + race tests.
check: build vet test-race

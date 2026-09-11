BINARY  := mph
CMD     := ./cmd/mph
VERSION ?= dev
LDFLAGS := -X main.version=$(VERSION) -s -w
GOFLAGS ?=

.PHONY: all build vet test test-race test-cover test-verbose fmt tidy run clean help

all: build

build:
	@mkdir -p bin
	go build -trimpath $(GOFLAGS) -ldflags "$(LDFLAGS)" -o bin/$(BINARY) $(CMD)

vet:
	go vet ./...

test:
	go test -count=1 -v ./...

test-race:
	go test -race -count=1  -v ./...

test-cover:
	go test -count=1 -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out | tail -n 20
	@echo "coverage profile: coverage.out"

fmt:
	go fmt ./...

tidy:
	go mod tidy
	go mod verify

clean:
	rm -rf bin/ coverage.out

help:
	@echo "Targets:"
	@echo "  build        - build bin/$(BINARY) with version $(VERSION)"
	@echo "  vet          - go vet ./..."
	@echo "  test         - go test -count=1 ./..."
	@echo "  test-race    - go test -race -count=1 ./... (race detector)"
	@echo "  test-cover   - go test with coverage profile"
	@echo "  fmt          - go fmt ./..."
	@echo "  tidy         - go mod tidy + verify"
	@echo "  clean        - remove bin/ and coverage.out"

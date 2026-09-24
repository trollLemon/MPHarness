BINARY  := mph
CMD     := ./cmd/mph
VERSION ?= dev
LDFLAGS := -X main.version=$(VERSION) -s -w
GOFLAGS ?=
SNAPCRAFT ?= snapcraft
SNAPCRAFT_FLAGS ?=

.PHONY: all build vet test test-race test-cover test-verbose fmt tidy run clean snap snap-destructive snap-clean help

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
	rm -f *.snap

snap:
	$(SNAPCRAFT) pack $(SNAPCRAFT_FLAGS)

snap-destructive:
	$(SNAPCRAFT) pack --destructive-mode $(SNAPCRAFT_FLAGS)

snap-clean:
	$(SNAPCRAFT) clean
	rm -f *.snap

help:
	@echo "Targets:"
	@echo "  build            - build bin/$(BINARY) with version $(VERSION)"
	@echo "  vet              - go vet ./..."
	@echo "  test             - go test -count=1 ./..."
	@echo "  test-race        - go test -race -count=1 ./... (race detector)"
	@echo "  test-cover       - go test with coverage profile"
	@echo "  fmt              - go fmt ./..."
	@echo "  tidy             - go mod tidy + verify"
	@echo "  clean            - remove bin/, coverage.out and *.snap"
	@echo "  snap             - build snap package (snapcraft pack)"
	@echo "  snap-destructive - build snap in destructive mode (no LXD/VM, uses host)"
	@echo "  snap-clean       - clean snapcraft build artefacts"

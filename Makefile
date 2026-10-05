BINARY  := mph
CMD     := ./cmd/mph
VERSION ?= dev
LDFLAGS := -X main.version=$(VERSION) -s -w
GOFLAGS ?=

OTEL_COMPOSE := docker-compose/docker-compose.yaml
OTEL_PROJECT := mph-otel

.PHONY: all build vet test test-cover test-verbose fmt tidy run clean help otel-up otel-down otel-reload-dashboards test-integration

test-integration: build
	sh testing/integration-output.sh

test-integration-compaction: build
	sh testing/integration-compaction.sh

all: build

build:
	@mkdir -p bin
	go build -trimpath $(GOFLAGS) -ldflags "$(LDFLAGS)" -o bin/$(BINARY) $(CMD)

vet:
	go vet ./...

test:
	go test -count=1 -v ./...

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

otel-up:
	docker compose -f $(OTEL_COMPOSE) up -d
	@echo "OTel stack up. Grafana: http://localhost:3000 (admin/admin), OTLP: localhost:4317"

otel-down:
	docker compose -f $(OTEL_COMPOSE) down

# Grafana persists provisioned dashboards in its data volume, so edits to
# example_grafana/dashboards/ are not picked up until that volume is dropped.
# Recreate only Grafana with a fresh volume; Prometheus/Loki/Tempo data is kept.
otel-reload-dashboards:
	docker compose -f $(OTEL_COMPOSE) rm -sf grafana
	-docker volume rm $(OTEL_PROJECT)_grafana-data
	docker compose -f $(OTEL_COMPOSE) up -d grafana
	@echo "Grafana reloaded with fresh dashboards: http://localhost:3000 (admin/admin)"

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
	@echo "  otel-up      - start OTel LGTM stack (Grafana+Loki+Tempo+Prometheus)"
	@echo "  otel-down    - stop OTel stack"
	@echo "  otel-reload-dashboards - recreate Grafana with a fresh volume to re-provision dashboards (keeps logs and traces)"
	@echo "  test-integration - run chunking/compaction fixture (needs VM + model)"
	@echo "  test-integration-compaction - run compaction fixture on tiny model (needs VM + model)"

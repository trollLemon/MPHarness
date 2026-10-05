# Example Grafana dashboard

The Grafana dashboard and provisioning files in this directory, and the
Compose stack and service configs in `docker-compose/`, exist for local
development only. They are not a deployment and carry
no production support commitment. Grafana has no authentication beyond its
default `admin`/`admin`, nothing is served over TLS, and nothing here deletes
data on its own.

Everything listens on localhost, and telemetry only reaches it when `mph` is
started with OTel switched on (`--otel`, `MPH_OTEL=true`, or `otel.enabled:
true`). Without one of those, nothing dials the collector.

## Running it

```bash
make otel-up                 # from the repo root
./bin/mph ./testing/test.yaml
```

Grafana is then on http://localhost:3000 (admin/admin). The dashboard is
provisioned read-only from `example_grafana/dashboards/`, so edit the JSON in this repo
rather than the panel editor: the file is the source of truth and the UI is only
ever a viewer. Tempo traces are reachable through Grafana Explore.

The dashboard's **Run traces** table lists one row per span for the selected run,
so nested spans such as `multipass.exec` appear alongside their parent; each row
links to its waterfall in Explore. TraceQL search only sees flushed blocks, so a
run becomes searchable roughly a minute after it finishes. Every other run panel
is LogQL over the structured `event=` log lines (see
`docs/how-to/enable_otel.md`). Grafana's Loki datasource turns each instant
query into one table frame (label columns plus `Value #<refId>`), so table
panels combine their queries with *Merge* and name columns with *Organize*,
not *Join by labels*. Token counts
are shown without pricing or cost estimates; the application does not calculate
monetary costs.

**Context over time** plots context used and remaining context (window minus
used) from each `token_usage` event, with applied compactions as points. It is an
XY chart over the raw log lines, so its time axis spans the run itself rather
than the dashboard range; the range only has to include the run.

To discard only this stack's stored telemetry and Grafana state and restart
with empty storage:

```bash
docker compose -f docker-compose/docker-compose.yaml down --volumes
make otel-up
```

These commands remove this project's named volumes, not other Docker projects
or the dashboard and configuration files in the repository.

## Test inputs and scripts

The YAML files in `testing/` are `mph` run inputs for e2e testing:
`test.yaml` is a short smoke run, `longrun.yaml` is a long checklist run, and
`compaction.yaml` is tuned to trigger compaction. The integration scripts also
live in `testing/`. From the repository root, run `make test-integration`
or `make test-integration-compaction`; both require Multipass and the
configured model.
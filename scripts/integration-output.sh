#!/bin/sh
# Integration smoke for chunked output + compaction. Needs a Multipass VM and
# the smoke-test model; run with the OTel stack up (make otel-up).
set -eu

BIN="${BIN:-bin/mph}"
FIXTURE="${FIXTURE:-testing/chunking.yaml}"

if [ ! -x "$BIN" ]; then
  echo "building $BIN..."
fi
# Always rebuild: a stale binary silently tests old code.
make build

OUT="$(mktemp)"
trap 'rm -f "$OUT"' EXIT

"$BIN" -pretty "$FIXTURE" 2>&1 | tee "$OUT"


fail() {
  echo "integration-output: FAIL: $1" >&2
  exit 1
}

grep -q 'output_id' "$OUT" || fail "no handle (output_id) on console"
grep -q 'total_matches' "$OUT" || fail "no search result (total_matches) on console"
grep -q '"lines"' "$OUT" || fail "no absolute line numbers (lines) on console"
grep -qi 'context compaction' "$OUT" || fail "no compaction warning on console"

echo "integration-output: PASS (handle + search + compaction observed)"

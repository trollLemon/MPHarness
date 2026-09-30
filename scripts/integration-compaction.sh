#!/bin/sh
# Integration smoke for conversation compaction on a tiny deterministic model.
# Needs a Multipass VM and the 14B reasoning model; run with the OTel stack up
# (make otel-up).
set -eu

BIN="${BIN:-bin/mph}"
FIXTURE="${FIXTURE:-testing/compaction.yaml}"

if [ ! -x "$BIN" ]; then
  echo "building $BIN..."
fi
# Always rebuild: a stale binary silently tests old code.
make build

OUT="$(mktemp)"
trap 'rm -f "$OUT"' EXIT

"$BIN" -pretty -verbose "$FIXTURE" 2>&1 | tee "$OUT"

fail() {
  echo "integration-compaction: FAIL: $1" >&2
  exit 1
}

grep -qi 'context compaction' "$OUT" || fail "no compaction warning on console"
grep -q 'outcome=applied' "$OUT" || fail "no applied compaction on console"

echo "integration-compaction: PASS (compaction applied observed)"

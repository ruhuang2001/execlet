#!/usr/bin/env bash
set -euo pipefail

UNIT_DIR=${1:?usage: coverage.sh <unit-dir> <e2e-dir> [gate]}
E2E_DIR=${2:?usage: coverage.sh <unit-dir> <e2e-dir> [gate]}
GATE=${3:-70}
MERGED=${MERGED_DIR:-/tmp/execlet-coverage-merged}
PROFILE=${PROFILE:-/tmp/execlet-coverage.txt}

rm -rf "$MERGED"
mkdir -p "$MERGED"

inputs=()
[ -d "$UNIT_DIR" ] && [ -n "$(ls -A "$UNIT_DIR" 2>/dev/null)" ] && inputs+=("$UNIT_DIR")
[ -d "$E2E_DIR" ] && [ -n "$(ls -A "$E2E_DIR" 2>/dev/null)" ] && inputs+=("$E2E_DIR")

if [ ${#inputs[@]} -eq 0 ]; then
  echo "coverage: no covdata files in $UNIT_DIR or $E2E_DIR" >&2
  exit 1
fi

joined=$(IFS=,; echo "${inputs[*]}")
go tool covdata merge -i="$joined" -o="$MERGED"
go tool covdata textfmt -i="$MERGED" -o="$PROFILE"

printf '\n=== merged coverage (%s) ===\n' "$joined"
go tool covdata percent -i="$MERGED" | tail -20

total=$(awk '
  /^mode:/ { next }
  NF >= 3 {
    total++
    if ($3 > 0) covered++
  }
  END {
    if (total == 0) exit 1
    printf "%.1f", 100 * covered / total
  }
' "$PROFILE")
if [ -z "$total" ]; then
  echo "coverage: could not compute a total from $PROFILE" >&2
  exit 1
fi
printf '\nmerged total: %s%% (gate: %s%%)\n' "$total" "$GATE"
awk -v total="$total" -v gate="$GATE" 'BEGIN { exit !(total >= gate) }'

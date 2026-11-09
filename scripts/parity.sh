#!/bin/sh
# Converts the same Monarch exports with both runtimes and checks that they
# write byte-identical import files.
#
#   scripts/parity.sh Balances.csv Transactions.csv
#
# Uses each runtime's suggested config offline, so no Sure server is needed.
# The exports and generated files stay in a temporary directory that is
# removed afterwards.
set -eu

if [ $# -ne 2 ]; then
  echo "usage: $0 BALANCES.csv TRANSACTIONS.csv" >&2
  exit 2
fi
balances=$1
transactions=$2
root=$(cd "$(dirname "$0")/.." && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

(cd "$root" && go build -o "$work/monarch-to-sure" ./go/cmd/monarch-to-sure)

python3 "$root/python/monarch_to_sure.py" init-config --no-input \
  --balances "$balances" --transactions "$transactions" -o "$work/python-accounts.json" >/dev/null
"$work/monarch-to-sure" init-config --no-input \
  --balances "$balances" --transactions "$transactions" -o "$work/go-accounts.json" >/dev/null

# Both runtimes convert with the Python config, so a difference can only come
# from the conversion. The configs themselves are compared separately.
python3 "$root/python/monarch_to_sure.py" convert --offline \
  --balances "$balances" --transactions "$transactions" \
  --config "$work/python-accounts.json" -o "$work/python.ndjson" >/dev/null
"$work/monarch-to-sure" convert --offline \
  --balances "$balances" --transactions "$transactions" \
  --config "$work/python-accounts.json" -o "$work/go.ndjson" >/dev/null

python3 - "$work/python-accounts.json" "$work/go-accounts.json" <<'PY'
import json, sys
python, go = (json.load(open(p))["accounts"] for p in sys.argv[1:])
if list(python) != list(go):
    sys.exit("account configs list different accounts")
for name in python:
    if python[name] != go[name]:
        sys.exit(f"account configs differ for an account: {sorted(k for k in python[name] if python[name][k] != go[name].get(k))}")
print(f"account configs match ({len(python)} accounts)")
PY

if cmp -s "$work/python.ndjson" "$work/go.ndjson"; then
  echo "import files match ($(wc -l < "$work/python.ndjson" | tr -d ' ') records)"
else
  echo "import files differ" >&2
  exit 1
fi

#!/usr/bin/env bash
# Turn a raw Alarm Manager delivery captured by hack/webhook-logger.mjs into a
# committable fixture — allowlist first, exactly like hack/capture-unifi.sh.
#
#   ./hack/sanitize-webhook-capture.sh --paths <raw.json>
#   ./hack/sanitize-webhook-capture.sh <raw.json> <name> <path,path,...>
#
# Example:
#   ./hack/sanitize-webhook-capture.sh --paths testdata/unifi/webhooks/raw/2026-01-01T00-00-00-000Z_000.json
#   ./hack/sanitize-webhook-capture.sh testdata/unifi/webhooks/raw/2026-01-01T00-00-00-000Z_000.json \
#     internet-disconnected alarm.trigger,alarm.title
#
# Why this is two steps. A raw record contains the whole request: every header,
# including the Authorization header carrying Reactor's own shared secret, and a
# body of unknown shape. The envelope can be allowlisted here once and for all,
# because its fields are known. The body cannot: nobody has seen one yet. So
# --paths prints every field path in the body and you name the ones the fixture
# should keep, deliberately, one at a time.
#
# Do not reach for a denylist instead. An earlier version of the API fixtures
# removed the sensitive fields someone thought of rather than keeping only the
# needed ones, and a live credential reached this repository's history as a
# result.
set -euo pipefail

cd "$(dirname "$0")/.."
OUT=testdata/unifi/webhooks

usage() {
  sed -n '2,23p' "$0" | sed 's/^# \{0,1\}//'
  exit 1
}

[ $# -ge 1 ] || usage
command -v jq >/dev/null || { echo "jq is required" >&2; exit 1; }

if [ "$1" = "--paths" ]; then
  [ $# -eq 2 ] || usage
  echo "field paths in the delivery body of $2:"
  jq -r '.bodyUtf8 | fromjson | [paths | join(".")] | .[]' "$2"
  echo
  echo "Name the ones the fixture should keep:"
  echo "  $0 $2 <name> alarm.trigger,alarm.title"
  exit 0
fi

[ $# -eq 3 ] || usage
RAW="$1"
NAME="$2"
[ -f "$RAW" ] || { echo "no such capture: $RAW" >&2; exit 1; }

# The allowlist as jq paths, e.g. "alarm.trigger" -> ["alarm","trigger"].
PATHS=$(printf '%s' "$3" | jq -R 'split(",") | map(gsub("^\\s+|\\s+$";"") | select(length > 0) | split("."))')
[ "$PATHS" != "[]" ] || { echo "name at least one field path to keep" >&2; exit 1; }

mkdir -p "$OUT"
DEST="$OUT/$NAME.json"

# The envelope allowlist. Note what is NOT in it: every request header (the
# Authorization header carries Reactor's shared secret), the source address, and
# the base64 copy of the body, which would otherwise smuggle the whole payload
# back in past every text-based check.
jq --arg name "$NAME" --argjson paths "$PATHS" '{
  capture: $name,
  method: .method,
  url: .url,
  contentType: (.headers["content-type"] // null),
  body: (.bodyUtf8 | fromjson as $body
    | reduce $paths[] as $path ({};
        if ($body | getpath($path)) == null then . else setpath($path; $body | getpath($path)) end))
} | with_entries(select(.value != null))' "$RAW" > "$DEST"

echo "wrote $DEST"
echo
./hack/verify-testdata.sh
echo
echo "Read $DEST before committing it. This script keeps what you named and"
echo "nothing else, but only you know whether what you named is safe to publish."

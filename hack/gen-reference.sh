#!/usr/bin/env bash
# Generate the Reference section of the documentation site.
#
# Four pages, none of them written by hand:
#
#   automation.md  the Automation API, from the doc comments and kubebuilder
#                  markers in api/v1alpha1, via crd-ref-docs
#   values.md      every chart value, from the prose above each key in
#                  charts/reactor/values.yaml
#   metrics.md     every published series, from internal/metrics
#   events.md      every Event and condition reason, from internal/controller
#
# Run by `make docs`. The output is committed; CI fails on drift, because a
# reference page nobody notices has gone stale is worse than no page at all.
set -euo pipefail

cd "$(dirname "$0")/.."

crd_ref_docs="${1:-bin/crd-ref-docs}"
if [ ! -x "$crd_ref_docs" ]; then
	echo "gen-reference: $crd_ref_docs is not executable; run 'make docs' rather than this script" >&2
	exit 1
fi

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

"$crd_ref_docs" \
	--source-path=./api \
	--config=hack/crd-ref-docs.yaml \
	--renderer=markdown \
	--output-path="$tmp/api.md" \
	--log-level=warn

go run ./hack/docsgen -root . -crd-markdown "$tmp/api.md"

echo "gen-reference: wrote docs/src/content/docs/reference/"

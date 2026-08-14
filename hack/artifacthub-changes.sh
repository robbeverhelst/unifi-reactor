#!/usr/bin/env bash
# Render an artifacthub.io/changes annotation from the commits in a release.
#
#   ./hack/artifacthub-changes.sh v1.1.0            # since the previous tag
#   ./hack/artifacthub-changes.sh v1.1.0 v1.0.0     # since an explicit one
#   ./hack/artifacthub-changes.sh --write v1.1.0    # into charts/reactor/Chart.yaml
#
# Prints the annotation value — a YAML list — on stdout, and nothing at all if
# the range contains no commit worth listing. With --write it inserts the
# annotation into charts/reactor/Chart.yaml instead; the release workflow does
# that just before `helm package`, on the runner's copy, and nothing is
# committed. Uses nothing but git, awk and sed, because the one place it runs
# unattended is the release, and a release is a bad place to discover that a
# tool was assumed to be on the runner.
#
# Why generate it rather than write it by hand: CHANGELOG.md refuses to keep a
# hand-maintained changelog, on the grounds that two sources of truth drift and
# a stale one is confidently wrong. An artifacthub.io/changes block committed in
# Chart.yaml would be exactly that file — it describes one release, and the
# release after it would ship the previous release's notes to Artifact Hub with
# no diff to notice. So it comes from the same place the GitHub release notes
# come from: the commits in the tag. Conventional commits are already required
# for that reason, which is what makes the mapping below possible.
set -euo pipefail

cd "$(dirname "$0")/.."

WRITE=""
if [ "${1:-}" = "--write" ]; then WRITE="yes"; shift; fi

TAG="${1:-}"
[ -n "$TAG" ] || { echo "usage: $0 [--write] <tag> [previous-tag]" >&2; exit 2; }

CHART=charts/reactor/Chart.yaml

PREV="${2:-}"
if [ -z "$PREV" ]; then
  # The tag before this one, by tag order rather than by date: a release cut
  # from an older commit must not be described as containing everything since.
  PREV="$(git tag --list 'v*' --sort=-v:refname | grep -A1 -x -- "$TAG" | tail -n1 || true)"
  [ "$PREV" = "$TAG" ] && PREV=""
fi

RANGE="$TAG"
[ -n "$PREV" ] && RANGE="$PREV..$TAG"

# Artifact Hub accepts exactly six kinds: added, changed, deprecated, removed,
# fixed, security. Conventional-commit types map onto them as below. Types with
# no meaningful entry — docs, test, ci, build, chore, style — are dropped
# rather than forced into `changed`, because a release note listing a lint fix
# is noise that hides the two lines that mattered.
kind_for() {
  case "$1" in
    feat)     echo added ;;
    fix)      echo fixed ;;
    perf)     echo changed ;;
    refactor) echo changed ;;
    revert)   echo removed ;;
    security) echo security ;;
    deprecate|deprecated) echo deprecated ;;
    *)        echo "" ;;
  esac
}

emitted=0
out=""

while IFS=$'\t' read -r sha subject; do
  [ -n "$subject" ] || continue

  # type(scope)!: description  —  the scope and the ! are optional.
  header="${subject%%:*}"
  [ "$header" = "$subject" ] && continue          # not a conventional commit
  desc="${subject#*: }"
  [ -n "$desc" ] || continue

  breaking=""
  case "$header" in *"!") breaking="yes"; header="${header%!}" ;; esac
  type="${header%%(*}"
  type="$(printf '%s' "$type" | tr '[:upper:]' '[:lower:]' | tr -d ' ')"

  kind="$(kind_for "$type")"
  # A breaking change is a change whatever its type: a feat! removes the old
  # behaviour as surely as it adds the new one, and `added` would undersell it.
  [ -n "$breaking" ] && kind=changed
  [ -n "$kind" ] || continue

  # YAML-quote by doubling single quotes, so a description containing a colon,
  # a quote or a leading dash cannot end the scalar early or start a new node.
  safe="$(printf '%s' "$desc" | sed "s/'/''/g")"
  [ -n "$breaking" ] && safe="breaking: $safe"

  out+="- kind: $kind"$'\n'
  out+="  description: '$safe'"$'\n'
  out+="  links:"$'\n'
  out+="    - name: Commit"$'\n'
  out+="      url: https://github.com/robbeverhelst/unifi-reactor/commit/$sha"$'\n'
  emitted=$((emitted + 1))
done < <(git log --no-merges --format=$'%H\t%s' "$RANGE" 2>/dev/null || true)

# Nothing to say is a valid outcome — a release of nothing but docs and CI. It
# is not an error, and it must not leave an empty annotation behind either.
[ "$emitted" -gt 0 ] || exit 0

if [ -z "$WRITE" ]; then
  printf '%s' "$out"
  exit 0
fi

[ -f "$CHART" ] || { echo "no $CHART" >&2; exit 1; }
grep -qx 'annotations:' "$CHART" || { echo "$CHART has no annotations block" >&2; exit 1; }
if grep -q 'artifacthub.io/changes:' "$CHART"; then
  echo "$CHART already carries artifacthub.io/changes; refusing to add a second" >&2
  exit 1
fi

# Inserted immediately after the `annotations:` line rather than appended to
# the file, so it does not depend on that block staying last.
#
# The block goes through a file rather than `awk -v`, because a -v assignment
# containing newlines is a GNU extension: BSD awk rejects it, and rejects it by
# writing a warning and carrying on rather than by failing, so the first
# symptom would be a released chart quietly missing the annotation.
BLOCK="$(mktemp)"
trap 'rm -f "$BLOCK" "$CHART.tmp"' EXIT
{ printf '  artifacthub.io/changes: |\n'; printf '%s' "$out" | sed 's/^/    /'; } > "$BLOCK"

awk -v blockfile="$BLOCK" '
  !inserted && $0 == "annotations:" {
    print
    while ((getline line < blockfile) > 0) print line
    close(blockfile)
    inserted = 1
    next
  }
  { print }
' "$CHART" > "$CHART.tmp"

grep -q '^  artifacthub.io/changes: |$' "$CHART.tmp" || {
  echo "insertion into $CHART produced nothing; leaving it alone" >&2
  exit 1
}
mv "$CHART.tmp" "$CHART"

echo "wrote $emitted change entries into $CHART" >&2

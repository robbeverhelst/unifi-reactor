#!/usr/bin/env bash
# Fails if a committed capture still contains a value that the sanitization
# policy in testdata/unifi/README.md requires to be redacted.
#
# Captures come from real hardware, so this runs in CI: the policy is easy to
# follow and equally easy to forget, and the cost of forgetting is a
# credential in a public repository.
set -euo pipefail

# Resolved before the cd below, because the expected placeholders are a fact
# about this repository's capture script rather than about the tree being
# scanned.
HACK_DIR="$(cd "$(dirname "$0")" && pwd)"

# An optional root exists so the guards below can be exercised against a
# throwaway tree of fixtures. With no argument this is the repository, which is
# how make test and hack/capture-unifi.sh both run it.
cd "${1:-$HACK_DIR/..}"
failed=0

# Fields that must never carry a real value.
SECRET_FIELDS='x_authkey|syslog_key|serial|anon_id|device_id|hash_id|external_id|setup_id'

# Fields whose value must be the placeholder the capture script writes, and
# nothing else.
#
# The tempting guard here is a list of carrier names never to publish, and it
# is the wrong way round: such a list has to contain the real value as a
# literal, in this file, in a public repository — which is the thing being
# removed, moved from a fixture to a script and labelled. It would also only
# ever catch the one carrier somebody thought to add.
#
# Required-to-equal catches every carrier instead, names none of them, and is
# the same move the MAC guard below already makes: the rule there is "inside
# the aa:bb:cc prefix", not "not one of these MACs".
#
# The placeholders are read out of hack/capture-unifi.sh rather than repeated
# here, so the value the capture writes and the value this requires cannot
# drift apart.
ISP_FIELDS='isp|isp_name|isp_organization'
placeholder() { sed -n "s/^$1='\(.*\)'\$/\1/p" "$HACK_DIR/capture-unifi.sh"; }
ISP_PLACEHOLDER="$(placeholder ISP)"
ISP_ORG_PLACEHOLDER="$(placeholder ISP_ORG)"
if [ -z "$ISP_PLACEHOLDER" ] || [ -z "$ISP_ORG_PLACEHOLDER" ]; then
  echo "hack/capture-unifi.sh no longer declares ISP and ISP_ORG, so the carrier"
  echo "guard below would accept anything. Fix that before trusting this script."
  exit 1
fi

# The counts are deliberately not guarded. There is no positive rule to write —
# every small integer is a plausible count — and enumerating forbidden ones
# would be the same inverted mistake as a carrier denylist. The capture script
# scaling them onto a placeholder site is the whole of the fix there.

for file in testdata/unifi/api/*.json testdata/unifi/webhooks/*.json; do
  [ -e "$file" ] || continue

  # Any of the above with something other than a redaction marker.
  while IFS= read -r hit; do
    echo "$file: unredacted secret field: $hit"
    failed=1
  done < <(grep -oE "\"($SECRET_FIELDS)\":\"[^\"]+\"" "$file" | grep -vE '"REDACTED"' || true)

  # Real-world addresses that should have been replaced with documentation ranges.
  while IFS= read -r hit; do
    # Skip anything that is not a valid dotted quad (version strings look alike).
    if ! printf '%s' "$hit" | awk -F. 'NF==4 && $1<256 && $2<256 && $3<256 && $4<256 {ok=1} END {exit !ok}'; then
      continue
    fi
    case "$hit" in
      # Private, loopback, documentation ranges, netmasks, and the public
      # resolvers UniFi legitimately lists as uptime-monitor targets.
      192.168.*|10.*|127.*|0.0.0.0) ;;
      203.0.113.*|198.51.100.*|192.0.2.*) ;;
      255.*) ;;
      1.1.1.1|8.8.8.8|8.8.4.4|9.9.9.9|208.67.222.222) ;;
      172.1[6-9].*|172.2[0-9].*|172.3[01].*) ;;
      *) echo "$file: routable IP address: $hit"; failed=1 ;;
    esac
  done < <(grep -oE '\b([0-9]{1,3}\.){3}[0-9]{1,3}\b' "$file" | sort -u || true)

  # A carrier field carrying anything but the placeholder. The offending value
  # is deliberately not echoed: it is a real carrier by definition, and CI logs
  # are as public as the fixture would have been.
  while IFS= read -r hit; do
    value="$(printf '%s' "$hit" | sed 's/.*: *"\(.*\)"$/\1/')"
    case "$value" in
      "$ISP_PLACEHOLDER"|"$ISP_ORG_PLACEHOLDER") continue ;;
    esac
    field="$(printf '%s' "$hit" | sed 's/^"\([a-z_]*\)".*/\1/')"
    echo "$file: $field is not the placeholder carrier."
    echo "  This capture was taken with the ISP fields in the tier that keeps the"
    echo "  captured value. Move them to the replaced-value tier in"
    echo "  hack/capture-unifi.sh, then re-derive the fixture by hand from the"
    echo "  committed one — do not edit this value in place and do not re-capture."
    failed=1
  done < <(grep -oE "\"($ISP_FIELDS)\" *: *\"[^\"]*\"" "$file" | sort -u || true)

  # MACs must be inside the aa:bb:cc documentation-ish prefix used by the captures.
  while IFS= read -r hit; do
    echo "$file: real-looking MAC address: $hit"
    failed=1
  done < <(grep -oiE '\b([0-9a-f]{2}:){5}[0-9a-f]{2}\b' "$file" | grep -viE '^aa:bb:cc:' | sort -u || true)
done

# Webhook captures carry one risk the API captures do not: a delivery arrives
# with Reactor's own shared secret in an Authorization header, so a fixture made
# by keeping "the request" rather than "these fields of the request" leaks the
# credential that protects the endpoint.
for file in testdata/unifi/webhooks/*.json; do
  [ -e "$file" ] || continue

  while IFS= read -r hit; do
    echo "$file: delivery credential material: $hit"
    failed=1
  done < <(grep -oiE '"(authorization|x-reactor-token|cookie|set-cookie|x-csrf-token|token|bodyBase64)"' "$file" || true)

  while IFS= read -r hit; do
    echo "$file: bearer credential: $hit"
    failed=1
  done < <(grep -oiE 'bearer [a-z0-9._~+/-]+' "$file" || true)
done

if [ "$failed" -ne 0 ]; then
  echo
  echo "Captures must be sanitized before committing — see testdata/unifi/README.md."
  exit 1
fi
echo "testdata captures are sanitized."

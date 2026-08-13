#!/usr/bin/env bash
# Fails if a committed capture still contains a value that the sanitization
# policy in testdata/unifi/README.md requires to be redacted.
#
# Captures come from real hardware, so this runs in CI: the policy is easy to
# follow and equally easy to forget, and the cost of forgetting is a
# credential in a public repository.
set -euo pipefail

cd "$(dirname "$0")/.."
failed=0

# Fields that must never carry a real value.
SECRET_FIELDS='x_authkey|syslog_key|serial|anon_id|device_id|hash_id|external_id|setup_id'

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

  # MACs must be inside the aa:bb:cc documentation-ish prefix used by the captures.
  while IFS= read -r hit; do
    echo "$file: real-looking MAC address: $hit"
    failed=1
  done < <(grep -oiE '\b([0-9a-f]{2}:){5}[0-9a-f]{2}\b' "$file" | grep -viE '^aa:bb:cc:' | sort -u || true)
done

if [ "$failed" -ne 0 ]; then
  echo
  echo "Captures must be sanitized before committing — see testdata/unifi/README.md."
  exit 1
fi
echo "testdata captures are sanitized."

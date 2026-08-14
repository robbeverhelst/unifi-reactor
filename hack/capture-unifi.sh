#!/usr/bin/env bash
# Capture UniFi API responses as test fixtures — allowlist first, redact second.
#
#   UNIFI_URL=https://192.168.1.1 UNIFI_API_KEY=... ./hack/capture-unifi.sh
#
# Responses from stat/device are whole device records: ~8KB each, containing
# management keys (x_authkey), syslog keys, adoption identifiers, and topology
# tables. The parser reads about a dozen fields. Committing the rest was how a
# live credential ended up in this repository's history.
#
# So this script keeps ONLY the fields named below and drops everything else,
# rather than stripping the sensitive ones it happens to think of. Adding a
# field to a parser means adding it here too — deliberately, one at a time.
set -euo pipefail

cd "$(dirname "$0")/.."
: "${UNIFI_URL:?set UNIFI_URL, e.g. https://192.168.1.1}"
: "${UNIFI_API_KEY:?set UNIFI_API_KEY}"
SITE="${UNIFI_SITE:-default}"
OUT=testdata/unifi/api
mkdir -p "$OUT"

api() {
  curl -sk --fail -m 20 -H "X-API-KEY: $UNIFI_API_KEY" "$UNIFI_URL/proxy/network/api/s/$SITE/$1"
}

# Placeholders. Real values never reach disk.
PUB_IP='203.0.113.10'   # TEST-NET-3
GW_IP='203.0.113.1'
MAC='aa:bb:cc:00:11:22'

# --- allowlists -------------------------------------------------------------
# A WAN port: only what the uplink decision and the docs need.
WAN='{is_uplink, up, ifname, name, speed, ip: (if .ip then "'"$PUB_IP"'" else null end)}'

# A device record. vbms_table is pure battery telemetry with no identifiers,
# so it is kept whole; everything else is named explicitly.
DEVICE='{
  model, type, name, state, adopted, version, displayable_version,
  disconnection_reason,
  upgradable, upgrade_to_firmware, required_version, safe_for_autoupgrade,
  model_in_eol, model_in_lts,
  has_temperature, has_fan, overheating, general_temperature,
  temperatures: (if .temperatures then [.temperatures[] | {name, type, value}] else null end),
  wan1: (if .wan1 then (.wan1 | '"$WAN"') else null end),
  wan2: (if .wan2 then (.wan2 | '"$WAN"') else null end),
  uplink: (if .uplink then {name: .uplink.name, type: .uplink.type} else null end),
  last_wan_status,
  isp: (.active_geo_info.WAN.isp_name // null),
  vbms_table,
  outlet_table: (if .outlet_table then [.outlet_table[] | {index, name, relay_state, relay_group}] else null end)
} | with_entries(select(.value != null))'

echo "capturing stat/device -> gateway + UPS"
api stat/device > /tmp/cap-device.json
jq '{meta: {rc: "ok"}, data: [.data[] | select(.model == "UDMPRO") | '"$DEVICE"']}' \
  /tmp/cap-device.json > "$OUT/stat-device-gateway.json"
jq '{meta: {rc: "ok"}, data: [.data[] | select(.vbms_table != null) | '"$DEVICE"' | .mac = "'"$MAC"'"]}' \
  /tmp/cap-device.json > "$OUT/stat-device-ups.json"

# A switch and an access point. Neither has ever been captured, and between them
# they are the ground truth for temperature (#11), PoE (#14) and the firmware
# flags (#12) — the three parsers currently written to a documented shape rather
# than to an observation. The UPS 2U reports no thermals and no PoE, so it
# cannot settle any of them.
#
# Only the first of each is kept: one record is enough to learn a field's shape,
# and every extra one is another device name and another 8KB to sanitize.
#
# Their names are replaced with placeholders, unlike the gateway's and the UPS's
# — those two kept the console's own defaults for that hardware, while a switch
# or an AP is usually named after a room or a person. A device name is the
# `device.<name>` state key, so it is API-shaped and belongs in a fixture; whose
# room it is does not.
echo "capturing stat/device -> switch + access point"
jq '{meta: {rc: "ok"}, data: [[.data[] | select(.type == "usw" and .vbms_table == null)][0] | select(. != null) | '"$DEVICE"' | .mac = "'"$MAC"'" | .name = "Switch"]}' \
  /tmp/cap-device.json > /tmp/cap-switch.json
jq '{meta: {rc: "ok"}, data: [[.data[] | select(.type == "uap")][0] | select(. != null) | '"$DEVICE"' | .mac = "'"$MAC"'" | .name = "Access Point"]}' \
  /tmp/cap-device.json > /tmp/cap-ap.json
for pair in "switch:/tmp/cap-switch.json" "ap:/tmp/cap-ap.json"; do
  kind="${pair%%:*}"; file="${pair#*:}"
  if [ "$(jq '.data | length' "$file")" -eq 0 ]; then
    echo "  no $kind adopted on this site; stat-device-$kind.json not written"
    rm -f "$file"
    continue
  fi
  mv "$file" "$OUT/stat-device-$kind.json"
  echo "  wrote stat-device-$kind.json"
done

echo "capturing stat/health"
api stat/health | jq '{meta: {rc: "ok"}, data: [.data[] | {
  subsystem, status, num_user, num_guest, num_ap, num_adopted, num_disconnected, num_pending, num_gw, num_sta,
  wan_ip: (if .wan_ip then "'"$PUB_IP"'" else null end),
  gateways: (if .gateways then ["'"$GW_IP"'"] else null end),
  isp_name, isp_organization,
  uptime_stats
} | with_entries(select(.value != null))]}' > "$OUT/stat-health.json"

echo "capturing integration API info + sites"
curl -sk --fail -m 20 -H "X-API-KEY: $UNIFI_API_KEY" "$UNIFI_URL/proxy/network/integration/v1/info" \
  | jq '{applicationVersion}' > "$OUT/integration-info.json"
curl -sk --fail -m 20 -H "X-API-KEY: $UNIFI_API_KEY" "$UNIFI_URL/proxy/network/integration/v1/sites" \
  | jq '{offset, limit, count, totalCount, data: [.data[] | {id: "00000000-0000-0000-0000-0000000000ff", internalReference, name}]}' \
  > "$OUT/integration-sites.json"

rm -f /tmp/cap-device.json
echo
./hack/verify-testdata.sh
echo "captured into $OUT"

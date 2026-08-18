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

# --- the two tiers ----------------------------------------------------------
# Everything that survives the allowlist below is in exactly one of two tiers,
# and adding a field means filing it in one of them on purpose:
#
#   1. kept as captured — the value is API shape. Subsystem names, statuses,
#      firmware versions, relay states, battery telemetry. A reader learns
#      something about UniFi from it and nothing about whose console it is.
#   2. kept in shape, value replaced — the field is API shape but its value
#      describes this particular household. Public IPs, MACs, device _ids,
#      device and port and outlet names, the carrier, and the counts.
#
# Tier 2 is not "the sensitive fields": none of it is a credential. It is "the
# fields whose value is about the owner rather than about the API". isp_name
# and isp_organization sat in tier 1 through two releases because nobody put
# that question to them — they are real fields a parser reads, so being in the
# allowlist at all looked like the deliberate decision. See #94. Being read is
# what earns a field a place here; it is not what earns its value one.

# Tier 2 placeholders. Real values never reach disk.
PUB_IP='203.0.113.10'   # TEST-NET-3
GW_IP='203.0.113.1'
MAC='aa:bb:cc:00:11:22'
DEVICE_ID='000000000000000000000042'   # the outlet write PUTs to rest/device/<_id>
# Two words, so a fixture still exercises the slugifier's space rule, and
# unmistakably not a carrier anybody buys service from.
ISP='Example Telecom'
ISP_ORG='Example Telecom NV'
# One placeholder site for every count. A stat/health response counts the
# household — the people, their phones, the access points on their walls — and
# none of that is API shape. Only three counts are read by anything
# (num_adopted, num_disconnected and num_ap on the wlan subsystem) and they are
# read as none/some/all rather than as numbers, so scaling every count onto
# this site costs no fidelity and keeps num_ap + num_disconnected == num_adopted.
CLIENTS=10    # any non-zero number of clients or stations
DEVICES=4     # adopted devices, and adopted access points
GATEWAYS=1    # a site has one gateway: that is shape, not inventory

# --- allowlists -------------------------------------------------------------
# A WAN port: only what the uplink decision and the docs need.
WAN='{is_uplink, up, ifname, name, speed, ip: (if .ip then "'"$PUB_IP"'" else null end)}'

# A device record. vbms_table is pure battery telemetry with no identifiers,
# so it is kept whole; everything else is named explicitly.
#
# The port_table projection carries what BOTH port readers need: poe_enable,
# poe_power and poe_class for the poe state key, and is_uplink, port_poe and a
# name for the write path's guard on a power-cycle. Keeping only one set would
# make the first switch capture useless to the other half and look like evidence
# that its fields do not exist.
#
# Port names are replaced with their index, for the reason the switch's own name
# is replaced: a port is usually named after the room or the person on the end
# of it. The write path matches on that name, so what a fixture needs to carry
# is the shape and the correlation with port_idx, not somebody's study.
#
# Outlet names are replaced with the console's own placeholder for exactly the
# same reason, and it became load-bearing when outlets became switchable: an
# outlet is named after whatever is plugged into it, and the outlet write
# matches on that name. The placeholder is also what an unnamed outlet really
# reports, so a fixture carrying it is not pretending — it is the state every
# console starts in, and the one Reactor refuses to switch.
#
# The carrier is replaced for the reason the public IP is. `isp` is a real
# state key, so a fixture has to carry *a* carrier — it never had to carry the
# true one, and the true one says which company bills this address.
#
# outlet_caps and outlet_overrides are here because the write path reads the
# first and modifies the second. Both were confirmed present on real hardware on
# 2026-08-15; the fixture committed before that date predates this projection
# and carries neither, which is why the write path is exercised against
# hack/mock-unifi rather than against a capture.
#
# mbb is the most sensitive block this API has shown so far, and its projection
# is the six booleans-and-a-slot the data.usage key reads — nothing else. A real
# SIM entry also carries the SIM's iccid (its serial number), the modem's imei,
# the PIN/PUK lock state and retry counters, the carrier's name in spn, and
# mcc/mnc/asn which together identify the country and network operator. None of
# that is redacted: it is absent, which is the only projection under which it
# cannot leak. rxbytes and txbytes are counters rather than identifiers, but
# nothing reads them, and being read is what earns a field a place here.
DEVICE='{
  model, type, name, state, adopted, version, displayable_version,
  disconnection_reason,
  upgradable, upgrade_to_firmware, required_version, safe_for_autoupgrade,
  model_in_eol, model_in_lts,
  has_temperature, has_fan, overheating, general_temperature,
  temperatures: (if .temperatures then [.temperatures[] | {name, type, value}] else null end),
  total_max_power,
  port_table: (if .port_table then [.port_table[] | {
    port_idx, poe_enable, poe_power, poe_class, is_uplink, port_poe,
    name: ("port-" + (.port_idx | tostring))
  }] else null end),
  uplink: (if .uplink then {name: .uplink.name, type: .uplink.type} else null end),
  last_wan_status,
  isp: (if .active_geo_info.WAN.isp_name then "'"$ISP"'" else null end),
  mbb: (if .mbb.sim then {sim: [.mbb.sim[] | {
    active, slot, card_present, has_data_plan, data_warning, data_limited
  }]} else null end),
  vbms_table,
  outlet_table: (if .outlet_table then [.outlet_table[] | {
    index, relay_state, relay_group, outlet_caps,
    name: ("Outlet " + (.index | tostring))
  }] else null end),
  outlet_overrides: (if .outlet_overrides then [.outlet_overrides[] | {
    index, relay_state, cycle_enabled,
    name: ("Outlet " + (.index | tostring))
  }] else null end)
}
# Every wanN the console reports, through the WAN projection above. This used
# to name wan1 and wan2, which is how a gateway with a cellular backup — which
# reports it as wan3 — could never produce a fixture reproducing #104. The wanN
# fields themselves stay in the allowlist one at a time exactly as before; what
# is dynamic is only which N exist.
+ (with_entries(select((.key | test("^wan[0-9]+$")) and .value != null)) | map_values('"$WAN"'))
| with_entries(select(.value != null))'

echo "capturing stat/device -> gateway + UPS"
api stat/device > /tmp/cap-device.json
jq '{meta: {rc: "ok"}, data: [.data[] | select(.model == "UDMPRO") | '"$DEVICE"']}' \
  /tmp/cap-device.json > "$OUT/stat-device-gateway.json"
# The UPS gets a placeholder _id as well as a placeholder MAC: the outlet write
# PUTs to rest/device/<_id>, so the fixture has to carry the shape of an address
# without carrying a real one.
jq '{meta: {rc: "ok"}, data: [.data[] | select(.vbms_table != null) | '"$DEVICE"'
      | .mac = "'"$MAC"'" | ._id = "'"$DEVICE_ID"'"]}' \
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

# Every count here is scaled onto the placeholder site declared above. An
# absent count stays absent and a zero stays zero — the parsers branch on both,
# and "no guests" and "no access points adopted" are shapes rather than numbers
# — while the AP triple keeps the only relation anything reads out of it:
# whether none, some, or all of the adopted access points are disconnected.
echo "capturing stat/health"
api stat/health | jq '
def scaled(n): if . == null then null elif . == 0 then 0 else n end;
{meta: {rc: "ok"}, data: [.data[]
  | . as $s
  # The wan subsystem counts gateways; the others count devices and access
  # points. Two placeholders, so num_adopted and num_gw cannot contradict.
  | (if $s.subsystem == "wan" then '"$GATEWAYS"' else '"$DEVICES"' end) as $adopted
  | (if ($s.num_disconnected // 0) == 0 then 0
     elif ($s.num_disconnected // 0) >= ($s.num_adopted // 0) then $adopted
     else 1 end) as $down
  | {
  subsystem, status,
  num_user: (.num_user | scaled('"$CLIENTS"')),
  num_guest: (.num_guest | scaled('"$CLIENTS"')),
  num_ap: (if .num_ap == null then null else $adopted - $down end),
  num_adopted: (.num_adopted | scaled($adopted)),
  num_disconnected: (if .num_disconnected == null then null else $down end),
  num_pending: (.num_pending | scaled(1)),
  num_gw: (.num_gw | scaled('"$GATEWAYS"')),
  num_sta: (.num_sta | scaled('"$CLIENTS"')),
  wan_ip: (if .wan_ip then "'"$PUB_IP"'" else null end),
  gateways: (if .gateways then ["'"$GW_IP"'"] else null end),
  isp_name: (if .isp_name then "'"$ISP"'" else null end),
  isp_organization: (if .isp_organization then "'"$ISP_ORG"'" else null end),
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

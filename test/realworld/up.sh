#!/bin/bash
# Brings up the L1 real-service rig: real mosquitto, real third-party Modbus
# server, real Home Assistant (demo integration = real controllable
# entities). Emits test/realworld/env for run.sh. Idempotent: always fresh.
set -euo pipefail
cd "$(dirname "$0")"

docker rm -f anetlink-ha anetlink-mosq anetlink-modbus anetlink-matter anetlink-opcua \
  anetlink-rtsp anetlink-onvif >/dev/null 2>&1 || true
rm -rf ha-config && mkdir -p ha-config
printf 'demo:\ndefault_config:\n' > ha-config/configuration.yaml

docker run -d --name anetlink-mosq -p 18830:1883 eclipse-mosquitto:2 \
  mosquitto -c /mosquitto-no-auth.conf >/dev/null
docker run -d --name anetlink-modbus -p 15020:5020 oitc/modbus-server >/dev/null
docker run -d --name anetlink-opcua --network host \
  mcr.microsoft.com/iotedge/opc-plc:latest --pn=50000 --autoaccept --unsecuretransport >/dev/null
docker run -d --name anetlink-ha -p 18123:8123 \
  -v "$PWD/ha-config:/config" homeassistant/home-assistant:stable >/dev/null
docker run -d --name anetlink-matter --network host \
  -v anetlink-matter-storage:/root/.matter -v anetlink-matter-npm:/root/.npm \
  node:22-slim bash -c "npm config set registry \${NPM_REGISTRY:-https://registry.npmmirror.com} && npx -y -p @matter/examples matter-device --type light" >/dev/null

# Camera rig: a real RTSP server carrying a real H.264 stream, and a real
# ONVIF Profile S device service in front of it. The ONVIF server resolves
# its advertised address from a MAC, so it must be given one this host has.
CAMDIR="$PWD/../testbed/camera"
IFACE=$(ip -o -4 route show to default | awk '{print $5; exit}')
MAC=$(ip -o link show "$IFACE" | grep -o 'link/ether [0-9a-f:]*' | awk '{print $2}')
HOSTIP=$(ip -4 -o addr show "$IFACE" | awk '{print $4}' | cut -d/ -f1)
if [ -n "$MAC" ]; then
  sed -i "s|mac: .*|mac: $MAC|" "$CAMDIR/onvif.yaml"
  docker run -d --name anetlink-rtsp --network host \
    -v "$CAMDIR/mediamtx.yml:/mediamtx.yml:ro" bluenviron/mediamtx:latest-ffmpeg >/dev/null
  sleep 8   # let ffmpeg establish the stream before ONVIF advertises it
  docker run -d --name anetlink-onvif --network host \
    -v "$CAMDIR/onvif.yaml:/onvif.yaml:ro" ghcr.io/daniela-hase/onvif-server:latest >/dev/null
else
  echo "no default-route interface found; skipping the camera rig" >&2
fi

echo "waiting for Home Assistant…"
for i in $(seq 1 120); do
  code=$(curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:18123/ || true)
  [ "$code" = "200" ] || [ "$code" = "302" ] && break
  sleep 3
done

AUTH=$(curl -s -X POST http://127.0.0.1:18123/api/onboarding/users \
  -H 'Content-Type: application/json' \
  -d '{"client_id":"http://127.0.0.1:18123/","name":"anetlink","username":"anetlink","password":"anetlink-rw-2026","language":"en"}' \
  | python3 -c "import json,sys; print(json.load(sys.stdin)['auth_code'])")
HATOKEN=$(curl -s -X POST http://127.0.0.1:18123/auth/token \
  -d "grant_type=authorization_code&code=$AUTH&client_id=http://127.0.0.1:18123/" \
  | python3 -c "import json,sys; print(json.load(sys.stdin)['access_token'])")
for step in core_config analytics; do
  curl -s -X POST "http://127.0.0.1:18123/api/onboarding/$step" \
    -H "Authorization: Bearer $HATOKEN" -H 'Content-Type: application/json' \
    -d '{"client_id":"http://127.0.0.1:18123/"}' -o /dev/null
done

cat > env <<ENVEOF
export ANETLINK_MQTT_BROKER=tcp://127.0.0.1:18830
export ANETLINK_MODBUS_ADDR=127.0.0.1:15020
export ANETLINK_HA_URL=ws://127.0.0.1:18123/api/websocket
export ANETLINK_HA_TOKEN=$HATOKEN
export ANETLINK_MATTER_EXPECT=3840
export ANETLINK_OPCUA_ENDPOINT=opc.tcp://127.0.0.1:50000
export ANETLINK_RTSP_URI=rtsp://127.0.0.1:8554/cam
export ANETLINK_ONVIF_XADDR=http://$HOSTIP:8081/onvif/device_service
# Hikvision ISAPI and Dahua CGI are proprietary and have no simulator: point
# these at real hardware to run their L2 tiers.
# export ANETLINK_HIKVISION_ADDR=192.168.1.64
# export ANETLINK_HIKVISION_USER=admin
# export ANETLINK_HIKVISION_PASS=...
# export ANETLINK_DAHUA_ADDR=192.168.1.108
# export ANETLINK_DAHUA_USER=admin
# export ANETLINK_DAHUA_PASS=...
ENVEOF
echo "L1 rig up. source test/realworld/env && ./test/realworld/run.sh"

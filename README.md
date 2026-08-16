# ANetLink

**The physical world, as ANet capabilities.** ANetLink models devices
(DeviceProfile: capabilities × access mask × trust ladder), speaks the smart-
home protocol zoo through pluggable adapters, and exposes exactly one
capability surface northbound — to Claude/IDEs via MCP, and to the
[ANet](https://github.com/ANetResearch/ANet) daemon via the C1 provider
contract. It is useful with zero network: one machine, one adapter, and your
devices become callable.

Status: **v0 skeleton** — contracts, runtime, trust rules and the sim adapter
are real and tested; protocol adapters land per the coverage matrix below.

## Doctrine (K207 A3/A3′/A5)

- **Pure Go first**; the only registered cross-language exemption is the
  bundled OTBR for Thread, run as a managed subprocess, invisible externally.
- **双后端制 / probe first, fallback always**: every adapter ships backends in
  priority order — host facilities (BlueZ, an existing OTBR, a running
  zigbee2mqtt or Home Assistant) are probed and used when alive, and a
  self-contained fallback always exists. 永远都要兜底。
- **One surface**: north of the runtime nobody sees adapters or backends —
  only devices, capabilities, and effects.
- **Honest effects**: an invocation can claim a verifiable `OK` only with
  measured metrics AND a device trust level that admits verification
  (T2+). The runtime downgrades everything else to `UNVERIFIED` — adapters
  and quirks cannot raise trust.

## Coverage matrix (all in v4.0)

| Protocol | Probe backend | Self-contained fallback |
|---|---|---|
| Modbus | — (direct) | pure-Go client |
| MQTT (zigbee2mqtt/Tasmota) | broker probe | bridge itself |
| Home Assistant | HA WebSocket/REST | (native rows below) |
| Zigbee | running zigbee2mqtt | pure-Go serial coordinator stack |
| BLE | BlueZ via D-Bus | pure-Go kernel HCI |
| BT Mesh | bluetooth-meshd | pure-Go provisioning + models |
| Matter (IP) | existing controller/HA | pure-Go stack (SPAKE2+/CASE + codegen clusters) |
| Thread | host OTBR | bundled OTBR (managed subprocess) |

## Try it

```bash
go run ./cmd/anetlinkd --demo
# device sim/lamp-1 online=true trust=T4 profile=bafyrei…
# demo: light.onoff on=true → status=OK verifiable=true metrics=map[power_state:1]
```

## Layout

`profile/` device model + trust ladder · `sdk/` C4 adapter contract, backend
probing, honest EffectBuilder · `runtime/` device table, routing, trust
enforcement · `adapters/` protocol adapters (start at `sim`) ·
`cmd/anetlinkd` the daemon.

Design corpus: K207 (anet4 module architecture), DocsANetLink 00–10.
Kernel types come from [ANetCore](https://github.com/ANetResearch/ANetCore)
(Apache-2.0): `effect.Effect`, `tsir.EffectRecord`, CoreDet-CBOR, CIDs.

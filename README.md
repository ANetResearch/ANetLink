# ANetLink

> **AgentNetwork connects intelligence. ANetLink connects the physical world.**
>
> ANetLink provides a protocol-independent, capability-oriented and
> trust-aware link between heterogeneous physical devices and AgentNetwork —
> ANet's **physical-world narrow waist** (AELP + TSIR-D). Positioning: K210.

**The physical world, as ANet capabilities.** ANetLink models devices
(DeviceProfile: capabilities × access mask × trust ladder), speaks the smart-
home protocol zoo through pluggable adapters, and exposes exactly one
capability surface northbound — to Claude/IDEs via MCP, and to the
[ANet](https://github.com/ANetResearch/ANet) daemon via the C1 provider
contract. It is useful with zero network: one machine, one adapter, and your
devices become callable.

Status: **v0.2** — contracts, runtime, trust rules, MCP + C1 northbound and
three adapters (modbus / mqtt-bridge / ha-bridge) are real and tested; native
stacks land per the coverage matrix below.

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
| Modbus ✅ | — (direct) | pure-Go client |
| MQTT (zigbee2mqtt/Tasmota) ✅ | broker probe | bridge itself |
| Home Assistant ✅ | HA WebSocket/REST | (native rows below) |
| **OPC UA (industrial)** ✅ | — (direct) | pure-Go gopcua client |
| **BACnet/IP (building)** ✅ | — (direct) | pure-Go ASHRAE 135 codec |
| **CAN (vehicle/industrial)** ✅ | SocketCAN probe | pure-Go signal codec |
| Zigbee ✅ | running zigbee2mqtt (MQTT bridge) | pure-Go Z-Stack serial coordinator |
| BLE ✅ | — | pure-Go kernel HCI socket (no BlueZ, no cgo) |
| BT Mesh ✅ | — | pure-Go CMAC/CCM crypto + models |
| Matter (IP) 🔶 | existing controller/HA | pure-Go stack — **PASE verified against a real device**; commissioning flow next |
| Thread ✅ | host OTBR (REST) | bundled OTBR as managed subprocess (the one exemption) |

## 使用手册

完整操作指南见 [docs/MANUAL-zh.md](docs/MANUAL-zh.md)：安装、配置、每个适配器、
MCP/C1 集成、信任模型、测试三层、故障排查、写自己的适配器。

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

License: ANet Open Source License.

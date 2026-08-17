# ANetLink 使用手册

> 把物理世界变成 ANet 能力。一份从零到能控真实设备的完整指南。
>
> 适用版本：anet4 开发线（2026-08）。协议内核见 [ANetCore](https://github.com/ANetResearch/ANetCore)；
> 设计总纲见 anet 母仓 K207，本手册是面向使用者的操作视角。

---

## 目录

1. [这是什么](#1-这是什么)
2. [5 分钟上手](#2-5-分钟上手)
3. [核心概念](#3-核心概念)
4. [安装与构建](#4-安装与构建)
5. [配置文件详解](#5-配置文件详解)
6. [适配器逐个说明](#6-适配器逐个说明)
7. [北向接口：MCP 与 C1](#7-北向接口mcp-与-c1)
8. [信任模型 T0–T4](#8-信任模型-t0t4)
9. [裁剪发行版（build tag）](#9-裁剪发行版build-tag)
10. [测试三层制](#10-测试三层制)
11. [故障排查](#11-故障排查)
12. [写一个自己的适配器](#12-写一个自己的适配器)
13. [命令与端口速查](#13-命令与端口速查)

---

## 1. 这是什么

**ANetLink 是一个自带一切依赖的"电器"**：跑起来一个 `anetlinkd` 进程，它

- 用**可插拔适配器**说各种智能家居协议（Modbus、MQTT/zigbee2mqtt、Home Assistant，
  以及在建的原生 Zigbee/BLE/Matter/Thread）；
- 把每台设备建模成一个**内容寻址的 DeviceProfile**（能力 × 访问位 × 信任等级）；
- 向北**只暴露一个能力面**——给 Claude/IDE 走 MCP，给 anet daemon 走 C1 合同；
- 对每次执行给出**诚实的效果**：能验证才叫成功，验证不了就明说 UNVERIFIED，
  绝不假报 OK。

**一句话价值**：装了 `anetlinkd` + 一个适配器，即使一个联系人都没有、hub 都不连，
它也立刻有用——你家的 Claude Code 能直接控制你家的灯。这是"单机就有用"的产品。

**三条铁律**（贯穿全手册）：

| 铁律 | 含义 |
|---|---|
| 纯 Go 优先 | 分发的运行时组件不引入跨语言依赖；缺哪个生态的适配就用 Go 按协议自己写 |
| 零 OS 假定 | 不假定宿主装了 BlueZ/OTBR；存在则探测利用，不存在则自研兜底——**永远都要兜底** |
| 一个服务面 | 北向永远只见设备与能力，永不见适配器与后端 |

---

## 2. 5 分钟上手

```bash
# 1. 拉取并构建（需要 Go 1.26+）
git clone https://github.com/ANetResearch/ANetLink.git
cd ANetLink
CGO_ENABLED=0 go build -o anetlinkd ./cmd/anetlinkd

# 2. 空跑 demo：内建模拟灯，验证 invoke 全链路
./anetlinkd --demo
# device sim/lamp-1 online=true trust=T4 profile=bafyrei…
# demo: light.onoff on=true → status=OK verifiable=true metrics=map[power_state:1]

# 3. 作为 MCP 服务给 Claude/IDE 用（stdio）
./anetlinkd --mcp
```

在 Claude Code 里把它注册为 MCP server（`claude mcp add anetlink -- /path/to/anetlinkd --mcp`），
然后就能让 Claude 调用 `devices_list` / `device_invoke` 控制设备。

要接真实设备，**先让它自己找**：

```bash
# 扫描所有已编译适配器能触达的网络，打印发现的设备与建议配置
./anetlinkd --scan

# 直接写成配置文件（不会覆盖已存在的文件）
./anetlinkd --scan --scan-out config.json
./anetlinkd --config config.json --mcp
```

对已跑 Home Assistant 的用户，只需给出 URL + token，`--scan` 就会列出全部实体
（实测：真实 HA 上一次扫出 31 个实体，带友好名、能力与信任级，无需手写任何设备）。
Zigbee 协调器会走 ZDO 邻居表自动枚举节点及其集群；BACnet 用 Who-Is 广播；
zigbee2mqtt 读它的 retained 设备表。**没有设备配置项时，Zigbee 适配器直接问网络。**

在 Claude 里也可以直接问——MCP 工具 `devices_discover` 返回发现结果与可粘贴的配置片段。

**买了新设备要配对**（Zigbee 等支持入网的协议）：

```bash
./anetlinkd --config config.json --pair 60s   # 开放入网 60 秒，报告谁加入了
```

或在 Claude 里说"我刚买了个传感器"——MCP 工具 `devices_pair` 会开窗口、等设备加入、
自动问出它的端点与集群，并给出可用配置。窗口结束后网络自动关闭（Zigbee 上限 254 秒）。

---

## 3. 核心概念

### DeviceProfile（设备档案）
一台设备"能做什么"的机器可读描述，是内容寻址的 CoreDet-CBOR 对象：

```
DeviceProfile {
  Schema   uint     # 档案模式主版本
  Vendor   string
  Model    string
  Protocol string   # "modbus" | "mqtt" | "homeassistant" | ...
  Caps     []Capability   # 能力列表（顺序有意义）
  MaxTrust Trust    # 此设备+链路的信任上限
}
Capability { ID, Access, Unit }
```

- **能力 ID** 用点分类法：`light.onoff`、`sensor.temperature`、`switch.onoff`。
- **访问位**是三比特掩码：`STATE`(0b001 主动状态流) `SET`(0b010 执行) `GET`(0b100 读取)。
- 每个 Profile 有稳定 **CID**（`profile.CID()`），设备型号变了 CID 就变——可用于缓存与去重。

### Effect（执行效果）
每次能力调用返回一个 `effect.Effect`（来自 ANetCore，C1/C4 共用）：

```
Effect { Status, Record?, Message }
Status ∈ { OK, UNVERIFIED, FAILED, UNAVAILABLE }
```

**状态轴与度量记录严格分离**，这是 ANetLink 的诚实核心：

| 状态 | 含义 |
|---|---|
| `OK` | 执行了，且效果**可验证**——Record 带着支撑的度量 |
| `UNVERIFIED` | 命令发出去了，但当前信任级拿不到可验证的效果信号。**不是失败** |
| `FAILED` | 执行失败（读回与请求相悖也算） |
| `UNAVAILABLE` | 调用时目标不可达 |

只有 `OK` + 非空 Record 才 `Verifiable()`。适配器用 `EffectBuilder` 构造效果，
**无度量就调不出 OK**——`Verified()` 没配 `Metric()` 时 Build 自动降级为 UNVERIFIED，
结构上杜绝假报。

### Adapter / Backend（适配器 / 后端）
- **适配器**说一个协议族（modbus、mqtt…），实现 C4 接口（见 §12）。
- 一个适配器可有**多个后端**，按优先级探测：宿主设施（BlueZ、已跑的 z2m/HA）活着就用，
  否则落到自研兜底路径。这叫**双后端制**。

---

## 4. 安装与构建

**前置**：Go 1.26+。运行时**不需要 cgo、不需要 C 编译器**（`CGO_ENABLED=0`），
所以交叉编译、静态二进制、塞进 scratch 容器都免费。

```bash
# 默认全含（所有适配器编入）
CGO_ENABLED=0 go build -o anetlinkd ./cmd/anetlinkd

# 交叉编译到 ARM（边缘盒子）
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o anetlinkd-arm64 ./cmd/anetlinkd

# 只要模拟器 + Modbus（裁掉 mqtt/ha，见 §9）
CGO_ENABLED=0 go build -tags no_mqtt,no_ha -o anetlinkd-min ./cmd/anetlinkd
```

产物是单个静态二进制，无外部运行时依赖。

---

## 5. 配置文件详解

`config.json`（JSON）。所有段可选；不写某适配器段即不注册它。

```json
{
  "backend_override": {
    "mqtt": "zigbee2mqtt"
  },
  "adapters": {
    "sim": { "enabled": true },

    "modbus": {
      "devices": [
        {
          "id": "plc-1",
          "addr": "192.168.1.50:502",
          "unit_id": 1,
          "vendor": "acme",
          "model": "rtu-8",
          "caps": [
            { "id": "switch.onoff", "kind": "coil",    "address": 3, "access": 6 },
            { "id": "sensor.temp",  "kind": "holding", "address": 7, "access": 4,
              "scale": 0.1, "unit": "°C" }
          ]
        }
      ]
    },

    "mqtt": {
      "broker": "tcp://127.0.0.1:1883",
      "base_topic": "zigbee2mqtt"
    },

    "homeassistant": {
      "url": "ws://homeassistant.local:8123/api/websocket",
      "token": "<long-lived access token>"
    }
  }
}
```

**要点**：

- `backend_override` 是 A5 逃生口：强制某适配器用指定后端；**强制但探测失败 = 配置错误**
  （启动即报），不会静默降级。
- **配置了未编译进本二进制的适配器 = 响亮报错**。裁掉了 `mqtt` 却在 config 写 `mqtt` 段，
  启动直接失败并提示 `no_mqtt?`——绝不静默失能力。
- `access` 是三比特整数：STATE=1、SET=2、GET=4，按位或。可写可读的开关 = 6。

启动：`./anetlinkd --config config.json --mcp`（或 `--c1-socket /run/anetlink.sock` 接 daemon）。

---

## 6. 适配器逐个说明

### 6.1 sim（模拟器）
内建、恒可用、零依赖的模拟灯。用于 demo 和证明 C4 合同。默认开启（裸 `anetlinkd` 也有东西可看），
config 里 `"sim": {"enabled": false}` 显式关闭。信任 T4（进程内闭环）。

### 6.2 modbus（Modbus/TCP）
纯 Go 客户端（grid-x/modbus）。**静态配置**（Modbus 协议无发现）。

- **诚实模型 = T2**：每次写后必读回，**读回值才是 metric**。
- 读回与请求相悖 → `FAILED`；目标不可达 → `UNAVAILABLE`；从不假报 OK。
- 能力映射：`kind: "coil"`（线圈，布尔）或 `"holding"`（保持寄存器，可带 `scale`）。
- 调用：SET 用 `args.on`（coil）或 `args.value`（holding）；GET 无参数。

### 6.3 mqtt（zigbee2mqtt / Tasmota 桥）
纯 Go MQTT 客户端（paho）。**探测型后端**——骑在你已经跑着的 zigbee2mqtt 上。

- 探测语义严格：broker 通但没有 z2m 的 retained `bridge/devices` = 不可用。
- 发现：解析 z2m 的 `bridge/devices`，把 exposes（含嵌套 features）映射成能力。
- 诚实模型 = T2：`/set` 后等设备状态回声——**回声=VERIFIED，静默=UNVERIFIED**，
  陈旧缓存读也停在 UNVERIFIED。

### 6.4 homeassistant（HA 桥）
纯 Go WebSocket 客户端。**覆盖杠杆最大**——一条认证连接把 HA 的每个实体变成能力。

- 需要 HA 的 **long-lived access token**（HA → 用户资料 → 长期访问令牌）。
- 实体映射：`switch`/`light` → `<domain>.onoff`（SET|GET|STATE），`sensor` →
  `sensor.<device_class>` 带单位。
- 诚实模型 = T2：`call_service` 后等 `state_changed` 事件确认；**幂等执行**
  （对已开的灯再 turn_on 无事件）回退到活读状态兜底——读到目标态=VERIFIED、
  相反态=FAILED、读不到=UNVERIFIED。

### 6.5 opcua（OPC UA，工业）
纯 Go 客户端（gopcua）。ANetLink 跨入工业自动化的入口。

- OPC UA Variable → property，Method → action（R02 §4 映射）。
- **诚实模型 = T2**：写后读回，读回值即 metric；相悖 → FAILED，不可达 → UNAVAILABLE。
- 静态配置节点映射：`node_id`（如 `ns=2;s=Temperature`）、`access`。
- 调用：SET 用 `args.value`；GET 无参。

配置示例：
```json
"opcua": {
  "devices": [{
    "id": "plc-1", "endpoint": "opc.tcp://192.168.1.10:4840",
    "nodes": [
      { "id": "sensor.temperature", "node_id": "ns=2;s=Temp", "access": 4, "unit": "°C" },
      { "id": "climate.setpoint",   "node_id": "ns=2;s=SP",   "access": 6, "unit": "°C" }
    ]
  }]
}
```

### 6.6 bacnet（BACnet/IP，楼宇自动化）
纯 Go 手写 ASHRAE 135 编解码（BVLL/NPDU/APDU），不绑 C 库。

- 对象映射：AnalogInput/Output/Value、BinaryInput/Output/Value 的 Present_Value。
- **诚实模型 = V2/A1**：写后读回；BACnet/IP 无设备身份，故认证轴只到 A1（网络背书）。
- 配置：`{"id","addr":"host:47808","objects":[{"id","type","instance","access","unit"}]}`
  （type：0=AI 1=AO 2=AV 3=BI 4=BO 5=BV）。

### 6.7 can（CAN 总线，车/工业）
纯 Go over SocketCAN（go.einride.tech/can）。

- 信号映射：帧内 little-endian 位段 + scale/offset → 能力。
- **诚实模型**：读取解码值 = V2；**写入 = V1 UNVERIFIED**——传统 CAN 无应用层
  ack、无设备身份（A0),适配器不得假装验证成功。
- 需要 SocketCAN 接口（`ip link add dev vcan0 type vcan && ip link set vcan0 up`）；
  接口不存在时探测如实报告不可用。

### 6.8 ble（Bluetooth LE）
**纯 Go 直连内核 HCI socket——不需要 BlueZ、不需要 D-Bus、不需要 cgo**（A3′ 零 OS 假定）。

- 扫描：原始 HCI 命令开关 LE scan，解码广播报文（地址/RSSI/名称/服务数据/厂商数据）。
- 传感器映射：声明式规则从广播载荷取值（offset/length/signed/scale）。
- **诚实模型 = V2/A0**：广播即观测值（V2），但**无设备身份**（A0）、**无 ack**；
  写入被拒绝而不是假装成功（GATT 写是后续增量）。
- 需要 `CAP_NET_RAW` 与蓝牙控制器；没有时探测如实报告不可用。

### 6.9 matter（Matter over IP，纯 Go）
七砖已就位：TLV 编解码 · mDNS 可配对发现 · SPAKE2+ · PASE 握手 · 消息帧 · UDP exchange ·
**加密会话**（HKDF 派生双向密钥 + 证明质询；AES-CCM 加密,消息头作为 AAD 认证;
CCM 对 NIST SP 800-38C 两组官方向量钉死）。
**PASE 已对真实 Matter 设备验证通过**（与 matter.js 虚拟灯完成完整握手、双方导出同一
会话密钥）；SPAKE2+ 同时对 RFC 9383 与 connectedhomeip 官方向量钉死。
后续：加密会话 + commissioning 全流程（arm-failsafe/CSR/AddNOC）→ 集群交互。

### 6.10 zigbee（原生 Z-Stack 协调器）
**纯 Go 自研栈**：自己实现 UNPI 帧（含 XOR 校验与流重同步）与 ZCL 编解码，
经串口驱动 TI Z-Stack 协调器棒——不需要 zigbee2mqtt、不需要 Python。

- 集群映射：OnOff/Level/Temperature/Humidity/Occupancy → 能力（R02 §4:
  Cluster→Capability、Attribute→Property）；多端点设备用 D50 端点层。
- **诚实模型 = V2/A2**：加密网络 + 每设备链路密钥，读回属性即 metric。
- 探测发 SYS_PING,普通串口设备冒充不了协调器。
- 配置：`{"port":"/dev/ttyUSB0","baud":115200,"devices":[{"id","short_addr",
  "endpoints":[{"id","endpoint","clusters":[6,1026]}]}]}`

### 6.11 btmesh（蓝牙 Mesh）
**密码学自己实现**：AES-CMAC（对 RFC 4493 向量钉死）、AES-CCM（Go 标准库没有,
按 NIST SP 800-38C 自写）、mesh 密钥调度（k2 网络密钥、k4 AID）。

- 模型映射：Element→Endpoint、Model→Capability（Generic OnOff / Sensor）。
- **诚实模型 = V2/A2**：AppKey 认证每条消息;用错密钥的 PDU 被拒绝而非解析。
- 未收到 status 的 set 报 UNVERIFIED。

### 6.12 thread（Thread 承载）
Thread 是**承载**不是应用协议,经 Border Router 触达设备。

- **双后端**：宿主已跑的 OTBR（REST API,优先）/ **打包 otbr-agent 受管子进程**
  ——这是全项目**唯一登记的跨语言豁免**（K207 A3′②）,未显式配置则完全不启用,
  普通构建保持纯 Go。
- 现状：边界路由器状态与设备投影可用;**CoAP 设备路径尚未实现,如实报
  UNAVAILABLE 而不是编造读数**。

### 6.13 覆盖总表
- **Matter**（纯 Go，长线）：已实现 TLV 编解码、mDNS 可配对发现、SPAKE2+、PASE 握手、
  消息帧层；剩 UDP exchange 环（MRP）+ commissioning 全流程后可真机配对。
- **Zigbee 原生 / BLE / 蓝牙 Mesh / Thread**：路线见 K207 §6 覆盖矩阵。Zigbee 无纯软件
  网络模拟（需协调器硬件），当前由 mqtt 桥覆盖。

---

## 7. 北向接口：MCP 与 C1

**北向只有一个能力面。** 两种消费方式，同一批设备与效果：

### 7.1 MCP（给 Claude / IDE）
`anetlinkd --mcp` 在 stdio 上起一个 MCP server，两个工具：

| 工具 | 作用 |
|---|---|
| `devices_list` | 列出所有设备：key、协议、在线态、信任级、可调能力、Profile CID |
| `device_invoke` | 调用一个能力，返回诚实效果（status/verifiable/metrics/message） |

设备 key 形如 `sim/lamp-1`（`<协议>/<设备ID>`）。这是**冷启动钩子**：零 hub 零 daemon，
Claude 直接控设备。

### 7.2 C1（给 anet daemon）
`anetlinkd --c1-socket /run/anetlink.sock` 在 Unix socket 上起 C1 wire（HTTP over UDS）：

```
GET  /v0/health        → 204
GET  /v0/capabilities  → {"capabilities": ["light.onoff@sim/lamp-1", ...]}
POST /v0/invoke        {"capability":"...","args":{...}} → effect
```

能力 id 形如 `light.onoff@sim/lamp-1`，**只在 ANetLink 侧铸造/解析**——daemon 永远不
知道"设备"这个概念，只知道"某能力可被调用"（这是 C1 红线）。

daemon 侧配置（ANet 仓 config.json）：
```json
{ "providers": { "anetlink": { "socket": "/run/anetlink.sock" } } }
```
之后 daemon 收到能力型委派会经 provider 注册表确定性执行，效果即交付物 + 签名回执，
并写入本地 AEL 证据链。

---

## 8. 信任模型：双轴 T0–T4

ANetLink 用**两条正交的轴**描述信任（K210 D49）：

| 轴 | 问题 | 字段 |
|---|---|---|
| **VerifyTrust**（V0–V4） | 我多确信这次执行**真的发生了** | `MaxTrust` / evidence.verify_trust |
| **AuthTrust**（A0–A4） | 我多确信这台设备**是谁** | `AuthTrust` / evidence.auth_trust |

二者独立：Modbus PLC 读回强（V2）但身份弱（A1）；云 API 可能身份不错却只有 ack（V1）。
认证轴：A0 未知 · A1 网关背书 · A2 协议认证会话 · A3 设备密钥 · A4 硬件根。

各适配器的诚实标注：Modbus V2/A1 · OPC UA V2/A2 · HA/MQTT V2/A1 · BACnet V2/A1 ·
CAN 读 V2 写 V1，A0 · sim V4/A4。

### 效果验证轴（VerifyTrust）详解

| 级 | 含义 | 典型 |
|---|---|---|
| T0 | 无法验证，命令消失在黑箱 | 单向红外发射 |
| T1 | 仅传输层 ack（云 API 封顶于此） | 纯云厂商 API |
| T2 | 设备自报状态读回 | Modbus 读回、HA 事件、z2m 回声 |
| T3 | 独立状态确认（另一条读路或第二传感器） | — |
| T4 | 物理闭环，效果被测量而非自报 | 进程内 sim |

**两条铁规**（runtime 强制、测试钉死）：

1. **link-minimum（链路下限）**：传输的信任上限封住下游，云 API 上不了 T2。
2. **信任只能钳、不能抬**：`MaxTrust < T2` 的设备即使适配器声称 OK，runtime 也强制
   降为 UNVERIFIED。适配器、quirk 都无权抬高信任。

`VerifiableFrom = T2`：只有 T2 及以上的设备，效果才可能是可验证的 OK。

---

## 9. 裁剪发行版（build tag）

适配器全部是**编译期插件**，用 `no_<name>` opt-out 剪裁（默认全含，发行版做减法）：

| tag | 剪掉 |
|---|---|
| `no_sim` | 模拟器 |
| `no_modbus` | Modbus 适配器 |
| `no_mqtt` | MQTT/z2m 桥 |
| `no_ha` | Home Assistant 桥 |

```bash
# 边缘盒子：只留 Modbus + BLE（示例）
CGO_ENABLED=0 go build -tags no_mqtt,no_ha,no_sim ./cmd/anetlinkd
```

CI 有 **unplug 矩阵**逐 tag 验证：剪裁后必须编译通过、启动成功——"可插拔"每次提交被验证，
不是口头承诺。裁掉的适配器若在 config 里被引用，启动响亮报错。

---

## 10. 测试三层制

ANetLink 的 IoT 测试**必须真实**。三层递进：

| 层 | 对象 | 何时跑 |
|---|---|---|
| **L0** 进程内假件 | FSM/协议逻辑（mbserver、mochi broker、fake-HA WS） | 每次提交（CI `test`） |
| **L1** 真实服务 | **真 mosquitto · 真第三方 Modbus 服务端 · 真 Home Assistant core**（demo 集成=真实可控实体） | 每次提交（CI `realworld`）+ 本地台架 |
| **L2** 真实硬件 | 真 Zigbee 棒+真设备、真 PLC、家里生产 HA | env 指向即跑（同一批测试） |

**跑 L1 本地台架**：
```bash
./test/realworld/up.sh      # 起真 HA + mosquitto + Modbus + matter.js 灯，自动 onboarding
source test/realworld/env
./test/realworld/run.sh      # 对真实服务跑 -tags realworld 测试
./test/realworld/down.sh     # 拆台架
```

**跑 L2 真硬件**：同一批 `-tags realworld` 测试，把 `ANETLINK_*` 环境变量指向真实目标：
- `ANETLINK_MODBUS_ADDR=<ip:port>` 指向真 PLC
- `ANETLINK_MQTT_BROKER=<broker>` + `ANETLINK_MQTT_LIVE_Z2M=1`（真 z2m，只读盘点，
  执行动作须操作员在场）
- `ANETLINK_HA_URL=<ws>` + `ANETLINK_HA_TOKEN=<token>` 指向家里 HA

**竞态检测**：CI 有 `race` job（`go test -race`）。本地无 gcc 可用
`docker run golang:1.26 go test -race ./...`。

**真实测试的价值**：mDNS 发现、HA 幂等执行两个 bug 都是真实测试首跑当场抓到的——
进程内假件永远测不出。

---

## 11. 故障排查

| 症状 | 原因 / 处置 |
|---|---|
| `adapter "x" configured but not compiled` | config 引用了被裁掉的适配器。重构建不带对应 `no_x` tag，或删掉 config 段 |
| `forced backend "y" unavailable` | `backend_override` 指定的后端探测失败。检查宿主设施（BlueZ/z2m/HA）是否真在跑 |
| MQTT 适配器发现 0 设备 | broker 通但没有 zigbee2mqtt 的 retained `bridge/devices`。确认 z2m 在同一 broker 上跑着 |
| HA 探测失败 `auth rejected` | token 无效或过期。重新在 HA 生成 long-lived token |
| 效果总是 UNVERIFIED | 设备信任级 < T2（云 API），或读回/事件没到。这是**诚实行为不是 bug**——命令发了但验证不了 |
| Matter 灯发现不到 | 配对窗 15 分钟制（Matter 规范），过期 `docker restart anetlink-matter` 重开新窗口 |
| `--demo` 输出 no devices | sim 被 `no_sim` 裁掉或 config 关闭。这是预期的优雅退化 |

**日志**：`anetlinkd` 启动时打印编译进的适配器、每个适配器选中的后端、设备清单与 CID。
`--mcp` 模式下日志走 stderr（stdout 归 MCP 会话）。

---

## 12. 写一个自己的适配器

实现 C4 的 `sdk.Adapter` 接口即可。骨架：

```go
package myproto

import (
    "context"
    "github.com/ANetResearch/ANetCore/effect"
    "github.com/ANetResearch/ANetLink/profile"
    "github.com/ANetResearch/ANetLink/sdk"
)

type Adapter struct{ /* … */ }

func (a *Adapter) Info() sdk.Info {
    return sdk.Info{Name: "myproto", Protocol: "myproto", Version: "0.1.0"}
}

// 双后端制：探测优先的后端在前，自研兜底在后（永远要有兜底）
func (a *Adapter) Backends() []sdk.Backend { return []sdk.Backend{hostBackend{}, nativeBackend{}} }

func (a *Adapter) Start(ctx context.Context, b sdk.Backend, sink sdk.Sink) error {
    // 发现设备，sink.UpsertDevice(...) 上报
    sink.UpsertDevice(sdk.Device{
        ID: "dev-1",
        Profile: &profile.DeviceProfile{
            Schema: 1, Protocol: "myproto",
            Caps: []profile.Capability{{ID: "light.onoff",
                Access: profile.AccessSet | profile.AccessGet}},
            MaxTrust: profile.T2,  // 诚实标注你真正能验证到的等级
        },
        Online: true,
    })
    return nil
}

func (a *Adapter) Stop(ctx context.Context) error { return nil }

func (a *Adapter) Invoke(ctx context.Context, call sdk.Call) (effect.Effect, error) {
    // 执行 + 读回，用 EffectBuilder 构造诚实效果
    return sdk.NewEffect().Metric("power_state", 1).Verified().Build(), nil
}
```

**诚实性由 `EffectBuilder` 保证**：`Verified()` 没有 `Metric()` 时自动降级为 UNVERIFIED。
你无法假报 OK。

挂进 `cmd/anetlinkd`：新建 `plugin_myproto.go`，`//go:build !no_myproto`，在 `init()` 里
`registerPlugin(...)`。这样它默认编入、可被 `no_myproto` 裁掉、可被 config 的 `adapters.myproto` 段配置。

**测试要求**：L0 假件（进程内 stub 设备）+ L1 真实服务（若有软件实现）。测试 sink 若有
并发回调必须加锁（见现有适配器）。

---

## 13. 命令与端口速查

**命令行**：
```
anetlinkd [flags]
  --config <path>       JSON 配置文件
  --mcp                 在 stdio 上服务 MCP 北向
  --c1-socket <path>    在 unix socket 上服务 C1 provider wire
  --demo                跑一次 demo invoke 后退出
```

**包地图**（14 个包）：
```
profile/            设备模型 + T0-T4 信任阶梯 + Clamp
sdk/                C4 适配器合同 + A5 探测框架 + 诚实 EffectBuilder
runtime/            设备表、路由、信任钳制
adapters/
  sim/              内建模拟灯
  modbus/           Modbus/TCP（读回验证）
  mqttbridge/       zigbee2mqtt/Tasmota 桥
  habridge/         Home Assistant WebSocket 桥
northbound/
  mcpserv/          MCP 北向（devices_list / device_invoke）
  c1serv/           C1 provider wire（HTTP over UDS）
matter/             纯 Go Matter 栈（在建）
  tlv/              TLV 编解码（规范附录 A）
  disc/             mDNS 可配对发现（_matterc._udp）
  spake2p/          SPAKE2+ over P-256
  pase/             PASE 握手（§4.13 五消息）
  frame/            Matter 消息帧（§4.4/§4.8）
cmd/anetlinkd/      守护进程 + build-tag 插件
```

**台架端口**（`test/realworld/up.sh`）：
```
18123  真 Home Assistant
18830  真 mosquitto broker
15020  真第三方 Modbus 服务端
5540   matter.js 虚拟灯（host 网络，passcode 20202021）
8997-9000  OpenThread 网络模拟器（OTNS）
```

---

*本手册随 ANetLink 代码演进。权威行为以代码与测试为准；设计取舍见 K207/K208/K209。*
*许可：ANet Open Source License。*

# ANetLink 使用手册

> 把物理世界变成 ANet 能力。一份从零到能控真实设备的完整指南。
>
> 适用版本：ANetLink 0.2.0（C1 v1，2026-10）。协议内核见 [ANetCore](https://github.com/ANetResearch/ANetCore)；
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

# 4. 作为服务给本机 anet daemon 用（C1，见 §7.2）
./anetlinkd
# c1 v1 on /run/user/1000/anetlink/c1.sock (other users allowed: 0; …)
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
  },

  "c1": {
    "socket": "/run/anetlink/c1.sock",
    "allow_users": ["anet"],
    "remote": ["light.onoff@sim/*"]
  }
}
```

**要点**：

- `backend_override` 是 A5 逃生口：强制某适配器用指定后端；**强制但探测失败 = 配置错误**
  （启动即报），不会静默降级。
- **配置了未编译进本二进制的适配器 = 响亮报错**。裁掉了 `mqtt` 却在 config 写 `mqtt` 段，
  启动直接失败并提示 `no_mqtt?`——绝不静默失能力。
- `access` 是三比特整数：STATE=1、SET=2、GET=4，按位或。可写可读的开关 = 6。
- `c1` 段管给 anet daemon 的 C1 socket：路径、允许连接的其他用户、远端调用方可用的能力。
  四个键（`socket`、`allow_users`、`group`、`remote`）与缺省值见 §7.2；**缺省全部关闭**，
  写错键名直接报错，不会静默生效成别的意思。

启动：`./anetlinkd --config config.json`（服务模式，给 daemon 走 C1）或
`./anetlinkd --config config.json --mcp`（给 Claude/IDE 走 MCP）。

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
**九砖齐备,从发现到入网的完整链路**：

| 砖 | 内容 | 验证方式 |
|---|---|---|
| tlv | Matter TLV 编解码 | 12 条规范向量 + 38 万次 fuzz |
| disc | mDNS `_matterc._udp` 发现 | **真机**（找到 matter.js 灯） |
| spake2p | SPAKE2+ over P-256 | **RFC 9383 + connectedhomeip 官方向量双钉** |
| pase | PASE 五消息握手 | **真机握手成功,双方导出同一会话密钥** |
| frame | 消息帧（§4.4/§4.8） | 字节级黄金布局 + fuzz |
| exchange | UDP + MRP 重传 | 真机 |
| session | 加密会话（AES-CCM,头作 AAD） | **NIST SP 800-38C 两组官方向量** |
| im | 交互模型（读/写/调用） | 编解码往返 |
| commission | 入网序列（失效保护→CSR→AddNOC→完成） | 顺序/中止/凭据/CSR 流向全测 |

Thread 的 CoAP 设备路径同样就位（手写 RFC 7252,CON 重传,写后读回验证）。
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

### 6.14 onvif（IP 摄像头，标准路径）

**默认不编入**，需 `-tags onvif`（理由见 §9.1）。

```jsonc
"onvif": {
  "verify_stream": true,            // 取流前先做 RTSP DESCRIBE 验活，强烈建议开
  "discovery_interface": "eth0",    // 可选，限定 WS-Discovery 的网卡
  "devices": [{
    "id": "cam-front",
    "xaddr": "http://192.168.1.64/onvif/device_service",  // --scan 会直接给出
    "user": "admin", "pass": "…",
    "profile_token": "MainStream",  // 可选，多目摄像机务必显式指定
    "events": true                  // 开 PullPoint 订阅（长轮询，默认关）
  }]
}
```

能力按**相机实际应答**投影,不按协议应许:没有 PTZ 服务就不会声明 `ptz.*`,
没开 events 就不会声明 `event.*`。

`anetlinkd --tags onvif --scan` 通过 WS-Discovery 直接找出全网摄像头,
连 IP 都不用你去路由器里翻。

### 6.15 hikvision（海康 ISAPI）/ 6.16 dahua（大华 CGI）

**默认不编入**,需 `-tags hikvision` / `-tags dahua`。

```jsonc
"hikvision": {
  "verify_stream": true,
  "devices": [{"id":"hik-1","address":"192.168.1.64","user":"admin","pass":"…",
               "channel":1,"stream":1,"events":true}]
},
"dahua": {
  "devices": [{"id":"dh-1","address":"192.168.1.108","user":"admin","pass":"…",
               "channel":1,"subtype":0,"events":true}]
}
```

**既然有 ONVIF,为什么还要厂商适配器?** ONVIF 是互操作的地板,不是设备的全部。
现场大量摄像机出厂就关了 ONVIF、或把 ONVIF 账号与 web 账号分开、或只给 Profile S。
而厂商接口永远在线,且带完整的智能事件词汇(越线、区域入侵、人脸)。
**两者都能用时优先 ONVIF**——它是标准;ONVIF 被关掉时,厂商适配器是"能用"和"砖头"的区别。

### 6.17 媒体能力族（stream / ptz / event）

摄像头是第一类**结果不是数字**的设备,所以能力族的形状与别处不同:

| 能力 | 访问 | 结果落在哪 |
|---|---|---|
| `stream.rtsp` | GET | `Artifact{Path: rtsp://…}` —— **只给引用,ANetLink 永不代理视频** |
| `stream.snapshot` | GET | `Artifact{ContentCID, SizeBytes}` —— 真字节,内容寻址 |
| `ptz.move` | SET | `{pan,tilt,zoom}` 均为 **[-1,1]**,可选 `duration_ms` |
| `ptz.stop` / `ptz.preset` | SET | 停止 / 调用预置位 |
| `ptz.position` | GET/STATE | 绝对位置,这是 PTZ 可验证的基础 |
| `event.motion` / `event.tamper` / `event.linecross` | STATE | 经 sink 上报,不可 invoke |

三条设计红线,都关乎诚实:

1. **`ptz.move` 的速度契约是统一的 [-1,1]**,厂商标度在适配器内部换算
   (海康 [-100,100]、大华离散方向码 + 1..8 速度)。调用方不需要知道这是谁家的相机。
2. **每次 move 都是有界的**,发完必停 —— ONVIF 的 continuous move 不停就一直转。
3. **位置回读是 PTZ 的验证手段**。相机到了机械限位时会照常 ack 却纹丝不动,
   只信 ack 就会报告一次没发生的转动 —— 这种情况我们报 UNVERIFIED 并说明"位置未变",
   不报成功。

**`verify_stream` 为什么重要**:`GetStreamUri` 只是返回一个字符串。相机会为一条
配置错误、编码器没起来、或被自身防火墙挡住的流照样发 URI。不探测就当成功,正是
"我发了命令" ≠ "我确认了效果"的混淆。开启后会真做一次 RTSP DESCRIBE,拿到 SDP
才算 V2 验证,并把真实编码(H264/H265)写进证据。

### 6.13 覆盖总表
- **Matter**（纯 Go，长线）：已实现 TLV 编解码、mDNS 可配对发现、SPAKE2+、PASE 握手、
  消息帧层；剩 UDP exchange 环（MRP）+ commissioning 全流程后可真机配对。
- **Zigbee 原生 / BLE / 蓝牙 Mesh / Thread**：路线见 K207 §6 覆盖矩阵。Zigbee 无纯软件
  网络模拟（需协调器硬件），当前由 mqtt 桥覆盖。

---

## 7. 北向接口：MCP 与 C1

**北向只有一个能力面。** 两种消费方式，同一批设备与效果。谁能调用什么，只在运行时
（`runtime.Invoke`）判定一次，两个入口对同一调用方给出同样的回答。

### 7.1 MCP（给 Claude / IDE）
`anetlinkd --mcp` 在 stdio 上起一个 MCP server，四个工具：

| 工具 | 作用 | 标注 |
|---|---|---|
| `devices_list` | 列出所有设备：key、协议、在线态、信任级、可调能力、Profile CID | 只读 |
| `device_invoke` | 调用一个能力，返回诚实效果（status/verifiable/metrics/message/evidence） | 有破坏性、开放世界 |
| `devices_discover` | 扫描已连接的网络，列出尚未配置的设备与建议配置 | 只读、开放世界 |
| `devices_pair` | 开放支持入网的网络一段时间，报告谁加入了 | 有破坏性、开放世界 |

设备 key 形如 `sim/lamp-1`（`<适配器>/<设备ID>`）。这是**冷启动钩子**：零 hub 零 daemon，
Claude 直接控设备。

> **注意**：`device_invoke` 与 `devices_pair` 会改动物理世界。不要让同一个 MCP 客户端在
> 无人确认的情况下同时接入 anet 与 anetlink：anet 会把其他 agent 发来的文本交给它，
> 对方的一段文字就可能让它去开门。MCP 客户端按上表的标注请你确认时，请保留这道确认。

### 7.2 C1 v1（给 anet daemon）

`anetlinkd` 不带 `--mcp` 时以服务模式运行，在 Unix socket 上起 C1 v1（HTTP over UDS，
请求头 `Host: anetlink`）。socket 缺省为 `$XDG_RUNTIME_DIR/anetlink/c1.sock`，与 anet 的
`modules.anetlink.socket` 缺省相同，两边都不配也能连上。`--mcp` 模式下只有显式给了
socket 才同时服务 C1，免得与服务模式的 anetlinkd 抢同一个 socket。

请求与响应体只用 CoreDet-CBOR（`Content-Type: application/cbor`），键号见 ANetCore
协议核心的 C1 v1 键位表：

| 方法与路径 | 成功 | 说明 |
|---|---|---|
| `GET /v1/health` | 204 | |
| `GET /v1/capabilities` | 200 `{1: [{1: id, 2: remote}, …]}` | `remote`：1 表示远端调用方可用，0 表示只限本机 |
| `GET /v1/describe` | 200 `{1: cid}` | 空串表示还没有设备目录对象 |
| `POST /v1/invoke` | 200 `{1: status, 2?: metrics, 3?: message, 4?: evidence}` | 请求 `{1: capability, 2?: args, 3?: call_id, 5: caller}`，`caller = {1: via, 2?: aid}`：`via` 为 0 表示本机，为 1 表示经 anet 中继的远端，此时必带 daemon 核实过的 `aid` |
| 任何 `/v0/*` | 426 | 旧版客户端得到 `c1_version` |
| 其他 | 404 | `not_found` |

拒绝一律回 `{1: reason}`，只带原因码；细节只写 anetlinkd 本地日志：

| reason | HTTP | 含义 |
|---|---|---|
| `not_permitted` | 403 | 远端调用方（`via = 1`）调用的能力不匹配 `c1.remote` |
| `bad_request` | 400 | 请求体不是合法的 C1 v1：未登记的键、退役的键（`InvokeReq` 键 4、`Caller` 键 3）、重复的键、CBOR 标签、非法 UTF-8、`via` 与 `aid` 不相配 |
| `too_large` | 413 | 请求体超过 1 MiB |
| `unsupported_media` | 415 | 不是 `application/cbor`（C1 v1 不再接受 JSON） |
| `unknown_capability` | 404 | 设备不存在、没有声明该能力，或该能力只能观察不能调用 |
| `c1_version` | 426 | 客户端说的是 C1 v0：把 anet 与 ANetLink 升级到同一发布 |
| `not_found` | 404 | 没有这个路由 |
| `internal` | 500 | 适配器出错，或响应无法编码 |

设备离线不是拒绝：回 200，`status` 为 `UNAVAILABLE`。

能力 id 形如 `light.onoff@sim/lamp-1`（`<能力>@<适配器>/<设备>`），**只在 ANetLink 侧铸造/
解析**——daemon 永远不知道"设备"这个概念，只知道"某能力可被调用"（这是 C1 红线）。
文件不经 C1：带文件的能力只能由 anet 的 service 模块提供。

**谁能连、谁能调**（缺省全部关闭）：

| 配置键 | 命令行 | 缺省 | 作用 |
|---|---|---|---|
| `c1.socket` | `--c1-socket` | `$XDG_RUNTIME_DIR/anetlink/c1.sock` | socket 路径 |
| `c1.allow_users` | `--c1-allow-user`（可重复） | 空：只允许 anetlinkd 自身用户 | 允许连接的其他用户（用户名或数字 uid）；root 不隐含放行 |
| `c1.group` | `--c1-group` | 不设 | 把 socket 开给这些用户的组；只有一个其他用户时缺省取其主组，多于一个时必填 |
| `c1.remote` | — | 空：全部能力拒绝远端调用 | 远端调用方可用的能力，`path.Match` 模式，对完整 id 匹配 |

命令行优先于配置文件。`c1.remote` 里的 `*` 不跨 `/`：`light.onoff@sim/*` 放行 sim 上每台设备
的开关，`*@sim/lamp-1` 放行这盏灯的全部能力，`*/*` 放行全部（慎用）。按 AID 的授权只在
anet daemon 的名单里做；ANetLink 只有这一个远端开关。anet 公开了某个能力而 `c1.remote`
没有放行它时，`anet doctor` 的 `anetlink.publish` 会提醒，远端调用方得到 `provider_policy`。

anetlinkd 启动时：
- socket 所在目录不存在则以 0700 创建；已存在则属主必须是自身或 root，且组与其他用户
  都不可写，否则拒绝启动（谁能写这个目录，谁就能换掉 socket）；已存在的目录不改权限；
- socket 路径不得是符号链接；已有进程在监听时拒绝启动，无人监听的陈旧 socket 自动删除；
- socket 为 0600；`allow_users` 里有其他用户时为 0660 并归属 `c1.group`，anetlinkd 自己
  创建的目录同时改为 0710，让该组可以进入；
- 每个连接在读任何字节之前核对对端 uid（Linux 用 `SO_PEERCRED`，macOS 与 FreeBSD 用
  `LOCAL_PEERCRED`），不在 {自身} ∪ `allow_users` 中即断开，日志每个 uid 每分钟至多一条：
  `拒绝 uid {n} 连接 c1.sock(不在 allow_users 中);如需允许,在 ANetLink 配置 c1.allow_users 加入该用户。`
- 其他平台读不到对端 uid，C1 不服务，anetlinkd 拒绝启动。

每次调用（C1 与 MCP 都算）在本地日志记一行 `invoke via=… aid=… cap=… result=…`，不记参数。

**daemon 侧配置**（ANet 仓 config.json）：两边都用缺省路径、以同一用户运行时什么都不用配。
anetlinkd 以另一用户运行（例如 root，为了直接访问硬件）时：

```bash
anetlinkd --config config.json --c1-socket /run/anetlink/c1.sock --c1-allow-user anet
```
```json
{ "modules": { "anetlink": { "socket": "/run/anetlink/c1.sock", "expected_user": "root" } } }
```

`expected_user` 让 daemon 在连接时核对监听者确实是 anetlinkd 的用户。之后 daemon 收到能力型
委派会经 provider 注册表确定性执行，效果即交付物 + 签名回执，并写入本地 AEL 证据链。

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

适配器全部是**编译期插件**，但有**两套 tag 约定**，用哪一套本身就说明了适配器的性质。

### 9.1 两套约定

**减法（`no_<name>`）—— 协议适配器,默认编入。**
它们对本地网络讲开放标准、不依赖发行版之外的任何东西,所以默认在;发行版做减法。

| tag | 剪掉 |
|---|---|
| `no_sim` | 模拟器 |
| `no_modbus` | Modbus 适配器 |
| `no_mqtt` | MQTT/z2m 桥 |
| `no_ha` | Home Assistant 桥 |
| `no_zigbee` / `no_bacnet` / `no_can` / `no_ble` / `no_btmesh` / `no_thread` | 对应适配器 |

**加法（`<name>`）—— 摄像头与厂商适配器,默认不编入,必须显式要。**

| tag | 加入 |
|---|---|
| `onvif` | ONVIF IP 摄像头 |
| `hikvision` | 海康 ISAPI |
| `dahua` | 大华 CGI |

**为什么反过来**:这些适配器经厂商 HTTP 接口触达设备、要持有凭据。
默认构建里不带它们,意味着一个没人启用过的攻击面根本不存在于二进制中。
这是你在 anet4 里定的原则的自然延伸 —— 裁剪是构建配置,不是 git 操作。

```bash
# 默认发行版（无摄像头、无厂商接口）
CGO_ENABLED=0 go build ./cmd/anetlinkd

# 安防网关：全部摄像头能力
CGO_ENABLED=0 go build -tags onvif,hikvision,dahua ./cmd/anetlinkd

# 边缘盒子：只留 Modbus + BLE（示例）
CGO_ENABLED=0 go build -tags no_mqtt,no_ha,no_sim ./cmd/anetlinkd
```

配置里写了没编入的适配器,启动会响亮报错,并指出**正确的修法**——
opt-in 的说"用 `-tags onvif` 重新构建",opt-out 的说"是不是带了 `no_x`"。

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
| `c1: … is writable by other users` | C1 socket 所在目录可被他人写，谁都能换掉 socket。`chmod go-w <目录>` 或换一个目录 |
| `c1: … is in use by another process` | 已有进程（多半是另一个 anetlinkd）在这个 socket 上。停掉它，或用 `--c1-socket` 换路径 |
| `c1: the socket path … is N bytes, longer than the 107 this platform allows` | Unix socket 路径有长度上限（Linux 107 字节，macOS 与 BSD 103 字节）。换一个短路径，例如 `/run/anetlink/c1.sock` |
| `c1: XDG_RUNTIME_DIR is not set` | 服务模式找不到缺省 socket 路径。加 `--c1-socket /run/anetlink/c1.sock`，daemon 侧 `modules.anetlink.socket` 设同一路径 |
| `c1: the users in c1.allow_users cannot enter …` | 已存在的 socket 目录不让允许的用户进入。删掉该目录让 anetlinkd 重建，或按提示 `chgrp` 与 `chmod 0710` |
| 日志 `c1: the users in c1.allow_users may not reach the socket, because …` | socket 更上层的某个目录不让允许的用户通过（缺省的 `$XDG_RUNTIME_DIR` 按定义是 0700）。改用他们能到达的路径，例如 `--c1-socket /run/anetlink/c1.sock`，daemon 侧 `modules.anetlink.socket` 设同一路径 |
| 日志 `c1_uid_refused` | 有用户连 c1.sock 但不在 `c1.allow_users` 中。确实需要时把该用户加入 `c1.allow_users` |
| daemon 报 `c1_version` | anet 与 ANetLink 的 C1 版本不同。把两者升级到同一发布（ANetLink 0.2.0 起为 C1 v1） |
| 远端调用方得到 `provider_policy` | 该能力不匹配 `c1.remote`。在 `c1.remote` 放行它，或在 anet 里不公开它 |
| `adap: allow_uid was removed` | 0.2.0 起 ADAP socket 只接受 anetlinkd 自身用户。以同一用户运行适配器进程并删掉 `allow_uid` |

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
  --config <path>          JSON 配置文件
  --mcp                    在 stdio 上服务 MCP 北向
  --c1-socket <path>       C1 socket 路径（缺省 $XDG_RUNTIME_DIR/anetlink/c1.sock；服务模式自动启用）
  --c1-allow-user <name>   允许该用户（用户名或 uid）连接 C1 socket，可重复
  --c1-group <group>       把 C1 socket 开给允许用户的组
  --demo                   跑一次 demo invoke 后退出
  --scan [--scan-out F]    扫描已连接的网络，打印或写出建议配置
  --pair <dur>             开放入网一段时间，报告谁加入了
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
  c1serv/           C1 v1（CoreDet-CBOR，HTTP over UDS）
internal/peercred/  Unix socket 对端核实与 socket 目录规则
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

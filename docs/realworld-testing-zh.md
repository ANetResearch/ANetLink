# 真实测试三层制（ink 指令：IoT 应用测试一定要真实）

| 层 | 对象 | 何时跑 | 现状 |
|---|---|---|---|
| **L0** 进程内假件 | FSM/协议逻辑（mbserver、mochi、fake-HA） | 每次提交（CI test job） | ✅ |
| **L1** 真实服务 | **真 mosquitto · 真第三方 Modbus 服务端（Python oitc）· 真 Home Assistant core**（demo 集成 = 真实可控实体、真鉴权、真事件总线） | 每次提交（CI realworld job）+ 本地 `test/realworld/up.sh` | ✅ |
| **L2** 真实硬件 | 真 Zigbee 棒 + 真设备、真 PLC/电表、家里生产 HA | 环境变量指向即跑（同一批测试），操作员在场 | 待设备清单 |

L2 用法：同一批 `-tags realworld` 测试，把 `ANETLINK_*` 指向真实目标即可——
`ANETLINK_MQTT_LIVE_Z2M=1` 时 mqtt 测试只做只读盘点（真实设备的执行动作
须操作员监督）。诚实边界：L1 的 mqtt"设备侧"仍是合成响应器（无射频硬件），
broker 与 wire 全真；HA/Modbus 从鉴权到状态机全真。


---

## 摄像头这一路（L0/L1/L2 边界）

```bash
./test/realworld/up.sh          # 起 mediamtx + onvif-server（连同既有 rig）
source test/realworld/env
./test/realworld/run.sh
```

`up.sh` 会把 `camera/onvif.yaml` 里的 mac 改写成本机默认路由网卡的 mac ——
onvif-server 靠 mac 反查自己要广告的 IP，写错就起不来。

| 层 | 覆盖 | 说明 |
|---|---|---|
| L0 | 全部路径，含 PTZ / 事件 / 摘要认证 | 忠实假件：真 SOAP 与 ISAPI/CGI 线格式、真 multipart 事件流、真 UDP/TCP socket |
| L1 | 发现、能力投影、RTSP 探测 | `ANETLINK_RTSP_URI` / `ANETLINK_ONVIF_XADDR` |
| **L2** | **PTZ、事件、JPEG 抓拍** | onvif-server 只实现 Profile S，这三样必须真机 |
| **L2** | **海康 ISAPI、大华 CGI 全部** | 厂商私有协议，**不存在模拟器** |

L2 的环境变量：

```bash
export ANETLINK_HIKVISION_ADDR=192.168.1.64
export ANETLINK_HIKVISION_USER=admin
export ANETLINK_HIKVISION_PASS=…
export ANETLINK_DAHUA_ADDR=192.168.1.108
export ANETLINK_DAHUA_USER=admin
export ANETLINK_DAHUA_PASS=…
```

> **注意**：`TestRealPTZ` 会真的转动云台（跑完会尽量转回去）。
> 不要对着此刻有人在看的画面跑。

### 找摄像头不用翻路由器

```bash
CGO_ENABLED=0 go build -tags onvif ./cmd/anetlinkd
./anetlinkd --config onvif-empty.json --scan
```

WS-Discovery 会把全网摄像头连同**可直接粘贴的配置**一起打印出来。

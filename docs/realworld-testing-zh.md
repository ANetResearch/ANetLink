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

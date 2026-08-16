# 试验场（IoT 模拟设备台架）

ink 指令（2026-08-16）：无实体设备期间，用专门的模拟项目建 docker 试验场。
常驻于 lab-node:/data/testbed（`docker compose up -d`）；成员与端口见 compose 注释。

分层归位：这些是 **L1.5 —— 真协议栈的软件形态**（真 HA core、真 mosquitto、
真第三方 Modbus 实现、真 OpenThread 仿真网络、真 matter.js 可配对设备），
高于进程内假件（L0/L1 夹具），低于带射频的实体硬件（L2）。

| 成员 | 用途 | 现状 |
|---|---|---|
| ha / mosq / modbus | realworld 测试即用（`test/realworld/`） | ✅ CI 同款 |
| matter（matter.js 虚拟灯） | 未来纯 Go Matter 栈的**跨实现互操作对手**（先配对它，再谈过黄金向量） | ✅ 已可配对，等 Go 栈 |
| otns | 未来 Thread 适配器的仿真网络 | ✅ 已起，接线待 Thread 里程碑 |
| ESPHome host 设备 | esphome native-API 适配器（覆盖矩阵候补行）目标 | 待入列 |
| Zigbee | **无纯软件方案**（协调器硬件必需）；建议购 CC2652/EFR32 棒转 L2 | 由 mqtt 层模拟代偿 |

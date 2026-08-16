# ANetLink 架构（v0 骨架）

四层：`profile`（设备模型 + T0–T4 信任阶梯）→ `sdk`（C4 适配器合同 + A5 探测
框架 + 诚实 EffectBuilder）→ `runtime`（设备表、路由、信任钳制）→ 北向
（MCP / C1 shim，待落）。

三条已被测试钉住的纪律：

1. **探测优先、兜底强制**（A5）：`sdk.Select` 按优先序探测后端，宿主设施活着
   就用，全灭则报每个后端的失败详情；强制指定但探测失败 = 配置错误。
2. **无度量不得称 OK**：`EffectBuilder.Verified()` 无 Metric 时 Build 降为
   UNVERIFIED——"命令发出去了"和"效果被验证了"在类型层就分开。
3. **信任只能钳、不能抬**：runtime.Invoke 对 MaxTrust < T2 的设备把 OK 强制
   降为 UNVERIFIED（link-minimum：云 API 封顶 T1；quirk 无权抬信任）。

待落（按 K209 队列）：MCP 北向 → C1-UDS shim → modbus/mqtt/ha 三个真实
适配器 → AEL 证据写入 → 后端探测进配置面。

# coldcheck — 乳品冷库温度核查

把**库区格位、门开事件、空气节点、产品批/产品段、人工抽测芯温**关联到同一条证据链上的核查系统。靠近蒸发器或门口的格位用**本格位/相邻节点**和**温区分桶均值**比较，不允许被全库均值掩盖。

> 系统边界：只做证据采集、规则核查与提示；**不控制任何制冷设备，不给出产品是否可食用的建议**。

## 技术栈

| 关注点 | 实现 |
|---|---|
| 格位/门磁/暴露区间规则 | Go（`rules/`，纯函数、可单测） |
| 库位版本、产品段、人工测量 | PostgreSQL（`sql/schema.sql` + `store/postgres.go`） |

温区与格位记录均带 `layout_id` 绑定到库位版本：Upsert 时未指定则绑定当前活动版本；`Load` 只返回活动版本（及历史遗留的未绑定行）的几何，切换版本后旧格位/温区不会混入当前平面。
| 空气节点广播 | LoRaWAN 网关 webhook → `POST /lorawan/uplink`（`lora/`） |
| 库内分层平面 | MapLibre GL JS（`/map`，GeoJSON `/api/geojson`，按层切换） |
| 手持浏览器录入 | HTMX（`/entry`，表单提交返回 HTML 片段） |

## 目录

```
domain/      领域模型（温区/格位/门/节点/批/占用/抽测/冻结计划/告警）
rules/       门磁去抖、暴露区间、空间缺口、离线、遮挡、本地差异、移位核对、芯温与冻结核查
store/       Store 接口；内存实现 + PostgreSQL 实现
lora/        LoRaWAN 上行解析（base64 二进制或 decoder object）
web/         HTTP、HTMX 页面/片段、GeoJSON
seed/        确定性演示场景（含五类必测情形）
cmd/server/  服务入口
sql/         PostgreSQL schema
```

## 运行

```bash
# 内存演示（自动种入五类情形），默认 :8080
go run ./cmd/server -demo -addr :8080

# PostgreSQL 系统记录
psql "$DSN" -f sql/schema.sql
go run ./cmd/server -dsn "postgres://user:pass@host/db?sslmode=disable" -window 4h
```

打开：

- `http://localhost:8080/` 核查台（告警 15s HTMX 轮询）
- `http://localhost:8080/map` 库内分层平面
- `http://localhost:8080/entry` 手持录入
- `http://localhost:8080/api/alerts` 告警 JSON
- `http://localhost:8080/api/geojson` 格位/温区/节点 GeoJSON

## 规则如何落到代码

| 业务情形 | 规则 / 代码 | 告警码 |
|---|---|---|
| 门磁反跳 | `rules/doors.go`：开/关/开抖动短于 `MinClosedPulse` 合并为一次开门；孤立短于 `MinOpenPulse` 的脉冲丢弃 | `DOOR_STILL_OPEN` |
| 暴露区间 | `rules/exposure.go`：占用 ∩（空气出带 / 开门 / 移位 / 温区不允许产品段），相邻同因区间合并，按批累计 | `AIR_OUT_OF_BAND` `ZONE_MISMATCH` `MOVE_EXPOSURE` `EXPOSURE_LIMIT` |
| 门口/蒸发器差异被均值掩盖 | 空气按格位节点聚合成 5 分钟桶；格位与**同温区同层同时刻均值**比较，并带 `NearDoor/NearEvap` 标记 | `LOCAL_DELTA`（`info`） |
| 空间缺口 | 占用或冻结格位无直接节点；非冻结格位允许同层相邻（`NeighbourRadiusM`）节点补位；冻结格位必须直接覆盖 | `SPATIAL_GAP`（`fail`） |
| 节点离线 | 最近上行早于 `Node.Deadline`（默认 20m） | `NODE_OFFLINE` |
| 节点被货箱遮住 | RSSI 较自由空气基线下降 ≥12 dBm **且** 90 分钟温度波动 ≤0.2°C（双条件，避免误报稳定冷点） | `NODE_OCCLUDED` |
| 托盘移位未扫 | 占用迁移/arrive 事件与扫描枪记录对账（±2 分钟）；无扫描或扫描无实际位移均提示 | `MOVE_UNSCANNED` `SCAN_WITHOUT_MOVE` |
| 探针未达中心 | `ProbeDepthMM < RequiredDepthMM - 容差` 或未勾选中心 → 读数不可作为芯温结论 | `PROBE_NOT_CENTER` |
| 芯温越限 | 对照**产品段**温度带 | `CORE_OUT_OF_BAND` / `CORE_FROZEN_OUT_OF_BAND` |
| 一批跨两个温区 | 物理占用跨多个温区即提示；各占用段按所在温区独立判定，冷冻段温暖不代表冷藏段合格 | `BATCH_CROSS_ZONE` |
| 质量冻结 | 质量人员冻结随机格位 × 随机时点（`rules.RandomFreeze`，落库后锁定）；每个格位×时点必须有时间容差内的芯温 | `FROZEN_MEASUREMENT_MISSING` |
| 除霜影响标注 | `rules/defrost.go`：**只读**接收除霜开始/结束状态（设备时刻配对），划出"活动除霜 + 回温尾"窗口；窗口内空气温升、开门、芯温与常规时段分开标注；跨边界读数单列。不据此判定品质变化 | `DEFROST_AIR_RISE` `DEFROST_DOOR_OPEN` `CORE_IN_DEFROST_WINDOW` `CORE_CROSSES_DEFROST_WINDOW` `DEFROST_STATE_LATE` `DEFROST_ONGOING` `NODE_MAINTENANCE` |

### LoRaWAN 负载

`POST /lorawan/uplink` 接受常见 NS webhook 字段 `devEUI/fPort/frmpayload/time/rssi`，二进制负载：

```
byte 0    设备版本
byte 1-2  int16 BE，温度 × 0.01°C
byte 3-4  uint16 电池 mV（可选）
```

也接受 decoder 预解析的 `object.temperature|tempC|temp_c|c`。二进制与 decoder 两条路径都做同一物理合理性校验（-60~80°C，拒绝 NaN/Inf）；decoder 回传的异常值（如错误寄存器、0x7FFF 哨兵值）不会被当成正常上行入库。平台只收上行，不发下行、不控制设备。

### HTMX 录入

- `POST /scan`：批号 / 源格位 / 目的格位（写扫描 + 标记为已扫描的新占用）
- `POST /door`：门编号 / 开或关 / RSSI（**原始事件**，再由引擎去抖）
- `POST /core`：批号、格位、芯温、实际插入深度、到中心所需深度、是否达中心
- `POST /freeze`：随机格位数、随机时点个数、覆盖小时数
- `POST /defrost`：蒸发器 / `state=start|end` / 设备时间（**只读接收除霜状态**；留空时间=现在，补录历史即迟到状态）
- `POST /maintenance`：节点 / 维护起止 / 原因（开放结束=维护中，期间读数剔除、离线与遮挡判断挂起）

## 除霜影响标注（只读，不判定品质）

除霜是冷库**预期内的致暖事件**，但平台只接收制冷系统上报的状态、只做证据分区，**不开始/停止/调度任何除霜，也不输出"品质变化/可食用"结论**。

- **窗口如何画出**：`defrost_events(device_at, starting)` 按蒸发器配对成 `[Start, End)` 活动除霜区间，再追加 `DefrostRecovery`（默认 15m）回温尾，得到温升窗口 `[Start, End+recovery)`。窗口**只来自上报状态**：空气自己变暖但没有除霜状态，不会生成窗口。
- **空气分开**：落入温升窗口的越限空气桶单独出 `DEFROST_AIR_RISE`（info），常规时段才出 `AIR_OUT_OF_BAND`（warn）；同一格位两类可以并存。暴露区间打 `DefrostEvap` 标签但**仍计入累计暴露**（温度事实不被除霜抹掉）。
- **芯温分开**：每条芯温按所在格位的窗口分为 `in`（窗口内）/ `cross`（距窗口边界 ≤ `CoreDefrostMargin`，默认 2m，归属存疑）/ `routine`。窗口内/跨边界各自出 info 标注；**产品段温度带核查照常用原始值执行**——窗口内越限依然是越限证据，窗口内合格也绝不自动生成品质结论。
- **五个必覆盖情形**（`rules/defrost_test.go`）：
  1. **除霜状态迟到**：结束状态晚到，配对仍按**设备时刻**回放历史窗口，仅打 `DEFROST_STATE_LATE`；迟到事件不被当成"正在除霜"，也不把迟到标记泄漏到下一周期；
  2. **多个蒸发器分区不同**：EV-1（CHILL，显式服务格位）与 EV-2（FREEZE 全区）独立配对；格位只命中本区窗口，FREEZE 窗口不会标到 CHILL 读数；
  3. **门在除霜时开启**：出 `DEFROST_DOOR_OPEN`（info），开门与除霜作为**并存原因**，温升不单方面归因；去抖开门证据照常保留；
  4. **芯温测量跨窗口**：窗口内（CM-4）单列、边界 2m 内（CM-5）标 `cross`、窗口外（CM-1/CM-3）保持常规；三类都不改变温度带判定；
  5. **节点恰好进入维护**：维护 `[From,To)`（半开）内读数不进入空气序列、不算离线/遮挡；边界恰在 `To` 的读数有效；开放维护只在覆盖评估时刻时挂起离线，维护结束后静默重新计为离线；除霜窗口来自状态，缺读数不会让窗口消失或扩张。

## 测试

五类要求情形均在 `rules/rules_test.go` 中固定：

1. `TestDoorContactBounce` 门磁反跳合并为一次 20 分钟开门，2 秒孤立脉冲丢弃；
2. `TestNodeCoveredByBox` 货箱遮住节点（RSSI 坍塌 + 温度停滞）；
3. `TestPalletMovedWithoutScan` 托盘移位未扫 + 该格位空间缺口 + 节点离线；
4. `TestProbeDepthAndBatchAcrossTwoZones` 探针未达中心、冰淇淋芯温越限、一批跨 FREEZE/CHILL；
5. `TestFrozenPlanAndLocalDelta` 冻结时点缺失、已测不报警，蒸发器格位本地偏冷不被均值掩盖。

除霜影响标注在 `rules/defrost_test.go`：迟到状态按设备时刻回放、跨分区蒸发器互不串窗、除霜中开门、芯温窗口内/跨边界/常规三分类、节点恰好进入维护（含多周期迟到不泄漏、维护半开边界）。

```bash
go test ./...
```

## 关键阈值（`rules/params.go`，均可按现场调整）

去抖 5s/15s、离线 20m、空气桶 5m、本地差异 1.5°C、RSSI 遮挡下降 12 dBm、
停滞窗口 90m/0.2°C、相邻节点半径 1.6m、默认累计暴露限值 30m、探针容差 5mm、
除霜回温尾 15m、除霜状态迟到阈值 10m、芯温跨边界容差 2m。

## 明确不做

- 不向制冷机组/阀门/网关下发任何控制指令；除霜状态只**只读接收**，平台不能开始或停止除霜；
- 不输出”可食用/报废”结论：只给出温度证据与合规告警；除霜窗口只做空气/芯温证据分区，窗口内或跨窗口测量都不自动判定品质变化；
- 告警为派生结果，不在库里充当事实；事实表为门原始事件、节点读数、占用/扫描、人工测量与只读除霜状态。

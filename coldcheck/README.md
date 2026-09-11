# coldcheck — 乳品冷库温度核查

把**库区格位、门开事件、空气节点、产品批/产品段、人工抽测芯温**关联到同一条证据链上的核查系统。靠近蒸发器或门口的格位用**本格位/相邻节点**和**温区分桶均值**比较，不允许被全库均值掩盖。

> 系统边界：只做证据采集、规则核查与提示；**不控制任何制冷设备，不给出产品是否可食用的建议**。

## 技术栈

| 关注点 | 实现 |
|---|---|
| 格位/门磁/暴露区间规则 | Go（`rules/`，纯函数、可单测） |
| 库位版本、产品段、人工测量 | PostgreSQL（`sql/schema.sql` + `store/postgres.go`） |
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

### LoRaWAN 负载

`POST /lorawan/uplink` 接受常见 NS webhook 字段 `devEUI/fPort/frmpayload/time/rssi`，二进制负载：

```
byte 0    设备版本
byte 1-2  int16 BE，温度 × 0.01°C
byte 3-4  uint16 电池 mV（可选）
```

也接受 decoder 预解析的 `object.temperature|tempC|temp_c|c`。平台只收上行，不发下行、不控制设备。

### HTMX 录入

- `POST /scan`：批号 / 源格位 / 目的格位（写扫描 + 标记为已扫描的新占用）
- `POST /door`：门编号 / 开或关 / RSSI（**原始事件**，再由引擎去抖）
- `POST /core`：批号、格位、芯温、实际插入深度、到中心所需深度、是否达中心
- `POST /freeze`：随机格位数、随机时点个数、覆盖小时数

## 测试

五类要求情形均在 `rules/rules_test.go` 中固定：

1. `TestDoorContactBounce` 门磁反跳合并为一次 20 分钟开门，2 秒孤立脉冲丢弃；
2. `TestNodeCoveredByBox` 货箱遮住节点（RSSI 坍塌 + 温度停滞）；
3. `TestPalletMovedWithoutScan` 托盘移位未扫 + 该格位空间缺口 + 节点离线；
4. `TestProbeDepthAndBatchAcrossTwoZones` 探针未达中心、冰淇淋芯温越限、一批跨 FREEZE/CHILL；
5. `TestFrozenPlanAndLocalDelta` 冻结时点缺失、已测不报警，蒸发器格位本地偏冷不被均值掩盖。

```bash
go test ./...
```

## 关键阈值（`rules/params.go`，均可按现场调整）

去抖 5s/15s、离线 20m、空气桶 5m、本地差异 1.5°C、RSSI 遮挡下降 12 dBm、
停滞窗口 90m/0.2°C、相邻节点半径 1.6m、默认累计暴露限值 30m、探针容差 5mm。

## 明确不做

- 不向制冷机组/阀门/网关下发任何控制指令；
- 不输出“可食用/报废”结论，只给出温度证据与合规告警；
- 告警为派生结果，不在库里充当事实；事实表为门原始事件、节点读数、占用/扫描与人工测量。

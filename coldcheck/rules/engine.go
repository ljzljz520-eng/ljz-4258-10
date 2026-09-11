package rules

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"coldcheck/domain"
)

type Result struct {
	At            time.Time
	Alerts        []domain.Alert
	DoorEvents    []domain.DoorEvent
	Exposure      []ExposureInterval
	TotalExposure map[string]time.Duration
	Deviations    []LocalDeviation
	Obstructions  []Obstruction
	Offline       []string
	Gaps          []string
	CoreIssues    []CoreIssue
	Missing       []MissingFrozen
	CrossZones    []CrossZone
	MoveIssues    []MoveIssue
}

func (r *Result) add(a domain.Alert) {
	a.At = r.At
	r.Alerts = append(r.Alerts, a)
}

// Evaluate runs the complete verification rule set. It never writes to
// refrigeration controls and never infers edibility; outputs are evidence
// and compliance alerts only.
func Evaluate(snap *domain.Snapshot, p Params) *Result {
	r := &Result{At: snap.At}
	if snap.At.IsZero() {
		r.At = time.Now()
		snap.At = r.At
	}

	doors := DebounceDoorEvents(snap.RawDoorEvents, p, snap.At)
	if len(snap.DoorEvents) > 0 {
		doors = append(doors, snap.DoorEvents...)
		sort.Slice(doors, func(i, j int) bool { return doors[i].OpenedAt.Before(doors[j].OpenedAt) })
	}
	r.DoorEvents = doors
	for _, d := range doors {
		if d.StillOpen {
			cell := ""
			if dr, ok := snap.Doors[d.DoorID]; ok {
				cell = dr.CellCode
			}
			r.add(domain.Alert{
				Code: "DOOR_STILL_OPEN", Severity: "warn",
				Msg:    fmt.Sprintf("门 %s 仍处于开启状态（自 %s）", d.DoorID, d.OpenedAt.Format("15:04:05")),
				DoorID: d.DoorID, CellCode: cell,
			})
		}
	}

	series := BuildAirSeries(snap, p)

	// Air band violations per occupied/frozen cell, with physical context.
	cells := make([]string, 0, len(series))
	for c := range series {
		cells = append(cells, c)
	}
	sort.Strings(cells)
	for _, c := range cells {
		s := series[c]
		var worst *CellPoint
		var wd float64
		for i := range s.Points {
			pt := s.Points[i]
			d := 0.0
			if pt.C < s.MinC {
				d = s.MinC - pt.C
			} else if pt.C > s.MaxC {
				d = pt.C - s.MaxC
			}
			if d > wd {
				wd = d
				worst = &s.Points[i]
			}
		}
		if worst != nil {
			cell := snap.Cells[c]
			where := where(cell)
			side := "高于"
			if worst.C < s.MinC {
				side = "低于"
			}
			r.add(domain.Alert{
				Code: "AIR_OUT_OF_BAND", Severity: "warn", CellCode: c, ZoneCode: s.ZoneCode,
				Msg: fmt.Sprintf("格位 %s%s 空气温度 %.2f°C %s温区限值 [%.1f,%.1f]°C",
					c, where, worst.C, side, s.MinC, s.MaxC),
			})
		}
	}

	r.Deviations = LocalDeviations(series, p)
	for _, d := range r.Deviations {
		r.add(domain.Alert{
			Code: "LOCAL_DELTA", Severity: "info", CellCode: d.CellCode, ZoneCode: d.ZoneCode,
			Msg: d.String(),
		})
	}

	r.Exposure = BatchExposure(snap, series, doors, p)
	r.TotalExposure = TotalExposure(r.Exposure)
	for _, e := range r.Exposure {
		switch e.Reason {
		case "zone-mismatch":
			r.add(domain.Alert{
				Code: "ZONE_MISMATCH", Severity: "fail", CellCode: e.CellCode,
				ZoneCode: e.ZoneCode, BatchID: e.BatchID,
				Msg: fmt.Sprintf("批 %s 存放于不允许其产品段的温区 %s", e.BatchID, e.ZoneCode),
			})
		case "move":
			r.add(domain.Alert{
				Code: "MOVE_EXPOSURE", Severity: "info", CellCode: e.CellCode,
				ZoneCode: e.ZoneCode, BatchID: e.BatchID,
				Msg: fmt.Sprintf("批 %s 在格位 %s 的移位缺少扫描佐证", e.BatchID, e.CellCode),
			})
		}
	}
	for batchID, total := range r.TotalExposure {
		limit := p.DefaultExposureLimit
		if b, ok := snap.Batches[batchID]; ok && b.MaxExposure > 0 {
			limit = b.MaxExposure
		}
		if total > limit {
			r.add(domain.Alert{
				Code: "EXPOSURE_LIMIT", Severity: "fail", BatchID: batchID,
				Msg: fmt.Sprintf("批 %s 累计暴露 %s 超过限值 %s", batchID, total.Round(time.Second), limit),
			})
		}
	}

	r.Gaps = SpatialGaps(snap, p)
	for _, c := range r.Gaps {
		r.add(domain.Alert{Code: "SPATIAL_GAP", Severity: "fail", CellCode: c,
			Msg: fmt.Sprintf("格位 %s 缺少可用空气节点，存在监测空间缺口", c)})
	}

	r.Offline = OfflineNodes(snap, p)
	for _, id := range r.Offline {
		cell := ""
		if n, ok := snap.Nodes[id]; ok {
			cell = n.CellCode
		}
		r.add(domain.Alert{Code: "NODE_OFFLINE", Severity: "warn", NodeID: id, CellCode: cell,
			Msg: fmt.Sprintf("空气节点 %s 离线超过时限（格位 %s）", id, cell)})
	}

	r.Obstructions = OccludedNodes(snap, p)
	for _, o := range r.Obstructions {
		r.add(domain.Alert{Code: "NODE_OCCLUDED", Severity: "warn", NodeID: o.NodeID, CellCode: o.CellCode,
			Msg: fmt.Sprintf("节点 %s 可能被货箱遮挡：RSSI %d 较基线 %d 低 %d dBm，且 %.2f 小时温度几乎不变（波动 %.2f°C）",
				o.NodeID, o.RSSI, o.Baseline, o.Baseline-o.RSSI,
				p.StagnationWindow.Hours(), o.RangeC)})
	}

	r.MoveIssues = UnscannedMoves(snap, 2*time.Minute)
	for _, m := range r.MoveIssues {
		switch m.Reason {
		case "occupancy-without-scan", "arrive-without-scan":
			r.add(domain.Alert{Code: "MOVE_UNSCANNED", Severity: "warn", BatchID: m.BatchID,
				CellCode: m.ToCell, Msg: fmt.Sprintf("批 %s 从 %s 移位至 %s 但未扫描", m.BatchID, m.FromCell, m.ToCell)})
		case "scan-without-move":
			r.add(domain.Alert{Code: "SCAN_WITHOUT_MOVE", Severity: "info", BatchID: m.BatchID,
				CellCode: m.ToCell, Msg: fmt.Sprintf("批 %s 有 %s→%s 扫描记录但无实际位移证据", m.BatchID, m.FromCell, m.ToCell)})
		}
	}

	r.CoreIssues = CoreChecks(snap, p)
	for _, c := range r.CoreIssues {
		sev := "warn"
		if c.Code == "CORE_OUT_OF_BAND" || c.Code == "CORE_FROZEN_OUT_OF_BAND" {
			sev = "fail"
		}
		r.add(domain.Alert{Code: c.Code, Severity: sev, BatchID: c.BatchID, CellCode: c.CellCode,
			PlanID: c.PlanID, At: c.At, Msg: fmt.Sprintf("批 %s 抽测芯温 %.2f°C：%s", c.BatchID, c.C, c.Msg)})
	}

	r.Missing = MissingFrozenMeasurements(snap, p.Bucket)
	for _, m := range r.Missing {
		r.add(domain.Alert{Code: "FROZEN_MEASUREMENT_MISSING", Severity: "fail", PlanID: m.PlanID,
			CellCode: m.CellCode, At: m.At,
			Msg: fmt.Sprintf("冻结抽测缺失：计划 %s 格位 %s 时点 %s 无芯温记录", m.PlanID, m.CellCode, m.At.Format("01-02 15:04"))})
	}

	r.CrossZones = BatchZoneSpread(snap)
	for _, x := range r.CrossZones {
		r.add(domain.Alert{Code: "BATCH_CROSS_ZONE", Severity: "warn", BatchID: x.BatchID,
			Msg: fmt.Sprintf("批 %s 同时分布于两个温区：%s；各段按所在温区独立判定", x.BatchID, strings.Join(x.Zones, "、"))})
	}

	sort.SliceStable(r.Alerts, func(i, j int) bool {
		if rank(r.Alerts[i].Severity) != rank(r.Alerts[j].Severity) {
			return rank(r.Alerts[i].Severity) < rank(r.Alerts[j].Severity)
		}
		return r.Alerts[i].Code < r.Alerts[j].Code
	})
	return r
}

func rank(s string) int {
	switch s {
	case "fail":
		return 0
	case "warn":
		return 1
	default:
		return 2
	}
}

func where(c *domain.Cell) string {
	parts := []string{}
	if c != nil && c.NearDoor {
		parts = append(parts, "靠近门口")
	}
	if c != nil && c.NearEvap {
		parts = append(parts, "靠近蒸发器")
	}
	if len(parts) == 0 {
		return ""
	}
	return "（" + strings.Join(parts, "，") + "）"
}

// MaxDeviation is a convenience accessor for dashboards.
func (r *Result) MaxDeviation() float64 {
	m := 0.0
	for _, d := range r.Deviations {
		if math.Abs(d.DeltaC) > m {
			m = math.Abs(d.DeltaC)
		}
	}
	return m
}

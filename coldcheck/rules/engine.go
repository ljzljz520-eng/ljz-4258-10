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
	At             time.Time
	Alerts         []domain.Alert
	DoorEvents     []domain.DoorEvent
	Exposure       []ExposureInterval
	TotalExposure  map[string]time.Duration
	Deviations     []LocalDeviation
	Obstructions   []Obstruction
	Offline        []string
	Gaps           []string
	CoreIssues     []CoreIssue
	CoreDefrost    []CoreDefrost
	Missing        []MissingFrozen
	CrossZones     []CrossZone
	MoveIssues     []MoveIssue
	DefrostWindows []DefrostWindow
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

	// Defrost state is READ ONLY: windows are drawn from reported coil state
	// (plus a recovery tail) and only annotate warming. They never suppress a
	// band fact and never conclude a quality change.
	defrostWindows := BuildDefrostWindows(snap, p)
	r.DefrostWindows = defrostWindows
	for _, w := range defrostWindows {
		scope := w.ZoneCode
		if len(w.Cells) > 0 {
			scope = strings.Join(w.Cells, "、")
		}
		if w.Ongoing {
			r.add(domain.Alert{
				Code: "DEFROST_ONGOING", Severity: "info", EvapID: w.EvapID, ZoneCode: w.ZoneCode,
				Msg: fmt.Sprintf("蒸发器 %s（%s）自 %s 起处于除霜状态、尚未收到结束状态；除霜为只读标注",
					w.EvapID, scope, w.Start.Format("15:04")),
			})
		}
		if w.Late {
			r.add(domain.Alert{
				Code: "DEFROST_STATE_LATE", Severity: "warn", EvapID: w.EvapID, ZoneCode: w.ZoneCode,
				Msg: fmt.Sprintf("蒸发器 %s 的除霜状态迟到（设备时刻早于入库时刻超过 %s）；窗口按设备时刻回放，不能当作实时状态",
					w.EvapID, p.DefrostStateLate),
			})
		}
	}

	// Door opened while a covering evaporator is in its rise window: two
	// independent warming causes coincide and must not be attributed to one
	// another. The door event itself (and its exposure interval) still stands.
	for _, d := range doors {
		dr := snap.Doors[d.DoorID]
		if dr == nil {
			continue
		}
		closed := d.ClosedAt
		if closed.IsZero() {
			closed = snap.At
		}
		for _, w := range WindowsForCell(snap, defrostWindows, dr.CellCode) {
			if s, e, ok := overlap(Interval{d.OpenedAt, closed}, w.RiseInterval()); ok && e.After(s) {
				r.add(domain.Alert{
					Code: "DEFROST_DOOR_OPEN", Severity: "info", DoorID: d.DoorID, CellCode: dr.CellCode,
					EvapID: w.EvapID, ZoneCode: w.ZoneCode,
					Msg: fmt.Sprintf("门 %s 在蒸发器 %s 除霜温升窗口内开启（%s–%s）；开门与除霜为并存原因，温升不得单方面归因",
						d.DoorID, w.EvapID, s.Format("15:04"), e.Format("15:04")),
				})
			}
		}
	}

	series := BuildAirSeries(snap, p, defrostWindows)

	// Air band violations per occupied/frozen cell. Buckets inside a defrost
	// rise window are emitted as DEFROST_AIR_RISE (info) — same temperature
	// fact, kept apart from routine AIR_OUT_OF_BAND (warn). A bucket that
	// straddles the active-heating edge is additionally marked cross so it is
	// never quoted as purely routine or purely defrost evidence.
	cells := make([]string, 0, len(series))
	for c := range series {
		cells = append(cells, c)
	}
	sort.Strings(cells)
	for _, c := range cells {
		s := series[c]
		var worstRoutine, worstDefrost *CellPoint
		var rd, dd float64
		for i := range s.Points {
			pt := &s.Points[i]
			d := 0.0
			if pt.C < s.MinC {
				d = s.MinC - pt.C
			} else if pt.C > s.MaxC {
				d = pt.C - s.MaxC
			}
			if d == 0 {
				continue
			}
			if len(pt.DefrostEvap) > 0 {
				if d > dd {
					dd, worstDefrost = d, pt
				}
			} else if d > rd {
				rd, worstRoutine = d, pt
			}
		}
		if worstRoutine != nil {
			cell := snap.Cells[c]
			r.add(domain.Alert{
				Code: "AIR_OUT_OF_BAND", Severity: "warn", CellCode: c, ZoneCode: s.ZoneCode,
				Msg: fmt.Sprintf("格位 %s%s 空气温度 %.2f°C %s温区限值 [%.1f,%.1f]°C（常规时段）",
					c, where(cell), worstRoutine.C, sideOf(worstRoutine, s.MinC), s.MinC, s.MaxC),
			})
		}
		if worstDefrost != nil {
			cell := snap.Cells[c]
			cross := ""
			if worstDefrost.DefrostCross {
				cross = "；该桶跨越除霜活动期边界，同时包含常规与除霜空气，不能归入任一结论"
			}
			r.add(domain.Alert{
				Code: "DEFROST_AIR_RISE", Severity: "info", CellCode: c, ZoneCode: s.ZoneCode,
				EvapID: strings.Join(worstDefrost.DefrostEvap, "、"),
				Msg: fmt.Sprintf("格位 %s%s 在蒸发器 %s 除霜温升窗口内空气 %.2f°C，%s温区限值 [%.1f,%.1f]°C；属除霜影响标注，不判定品质变化%s",
					c, where(cell), strings.Join(worstDefrost.DefrostEvap, "、"),
					worstDefrost.C, sideOf(worstDefrost, s.MinC), s.MinC, s.MaxC, cross),
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

	r.Exposure = BatchExposure(snap, series, doors, defrostWindows, p)
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

	// Nodes on an open maintenance interval are expected silent.
	maintenanceSet := map[string]bool{}
	for _, mn := range snap.Maintenance {
		if !mn.To.IsZero() && !snap.At.Before(mn.To) {
			continue // closed interval in the past
		}
		if !snap.At.Before(mn.From) && !maintenanceSet[mn.NodeID] {
			maintenanceSet[mn.NodeID] = true
			cell := ""
			if n, ok := snap.Nodes[mn.NodeID]; ok {
				cell = n.CellCode
			}
			reason := mn.Reason
			if reason == "" {
				reason = "维护/校准"
			}
			r.add(domain.Alert{Code: "NODE_MAINTENANCE", Severity: "info", NodeID: mn.NodeID, CellCode: cell,
				Msg: fmt.Sprintf("空气节点 %s（格位 %s）处于%s时段，其读数与离线判断已挂起，恢复后重新计入", mn.NodeID, cell, reason)})
		}
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
	issueDefrost := map[string][]string{} // measurement id -> evaporator ids
	for _, c := range r.CoreIssues {
		sev := "warn"
		if c.Code == "CORE_OUT_OF_BAND" || c.Code == "CORE_FROZEN_OUT_OF_BAND" {
			sev = "fail"
		}
		msg := fmt.Sprintf("批 %s 抽测芯温 %.2f°C：%s", c.BatchID, c.C, c.Msg)
		class, ws := CoreDefrostClass(snap, defrostWindows, c.CellCode, c.At, p.CoreDefrostMargin)
		if class != "routine" {
			evaps := make([]string, 0, len(ws))
			for _, w := range ws {
				evaps = append(evaps, w.EvapID)
			}
			issueDefrost[c.MeasurementID] = evaps
			switch class {
			case "in":
				msg += fmt.Sprintf("；该读数处于除霜温升窗口（蒸发器 %s），与常规时段分开记录，不因温升自动判定品质变化", strings.Join(evaps, "、"))
			case "cross":
				msg += fmt.Sprintf("；测量时刻跨越除霜窗口边界（蒸发器 %s），不能归入常规或除霜任一结论", strings.Join(evaps, "、"))
			}
		}
		r.add(domain.Alert{Code: c.Code, Severity: sev, BatchID: c.BatchID, CellCode: c.CellCode,
			PlanID: c.PlanID, EvapID: strings.Join(issueDefrost[c.MeasurementID], "、"), At: c.At, Msg: msg})
	}

	// Every core measurement is also classified against defrost rise windows
	// so a defrost-window reading is separable even when it stays in band —
	// classification never creates or removes a band verdict.
	r.CoreDefrost = AnnotateCoreMeasurements(snap, defrostWindows, p)
	for _, cd := range r.CoreDefrost {
		evaps := make([]string, 0, len(cd.Windows))
		for _, w := range cd.Windows {
			evaps = append(evaps, w.EvapID)
		}
		switch cd.Class {
		case "in":
			r.add(domain.Alert{
				Code: "CORE_IN_DEFROST_WINDOW", Severity: "info", BatchID: cd.BatchID,
				CellCode: cd.CellCode, EvapID: strings.Join(evaps, "、"), At: cd.At,
				Msg: fmt.Sprintf("批 %s 芯温 %.2f°C 采于蒸发器 %s 除霜温升窗口内，单列保存，不与常规时段混判（不自动判定品质变化）",
					cd.BatchID, cd.C, strings.Join(evaps, "、")),
			})
		case "cross":
			r.add(domain.Alert{
				Code: "CORE_CROSSES_DEFROST_WINDOW", Severity: "info", BatchID: cd.BatchID,
				CellCode: cd.CellCode, EvapID: strings.Join(evaps, "、"), At: cd.At,
				Msg: fmt.Sprintf("批 %s 芯温 %.2f°C 的测量时刻跨越蒸发器 %s 除霜窗口边界（±%s 内），读数单列为边界存疑，不作自动结论",
					cd.BatchID, cd.C, strings.Join(evaps, "、"), p.CoreDefrostMargin),
			})
		}
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

func sideOf(pt *CellPoint, minC float64) string {
	if pt.C < minC {
		return "低于"
	}
	return "高于"
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

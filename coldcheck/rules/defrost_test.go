package rules_test

import (
	"testing"
	"time"

	"coldcheck/domain"
	"coldcheck/rules"
	"coldcheck/seed"
)

func alertByCodeBatch(res *rules.Result, code, batch string) *domain.Alert {
	for i := range res.Alerts {
		a := &res.Alerts[i]
		if a.Code == code && a.BatchID == batch {
			return a
		}
	}
	return nil
}

func alertByCodeNode(res *rules.Result, code, node string) *domain.Alert {
	for i := range res.Alerts {
		a := &res.Alerts[i]
		if a.Code == code && a.NodeID == node {
			return a
		}
	}
	return nil
}

// 除霜状态迟到：结束状态晚到，窗口仍按设备时刻回放并标记迟到，且晚到事件
// 不能被当作"正在除霜"。
func TestDefrostStateLate(t *testing.T) {
	_, res := evalSeed(t)
	a := hasAlert(res, "DEFROST_STATE_LATE")
	if a == nil || a.EvapID != "EV-1" {
		t.Fatalf("expected DEFROST_STATE_LATE for EV-1, got %+v", a)
	}
	// EV-1 window closed in the past: no ongoing alert for it.
	for _, al := range res.Alerts {
		if al.Code == "DEFROST_ONGOING" && al.EvapID == "EV-1" {
			t.Fatalf("late end event must close the historical window, got ongoing: %+v", al)
		}
	}
	var w *rules.DefrostWindow
	for i := range res.DefrostWindows {
		if res.DefrostWindows[i].EvapID == "EV-1" {
			w = &res.DefrostWindows[i]
		}
	}
	if w == nil {
		t.Fatal("EV-1 window must be reconstructed despite late arrival")
	}
	if w.Ongoing {
		t.Fatal("EV-1 defrost must be closed by the late end state")
	}
	sc := seed.Build(time.Date(2026, 9, 11, 10, 0, 0, 0, time.UTC))
	wantStart := sc.Now.Add(-110 * time.Minute)
	wantEnd := sc.Now.Add(-100 * time.Minute)
	if !w.Start.Equal(wantStart) || !w.End.Equal(wantEnd) {
		t.Fatalf("window = [%v,%v], want device-time [%v,%v]", w.Start, w.End, wantStart, wantEnd)
	}
	if !w.RiseUntil.Equal(wantEnd.Add(rules.DefaultParams().DefrostRecovery)) {
		t.Fatalf("rise until = %v, want %v", w.RiseUntil, wantEnd.Add(rules.DefaultParams().DefrostRecovery))
	}
	// EV-2 reported on time: no late flag.
	for i := range res.DefrostWindows {
		if res.DefrostWindows[i].EvapID == "EV-2" && res.DefrostWindows[i].Late {
			t.Fatal("EV-2 timely reports must not be flagged late")
		}
	}
}

// 多个蒸发器分区不同：CHILL 与 FREEZE 的除霜窗口互不串区，芯温/空气只按
// 本分区窗口标注。
func TestDefrostZonesIndependent(t *testing.T) {
	sc, res := evalSeed(t)
	winByEvap := map[string]rules.DefrostWindow{}
	for _, w := range res.DefrostWindows {
		winByEvap[w.EvapID] = w
	}
	w1, w2 := winByEvap["EV-1"], winByEvap["EV-2"]
	if w1.ZoneCode != "CHILL" || w2.ZoneCode != "FREEZE" {
		t.Fatalf("zones = %s / %s, want CHILL / FREEZE", w1.ZoneCode, w2.ZoneCode)
	}
	// A-03-03 is explicitly served by EV-1; A-03-02 is routine CHILL air.
	snap := sc.Snapshot
	if got := rules.WindowsForCell(snap, res.DefrostWindows, "A-03-03"); len(got) != 1 || got[0].EvapID != "EV-1" {
		t.Fatalf("A-03-03 windows = %+v, want only EV-1", got)
	}
	if got := rules.WindowsForCell(snap, res.DefrostWindows, "A-03-02"); len(got) != 0 {
		t.Fatalf("A-03-02 is not served by EV-1, windows = %+v", got)
	}
	if got := rules.WindowsForCell(snap, res.DefrostWindows, "B-01-01"); len(got) != 1 || got[0].EvapID != "EV-2" {
		t.Fatalf("B-01-01 windows = %+v, want only EV-2", got)
	}
	// FREEZE air during EV-2 defrost stays in band, so no rise alert there.
	for _, al := range res.Alerts {
		if al.Code == "DEFROST_AIR_RISE" && al.ZoneCode == "FREEZE" {
			t.Fatalf("FREEZE air stayed in band during EV-2: %+v", al)
		}
	}
	// Routine OOB in CHILL (door warm at -60m) stays a routine warn alert.
	a := hasAlert(res, "AIR_OUT_OF_BAND")
	if a == nil || a.CellCode != "A-01-02" {
		t.Fatalf("routine doorway OOB must remain AIR_OUT_OF_BAND, got %+v", a)
	}
	// Defrost-window OOB at A-03-03 is separated into an info rise annotation.
	ra := hasAlert(res, "DEFROST_AIR_RISE")
	if ra == nil || ra.CellCode != "A-03-03" || ra.EvapID != "EV-1" {
		t.Fatalf("expected DEFROST_AIR_RISE A-03-03/EV-1, got %+v", ra)
	}
	for _, al := range res.Alerts {
		if al.Code == "AIR_OUT_OF_BAND" && al.CellCode == "A-03-03" {
			t.Fatal("defrost-window warming must not also raise routine AIR_OUT_OF_BAND")
		}
	}
}

// 门在除霜时开启：两种致暖原因并存标注，开门证据不被吞掉。
func TestDoorOpenDuringDefrost(t *testing.T) {
	_, res := evalSeed(t)
	var got *domain.Alert
	for i := range res.Alerts {
		if res.Alerts[i].Code == "DEFROST_DOOR_OPEN" && res.Alerts[i].DoorID == "D3" {
			got = &res.Alerts[i]
		}
	}
	if got == nil {
		t.Fatal("expected DEFROST_DOOR_OPEN for D3 during EV-1 defrost")
	}
	if got.EvapID != "EV-1" || got.CellCode != "A-03-03" {
		t.Fatalf("DEFROST_DOOR_OPEN attribution = %+v", got)
	}
	// The bounced D1 opening (-60m) is outside every defrost window: no tag.
	for _, al := range res.Alerts {
		if al.Code == "DEFROST_DOOR_OPEN" && al.DoorID == "D1" {
			t.Fatal("D1 open at -60m is not inside a defrost window")
		}
	}
	// Door debounce evidence itself remains.
	found := false
	for _, d := range res.DoorEvents {
		if d.DoorID == "D3" {
			found = true
		}
	}
	if !found {
		t.Fatal("D3 debounced door event must still exist")
	}
}

// 芯温测量跨窗口：窗口内读数单列、边界读数标 cross、窗口外保持常规；
// 任何一类都不自动改变品质判定（越限告警仍然按产品带给出）。
func TestCoreMeasurementDefrostSeparation(t *testing.T) {
	_, res := evalSeed(t)
	class := map[string]string{}
	for _, cd := range res.CoreDefrost {
		class[cd.MeasurementID] = cd.Class
	}
	if class["CM-4"] != "in" {
		t.Fatalf("CM-4 (-105m, heating) class = %q, want in", class["CM-4"])
	}
	if class["CM-5"] != "cross" {
		t.Fatalf("CM-5 (2m before rise end) class = %q, want cross", class["CM-5"])
	}
	if class["CM-1"] != "routine" {
		t.Fatalf("CM-1 (-30m) class = %q, want routine", class["CM-1"])
	}
	if class["CM-3"] != "routine" {
		t.Fatalf("CM-3 in FREEZE (-10m) class = %q, want routine (EV-2 window ended at -25m rise end)", class["CM-3"])
	}
	if a := alertByCodeBatch(res, "CORE_IN_DEFROST_WINDOW", "LOT-5"); a == nil || a.CellCode != "A-03-03" {
		t.Fatal("expected CORE_IN_DEFROST_WINDOW for LOT-5/A-03-03 (CM-4)")
	}
	if a := alertByCodeBatch(res, "CORE_CROSSES_DEFROST_WINDOW", "LOT-5"); a == nil || a.CellCode != "A-03-03" {
		t.Fatal("expected CORE_CROSSES_DEFROST_WINDOW for LOT-5/A-03-03 (CM-5)")
	}
	// CM-3 out of band keeps its fail verdict regardless of defrost context.
	var oob *domain.Alert
	for i := range res.Alerts {
		a := &res.Alerts[i]
		if a.BatchID == "LOT-3" && a.CellCode == "B-01-01" &&
			(a.Code == "CORE_OUT_OF_BAND" || a.Code == "CORE_FROZEN_OUT_OF_BAND") {
			oob = a
		}
	}
	if oob == nil {
		t.Fatal("defrost classification must not remove the CM-3 band verdict")
	}
	if oob.EvapID != "" {
		t.Fatalf("routine (-10m) CM-3 must not be annotated with an evaporator: %+v", oob)
	}
	// In-band defrost-window CM-4 must not invent an out-of-band verdict.
	for _, al := range res.Alerts {
		if (al.Code == "CORE_OUT_OF_BAND" || al.Code == "CORE_FROZEN_OUT_OF_BAND") &&
			al.BatchID == "LOT-5" {
			t.Fatalf("in-band defrost-window core must not create a band verdict: %+v", al)
		}
	}
}

// 节点恰好进入维护：维护期读数被剔除、节点不算离线/遮挡；维护结束后读数
// 重新计入，且除霜窗口不因缺读数而消失或扩张。
func TestNodeEntersMaintenance(t *testing.T) {
	sc, res := evalSeed(t)
	// n-b12 is under open maintenance from -50m: no offline/occlusion alert.
	if a := alertByCodeNode(res, "NODE_OFFLINE", "n-b12"); a != nil {
		t.Fatalf("maintained node must not be offline: %+v", a)
	}
	if a := alertByCodeNode(res, "NODE_OCCLUDED", "n-b12"); a != nil {
		t.Fatalf("maintained node must not be flagged occluded: %+v", a)
	}
	if a := alertByCodeNode(res, "NODE_MAINTENANCE", "n-b12"); a == nil {
		t.Fatal("expected NODE_MAINTENANCE info for n-b12")
	}
	// n-b12 seeded readings during maintenance (-50m..now) are dropped; the
	// node had valid uplinks before that, but since maintenance is open it is
	// exempt rather than offline.
	snap := sc.Snapshot
	if !snap.InMaintenance("n-b12", sc.Now.Add(-30*time.Minute)) {
		t.Fatal("InMaintenance must cover -30m")
	}
	if snap.InMaintenance("n-b12", sc.Now.Add(-55*time.Minute)) {
		t.Fatal("InMaintenance must be false before the interval start")
	}
	// n-dead still offline (no maintenance) — the exemption is targeted.
	if a := alertByCodeNode(res, "NODE_OFFLINE", "n-dead"); a == nil {
		t.Fatal("n-dead remains offline")
	}
	// EV-2 window exists from reported state even though n-b12 went silent.
	var ev2 *rules.DefrostWindow
	for i := range res.DefrostWindows {
		if res.DefrostWindows[i].EvapID == "EV-2" {
			ev2 = &res.DefrostWindows[i]
		}
	}
	if ev2 == nil || ev2.Ongoing {
		t.Fatalf("EV-2 window must be built from state, not inferred from missing readings: %+v", ev2)
	}
}

// Synthetic: an end state arriving after evaluation time must still pair the
// history correctly, and an unmatched END yields no phantom window.
func TestDefrostPairingEdgeCases(t *testing.T) {
	now := time.Date(2026, 9, 11, 10, 0, 0, 0, time.UTC)
	base := func() *domain.Snapshot {
		s := &domain.Snapshot{
			At:          now,
			Zones:       map[string]*domain.Zone{"Z": {Code: "Z", MinC: 0, MaxC: 4}},
			Cells:       map[string]*domain.Cell{"C1": {Code: "C1", ZoneCode: "Z", Active: true}},
			Doors:       map[string]*domain.Door{},
			Evaporators: map[string]*domain.Evaporator{"E": {ID: "E", ZoneCode: "Z"}},
			Nodes:       map[string]*domain.Node{},
			Batches:     map[string]*domain.Batch{},
		}
		return s
	}
	p := rules.DefaultParams()

	// Start without end -> ongoing.
	s := base()
	s.Defrosts = []domain.DefrostEvent{{EvapID: "E", At: now.Add(-3 * time.Minute), Starting: true, IngestedAt: now.Add(-3 * time.Minute)}}
	ws := rules.BuildDefrostWindows(s, p)
	if len(ws) != 1 || !ws[0].Ongoing || !ws[0].RiseUntil.Equal(now) {
		t.Fatalf("ongoing window wrong: %+v", ws)
	}

	// Duplicate starts / duplicate ends tolerated.
	s = base()
	s.Defrosts = []domain.DefrostEvent{
		{EvapID: "E", At: now.Add(-30 * time.Minute), Starting: true},
		{EvapID: "E", At: now.Add(-29 * time.Minute), Starting: true},
		{EvapID: "E", At: now.Add(-20 * time.Minute), Starting: false},
		{EvapID: "E", At: now.Add(-19 * time.Minute), Starting: false},
	}
	if ws := rules.BuildDefrostWindows(s, p); len(ws) != 1 || ws[0].Start != s.Defrosts[0].At ||
		!ws[0].End.Equal(now.Add(-20*time.Minute)) || ws[0].Ongoing {
		t.Fatalf("duplicate-state pairing wrong: %+v", ws)
	}

	// End without start -> no window.
	s = base()
	s.Defrosts = []domain.DefrostEvent{{EvapID: "E", At: now.Add(-5 * time.Minute), Starting: false}}
	if ws := rules.BuildDefrostWindows(s, p); len(ws) != 0 {
		t.Fatalf("unmatched end must not create a window: %+v", ws)
	}

	// Maintenance half-open boundary: reading exactly at To is valid.
	s = base()
	s.Maintenance = []domain.NodeMaintenance{{NodeID: "N", From: now.Add(-10 * time.Minute), To: now.Add(-5 * time.Minute)}}
	if !s.InMaintenance("N", now.Add(-5*time.Minute).Add(-time.Second)) {
		t.Fatal("just before To must be in maintenance")
	}
	if s.InMaintenance("N", now.Add(-5*time.Minute)) {
		t.Fatal("at To the interval is already closed (half-open)")
	}
}

// Bucket 级标注：升温窗口内的空气桶打 EV 标记，跨越活动期边界的桶额外标
// DefrostCross；维护期读数不进入空气序列。
func TestAirSeriesDefrostAndMaintenanceAnnotations(t *testing.T) {
	now := time.Date(2026, 9, 11, 10, 0, 0, 0, time.UTC)
	p := rules.DefaultParams()
	bucket := p.Bucket // 5m
	snap := &domain.Snapshot{
		At:          now,
		Zones:       map[string]*domain.Zone{"Z": {Code: "Z", MinC: 0, MaxC: 4}},
		Cells:       map[string]*domain.Cell{"C1": {Code: "C1", ZoneCode: "Z", Active: true}},
		Doors:       map[string]*domain.Door{},
		Evaporators: map[string]*domain.Evaporator{"E": {ID: "E", ZoneCode: "Z", Cells: []string{"C1"}}},
		Nodes:       map[string]*domain.Node{"N": {ID: "N", CellCode: "C1", Active: true, Deadline: time.Hour}},
		Batches:     map[string]*domain.Batch{},
	}
	// Active heating [10:10,10:20); recovery tail until 10:35.
	snap.Defrosts = []domain.DefrostEvent{
		{EvapID: "E", At: now.Add(10 * time.Minute), Starting: true, IngestedAt: now.Add(10 * time.Minute)},
		{EvapID: "E", At: now.Add(20 * time.Minute), Starting: false, IngestedAt: now.Add(20 * time.Minute)},
	}
	ws := rules.BuildDefrostWindows(snap, p)
	if len(ws) != 1 || !ws[0].RiseUntil.Equal(now.Add(35*time.Minute)) {
		t.Fatalf("window = %+v", ws)
	}
	// Readings at :07 routine, :12 inside heating, :22 recovery-only,
	// :37 routine again; plus a maintenance-window reading at :30.
	snap.Air = []domain.AirReading{
		{NodeID: "N", At: now.Add(7 * time.Minute), C: 2.0, RSSI: -68},
		{NodeID: "N", At: now.Add(12 * time.Minute), C: 6.0, RSSI: -68},
		{NodeID: "N", At: now.Add(22 * time.Minute), C: 5.0, RSSI: -68},
		{NodeID: "N", At: now.Add(30 * time.Minute), C: 99.0, RSSI: -68},
		{NodeID: "N", At: now.Add(37 * time.Minute), C: 2.2, RSSI: -68},
	}
	snap.Maintenance = []domain.NodeMaintenance{
		{NodeID: "N", From: now.Add(28 * time.Minute), To: now.Add(32 * time.Minute)},
	}
	series := rules.BuildAirSeries(snap, p, ws)
	s := series["C1"]
	if s == nil {
		t.Fatal("expected series for C1")
	}
	byMinute := map[time.Time]rules.CellPoint{}
	for _, pt := range s.Points {
		byMinute[pt.At.Truncate(bucket)] = pt
	}
	routine := byMinute[now.Add(5*time.Minute).Truncate(bucket)]
	if len(routine.DefrostEvap) != 0 || routine.C != 2.0 {
		t.Fatalf(":07 bucket must be routine: %+v", routine)
	}
	heating := byMinute[now.Add(10*time.Minute).Truncate(bucket)]
	if len(heating.DefrostEvap) != 1 || heating.DefrostEvap[0] != "E" || !heating.DefrostCross {
		t.Fatalf(":12 bucket must be tagged E and cross (contains heating edge): %+v", heating)
	}
	recovery := byMinute[now.Add(20*time.Minute).Truncate(bucket)]
	if len(recovery.DefrostEvap) != 1 || recovery.DefrostEvap[0] != "E" || recovery.DefrostCross {
		t.Fatalf(":22 recovery-only bucket is tagged but not cross: %+v", recovery)
	}
	after := byMinute[now.Add(35*time.Minute).Truncate(bucket)]
	if len(after.DefrostEvap) != 0 || after.C != 2.2 {
		t.Fatalf(":37 bucket must be routine again: %+v", after)
	}
	if _, ok := byMinute[now.Add(30*time.Minute).Truncate(bucket)]; ok {
		t.Fatal("reading at :30 falls in maintenance and must not create a bucket")
	}
}

// 节点恰好进入维护（开放区间）：静默不算离线；维护结束后旧节点恢复离线判定。
func TestMaintenanceSuppressesOfflineOnlyWhileOpen(t *testing.T) {
	now := time.Date(2026, 9, 11, 10, 0, 0, 0, time.UTC)
	p := rules.DefaultParams()
	mk := func(mn []domain.NodeMaintenance) *domain.Snapshot {
		return &domain.Snapshot{
			At:          now,
			Zones:       map[string]*domain.Zone{"Z": {Code: "Z"}},
			Cells:       map[string]*domain.Cell{"C1": {Code: "C1", ZoneCode: "Z", Active: true}},
			Doors:       map[string]*domain.Door{},
			Evaporators: map[string]*domain.Evaporator{},
			Nodes:       map[string]*domain.Node{"N": {ID: "N", CellCode: "C1", Active: true, Deadline: 20 * time.Minute}},
			Batches:     map[string]*domain.Batch{},
			Air: []domain.AirReading{
				// last valid uplink 60m ago; in-window reading is under a
				// past closed maintenance and must not count either.
				{NodeID: "N", At: now.Add(-60 * time.Minute), C: 2.0, RSSI: -68},
				{NodeID: "N", At: now.Add(-5 * time.Minute), C: 9.0, RSSI: -68},
			},
			Maintenance: mn,
		}
	}
	// Open maintenance covering the fresh reading and the evaluation instant:
	// the node is expected silent on the bench, not offline.
	s := mk([]domain.NodeMaintenance{{NodeID: "N", From: now.Add(-10 * time.Minute)}})
	for _, id := range rules.OfflineNodes(s, p) {
		if id == "N" {
			t.Fatal("open maintenance must suppress NODE_OFFLINE")
		}
	}
	// Closed maintenance that ended before evaluation: the :05 reading sits
	// inside the interval and is dropped, so the node IS offline (60m stale).
	s = mk([]domain.NodeMaintenance{{NodeID: "N", From: now.Add(-10 * time.Minute), To: now.Add(-2 * time.Minute)}})
	found := false
	for _, id := range rules.OfflineNodes(s, p) {
		if id == "N" {
			found = true
		}
	}
	if !found {
		t.Fatal("after maintenance closes, silence counts as offline again")
	}
	// Maintenance starting exactly at evaluation time covers the instant.
	s = mk([]domain.NodeMaintenance{{NodeID: "N", From: now}})
	for _, id := range rules.OfflineNodes(s, p) {
		if id == "N" {
			t.Fatal("maintenance starting exactly at evaluation time suppresses offline")
		}
	}
}

// 迟到的结束状态（含重试重复 END）不能把迟到标记泄漏到下一个除霜周期。
func TestLateEndDoesNotLeakIntoNextCycle(t *testing.T) {
	now := time.Date(2026, 9, 11, 14, 0, 0, 0, time.UTC)
	s := &domain.Snapshot{
		At:          now,
		Zones:       map[string]*domain.Zone{"Z": {Code: "Z"}},
		Cells:       map[string]*domain.Cell{},
		Doors:       map[string]*domain.Door{},
		Evaporators: map[string]*domain.Evaporator{"E": {ID: "E", ZoneCode: "Z"}},
		Nodes:       map[string]*domain.Node{},
		Batches:     map[string]*domain.Batch{},
		Defrosts: []domain.DefrostEvent{
			{EvapID: "E", At: now.Add(-90 * time.Minute), Starting: true, IngestedAt: now.Add(-90 * time.Minute)},
			{EvapID: "E", At: now.Add(-80 * time.Minute), Starting: false, IngestedAt: now.Add(-2 * time.Minute)},
			{EvapID: "E", At: now.Add(-80 * time.Minute), Starting: false, IngestedAt: now.Add(-1 * time.Minute)},
			{EvapID: "E", At: now.Add(-30 * time.Minute), Starting: true, IngestedAt: now.Add(-30 * time.Minute)},
			{EvapID: "E", At: now.Add(-20 * time.Minute), Starting: false, IngestedAt: now.Add(-20 * time.Minute)},
		},
	}
	ws := rules.BuildDefrostWindows(s, rules.DefaultParams())
	if len(ws) != 2 {
		t.Fatalf("want 2 cycles, got %d: %+v", len(ws), ws)
	}
	if !ws[0].Late {
		t.Fatal("first cycle with a 78m-late end must be flagged late")
	}
	if ws[1].Late {
		t.Fatal("timely second cycle must not inherit the previous cycle's late flag")
	}
	if !ws[0].End.Equal(now.Add(-80 * time.Minute)) {
		t.Fatalf("first cycle end = %v, want -80m (device time pairing)", ws[0].End)
	}
}

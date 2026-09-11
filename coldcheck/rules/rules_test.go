package rules_test

import (
	"testing"
	"time"

	"coldcheck/domain"
	"coldcheck/rules"
	"coldcheck/seed"
)

func evalSeed(t *testing.T) (*seed.Scenario, *rules.Result) {
	t.Helper()
	now := time.Date(2026, 9, 11, 10, 0, 0, 0, time.UTC)
	sc := seed.Build(now)
	res := rules.Evaluate(sc.Snapshot, rules.DefaultParams())
	return sc, res
}

func hasAlert(res *rules.Result, code string) *domain.Alert {
	for i := range res.Alerts {
		if res.Alerts[i].Code == code {
			return &res.Alerts[i]
		}
	}
	return nil
}

func alertCount(res *rules.Result, code string) int {
	n := 0
	for _, a := range res.Alerts {
		if a.Code == code {
			n++
		}
	}
	return n
}

// 1. 门磁反跳必须被去抖成一次开门。
func TestDoorContactBounce(t *testing.T) {
	sc, res := evalSeed(t)
	var d1 []domain.DoorEvent
	for _, d := range res.DoorEvents {
		if d.DoorID == "D1" {
			d1 = append(d1, d)
		}
	}
	if len(d1) != 1 {
		t.Fatalf("D1 expected exactly one debounced open interval, got %d: %+v", len(d1), d1)
	}
	wantOpen := sc.Now.Add(-60 * time.Minute)
	wantClose := sc.Now.Add(-40 * time.Minute)
	if !d1[0].OpenedAt.Equal(wantOpen) || !d1[0].ClosedAt.Equal(wantClose) {
		t.Fatalf("merged interval = [%v,%v], want [%v,%v]", d1[0].OpenedAt, d1[0].ClosedAt, wantOpen, wantClose)
	}
	if d1[0].StillOpen {
		t.Fatal("D1 should be closed")
	}
	// micro-pulse alone must be dropped
	snap := *sc.Snapshot
	snap.RawDoorEvents = []domain.RawDoorEvent{
		{DoorID: "Dx", At: sc.Now.Add(-time.Minute), IsOpen: true},
		{DoorID: "Dx", At: sc.Now.Add(-time.Minute + 2*time.Second), IsOpen: false},
	}
	r2 := rules.Evaluate(&snap, rules.DefaultParams())
	for _, d := range r2.DoorEvents {
		if d.DoorID == "Dx" {
			t.Fatalf("2s pulse must be debounced away, got %+v", d)
		}
	}
	if a := hasAlert(res, "DOOR_STILL_OPEN"); a == nil || a.DoorID != "D2" {
		t.Fatalf("expected D2 still-open alert, got %+v", a)
	}
}

// 2. 节点被货箱遮住：RSSI 下降且温度停滞 -> NODE_OCCLUDED。
func TestNodeCoveredByBox(t *testing.T) {
	_, res := evalSeed(t)
	a := hasAlert(res, "NODE_OCCLUDED")
	if a == nil {
		t.Fatal("expected NODE_OCCLUDED alert for n-a13")
	}
	if a.NodeID != "n-a13" {
		t.Fatalf("occluded node = %s, want n-a13", a.NodeID)
	}
}

// 3. 托盘移位未扫 -> MOVE_UNSCANNED + 空间缺口 SPATIAL_GAP。
func TestPalletMovedWithoutScan(t *testing.T) {
	_, res := evalSeed(t)
	a := hasAlert(res, "MOVE_UNSCANNED")
	if a == nil || a.BatchID != "LOT-2" || a.CellCode != "A-02-03" {
		t.Fatalf("expected unscanned move LOT-2 -> A-02-03, got %+v", a)
	}
	gap := false
	for _, c := range res.Gaps {
		if c == "A-02-03" {
			gap = true
		}
	}
	if !gap {
		t.Fatalf("A-02-03 must be a spatial gap, gaps=%v", res.Gaps)
	}
	// The legitimately scanned move must not be reported.
	for _, al := range res.Alerts {
		if al.Code == "MOVE_UNSCANNED" && al.BatchID == "LOT-1" {
			t.Fatal("LOT-1 move had a scan and must not alert")
		}
	}
	// offline node surfaced too
	if a := hasAlert(res, "NODE_OFFLINE"); a == nil || a.NodeID != "n-dead" {
		t.Fatalf("expected n-dead offline, got %+v", a)
	}
}

// 4. 探针未达中心 -> PROBE_NOT_CENTER；跨温区批按所在温区独立判定。
func TestProbeDepthAndBatchAcrossTwoZones(t *testing.T) {
	_, res := evalSeed(t)
	var probe, coreOut *domain.Alert
	for i := range res.Alerts {
		switch res.Alerts[i].Code {
		case "PROBE_NOT_CENTER":
			if res.Alerts[i].BatchID == "LOT-3" {
				probe = &res.Alerts[i]
			}
		case "CORE_OUT_OF_BAND":
			if res.Alerts[i].BatchID == "LOT-3" && res.Alerts[i].CellCode == "B-01-01" {
				coreOut = &res.Alerts[i]
			}
		}
	}
	if probe == nil {
		t.Fatal("expected PROBE_NOT_CENTER for shallow CM-2")
	}
	if coreOut == nil {
		t.Fatal("expected CORE_OUT_OF_BAND for -15°C ice cream in FREEZE band [-22,-18]")
	}
	x := hasAlert(res, "BATCH_CROSS_ZONE")
	if x == nil || x.BatchID != "LOT-3" {
		t.Fatalf("expected BATCH_CROSS_ZONE for LOT-3, got %+v", x)
	}
	// The -2°C shallow reading in the disallowed CHILL segment also creates a mismatch.
	if a := hasAlert(res, "ZONE_MISMATCH"); a == nil || a.BatchID != "LOT-3" {
		t.Fatalf("expected ZONE_MISMATCH for ice cream in CHILL, got %+v", a)
	}
}

// 5. 冻结随机格位/时点：完成的不报警，缺失的必须报警；本地差异不能被均值掩盖。
func TestFrozenPlanAndLocalDelta(t *testing.T) {
	sc, res := evalSeed(t)
	missing := map[string]bool{}
	for _, m := range res.Missing {
		missing[m.CellCode+"@"+m.At.Format(time.RFC3339)] = true
	}
	if len(res.Missing) == 0 {
		t.Fatal("expected at least one missing frozen measurement")
	}
	// A-01-02 first point was measured (CM-1) and must not appear.
	measured := sc.Now.Add(-30 * time.Minute)
	for _, m := range res.Missing {
		if m.CellCode == "A-01-02" && m.At.Equal(measured) {
			t.Fatalf("A-01-02 had CM-1 at the first frozen point, must not be missing: %+v", m)
		}
	}
	if a := hasAlert(res, "FROZEN_MEASUREMENT_MISSING"); a == nil {
		t.Fatal("expected FROZEN_MEASUREMENT_MISSING alert")
	}

	// Evaporator cell A-03-02: -0.8°C vs zone mean ~+2°C => local cold delta.
	var ld *rules.LocalDeviation
	for i := range res.Deviations {
		if res.Deviations[i].CellCode == "A-03-02" {
			ld = &res.Deviations[i]
		}
	}
	if ld == nil {
		t.Fatalf("expected local deviation at evaporator cell, deviations=%+v", res.Deviations)
	}
	if ld.DeltaC >= 0 || !ld.NearEvap {
		t.Fatalf("evaporator cell must read locally colder and be flagged NearEvap: %+v", ld)
	}
	// Door cell warming is surfaced both as out-of-band air and a contextual local delta.
	if a := hasAlert(res, "AIR_OUT_OF_BAND"); a == nil || a.CellCode != "A-01-02" {
		t.Fatalf("expected doorway cell A-01-02 air out-of-band, got %+v", a)
	}
}

func TestFreezePlanLocksCellsAndPoints(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Hour)
	sc := seed.Build(now)
	at := now.Add(-7 * time.Minute)
	if !sc.Snapshot.Plans[0].Frozen("A-01-02", sc.Snapshot.Plans[0].Points[0].At) {
		t.Fatal("frozen cell/point must be locked")
	}
	if sc.Snapshot.Plans[0].Frozen("A-01-02", at) {
		t.Fatal("non-frozen instant must not be locked")
	}
	if sc.Snapshot.Plans[0].Frozen("A-09-99", sc.Snapshot.Plans[0].Points[0].At) {
		t.Fatal("non-frozen cell must not be locked")
	}
}

// Package seed builds a deterministic, layered dairy store used by demo mode
// and the rule tests. It deliberately contains all five verification
// scenarios (covered node, unscanned move, bouncing door, shallow probe,
// one batch across two zones).
package seed

import (
	"context"
	"math/rand"
	"time"

	"coldcheck/domain"
	"coldcheck/rules"
	"coldcheck/store"
)

func must0(err error) {
	if err != nil {
		panic(err)
	}
}

type tctx struct{}

func (tctx) Deadline() (time.Time, bool) { return time.Time{}, false }
func (tctx) Done() <-chan struct{}       { return nil }
func (tctx) Err() error                  { return nil }
func (tctx) Value(any) any               { return nil }

type Scenario struct {
	Now      time.Time
	Snapshot *domain.Snapshot
	Memory   store.Store
	PlanID   string
}

// Clock always returns the scenario time, so a demo server evaluates the
// complete seeded window instead of the host's wall clock.
type Clock struct{ T time.Time }

func (c Clock) Now() time.Time { return c.T }

// Build populates the in-memory store.
// layer 0 = 冷藏/冷冻层, layer 1 = 近门穿堂缓冲.
func Build(now time.Time) *Scenario {
	st := store.NewMemory()
	ctx := tctx{}
	win := 4 * time.Hour
	must0(st.SaveLayoutVersion(ctx, domain.LayoutVersion{ID: "L1", Active: true, CreatedAt: now.Add(-24 * time.Hour), Note: "初始库位版本"}))

	chill := domain.Zone{Code: "CHILL", Name: "冷藏区", Layer: 0, MinC: 0, MaxC: 4, TargetC: 2,
		DairySegs: []string{"FRESH-MILK", "YOGURT", "HARD-CHEESE"},
		Polygon:   [][2]float64{{0, 0}, {8, 0}, {8, 6}, {0, 6}}}
	freeze := domain.Zone{Code: "FREEZE", Name: "冷冻区", Layer: 0, MinC: -22, MaxC: -18, TargetC: -20,
		DairySegs: []string{"ICE-CREAM"}, Polygon: [][2]float64{{8, 0}, {12, 0}, {12, 6}, {8, 6}}}
	buffer := domain.Zone{Code: "BUFFER", Name: "门口缓冲", Layer: 1, MinC: 0, MaxC: 6, TargetC: 3,
		DairySegs: []string{"FRESH-MILK", "YOGURT", "HARD-CHEESE"},
		Polygon:   [][2]float64{{0, 6}, {4, 6}, {4, 8}, {0, 8}}}
	must0(st.UpsertZone(ctx, chill))
	must0(st.UpsertZone(ctx, freeze))
	must0(st.UpsertZone(ctx, buffer))

	grid := []struct {
		code   string
		x, y   float64
		nd, ne bool
	}{
		{"A-01-01", 1, 1, false, false}, {"A-01-02", 1, 2, true, false}, {"A-01-03", 1, 3, false, false},
		{"A-02-01", 3, 1, false, false}, {"A-02-02", 3, 2, false, false}, {"A-02-03", 3, 3, false, true},
		{"A-03-01", 5, 1, false, false}, {"A-03-02", 5, 2, false, true}, {"A-03-03", 5, 3, false, false},
		{"B-01-01", 9, 1, false, false}, {"B-01-02", 9, 2, false, false},
		{"U-01-01", 1, 7, true, false},
	}
	cells := map[string]*domain.Cell{}
	for _, g := range grid {
		zone := "CHILL"
		if g.code[0] == 'B' {
			zone = "FREEZE"
		}
		if g.code[0] == 'U' {
			zone = "BUFFER"
		}
		c := &domain.Cell{Code: g.code, ZoneCode: zone, Layer: 0, X: g.x, Y: g.y,
			NearDoor: g.nd, NearEvap: g.ne, Active: true}
		if g.code[0] == 'U' {
			c.Layer = 1
		}
		cells[g.code] = c
		must0(st.UpsertCell(ctx, *c))
	}
	must0(st.UpsertDoor(ctx, domain.Door{ID: "D1", CellCode: "A-01-02", Name: "冷藏穿堂门"}))
	must0(st.UpsertDoor(ctx, domain.Door{ID: "D2", CellCode: "U-01-01", Name: "装卸门"}))
	must0(st.UpsertDoor(ctx, domain.Door{ID: "D3", CellCode: "A-03-03", Name: "冷藏后门"}))

	// Two evaporators in DIFFERENT zones defrost on independent schedules.
	// EV-1 explicitly serves the A-03-03 coil bay (other CHILL cells keep
	// routine air); EV-2 serves the whole FREEZE zone. Both are read-only:
	// the platform ingests state and draws rise windows, never starts one.
	must0(st.UpsertEvaporator(ctx, domain.Evaporator{
		ID: "EV-1", ZoneCode: "CHILL", Name: "冷藏蒸发器1", Cells: []string{"A-03-03"},
	}))
	must0(st.UpsertEvaporator(ctx, domain.Evaporator{ID: "EV-2", ZoneCode: "FREEZE", Name: "冷冻蒸发器1"}))

	// A-02-01 is covered by adjacent A-01-01. A-02-03 is a deliberate gap.
	mount := map[string]string{
		"A-01-01": "n-a11", "A-01-02": "n-a12", "A-01-03": "n-a13",
		"A-03-01": "n-a31", "A-03-02": "n-a32", "A-03-03": "n-a33",
		"B-01-01": "n-b11", "B-01-02": "n-b12",
		"U-01-01": "n-u11",
	}
	for cell, id := range mount {
		c := cells[cell]
		must0(st.UpsertNode(ctx, domain.Node{ID: id, CellCode: cell, Layer: c.Layer,
			BaselineRSSI: -70, Deadline: 20 * time.Minute, Active: true}))
	}
	must0(st.UpsertNode(ctx, domain.Node{ID: "n-dead", CellCode: "A-02-01", Layer: 0,
		BaselineRSSI: -72, Deadline: 20 * time.Minute, Active: true}))

	must0(st.UpsertBatch(ctx, domain.Batch{ID: "LOT-1", Product: "鲜牛奶 1L", SegCode: "FRESH-MILK", Lot: "20260911-01",
		MinC: 0, MaxC: 4, MaxExposure: 30 * time.Minute}))
	must0(st.UpsertBatch(ctx, domain.Batch{ID: "LOT-2", Product: "硬质干酪", SegCode: "HARD-CHEESE", Lot: "20260911-02",
		MinC: 0, MaxC: 6, MaxExposure: 45 * time.Minute}))
	must0(st.UpsertBatch(ctx, domain.Batch{ID: "LOT-3", Product: "冰淇淋", SegCode: "ICE-CREAM", Lot: "20260911-03",
		MinC: -22, MaxC: -18, MaxExposure: 10 * time.Minute}))
	must0(st.UpsertBatch(ctx, domain.Batch{ID: "LOT-4", Product: "酸奶", SegCode: "YOGURT", Lot: "20260911-04",
		MinC: 0, MaxC: 4, MaxExposure: 30 * time.Minute}))
	// LOT-5 occupies the EV-1 served bay A-03-03 around its defrost cycle so
	// core measurements in / across the rise window have a product context.
	must0(st.UpsertBatch(ctx, domain.Batch{ID: "LOT-5", Product: "巴氏鲜奶", SegCode: "FRESH-MILK", Lot: "20260911-05",
		MinC: 0, MaxC: 4, MaxExposure: 30 * time.Minute}))

	// LOT-1 relocation WITH scan.
	must0(st.AddOccupancy(ctx, domain.Occupancy{BatchID: "LOT-1", CellCode: "A-01-03",
		From: now.Add(-3 * time.Hour), To: now.Add(-90 * time.Minute)}))
	must0(st.AddOccupancy(ctx, domain.Occupancy{BatchID: "LOT-1", CellCode: "A-01-02",
		From: now.Add(-90 * time.Minute), To: now.Add(2 * time.Hour),
		MoveScan: true, MoveScanAt: now.Add(-90 * time.Minute)}))
	must0(st.AddMoveScan(ctx, domain.MoveScan{ScanID: "S-1", BatchID: "LOT-1",
		FromCell: "A-01-03", ToCell: "A-01-02", At: now.Add(-90 * time.Minute)}))

	// LOT-2 relocation WITHOUT scan (occupies the spatial-gap cell).
	must0(st.AddOccupancy(ctx, domain.Occupancy{BatchID: "LOT-2", CellCode: "A-03-01",
		From: now.Add(-3 * time.Hour), To: now.Add(-40 * time.Minute)}))
	must0(st.AddOccupancy(ctx, domain.Occupancy{BatchID: "LOT-2", CellCode: "A-02-03",
		From: now.Add(-40 * time.Minute), To: now.Add(2 * time.Hour)}))
	must0(st.AddPresenceEvent(ctx, domain.PresenceEvent{Kind: "arrive", BatchID: "LOT-2",
		CellCode: "A-02-03", At: now.Add(-40 * time.Minute)}))

	// LOT-3 spread across FREEZE + CHILL, and CHILL does not allow ICE-CREAM.
	must0(st.AddOccupancy(ctx, domain.Occupancy{BatchID: "LOT-3", CellCode: "B-01-01",
		From: now.Add(-2 * time.Hour), To: now.Add(2 * time.Hour)}))
	must0(st.AddOccupancy(ctx, domain.Occupancy{BatchID: "LOT-3", CellCode: "A-03-03",
		From: now.Add(-30 * time.Minute), To: now.Add(2 * time.Hour)}))

	// LOT-5 occupies A-03-03 (EV-1 served bay) from -115m onward, spanning
	// the whole defrost rise window; short warm-air exposure stays annotated
	// and under the batch exposure limit.
	must0(st.AddOccupancy(ctx, domain.Occupancy{BatchID: "LOT-5", CellCode: "A-03-03",
		From: now.Add(-115 * time.Minute), To: now.Add(2 * time.Hour)}))

	// LOT-4 near the evaporator: locally cold while zone mean stays in band.
	must0(st.AddOccupancy(ctx, domain.Occupancy{BatchID: "LOT-4", CellCode: "A-03-02",
		From: now.Add(-3 * time.Hour), To: now.Add(2 * time.Hour)}))

	bucket := rules.DefaultParams().Bucket
	// EV-1 active heating: [-110m,-100m); rise window incl. recovery: until -85m.
	defrostWarm := func(t time.Time) bool {
		return !t.Before(now.Add(-110*time.Minute)) && t.Before(now.Add(-85*time.Minute))
	}
	for t := now.Add(-win + bucket/2); t.Before(now); t = t.Add(bucket) {
		doorWarm := func(base float64) float64 {
			if t.After(now.Add(-70*time.Minute)) && t.Before(now.Add(-45*time.Minute)) {
				return 5.6
			}
			return base
		}
		readings := map[string]float64{
			"n-a11": 2.1, "n-a12": doorWarm(2.6), "n-a13": 2.05,
			"n-a22": 2.2, "n-a31": 2.0,
			"n-a32": -0.8,
			"n-a33": 2.1, "n-b11": -20.0, "n-b12": -19.6, "n-u11": 3.4,
		}
		// EV-1 defrost heats the CHILL cell near its coil; the warm readings
		// stay inside the read-only rise window and never enter routine stats.
		if defrostWarm(t) {
			readings["n-a33"] = 6.8
		}
		for id, c := range readings {
			rssi := -68
			if id == "n-a13" {
				rssi = -92 // carton covering the node
			}
			must0(st.AddAirReading(ctx, domain.AirReading{NodeID: id, At: t, C: c, RSSI: rssi}))
		}
	}
	must0(st.AddAirReading(ctx, domain.AirReading{NodeID: "n-dead", At: now.Add(-2 * time.Hour), C: 2.2, RSSI: -70}))

	// EV-1 defrost state pair. The END report is late telemetry (device time
	// -100m, ingested only at -3m), so pairing must still draw the historical
	// window from device time and flag DEFROST_STATE_LATE.
	must0(st.AddDefrostEvent(ctx, domain.DefrostEvent{EvapID: "EV-1",
		At: now.Add(-110 * time.Minute), Starting: true, IngestedAt: now.Add(-109 * time.Minute)}))
	must0(st.AddDefrostEvent(ctx, domain.DefrostEvent{EvapID: "EV-1",
		At: now.Add(-100 * time.Minute), Starting: false, IngestedAt: now.Add(-3 * time.Minute)}))
	// EV-2 defrosts independently in FREEZE; reports arrive on time, and the
	// zone air stays in band (defrost state is annotated, not inferred).
	must0(st.AddDefrostEvent(ctx, domain.DefrostEvent{EvapID: "EV-2",
		At: now.Add(-50 * time.Minute), Starting: true, IngestedAt: now.Add(-50 * time.Minute)}))
	must0(st.AddDefrostEvent(ctx, domain.DefrostEvent{EvapID: "EV-2",
		At: now.Add(-40 * time.Minute), Starting: false, IngestedAt: now.Add(-40 * time.Minute)}))

	// Door D3 opens while EV-1 is heating: two warming causes coincide.
	must0(st.AddRawDoorEvent(ctx, domain.RawDoorEvent{DoorID: "D3", At: now.Add(-106 * time.Minute), IsOpen: true, RSSI: -61}))
	must0(st.AddRawDoorEvent(ctx, domain.RawDoorEvent{DoorID: "D3", At: now.Add(-103 * time.Minute), IsOpen: false, RSSI: -62}))

	// n-b12 enters maintenance exactly when EV-2 defrost starts. The interval
	// is open, so its readings are dropped from the series and the node is not
	// offline despite going silent.
	must0(st.AddNodeMaintenance(ctx, domain.NodeMaintenance{NodeID: "n-b12",
		From: now.Add(-50 * time.Minute), Reason: "校准/换电池"}))

	// D1: contact bounce (open/closed/open within 2s) then one real 20m open.
	o := now.Add(-60 * time.Minute)
	must0(st.AddRawDoorEvent(ctx, domain.RawDoorEvent{DoorID: "D1", At: o, IsOpen: true, RSSI: -60}))
	must0(st.AddRawDoorEvent(ctx, domain.RawDoorEvent{DoorID: "D1", At: o.Add(1 * time.Second), IsOpen: false, RSSI: -61}))
	must0(st.AddRawDoorEvent(ctx, domain.RawDoorEvent{DoorID: "D1", At: o.Add(2 * time.Second), IsOpen: true, RSSI: -60}))
	must0(st.AddRawDoorEvent(ctx, domain.RawDoorEvent{DoorID: "D1", At: o.Add(20 * time.Minute), IsOpen: false, RSSI: -62}))
	// D2 is currently stuck open.
	must0(st.AddRawDoorEvent(ctx, domain.RawDoorEvent{DoorID: "D2", At: now.Add(-8 * time.Minute), IsOpen: true, RSSI: -58}))

	// Quality freeze with deterministic cells/points.
	snap, _ := st.Load(tctx{}, now, win)
	rng := rand.New(rand.NewSource(42))
	plan := rules.RandomFreeze(snap, "FZ-1", now.Add(-2*time.Hour), now.Add(time.Hour), 2, 2, rng)
	plan.Cells = []domain.FrozenCell{{CellCode: "A-01-02"}, {CellCode: "A-03-01"}}
	plan.Points = []domain.FrozenTimePoint{{At: now.Add(-30 * time.Minute)}, {At: now.Add(-15 * time.Minute)}}
	must0(st.SaveFreezePlan(ctx, plan))

	must0(st.AddCoreMeasurement(ctx, domain.CoreMeasurement{
		ID: "CM-1", BatchID: "LOT-1", CellCode: "A-01-02", PlanID: "FZ-1",
		At: now.Add(-30 * time.Minute), C: 3.1,
		ProbeDepthMM: 120, RequiredDepthMM: 120, ReachedCenter: true, Operator: "qa-li"}))
	must0(st.AddCoreMeasurement(ctx, domain.CoreMeasurement{
		ID: "CM-2", BatchID: "LOT-3", CellCode: "A-03-03", At: now.Add(-12 * time.Minute), C: -2.0,
		ProbeDepthMM: 40, RequiredDepthMM: 110, ReachedCenter: false, Operator: "wh-wang"}))
	must0(st.AddCoreMeasurement(ctx, domain.CoreMeasurement{
		ID: "CM-3", BatchID: "LOT-3", CellCode: "B-01-01", At: now.Add(-10 * time.Minute), C: -15.0,
		ProbeDepthMM: 100, RequiredDepthMM: 100, ReachedCenter: true, Operator: "wh-wang"}))
	// CM-4: in-band core measured while EV-1 is heating -> separated into the
	// defrost-window set but raises no band or quality verdict.
	must0(st.AddCoreMeasurement(ctx, domain.CoreMeasurement{
		ID: "CM-4", BatchID: "LOT-5", CellCode: "A-03-03", At: now.Add(-105 * time.Minute), C: 2.4,
		ProbeDepthMM: 120, RequiredDepthMM: 120, ReachedCenter: true, Operator: "qa-li"}))
	// CM-5: within CoreDefrostMargin of the rise-window end (-85m) -> crossing
	// the window edge, kept apart from both routine and defrost sets.
	must0(st.AddCoreMeasurement(ctx, domain.CoreMeasurement{
		ID: "CM-5", BatchID: "LOT-5", CellCode: "A-03-03", At: now.Add(-83 * time.Minute), C: 2.6,
		ProbeDepthMM: 120, RequiredDepthMM: 120, ReachedCenter: true, Operator: "qa-li"}))

	snap, _ = st.Load(tctx{}, now, win)
	return &Scenario{Now: now, Snapshot: snap, Memory: timedStore{Store: st, now: now}, PlanID: "FZ-1"}
}

// timedStore forces Load's "now" to the seed time; writes pass through.
type timedStore struct {
	store.Store
	now time.Time
}

func (s timedStore) Load(ctx context.Context, _ time.Time, window time.Duration) (*domain.Snapshot, error) {
	return s.Store.Load(ctx, s.now, window)
}

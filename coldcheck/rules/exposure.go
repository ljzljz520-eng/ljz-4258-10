package rules

import (
	"sort"
	"time"

	"coldcheck/domain"
)

type Interval struct {
	From, To time.Time
}

type CellPoint struct {
	CellCode string
	At       time.Time
	C        float64
	NearDoor bool
	NearEvap bool
	// DefrostEvap lists evaporators whose rise windows overlap this bucket;
	// empty means a routine (non-defrost-affected) bucket.
	DefrostEvap []string
	// DefrostCross marks a bucket that straddles a rise-window edge, so its
	// air value is partly routine and partly defrost-affected.
	DefrostCross bool
}

type AirSeries struct {
	CellCode string
	ZoneCode string
	Layer    int
	MinC     float64
	MaxC     float64
	Points   []CellPoint
}

type ExposureInterval struct {
	BatchID  string
	CellCode string
	ZoneCode string
	From, To time.Time
	Reason   string // "air-out-of-band", "door-open", "move", "zone-mismatch"
	MinC     float64
	MaxC     float64
	AirC     float64 // representative air temperature, zero when unavailable
	// DefrostEvap annotates evidence that overlaps a defrost rise window;
	// the interval is still counted, but the UI keeps it apart from routine
	// exposure. It never changes a quality verdict.
	DefrostEvap []string
}

func dur(a, b time.Time) time.Duration { return b.Sub(a) }

func overlap(a, b Interval) (time.Time, time.Time, bool) {
	s := a.From
	if b.From.After(s) {
		s = b.From
	}
	e := a.To
	if b.To.Before(e) {
		e = b.To
	}
	return s, e, s.Before(e)
}

// zoneAt resolves the zone of a cell from a layout snapshot.
func zoneAt(snap *domain.Snapshot, cellCode string) *domain.Zone {
	if c, ok := snap.Cells[cellCode]; ok {
		return snap.Zones[c.ZoneCode]
	}
	return nil
}

// BuildAirSeries groups node readings by mounted cell, resampled onto fixed
// buckets so nodes on different uplink periods can be compared and a local
// mean computed instead of relying on a warehouse-wide average. Readings
// whose node is under maintenance in the bucket interval are dropped, and
// each produced bucket is annotated with the defrost rise windows covering
// it so routine and defrost-affected air stay separated downstream.
func BuildAirSeries(snap *domain.Snapshot, p Params, ws []DefrostWindow) map[string]*AirSeries {
	type agg struct {
		sum float64
		n   int
	}
	buckets := map[string]map[time.Time]*agg{}
	meta := map[string]*AirSeries{}
	for _, r := range snap.Air {
		if snap.InMaintenance(r.NodeID, r.At) {
			continue // calibration/swap window: reading is untrustworthy
		}
		n, ok := snap.Nodes[r.NodeID]
		if !ok || !n.Active {
			continue
		}
		c, ok := snap.Cells[n.CellCode]
		if !ok || !c.Active {
			continue
		}
		z := snap.Zones[c.ZoneCode]
		if z == nil {
			continue
		}
		s, ok := meta[n.CellCode]
		if !ok {
			s = &AirSeries{CellCode: n.CellCode, ZoneCode: c.ZoneCode, Layer: n.Layer, MinC: z.MinC, MaxC: z.MaxC}
			meta[n.CellCode] = s
			buckets[n.CellCode] = map[time.Time]*agg{}
		}
		b := r.At.Truncate(p.Bucket)
		a := buckets[n.CellCode][b]
		if a == nil {
			a = &agg{}
			buckets[n.CellCode][b] = a
		}
		a.sum += r.C
		a.n++
	}
	for cell, bs := range buckets {
		var ts []time.Time
		for t := range bs {
			ts = append(ts, t)
		}
		sort.Slice(ts, func(i, j int) bool { return ts[i].Before(ts[j]) })
		c := snap.Cells[cell]
		cellWindows := WindowsForCell(snap, ws, cell)
		for _, t := range ts {
			a := bs[t]
			pt := CellPoint{
				CellCode: cell, At: t.Add(p.Bucket / 2), C: a.sum / float64(a.n),
				NearDoor: c.NearDoor, NearEvap: c.NearEvap,
			}
			biv := Interval{t, t.Add(p.Bucket)}
			for _, w := range cellWindows {
				if _, _, ok := overlap(biv, w.RiseInterval()); ok {
					pt.DefrostEvap = append(pt.DefrostEvap, w.EvapID)
				}
				// A bucket that also contains active-heating time straddles
				// the defrost start/end edge (recovery-only buckets don't).
				if ai := w.ActiveInterval(); ai.To.After(ai.From) {
					if _, _, ok := overlap(biv, ai); ok {
						pt.DefrostCross = true
					}
				}
			}
			meta[cell].Points = append(meta[cell].Points, pt)
		}
	}
	return meta
}

func inBand(c, min, max float64) bool { return c >= min && c <= max }

// BatchExposure reconstructs, for each batch occupancy, every interval in
// which the local air node is outside the zone band, the cell door is open,
// the product was moved, or the batch sat in a zone that does not allow its
// product segment. Adjacent same-reason intervals are merged. Intervals
// intersecting a defrost rise window keep their reason and duration but are
// annotated with the evaporator id so evidence stays separable; defrost is a
// known warming cause, never a verdict on product quality.
func BatchExposure(snap *domain.Snapshot, series map[string]*AirSeries, doors []domain.DoorEvent, ws []DefrostWindow, p Params) []ExposureInterval {
	var ex []ExposureInterval
	prevCell := map[string]string{}
	defrostTags := func(cell string, from, to time.Time) []string {
		var tags []string
		for _, w := range WindowsForCell(snap, ws, cell) {
			if s, e, ok := overlap(Interval{from, to}, w.RiseInterval()); ok && e.After(s) {
				tags = append(tags, w.EvapID)
			}
		}
		return tags
	}
	appendRun := func(run []ExposureInterval, e ExposureInterval) []ExposureInterval {
		if len(run) > 0 {
			last := &run[len(run)-1]
			if e.BatchID == last.BatchID && e.CellCode == last.CellCode && e.Reason == last.Reason && !e.From.After(last.To.Add(time.Second)) {
				if e.To.After(last.To) {
					last.To = e.To
				}
				for _, t := range e.DefrostEvap {
					found := false
					for _, ex2 := range last.DefrostEvap {
						if ex2 == t {
							found = true
						}
					}
					if !found {
						last.DefrostEvap = append(last.DefrostEvap, t)
					}
				}
				return run
			}
		}
		return append(run, e)
	}

	for _, occ := range snap.Occupancies {
		b, ok := snap.Batches[occ.BatchID]
		if !ok {
			continue
		}
		c, ok := snap.Cells[occ.CellCode]
		if !ok {
			continue
		}
		z := snap.Zones[c.ZoneCode]
		if z == nil {
			continue
		}
		// Zone/segment mismatch covers the whole occupancy.
		allowed := false
		for _, s := range z.DairySegs {
			if s == b.SegCode {
				allowed = true
				break
			}
		}
		if !allowed {
			ex = appendRun(ex, ExposureInterval{
				BatchID: b.ID, CellCode: occ.CellCode, ZoneCode: z.Code,
				From: occ.From, To: occ.To, Reason: "zone-mismatch",
				MinC: z.MinC, MaxC: z.MaxC,
			})
		}
		// A relocation (a later occupancy in another cell) without a scan
		// implies a move exposure event. The first occupancy is put-away,
		// not a relocation, and does not count as move exposure.
		if !occ.MoveScan {
			if pr, ok := prevCell[b.ID]; ok && pr != occ.CellCode {
				ex = appendRun(ex, ExposureInterval{
					BatchID: b.ID, CellCode: occ.CellCode, ZoneCode: z.Code,
					From: occ.From, To: occ.From.Add(time.Minute), Reason: "move",
				})
			}
		}
		prevCell[b.ID] = occ.CellCode
		// Door-open overlap with this cell.
		for _, d := range doors {
			if snap.Doors[d.DoorID] == nil || snap.Doors[d.DoorID].CellCode != occ.CellCode {
				continue
			}
			s, e, ok2 := overlap(Interval{occ.From, occ.To}, Interval{d.OpenedAt, d.ClosedAt})
			if ok2 {
				ex = appendRun(ex, ExposureInterval{
					BatchID: b.ID, CellCode: occ.CellCode, ZoneCode: z.Code,
					From: s, To: e, Reason: "door-open",
					DefrostEvap: defrostTags(occ.CellCode, s, e),
				})
			}
		}
		// Air out of band, using local cell points only.
		if s := series[occ.CellCode]; s != nil {
			for _, pt := range s.Points {
				if !inBand(pt.C, z.MinC, z.MaxC) {
					s2, e2, ok2 := overlap(Interval{occ.From, occ.To},
						Interval{pt.At.Add(-p.Bucket / 2), pt.At.Add(p.Bucket / 2)})
					if ok2 {
						ex = appendRun(ex, ExposureInterval{
							BatchID: b.ID, CellCode: occ.CellCode, ZoneCode: z.Code,
							From: s2, To: e2, Reason: "air-out-of-band",
							MinC: z.MinC, MaxC: z.MaxC, AirC: pt.C,
							DefrostEvap: append([]string(nil), pt.DefrostEvap...),
						})
					}
				}
			}
		}
	}
	sort.Slice(ex, func(i, j int) bool {
		if ex[i].BatchID != ex[j].BatchID {
			return ex[i].BatchID < ex[j].BatchID
		}
		return ex[i].From.Before(ex[j].From)
	})
	return ex
}

// TotalExposure sums exposure duration per batch.
func TotalExposure(ex []ExposureInterval) map[string]time.Duration {
	t := map[string]time.Duration{}
	for _, e := range ex {
		t[e.BatchID] += e.To.Sub(e.From)
	}
	return t
}

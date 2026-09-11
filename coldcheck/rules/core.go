package rules

import (
	"sort"
	"time"

	"coldcheck/domain"
)

type CoreIssue struct {
	MeasurementID string
	BatchID       string
	CellCode      string
	PlanID        string
	At            time.Time
	C             float64
	Code          string // PROBE_NOT_CENTER, CORE_OUT_OF_BAND, CORE_FROZEN_OUT_OF_BAND
	Msg           string
}

type MissingFrozen struct {
	PlanID   string
	CellCode string
	At       time.Time
}

// CoreChecks validates manual probe readings.
// A probe that did not reach the geometric centre is flagged and its value
// must not be treated as a trustworthy core temperature.
func CoreChecks(snap *domain.Snapshot, p Params) []CoreIssue {
	var out []CoreIssue
	for _, m := range snap.Core {
		b := snap.Batches[m.BatchID]
		if b == nil {
			continue
		}
		if !m.ReachedCenter && m.RequiredDepthMM > 0 &&
			m.ProbeDepthMM < m.RequiredDepthMM-p.ProbeToleranceMM {
			out = append(out, CoreIssue{
				MeasurementID: m.ID, BatchID: m.BatchID, CellCode: m.CellCode,
				PlanID: m.PlanID, At: m.At, C: m.C, Code: "PROBE_NOT_CENTER",
				Msg: "芯温探针未达到产品中心，读数不能作为芯温结论",
			})
		}
		if m.C < b.MinC || m.C > b.MaxC {
			code := "CORE_OUT_OF_BAND"
			plan := ""
			for _, pl := range snap.Plans {
				if m.PlanID == pl.ID {
					plan = pl.ID
					if pl.Locked {
						code = "CORE_FROZEN_OUT_OF_BAND"
					}
				}
			}
			if m.PlanID != "" && plan == "" {
				code = "CORE_OUT_OF_BAND"
			}
			out = append(out, CoreIssue{
				MeasurementID: m.ID, BatchID: m.BatchID, CellCode: m.CellCode,
				PlanID: m.PlanID, At: m.At, C: m.C, Code: code,
				Msg: "抽测芯温超出产品段温度带",
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out
}

// MissingFrozenMeasurements verifies the quality freeze: every combination
// of frozen random cell and frozen measurement instant must have a manual
// core measurement close in time and at the right cell.
func MissingFrozenMeasurements(snap *domain.Snapshot, tolerance time.Duration) []MissingFrozen {
	var out []MissingFrozen
	for _, pl := range snap.Plans {
		if !pl.Locked {
			continue
		}
		cells := append([]domain.FrozenCell(nil), pl.Cells...)
		points := append([]domain.FrozenTimePoint(nil), pl.Points...)
		sort.Slice(cells, func(i, j int) bool { return cells[i].CellCode < cells[j].CellCode })
		sort.Slice(points, func(i, j int) bool { return points[i].At.Before(points[j].At) })
		for _, fc := range cells {
			for _, tp := range points {
				found := false
				for _, m := range snap.Core {
					if m.CellCode != fc.CellCode {
						continue
					}
					dt := m.At.Sub(tp.At)
					if dt < 0 {
						dt = -dt
					}
					if dt <= tolerance {
						found = true
						break
					}
				}
				if !found {
					out = append(out, MissingFrozen{PlanID: pl.ID, CellCode: fc.CellCode, At: tp.At})
				}
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].PlanID != out[j].PlanID {
			return out[i].PlanID < out[j].PlanID
		}
		return out[i].At.Before(out[j].At)
	})
	return out
}

type CrossZone struct {
	BatchID string
	Zones   []string
}

// BatchZoneSpread finds a batch physically spread across more than one
// temperature zone in the window. Neither segment is "the batch temperature";
// each occupancy is evaluated against its own zone band.
func BatchZoneSpread(snap *domain.Snapshot) []CrossZone {
	zonesByBatch := map[string]map[string]bool{}
	for _, o := range snap.Occupancies {
		c, ok := snap.Cells[o.CellCode]
		if !ok {
			continue
		}
		if zonesByBatch[o.BatchID] == nil {
			zonesByBatch[o.BatchID] = map[string]bool{}
		}
		zonesByBatch[o.BatchID][c.ZoneCode] = true
	}
	ids := make([]string, 0)
	for b := range zonesByBatch {
		ids = append(ids, b)
	}
	sort.Strings(ids)
	var out []CrossZone
	for _, b := range ids {
		zs := zonesByBatch[b]
		if len(zs) > 1 {
			list := make([]string, 0, len(zs))
			for z := range zs {
				list = append(list, z)
			}
			sort.Strings(list)
			out = append(out, CrossZone{BatchID: b, Zones: list})
		}
	}
	return out
}

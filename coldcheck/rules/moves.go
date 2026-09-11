package rules

import (
	"sort"
	"time"

	"coldcheck/domain"
)

type MoveIssue struct {
	BatchID  string
	FromCell string
	ToCell   string
	At       time.Time
	Reason   string // "occupancy-without-scan", "arrive-without-scan", "scan-without-move"
}

// UnscannedMoves reconciles physical pallet evidence with gun scans:
//   - a new occupancy (relocation) without MoveScan is a missed scan;
//   - an inferred "arrive" presence without a nearby scan is missed too;
//   - a scan claiming a move while no occupancy changes is data noise.
func UnscannedMoves(snap *domain.Snapshot, tolerance time.Duration) []MoveIssue {
	var issues []MoveIssue

	scanNear := func(batch, from, to string, at time.Time) bool {
		for _, sc := range snap.MoveScans {
			if sc.BatchID != batch {
				continue
			}
			dt := sc.At.Sub(at)
			if dt < 0 {
				dt = -dt
			}
			if dt <= tolerance && (from == "" || sc.FromCell == from) && (to == "" || sc.ToCell == to) {
				return true
			}
		}
		return false
	}

	occs := append([]domain.Occupancy(nil), snap.Occupancies...)
	sort.Slice(occs, func(i, j int) bool {
		if occs[i].BatchID == occs[j].BatchID {
			return occs[i].From.Before(occs[j].From)
		}
		return occs[i].BatchID < occs[j].BatchID
	})
	prev := map[string]domain.Occupancy{}
	for _, o := range occs {
		if pr, ok := prev[o.BatchID]; ok && pr.CellCode != o.CellCode && pr.To.After(o.From.Add(-time.Minute)) {
			if !o.MoveScan && !scanNear(o.BatchID, pr.CellCode, o.CellCode, o.From) {
				issues = append(issues, MoveIssue{
					BatchID: o.BatchID, FromCell: pr.CellCode, ToCell: o.CellCode,
					At: o.From, Reason: "occupancy-without-scan",
				})
			}
		}
		prev[o.BatchID] = o
	}

	events := append([]domain.PresenceEvent(nil), snap.Events...)
	sort.Slice(events, func(i, j int) bool { return events[i].At.Before(events[j].At) })
	for _, ev := range events {
		if ev.Kind != "arrive" {
			continue
		}
		if !scanNear(ev.BatchID, "", ev.CellCode, ev.At) {
			// already reported as occupancy-without-scan?
			dup := false
			for _, is := range issues {
				if is.BatchID == ev.BatchID && is.ToCell == ev.CellCode &&
					(is.At.Sub(ev.At) == 0 || within(is.At, ev.At, tolerance)) {
					dup = true
				}
			}
			if !dup {
				issues = append(issues, MoveIssue{
					BatchID: ev.BatchID, ToCell: ev.CellCode, At: ev.At,
					Reason: "arrive-without-scan",
				})
			}
		}
	}

	// Scans that no occupancy transition supports.
	cells := map[string]map[time.Time]bool{} // batch -> set of from/to change times
	for _, o := range occs {
		if cells[o.BatchID] == nil {
			cells[o.BatchID] = map[time.Time]bool{}
		}
		cells[o.BatchID][o.From] = true
	}
	for _, sc := range snap.MoveScans {
		ok := false
		for t := range cells[sc.BatchID] {
			if within(t, sc.At, tolerance) {
				ok = true
			}
		}
		if !ok {
			issues = append(issues, MoveIssue{
				BatchID: sc.BatchID, FromCell: sc.FromCell, ToCell: sc.ToCell,
				At: sc.At, Reason: "scan-without-move",
			})
		}
	}

	sort.Slice(issues, func(i, j int) bool { return issues[i].At.Before(issues[j].At) })
	return issues
}

func within(a, b time.Time, d time.Duration) bool {
	dt := a.Sub(b)
	if dt < 0 {
		dt = -dt
	}
	return dt <= d
}

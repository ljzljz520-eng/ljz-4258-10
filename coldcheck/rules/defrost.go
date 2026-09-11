package rules

import (
	"sort"
	"time"

	"coldcheck/domain"
)

// DefrostWindow is the read-only annotation of one defrost cycle:
// the active heating interval [Start, End) plus the thermal recovery tail
// ending at RiseUntil. Nothing here is inferred from the air temperature —
// the window is drawn strictly from reported defrost state, so a coil that
// reports nothing creates no window even if the air warms.
type DefrostWindow struct {
	EvapID    string
	ZoneCode  string
	Start     time.Time
	End       time.Time // zero when defrost is still active at evaluation time
	RiseUntil time.Time // End + recovery tail; equals evaluation instant when active
	Cells     []string  // cells covered; empty means the whole zone
	Ongoing   bool      // no end state yet
	Late      bool      // a state transition was ingested well after device time
}

// ActiveInterval is [Start, End); an ongoing defrost yields an empty interval.
func (w DefrostWindow) ActiveInterval() Interval {
	if w.Ongoing {
		return Interval{w.Start, w.Start}
	}
	return Interval{w.Start, w.End}
}

// RiseInterval is [Start, RiseUntil): active heating plus recovery tail.
func (w DefrostWindow) RiseInterval() Interval {
	return Interval{w.Start, w.RiseUntil}
}

func (w DefrostWindow) coversCell(snap *domain.Snapshot, cell string) bool {
	c, ok := snap.Cells[cell]
	if !ok {
		return false
	}
	if c.ZoneCode != w.ZoneCode {
		return false
	}
	if len(w.Cells) == 0 {
		return true // zone-wide evaporator
	}
	for _, cc := range w.Cells {
		if cc == cell {
			return true
		}
	}
	return false
}

// WindowsForCell returns the rise windows affecting a cell, in chronological
// order. Two independently defrosting evaporators may overlap here when their
// served cell sets intersect; callers must attribute evidence to each window
// rather than merging the two physical causes.
func WindowsForCell(snap *domain.Snapshot, ws []DefrostWindow, cell string) []DefrostWindow {
	var out []DefrostWindow
	for _, w := range ws {
		if w.coversCell(snap, cell) {
			out = append(out, w)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Start.Before(out[j].Start) })
	return out
}

// BuildDefrostWindows pairs read-only defrost state transitions per
// evaporator. Pairing uses DEVICE time (event.At), so a state report that
// arrives late still closes the correct historical interval; only the Late
// flag records the delayed arrival. Consecutive duplicate states are ignored.
// A start without an end is an ongoing defrost (open until now); an end
// without a preceding start is unmatched telemetry and yields no window.
func BuildDefrostWindows(snap *domain.Snapshot, p Params) []DefrostWindow {
	byEvap := map[string][]domain.DefrostEvent{}
	ids := make([]string, 0)
	for _, e := range snap.Defrosts {
		if _, ok := byEvap[e.EvapID]; !ok {
			ids = append(ids, e.EvapID)
		}
		byEvap[e.EvapID] = append(byEvap[e.EvapID], e)
	}
	sort.Strings(ids)

	var out []DefrostWindow
	for _, id := range ids {
		evs := append([]domain.DefrostEvent(nil), byEvap[id]...)
		sort.SliceStable(evs, func(i, j int) bool {
			if evs[i].At.Equal(evs[j].At) {
				// deterministic tie: starts before ends, earlier ingest first
				if evs[i].Starting != evs[j].Starting {
					return evs[i].Starting
				}
				return evs[i].IngestedAt.Before(evs[j].IngestedAt)
			}
			return evs[i].At.Before(evs[j].At)
		})
		ev, known := snap.Evaporators[id]
		zone := ""
		var cells []string
		if known {
			zone = ev.ZoneCode
			cells = append([]string(nil), ev.Cells...)
		}

		var start *domain.DefrostEvent
		emit := func(end *domain.DefrostEvent) {
			w := DefrostWindow{EvapID: id, ZoneCode: zone, Start: start.At, Cells: cells}
			// Lateness belongs to THIS cycle only, so a late (or retried) end
			// of an earlier cycle cannot mark the following one.
			late := lagOf(start, snap.At) > p.DefrostStateLate
			if end == nil {
				w.End = time.Time{}
				w.RiseUntil = snap.At
				w.Ongoing = true
			} else {
				w.End = end.At
				w.RiseUntil = end.At.Add(p.DefrostRecovery)
				late = late || lagOf(end, snap.At) > p.DefrostStateLate
			}
			w.Late = late
			out = append(out, w)
		}
		for i := range evs {
			e := &evs[i]
			if e.Starting {
				if start == nil {
					start = e
				}
				// repeated START before an END: keep the earliest, drop the rest
				continue
			}
			// END state
			if start == nil {
				continue // unmatched end: cannot reconstruct a window
			}
			emit(e)
			start = nil
		}
		if start != nil {
			emit(nil)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].EvapID != out[j].EvapID {
			return out[i].EvapID < out[j].EvapID
		}
		return out[i].Start.Before(out[j].Start)
	})
	return out
}

// lagOf is how long after its device time a state report was ingested. A
// zero ingest time means "not known" and contributes no lag.
func lagOf(e *domain.DefrostEvent, now time.Time) time.Duration {
	if e.IngestedAt.IsZero() {
		return 0
	}
	d := e.IngestedAt.Sub(e.At)
	if d < 0 {
		d = 0
	}
	return d
}

// nearEdge reports whether t is within margin (inclusive) of either edge of
// the rise window.
func nearEdge(w DefrostWindow, t time.Time, margin time.Duration) bool {
	for _, edge := range []time.Time{w.Start, w.RiseUntil} {
		d := t.Sub(edge)
		if d < 0 {
			d = -d
		}
		if d <= margin {
			return true
		}
	}
	return false
}

// CoreDefrostClass separates a manual core measurement from the routine
// temperature record:
//   - "in"      : inside a rise window and comfortably away from its edges;
//   - "cross"   : within CoreDefrostMargin of a window edge (inside or out),
//     or inside one window while skimming another — attribution to routine
//     vs defrost-affected air is ambiguous, so the point is kept apart and
//     never auto-classified;
//   - "routine": no window nearby.
//
// Classification is annotation only; it never concludes a quality change.
func CoreDefrostClass(snap *domain.Snapshot, ws []DefrostWindow, cell string, at time.Time, margin time.Duration) (string, []DefrostWindow) {
	hit := WindowsForCell(snap, ws, cell)
	if len(hit) == 0 {
		return "routine", nil
	}
	inside := 0
	for _, w := range hit {
		r := w.RiseInterval()
		if !at.Before(r.From) && at.Before(r.To) {
			inside++
		}
		if nearEdge(w, at, margin) {
			return "cross", hit
		}
	}
	if inside > 0 {
		return "in", hit
	}
	// Outside every window and not within margin of an edge: routine record.
	return "routine", nil
}

// overlapWindow returns the intersection of interval iv with a rise window.
func overlapWindow(iv Interval, w DefrostWindow) (time.Time, time.Time, bool) {
	return overlap(iv, w.RiseInterval())
}

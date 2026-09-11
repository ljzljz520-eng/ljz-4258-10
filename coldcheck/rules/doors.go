package rules

import (
	"sort"
	"time"

	"coldcheck/domain"
)

// DebounceDoorEvents turns raw magnetic-contact telemetry into trustworthy
// open intervals. It handles contact bounce:
//   - open/closed/open flickers inside BounceWindow / MinClosedPulse merge
//     into a single open interval (门磁反跳不得被记成多次开门);
//   - an isolated open pulse shorter than MinOpenPulse is noise and dropped.
//
// Events per door are sorted; contact starts assumed closed.
func DebounceDoorEvents(raw []domain.RawDoorEvent, p Params, now time.Time) []domain.DoorEvent {
	byDoor := map[string][]domain.RawDoorEvent{}
	for _, e := range raw {
		byDoor[e.DoorID] = append(byDoor[e.DoorID], e)
	}
	var out []domain.DoorEvent
	ids := make([]string, 0, len(byDoor))
	for id := range byDoor {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		evs := byDoor[id]
		sort.Slice(evs, func(i, j int) bool { return evs[i].At.Before(evs[j].At) })

		type iv struct{ s, e time.Time }
		var ivs []iv
		var openAt time.Time
		open := false
		for _, e := range evs {
			switch {
			case e.IsOpen && !open:
				open, openAt = true, e.At
			case !e.IsOpen && open:
				ivs = append(ivs, iv{openAt, e.At})
				open = false
			}
		}
		if open {
			ivs = append(ivs, iv{openAt, time.Time{}})
		}

		// Merge intervals separated by a closed flicker shorter than the
		// bounce/closed-pulse threshold. Repeat until stable so chains merge.
		threshold := p.MinClosedPulse
		if p.BounceWindow > threshold {
			threshold = p.BounceWindow
		}
		for changed := true; changed; {
			changed = false
			for i := 0; i < len(ivs)-1; i++ {
				gap := ivs[i+1].s.Sub(ivs[i].e)
				if gap >= 0 && gap < threshold {
					ivs[i].e = ivs[i+1].e
					ivs = append(ivs[:i+1], ivs[i+2:]...)
					changed = true
					break
				}
			}
		}

		for _, v := range ivs {
			if !v.e.IsZero() && v.e.Sub(v.s) < p.MinOpenPulse {
				continue // isolated micro-pulse: contact noise
			}
			de := domain.DoorEvent{DoorID: id, OpenedAt: v.s, ClosedAt: v.e}
			if v.e.IsZero() {
				de.StillOpen = true
				de.ClosedAt = now
			}
			out = append(out, de)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].DoorID == out[j].DoorID {
			return out[i].OpenedAt.Before(out[j].OpenedAt)
		}
		return out[i].DoorID < out[j].DoorID
	})
	return out
}

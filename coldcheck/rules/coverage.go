package rules

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"coldcheck/domain"
)

type LocalDeviation struct {
	CellCode string
	ZoneCode string
	At       time.Time
	LocalC   float64
	MeanC    float64
	DeltaC   float64
	NearDoor bool
	NearEvap bool
	// DefrostEvap non-empty means the deviation bucket overlaps a defrost
	// rise window: the local mean must not be quoted as routine.
	DefrostEvap []string
}

type Obstruction struct {
	NodeID   string
	CellCode string
	RSSI     int
	Baseline int
	RangeC   float64
}

func occupiedNow(snap *domain.Snapshot, cell string) bool {
	for _, o := range snap.Occupancies {
		if o.CellCode == cell && !o.From.After(snap.At) && o.To.After(snap.At) {
			return true
		}
	}
	return false
}

func frozenCell(snap *domain.Snapshot, cell string) bool {
	for _, pl := range snap.Plans {
		if !pl.ActiveAt(snap.At) {
			continue
		}
		for _, fc := range pl.Cells {
			if fc.CellCode == cell {
				return true
			}
		}
	}
	return false
}

func dist(a, b *domain.Cell) float64 {
	dx, dy := a.X-b.X, a.Y-b.Y
	return math.Sqrt(dx*dx + dy*dy)
}

// SpatialGaps reports occupied/frozen cells that have no usable air node.
// A frozen cell needs its own node; an ordinary occupied cell may be covered
// by a node in a same-layer adjacent cell within NeighbourRadiusM.
func SpatialGaps(snap *domain.Snapshot, p Params) []string {
	var gaps []string
	codes := make([]string, 0)
	for code, c := range snap.Cells {
		if !c.Active {
			continue
		}
		if occupiedNow(snap, code) || frozenCell(snap, code) {
			codes = append(codes, code)
		}
	}
	sort.Strings(codes)
	for _, code := range codes {
		c := snap.Cells[code]
		direct := false
		adjacent := false
		for _, n := range snap.Nodes {
			if !n.Active || n.Layer != c.Layer {
				continue
			}
			nc, ok := snap.Cells[n.CellCode]
			if !ok || nc.ZoneCode != c.ZoneCode {
				continue
			}
			if n.CellCode == code {
				direct = true
				break
			}
			if dist(nc, c) <= p.NeighbourRadiusM {
				adjacent = true
			}
		}
		if !direct && (frozenCell(snap, code) || !adjacent) {
			gaps = append(gaps, code)
		}
	}
	return gaps
}

// OfflineNodes returns active nodes whose most recent valid uplink is older
// than their configured deadline (or the default offline window). Uplinks
// inside a maintenance interval do not count, and a node currently under an
// open maintenance interval is expected to be silent (calibration/swap), so
// it is neither offline nor an evidence gap.
func OfflineNodes(snap *domain.Snapshot, p Params) []string {
	last := map[string]time.Time{}
	for _, r := range snap.Air {
		if snap.InMaintenance(r.NodeID, r.At) {
			continue // readings while in maintenance do not count
		}
		if r.At.After(last[r.NodeID]) {
			last[r.NodeID] = r.At
		}
	}
	ids := make([]string, 0)
	for id := range snap.Nodes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var out []string
	for _, id := range ids {
		n := snap.Nodes[id]
		if !n.Active {
			continue
		}
		if snap.NodeUnderMaintenance(id) {
			continue // operator-acknowledged silence, not a node fault
		}
		win := n.Deadline
		if win == 0 {
			win = p.OfflineWindow
		}
		if t, ok := last[id]; !ok || snap.At.Sub(t) > win {
			out = append(out, id)
		}
	}
	return out
}

// OccludedNodes detects an air node that a carton/pallet may be covering:
// gateway RSSI collapses relative to its free-air baseline while its
// temperature series goes suspiciously flat. Either signal alone is weak
// (a node can be cold-stable), both together justify a physical check.
// Maintenance intervals are excluded: a node on the bench must not look
// occluded, and the check is skipped while maintenance is still open.
func OccludedNodes(snap *domain.Snapshot, p Params) []Obstruction {
	start := snap.At.Add(-p.StagnationWindow)
	type st struct {
		min, max float64
		rssiSum  int
		rssiN    int
		have     bool
	}
	m := map[string]*st{}
	for _, r := range snap.Air {
		if r.At.Before(start) || r.At.After(snap.At) {
			continue
		}
		if snap.InMaintenance(r.NodeID, r.At) {
			continue
		}
		s := m[r.NodeID]
		if s == nil {
			s = &st{min: r.C, max: r.C}
			m[r.NodeID] = s
		}
		s.have = true
		if r.C < s.min {
			s.min = r.C
		}
		if r.C > s.max {
			s.max = r.C
		}
		s.rssiSum += r.RSSI
		s.rssiN++
	}
	ids := make([]string, 0)
	for id := range m {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var out []Obstruction
	for _, id := range ids {
		n, ok := snap.Nodes[id]
		if !ok || !n.Active || n.BaselineRSSI == 0 {
			continue
		}
		if snap.NodeUnderMaintenance(id) {
			continue
		}
		s := m[id]
		if !s.have || s.rssiN == 0 {
			continue
		}
		avgRSSI := s.rssiSum / s.rssiN
		drop := n.BaselineRSSI - avgRSSI
		if drop >= p.RSSICoverDropDB && s.max-s.min <= p.StagnationC {
			out = append(out, Obstruction{
				NodeID: id, CellCode: n.CellCode, RSSI: avgRSSI,
				Baseline: n.BaselineRSSI, RangeC: s.max - s.min,
			})
		}
	}
	return out
}

// LocalDeviations compares each cell's resampled air temperature against the
// zone/layer mean for the same bucket. A cell near an evaporator (often
// colder) or a doorway (often warmer) is still reported with its location
// flags, so local effects are never hidden by the warehouse mean.
func LocalDeviations(series map[string]*AirSeries, p Params) []LocalDeviation {
	type key struct {
		zone string
		at   time.Time
	}
	sum := map[key]float64{}
	cnt := map[key]int{}
	for _, s := range series {
		for _, pt := range s.Points {
			k := key{s.ZoneCode, pt.At}
			sum[k] += pt.C
			cnt[k]++
		}
	}
	var out []LocalDeviation
	cells := make([]string, 0, len(series))
	for c := range series {
		cells = append(cells, c)
	}
	sort.Strings(cells)
	seen := map[string]bool{}
	for _, c := range cells {
		s := series[c]
		// Report one strongest deviation per cell to avoid duplicate alerts.
		var worst *LocalDeviation
		for _, pt := range s.Points {
			mean := sum[key{s.ZoneCode, pt.At}] / float64(cnt[key{s.ZoneCode, pt.At}])
			d := pt.C - mean
			if math.Abs(d) >= p.LocalDeltaC {
				ld := LocalDeviation{
					CellCode: c, ZoneCode: s.ZoneCode, At: pt.At,
					LocalC: pt.C, MeanC: mean, DeltaC: d,
					NearDoor: pt.NearDoor, NearEvap: pt.NearEvap,
					DefrostEvap: append([]string(nil), pt.DefrostEvap...),
				}
				if worst == nil || math.Abs(ld.DeltaC) > math.Abs(worst.DeltaC) {
					w := ld
					worst = &w
				}
			}
		}
		if worst != nil && !seen[worst.CellCode] {
			seen[worst.CellCode] = true
			out = append(out, *worst)
		}
	}
	return out
}

func (l LocalDeviation) String() string {
	where := ""
	if l.NearDoor && l.NearEvap {
		where = "（门口且靠近蒸发器）"
	} else if l.NearDoor {
		where = "（靠近门口）"
	} else if l.NearEvap {
		where = "（靠近蒸发器）"
	}
	sign := "偏暖"
	if l.DeltaC < 0 {
		sign = "偏冷"
	}
	msg := fmt.Sprintf("格位 %s%s 空气 %.2f°C，%s于温区均值 %.2f°C（Δ%+.2f°C）",
		l.CellCode, where, l.LocalC, sign, l.MeanC, l.DeltaC)
	if len(l.DefrostEvap) > 0 {
		msg += fmt.Sprintf("；该时刻处于蒸发器 %s 除霜温升窗口，不得与常规时段均值混读", strings.Join(l.DefrostEvap, "、"))
	}
	return msg
}

package rules

import (
	"math/rand"
	"time"

	"coldcheck/domain"
)

// RandomFreeze builds a quality freeze: random active cells and random
// measurement instants inside [start,end). Once persisted with Locked=true
// neither side can move the sample.
func RandomFreeze(snap *domain.Snapshot, id string, start, end time.Time, cellsN, pointsN int, rng *rand.Rand) domain.FreezePlan {
	var codes []string
	for code, c := range snap.Cells {
		if c.Active {
			codes = append(codes, code)
		}
	}
	// deterministic shuffle independent of map iteration
	for i := len(codes) - 1; i > 0; i-- {
		j := rng.Intn(i + 1)
		codes[i], codes[j] = codes[j], codes[i]
	}
	if cellsN > len(codes) {
		cellsN = len(codes)
	}
	pl := domain.FreezePlan{ID: id, CreatedAt: snap.At, Start: start, End: end, Locked: true}
	for _, code := range codes[:cellsN] {
		pl.Cells = append(pl.Cells, domain.FrozenCell{CellCode: code})
	}
	if pointsN > 0 {
		span := end.Sub(start)
		for i := 0; i < pointsN; i++ {
			off := time.Duration(rng.Int63n(int64(span)))
			pl.Points = append(pl.Points, domain.FrozenTimePoint{At: start.Add(off).Truncate(time.Minute)})
		}
	}
	return pl
}

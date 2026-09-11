package web

import (
	"encoding/json"
	"net/http"
)

type featureCollection struct {
	Type     string           `json:"type"`
	Features []map[string]any `json:"features"`
}

// apiGeoJSON emits layered store geometry for MapLibre. Coordinates are the
// local metre grid wrapped in GeoJSON [x,y]; the front-end defines the CRS.
// Each cell carries zone, layer, near-evap/door flags and current alerts so
// local anomalies are drawn at the cell rather than as a warehouse mean.
func (a *App) apiGeoJSON(w http.ResponseWriter, r *http.Request) {
	b, err := a.evaluate(r.Context())
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	snap := b.Snap
	cellAlert := map[string]string{}
	for _, al := range b.Res.Alerts {
		if al.CellCode == "" {
			continue
		}
		if cellAlert[al.CellCode] != "fail" {
			cellAlert[al.CellCode] = al.Severity
		}
	}
	fc := featureCollection{Type: "FeatureCollection"}

	occupants := map[string][]string{}
	for _, o := range snap.Occupancies {
		if !o.From.After(b.Res.At) && o.To.After(b.Res.At) {
			occupants[o.CellCode] = append(occupants[o.CellCode], o.BatchID)
		}
	}

	// zone polygons
	for _, z := range snap.Zones {
		if len(z.Polygon) < 3 {
			continue
		}
		ring := make([][2]float64, 0, len(z.Polygon)+1)
		ring = append(ring, z.Polygon...)
		ring = append(ring, z.Polygon[0])
		fc.Features = append(fc.Features, map[string]any{
			"type":     "Feature",
			"geometry": map[string]any{"type": "Polygon", "coordinates": [][][2]float64{ring}},
			"properties": map[string]any{
				"kind": "zone", "code": z.Code, "name": z.Name, "layer": z.Layer,
				"min": z.MinC, "max": z.MaxC,
			},
		})
	}
	for code, c := range snap.Cells {
		if !c.Active {
			continue
		}
		zoneName := ""
		if z := snap.Zones[c.ZoneCode]; z != nil {
			zoneName = z.Name
		}
		fc.Features = append(fc.Features, map[string]any{
			"type": "Feature",
			"geometry": map[string]any{
				"type":        "Polygon",
				"coordinates": [][][2]float64{cellSquare(c.X, c.Y, 0.9)},
			},
			"properties": map[string]any{
				"kind": "cell", "code": code, "zone": c.ZoneCode, "zoneName": zoneName,
				"layer": c.Layer, "nearEvap": c.NearEvap, "nearDoor": c.NearDoor,
				"status": cellAlert[code], "batches": occupants[code],
			},
		})
	}
	for _, n := range snap.Nodes {
		c := snap.Cells[n.CellCode]
		if c == nil {
			continue
		}
		status := cellAlert[n.CellCode]
		offline := false
		for _, id := range b.Res.Offline {
			if id == n.ID {
				offline = true
			}
		}
		if offline {
			status = "offline"
		}
		fc.Features = append(fc.Features, map[string]any{
			"type":     "Feature",
			"geometry": map[string]any{"type": "Point", "coordinates": [2]float64{c.X + 0.3, c.Y + 0.3}},
			"properties": map[string]any{
				"kind": "node", "id": n.ID, "cell": n.CellCode, "layer": n.Layer, "status": status,
			},
		})
	}
	w.Header().Set("Content-Type", "application/geo+json")
	json.NewEncoder(w).Encode(fc)
}

func cellSquare(x, y, size float64) [][2]float64 {
	h := size / 2
	return [][2]float64{
		{x - h, y - h}, {x + h, y - h}, {x + h, y + h}, {x - h, y + h}, {x - h, y - h},
	}
}

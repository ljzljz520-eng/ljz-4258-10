// Package web exposes the verification UI and ingest endpoints.
// It is deliberately read-only with respect to refrigeration equipment:
// nothing here sends downlinks or control commands.
package web

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"strconv"
	"time"

	"coldcheck/domain"
	"coldcheck/lora"
	"coldcheck/rules"
	"coldcheck/store"
)

type App struct {
	Store  store.Store
	Params rules.Params
	Window time.Duration
	tmpl   *template.Template
}

func NewApp(st store.Store, p rules.Params, window time.Duration) (*App, error) {
	tmpl, err := template.New("").Funcs(template.FuncMap{
		"severityClass": func(s string) string {
			switch s {
			case "fail":
				return "fail"
			case "warn":
				return "warn"
			default:
				return "info"
			}
		},
		"fmtTime": func(t time.Time) string { return t.Local().Format("01-02 15:04:05") },
	}).ParseFS(templates, "templates/*.html")
	if err != nil {
		return nil, err
	}
	return &App{Store: st, Params: p, Window: window, tmpl: tmpl}, nil
}

type evalBundle struct {
	Snap *domain.Snapshot
	Res  *rules.Result
}

func (a *App) evaluate(ctx context.Context) (*evalBundle, error) {
	snap, err := a.Store.Load(ctx, time.Now(), a.Window)
	if err != nil {
		return nil, err
	}
	return &evalBundle{Snap: snap, Res: rules.Evaluate(snap, a.Params)}, nil
}

func (a *App) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", a.dashboard)
	mux.HandleFunc("GET /entry", a.entry)
	mux.HandleFunc("GET /map", a.mapPage)
	mux.HandleFunc("GET /partials/alerts", a.alertsPartial)
	mux.HandleFunc("GET /api/alerts", a.apiAlerts)
	mux.HandleFunc("GET /api/geojson", a.apiGeoJSON)
	mux.HandleFunc("POST /lorawan/uplink", a.loraUplink)
	mux.HandleFunc("POST /scan", a.postScan)
	mux.HandleFunc("POST /door", a.postDoor)
	mux.HandleFunc("POST /core", a.postCore)
	mux.HandleFunc("POST /freeze", a.postFreeze)
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(staticFS))))
	return mux
}

func (a *App) render(w http.ResponseWriter, name string, code int, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(code)
	if err := a.tmpl.ExecuteTemplate(w, name, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (a *App) dashboard(w http.ResponseWriter, r *http.Request) {
	b, err := a.evaluate(r.Context())
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	a.render(w, "dashboard.html", 200, map[string]any{
		"Bundle": b, "Now": time.Now(), "Window": a.Window,
	})
}

func (a *App) entry(w http.ResponseWriter, r *http.Request) {
	b, err := a.evaluate(r.Context())
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	a.render(w, "entry.html", 200, map[string]any{"Bundle": b})
}

func (a *App) mapPage(w http.ResponseWriter, r *http.Request) {
	b, err := a.evaluate(r.Context())
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	a.render(w, "map.html", 200, map[string]any{"Bundle": b})
}

func (a *App) alertsPartial(w http.ResponseWriter, r *http.Request) {
	b, err := a.evaluate(r.Context())
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	a.render(w, "alerts.html", 200, map[string]any{"Alerts": b.Res.Alerts})
}

func (a *App) apiAlerts(w http.ResponseWriter, r *http.Request) {
	b, err := a.evaluate(r.Context())
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	type out struct {
		At     time.Time      `json:"at"`
		Alerts []domain.Alert `json:"alerts"`
		Counts map[string]int `json:"counts"`
	}
	o := out{At: b.Res.At, Alerts: b.Res.Alerts, Counts: map[string]int{}}
	for _, al := range b.Res.Alerts {
		o.Counts[al.Severity]++
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(o)
}

// loraUplink accepts a Network Server webhook and stores the air reading.
func (a *App) loraUplink(w http.ResponseWriter, r *http.Request) {
	reading, err := lora.Parse(readBody(r))
	if err != nil {
		http.Error(w, "bad uplink: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := a.Store.AddAirReading(r.Context(), reading); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"stored": true, "node": reading.NodeID, "c": reading.C})
}

// postScan records a warehouse-gun relocation scan. It also writes an
// occupancy interval ending any previous same-batch occupancy is the
// caller's responsibility at import; here the new interval is marked scanned.
func (a *App) postScan(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	now := time.Now()
	m := domain.MoveScan{
		ScanID:   defaultID(r, "scan_id", "S"),
		BatchID:  r.FormValue("batch_id"),
		FromCell: r.FormValue("from_cell"),
		ToCell:   r.FormValue("to_cell"),
		At:       parseTime(r.FormValue("at"), now),
	}
	if m.BatchID == "" || m.ToCell == "" {
		http.Error(w, "batch_id and to_cell required", 400)
		return
	}
	ctx := r.Context()
	if err := a.Store.AddMoveScan(ctx, m); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	if err := a.Store.AddOccupancy(ctx, domain.Occupancy{
		BatchID: m.BatchID, CellCode: m.ToCell, From: m.At, To: now.Add(6 * time.Hour),
		MoveScan: true, MoveScanAt: m.At,
	}); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	_ = a.Store.AddPresenceEvent(ctx, domain.PresenceEvent{
		Kind: "arrive", BatchID: m.BatchID, CellCode: m.ToCell, At: m.At,
	})
	a.render(w, "ok.html", 200, map[string]any{"Msg": fmt.Sprintf("已记录移位扫描 %s：%s → %s", m.ScanID, m.FromCell, m.ToCell)})
}

func (a *App) postDoor(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	e := domain.RawDoorEvent{
		DoorID: r.FormValue("door_id"),
		At:     parseTime(r.FormValue("at"), time.Now()),
		IsOpen: r.FormValue("state") == "open",
		RSSI:   atoiOr(r.FormValue("rssi"), -60),
	}
	if e.DoorID == "" {
		http.Error(w, "door_id required", 400)
		return
	}
	if err := a.Store.AddRawDoorEvent(r.Context(), e); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	state := "关闭"
	if e.IsOpen {
		state = "开启"
	}
	a.render(w, "ok.html", 200, map[string]any{"Msg": fmt.Sprintf("已记录门 %s %s（原始事件，参与去抖）", e.DoorID, state)})
}

func (a *App) postCore(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	now := time.Now()
	depth, _ := strconv.ParseFloat(r.FormValue("probe_depth_mm"), 64)
	required, _ := strconv.ParseFloat(r.FormValue("required_depth_mm"), 64)
	c, err1 := strconv.ParseFloat(r.FormValue("c"), 64)
	if err1 != nil {
		http.Error(w, "c must be a number", 400)
		return
	}
	m := domain.CoreMeasurement{
		ID: defaultID(r, "measurement_id", "CM"), BatchID: r.FormValue("batch_id"),
		CellCode: r.FormValue("cell_code"), PlanID: r.FormValue("plan_id"),
		At: parseTime(r.FormValue("at"), now), C: c,
		ProbeDepthMM: depth, RequiredDepthMM: required,
		ReachedCenter: r.FormValue("reached_center") == "on" || depth+a.Params.ProbeToleranceMM >= required && required > 0,
		Operator:      r.FormValue("operator"),
	}
	if m.BatchID == "" || m.CellCode == "" {
		http.Error(w, "batch_id and cell_code required", 400)
		return
	}
	if err := a.Store.AddCoreMeasurement(r.Context(), m); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	note := ""
	if !m.ReachedCenter {
		note = "；探针未达中心，系统将标记该读数不可作为芯温结论"
	}
	a.render(w, "ok.html", 200, map[string]any{"Msg": fmt.Sprintf("已记录芯温 %s = %.2f°C%s", m.ID, m.C, note)})
}

func (a *App) postFreeze(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	cellsN, _ := strconv.Atoi(r.FormValue("cells"))
	pointsN, _ := strconv.Atoi(r.FormValue("points"))
	hours, _ := strconv.Atoi(r.FormValue("hours"))
	if cellsN <= 0 {
		cellsN = 3
	}
	if pointsN <= 0 {
		pointsN = 2
	}
	if hours <= 0 {
		hours = 4
	}
	snap, err := a.Store.Load(r.Context(), time.Now(), a.Window)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	plan := rules.RandomFreeze(snap, defaultID(r, "plan_id", "FZ"),
		time.Now(), time.Now().Add(time.Duration(hours)*time.Hour),
		cellsN, pointsN, newLockedRNG())
	if err := a.Store.SaveFreezePlan(r.Context(), plan); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	a.render(w, "ok.html", 200, map[string]any{"Msg": fmt.Sprintf("已冻结质量计划 %s：%d 个随机格位 × %d 个随机时点", plan.ID, len(plan.Cells), len(plan.Points))})
}

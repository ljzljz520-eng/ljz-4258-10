package web_test

import (
	"encoding/base64"
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"coldcheck/rules"
	"coldcheck/seed"
	"coldcheck/store"
	"coldcheck/web"
)

func newSrv(t *testing.T) *httptest.Server {
	t.Helper()
	sc := seed.Build(time.Now())
	app, err := web.NewApp(sc.Memory, rules.DefaultParams(), 4*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return httptest.NewServer(app.Routes())
}

// newSrvAt fixes the whole app (including the store's seeded scenario) at a
// single clock, mirroring demo mode's injected clock.
func newSrvAt(t *testing.T, at time.Time) *httptest.Server {
	t.Helper()
	sc := seed.Build(at)
	app, err := web.NewApp(sc.Memory, rules.DefaultParams(), 4*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	app.WithClock(func() time.Time { return at })
	return httptest.NewServer(app.Routes())
}

func TestAlertsAndHTMXEntry(t *testing.T) {
	srv := newSrv(t)
	defer srv.Close()

	// API surfaces the five evidence scenarios.
	resp, err := srv.Client().Get(srv.URL + "/api/alerts")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var payload struct {
		Alerts []struct {
			Code string `json:"code"`
		} `json:"alerts"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		"DOOR_STILL_OPEN": false, "NODE_OCCLUDED": false, "MOVE_UNSCANNED": false,
		"PROBE_NOT_CENTER": false, "BATCH_CROSS_ZONE": false, "SPATIAL_GAP": false,
	}
	for _, a := range payload.Alerts {
		if _, ok := want[a.Code]; ok {
			want[a.Code] = true
		}
	}
	for code, found := range want {
		if !found {
			t.Errorf("expected alert code %s in API", code)
		}
	}

	// Shallow-probe core entry via HTMX form.
	form := url.Values{
		"batch_id": {"LOT-1"}, "cell_code": {"A-01-01"}, "c": {"5.2"},
		"probe_depth_mm": {"20"}, "required_depth_mm": {"120"}, "operator": {"t"},
	}
	r, err := srv.Client().PostForm(srv.URL+"/core", form)
	if err != nil {
		t.Fatal(err)
	}
	body := make([]byte, 4096)
	n, _ := r.Body.Read(body)
	r.Body.Close()
	if !strings.Contains(string(body[:n]), "探针未达中心") {
		t.Fatalf("HTMX response should warn about probe depth: %q", body[:n])
	}
}

func TestLoRaIngest(t *testing.T) {
	mem := store.NewMemory()
	app, err := web.NewApp(mem, rules.DefaultParams(), 4*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(app.Routes())
	defer srv.Close()

	v := uint16(340) // 3.40C
	bin := []byte{1, byte(v >> 8), byte(v), 0x0c, 0xe4}
	body, _ := json.Marshal(map[string]any{
		"devEUI": "n-new", "frmpayload": base64.StdEncoding.EncodeToString(bin),
		"time": time.Now().UTC().Format(time.RFC3339), "rssi": -65,
	})
	resp, err := srv.Client().Post(srv.URL+"/lorawan/uplink", "application/json",
		strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	if out["stored"] != true || out["c"].(float64) != 3.4 {
		t.Fatalf("unexpected ingest response %v", out)
	}
}

// A handheld relocation submitted under the demo's fixed clock must appear as
// a current occupant of the destination cell in the map GeoJSON.
func TestScanAppearsOnMapWithFixedClock(t *testing.T) {
	at := time.Date(2026, 9, 11, 10, 0, 0, 0, time.UTC)
	srv := newSrvAt(t, at)
	defer srv.Close()

	form := url.Values{
		"batch_id": {"LOT-4"}, "from_cell": {"A-03-02"}, "to_cell": {"A-02-02"},
	}
	resp, err := srv.Client().PostForm(srv.URL+"/scan", form)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("scan status = %d", resp.StatusCode)
	}

	gj, err := srv.Client().Get(srv.URL + "/api/geojson")
	if err != nil {
		t.Fatal(err)
	}
	defer gj.Body.Close()
	var fc struct {
		Features []struct {
			Properties map[string]any `json:"properties"`
		} `json:"features"`
	}
	if err := json.NewDecoder(gj.Body).Decode(&fc); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, f := range fc.Features {
		if f.Properties["code"] != "A-02-02" || f.Properties["kind"] != "cell" {
			continue
		}
		batches, _ := f.Properties["batches"].([]any)
		for _, b := range batches {
			if b == "LOT-4" {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("new scan destination A-02-02 does not show LOT-4 on the map")
	}
}

// Non-numeric probe depths must be rejected (400) instead of silently stored
// as 0 mm with a success page.
func TestCoreRejectsNonNumericDepth(t *testing.T) {
	srv := newSrv(t)
	defer srv.Close()

	for _, field := range []string{"probe_depth_mm", "required_depth_mm"} {
		v := url.Values{
			"batch_id": {"LOT-1"}, "cell_code": {"A-01-01"}, "c": {"3.1"},
			"probe_depth_mm": {"100"}, "required_depth_mm": {"120"},
		}
		v.Set(field, "abc")
		resp, err := srv.Client().PostForm(srv.URL+"/core", v)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 400 {
			t.Fatalf("non-numeric %s: status = %d, want 400", field, resp.StatusCode)
		}
	}

	// Negative depth is also invalid.
	v := url.Values{
		"batch_id": {"LOT-1"}, "cell_code": {"A-01-01"}, "c": {"3.1"},
		"probe_depth_mm": {"-5"}, "required_depth_mm": {"120"},
	}
	resp, err := srv.Client().PostForm(srv.URL+"/core", v)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("negative depth: status = %d, want 400", resp.StatusCode)
	}
}

// Implausible decoder-object temperatures must be rejected at the webhook.
func TestLoraRejectsImplausibleObjectTemp(t *testing.T) {
	mem := store.NewMemory()
	app, err := web.NewApp(mem, rules.DefaultParams(), 4*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(app.Routes())
	defer srv.Close()

	body := strings.NewReader(`{"devEUI":"n-x","object":{"temperature":327.67}}`)
	resp, err := srv.Client().Post(srv.URL+"/lorawan/uplink", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("status = %d, want 400 for implausible decoder temp", resp.StatusCode)
	}
}

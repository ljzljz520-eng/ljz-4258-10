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

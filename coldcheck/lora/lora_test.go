package lora_test

import (
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"

	"coldcheck/lora"
)

func payload(temp int16) string {
	b := []byte{1, 0, 0, 0x0c, 0xe4} // class 1, temp, battery 3300mV
	b[1] = byte(uint16(temp) >> 8)
	b[2] = byte(uint16(temp))
	return base64.StdEncoding.EncodeToString(b)
}

func TestParseBinaryUplink(t *testing.T) {
	body, _ := json.Marshal(map[string]any{
		"devEUI":     "AABBCCDDEEFF0011",
		"fPort":      2,
		"frmpayload": payload(235), // 2.35C
		"time":       "2026-09-11T09:00:00Z",
		"rssi":       -71,
	})
	r, err := lora.Parse(body)
	if err != nil {
		t.Fatal(err)
	}
	if r.NodeID != "aabbccddeeff0011" {
		t.Fatalf("deveui not normalized: %s", r.NodeID)
	}
	if r.C < 2.34 || r.C > 2.36 {
		t.Fatalf("temp = %.2f, want 2.35", r.C)
	}
	if r.RSSI != -71 {
		t.Fatalf("rssi = %d", r.RSSI)
	}
	want := time.Date(2026, 9, 11, 9, 0, 0, 0, time.UTC)
	if !r.At.Equal(want) {
		t.Fatalf("at = %v", r.At)
	}
}

func TestParseObjectUplinkAndRejectBad(t *testing.T) {
	body, _ := json.Marshal(map[string]any{"devEUI": "x", "object": map[string]any{"temperature": -18.4}})
	r, err := lora.Parse(body)
	if err != nil || r.C != -18.4 {
		t.Fatalf("object temp parse: %+v err=%v", r, err)
	}
	if _, err := lora.Parse([]byte(`{"devEUI":""}`)); err == nil {
		t.Fatal("missing deveui must error")
	}
	body, _ = json.Marshal(map[string]any{"devEUI": "y", "frmpayload": payload(9000)})
	if _, err := lora.Parse(body); err == nil {
		t.Fatal("90C must be rejected as implausible")
	}
}

func TestParseObjectRejectsImplausibleTemperature(t *testing.T) {
	// A decoder object must not bypass the physical plausibility envelope:
	// error sentinels / out-of-range values come back as numbers too.
	for _, bad := range []float64{-999, 90.01, 125, -60.01} {
		body, _ := json.Marshal(map[string]any{"devEUI": "z", "object": map[string]any{"tempC": bad}})
		if _, err := lora.Parse(body); err == nil {
			t.Fatalf("decoder temp %.2f must be rejected", bad)
		}
	}
	// Boundary values are accepted.
	for _, ok := range []float64{-60, -18.4, 0, 2.35, 80} {
		body, _ := json.Marshal(map[string]any{"devEUI": "z", "object": map[string]any{"temp_c": ok}})
		r, err := lora.Parse(body)
		if err != nil || r.C != ok {
			t.Fatalf("decoder temp %.2f must be accepted: r=%+v err=%v", ok, r, err)
		}
	}
}

func TestParseAtUsesInjectedClock(t *testing.T) {
	fixed := time.Date(2026, 9, 11, 8, 0, 0, 0, time.UTC)
	body, _ := json.Marshal(map[string]any{"devEUI": "x", "object": map[string]any{"temperature": 2.0}})
	r, err := lora.ParseAt(body, fixed)
	if err != nil {
		t.Fatal(err)
	}
	if !r.At.Equal(fixed) {
		t.Fatalf("at = %v, want injected %v", r.At, fixed)
	}
}

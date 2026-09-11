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

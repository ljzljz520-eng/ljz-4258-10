// Package lora ingests LoRaWAN gateway uplinks carrying air-node broadcasts.
// It is receive-only: no downlink is sent and the platform never controls
// refrigeration equipment.
package lora

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"coldcheck/domain"
)

// Uplink models the common fields of a LoRaWAN Network Server webhook
// (ChirpStack / generic). FRMPayload is base64 encoded sensor bytes:
//
//	byte 0:    device class/version
//	bytes 1-2: int16 BE temperature in 0.01 °C
//	bytes 3-4: uint16 battery mV (optional)
type Uplink struct {
	DevEUI     string         `json:"devEUI"`
	DeviceName string         `json:"deviceName"`
	GatewayID  string         `json:"gatewayID"`
	FPort      uint8          `json:"fPort"`
	FRMPayload string         `json:"frmpayload"`
	Time       string         `json:"time"`
	RSSI       *int           `json:"rssi"`
	Object     map[string]any `json:"object,omitempty"` // decoder may pre-parse
}

// Parse decodes an uplink body into an AirReading.
func Parse(body []byte) (domain.AirReading, error) {
	var u Uplink
	if err := json.Unmarshal(body, &u); err != nil {
		return domain.AirReading{}, fmt.Errorf("decode uplink json: %w", err)
	}
	return u.Reading(time.Now())
}

func (u Uplink) Reading(now time.Time) (domain.AirReading, error) {
	if strings.TrimSpace(u.DevEUI) == "" {
		return domain.AirReading{}, errors.New("uplink missing devEUI")
	}
	r := domain.AirReading{NodeID: strings.ToLower(u.DevEUI), At: now}
	if u.Time != "" {
		if t, err := time.Parse(time.RFC3339Nano, u.Time); err == nil {
			r.At = t
		} else if t, err := time.Parse(time.RFC3339, u.Time); err == nil {
			r.At = t
		}
	}
	if u.RSSI != nil {
		r.RSSI = *u.RSSI
	}
	if v, ok := numFromObject(u.Object); ok {
		r.C = v
		return r, nil
	}
	raw, err := base64.StdEncoding.DecodeString(u.FRMPayload)
	if err != nil {
		return r, fmt.Errorf("decode frm payload: %w", err)
	}
	if len(raw) < 3 {
		return r, fmt.Errorf("payload too short: %d bytes", len(raw))
	}
	hundredths := int16(binary.BigEndian.Uint16(raw[1:3]))
	c := float64(hundredths) / 100.0
	if math.IsNaN(c) || math.IsInf(c, 0) || c < -60 || c > 80 {
		return r, fmt.Errorf("implausible temperature %.2f", c)
	}
	r.C = c
	return r, nil
}

func numFromObject(o map[string]any) (float64, bool) {
	if o == nil {
		return 0, false
	}
	for _, k := range []string{"temperature", "tempC", "temp_c", "c"} {
		switch v := o[k].(type) {
		case float64:
			return v, true
		case int:
			return float64(v), true
		}
	}
	return 0, false
}

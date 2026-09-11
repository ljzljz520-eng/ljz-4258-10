package web

import (
	crand "crypto/rand"
	"encoding/hex"
	"io"
	"math/big"
	"math/rand"
	"net/http"
	"strconv"
	"time"
)

func readBody(r *http.Request) []byte {
	b, _ := io.ReadAll(r.Body)
	return b
}

func parseTime(s string, fallback time.Time) time.Time {
	if s == "" {
		return fallback
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return fallback
}

func atoiOr(s string, def int) int {
	if v, err := strconv.Atoi(s); err == nil {
		return v
	}
	return def
}

func defaultID(r *http.Request, field, prefix string) string {
	if v := r.FormValue(field); v != "" {
		return v
	}
	b := make([]byte, 4)
	_, _ = crand.Read(b)
	return prefix + "-" + hex.EncodeToString(b)
}

func newLockedRNG() *rand.Rand {
	seed, _ := crand.Int(crand.Reader, big.NewInt(1<<62))
	return rand.New(rand.NewSource(seed.Int64()))
}

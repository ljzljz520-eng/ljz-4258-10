package rules

import "time"

// Params holds tunable verification thresholds. They are deliberately
// explicit so "near evaporator" and "near door" effects can be judged
// against local nodes rather than a single warehouse average.
type Params struct {
	// Door contact debounce
	BounceWindow   time.Duration // opens/closers closer than this merge
	MinOpenPulse   time.Duration // shorter open intervals are bounce noise
	MinClosedPulse time.Duration // gaps shorter than this stay "open"

	// Air nodes
	OfflineWindow    time.Duration
	Bucket           time.Duration // air resampling bucket
	LocalDeltaC      float64       // local-node vs zone-mean warning delta
	RSSICoverDropDB  int           // RSSI under baseline by this => possible box cover
	StagnationWindow time.Duration // no meaningful temperature movement
	StagnationC      float64       // max movement that counts as stagnant

	// Spatial coverage
	NeighbourRadiusM float64

	// Exposure
	DefaultExposureLimit time.Duration
	ProbeToleranceMM     float64

	// Core temperature
	CoreBandMarginC float64
}

func DefaultParams() Params {
	return Params{
		BounceWindow:         3 * time.Second,
		MinOpenPulse:         5 * time.Second,
		MinClosedPulse:       15 * time.Second,
		OfflineWindow:        20 * time.Minute,
		Bucket:               5 * time.Minute,
		LocalDeltaC:          1.5,
		RSSICoverDropDB:      12,
		StagnationWindow:     90 * time.Minute,
		StagnationC:          0.2,
		NeighbourRadiusM:     1.6, // adjacent cell centre distance
		DefaultExposureLimit: 30 * time.Minute,
		ProbeToleranceMM:     5,
		CoreBandMarginC:      0.5,
	}
}

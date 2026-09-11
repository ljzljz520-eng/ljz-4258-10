package domain

import "time"

// Layout versions freeze the geometric definition of the store.
type LayoutVersion struct {
	ID        string
	Active    bool
	CreatedAt time.Time
	Note      string
}

type Zone struct {
	Code      string // e.g. CHILL-A, FREEZE-B
	Name      string
	Layer     int // floor level for the layered plan
	MinC      float64
	MaxC      float64
	TargetC   float64
	DairySegs []string     // product segment codes allowed here
	Polygon   [][2]float64 // x/y metres in the local store coordinate system
	// LayoutID binds the geometry to a frozen layout version; empty means
	// the zone belongs to the currently active layout.
	LayoutID string
}

type Cell struct {
	Code     string // 库区格位, e.g. A-03-12
	ZoneCode string
	Layer    int
	X, Y     float64
	// Neighbours are cell codes sharing a wall/aisle edge.
	Neighbours []string
	NearEvap   bool // close to an evaporator
	NearDoor   bool // close to a loading door
	Active     bool
	// LayoutID binds the cell geometry to a frozen layout version; empty
	// means the cell belongs to the currently active layout.
	LayoutID string
}

type Door struct {
	ID       string
	CellCode string
	Name     string
}

// RawDoorEvent is what the magnetic contact reports, including bounces.
type RawDoorEvent struct {
	DoorID string
	At     time.Time
	IsOpen bool
	RSSI   int
}

// DoorEvent is a debounced, trustworthy open or close transition.
type DoorEvent struct {
	DoorID    string
	OpenedAt  time.Time
	ClosedAt  time.Time // zero when still open
	StillOpen bool
}

type Node struct {
	ID       string // air node (LoRaWAN DevEUI)
	CellCode string
	Layer    int
	// BaselineRSSI is the gateway RSSI expected when the node antenna is free.
	BaselineRSSI int
	Deadline     time.Duration // offline after no uplink for this long
	Active       bool
}

type AirReading struct {
	NodeID string
	At     time.Time
	C      float64
	RSSI   int
}

type Batch struct {
	ID          string // 产品批
	Product     string
	SegCode     string // 产品段, e.g. FRESH-MILK, HARD-CHEESE
	Lot         string
	MinC        float64 // product-segment required band (from master data)
	MaxC        float64
	MaxExposure time.Duration // cumulative allowed out-of-band air exposure
}

// Occupancy says a batch physically occupied a cell during [From,To).
// MoveScan==true means the warehouse scan gun recorded the relocation;
// false means presence was inferred (e.g. a pallet appeared in a cell).
type Occupancy struct {
	BatchID    string
	CellCode   string
	From       time.Time
	To         time.Time
	MoveScan   bool
	MoveScanAt time.Time
}

type PresenceEvent struct {
	Kind     string // "arrive" or "leave"
	BatchID  string
	CellCode string
	At       time.Time
}

type MoveScan struct {
	ScanID   string
	BatchID  string
	FromCell string
	ToCell   string
	At       time.Time
}

type CoreMeasurement struct { // 人工抽测芯温
	ID              string
	BatchID         string
	CellCode        string
	PlanID          string
	At              time.Time
	C               float64
	ProbeDepthMM    float64 // insertion depth achieved
	RequiredDepthMM float64 // depth to geometric centre
	ReachedCenter   bool
	Operator        string
}

type FreezePlan struct {
	ID        string // quality freeze of random cells and measurement instants
	CreatedAt time.Time
	Start     time.Time
	End       time.Time
	Cells     []FrozenCell
	Points    []FrozenTimePoint
	Locked    bool
}

type FrozenCell struct {
	CellCode string
}

type FrozenTimePoint struct {
	At time.Time
}

type Alert struct {
	Code     string    `json:"code"`
	Severity string    `json:"severity"` // info, warn, fail
	Msg      string    `json:"msg"`
	CellCode string    `json:"cellCode,omitempty"`
	ZoneCode string    `json:"zoneCode,omitempty"`
	BatchID  string    `json:"batchId,omitempty"`
	NodeID   string    `json:"nodeId,omitempty"`
	DoorID   string    `json:"doorId,omitempty"`
	PlanID   string    `json:"planId,omitempty"`
	At       time.Time `json:"at"`
}

// Snapshot is everything the rule engine evaluates. Repositories
// assemble it from PostgreSQL (or from seeded demo data).
type Snapshot struct {
	At            time.Time
	Layout        *LayoutVersion
	Zones         map[string]*Zone
	Cells         map[string]*Cell
	Doors         map[string]*Door
	Nodes         map[string]*Node
	RawDoorEvents []RawDoorEvent
	DoorEvents    []DoorEvent // when pre-debounced
	Air           []AirReading
	Batches       map[string]*Batch
	Occupancies   []Occupancy
	MoveScans     []MoveScan
	Core          []CoreMeasurement
	Plans         []FreezePlan
	Events        []PresenceEvent
	Window        time.Duration // evaluation look-back window
}

// ActiveAt reports whether the quality freeze covers instant t.
func (p FreezePlan) ActiveAt(t time.Time) bool {
	return t.Before(p.End) && !t.Before(p.Start)
}

// Frozen reports whether the plan locks this cell and measurement instant.
func (p FreezePlan) Frozen(cell string, t time.Time) bool {
	if !p.Locked || !p.ActiveAt(t) {
		return false
	}
	cellOK := false
	for _, c := range p.Cells {
		if c.CellCode == cell {
			cellOK = true
			break
		}
	}
	if !cellOK {
		return false
	}
	for _, tp := range p.Points {
		if tp.At.Equal(t) {
			return true
		}
	}
	return false
}

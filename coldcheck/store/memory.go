package store

import (
	"context"
	"sync"
	"time"

	"coldcheck/domain"
)

// Memory is an in-process Store used for tests and the demo binary.
type Memory struct {
	mu          sync.RWMutex
	layout      *domain.LayoutVersion
	zones       map[string]*domain.Zone
	cells       map[string]*domain.Cell
	doors       map[string]*domain.Door
	nodes       map[string]*domain.Node
	batches     map[string]*domain.Batch
	rawDoors    []domain.RawDoorEvent
	air         []domain.AirReading
	occupancies []domain.Occupancy
	presence    []domain.PresenceEvent
	moves       []domain.MoveScan
	core        []domain.CoreMeasurement
	plans       []domain.FreezePlan
}

func NewMemory() *Memory {
	return &Memory{
		zones:   map[string]*domain.Zone{},
		cells:   map[string]*domain.Cell{},
		doors:   map[string]*domain.Door{},
		nodes:   map[string]*domain.Node{},
		batches: map[string]*domain.Batch{},
		layout:  &domain.LayoutVersion{ID: "mem-layout", Active: true, CreatedAt: time.Now()},
	}
}

func (m *Memory) Load(_ context.Context, now time.Time, window time.Duration) (*domain.Snapshot, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if now.IsZero() {
		now = time.Now()
	}
	from := now.Add(-window)
	snap := &domain.Snapshot{
		At: now, Window: window, Layout: clonePtr(m.layout),
		Zones: map[string]*domain.Zone{}, Cells: map[string]*domain.Cell{},
		Doors: map[string]*domain.Door{}, Nodes: map[string]*domain.Node{},
		Batches: map[string]*domain.Batch{},
	}
	for k, v := range m.zones {
		snap.Zones[k] = clonePtr(v)
	}
	for k, v := range m.cells {
		snap.Cells[k] = clonePtr(v)
	}
	for k, v := range m.doors {
		snap.Doors[k] = clonePtr(v)
	}
	for k, v := range m.nodes {
		snap.Nodes[k] = clonePtr(v)
	}
	for k, v := range m.batches {
		snap.Batches[k] = clonePtr(v)
	}
	for _, e := range m.rawDoors {
		if !e.At.Before(from) {
			snap.RawDoorEvents = append(snap.RawDoorEvents, e)
		}
	}
	for _, r := range m.air {
		if !r.At.Before(from) {
			snap.Air = append(snap.Air, r)
		}
	}
	for _, o := range m.occupancies {
		if o.To.After(from) && o.From.Before(now.Add(time.Minute)) {
			snap.Occupancies = append(snap.Occupancies, o)
		}
	}
	snap.Events = append(snap.Events, m.presence...)
	snap.MoveScans = append(snap.MoveScans, m.moves...)
	for _, c := range m.core {
		if !c.At.Before(from) {
			snap.Core = append(snap.Core, c)
		}
	}
	for _, p := range m.plans {
		snap.Plans = append(snap.Plans, clonePlan(p))
	}
	return snap, nil
}

func clonePtr[T any](p *T) *T {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

func clonePlan(p domain.FreezePlan) domain.FreezePlan {
	q := p
	q.Cells = append([]domain.FrozenCell(nil), p.Cells...)
	q.Points = append([]domain.FrozenTimePoint(nil), p.Points...)
	return q
}

func (m *Memory) SaveLayoutVersion(_ context.Context, v domain.LayoutVersion) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := v
	m.layout = &cp
	return nil
}
func (m *Memory) UpsertZone(_ context.Context, z domain.Zone) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := z
	cp.DairySegs = append([]string(nil), z.DairySegs...)
	cp.Polygon = append([][2]float64(nil), z.Polygon...)
	m.zones[z.Code] = &cp
	return nil
}
func (m *Memory) UpsertCell(_ context.Context, c domain.Cell) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := c
	cp.Neighbours = append([]string(nil), c.Neighbours...)
	m.cells[c.Code] = &cp
	return nil
}
func (m *Memory) UpsertDoor(_ context.Context, d domain.Door) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.doors[d.ID] = &d
	return nil
}
func (m *Memory) UpsertNode(_ context.Context, n domain.Node) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nodes[n.ID] = &n
	return nil
}
func (m *Memory) UpsertBatch(_ context.Context, b domain.Batch) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.batches[b.ID] = &b
	return nil
}
func (m *Memory) AddRawDoorEvent(_ context.Context, e domain.RawDoorEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rawDoors = append(m.rawDoors, e)
	return nil
}
func (m *Memory) AddAirReading(_ context.Context, r domain.AirReading) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.air = append(m.air, r)
	return nil
}
func (m *Memory) AddOccupancy(_ context.Context, o domain.Occupancy) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.occupancies = append(m.occupancies, o)
	return nil
}
func (m *Memory) AddPresenceEvent(_ context.Context, e domain.PresenceEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.presence = append(m.presence, e)
	return nil
}
func (m *Memory) AddMoveScan(_ context.Context, sc domain.MoveScan) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.moves = append(m.moves, sc)
	return nil
}
func (m *Memory) AddCoreMeasurement(_ context.Context, c domain.CoreMeasurement) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.core = append(m.core, c)
	return nil
}
func (m *Memory) SaveFreezePlan(_ context.Context, p domain.FreezePlan) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, ex := range m.plans {
		if ex.ID == p.ID {
			m.plans[i] = clonePlan(p)
			return nil
		}
	}
	m.plans = append(m.plans, clonePlan(p))
	return nil
}

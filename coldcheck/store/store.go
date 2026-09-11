package store

import (
	"context"
	"time"

	"coldcheck/domain"
)

// Store is the persistence boundary. PostgreSQL is the system of record for
// layout versions, product segments and manual measurements; the memory
// implementation backs tests/demo mode without external services.
type Store interface {
	Load(ctx context.Context, now time.Time, window time.Duration) (*domain.Snapshot, error)

	UpsertZone(ctx context.Context, z domain.Zone) error
	UpsertCell(ctx context.Context, c domain.Cell) error
	UpsertDoor(ctx context.Context, d domain.Door) error
	UpsertNode(ctx context.Context, n domain.Node) error
	UpsertBatch(ctx context.Context, b domain.Batch) error
	SaveLayoutVersion(ctx context.Context, v domain.LayoutVersion) error

	AddRawDoorEvent(ctx context.Context, e domain.RawDoorEvent) error
	AddAirReading(ctx context.Context, r domain.AirReading) error
	AddOccupancy(ctx context.Context, o domain.Occupancy) error
	AddPresenceEvent(ctx context.Context, e domain.PresenceEvent) error
	AddMoveScan(ctx context.Context, m domain.MoveScan) error
	AddCoreMeasurement(ctx context.Context, m domain.CoreMeasurement) error
	SaveFreezePlan(ctx context.Context, p domain.FreezePlan) error
}

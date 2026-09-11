package store_test

import (
	"context"
	"testing"
	"time"

	"coldcheck/domain"
	"coldcheck/store"
)

type tctx struct{}

func (tctx) Deadline() (time.Time, bool) { return time.Time{}, false }
func (tctx) Done() <-chan struct{}       { return nil }
func (tctx) Err() error                  { return nil }
func (tctx) Value(any) any               { return nil }

// Zones and cells written while a layout is active must bind to that layout
// version; geometry bound to another version must not appear in the snapshot.
func TestMemoryLayoutBinding(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	m := store.NewMemory()

	l1 := domain.LayoutVersion{ID: "L1", Active: true, CreatedAt: now}
	if err := m.SaveLayoutVersion(ctx, l1); err != nil {
		t.Fatal(err)
	}
	z1 := domain.Zone{Code: "Z1", Name: "z1", Layer: 0}
	c1 := domain.Cell{Code: "C1", ZoneCode: "Z1", Layer: 0, X: 1, Y: 1, Active: true}
	if err := m.UpsertZone(ctx, z1); err != nil {
		t.Fatal(err)
	}
	if err := m.UpsertCell(ctx, c1); err != nil {
		t.Fatal(err)
	}

	snap, err := m.Load(ctx, now, 4*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if got := snap.Zones["Z1"].LayoutID; got != "L1" {
		t.Fatalf("zone bound to %q, want L1", got)
	}
	if got := snap.Cells["C1"].LayoutID; got != "L1" {
		t.Fatalf("cell bound to %q, want L1", got)
	}

	// Activate a new layout; geometry explicitly bound to the old version is
	// excluded, geometry bound to the new version is included.
	l2 := domain.LayoutVersion{ID: "L2", Active: true, CreatedAt: now.Add(time.Hour)}
	if err := m.SaveLayoutVersion(ctx, l2); err != nil {
		t.Fatal(err)
	}
	if err := m.UpsertCell(ctx, domain.Cell{Code: "C2", ZoneCode: "Z1", Layer: 0, X: 2, Y: 2, Active: true, LayoutID: "L2"}); err != nil {
		t.Fatal(err)
	}

	snap, err = m.Load(ctx, now.Add(time.Hour), 4*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Layout.ID != "L2" {
		t.Fatalf("active layout = %q, want L2", snap.Layout.ID)
	}
	if _, ok := snap.Zones["Z1"]; ok {
		t.Fatal("zone bound to L1 must not load under active layout L2")
	}
	if _, ok := snap.Cells["C1"]; ok {
		t.Fatal("cell bound to L1 must not load under active layout L2")
	}
	c2 := snap.Cells["C2"]
	if c2 == nil || c2.LayoutID != "L2" {
		t.Fatalf("C2 missing or unbound under L2: %+v", c2)
	}
}

package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"coldcheck/domain"

	"github.com/lib/pq"
)

// Postgres is the system-of-record implementation. It reads one coherent
// window for evaluation and treats derived alerts as engine output.
type Postgres struct {
	DB *sql.DB
}

func NewPostgres(dsn string) (*Postgres, error) {
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	return &Postgres{DB: db}, nil
}

func (pg *Postgres) Load(ctx context.Context, now time.Time, window time.Duration) (*domain.Snapshot, error) {
	if now.IsZero() {
		now = time.Now()
	}
	from := now.Add(-window)
	snap := &domain.Snapshot{
		At: now, Window: window,
		Zones: map[string]*domain.Zone{}, Cells: map[string]*domain.Cell{},
		Doors: map[string]*domain.Door{}, Nodes: map[string]*domain.Node{},
		Batches: map[string]*domain.Batch{},
	}

	var lid, lnote string
	var lactive bool
	var lcreated time.Time
	switch err := pg.DB.QueryRowContext(ctx,
		`SELECT id, active, note, created_at FROM layout_versions WHERE active = true
		 ORDER BY created_at DESC LIMIT 1`).Scan(&lid, &lactive, &lnote, &lcreated); err {
	case nil:
		snap.Layout = &domain.LayoutVersion{ID: lid, Active: lactive, Note: lnote, CreatedAt: lcreated}
	case sql.ErrNoRows:
	default:
		return nil, err
	}

	zrows, err := pg.DB.QueryContext(ctx,
		`SELECT code,name,layer,min_c,max_c,target_c,dairy_segs,polygon,COALESCE(layout_id,'') FROM zones
		 WHERE layout_id IS NULL OR layout_id = $1`, lid)
	if err != nil {
		return nil, err
	}
	for zrows.Next() {
		var z domain.Zone
		var segs pq.StringArray
		var poly []byte
		if err := zrows.Scan(&z.Code, &z.Name, &z.Layer, &z.MinC, &z.MaxC, &z.TargetC, &segs, &poly, &z.LayoutID); err != nil {
			zrows.Close()
			return nil, err
		}
		z.DairySegs = []string(segs)
		if len(poly) > 0 {
			_ = json.Unmarshal(poly, &z.Polygon)
		}
		snap.Zones[z.Code] = &z
	}
	zrows.Close()

	crows, err := pg.DB.QueryContext(ctx,
		`SELECT code,zone_code,layer,x,y,neighbours,near_evap,near_door,active,COALESCE(layout_id,'') FROM cells
		 WHERE active = true AND (layout_id IS NULL OR layout_id = $1)`, lid)
	if err != nil {
		return nil, err
	}
	for crows.Next() {
		var c domain.Cell
		var nb pq.StringArray
		if err := crows.Scan(&c.Code, &c.ZoneCode, &c.Layer, &c.X, &c.Y, &nb, &c.NearEvap, &c.NearDoor, &c.Active, &c.LayoutID); err != nil {
			crows.Close()
			return nil, err
		}
		c.Neighbours = []string(nb)
		snap.Cells[c.Code] = &c
	}
	crows.Close()

	drows, err := pg.DB.QueryContext(ctx, `SELECT id,cell_code,name FROM doors`)
	if err != nil {
		return nil, err
	}
	for drows.Next() {
		var d domain.Door
		if err := drows.Scan(&d.ID, &d.CellCode, &d.Name); err != nil {
			drows.Close()
			return nil, err
		}
		snap.Doors[d.ID] = &d
	}
	drows.Close()

	nrows, err := pg.DB.QueryContext(ctx,
		`SELECT dev_eui,cell_code,layer,baseline_rssi,
		        EXTRACT(EPOCH FROM deadline)::float8,active FROM nodes`)
	if err != nil {
		return nil, err
	}
	for nrows.Next() {
		var n domain.Node
		var secs float64
		if err := nrows.Scan(&n.ID, &n.CellCode, &n.Layer, &n.BaselineRSSI, &secs, &n.Active); err != nil {
			nrows.Close()
			return nil, err
		}
		n.Deadline = time.Duration(secs) * time.Second
		snap.Nodes[n.ID] = &n
	}
	nrows.Close()

	brows, err := pg.DB.QueryContext(ctx,
		`SELECT id,product,seg_code,lot,min_c,max_c,EXTRACT(EPOCH FROM max_exposure)::float8 FROM batches`)
	if err != nil {
		return nil, err
	}
	for brows.Next() {
		var b domain.Batch
		var secs float64
		if err := brows.Scan(&b.ID, &b.Product, &b.SegCode, &b.Lot, &b.MinC, &b.MaxC, &secs); err != nil {
			brows.Close()
			return nil, err
		}
		b.MaxExposure = time.Duration(secs) * time.Second
		snap.Batches[b.ID] = &b
	}
	brows.Close()

	erows, err := pg.DB.QueryContext(ctx,
		`SELECT door_id,at,is_open,rssi FROM raw_door_events WHERE at >= $1 ORDER BY at`, from)
	if err != nil {
		return nil, err
	}
	for erows.Next() {
		var e domain.RawDoorEvent
		if err := erows.Scan(&e.DoorID, &e.At, &e.IsOpen, &e.RSSI); err != nil {
			erows.Close()
			return nil, err
		}
		snap.RawDoorEvents = append(snap.RawDoorEvents, e)
	}
	erows.Close()

	arows, err := pg.DB.QueryContext(ctx,
		`SELECT dev_eui,at,c,rssi FROM node_readings WHERE at >= $1 ORDER BY at`, from)
	if err != nil {
		return nil, err
	}
	for arows.Next() {
		var a domain.AirReading
		if err := arows.Scan(&a.NodeID, &a.At, &a.C, &a.RSSI); err != nil {
			arows.Close()
			return nil, err
		}
		snap.Air = append(snap.Air, a)
	}
	arows.Close()

	orows, err := pg.DB.QueryContext(ctx, `
		SELECT batch_id,cell_code,from_at,to_at,move_scan,COALESCE(move_scan_at,to_timestamp(0))
		FROM occupancies WHERE to_at >= $1 ORDER BY from_at`, from)
	if err != nil {
		return nil, err
	}
	for orows.Next() {
		var o domain.Occupancy
		var msAt time.Time
		if err := orows.Scan(&o.BatchID, &o.CellCode, &o.From, &o.To, &o.MoveScan, &msAt); err != nil {
			orows.Close()
			return nil, err
		}
		if o.MoveScan {
			o.MoveScanAt = msAt
		}
		snap.Occupancies = append(snap.Occupancies, o)
	}
	orows.Close()

	prows, err := pg.DB.QueryContext(ctx,
		`SELECT kind,batch_id,cell_code,at FROM presence_events WHERE at >= $1 ORDER BY at`, from)
	if err != nil {
		return nil, err
	}
	for prows.Next() {
		var e domain.PresenceEvent
		if err := prows.Scan(&e.Kind, &e.BatchID, &e.CellCode, &e.At); err != nil {
			prows.Close()
			return nil, err
		}
		snap.Events = append(snap.Events, e)
	}
	prows.Close()

	mrows, err := pg.DB.QueryContext(ctx,
		`SELECT scan_id,batch_id,from_cell,to_cell,at FROM move_scans WHERE at >= $1 ORDER BY at`, from)
	if err != nil {
		return nil, err
	}
	for mrows.Next() {
		var m domain.MoveScan
		var from sql.NullString
		if err := mrows.Scan(&m.ScanID, &m.BatchID, &from, &m.ToCell, &m.At); err != nil {
			mrows.Close()
			return nil, err
		}
		m.FromCell = from.String
		snap.MoveScans = append(snap.MoveScans, m)
	}
	mrows.Close()

	coreRows, err := pg.DB.QueryContext(ctx, `
		SELECT id,batch_id,cell_code,COALESCE(plan_id,''),at,c,
		       probe_depth_mm,required_depth_mm,reached_center,operator
		FROM core_measurements WHERE at >= $1 ORDER BY at`, from)
	if err != nil {
		return nil, err
	}
	for coreRows.Next() {
		var c domain.CoreMeasurement
		if err := coreRows.Scan(&c.ID, &c.BatchID, &c.CellCode, &c.PlanID,
			&c.At, &c.C, &c.ProbeDepthMM, &c.RequiredDepthMM, &c.ReachedCenter, &c.Operator); err != nil {
			coreRows.Close()
			return nil, err
		}
		snap.Core = append(snap.Core, c)
	}
	coreRows.Close()

	frows, err := pg.DB.QueryContext(ctx,
		`SELECT id,created_at,start_at,end_at,locked,cells,points FROM freeze_plans WHERE end_at >= $1`, from)
	if err != nil {
		return nil, err
	}
	for frows.Next() {
		var p domain.FreezePlan
		var cells, points []byte
		if err := frows.Scan(&p.ID, &p.CreatedAt, &p.Start, &p.End, &p.Locked, &cells, &points); err != nil {
			frows.Close()
			return nil, err
		}
		_ = json.Unmarshal(cells, &p.Cells)
		var pts []time.Time
		if err := json.Unmarshal(points, &pts); err == nil {
			for _, t := range pts {
				p.Points = append(p.Points, domain.FrozenTimePoint{At: t})
			}
		}
		snap.Plans = append(snap.Plans, p)
	}
	frows.Close()
	return snap, nil
}

func (pg *Postgres) SaveLayoutVersion(ctx context.Context, v domain.LayoutVersion) error {
	tx, err := pg.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if v.Active {
		if _, err := tx.ExecContext(ctx, `UPDATE layout_versions SET active=false`); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO layout_versions(id,active,note,created_at) VALUES($1,$2,$3,$4)
		 ON CONFLICT (id) DO UPDATE SET active=EXCLUDED.active,note=EXCLUDED.note`,
		v.ID, v.Active, v.Note, v.CreatedAt)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (pg *Postgres) UpsertZone(ctx context.Context, z domain.Zone) error {
	poly, _ := json.Marshal(z.Polygon)
	layoutID, err := pg.resolveLayout(ctx, z.LayoutID)
	if err != nil {
		return err
	}
	var lid any
	if layoutID != "" {
		lid = layoutID
	}
	_, err = pg.DB.ExecContext(ctx, `
		INSERT INTO zones(code,name,layer,min_c,max_c,target_c,dairy_segs,polygon,layout_id)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)
		ON CONFLICT (code) DO UPDATE SET name=EXCLUDED.name,layer=EXCLUDED.layer,
		  min_c=EXCLUDED.min_c,max_c=EXCLUDED.max_c,target_c=EXCLUDED.target_c,
		  dairy_segs=EXCLUDED.dairy_segs,polygon=EXCLUDED.polygon,layout_id=EXCLUDED.layout_id`,
		z.Code, z.Name, z.Layer, z.MinC, z.MaxC, z.TargetC, pq.Array(z.DairySegs), poly, lid)
	return err
}

func (pg *Postgres) UpsertCell(ctx context.Context, c domain.Cell) error {
	layoutID, err := pg.resolveLayout(ctx, c.LayoutID)
	if err != nil {
		return err
	}
	var lid any
	if layoutID != "" {
		lid = layoutID
	}
	_, err = pg.DB.ExecContext(ctx, `
		INSERT INTO cells(code,zone_code,layer,x,y,neighbours,near_evap,near_door,active,layout_id)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
		ON CONFLICT (code) DO UPDATE SET zone_code=EXCLUDED.zone_code,layer=EXCLUDED.layer,
		  x=EXCLUDED.x,y=EXCLUDED.y,neighbours=EXCLUDED.neighbours,
		  near_evap=EXCLUDED.near_evap,near_door=EXCLUDED.near_door,active=EXCLUDED.active,
		  layout_id=EXCLUDED.layout_id`,
		c.Code, c.ZoneCode, c.Layer, c.X, c.Y, pq.Array(c.Neighbours), c.NearEvap, c.NearDoor, c.Active, lid)
	return err
}

// resolveLayout returns the layout id a geometry row must bind to. An
// explicit id wins; otherwise the currently active layout version is used;
// with no layout versions at all the geometry stays unbound (NULL).
func (pg *Postgres) resolveLayout(ctx context.Context, explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	var id string
	err := pg.DB.QueryRowContext(ctx,
		`SELECT id FROM layout_versions WHERE active = true ORDER BY created_at DESC LIMIT 1`).Scan(&id)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return id, err
}

func (pg *Postgres) UpsertDoor(ctx context.Context, d domain.Door) error {
	_, err := pg.DB.ExecContext(ctx,
		`INSERT INTO doors(id,cell_code,name) VALUES($1,$2,$3)
		 ON CONFLICT (id) DO UPDATE SET cell_code=EXCLUDED.cell_code,name=EXCLUDED.name`,
		d.ID, d.CellCode, d.Name)
	return err
}

func (pg *Postgres) UpsertNode(ctx context.Context, n domain.Node) error {
	_, err := pg.DB.ExecContext(ctx,
		`INSERT INTO nodes(dev_eui,cell_code,layer,baseline_rssi,deadline,active)
		 VALUES($1,$2,$3,$4,$5 * interval '1 second',$6)
		 ON CONFLICT (dev_eui) DO UPDATE SET cell_code=EXCLUDED.cell_code,layer=EXCLUDED.layer,
		   baseline_rssi=EXCLUDED.baseline_rssi,deadline=EXCLUDED.deadline,active=EXCLUDED.active`,
		n.ID, n.CellCode, n.Layer, n.BaselineRSSI, int64(n.Deadline.Seconds()), n.Active)
	return err
}

func (pg *Postgres) UpsertBatch(ctx context.Context, b domain.Batch) error {
	_, err := pg.DB.ExecContext(ctx,
		`INSERT INTO batches(id,product,seg_code,lot,min_c,max_c,max_exposure)
		 VALUES($1,$2,$3,$4,$5,$6,$7 * interval '1 second')
		 ON CONFLICT (id) DO UPDATE SET product=EXCLUDED.product,seg_code=EXCLUDED.seg_code,
		   lot=EXCLUDED.lot,min_c=EXCLUDED.min_c,max_c=EXCLUDED.max_c,max_exposure=EXCLUDED.max_exposure`,
		b.ID, b.Product, b.SegCode, b.Lot, b.MinC, b.MaxC, int64(b.MaxExposure.Seconds()))
	return err
}

func (pg *Postgres) AddRawDoorEvent(ctx context.Context, e domain.RawDoorEvent) error {
	_, err := pg.DB.ExecContext(ctx,
		`INSERT INTO raw_door_events(door_id,at,is_open,rssi) VALUES($1,$2,$3,$4)`,
		e.DoorID, e.At, e.IsOpen, e.RSSI)
	return err
}

func (pg *Postgres) AddAirReading(ctx context.Context, r domain.AirReading) error {
	_, err := pg.DB.ExecContext(ctx,
		`INSERT INTO node_readings(dev_eui,at,c,rssi) VALUES($1,$2,$3,$4)
		 ON CONFLICT DO NOTHING`, r.NodeID, r.At, r.C, r.RSSI)
	return err
}

func (pg *Postgres) AddOccupancy(ctx context.Context, o domain.Occupancy) error {
	var msAt interface{}
	if o.MoveScan && !o.MoveScanAt.IsZero() {
		msAt = o.MoveScanAt
	}
	_, err := pg.DB.ExecContext(ctx,
		`INSERT INTO occupancies(batch_id,cell_code,from_at,to_at,move_scan,move_scan_at)
		 VALUES($1,$2,$3,$4,$5,$6)`,
		o.BatchID, o.CellCode, o.From, o.To, o.MoveScan, msAt)
	return err
}

func (pg *Postgres) AddPresenceEvent(ctx context.Context, e domain.PresenceEvent) error {
	_, err := pg.DB.ExecContext(ctx,
		`INSERT INTO presence_events(kind,batch_id,cell_code,at) VALUES($1,$2,$3,$4)`,
		e.Kind, e.BatchID, e.CellCode, e.At)
	return err
}

func (pg *Postgres) AddMoveScan(ctx context.Context, m domain.MoveScan) error {
	var from interface{}
	if m.FromCell != "" {
		from = m.FromCell
	}
	_, err := pg.DB.ExecContext(ctx,
		`INSERT INTO move_scans(scan_id,batch_id,from_cell,to_cell,at) VALUES($1,$2,$3,$4,$5)
		 ON CONFLICT (scan_id) DO NOTHING`,
		m.ScanID, m.BatchID, from, m.ToCell, m.At)
	return err
}

func (pg *Postgres) AddCoreMeasurement(ctx context.Context, m domain.CoreMeasurement) error {
	_, err := pg.DB.ExecContext(ctx,
		`INSERT INTO core_measurements(id,batch_id,cell_code,plan_id,at,c,
		   probe_depth_mm,required_depth_mm,reached_center,operator)
		 VALUES($1,$2,$3,NULLIF($4,''),$5,$6,$7,$8,$9,$10)
		 ON CONFLICT (id) DO NOTHING`,
		m.ID, m.BatchID, m.CellCode, m.PlanID, m.At, m.C,
		m.ProbeDepthMM, m.RequiredDepthMM, m.ReachedCenter, m.Operator)
	return err
}

func (pg *Postgres) SaveFreezePlan(ctx context.Context, p domain.FreezePlan) error {
	cells, _ := json.Marshal(p.Cells)
	pts := make([]time.Time, 0, len(p.Points))
	for _, t := range p.Points {
		pts = append(pts, t.At)
	}
	points, _ := json.Marshal(pts)
	_, err := pg.DB.ExecContext(ctx,
		`INSERT INTO freeze_plans(id,created_at,start_at,end_at,locked,cells,points)
		 VALUES($1,$2,$3,$4,$5,$6,$7)
		 ON CONFLICT (id) DO UPDATE SET start_at=EXCLUDED.start_at,end_at=EXCLUDED.end_at,
		   locked=EXCLUDED.locked,cells=EXCLUDED.cells,points=EXCLUDED.points`,
		p.ID, p.CreatedAt, p.Start, p.End, p.Locked, cells, points)
	return err
}

-- Cold-chain verification schema (PostgreSQL 14+).
-- Layout versions, product segments and manual measurements live here.
-- Derived alerts are computed by the Go rule engine, not stored as truth.

CREATE TABLE IF NOT EXISTS layout_versions (
    id          text PRIMARY KEY,
    active      boolean NOT NULL DEFAULT false,
    note        text NOT NULL DEFAULT '',
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS zones (
    code        text PRIMARY KEY,
    name        text NOT NULL,
    layer       int  NOT NULL,
    min_c       numeric(5,2) NOT NULL,
    max_c       numeric(5,2) NOT NULL,
    target_c    numeric(5,2) NOT NULL,
    dairy_segs  text[] NOT NULL DEFAULT '{}',
    polygon     jsonb NOT NULL DEFAULT '[]',
    -- Binds this zone geometry to a frozen layout version. NULL is allowed
    -- only for legacy rows; new upserts resolve the active layout version.
    layout_id   text REFERENCES layout_versions(id)
);

CREATE TABLE IF NOT EXISTS cells (
    code        text PRIMARY KEY,
    zone_code   text NOT NULL REFERENCES zones(code),
    layer       int  NOT NULL,
    x           double precision NOT NULL,
    y           double precision NOT NULL,
    neighbours  text[] NOT NULL DEFAULT '{}',
    near_evap   boolean NOT NULL DEFAULT false,
    near_door   boolean NOT NULL DEFAULT false,
    active      boolean NOT NULL DEFAULT true,
    -- Binds this cell geometry to a frozen layout version (see zones.layout_id).
    layout_id   text REFERENCES layout_versions(id)
);

CREATE INDEX IF NOT EXISTS zones_layout_idx ON zones (layout_id);
CREATE INDEX IF NOT EXISTS cells_layout_idx ON cells (layout_id);

CREATE TABLE IF NOT EXISTS doors (
    id        text PRIMARY KEY,
    cell_code text NOT NULL REFERENCES cells(code),
    name      text NOT NULL
);

-- Evaporators are cooling coils. The platform only READS their defrost
-- state; no defrost cycle can be started or stopped from here.
CREATE TABLE IF NOT EXISTS evaporators (
    id        text PRIMARY KEY,
    zone_code text NOT NULL REFERENCES zones(code),
    name      text NOT NULL DEFAULT '',
    -- Explicitly served cells; empty array means the whole zone, so one
    -- zone can host several independently defrosting evaporators.
    cells     text[] NOT NULL DEFAULT '{}'
);

-- Read-only defrost-state transitions. device_at is the coil clock (truth
-- time), ingested_at when the platform learned of the state; a large gap is
-- late telemetry. Windows are always reconstructed from device_at.
CREATE TABLE IF NOT EXISTS defrost_events (
    id          bigserial PRIMARY KEY,
    evap_id     text NOT NULL REFERENCES evaporators(id),
    device_at   timestamptz NOT NULL,
    starting    boolean NOT NULL,
    ingested_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS defrost_events_time_idx ON defrost_events (device_at);

-- Air nodes out of service (calibration / battery swap / replacement).
-- Readings inside [from_at,to_at) are untrustworthy; an open to_at means
-- maintenance is still ongoing and the node must not look offline.
CREATE TABLE IF NOT EXISTS node_maintenance (
    id        bigserial PRIMARY KEY,
    dev_eui   text NOT NULL REFERENCES nodes(dev_eui),
    from_at   timestamptz NOT NULL,
    to_at     timestamptz,
    reason    text NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS node_maintenance_time_idx ON node_maintenance (from_at, to_at);

CREATE TABLE IF NOT EXISTS nodes (
    dev_eui       text PRIMARY KEY,
    cell_code     text NOT NULL REFERENCES cells(code),
    layer         int  NOT NULL,
    baseline_rssi int  NOT NULL DEFAULT 0,
    deadline      interval NOT NULL DEFAULT interval '20 minutes',
    active        boolean NOT NULL DEFAULT true
);

CREATE TABLE IF NOT EXISTS node_readings (
    dev_eui   text NOT NULL REFERENCES nodes(dev_eui),
    at        timestamptz NOT NULL,
    c         numeric(6,3) NOT NULL,
    rssi      int NOT NULL,
    gateway_id text,
    raw       jsonb,
    PRIMARY KEY (dev_eui, at)
);
CREATE INDEX IF NOT EXISTS node_readings_at_idx ON node_readings (at);

CREATE TABLE IF NOT EXISTS raw_door_events (
    id      bigserial PRIMARY KEY,
    door_id text NOT NULL REFERENCES doors(id),
    at      timestamptz NOT NULL,
    is_open boolean NOT NULL,
    rssi    int NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS raw_door_events_at_idx ON raw_door_events (at);

CREATE TABLE IF NOT EXISTS batches (
    id           text PRIMARY KEY,
    product      text NOT NULL,
    seg_code     text NOT NULL,           -- product segment
    lot          text NOT NULL,
    min_c        numeric(5,2) NOT NULL,
    max_c        numeric(5,2) NOT NULL,
    max_exposure interval NOT NULL DEFAULT interval '30 minutes'
);

CREATE TABLE IF NOT EXISTS occupancies (
    id          bigserial PRIMARY KEY,
    batch_id    text NOT NULL REFERENCES batches(id),
    cell_code   text NOT NULL REFERENCES cells(code),
    from_at     timestamptz NOT NULL,
    to_at       timestamptz NOT NULL,
    move_scan   boolean NOT NULL DEFAULT false,
    move_scan_at timestamptz
);
CREATE INDEX IF NOT EXISTS occupancies_time_idx ON occupancies (from_at, to_at);

CREATE TABLE IF NOT EXISTS presence_events (
    id        bigserial PRIMARY KEY,
    kind      text NOT NULL CHECK (kind IN ('arrive','leave')),
    batch_id  text NOT NULL REFERENCES batches(id),
    cell_code text NOT NULL REFERENCES cells(code),
    at        timestamptz NOT NULL
);

CREATE TABLE IF NOT EXISTS move_scans (
    scan_id   text PRIMARY KEY,
    batch_id  text NOT NULL REFERENCES batches(id),
    from_cell text REFERENCES cells(code),
    to_cell   text NOT NULL REFERENCES cells(code),
    at        timestamptz NOT NULL
);

CREATE TABLE IF NOT EXISTS core_measurements (
    id                 text PRIMARY KEY,
    batch_id           text NOT NULL REFERENCES batches(id),
    cell_code          text NOT NULL REFERENCES cells(code),
    plan_id            text,
    at                 timestamptz NOT NULL,
    c                  numeric(6,3) NOT NULL,
    probe_depth_mm     numeric(7,2) NOT NULL,
    required_depth_mm  numeric(7,2) NOT NULL,
    reached_center     boolean NOT NULL DEFAULT false,
    operator           text NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS core_measurements_at_idx ON core_measurements (at);

CREATE TABLE IF NOT EXISTS freeze_plans (
    id         text PRIMARY KEY,
    created_at timestamptz NOT NULL DEFAULT now(),
    start_at   timestamptz NOT NULL,
    end_at     timestamptz NOT NULL,
    locked     boolean NOT NULL DEFAULT true,
    cells      jsonb NOT NULL DEFAULT '[]',
    points     jsonb NOT NULL DEFAULT '[]'
);

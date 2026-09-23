-- geofence-service schema — genesis (0001)
--
-- This directory is the source of truth for the geofence-service Oracle
-- schema. The service does NOT apply these itself: schema changes are
-- reviewed here, then run by the DBA against each environment by hand. The
-- Go model (internal/database/model.Geofence) and this file must be kept in
-- sync manually — nothing checks them against each other automatically, so
-- any change to one needs a matching change to the other in the same PR.
--
-- Convention: one file per change, numbered sequentially
-- (0002_add_x.sql, 0003_add_y.sql, ...), each containing only the
-- incremental DDL for that change. Never edit an already-applied file —
-- once the DBA has run it against an environment, treat it as immutable
-- history and add a new numbered file instead.
--
-- This file is the frozen genesis schema (pre-release — no deployed data),
-- previously tracked as a gormigrate InitSchema in
-- internal/db/initial/oracle/migrate.go before the service moved to
-- DBA-applied migrations.

CREATE TABLE geofence_rules (
    id                       RAW(16)            DEFAULT SYS_GUID() PRIMARY KEY,
    agency_id                RAW(16)            NOT NULL,
    client_id                RAW(16),
    name                     VARCHAR2(150)      NOT NULL,
    type                     NUMERIC            NOT NULL CHECK (type IN (1,2,3)),
    geometry                 MDSYS.SDO_GEOMETRY NOT NULL,
    geo_json                 CLOB               NOT NULL,
    status                   NUMERIC            DEFAULT 1 NOT NULL CHECK (status IN (1,2,3,4)),
    exclude_from_colocation  NUMBER(1)          DEFAULT 0 NOT NULL,
    notes                    VARCHAR2(500),
    created_at               TIMESTAMP DEFAULT SYSTIMESTAMP NOT NULL,
    updated_at               TIMESTAMP DEFAULT SYSTIMESTAMP,
    version                  NUMERIC,
    created_by               RAW(16)            NOT NULL,
    updated_by               RAW(16)
);

-- type codes: 1=Circle, 2=Polygon, 3=Rectangle (model.GeofenceType)
-- status codes: 1=active, 2=inactive, 3=archived, 4=deleted (model.GeofenceStatus)
-- Both are numeric-coded enums, mirroring sso-agency-service's
-- AgencyStatus/AgencyType convention — the API layer never sees the raw
-- codes; pkg/handler/model translates to/from friendly strings at the
-- request/response boundary.

-- geo_json stores the canonical GeoJSON alongside the spatial column so API
-- reads are lossless without reverse-engineering Oracle's internal
-- SDO_GEOMETRY encoding (see internal/database/model.Geofence's doc comment).

-- exclude_from_colocation / notes back the "Known Locations" feature
-- (circle geofences excluded from co-location detection).

-- created_by / updated_by are actor-tracking columns for the Known
-- Locations audit trail.

-- Location names must be unique within an agency.
CREATE UNIQUE INDEX uni_geofence_agency_name ON geofence_rules(agency_id, name);

-- Backs lookups scoped by agency/client and filtered by status.
CREATE INDEX idx_geofence_agency_client ON geofence_rules(agency_id, client_id, status);

-- Oracle Spatial setup — mandatory dimensional metadata registration plus
-- the spatial index backing FindContainingPoint's SDO_RELATE query.
--
-- SRID 4326 (internal/database/spatial.DefaultSRID) matches the SRID used
-- when constructing SDO_GEOMETRY values in that package — these must stay
-- in sync.
DELETE FROM USER_SDO_GEOM_METADATA WHERE TABLE_NAME = 'GEOFENCE_RULES' AND COLUMN_NAME = 'GEOMETRY';

INSERT INTO USER_SDO_GEOM_METADATA (TABLE_NAME, COLUMN_NAME, DIMINFO, SRID)
VALUES ('GEOFENCE_RULES', 'GEOMETRY',
  MDSYS.SDO_DIM_ARRAY(
    MDSYS.SDO_DIM_ELEMENT('LONGITUDE', -180, 180, 0.005),
    MDSYS.SDO_DIM_ELEMENT('LATITUDE', -90, 90, 0.005)
  ), 4326);

CREATE INDEX idx_geofence_spatial ON geofence_rules(geometry) INDEXTYPE IS MDSYS.SPATIAL_INDEX;

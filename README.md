# Cellar Run Control

Cellar Run Control is a local Go HTTP service for coordinating a small
production cellar. It keeps formula approval, vessel availability, active
fermentation runs, gravity observations, conditioning, completion metrics, and
vessel cleaning in one deterministic process.

This repository is a healthy initialization baseline. It has no intentional
defects and no automated test suite. Tests are intentionally deferred to a
later engineering task; the production workflow checks in this repository
remain available for immediate build and behavior verification.

## Requirements

- Go 1.23 or later
- A writable local data directory
- No database, network service, or third-party module

## Build And Run

Build every package:

```bash
go build ./...
```

Start the service:

```bash
go run ./cmd/fermentctl serve
```

The default address is `127.0.0.1:8088`. The default snapshot directory is
`./data`.

Environment variables:

- `CELLAR_CONTROL_ADDR`: HTTP listen address.
- `CELLAR_CONTROL_DATA_DIR`: directory containing `state.json`.

Example:

```bash
CELLAR_CONTROL_ADDR=127.0.0.1:9099 \
CELLAR_CONTROL_DATA_DIR=/tmp/cellar-control \
go run ./cmd/fermentctl serve
```

## Directory Structure

```text
cmd/fermentctl                 service and workflow-check executable
internal/clock                 time boundary
internal/config                environment configuration
internal/domain                entities, validation, transitions, metrics
internal/httpapi               HTTP routes, JSON decoding, errors
internal/service               use-case orchestration
internal/storage               revisioned snapshot persistence
internal/workflowcheck         real HTTP production checks
```

## Public API

Health:

- `GET /healthz`

Formula workflow:

- `POST /v1/formulas`
- `GET /v1/formulas`
- `GET /v1/formulas/{id}`
- `PUT /v1/formulas/{id}/steps`
- `POST /v1/formulas/{id}/approve`
- `POST /v1/formulas/{id}/retire`

Vessel workflow:

- `POST /v1/vessels`
- `GET /v1/vessels`
- `GET /v1/vessels/{id}`
- `POST /v1/vessels/{id}/finish-cleaning`
- `POST /v1/vessels/{id}/retire`

Run workflow:

- `POST /v1/runs`
- `GET /v1/runs`
- `GET /v1/runs/{id}`
- `POST /v1/runs/{id}/reserve`
- `POST /v1/runs/{id}/start`
- `POST /v1/runs/{id}/observations`
- `POST /v1/runs/{id}/conditioning`
- `POST /v1/runs/{id}/complete`
- `POST /v1/runs/{id}/abort`

Seasonal batch import (two-phase, preview then confirm):

- `POST /v1/imports` — upload the CSV bundle (`text/csv` body, or
  `multipart/form-data` with a `file` field) and receive a line-annotated
  precheck. Nothing is created at this stage.
- `GET /v1/imports` — list import sessions and their progress.
- `GET /v1/imports/{id}` — read one session with its full plan.
- `POST /v1/imports/{id}/confirm` — create the whole validated batch in one
  atomic snapshot revision.

Writes accept an optional `X-Expected-Version` header. Errors use a stable
shape:

```json
{
  "error": {
    "code": "revision_conflict",
    "detail": "the aggregate version does not match X-Expected-Version",
    "field": ""
  }
}
```

## Workflow Checks

Run each production workflow check separately:

```bash
go run ./cmd/fermentctl check formula-approval
go run ./cmd/fermentctl check vessel-run-lifecycle
go run ./cmd/fermentctl check observations-completion
go run ./cmd/fermentctl check batch-import
```

Each check creates a temporary snapshot directory, starts the real HTTP
handler on a loopback listener, issues JSON requests, verifies success and
failure behavior, and removes its temporary data. The `batch-import` check
exercises CSV precheck, blocked confirmation, atomic one-revision creation,
idempotent re-upload as adopt, code collisions, and crash reconciliation.

## Seasonal CSV Format

The first line is a fixed header; every later line uses only the columns
its `kind` needs:

```text
kind,ref,name,style,target_original_gravity,target_final_gravity,max_fermentation_temp_c,minimum_days,maximum_days,lifecycle,step_sequence,step_label,step_temperature_c,step_hold_minutes,vessel_code,vessel_capacity_liters,run_code,run_formula,run_vessel,planned_volume_liters,notes
```

- `formula` rows carry `ref`, the name/style/gravity/temperature/days
  fields, and `lifecycle` (`draft` or `approved`, default `approved`).
- `step` rows attach to a formula by its file `ref` and carry the sequence,
  label, temperature, and hold time. An approved formula needs a valid
  contiguous step list beginning at 1.
- `vessel` rows carry `ref`, `vessel_code`, and `vessel_capacity_liters`.
- `run` rows carry `run_code`, `run_formula`, an optional `run_vessel`,
  `planned_volume_liters`, and `notes`. `run_formula`/`run_vessel` resolve
  against file `ref`s first, then existing formula names / vessel codes.

The preview returns every entity as `create`, `adopt`, or `conflict`, plus
issues anchored at the CSV line. Confirmation applies only a plan with zero
errors and zero conflicts. Re-uploading the identical file is keyed by its
SHA-256 and reports `adopt`, so it can never create a second round. Session
ledgers live in `<data-directory>/imports`; an interrupted `applying`
session is reconciled to `completed` (or back to `previewed`) on its next
read, making the last stopping point visible after a restart.

## Persistence And Recovery

The service stores a complete revisioned snapshot at
`<data-directory>/state.json`. A write creates a temporary file in the same
directory, commits the encoded bytes durably, closes the handle, and renames
the result over the previous snapshot. The in-memory snapshot is replaced only
after that operation succeeds.

On startup, an existing snapshot is decoded and normalized. Unknown or
conflicting map indexes are not silently invented; invalid JSON or a negative
revision stops the process before HTTP serving begins.

## Repair-Ready Boundaries

The baseline intentionally keeps several independent behavior surfaces visible
for later engineering evaluation:

- formula name normalization and duplicate detection;
- target gravity and process-step range validation;
- aggregate version conflict checks;
- atomic vessel reservation and release;
- run state transition guards;
- observation sequence and time ordering;
- gravity movement and range checks;
- metric calculation and rounding;
- durable snapshot replacement and startup normalization.

These are production mechanisms, not injected defects. A later task can add
automated tests or request a focused repair without changing the public API.

## Test Boundary

This stage does not create unit tests, integration tests, test fixtures, or an
E2E suite. The later test task owns systematic coverage of validation limits,
state transitions, concurrency, persistence failures, and numeric edge cases.

## Scope

The service does not control physical equipment, authenticate remote users,
send notifications, expose a browser interface, or replicate data across
processes. It is intended for a trusted local network or an internal adapter.

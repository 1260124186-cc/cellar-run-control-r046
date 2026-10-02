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

Season CSV import (two-phase precheck then confirm):

- `POST /v1/imports`
- `GET /v1/imports`
- `GET /v1/imports/{id}`
- `POST /v1/imports/{id}/confirm`

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
go run ./cmd/fermentctl check season-import
```

Each check creates a temporary snapshot directory, starts the real HTTP
handler on a loopback listener, issues JSON requests, verifies success and
failure behavior, and removes its temporary data.

## Season CSV Import

Before the season starts, one CSV can carry formulas, their process steps,
vessels, and planned runs in a single upload. The import is a deliberate
two-phase operation so nothing is half-created:

1. `POST /v1/imports` stores and runs a full precheck. The response lists, with
   physical file line numbers, the entities it **would create**, every
   **conflict** with an existing name/code, and every **blocking issue**.
2. `POST /v1/imports/{id}/confirm` commits the whole planned set in a single
   atomic snapshot revision. If any entity or reference is invalid, precheck is
   `rejected` and confirmation is refused, so nothing is created at all.

The same byte-identical file uploaded twice resolves to the same import
session (keyed by a SHA-256 of its contents): a completed file is a no-op on
repeat and never generates a second round of entities. Import sessions live in
the durable snapshot, so an upload or confirmation interrupted by a restart can
be inspected with `GET /v1/imports/{id}` and then confirmed.

The first row is a fixed header. Each later row sets `kind` to one of
`formula`, `step`, `vessel`, or `run`; columns that do not apply to a row are
left empty.

| Column | Used by | Meaning |
| --- | --- | --- |
| `kind` | all | `formula`, `step`, `vessel`, or `run` |
| `name`, `style` | formula | normalized formula name (unique, case/space folded) and style |
| `target_original_gravity`, `target_final_gravity` | formula | validated against the same bounds as the API |
| `max_fermentation_temp_c`, `minimum_days`, `maximum_days` | formula | validated against the same bounds as the API |
| `approved` | formula | `true` imports the formula already approved and requires valid steps; `false` leaves it a draft |
| `capacity_liters`, `code` | vessel | 10–5000 L and a unique vessel code |
| `code`, `planned_volume_liters`, `notes` | run | unique run code, positive planned volume |
| `formula` | step, run | formula name the step belongs to or the run uses |
| `sequence`, `label`, `temperature_c`, `hold_minutes` | step | contiguous steps from 1, validated like `PUT /steps` |

A run's `formula` may name a formula defined earlier or later in the same file
or an already-approved formula in the cellar. Referencing a non-existent or
unapproved formula is a blocking issue on that run's line. A formula, vessel,
or run whose name/code already exists is reported under `conflicts` and is
skipped (it does not block the rest of the file).

Example:

```text
kind,name,style,...,approved,capacity_liters,code,formula,planned_volume_liters,...,sequence,label,temperature_c,hold_minutes
formula,Cellar Helles,Lager,...,true,,,,,,...
step,,,,,,,,Cellar Helles,,...,1,Mash,64,70
vessel,,,,,,1800,FV-100,,,,,...
run,,,,,,,,,RUN-OPEN-1,Cellar Helles,850,...
```

The same flow is available on the command line:

```bash
# precheck only; prints what would be created, conflicts, and line-numbered issues
go run ./cmd/fermentctl import season.csv --data-dir=./data
# precheck and, if clean, commit everything atomically
go run ./cmd/fermentctl import season.csv --yes
# inspect where an interrupted import finished (or list every session)
go run ./cmd/fermentctl import-status
go run ./cmd/fermentctl import-status <import-id>
# confirm a session that passed precheck in an earlier process
go run ./cmd/fermentctl import-confirm <import-id>
```

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

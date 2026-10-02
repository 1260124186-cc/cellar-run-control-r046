// Package importplan parses the seasonal CSV bundle and turns it into a
// deterministic, line-annotated import plan. It performs no persistence:
// callers supply the current catalog and receive a plan describing what
// would be created, adopted from a previous identical upload, or rejected
// because of a name or code conflict.
package importplan

import (
	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/domain"
)

const (
	KindFormula = "formula"
	KindStep    = "step"
	KindVessel  = "vessel"
	KindRun     = "run"
)

const (
	SeverityError   = "error"
	SeverityWarning = "warning"
)

const (
	IssueInvalidRow     = "invalid_row"
	IssueUnknownKind    = "unknown_kind"
	IssueEmptyFile      = "empty_file"
	IssueDuplicateRef   = "duplicate_reference"
	IssueReference      = "unresolved_reference"
	IssueFormulaState   = "formula_not_approved"
	IssueCapacity       = "volume_exceeds_capacity"
	IssueNameConflict   = "formula_name_conflict"
	IssueVesselConflict = "vessel_code_conflict"
	IssueRunConflict    = "run_code_conflict"
	IssueDrift          = "imported_entity_changed"
)

const (
	// ActionCreate marks an entity that would be inserted on confirmation.
	ActionCreate = "create"
	// ActionAdopt marks an entity already introduced by an earlier upload
	// of the exact same file; confirmation leaves it untouched.
	ActionAdopt = "adopt"
	// ActionConflict marks a name/code owned by an unrelated entity; the
	// plan is not confirmable until the file changes.
	ActionConflict = "conflict"
)

// Issue is a single precheck finding anchored at the source CSV line.
type Issue struct {
	Line     int    `json:"line"`
	Kind     string `json:"kind"`
	Severity string `json:"severity"`
	Field    string `json:"field,omitempty"`
	Ref      string `json:"ref,omitempty"`
	Detail   string `json:"detail"`
}

// FormulaRow is a parsed `kind=formula` line.
type FormulaRow struct {
	Line                  int
	Ref                   string
	Name                  string
	Style                 string
	TargetOriginalGravity float64
	TargetFinalGravity    float64
	MaxFermentationTempC  float64
	MinimumDays           int
	MaximumDays           int
	Lifecycle             domain.FormulaState
	HasParseError         bool
}

// StepRow is a parsed `kind=step` line.
type StepRow struct {
	Line          int
	FormulaRef    string
	Sequence      int
	Label         string
	TemperatureC  float64
	HoldMinutes   int
	HasParseError bool
}

// VesselRow is a parsed `kind=vessel` line.
type VesselRow struct {
	Line          int
	Ref           string
	Code          string
	CapacityL     float64
	HasParseError bool
}

// RunRow is a parsed `kind=run` line.
type RunRow struct {
	Line           int
	Code           string
	FormulaRef     string
	VesselRef      string
	PlannedVolumeL float64
	Notes          string
	HasVesselRef   bool
	HasParseError  bool
}

// ParsedFile holds every decoded row plus structural parse findings.
type ParsedFile struct {
	Formulas []FormulaRow
	Steps    []StepRow
	Vessels  []VesselRow
	Runs     []RunRow
	Issues   []Issue
	// HeaderOK is false when the file is empty or its header cannot be
	// interpreted; such files never become import sessions.
	HeaderOK bool
	// TotalLines counts the last physical record line of the file.
	TotalLines int
}

// FormulaItem describes one formula declaration from the file.
type FormulaItem struct {
	Line   int    `json:"line"`
	Ref    string `json:"ref"`
	Action string `json:"action"`
	// ID is the deterministic id this import assigns on create, or the
	// existing entity id on adopt/conflict.
	ID        string               `json:"id"`
	Name      string               `json:"name"`
	Style     string               `json:"style"`
	State     domain.FormulaState  `json:"state"`
	Steps     []domain.FormulaStep `json:"steps"`
	Conflicts []Issue              `json:"conflicts,omitempty"`
}

// VesselItem describes one vessel declaration from the file.
type VesselItem struct {
	Line      int     `json:"line"`
	Ref       string  `json:"ref"`
	Action    string  `json:"action"`
	ID        string  `json:"id"`
	Code      string  `json:"code"`
	CapacityL float64 `json:"capacity_liters"`
	Conflicts []Issue `json:"conflicts,omitempty"`
}

// RunItem describes one planned run declaration from the file.
type RunItem struct {
	Line           int     `json:"line"`
	Action         string  `json:"action"`
	ID             string  `json:"id"`
	Code           string  `json:"code"`
	FormulaRef     string  `json:"formula_ref"`
	FormulaID      string  `json:"formula_id"`
	VesselRef      string  `json:"vessel_ref"`
	VesselID       string  `json:"vessel_id"`
	PlannedVolumeL float64 `json:"planned_volume_liters"`
	Notes          string  `json:"notes"`
	Conflicts      []Issue `json:"conflicts,omitempty"`
}

// Plan is the result of prechecking a parsed file against a catalog.
type Plan struct {
	Formulas []FormulaItem `json:"formulas"`
	Vessels  []VesselItem  `json:"vessels"`
	Runs     []RunItem     `json:"runs"`
	Issues   []Issue       `json:"issues"`
}

// Counts summarizes planned entity handling.
type Counts struct {
	FormulasCreate int `json:"formulas_create"`
	FormulasAdopt  int `json:"formulas_adopt"`
	VesselsCreate  int `json:"vessels_create"`
	VesselsAdopt   int `json:"vessels_adopt"`
	RunsCreate     int `json:"runs_create"`
	RunsAdopt      int `json:"runs_adopt"`
	Conflicts      int `json:"conflicts"`
	Errors         int `json:"errors"`
	Warnings       int `json:"warnings"`
}

// isConflictIssue reports whether an issue blocks a plan because an
// entity name or code is already taken or drifted.
func isConflictIssue(kind string) bool {
	switch kind {
	case IssueNameConflict, IssueVesselConflict, IssueRunConflict,
		IssueDuplicateRef, IssueDrift:
		return true
	}
	return false
}

func (p *Plan) Counts() Counts {
	var counts Counts
	for _, formula := range p.Formulas {
		switch formula.Action {
		case ActionCreate:
			counts.FormulasCreate++
		case ActionAdopt:
			counts.FormulasAdopt++
		}
	}
	for _, vessel := range p.Vessels {
		switch vessel.Action {
		case ActionCreate:
			counts.VesselsCreate++
		case ActionAdopt:
			counts.VesselsAdopt++
		}
	}
	for _, run := range p.Runs {
		switch run.Action {
		case ActionCreate:
			counts.RunsCreate++
		case ActionAdopt:
			counts.RunsAdopt++
		}
	}
	// Every blocking finding lives in the plan's issue list exactly once;
	// classify it there rather than re-reading per-entity conflict slices.
	for _, issue := range p.Issues {
		switch {
		case issue.Severity == SeverityWarning:
			counts.Warnings++
		case isConflictIssue(issue.Kind):
			counts.Conflicts++
		default:
			counts.Errors++
		}
	}
	return counts
}

// Confirmable reports whether the plan can be applied without errors or
// conflicting entities.
func (p *Plan) Confirmable() bool {
	counts := p.Counts()
	return counts.Errors == 0 && counts.Conflicts == 0
}

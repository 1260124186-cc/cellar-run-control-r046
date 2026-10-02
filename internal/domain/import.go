package domain

import "time"

// ImportStatus is the lifecycle state of a season CSV import session.
type ImportStatus string

const (
	// ImportReady means precheck found no blocking issues and confirmation
	// is allowed.
	ImportReady ImportStatus = "ready"
	// ImportRejected means precheck found blocking issues; confirmation is
	// refused and nothing is created.
	ImportRejected ImportStatus = "rejected"
	// ImportCompleted means the whole planned set was committed in one
	// atomic snapshot revision.
	ImportCompleted ImportStatus = "completed"
)

// ImportKind identifies the entity kind an import row describes.
type ImportKind string

const (
	ImportFormula ImportKind = "formula"
	ImportVessel  ImportKind = "vessel"
	ImportRun     ImportKind = "run"
)

// ImportItemState tracks whether a planned entity has been committed.
type ImportItemState string

const (
	ImportItemPlanned ImportItemState = "planned"
	ImportItemCreated ImportItemState = "created"
)

// ImportIssue is a blocking precheck finding tied to a physical file line.
type ImportIssue struct {
	Line   int    `json:"line"`
	Field  string `json:"field,omitempty"`
	Code   Code   `json:"code"`
	Detail string `json:"detail"`
}

// ImportConflict reports a row whose business key already exists in the
// cellar. Conflicts are skipped on confirmation and do not block the import.
type ImportConflict struct {
	Line   int        `json:"line"`
	Entity ImportKind `json:"entity"`
	Key    string     `json:"key"`
	Detail string     `json:"detail"`
}

// ImportItem is one entity that the import would create or has created.
type ImportItem struct {
	Kind       ImportKind      `json:"kind"`
	Line       int             `json:"line"`
	Key        string          `json:"key"`
	EntityID   string          `json:"entity_id"`
	State      ImportItemState `json:"state"`
	Steps      int             `json:"step_count,omitempty"`
	Approved   bool            `json:"approved,omitempty"`
	FormulaKey string          `json:"formula,omitempty"`
}

// ImportCounts summarizes the rows the precheck resolved.
type ImportCounts struct {
	Formulas int `json:"formulas"`
	Steps    int `json:"steps"`
	Vessels  int `json:"vessels"`
	Runs     int `json:"runs"`
}

// ImportFormulaPlan is a validated formula row together with its step rows.
type ImportFormulaPlan struct {
	Line    int
	Key     string
	Formula Formula
	Steps   []FormulaStep
	Approve bool
}

// ImportVesselPlan is a validated vessel row.
type ImportVesselPlan struct {
	Line   int
	Vessel Vessel
}

// ImportRunPlan is a validated run row with its formula reference resolved.
// When the run references a formula already present in the cellar,
// ExistingFormulaID carries that formula's id; otherwise the reference points
// at one of the import's own FormulaPlans.
type ImportRunPlan struct {
	Line              int
	Run               FermentationRun
	FormulaKey        string
	ExistingFormulaID string
}

// ImportSession is the durable precheck/confirmation record for one uploaded
// file. It is stored in the snapshot so progress survives restarts.
type ImportSession struct {
	ID                string           `json:"id"`
	ContentHash       string           `json:"content_hash"`
	Filename          string           `json:"filename"`
	Status            ImportStatus     `json:"status"`
	BaseRevision      int64            `json:"base_revision"`
	CommittedRevision int64            `json:"committed_revision,omitempty"`
	Counts            ImportCounts     `json:"summary"`
	Issues            []ImportIssue    `json:"issues"`
	Conflicts         []ImportConflict `json:"conflicts"`
	Items             []ImportItem     `json:"items"`
	CreatedAt         time.Time        `json:"created_at"`
	UpdatedAt         time.Time        `json:"updated_at"`
	ConfirmedAt       *time.Time       `json:"confirmed_at,omitempty"`

	// FormulaPlans, VesselPlans and RunPlans carry the resolved, ID-bearing
	// entities so a confirmation that happens after a restart still applies
	// exactly the set advertised by precheck.
	FormulaPlans []ImportFormulaPlan `json:"formula_plans"`
	VesselPlans  []ImportVesselPlan  `json:"vessel_plans"`
	RunPlans     []ImportRunPlan     `json:"run_plans"`
}

// Clone returns a deep copy of the session, including its apply plans.
func (s ImportSession) Clone() ImportSession {
	cloned := s
	cloned.Issues = append([]ImportIssue(nil), s.Issues...)
	cloned.Conflicts = append([]ImportConflict(nil), s.Conflicts...)
	cloned.Items = append([]ImportItem(nil), s.Items...)
	cloned.FormulaPlans = make([]ImportFormulaPlan, len(s.FormulaPlans))
	for i, plan := range s.FormulaPlans {
		plan.Formula = plan.Formula.Clone()
		plan.Steps = append([]FormulaStep(nil), plan.Steps...)
		cloned.FormulaPlans[i] = plan
	}
	cloned.VesselPlans = append([]ImportVesselPlan(nil), s.VesselPlans...)
	cloned.RunPlans = make([]ImportRunPlan, len(s.RunPlans))
	for i, plan := range s.RunPlans {
		plan.Run = plan.Run.Clone()
		cloned.RunPlans[i] = plan
	}
	if s.ConfirmedAt != nil {
		value := *s.ConfirmedAt
		cloned.ConfirmedAt = &value
	}
	return cloned
}

package importplan

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/domain"
)

// CatalogFormula is the formula projection the planner needs from the
// current snapshot.
type CatalogFormula struct {
	ID                    string
	Name                  string
	Style                 string
	TargetOriginalGravity float64
	TargetFinalGravity    float64
	MaxFermentationTempC  float64
	MinimumDays           int
	MaximumDays           int
	State                 domain.FormulaState
	Steps                 []domain.FormulaStep
	Source                *domain.ImportSource
}

// CatalogVessel is the vessel projection the planner needs.
type CatalogVessel struct {
	ID        string
	Code      string
	CapacityL float64
	Source    *domain.ImportSource
}

// CatalogRun is the run projection the planner needs.
type CatalogRun struct {
	ID             string
	Code           string
	FormulaID      string
	VesselID       string
	PlannedVolumeL float64
	Notes          string
	Source         *domain.ImportSource
}

// Catalog is the current state the planner checks against.
type Catalog struct {
	Formulas []CatalogFormula
	Vessels  []CatalogVessel
	Runs     []CatalogRun
}

type formulaRef struct {
	planIndex int
	catalog   *CatalogFormula
}

type vesselRef struct {
	planIndex int
	catalog   *CatalogVessel
}

type planBuilder struct {
	importID string
	parsed   ParsedFile
	catalog  Catalog
	plan     Plan

	formulaByRef  map[string]formulaRef
	vesselByRef   map[string]vesselRef
	formulaByName map[string]formulaRef
	vesselByCode  map[string]vesselRef
}

// Build turns parsed rows into a line-annotated plan against the catalog.
// importID is the content hash of the file; it scopes deterministic entity
// ids and adopt decisions, so re-uploading the same bytes never creates a
// second round of entities. Entity ids embed the declaration position, not
// the create count, so an interrupted import resumes with the same ids.
func Build(importID string, parsed ParsedFile, catalog Catalog) Plan {
	builder := &planBuilder{
		importID:      importID,
		parsed:        parsed,
		catalog:       catalog,
		formulaByRef:  map[string]formulaRef{},
		vesselByRef:   map[string]vesselRef{},
		formulaByName: map[string]formulaRef{},
		vesselByCode:  map[string]vesselRef{},
	}
	builder.plan.Formulas = make([]FormulaItem, 0, len(parsed.Formulas))
	builder.plan.Vessels = make([]VesselItem, 0, len(parsed.Vessels))
	builder.plan.Runs = make([]RunItem, 0, len(parsed.Runs))
	builder.plan.Issues = append(builder.plan.Issues, parsed.Issues...)

	// Existing catalog entities are referenceable by name (formulas) or
	// code (vessels) even when this file does not redeclare them. File
	// declarations below overwrite the matching key when present.
	for index := range catalog.Formulas {
		formula := &catalog.Formulas[index]
		builder.formulaByName[domain.NameKey(formula.Name)] = formulaRef{catalog: formula}
	}
	for index := range catalog.Vessels {
		vessel := &catalog.Vessels[index]
		builder.vesselByCode[vessel.Code] = vesselRef{catalog: vessel}
	}

	builder.registerFormulas()
	builder.registerVessels()
	builder.bindSteps()
	builder.registerRuns()
	builder.validate()
	builder.sortItems()
	return builder.plan
}

// deterministicID derives a stable per-file entity id from the file hash,
// entity kind, and one-based declaration position within the file.
func deterministicID(importID, kind string, position int) string {
	prefix := importID
	if len(prefix) > 16 {
		prefix = prefix[:16]
	}
	return fmt.Sprintf("imp-%s-%s-%04d", prefix, kind, position)
}

func (b *planBuilder) registerFormulas() {
	seenRef := map[string]int{}
	seenName := map[string]int{}
	for position, row := range b.parsed.Formulas {
		index := len(b.plan.Formulas)
		b.plan.Formulas = append(b.plan.Formulas, FormulaItem{
			Line:  row.Line,
			Ref:   row.Ref,
			Name:  row.Name,
			Style: row.Style,
			State: row.Lifecycle,
		})
		if row.HasParseError {
			continue
		}
		b.plan.Formulas[index].ID = deterministicID(b.importID, "f", position+1)
		b.plan.Formulas[index].Action = ActionCreate

		duplicate := false
		if key := strings.ToLower(strings.TrimSpace(row.Ref)); key != "" {
			if first, ok := seenRef[key]; ok {
				duplicate = true
				b.conflictFormula(index, Issue{
					Line: row.Line, Kind: IssueDuplicateRef, Severity: SeverityError,
					Field: "ref", Ref: row.Ref,
					Detail: fmt.Sprintf("formula ref is also declared on line %d", first),
				})
			} else {
				seenRef[key] = row.Line
				b.formulaByRef[key] = formulaRef{planIndex: index}
			}
		}

		nameKey := domain.NameKey(row.Name)
		if first, ok := seenName[nameKey]; ok {
			duplicate = true
			b.conflictFormula(index, Issue{
				Line: row.Line, Kind: IssueNameConflict, Severity: SeverityError,
				Field: "name", Ref: row.Name,
				Detail: fmt.Sprintf("formula name is also declared on line %d", first),
			})
		} else {
			seenName[nameKey] = row.Line
			b.formulaByName[nameKey] = formulaRef{planIndex: index}
		}
		if duplicate {
			continue
		}

		if existing := b.findCatalogFormula(nameKey); existing != nil {
			b.formulaByName[nameKey] = formulaRef{catalog: existing}
			if key := strings.ToLower(strings.TrimSpace(row.Ref)); key != "" {
				b.formulaByRef[key] = formulaRef{catalog: existing}
			}
			b.plan.Formulas[index].ID = existing.ID
			if existing.Source != nil && existing.Source.ImportID == b.importID {
				b.plan.Formulas[index].Action = ActionAdopt
			} else {
				b.plan.Formulas[index].Action = ActionConflict
				b.conflictFormula(index, Issue{
					Line: row.Line, Kind: IssueNameConflict, Severity: SeverityError,
					Field: "name", Ref: row.Name,
					Detail: "a formula with the same normalized name already exists",
				})
			}
		}
	}
}

func (b *planBuilder) findCatalogFormula(nameKey string) *CatalogFormula {
	for index := range b.catalog.Formulas {
		formula := &b.catalog.Formulas[index]
		if domain.NameKey(formula.Name) == nameKey {
			return formula
		}
	}
	return nil
}

func (b *planBuilder) registerVessels() {
	seenRef := map[string]int{}
	seenCode := map[string]int{}
	for position, row := range b.parsed.Vessels {
		index := len(b.plan.Vessels)
		b.plan.Vessels = append(b.plan.Vessels, VesselItem{
			Line:      row.Line,
			Ref:       row.Ref,
			Code:      row.Code,
			CapacityL: row.CapacityL,
		})
		if row.HasParseError {
			continue
		}
		b.plan.Vessels[index].ID = deterministicID(b.importID, "v", position+1)
		b.plan.Vessels[index].Action = ActionCreate

		duplicate := false
		if key := strings.ToLower(strings.TrimSpace(row.Ref)); key != "" {
			if first, ok := seenRef[key]; ok {
				duplicate = true
				b.conflictVessel(index, Issue{
					Line: row.Line, Kind: IssueDuplicateRef, Severity: SeverityError,
					Field: "ref", Ref: row.Ref,
					Detail: fmt.Sprintf("vessel ref is also declared on line %d", first),
				})
			} else {
				seenRef[key] = row.Line
				b.vesselByRef[key] = vesselRef{planIndex: index}
			}
		}

		if first, ok := seenCode[row.Code]; ok {
			duplicate = true
			b.conflictVessel(index, Issue{
				Line: row.Line, Kind: IssueVesselConflict, Severity: SeverityError,
				Field: "vessel_code", Ref: row.Code,
				Detail: fmt.Sprintf("vessel code is also declared on line %d", first),
			})
		} else {
			seenCode[row.Code] = row.Line
			b.vesselByCode[row.Code] = vesselRef{planIndex: index}
		}
		if duplicate {
			continue
		}

		if existing := b.findCatalogVessel(row.Code); existing != nil {
			b.vesselByCode[row.Code] = vesselRef{catalog: existing}
			if key := strings.ToLower(strings.TrimSpace(row.Ref)); key != "" {
				b.vesselByRef[key] = vesselRef{catalog: existing}
			}
			b.plan.Vessels[index].ID = existing.ID
			if existing.Source != nil && existing.Source.ImportID == b.importID {
				b.plan.Vessels[index].Action = ActionAdopt
			} else {
				b.plan.Vessels[index].Action = ActionConflict
				b.conflictVessel(index, Issue{
					Line: row.Line, Kind: IssueVesselConflict, Severity: SeverityError,
					Field: "vessel_code", Ref: row.Code,
					Detail: "a vessel with the same code already exists",
				})
			}
		}
	}
}

func (b *planBuilder) findCatalogVessel(code string) *CatalogVessel {
	for index := range b.catalog.Vessels {
		vessel := &b.catalog.Vessels[index]
		if vessel.Code == code {
			return vessel
		}
	}
	return nil
}

func (b *planBuilder) bindSteps() {
	grouped := map[string][]domain.FormulaStep{}
	reported := map[string]bool{}
	for _, row := range b.parsed.Steps {
		if row.HasParseError {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(row.FormulaRef))
		if _, ok := b.formulaByRef[key]; !ok {
			if !reported[key] {
				reported[key] = true
				b.addIssue(row.Line, IssueReference, "ref", row.FormulaRef,
					"step references a formula ref that is not declared in this file")
			}
			continue
		}
		grouped[key] = append(grouped[key], domain.FormulaStep{
			Sequence:     row.Sequence,
			Label:        row.Label,
			TemperatureC: row.TemperatureC,
			HoldMinutes:  row.HoldMinutes,
		})
	}

	for index := range b.plan.Formulas {
		formula := &b.plan.Formulas[index]
		if formula.Ref == "" {
			continue
		}
		steps := grouped[strings.ToLower(strings.TrimSpace(formula.Ref))]
		sort.Slice(steps, func(i, j int) bool { return steps[i].Sequence < steps[j].Sequence })
		formula.Steps = steps
	}
}

func (b *planBuilder) registerRuns() {
	catalogRunByCode := map[string]*CatalogRun{}
	for index := range b.catalog.Runs {
		run := &b.catalog.Runs[index]
		catalogRunByCode[run.Code] = run
	}

	seenCode := map[string]int{}
	for position, row := range b.parsed.Runs {
		index := len(b.plan.Runs)
		b.plan.Runs = append(b.plan.Runs, RunItem{
			Line:           row.Line,
			Code:           row.Code,
			FormulaRef:     row.FormulaRef,
			VesselRef:      row.VesselRef,
			PlannedVolumeL: row.PlannedVolumeL,
			Notes:          row.Notes,
		})
		if row.HasParseError {
			continue
		}
		b.plan.Runs[index].ID = deterministicID(b.importID, "r", position+1)
		b.plan.Runs[index].Action = ActionCreate

		if first, ok := seenCode[row.Code]; ok {
			b.plan.Runs[index].Action = ActionConflict
			b.conflictRun(index, Issue{
				Line: row.Line, Kind: IssueRunConflict, Severity: SeverityError,
				Field: "run_code", Ref: row.Code,
				Detail: fmt.Sprintf("run code is also declared on line %d", first),
			})
			continue
		}
		seenCode[row.Code] = row.Line

		formula := b.resolveFormula(row.FormulaRef, row.Line)
		if formula != nil {
			if formula.catalog != nil {
				b.plan.Runs[index].FormulaID = formula.catalog.ID
				if formula.catalog.State != domain.FormulaApproved {
					b.addIssue(row.Line, IssueFormulaState, "run_formula", row.FormulaRef,
						"the referenced existing formula is not approved")
				}
			} else {
				item := &b.plan.Formulas[formula.planIndex]
				b.plan.Runs[index].FormulaID = item.ID
				if item.State != domain.FormulaApproved {
					b.addIssue(row.Line, IssueFormulaState, "run_formula", row.FormulaRef,
						"the referenced formula must be approved in this file")
				}
			}
		}

		if row.HasVesselRef {
			if vessel := b.resolveVessel(row.VesselRef, row.Line); vessel != nil {
				capacity := 0.0
				if vessel.catalog != nil {
					b.plan.Runs[index].VesselID = vessel.catalog.ID
					capacity = vessel.catalog.CapacityL
				} else {
					item := &b.plan.Vessels[vessel.planIndex]
					b.plan.Runs[index].VesselID = item.ID
					capacity = item.CapacityL
				}
				if capacity > 0 && row.PlannedVolumeL > capacity {
					b.addIssue(row.Line, IssueCapacity, "planned_volume_liters", row.VesselRef,
						"planned volume exceeds the referenced vessel capacity")
				}
			}
		}

		if existing := catalogRunByCode[row.Code]; existing != nil {
			b.plan.Runs[index].ID = existing.ID
			sameImport := existing.Source != nil && existing.Source.ImportID == b.importID &&
				existing.Source.Line == row.Line
			if sameImport {
				b.plan.Runs[index].Action = ActionAdopt
			} else {
				b.plan.Runs[index].Action = ActionConflict
				b.conflictRun(index, Issue{
					Line: row.Line, Kind: IssueRunConflict, Severity: SeverityError,
					Field: "run_code", Ref: row.Code,
					Detail: "a run with the same code already exists",
				})
			}
		}
	}
}

func (b *planBuilder) resolveFormula(ref string, line int) *formulaRef {
	key := strings.ToLower(strings.TrimSpace(ref))
	if resolution, ok := b.formulaByRef[key]; ok {
		return &resolution
	}
	if resolution, ok := b.formulaByName[domain.NameKey(ref)]; ok {
		return &resolution
	}
	b.addIssue(line, IssueReference, "run_formula", ref,
		"formula reference does not match a ref or name in the file or an existing formula")
	return nil
}

func (b *planBuilder) resolveVessel(ref string, line int) *vesselRef {
	key := strings.ToLower(strings.TrimSpace(ref))
	if resolution, ok := b.vesselByRef[key]; ok {
		return &resolution
	}
	if resolution, ok := b.vesselByCode[strings.ToUpper(strings.TrimSpace(ref))]; ok {
		return &resolution
	}
	b.addIssue(line, IssueReference, "run_vessel", ref,
		"vessel reference does not match a ref or code in the file or an existing vessel")
	return nil
}

func (b *planBuilder) validate() {
	for index := range b.plan.Formulas {
		item := &b.plan.Formulas[index]
		if item.Action == ActionConflict || item.ID == "" {
			continue
		}
		row := b.parsed.Formulas[index]
		formula := domain.Formula{
			Name:                  row.Name,
			Style:                 row.Style,
			TargetOriginalGravity: row.TargetOriginalGravity,
			TargetFinalGravity:    row.TargetFinalGravity,
			MaxFermentationTempC:  row.MaxFermentationTempC,
			MinimumDays:           row.MinimumDays,
			MaximumDays:           row.MaximumDays,
			Steps:                 item.Steps,
			State:                 row.Lifecycle,
		}
		formula.Normalize()
		b.reportDomainError(row.Line, formula.Validate())
		if row.Lifecycle == domain.FormulaDraft {
			b.reportDomainError(row.Line, domain.ValidateFormulaSteps(item.Steps, false))
		}

		if item.Action == ActionAdopt {
			existing := b.findCatalogFormula(domain.NameKey(row.Name))
			if existing != nil && !formulaMatchesCatalog(formula, existing) {
				b.conflictFormula(index, Issue{
					Line: row.Line, Kind: IssueDrift, Severity: SeverityError,
					Field: "name", Ref: row.Name,
					Detail: "the formula introduced by this file changed since the original import",
				})
			}
		}
	}

	for index := range b.plan.Vessels {
		item := &b.plan.Vessels[index]
		if item.Action == ActionConflict || item.ID == "" {
			continue
		}
		row := b.parsed.Vessels[index]
		vessel := domain.Vessel{Code: row.Code, CapacityL: row.CapacityL}
		b.reportDomainError(row.Line, vessel.Validate())

		if item.Action == ActionAdopt {
			existing := b.findCatalogVessel(row.Code)
			if existing != nil && !floatEqual(existing.CapacityL, row.CapacityL) {
				b.conflictVessel(index, Issue{
					Line: row.Line, Kind: IssueDrift, Severity: SeverityError,
					Field: "vessel_capacity_liters", Ref: row.Code,
					Detail: "the vessel introduced by this file changed since the original import",
				})
			}
		}
	}

	for index := range b.plan.Runs {
		item := &b.plan.Runs[index]
		if item.Action == ActionConflict || item.ID == "" {
			continue
		}
		row := b.parsed.Runs[index]
		run := domain.FermentationRun{
			Code:           row.Code,
			PlannedVolumeL: row.PlannedVolumeL,
			Notes:          row.Notes,
		}
		b.reportDomainError(row.Line, run.Validate())

		if item.Action == ActionAdopt {
			existing := b.findCatalogRun(row.Code)
			if existing != nil && (existing.FormulaID != item.FormulaID ||
				existing.VesselID != item.VesselID ||
				!floatEqual(existing.PlannedVolumeL, row.PlannedVolumeL) ||
				existing.Notes != row.Notes) {
				b.conflictRun(index, Issue{
					Line: row.Line, Kind: IssueDrift, Severity: SeverityError,
					Field: "run_code", Ref: row.Code,
					Detail: "the run introduced by this file changed since the original import",
				})
			}
		}
	}
}

func (b *planBuilder) findCatalogRun(code string) *CatalogRun {
	for index := range b.catalog.Runs {
		run := &b.catalog.Runs[index]
		if run.Code == code {
			return run
		}
	}
	return nil
}

func formulaMatchesCatalog(file domain.Formula, existing *CatalogFormula) bool {
	if file.Name != existing.Name || file.Style != existing.Style ||
		file.State != existing.State ||
		!floatEqual(file.TargetOriginalGravity, existing.TargetOriginalGravity) ||
		!floatEqual(file.TargetFinalGravity, existing.TargetFinalGravity) ||
		!floatEqual(file.MaxFermentationTempC, existing.MaxFermentationTempC) ||
		file.MinimumDays != existing.MinimumDays ||
		file.MaximumDays != existing.MaximumDays {
		return false
	}
	if len(file.Steps) != len(existing.Steps) {
		return false
	}
	for index := range file.Steps {
		if file.Steps[index] != existing.Steps[index] {
			return false
		}
	}
	return true
}

func (b *planBuilder) reportDomainError(line int, err error) {
	if err == nil {
		return
	}
	var domainErr *domain.Error
	if errors.As(err, &domainErr) {
		b.addIssue(line, IssueInvalidRow, domainErr.Field, "", domainErr.Message)
		return
	}
	b.addIssue(line, IssueInvalidRow, "", "", err.Error())
}

func (b *planBuilder) addIssue(line int, kind, field, ref, detail string) {
	b.plan.Issues = append(b.plan.Issues, Issue{
		Line: line, Kind: kind, Severity: SeverityError,
		Field: field, Ref: ref, Detail: detail,
	})
}

func (b *planBuilder) conflictFormula(index int, issue Issue) {
	b.plan.Formulas[index].Action = ActionConflict
	b.plan.Formulas[index].Conflicts = append(b.plan.Formulas[index].Conflicts, issue)
	b.plan.Issues = append(b.plan.Issues, issue)
}

func (b *planBuilder) conflictVessel(index int, issue Issue) {
	b.plan.Vessels[index].Action = ActionConflict
	b.plan.Vessels[index].Conflicts = append(b.plan.Vessels[index].Conflicts, issue)
	b.plan.Issues = append(b.plan.Issues, issue)
}

func (b *planBuilder) conflictRun(index int, issue Issue) {
	b.plan.Runs[index].Action = ActionConflict
	b.plan.Runs[index].Conflicts = append(b.plan.Runs[index].Conflicts, issue)
	b.plan.Issues = append(b.plan.Issues, issue)
}

func (b *planBuilder) sortItems() {
	sort.SliceStable(b.plan.Issues, func(i, j int) bool {
		if b.plan.Issues[i].Line == b.plan.Issues[j].Line {
			if b.plan.Issues[i].Field == b.plan.Issues[j].Field {
				return b.plan.Issues[i].Detail < b.plan.Issues[j].Detail
			}
			return b.plan.Issues[i].Field < b.plan.Issues[j].Field
		}
		return b.plan.Issues[i].Line < b.plan.Issues[j].Line
	})
}

func floatEqual(a, b float64) bool {
	difference := a - b
	if difference < 0 {
		difference = -difference
	}
	return difference <= 1e-9
}

// Package importplan parses the season CSV upload and builds a validated
// import plan: entities that would be created, keys that already conflict, and
// blocking issues keyed by physical file line.
package importplan

import (
	"bytes"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/domain"
)

// RequiredColumns is the exact header set a season CSV must declare.
var RequiredColumns = []string{
	"kind",
	"name",
	"style",
	"target_original_gravity",
	"target_final_gravity",
	"max_fermentation_temp_c",
	"minimum_days",
	"maximum_days",
	"approved",
	"capacity_liters",
	"code",
	"formula",
	"planned_volume_liters",
	"notes",
	"sequence",
	"label",
	"temperature_c",
	"hold_minutes",
}

// ExistingFormula describes one formula already present in the cellar.
type ExistingFormula struct {
	ID       string
	Approved bool
}

// CurrentState exposes the business keys the precheck must resolve against.
type CurrentState struct {
	// FormulaNames maps a normalized name key to the stored formula.
	FormulaNames map[string]ExistingFormula
	VesselCodes  map[string]struct{}
	RunCodes     map[string]struct{}
}

// NormalizeContent strips one leading UTF-8 BOM so byte-identical content
// uploaded by different clients hashes to the same key.
func NormalizeContent(content []byte) []byte {
	return bytes.TrimPrefix(content, []byte{0xEF, 0xBB, 0xBF})
}

// Build parses and prechecks the supplied CSV content. It never returns a
// partial plan: malformed rows become issues, and plans are only produced for
// rows that validate cleanly. IDs for new entities are generated through
// newID so a later confirm applies exactly the advertised entities.
func Build(
	content []byte,
	state CurrentState,
	newID func(prefix string) (string, error),
) (domain.ImportSession, error) {
	session := domain.ImportSession{
		Status:    domain.ImportReady,
		Issues:    []domain.ImportIssue{},
		Conflicts: []domain.ImportConflict{},
		Items:     []domain.ImportItem{},
	}

	byFormula := map[string]*formulaEntry{}
	vesselFileCodes := map[string]int{}
	runFileCodes := map[string]int{}
	stepRows := map[string][]stepRow{}
	var vessels []domain.ImportVesselPlan
	var runs []domain.ImportRunPlan

	reader := csv.NewReader(bytes.NewReader(content))
	reader.FieldsPerRecord = -1

	header, err := reader.Read()
	if err != nil {
		if errors.Is(err, io.EOF) {
			addIssue(&session, 1, "", "file must contain a header row and at least one data row")
			return finalize(session, nil, nil, nil), nil
		}
		addIssue(&session, 1, "", fmt.Sprintf("cannot read header: %v", err))
		return finalize(session, nil, nil, nil), nil
	}

	columns, headerOK := mapColumns(header, &session)
	if !headerOK {
		return finalize(session, nil, nil, nil), nil
	}

	for {
		record, readErr := reader.Read()
		if errors.Is(readErr, io.EOF) {
			break
		}
		line, _ := reader.FieldPos(0)
		if readErr != nil {
			addIssue(&session, line, "", fmt.Sprintf("cannot parse CSV row: %v", readErr))
			continue
		}
		if len(record) != len(columns) {
			addIssue(&session, line, "kind",
				fmt.Sprintf("row has %d fields but the header declares %d", len(record), len(columns)))
			continue
		}
		get := func(name string) string { return strings.TrimSpace(record[columns[name]]) }
		switch kind := strings.ToLower(get("kind")); kind {
		case string(domain.ImportFormula):
			parseFormula(&session, line, get, byFormula, state)
		case string(domain.ImportVessel):
			parseVessel(&session, line, get, &vessels, vesselFileCodes, state)
		case "step":
			collectStep(&session, line, get, stepRows)
		case string(domain.ImportRun):
			parseRun(&session, line, get, &runs, runFileCodes, state)
		default:
			addIssue(&session, line, "kind",
				"must be one of formula, step, vessel, run")
		}
	}

	resolveRunReferences(&session, runs, byFormula, state)

	// Stable IDs for every new formula before runs resolve their references.
	for _, entry := range byFormula {
		if entry.storage {
			continue
		}
		id, err := newID("formula")
		if err != nil {
			return domain.ImportSession{}, err
		}
		entry.id = id
		entry.plan.Formula.ID = id
		entry.item.EntityID = id
	}

	formulaPlans := resolveSteps(&session, byFormula, stepRows)

	for i := range vessels {
		id, err := newID("vessel")
		if err != nil {
			return domain.ImportSession{}, err
		}
		vessels[i].Vessel.ID = id
	}
	for i := range runs {
		id, err := newID("run")
		if err != nil {
			return domain.ImportSession{}, err
		}
		runs[i].Run.ID = id
		switch {
		case runs[i].ExistingFormulaID != "":
			runs[i].Run.FormulaID = runs[i].ExistingFormulaID
		default:
			key := domain.NameKey(runs[i].FormulaKey)
			if resolved, ok := byFormula[key]; ok {
				if resolved.storage {
					runs[i].Run.FormulaID = resolved.storageID
				} else {
					runs[i].Run.FormulaID = resolved.id
				}
			}
		}
	}

	// Items in deterministic creation order: formulas, vessels, then runs.
	var newFormulaKeys []string
	for key, entry := range byFormula {
		if !entry.storage {
			newFormulaKeys = append(newFormulaKeys, key)
		}
	}
	sort.Slice(newFormulaKeys, func(i, j int) bool {
		return byFormula[newFormulaKeys[i]].line < byFormula[newFormulaKeys[j]].line
	})
	for _, key := range newFormulaKeys {
		session.Items = append(session.Items, byFormula[key].item)
	}
	sort.Slice(vessels, func(i, j int) bool { return vessels[i].Line < vessels[j].Line })
	for _, plan := range vessels {
		session.Items = append(session.Items, domain.ImportItem{
			Kind:     domain.ImportVessel,
			Line:     plan.Line,
			Key:      plan.Vessel.Code,
			EntityID: plan.Vessel.ID,
			State:    domain.ImportItemPlanned,
		})
	}
	sort.Slice(runs, func(i, j int) bool { return runs[i].Line < runs[j].Line })
	for _, plan := range runs {
		session.Items = append(session.Items, domain.ImportItem{
			Kind:       domain.ImportRun,
			Line:       plan.Line,
			Key:        plan.Run.Code,
			EntityID:   plan.Run.ID,
			State:      domain.ImportItemPlanned,
			FormulaKey: plan.FormulaKey,
		})
	}

	stepCount := 0
	for _, plan := range formulaPlans {
		stepCount += len(plan.Steps)
	}
	session.Counts = domain.ImportCounts{
		Formulas: len(newFormulaKeys),
		Steps:    stepCount,
		Vessels:  len(vessels),
		Runs:     len(runs),
	}

	return finalize(session, formulaPlans, vessels, runs), nil
}

type formulaEntry struct {
	line      int
	key       string
	storage   bool
	storageID string
	plan      domain.ImportFormulaPlan
	item      domain.ImportItem
	id        string
}

type stepRow struct {
	line        int
	formulaName string
	sequence    int
	label       string
	temperature float64
	hold        int
}

func mapColumns(header []string, session *domain.ImportSession) (map[string]int, bool) {
	columns := map[string]int{}
	ok := true
	for index, raw := range header {
		name := strings.ToLower(strings.TrimSpace(raw))
		if !knownColumn(name) {
			addIssue(session, 1, name, "unknown CSV column")
			ok = false
			continue
		}
		if _, duplicated := columns[name]; duplicated {
			addIssue(session, 1, name, "CSV header is declared more than once")
			ok = false
			continue
		}
		columns[name] = index
	}
	for _, required := range RequiredColumns {
		if _, found := columns[required]; !found {
			addIssue(session, 1, required, "required CSV column is missing")
			ok = false
		}
	}
	return columns, ok
}

func knownColumn(name string) bool {
	for _, column := range RequiredColumns {
		if column == name {
			return true
		}
	}
	return false
}

func parseFormula(
	session *domain.ImportSession,
	line int,
	get func(string) string,
	byKey map[string]*formulaEntry,
	state CurrentState,
) {
	name := domain.NormalizeName(get("name"))
	style := domain.NormalizeName(get("style"))
	og := parseFloatOrZero(session, line, "target_original_gravity", get("target_original_gravity"))
	fg := parseFloatOrZero(session, line, "target_final_gravity", get("target_final_gravity"))
	maxTemp := parseFloatOrZero(session, line, "max_fermentation_temp_c", get("max_fermentation_temp_c"))
	minDays := parseIntOrZero(session, line, "minimum_days", get("minimum_days"))
	maxDays := parseIntOrZero(session, line, "maximum_days", get("maximum_days"))
	approved := parseApproved(session, line, get("approved"))

	key := domain.NameKey(name)
	entry := &formulaEntry{
		line: line,
		key:  key,
		plan: domain.ImportFormulaPlan{
			Line:    line,
			Key:     key,
			Approve: approved,
			Formula: domain.Formula{
				Name:                  name,
				Style:                 style,
				TargetOriginalGravity: og,
				TargetFinalGravity:    fg,
				MaxFermentationTempC:  maxTemp,
				MinimumDays:           minDays,
				MaximumDays:           maxDays,
				Steps:                 []domain.FormulaStep{},
				State:                 domain.FormulaDraft,
			},
		},
		item: domain.ImportItem{
			Kind:     domain.ImportFormula,
			Line:     line,
			Key:      name,
			State:    domain.ImportItemPlanned,
			Approved: approved,
		},
	}
	if err := entry.plan.Formula.Validate(); err != nil {
		addDomainIssue(session, line, err)
	}
	if key == "" {
		return
	}
	if existing, duplicated := byKey[key]; duplicated {
		addIssue(session, line, "name",
			fmt.Sprintf("duplicate formula name in file; first declared on line %d", existing.line))
		return
	}
	if stored, found := state.FormulaNames[key]; found {
		entry.storage = true
		entry.storageID = stored.ID
		session.Conflicts = append(session.Conflicts, domain.ImportConflict{
			Line:   line,
			Entity: domain.ImportFormula,
			Key:    name,
			Detail: "a formula with the same normalized name already exists",
		})
	}
	byKey[key] = entry
}

func parseVessel(
	session *domain.ImportSession,
	line int,
	get func(string) string,
	plans *[]domain.ImportVesselPlan,
	inFile map[string]int,
	state CurrentState,
) {
	code := domain.NormalizeVesselCode(get("code"))
	capacity := parseFloatOrZero(session, line, "capacity_liters", get("capacity_liters"))
	vessel := domain.Vessel{
		Code:      code,
		CapacityL: capacity,
		State:     domain.VesselAvailable,
	}
	if err := vessel.Validate(); err != nil {
		addDomainIssue(session, line, err)
	}
	if code == "" {
		return
	}
	if firstLine, duplicated := inFile[code]; duplicated {
		addIssue(session, line, "code",
			fmt.Sprintf("duplicate vessel code in file; first declared on line %d", firstLine))
		return
	}
	inFile[code] = line
	if _, found := state.VesselCodes[code]; found {
		session.Conflicts = append(session.Conflicts, domain.ImportConflict{
			Line:   line,
			Entity: domain.ImportVessel,
			Key:    code,
			Detail: "a vessel with the same code already exists",
		})
		return
	}
	*plans = append(*plans, domain.ImportVesselPlan{Line: line, Vessel: vessel})
}

func collectStep(
	session *domain.ImportSession,
	line int,
	get func(string) string,
	rows map[string][]stepRow,
) {
	formulaName := domain.NormalizeName(get("formula"))
	if formulaName == "" {
		addIssue(session, line, "formula", "step must name its formula")
		return
	}
	rows[domain.NameKey(formulaName)] = append(rows[domain.NameKey(formulaName)], stepRow{
		line:        line,
		formulaName: formulaName,
		sequence:    parseIntOrZero(session, line, "sequence", get("sequence")),
		label:       domain.NormalizeName(get("label")),
		temperature: parseFloatOrZero(session, line, "temperature_c", get("temperature_c")),
		hold:        parseIntOrZero(session, line, "hold_minutes", get("hold_minutes")),
	})
}

func parseRun(
	session *domain.ImportSession,
	line int,
	get func(string) string,
	plans *[]domain.ImportRunPlan,
	inFile map[string]int,
	state CurrentState,
) {
	code := domain.NormalizeRunCode(get("code"))
	formulaName := domain.NormalizeName(get("formula"))
	volume := parseFloatOrZero(session, line, "planned_volume_liters", get("planned_volume_liters"))
	notes := get("notes")
	run := domain.FermentationRun{
		Code:           code,
		PlannedVolumeL: volume,
		Notes:          notes,
		State:          domain.RunPlanned,
		Observations:   []domain.GravityObservation{},
	}
	if err := run.Validate(); err != nil {
		addDomainIssue(session, line, err)
	}
	keepCode := code != ""
	if code != "" {
		if firstLine, duplicated := inFile[code]; duplicated {
			addIssue(session, line, "code",
				fmt.Sprintf("duplicate run code in file; first declared on line %d", firstLine))
			keepCode = false
		} else if _, found := state.RunCodes[code]; found {
			session.Conflicts = append(session.Conflicts, domain.ImportConflict{
				Line:   line,
				Entity: domain.ImportRun,
				Key:    code,
				Detail: "a run with the same code already exists",
			})
			keepCode = false
		}
		inFile[code] = line
	}
	if formulaName == "" {
		addIssue(session, line, "formula", "run must name its formula")
	}
	if keepCode {
		*plans = append(*plans, domain.ImportRunPlan{
			Line:       line,
			Run:        run,
			FormulaKey: formulaName,
		})
	}
}

// resolveRunReferences validates each run's formula reference against both the
// cellar and the formulas declared in the same file, independent of row order,
// and records the existing formula id when applicable.
func resolveRunReferences(
	session *domain.ImportSession,
	plans []domain.ImportRunPlan,
	byFormula map[string]*formulaEntry,
	state CurrentState,
) {
	for index := range plans {
		plan := &plans[index]
		key := domain.NameKey(plan.FormulaKey)
		if key == "" {
			continue
		}
		if stored, found := state.FormulaNames[key]; found {
			plan.ExistingFormulaID = stored.ID
			if !stored.Approved {
				addIssue(session, plan.Line, "formula",
					"referenced formula exists but is not approved")
			}
			continue
		}
		if inFile, found := byFormula[key]; found {
			if inFile.storage {
				// The formula row was reported as a conflict; its stored
				// record was covered by the state lookup above.
				continue
			}
			if !inFile.plan.Approve {
				addIssue(session, plan.Line, "formula",
					"referenced formula is imported as a draft; set approved=true and supply steps before it can run")
			}
			continue
		}
		addIssue(session, plan.Line, "formula",
			fmt.Sprintf("no formula named %q exists in the cellar or in this file", plan.FormulaKey))
	}
}

func resolveSteps(
	session *domain.ImportSession,
	byKey map[string]*formulaEntry,
	rows map[string][]stepRow,
) []domain.ImportFormulaPlan {
	var keys []string
	for key := range byKey {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		return byKey[keys[i]].line < byKey[keys[j]].line
	})

	var plans []domain.ImportFormulaPlan
	for _, key := range keys {
		entry := byKey[key]
		steps := rows[key]
		sort.SliceStable(steps, func(i, j int) bool { return steps[i].line < steps[j].line })
		if entry.storage {
			for _, row := range steps {
				addIssue(session, row.line, "formula",
					"steps cannot be attached to a formula that already exists")
			}
			continue
		}
		built := make([]domain.FormulaStep, 0, len(steps))
		for _, row := range steps {
			if row.sequence != len(built)+1 {
				addIssue(session, row.line, "sequence",
					fmt.Sprintf("step sequences must be contiguous and begin at 1; expected %d",
						len(built)+1))
			}
			built = append(built, domain.FormulaStep{
				Sequence:     row.sequence,
				Label:        row.label,
				TemperatureC: row.temperature,
				HoldMinutes:  row.hold,
			})
		}
		required := entry.plan.Approve
		if err := domain.ValidateFormulaSteps(built, required); err != nil {
			targetLine := entry.line
			if len(steps) > 0 {
				targetLine = steps[0].line
			}
			addDomainIssue(session, targetLine, err)
		}
		entry.plan.Steps = built
		entry.item.Steps = len(built)
		plans = append(plans, entry.plan)
	}
	for key, steps := range rows {
		if _, known := byKey[key]; known {
			continue
		}
		for _, row := range steps {
			addIssue(session, row.line, "formula",
				fmt.Sprintf("no formula row named %q in this file", row.formulaName))
		}
	}
	return plans
}

func finalize(
	session domain.ImportSession,
	formulaPlans []domain.ImportFormulaPlan,
	vesselPlans []domain.ImportVesselPlan,
	runPlans []domain.ImportRunPlan,
) domain.ImportSession {
	sort.SliceStable(session.Issues, func(i, j int) bool {
		if session.Issues[i].Line != session.Issues[j].Line {
			return session.Issues[i].Line < session.Issues[j].Line
		}
		return session.Issues[i].Field < session.Issues[j].Field
	})
	sort.SliceStable(session.Conflicts, func(i, j int) bool {
		return session.Conflicts[i].Line < session.Conflicts[j].Line
	})
	if len(session.Issues) > 0 {
		session.Status = domain.ImportRejected
	}
	session.FormulaPlans = orEmptyFormulaPlans(formulaPlans)
	session.VesselPlans = orEmptyVesselPlans(vesselPlans)
	session.RunPlans = orEmptyRunPlans(runPlans)
	return session
}

func orEmptyFormulaPlans(plans []domain.ImportFormulaPlan) []domain.ImportFormulaPlan {
	if plans == nil {
		return []domain.ImportFormulaPlan{}
	}
	return plans
}

func orEmptyVesselPlans(plans []domain.ImportVesselPlan) []domain.ImportVesselPlan {
	if plans == nil {
		return []domain.ImportVesselPlan{}
	}
	return plans
}

func orEmptyRunPlans(plans []domain.ImportRunPlan) []domain.ImportRunPlan {
	if plans == nil {
		return []domain.ImportRunPlan{}
	}
	return plans
}

func addIssue(session *domain.ImportSession, line int, field, detail string) {
	session.Issues = append(session.Issues, domain.ImportIssue{
		Line:   line,
		Field:  field,
		Code:   domain.CodeInvalidInput,
		Detail: detail,
	})
}

func addDomainIssue(session *domain.ImportSession, line int, err error) {
	var domainErr *domain.Error
	if errors.As(err, &domainErr) {
		session.Issues = append(session.Issues, domain.ImportIssue{
			Line:   line,
			Field:  domainErr.Field,
			Code:   domainErr.Code,
			Detail: domainErr.Message,
		})
		return
	}
	addIssue(session, line, "", err.Error())
}

// parseFloatOrZero records a syntax error but lets domain range validation
// report empty or out-of-range cells with the proper field name.
func parseFloatOrZero(session *domain.ImportSession, line int, field, raw string) float64 {
	value := strings.TrimSpace(raw)
	if value == "" {
		return 0
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) {
		addIssue(session, line, field, "must be a number")
		return 0
	}
	return parsed
}

// parseIntOrZero mirrors parseFloatOrZero for integer columns.
func parseIntOrZero(session *domain.ImportSession, line int, field, raw string) int {
	value := strings.TrimSpace(raw)
	if value == "" {
		return 0
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		addIssue(session, line, field, "must be an integer")
		return 0
	}
	return parsed
}

func parseApproved(session *domain.ImportSession, line int, raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "false":
		return false
	case "true":
		return true
	default:
		addIssue(session, line, "approved", "must be true or false")
		return false
	}
}

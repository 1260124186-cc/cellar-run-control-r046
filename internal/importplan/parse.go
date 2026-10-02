package importplan

import (
	"bytes"
	"encoding/csv"
	"io"
	"math"
	"strconv"
	"strings"

	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/domain"
)

// HeaderColumns is the fixed, ordered CSV header every seasonal file must
// carry. Each row uses the columns relevant to its kind.
var HeaderColumns = []string{
	"kind",
	"ref",
	"name",
	"style",
	"target_original_gravity",
	"target_final_gravity",
	"max_fermentation_temp_c",
	"minimum_days",
	"maximum_days",
	"lifecycle",
	"step_sequence",
	"step_label",
	"step_temperature_c",
	"step_hold_minutes",
	"vessel_code",
	"vessel_capacity_liters",
	"run_code",
	"run_formula",
	"run_vessel",
	"planned_volume_liters",
	"notes",
}

const (
	colKind = iota
	colRef
	colName
	colStyle
	colTargetOG
	colTargetFG
	colMaxTemp
	colMinDays
	colMaxDays
	colLifecycle
	colStepSeq
	colStepLabel
	colStepTemp
	colStepHold
	colVesselCode
	colVesselCap
	colRunCode
	colRunFormula
	colRunVessel
	colRunVolume
	colNotes
)

type rowScanner struct {
	issues []Issue
}

func (r *rowScanner) fail(line int, kind, field, detail string) {
	r.issues = append(r.issues, Issue{
		Line:     line,
		Kind:     kind,
		Severity: SeverityError,
		Field:    field,
		Detail:   detail,
	})
}

func textField(record []string, column int) string {
	return strings.TrimSpace(record[column])
}

func (r *rowScanner) requiredText(record []string, column int, field string, line int) (string, bool) {
	value := textField(record, column)
	if value == "" {
		r.fail(line, IssueInvalidRow, field, "is required")
		return "", false
	}
	return value, true
}

func (r *rowScanner) parseFloat(record []string, column int, field string, line int) (float64, bool) {
	value, ok := r.requiredText(record, column, field, line)
	if !ok {
		return 0, false
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) {
		r.fail(line, IssueInvalidRow, field, "must be a finite number")
		return 0, false
	}
	return parsed, true
}

func (r *rowScanner) parseInt(record []string, column int, field string, line int) (int, bool) {
	value, ok := r.requiredText(record, column, field, line)
	if !ok {
		return 0, false
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		r.fail(line, IssueInvalidRow, field, "must be an integer")
		return 0, false
	}
	return parsed, true
}

// Parse decodes a seasonal CSV bundle. Structural problems (bad header,
// malformed records, unparseable values) become issues anchored at the
// physical file line. Rows with parse errors are still returned so later
// cross-row checks can report duplicate and dangling references.
func Parse(content []byte) ParsedFile {
	parsed := ParsedFile{}
	trimmed := bytes.TrimPrefix(content, []byte{0xEF, 0xBB, 0xBF})
	if len(bytes.TrimSpace(trimmed)) == 0 {
		parsed.Issues = append(parsed.Issues, Issue{
			Line:     1,
			Kind:     IssueEmptyFile,
			Severity: SeverityError,
			Detail:   "the CSV file contains no rows",
		})
		return parsed
	}

	reader := csv.NewReader(bytes.NewReader(trimmed))
	reader.FieldsPerRecord = -1
	reader.LazyQuotes = true
	reader.TrimLeadingSpace = true

	header, err := reader.Read()
	if err != nil {
		parsed.Issues = append(parsed.Issues, Issue{
			Line: 1, Kind: IssueInvalidRow, Severity: SeverityError,
			Field: "header", Detail: "the header row could not be read: " + err.Error(),
		})
		return parsed
	}
	if problem := compareHeader(header); problem != "" {
		parsed.Issues = append(parsed.Issues, Issue{
			Line: 1, Kind: IssueInvalidRow, Severity: SeverityError,
			Field: "header", Detail: problem,
		})
		return parsed
	}
	parsed.HeaderOK = true

	scanner := rowScanner{}
	for {
		record, readErr := reader.Read()
		if readErr == io.EOF {
			break
		}
		line, _ := reader.FieldPos(0)
		if readErr != nil {
			scanner.fail(line, IssueInvalidRow, "row", "malformed CSV record: "+readErr.Error())
			continue
		}
		if isBlankRecord(record) {
			continue
		}
		parsed.TotalLines = line
		if len(record) != len(HeaderColumns) {
			scanner.fail(line, IssueInvalidRow, "row",
				"expected 21 columns, found "+strconv.Itoa(len(record)))
			continue
		}
		kind := strings.ToLower(textField(record, colKind))
		switch kind {
		case KindFormula:
			parsed.Formulas = append(parsed.Formulas, parseFormulaRow(record, line, &scanner))
		case KindStep:
			parsed.Steps = append(parsed.Steps, parseStepRow(record, line, &scanner))
		case KindVessel:
			parsed.Vessels = append(parsed.Vessels, parseVesselRow(record, line, &scanner))
		case KindRun:
			parsed.Runs = append(parsed.Runs, parseRunRow(record, line, &scanner))
		case "":
			scanner.fail(line, IssueInvalidRow, "kind", "is required")
		default:
			scanner.fail(line, IssueUnknownKind, "kind",
				"must be one of formula, step, vessel, run")
		}
	}
	parsed.Issues = append(parsed.Issues, scanner.issues...)
	return parsed
}

func compareHeader(header []string) string {
	if len(header) != len(HeaderColumns) {
		return "header must list exactly these columns: " + strings.Join(HeaderColumns, ", ")
	}
	seen := map[string]bool{}
	for index, value := range header {
		name := strings.TrimSpace(value)
		if name != HeaderColumns[index] {
			return "column " + strconv.Itoa(index+1) + " must be named " +
				HeaderColumns[index] + ", found " + name
		}
		if seen[name] {
			return "header column " + name + " is repeated"
		}
		seen[name] = true
	}
	return ""
}

func isBlankRecord(record []string) bool {
	for _, value := range record {
		if strings.TrimSpace(value) != "" {
			return false
		}
	}
	return true
}

func parseFormulaRow(record []string, line int, scanner *rowScanner) FormulaRow {
	row := FormulaRow{Line: line, Ref: textField(record, colRef)}
	ok := true
	name, nameOK := scanner.requiredText(record, colName, "name", line)
	row.Name = domain.NormalizeName(name)
	ok = nameOK && ok
	style, styleOK := scanner.requiredText(record, colStyle, "style", line)
	row.Style = domain.NormalizeName(style)
	ok = styleOK && ok
	if og, valid := scanner.parseFloat(record, colTargetOG, "target_original_gravity", line); valid {
		row.TargetOriginalGravity = og
	} else {
		ok = false
	}
	if fg, valid := scanner.parseFloat(record, colTargetFG, "target_final_gravity", line); valid {
		row.TargetFinalGravity = fg
	} else {
		ok = false
	}
	if temp, valid := scanner.parseFloat(record, colMaxTemp, "max_fermentation_temp_c", line); valid {
		row.MaxFermentationTempC = temp
	} else {
		ok = false
	}
	if days, valid := scanner.parseInt(record, colMinDays, "minimum_days", line); valid {
		row.MinimumDays = days
	} else {
		ok = false
	}
	if days, valid := scanner.parseInt(record, colMaxDays, "maximum_days", line); valid {
		row.MaximumDays = days
	} else {
		ok = false
	}
	lifecycle := domain.FormulaState(textField(record, colLifecycle))
	if lifecycle == "" {
		lifecycle = domain.FormulaApproved
	}
	if lifecycle != domain.FormulaDraft && lifecycle != domain.FormulaApproved {
		scanner.fail(line, IssueInvalidRow, "lifecycle",
			"must be draft or approved")
		ok = false
	}
	row.Lifecycle = lifecycle
	row.HasParseError = !ok
	return row
}

func parseStepRow(record []string, line int, scanner *rowScanner) StepRow {
	row := StepRow{Line: line}
	ok := true
	if ref, valid := scanner.requiredText(record, colRef, "ref", line); valid {
		row.FormulaRef = ref
	} else {
		ok = false
	}
	if seq, valid := scanner.parseInt(record, colStepSeq, "step_sequence", line); valid {
		row.Sequence = seq
	} else {
		ok = false
	}
	if label, valid := scanner.requiredText(record, colStepLabel, "step_label", line); valid {
		row.Label = label
	} else {
		ok = false
	}
	if temp, valid := scanner.parseFloat(record, colStepTemp, "step_temperature_c", line); valid {
		row.TemperatureC = temp
	} else {
		ok = false
	}
	if hold, valid := scanner.parseInt(record, colStepHold, "step_hold_minutes", line); valid {
		row.HoldMinutes = hold
	} else {
		ok = false
	}
	row.HasParseError = !ok
	return row
}

func parseVesselRow(record []string, line int, scanner *rowScanner) VesselRow {
	row := VesselRow{Line: line, Ref: textField(record, colRef)}
	ok := true
	if code, valid := scanner.requiredText(record, colVesselCode, "vessel_code", line); valid {
		row.Code = domain.NormalizeVesselCode(code)
	} else {
		ok = false
	}
	if capacity, valid := scanner.parseFloat(record, colVesselCap, "vessel_capacity_liters", line); valid {
		row.CapacityL = capacity
	} else {
		ok = false
	}
	row.HasParseError = !ok
	return row
}

func parseRunRow(record []string, line int, scanner *rowScanner) RunRow {
	row := RunRow{Line: line}
	ok := true
	if code, valid := scanner.requiredText(record, colRunCode, "run_code", line); valid {
		row.Code = domain.NormalizeRunCode(code)
	} else {
		ok = false
	}
	if ref, valid := scanner.requiredText(record, colRunFormula, "run_formula", line); valid {
		row.FormulaRef = ref
	} else {
		ok = false
	}
	if vessel := textField(record, colRunVessel); vessel != "" {
		row.HasVesselRef = true
		row.VesselRef = vessel
	}
	if volume, valid := scanner.parseFloat(record, colRunVolume, "planned_volume_liters", line); valid {
		row.PlannedVolumeL = volume
	} else {
		ok = false
	}
	row.Notes = textField(record, colNotes)
	row.HasParseError = !ok
	return row
}

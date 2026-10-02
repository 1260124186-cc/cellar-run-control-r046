package workflowcheck

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/domain"
)

const importHeader = "kind,ref,name,style,target_original_gravity,target_final_gravity,max_fermentation_temp_c,minimum_days,maximum_days,lifecycle,step_sequence,step_label,step_temperature_c,step_hold_minutes,vessel_code,vessel_capacity_liters,run_code,run_formula,run_vessel,planned_volume_liters,notes"

// csvRow renders a 21-column record from the supplied column->value map.
func csvRow(values map[int]string) string {
	columns := make([]string, 21)
	for index, value := range values {
		columns[index] = value
	}
	return joinColumns(columns)
}

func joinColumns(columns []string) string {
	result := ""
	for index, value := range columns {
		if index > 0 {
			result += ","
		}
		result += value
	}
	return result
}

func validSeasonCSV() []byte {
	rows := []string{
		importHeader,
		csvRow(map[int]string{
			0: "formula", 1: "f1", 2: "Season Lager", 3: "Cellar lager",
			4: "1.050", 5: "1.010", 6: "18", 7: "10", 8: "30", 9: "approved",
		}),
		csvRow(map[int]string{
			0: "step", 1: "f1", 10: "1", 11: "Heat mash", 12: "66", 13: "60",
		}),
		csvRow(map[int]string{
			0: "step", 1: "f1", 10: "2", 11: "Cool wort", 12: "18", 13: "45",
		}),
		csvRow(map[int]string{
			0: "vessel", 1: "v1", 14: "FV-100", 15: "500",
		}),
		csvRow(map[int]string{
			0: "run", 16: "RUN-SEASON-1", 17: "f1", 18: "v1", 19: "300",
			20: "season opening",
		}),
	}
	return joinCSV(rows)
}

type importPreview struct {
	ID       string `json:"id"`
	Status   string `json:"status"`
	Filename string `json:"filename"`
	Counts   struct {
		FormulasCreate int `json:"formulas_create"`
		FormulasAdopt  int `json:"formulas_adopt"`
		VesselsCreate  int `json:"vessels_create"`
		VesselsAdopt   int `json:"vessels_adopt"`
		RunsCreate     int `json:"runs_create"`
		RunsAdopt      int `json:"runs_adopt"`
		Conflicts      int `json:"conflicts"`
		Errors         int `json:"errors"`
	} `json:"counts"`
	Plan *struct {
		Formulas []struct {
			Line int    `json:"line"`
			ID   string `json:"id"`
			Ref  string `json:"ref"`
		} `json:"formulas"`
		Vessels []struct {
			Line int    `json:"line"`
			ID   string `json:"id"`
			Code string `json:"code"`
		} `json:"vessels"`
		Runs []struct {
			Line      int    `json:"line"`
			ID        string `json:"id"`
			Code      string `json:"code"`
			FormulaID string `json:"formula_id"`
			VesselID  string `json:"vessel_id"`
		} `json:"runs"`
		Issues []struct {
			Line  int    `json:"line"`
			Kind  string `json:"kind"`
			Field string `json:"field"`
			Ref   string `json:"ref"`
		} `json:"issues"`
	} `json:"plan"`
}

func joinCSV(rows []string) []byte {
	var content []byte
	for _, row := range rows {
		content = append(content, row...)
		content = append(content, '\n')
	}
	return content
}

func batchImport(api *client, dataDir string) error {
	// A structurally bad file is rejected before any session exists and
	// nothing is created.
	badResponse, err := api.callCSV("POST", "/v1/imports", []byte("not,a,valid,header\nx,y,z,w\n"))
	if err != nil {
		return err
	}
	if err := requireCode(badResponse, 400, string(domain.CodeImportInvalid),
		"preview malformed csv"); err != nil {
		return err
	}

	// A semantically invalid file opens a preview session that clearly
	// reports every blocking issue and must not be confirmable.
	invalidRows := []string{
		importHeader,
		// Gravity out of range.
		csvRow(map[int]string{
			0: "formula", 1: "bad", 2: "Broken Formula", 3: "Cellar",
			4: "1.500", 5: "1.010", 6: "18", 7: "10", 8: "30", 9: "approved",
		}),
		csvRow(map[int]string{
			0: "step", 1: "bad", 10: "1", 11: "Heat", 12: "66", 13: "60",
		}),
		// Step references a formula that does not exist in the file.
		csvRow(map[int]string{
			0: "step", 1: "missing", 10: "1", 11: "Heat", 12: "66", 13: "60",
		}),
		// Run points at an unresolved formula and exceeds vessel capacity.
		csvRow(map[int]string{
			0: "vessel", 1: "small", 14: "FV-S", 15: "100",
		}),
		csvRow(map[int]string{
			0: "run", 16: "RUN-BAD", 17: "ghost", 18: "small", 19: "150",
		}),
	}
	invalidResponse, err := api.callCSV("POST", "/v1/imports", joinCSV(invalidRows))
	if err != nil {
		return err
	}
	if err := requireStatus(invalidResponse, 201, "preview invalid csv"); err != nil {
		return err
	}
	var invalidPreview importPreview
	if err := invalidResponse.decode(&invalidPreview); err != nil {
		return err
	}
	if invalidPreview.Counts.Errors < 4 {
		return fmt.Errorf("expected at least 4 blocking issues, got %d", invalidPreview.Counts.Errors)
	}
	invalidConfirm, err := api.call("POST", "/v1/imports/"+invalidPreview.ID+"/confirm", nil, nil)
	if err != nil {
		return err
	}
	if err := requireCode(invalidConfirm, 409, string(domain.CodeImportConflict),
		"confirm an invalid plan"); err != nil {
		return err
	}
	if err := assertImportCreatedNothing(api, invalidPreview); err != nil {
		return err
	}

	// Happy path: preview shows exactly one of each, confirm builds all of
	// them in one revision, and the planned run references the new formula
	// and vessel by their deterministic ids.
	firstResponse, err := api.callCSV("POST", "/v1/imports", validSeasonCSV())
	if err != nil {
		return err
	}
	if err := requireStatus(firstResponse, 201, "preview valid csv"); err != nil {
		return err
	}
	var preview importPreview
	if err := firstResponse.decode(&preview); err != nil {
		return err
	}
	if preview.Counts.FormulasCreate != 1 || preview.Counts.VesselsCreate != 1 ||
		preview.Counts.RunsCreate != 1 || preview.Counts.Errors != 0 ||
		preview.Counts.Conflicts != 0 {
		return fmt.Errorf("unexpected preview counts: %+v", preview.Counts)
	}
	if preview.Plan == nil || len(preview.Plan.Formulas) != 1 ||
		preview.Plan.Formulas[0].ID == "" || preview.Plan.Runs[0].FormulaID == "" ||
		preview.Plan.Runs[0].VesselID == "" {
		return fmt.Errorf("preview did not resolve cross-table references")
	}

	beforeRevision := currentRevision(api)
	confirmResponse, err := api.call("POST", "/v1/imports/"+preview.ID+"/confirm", nil, nil)
	if err != nil {
		return err
	}
	if err := requireStatus(confirmResponse, 200, "confirm valid csv"); err != nil {
		return err
	}
	var confirmed importPreview
	if err := confirmResponse.decode(&confirmed); err != nil {
		return err
	}
	if confirmed.Status != "completed" {
		return fmt.Errorf("expected completed status, got %q", confirmed.Status)
	}
	if revision := currentRevision(api); revision != beforeRevision+1 {
		return fmt.Errorf("import did not commit in exactly one revision")
	}

	formulaRead, err := api.call("GET", "/v1/formulas/"+preview.Plan.Formulas[0].ID, nil, nil)
	if err != nil {
		return err
	}
	if err := requireStatus(formulaRead, 200, "read imported formula"); err != nil {
		return err
	}
	var formula formulaView
	if err := formulaRead.decode(&formula); err != nil {
		return err
	}
	if formula.State != domain.FormulaApproved || len(formula.Steps) != 2 {
		return fmt.Errorf("imported formula was not approved with its steps")
	}
	runRead, err := api.call("GET", "/v1/runs/"+preview.Plan.Runs[0].ID, nil, nil)
	if err != nil {
		return err
	}
	if err := requireStatus(runRead, 200, "read imported run"); err != nil {
		return err
	}

	// Re-uploading the exact same file is recognized as adopt and
	// confirming it again creates nothing.
	repeatResponse, err := api.callCSV("POST", "/v1/imports", validSeasonCSV())
	if err != nil {
		return err
	}
	if err := requireStatus(repeatResponse, 200, "repeat preview is idempotent"); err != nil {
		return err
	}
	var repeat importPreview
	if err := repeatResponse.decode(&repeat); err != nil {
		return err
	}
	if repeat.ID != preview.ID {
		return fmt.Errorf("repeated upload opened a different import session")
	}
	if repeat.Counts.FormulasCreate != 0 || repeat.Counts.FormulasAdopt != 1 ||
		repeat.Counts.VesselsAdopt != 1 || repeat.Counts.RunsAdopt != 1 {
		return fmt.Errorf("repeated upload was not treated as adopt: %+v", repeat.Counts)
	}
	revisionBefore := currentRevision(api)
	repeatConfirm, err := api.call("POST", "/v1/imports/"+repeat.ID+"/confirm", nil, nil)
	if err != nil {
		return err
	}
	if err := requireStatus(repeatConfirm, 200, "repeat confirm"); err != nil {
		return err
	}
	if currentRevision(api) != revisionBefore {
		return fmt.Errorf("confirming an adopted import changed the snapshot")
	}

	// A file whose vessel code and run code collide with entities owned by
	// the earlier import is a conflict, not an adopt.
	collisionRows := []string{
		importHeader,
		csvRow(map[int]string{
			0: "vessel", 14: "FV-100", 15: "900",
		}),
		csvRow(map[int]string{
			0: "run", 16: "RUN-SEASON-1", 17: "Season Lager", 19: "42",
			20: "conflict probe",
		}),
	}
	collisionResponse, err := api.callCSV("POST", "/v1/imports", joinCSV(collisionRows))
	if err != nil {
		return err
	}
	if err := requireStatus(collisionResponse, 201, "preview collision csv"); err != nil {
		return err
	}
	var collision importPreview
	if err := collisionResponse.decode(&collision); err != nil {
		return err
	}
	if collision.Counts.Conflicts == 0 {
		return fmt.Errorf("code collision was not reported as a conflict")
	}
	collisionConfirm, err := api.call("POST", "/v1/imports/"+collision.ID+"/confirm", nil, nil)
	if err != nil {
		return err
	}
	if err := requireCode(collisionConfirm, 409, string(domain.CodeImportConflict),
		"confirm a colliding plan"); err != nil {
		return err
	}

	// Simulate a crash between setting "applying" and completing the
	// ledger: hand-write the session state into the ledger directory,
	// reopen the view, and verify reconciliation reports completion and
	// preserves visibility of the previous run.
	if err := simulateInterruptedImport(dataDir, preview.ID); err != nil {
		return err
	}
	recoveredResponse, err := api.call("GET", "/v1/imports/"+preview.ID, nil, nil)
	if err != nil {
		return err
	}
	if err := requireStatus(recoveredResponse, 200, "read interrupted import"); err != nil {
		return err
	}
	var recovered importPreview
	if err := recoveredResponse.decode(&recovered); err != nil {
		return err
	}
	if recovered.Status != "completed" {
		return fmt.Errorf("interrupted import was not reconciled to completed, got %q",
			recovered.Status)
	}

	listResponse, err := api.call("GET", "/v1/imports", nil, nil)
	if err != nil {
		return err
	}
	if err := requireStatus(listResponse, 200, "list imports"); err != nil {
		return err
	}
	return nil
}

func assertImportCreatedNothing(api *client, preview importPreview) error {
	formulas, err := api.call("GET", "/v1/formulas", nil, nil)
	if err != nil {
		return err
	}
	var formulaList struct {
		Formulas []json.RawMessage `json:"formulas"`
	}
	if err := formulas.decode(&formulaList); err != nil {
		return err
	}
	if len(formulaList.Formulas) != 0 {
		return fmt.Errorf("invalid import created %d formulas", len(formulaList.Formulas))
	}
	vessels, err := api.call("GET", "/v1/vessels", nil, nil)
	if err != nil {
		return err
	}
	var vesselList struct {
		Vessels []json.RawMessage `json:"vessels"`
	}
	if err := vessels.decode(&vesselList); err != nil {
		return err
	}
	if len(vesselList.Vessels) != 0 {
		return fmt.Errorf("invalid import created %d vessels", len(vesselList.Vessels))
	}
	runs, err := api.call("GET", "/v1/runs", nil, nil)
	if err != nil {
		return err
	}
	var runList struct {
		Runs []json.RawMessage `json:"runs"`
	}
	if err := runs.decode(&runList); err != nil {
		return err
	}
	if len(runList.Runs) != 0 {
		return fmt.Errorf("invalid import created %d runs", len(runList.Runs))
	}
	return nil
}

func currentRevision(api *client) int64 {
	response, err := api.call("GET", "/healthz", nil, nil)
	if err != nil {
		return -1
	}
	var health struct {
		Revision int64 `json:"revision"`
	}
	if err := response.decode(&health); err != nil {
		return -1
	}
	return health.Revision
}

// simulateInterruptedImport rewrites the ledger entry of an already
// completed import as "applying", mimicking a process that died after the
// snapshot commit but before the completion ledger write.
func simulateInterruptedImport(dataDir, id string) error {
	path := filepath.Join(dataDir, "imports", id+".json")
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read ledger for crash simulation: %w", err)
	}
	var session map[string]any
	if err := json.Unmarshal(raw, &session); err != nil {
		return err
	}
	session["status"] = "applying"
	session["failure"] = ""
	encoded, err := json.MarshalIndent(session, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, encoded, 0o644); err != nil {
		return fmt.Errorf("rewrite ledger for crash simulation: %w", err)
	}
	return nil
}

package workflowcheck

import (
	"encoding/csv"
	"fmt"
	"strings"

	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/domain"
)

type importView struct {
	ID          string                  `json:"id"`
	Status      domain.ImportStatus     `json:"status"`
	Reused      bool                    `json:"reused"`
	Counts      domain.ImportCounts     `json:"summary"`
	Issues      []domain.ImportIssue    `json:"issues"`
	Conflicts   []domain.ImportConflict `json:"conflicts"`
	Items       []domain.ImportItem     `json:"items"`
	ConfirmedAt *string                 `json:"confirmed_at"`
}

func seasonImport(api *client) error {
	valid := buildSeasonCSV([][]string{
		{"formula", "Season Pils", "Pilsner", "1.048", "1.010", "20", "10", "24", "true", "", "", "", "", "", "", "", "", ""},
		{"step", "", "", "", "", "", "", "", "", "", "", "Season Pils", "", "", "1", "Mash in", "63", "60"},
		{"step", "", "", "", "", "", "", "", "", "", "", "Season Pils", "", "", "2", "Cool to ferment", "14", "30"},
		{"vessel", "", "", "", "", "", "", "", "", "2000", "VSL-FV-1", "", "", "", "", "", "", ""},
		{"vessel", "", "", "", "", "", "", "", "", "1200", "VSL-FV-2", "", "", "", "", "", "", ""},
		{"run", "", "", "", "", "", "", "", "", "", "RUN-SEASON-1", "Season Pils", "900", "", "", "", "", ""},
		{"run", "", "", "", "", "", "", "", "", "", "RUN-SEASON-2", "Season Pils", "500", "", "", "", "", ""},
	})

	created, err := api.call("POST", "/v1/imports", map[string]any{
		"filename": "season.csv",
		"content":  valid,
	}, nil)
	if err != nil {
		return err
	}
	if err := requireStatus(created, 201, "create import session"); err != nil {
		return err
	}
	var first importView
	if err := created.decode(&first); err != nil {
		return err
	}
	if first.Status != domain.ImportReady {
		return fmt.Errorf("valid season CSV was rejected: %+v", first.Issues)
	}
	if first.Counts.Formulas != 1 || first.Counts.Steps != 2 ||
		first.Counts.Vessels != 2 || first.Counts.Runs != 2 {
		return fmt.Errorf("precheck summary is wrong: %+v", first.Counts)
	}
	if len(first.Items) != 5 || len(first.Issues) != 0 {
		return fmt.Errorf("precheck planned %d items with %d issues, want 5 and 0",
			len(first.Items), len(first.Issues))
	}

	// Confirmation must create everything atomically.
	confirmed, err := api.call("POST", "/v1/imports/"+first.ID+"/confirm", nil, nil)
	if err != nil {
		return err
	}
	if err := requireStatus(confirmed, 200, "confirm import"); err != nil {
		return err
	}
	var done importView
	if err := confirmed.decode(&done); err != nil {
		return err
	}
	if done.Status != domain.ImportCompleted || done.ConfirmedAt == nil {
		return fmt.Errorf("import was not marked completed: %+v", done)
	}
	for _, item := range done.Items {
		if item.State != domain.ImportItemCreated {
			return fmt.Errorf("item on line %d was not marked created", item.Line)
		}
	}

	// The formula must be approved and carry both steps.
	formulas, err := api.call("GET", "/v1/formulas", nil, nil)
	if err != nil {
		return err
	}
	if err := requireStatus(formulas, 200, "list formulas after import"); err != nil {
		return err
	}
	var formulaList struct {
		Formulas []formulaView `json:"formulas"`
	}
	if err := formulas.decode(&formulaList); err != nil {
		return err
	}
	if len(formulaList.Formulas) != 1 {
		return fmt.Errorf("expected exactly one formula, got %d", len(formulaList.Formulas))
	}
	if formulaList.Formulas[0].State != domain.FormulaApproved ||
		len(formulaList.Formulas[0].Steps) != 2 {
		return fmt.Errorf("imported formula is not approved with two steps: %+v",
			formulaList.Formulas[0])
	}

	runs, err := api.call("GET", "/v1/runs", nil, nil)
	if err != nil {
		return err
	}
	if err := requireStatus(runs, 200, "list runs after import"); err != nil {
		return err
	}
	var runList struct {
		Runs []runView `json:"runs"`
	}
	if err := runs.decode(&runList); err != nil {
		return err
	}
	if len(runList.Runs) != 2 || runList.Runs[0].State != domain.RunPlanned {
		return fmt.Errorf("imported runs were not created as planned: %+v", runList.Runs)
	}

	// Re-uploading the identical file must not create a second round.
	repeated, err := api.call("POST", "/v1/imports", map[string]any{
		"filename": "season.csv",
		"content":  valid,
	}, nil)
	if err != nil {
		return err
	}
	if err := requireStatus(repeated, 200, "re-upload identical file"); err != nil {
		return err
	}
	var again importView
	if err := repeated.decode(&again); err != nil {
		return err
	}
	if !again.Reused || again.ID != first.ID || again.Status != domain.ImportCompleted {
		return fmt.Errorf("identical file did not resolve to the completed session: %+v", again)
	}
	runsAgain, err := api.call("GET", "/v1/runs", nil, nil)
	if err != nil {
		return err
	}
	if err := runsAgain.decode(&runList); err != nil {
		return err
	}
	if len(runList.Runs) != 2 {
		return fmt.Errorf("repeat upload created extra runs: now %d", len(runList.Runs))
	}

	if err := rejectInvalidSeason(api); err != nil {
		return err
	}
	return conflictVisibility(api)
}

func rejectInvalidSeason(api *client) error {
	invalid := buildSeasonCSV([][]string{
		{"formula", "Bad Grav", "Stout", "1.010", "1.099", "20", "10", "24", "true", "", "", "", "", "", "", "", "", ""},
		{"step", "", "", "", "", "", "", "", "", "", "", "Missing Formula", "", "", "1", "Orphan step", "20", "30"},
		{"vessel", "", "", "", "", "", "", "", "", "5", "VSL-TINY", "", "", "", "", "", "", ""},
		{"run", "", "", "", "", "", "", "", "", "", "RUN-BAD", "Nowhere", "10", "", "", "", "", ""},
	})
	rejected, err := api.call("POST", "/v1/imports", map[string]any{
		"filename": "bad.csv",
		"content":  invalid,
	}, nil)
	if err != nil {
		return err
	}
	if err := requireStatus(rejected, 201, "precheck invalid file"); err != nil {
		return err
	}
	var session importView
	if err := rejected.decode(&session); err != nil {
		return err
	}
	if session.Status != domain.ImportRejected || len(session.Issues) < 4 {
		return fmt.Errorf("invalid CSV should be rejected with multiple line issues: %+v", session)
	}
	lines := map[int]bool{}
	for _, issue := range session.Issues {
		lines[issue.Line] = true
	}
	for _, expected := range []int{2, 3, 4, 5} {
		if !lines[expected] {
			return fmt.Errorf("expected a blocking issue on line %d, got %+v", expected, session.Issues)
		}
	}

	denied, err := api.call("POST", "/v1/imports/"+session.ID+"/confirm", nil, nil)
	if err != nil {
		return err
	}
	if err := requireCode(denied, 409, string(domain.CodeImportNotReady),
		"confirm rejected import"); err != nil {
		return err
	}

	// Nothing should have leaked into the cellar.
	vessels, err := api.call("GET", "/v1/vessels", nil, nil)
	if err != nil {
		return err
	}
	var vesselList struct {
		Vessels []vesselView `json:"vessels"`
	}
	if err := vessels.decode(&vesselList); err != nil {
		return err
	}
	if len(vesselList.Vessels) != 2 {
		return fmt.Errorf("rejected import changed vessel count: got %d", len(vesselList.Vessels))
	}
	return nil
}

func conflictVisibility(api *client) error {
	// A file that reuses an existing formula and run code must surface
	// conflicts, remain ready, and only create the genuinely new vessel.
	content := buildSeasonCSV([][]string{
		{"formula", "Season Pils", "Pilsner", "1.048", "1.010", "20", "10", "24", "true", "", "", "", "", "", "", "", "", ""},
		{"vessel", "", "", "", "", "", "", "", "", "900", "VSL-FV-3", "", "", "", "", "", "", ""},
		{"run", "", "", "", "", "", "", "", "", "", "RUN-SEASON-1", "Season Pils", "100", "", "", "", "", ""},
	})
	response, err := api.call("POST", "/v1/imports", map[string]any{
		"filename": "partial.csv",
		"content":  content,
	}, nil)
	if err != nil {
		return err
	}
	if err := requireStatus(response, 201, "precheck conflicting file"); err != nil {
		return err
	}
	var session importView
	if err := response.decode(&session); err != nil {
		return err
	}
	if session.Status != domain.ImportReady {
		return fmt.Errorf("conflicts should not block an otherwise valid import: %+v", session.Issues)
	}
	if len(session.Conflicts) != 2 {
		return fmt.Errorf("expected two conflicts, got %+v", session.Conflicts)
	}
	if len(session.Items) != 1 || session.Items[0].Key != "VSL-FV-3" {
		return fmt.Errorf("only the new vessel should be planned, got %+v", session.Items)
	}
	confirmed, err := api.call("POST", "/v1/imports/"+session.ID+"/confirm", nil, nil)
	if err != nil {
		return err
	}
	if err := requireStatus(confirmed, 200, "confirm conflicting import"); err != nil {
		return err
	}
	return nil
}

func buildSeasonCSV(rows [][]string) string {
	header := []string{
		"kind", "name", "style", "target_original_gravity", "target_final_gravity",
		"max_fermentation_temp_c", "minimum_days", "maximum_days", "approved",
		"capacity_liters", "code", "formula", "planned_volume_liters", "notes",
		"sequence", "label", "temperature_c", "hold_minutes",
	}
	var builder strings.Builder
	writer := csv.NewWriter(&builder)
	_ = writer.Write(header)
	for _, row := range rows {
		_ = writer.Write(row)
	}
	writer.Flush()
	return builder.String()
}

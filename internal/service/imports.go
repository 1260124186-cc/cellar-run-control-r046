package service

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"path/filepath"
	"strings"
	"time"

	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/domain"
	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/importplan"
	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/storage"
)

const maxImportBytes = 10 << 20

// ImportSummary is the public view of an import session ledger.
type ImportSummary struct {
	ID          string            `json:"id"`
	Filename    string            `json:"filename"`
	Status      string            `json:"status"`
	ByteSize    int               `json:"byte_size"`
	SHA256      string            `json:"sha256"`
	Revision    int64             `json:"revision,omitempty"`
	CreatedAt   time.Time         `json:"created_at"`
	UpdatedAt   time.Time         `json:"updated_at"`
	CompletedAt *time.Time        `json:"completed_at,omitempty"`
	Failure     string            `json:"failure,omitempty"`
	Counts      importplan.Counts `json:"counts"`
	Plan        *importplan.Plan  `json:"plan,omitempty"`
}

// PreviewImport parses and prechecks a CSV bundle without writing any
// business entity. The session ledger is durable, so the same bytes can
// never open a second import session.
func (s *Service) PreviewImport(content []byte, filename string) (ImportSummary, bool, error) {
	if s.imports == nil {
		return ImportSummary{}, false, domain.NewError(domain.CodeStorage,
			"the import ledger is not configured")
	}
	if len(content) > maxImportBytes {
		return ImportSummary{}, false, domain.FieldError(domain.CodeImportInvalid,
			"file", "the CSV file must not exceed 10 MiB")
	}
	sum := sha256.Sum256(content)
	id := bytesToHex(sum[:])

	lock := s.sessionLock(id)
	lock.Lock()
	defer lock.Unlock()

	if existing, ok, err := s.imports.Get(id); err != nil {
		return ImportSummary{}, false, domain.NewError(domain.CodeStorage, err.Error())
	} else if ok {
		existing, err = s.reconcile(existing)
		if err != nil {
			return ImportSummary{}, false, err
		}
		// A completed session's stored plan predates its own apply, so
		// rebuild against current state to report adopt instead of create.
		if existing.Status == storage.ImportCompleted {
			if rebuilt, err := s.rebuiltSession(existing); err == nil {
				existing = rebuilt
			}
		}
		summary, err := s.sessionSummary(existing, existing.Status != storage.ImportCompleted)
		return summary, false, err
	}

	parsed := importplan.Parse(content)
	if !parsed.HeaderOK {
		first := importplan.Issue{}
		if len(parsed.Issues) > 0 {
			first = parsed.Issues[0]
		}
		return ImportSummary{}, false, domain.FieldError(domain.CodeImportInvalid,
			"header", first.Detail)
	}

	var catalog importplan.Catalog
	if err := s.store.View(func(snapshot domain.Snapshot) error {
		catalog = catalogFromSnapshot(snapshot)
		return nil
	}); err != nil {
		return ImportSummary{}, false, err
	}
	plan := importplan.Build(id, parsed, catalog)
	session, err := newImportSession(id, filename, content, plan, s.clock.Now())
	if err != nil {
		return ImportSummary{}, false, err
	}
	if err := s.imports.Save(session); err != nil {
		return ImportSummary{}, false, domain.NewError(domain.CodeStorage, err.Error())
	}
	summary, err := s.sessionSummary(session, true)
	if err != nil {
		return ImportSummary{}, false, err
	}
	return summary, true, nil
}

// ConfirmImport applies a prechecked, conflict-free session in one atomic
// snapshot revision. Applying is idempotent: a completed session is
// returned unchanged, and an interrupted applying session is reconciled
// against the snapshot before doing anything else.
func (s *Service) ConfirmImport(id string) (ImportSummary, error) {
	if s.imports == nil {
		return ImportSummary{}, domain.NewError(domain.CodeStorage,
			"the import ledger is not configured")
	}
	if !storage.ValidImportID(id) {
		return ImportSummary{}, domain.NewError(domain.CodeImportNotFound,
			"import session was not found")
	}
	lock := s.sessionLock(id)
	lock.Lock()
	defer lock.Unlock()

	session, ok, err := s.imports.Get(id)
	if err != nil {
		return ImportSummary{}, domain.NewError(domain.CodeStorage, err.Error())
	}
	if !ok {
		return ImportSummary{}, domain.NewError(domain.CodeImportNotFound,
			"import session was not found")
	}

	session, err = s.reconcile(session)
	if err != nil {
		return ImportSummary{}, err
	}
	if session.Status == storage.ImportCompleted {
		// The stored plan predates the apply; rebuild it so a repeated
		// confirmation reports adopt instead of create.
		if rebuilt, err := s.rebuiltSession(session); err == nil {
			session = rebuilt
		}
		return s.sessionSummary(session, false)
	}

	content, err := base64.StdEncoding.DecodeString(session.RawContentBase64)
	if err != nil {
		return ImportSummary{}, domain.NewError(domain.CodeStorage,
			"the stored import content is unreadable")
	}
	parsed := importplan.Parse(content)
	if !parsed.HeaderOK {
		return ImportSummary{}, domain.NewError(domain.CodeImportInvalid,
			"the stored import file is no longer parseable")
	}

	// Mark intent before touching the snapshot. If the process dies here,
	// the next call sees "applying" and reconciles against what landed.
	session.Status = storage.ImportApplying
	session.Failure = ""
	if err := s.imports.Save(session); err != nil {
		return ImportSummary{}, domain.NewError(domain.CodeStorage, err.Error())
	}

	applyErr := s.store.Execute(func(snapshot *domain.Snapshot) error {
		// Re-plan against the live snapshot: a concurrent entity created
		// after the preview must abort the whole import rather than half.
		livePlan := importplan.Build(id, parsed, catalogFromSnapshot(*snapshot))
		if !livePlan.Confirmable() {
			return domain.NewError(domain.CodeImportConflict,
				"the import plan now conflicts with current state")
		}
		return applyPlan(id, parsed, livePlan, snapshot, s.clock.Now())
	})
	if applyErr != nil {
		if domain.IsCode(applyErr, domain.CodeImportConflict) {
			refreshed, planErr := s.refreshedSession(session, parsed)
			if planErr == nil {
				session = refreshed
			}
			session.Status = storage.ImportPreviewed
			session.Failure = "confirmation aborted: the plan conflicts with current state"
			_ = s.imports.Save(session)
		}
		return ImportSummary{}, applyErr
	}

	revision := s.store.Revision()
	stamp := s.clock.Now()
	session.Status = storage.ImportCompleted
	session.Failure = ""
	session.Revision = revision
	session.CompletedAt = &stamp
	if err := s.imports.Save(session); err != nil {
		// The snapshot committed; only the ledger update failed. The
		// session stays "applying" and the next read reconciles it.
		return ImportSummary{}, domain.NewError(domain.CodeStorage, err.Error())
	}
	if rebuilt, err := s.rebuiltSession(session); err == nil {
		session = rebuilt
	}
	return s.sessionSummary(session, false)
}

// GetImport reads one session, reconciling a crashed applying ledger.
func (s *Service) GetImport(id string) (ImportSummary, error) {
	if s.imports == nil {
		return ImportSummary{}, domain.NewError(domain.CodeStorage,
			"the import ledger is not configured")
	}
	if !storage.ValidImportID(id) {
		return ImportSummary{}, domain.NewError(domain.CodeImportNotFound,
			"import session was not found")
	}
	lock := s.sessionLock(id)
	lock.Lock()
	defer lock.Unlock()
	session, ok, err := s.imports.Get(id)
	if err != nil {
		return ImportSummary{}, domain.NewError(domain.CodeStorage, err.Error())
	}
	if !ok {
		return ImportSummary{}, domain.NewError(domain.CodeImportNotFound,
			"import session was not found")
	}
	session, err = s.reconcile(session)
	if err != nil {
		return ImportSummary{}, err
	}
	if session.Status == storage.ImportCompleted {
		if rebuilt, err := s.rebuiltSession(session); err == nil {
			session = rebuilt
		}
	}
	return s.sessionSummary(session, true)
}

// ListImports returns all ledgers, newest first, without embedded plans.
func (s *Service) ListImports() ([]ImportSummary, error) {
	if s.imports == nil {
		return nil, domain.NewError(domain.CodeStorage,
			"the import ledger is not configured")
	}
	sessions, err := s.imports.List()
	if err != nil {
		return nil, domain.NewError(domain.CodeStorage, err.Error())
	}
	summaries := make([]ImportSummary, 0, len(sessions))
	for _, session := range sessions {
		if session.Status == storage.ImportCompleted {
			if rebuilt, err := s.rebuiltSession(session); err == nil {
				session = rebuilt
			}
		}
		summary, err := s.sessionSummary(session, false)
		if err != nil {
			return nil, err
		}
		summaries = append(summaries, summary)
	}
	return summaries, nil
}

func (s *Service) refreshedSession(
	session storage.ImportSession,
	parsed importplan.ParsedFile,
) (storage.ImportSession, error) {
	var catalog importplan.Catalog
	if err := s.store.View(func(snapshot domain.Snapshot) error {
		catalog = catalogFromSnapshot(snapshot)
		return nil
	}); err != nil {
		return session, err
	}
	plan := importplan.Build(session.ID, parsed, catalog)
	encoded, err := json.Marshal(plan)
	if err != nil {
		return session, err
	}
	session.Plan = encoded
	session.Items = sessionItems(plan)
	return session, nil
}

// rebuiltSession re-plans a session's stored file against current state,
// without persisting the rebuilt ledger.
func (s *Service) rebuiltSession(session storage.ImportSession) (storage.ImportSession, error) {
	content, err := base64.StdEncoding.DecodeString(session.RawContentBase64)
	if err != nil {
		return session, err
	}
	parsed := importplan.Parse(content)
	if !parsed.HeaderOK {
		return session, domain.NewError(domain.CodeImportInvalid,
			"the stored import file is no longer parseable")
	}
	return s.refreshedSession(session, parsed)
}

// reconcile repairs a session that was marked applying when the process
// stopped. The snapshot write is atomic, so the batch either landed whole
// or not at all.
func (s *Service) reconcile(session storage.ImportSession) (storage.ImportSession, error) {
	if session.Status != storage.ImportApplying {
		return session, nil
	}
	present, expected, err := s.importLanded(session)
	if err != nil {
		return session, err
	}
	switch {
	case expected == 0 || present == expected:
		session.Status = storage.ImportCompleted
		session.Failure = ""
		session.Revision = s.store.Revision()
		if session.CompletedAt == nil {
			stamp := session.UpdatedAt
			session.CompletedAt = &stamp
		}
	case present == 0:
		session.Status = storage.ImportPreviewed
		session.Failure = "the previous confirmation did not commit; confirm again to apply"
	default:
		// Cannot happen with atomic snapshot replacement, but never invent
		// state: keep the ledger visible as applying with a failure note.
		session.Failure = "partial state detected; manual inspection required"
		return session, nil
	}
	if err := s.imports.Save(session); err != nil {
		return session, domain.NewError(domain.CodeStorage, err.Error())
	}
	return session, nil
}

func (s *Service) importLanded(session storage.ImportSession) (int, int, error) {
	present := 0
	expected := 0
	err := s.store.View(func(snapshot domain.Snapshot) error {
		for _, item := range session.Items {
			if item.Action != importplan.ActionCreate {
				continue
			}
			expected++
			source, ok := sourceForItem(snapshot, item.Kind, item.ID)
			if ok && source.ImportID == session.ID && source.Line == item.Line {
				present++
			}
		}
		return nil
	})
	return present, expected, err
}

func sourceForItem(snapshot domain.Snapshot, kind, id string) (domain.ImportSource, bool) {
	switch kind {
	case importplan.KindFormula:
		source, ok := snapshot.FormulaSources[id]
		return source, ok
	case importplan.KindVessel:
		source, ok := snapshot.VesselSources[id]
		return source, ok
	case importplan.KindRun:
		source, ok := snapshot.RunSources[id]
		return source, ok
	}
	return domain.ImportSource{}, false
}

func (s *Service) sessionSummary(
	session storage.ImportSession,
	includePlan bool,
) (ImportSummary, error) {
	plan, counts, err := decodeStoredPlan(session)
	if err != nil {
		return ImportSummary{}, err
	}
	summary := ImportSummary{
		ID:          session.ID,
		Filename:    session.Filename,
		Status:      session.Status,
		ByteSize:    session.ByteSize,
		SHA256:      session.SHA256,
		Revision:    session.Revision,
		CreatedAt:   session.CreatedAt,
		UpdatedAt:   session.UpdatedAt,
		CompletedAt: session.CompletedAt,
		Failure:     session.Failure,
		Counts:      counts,
	}
	if includePlan {
		summary.Plan = &plan
	}
	return summary, nil
}

func decodeStoredPlan(session storage.ImportSession) (importplan.Plan, importplan.Counts, error) {
	var plan importplan.Plan
	if len(session.Plan) > 0 {
		if err := json.Unmarshal(session.Plan, &plan); err != nil {
			return importplan.Plan{}, importplan.Counts{},
				domain.NewError(domain.CodeStorage, "the stored import plan is unreadable")
		}
	}
	return plan, plan.Counts(), nil
}

func newImportSession(
	id string,
	filename string,
	content []byte,
	plan importplan.Plan,
	now time.Time,
) (storage.ImportSession, error) {
	encoded, err := json.Marshal(plan)
	if err != nil {
		return storage.ImportSession{}, domain.NewError(domain.CodeStorage, err.Error())
	}
	sum := sha256.Sum256(content)
	return storage.ImportSession{
		ID:               id,
		Filename:         safeFilename(filename),
		ByteSize:         len(content),
		SHA256:           bytesToHex(sum[:]),
		Status:           storage.ImportPreviewed,
		RawContentBase64: base64.StdEncoding.EncodeToString(content),
		Plan:             encoded,
		Items:            sessionItems(plan),
		CreatedAt:        now,
		UpdatedAt:        now,
	}, nil
}

func safeFilename(filename string) string {
	name := strings.TrimSpace(filepath.Base(filepath.FromSlash(filename)))
	name = strings.TrimLeft(name, ".")
	if name == "" || name == string(filepath.Separator) {
		return "season-upload.csv"
	}
	if len(name) > 200 {
		name = name[:200]
	}
	return name
}

func sessionItems(plan importplan.Plan) []storage.ImportItem {
	items := make([]storage.ImportItem, 0, len(plan.Formulas)+len(plan.Vessels)+len(plan.Runs))
	for _, formula := range plan.Formulas {
		items = append(items, storage.ImportItem{
			Kind: importplan.KindFormula, Line: formula.Line,
			Action: formula.Action, ID: formula.ID, Name: formula.Name, Ref: formula.Ref,
		})
	}
	for _, vessel := range plan.Vessels {
		items = append(items, storage.ImportItem{
			Kind: importplan.KindVessel, Line: vessel.Line,
			Action: vessel.Action, ID: vessel.ID, Code: vessel.Code, Ref: vessel.Ref,
		})
	}
	for _, run := range plan.Runs {
		items = append(items, storage.ImportItem{
			Kind: importplan.KindRun, Line: run.Line,
			Action: run.Action, ID: run.ID, Code: run.Code,
		})
	}
	return items
}

func catalogFromSnapshot(snapshot domain.Snapshot) importplan.Catalog {
	catalog := importplan.Catalog{
		Formulas: make([]importplan.CatalogFormula, 0, len(snapshot.Formulas)),
		Vessels:  make([]importplan.CatalogVessel, 0, len(snapshot.Vessels)),
		Runs:     make([]importplan.CatalogRun, 0, len(snapshot.Runs)),
	}
	for id, formula := range snapshot.Formulas {
		entry := importplan.CatalogFormula{
			ID:                    id,
			Name:                  formula.Name,
			Style:                 formula.Style,
			TargetOriginalGravity: formula.TargetOriginalGravity,
			TargetFinalGravity:    formula.TargetFinalGravity,
			MaxFermentationTempC:  formula.MaxFermentationTempC,
			MinimumDays:           formula.MinimumDays,
			MaximumDays:           formula.MaximumDays,
			State:                 formula.State,
			Steps:                 append([]domain.FormulaStep(nil), formula.Steps...),
			Source:                sourcePointer(snapshot.FormulaSources, id),
		}
		catalog.Formulas = append(catalog.Formulas, entry)
	}
	for id, vessel := range snapshot.Vessels {
		catalog.Vessels = append(catalog.Vessels, importplan.CatalogVessel{
			ID:        id,
			Code:      vessel.Code,
			CapacityL: vessel.CapacityL,
			Source:    sourcePointer(snapshot.VesselSources, id),
		})
	}
	for id, run := range snapshot.Runs {
		vesselID := ""
		if run.VesselID != nil {
			vesselID = *run.VesselID
		}
		catalog.Runs = append(catalog.Runs, importplan.CatalogRun{
			ID:             id,
			Code:           run.Code,
			FormulaID:      run.FormulaID,
			VesselID:       vesselID,
			PlannedVolumeL: run.PlannedVolumeL,
			Notes:          run.Notes,
			Source:         sourcePointer(snapshot.RunSources, id),
		})
	}
	return catalog
}

func sourcePointer(sources map[string]domain.ImportSource, id string) *domain.ImportSource {
	if source, ok := sources[id]; ok {
		copy := source
		return &copy
	}
	return nil
}

// applyPlan inserts every create item of a confirmed plan into snapshot.
// It runs inside the single atomic snapshot update; any returned error
// aborts the revision and leaves the snapshot untouched.
func applyPlan(
	id string,
	parsed importplan.ParsedFile,
	plan importplan.Plan,
	snapshot *domain.Snapshot,
	now time.Time,
) error {
	for index, item := range plan.Formulas {
		if item.Action != importplan.ActionCreate {
			continue
		}
		row := parsed.Formulas[index]
		if _, exists := snapshot.Formulas[item.ID]; exists {
			return domain.NewError(domain.CodeImportConflict,
				"an entity with the generated id already exists")
		}
		formula := domain.Formula{
			ID:                    item.ID,
			Name:                  item.Name,
			Style:                 item.Style,
			TargetOriginalGravity: row.TargetOriginalGravity,
			TargetFinalGravity:    row.TargetFinalGravity,
			MaxFermentationTempC:  row.MaxFermentationTempC,
			MinimumDays:           row.MinimumDays,
			MaximumDays:           row.MaximumDays,
			Steps:                 append([]domain.FormulaStep(nil), item.Steps...),
			State:                 row.Lifecycle,
			Version:               1,
			CreatedAt:             now,
			UpdatedAt:             now,
		}
		if formula.State == domain.FormulaApproved {
			stamp := now
			formula.ApprovedAt = &stamp
		}
		if err := formula.Validate(); err != nil {
			return err
		}
		nameKey := domain.NameKey(formula.Name)
		if _, exists := snapshot.NameIndex[nameKey]; exists {
			return domain.NewError(domain.CodeFormulaNameConflict,
				"a formula with the same normalized name already exists")
		}
		snapshot.Formulas[item.ID] = formula
		snapshot.NameIndex[nameKey] = item.ID
		snapshot.FormulaSources[item.ID] = domain.ImportSource{ImportID: id, Line: item.Line}
	}

	for index, item := range plan.Vessels {
		if item.Action != importplan.ActionCreate {
			continue
		}
		row := parsed.Vessels[index]
		if _, exists := snapshot.Vessels[item.ID]; exists {
			return domain.NewError(domain.CodeImportConflict,
				"an entity with the generated id already exists")
		}
		vessel := domain.Vessel{
			ID:        item.ID,
			Code:      item.Code,
			CapacityL: row.CapacityL,
			State:     domain.VesselAvailable,
			Version:   1,
			CreatedAt: now,
			UpdatedAt: now,
		}
		if err := vessel.Validate(); err != nil {
			return err
		}
		if _, exists := snapshot.VesselIndex[vessel.Code]; exists {
			return domain.NewError(domain.CodeVesselConflict,
				"a vessel with the same code already exists")
		}
		snapshot.Vessels[item.ID] = vessel
		snapshot.VesselIndex[vessel.Code] = item.ID
		snapshot.VesselSources[item.ID] = domain.ImportSource{ImportID: id, Line: item.Line}
	}

	for index, item := range plan.Runs {
		if item.Action != importplan.ActionCreate {
			continue
		}
		row := parsed.Runs[index]
		if _, exists := snapshot.Runs[item.ID]; exists {
			return domain.NewError(domain.CodeImportConflict,
				"an entity with the generated id already exists")
		}
		formula, ok := snapshot.Formulas[item.FormulaID]
		if !ok {
			return domain.NewError(domain.CodeFormulaNotFound, "formula was not found")
		}
		if formula.State != domain.FormulaApproved {
			return domain.NewError(domain.CodeFormulaState,
				"run requires an approved formula")
		}
		if item.VesselID != "" {
			if _, ok := snapshot.Vessels[item.VesselID]; !ok {
				return domain.NewError(domain.CodeVesselNotFound, "vessel was not found")
			}
		}
		run := domain.FermentationRun{
			ID:             item.ID,
			Code:           item.Code,
			FormulaID:      item.FormulaID,
			PlannedVolumeL: row.PlannedVolumeL,
			Notes:          row.Notes,
			State:          domain.RunPlanned,
			Observations:   []domain.GravityObservation{},
			Version:        1,
			CreatedAt:      now,
			UpdatedAt:      now,
		}
		if item.VesselID != "" {
			vesselID := item.VesselID
			run.VesselID = &vesselID
		}
		if err := run.Validate(); err != nil {
			return err
		}
		if _, exists := snapshot.RunIndex[run.Code]; exists {
			return domain.NewError(domain.CodeRunConflict,
				"a run with the same code already exists")
		}
		snapshot.Runs[item.ID] = run.Clone()
		snapshot.RunIndex[run.Code] = item.ID
		snapshot.RunSources[item.ID] = domain.ImportSource{ImportID: id, Line: item.Line}
	}
	return nil
}

func bytesToHex(value []byte) string {
	const digits = "0123456789abcdef"
	buffer := make([]byte, len(value)*2)
	for index, b := range value {
		buffer[index*2] = digits[b>>4]
		buffer[index*2+1] = digits[b&0x0f]
	}
	return string(buffer)
}

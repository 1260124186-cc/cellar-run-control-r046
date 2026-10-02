package service

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"

	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/domain"
	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/importplan"
)

// StartImport prechecks the supplied CSV and persists the resulting import
// session. Re-uploading byte-identical content returns the existing session
// (refreshing its precheck while it has not been confirmed yet) so the same
// file never produces a second creation round.
func (s *Service) StartImport(filename string, content []byte) (domain.ImportSession, bool, error) {
	normalized := importplan.NormalizeContent(content)
	if len(normalized) == 0 {
		return domain.ImportSession{}, false,
			domain.NewError(domain.CodeInvalidInput, "import content is required")
	}
	hash := contentHash(normalized)
	filename = strings.TrimSpace(filename)

	// A file that already completed is an immutable, pure read: re-uploading
	// it reports the existing result without creating a snapshot revision.
	var completed domain.ImportSession
	viewErr := s.store.View(func(snapshot domain.Snapshot) error {
		if existingID, found := snapshot.ImportHashIdx[hash]; found {
			existing := snapshot.Imports[existingID]
			if existing.Status == domain.ImportCompleted {
				completed = existing.Clone()
			}
		}
		return nil
	})
	if viewErr != nil {
		return domain.ImportSession{}, false, viewErr
	}
	if completed.ID != "" {
		return completed, true, nil
	}

	var plan domain.ImportSession
	err := s.store.View(func(snapshot domain.Snapshot) error {
		built, buildErr := importplan.Build(normalized, currentState(snapshot), generateEntityID)
		if buildErr != nil {
			return domain.NewError(domain.CodeStorage, buildErr.Error())
		}
		plan = built
		return nil
	})
	if err != nil {
		return domain.ImportSession{}, false, err
	}

	var saved domain.ImportSession
	var reused bool
	err = s.store.Execute(func(snapshot *domain.Snapshot) error {
		now := s.clock.Now()
		if existingID, found := snapshot.ImportHashIdx[hash]; found {
			existing := snapshot.Imports[existingID]
			if existing.Status == domain.ImportCompleted {
				// Lost a race with a concurrent confirmation; never overwrite
				// a completed session with a fresh precheck.
				saved = existing.Clone()
				reused = true
				return nil
			}
			// A non-completed session for this file: refresh the precheck so
			// the same upload can be reviewed after cellar changes, but it can
			// never become a second creation round.
			reused = true
			plan.ID = existing.ID
			plan.ContentHash = hash
			plan.Filename = filename
			plan.BaseRevision = snapshot.Revision
			plan.CreatedAt = existing.CreatedAt
			plan.UpdatedAt = now
			plan.ConfirmedAt = existing.ConfirmedAt
			snapshot.Imports[existing.ID] = plan
			saved = plan.Clone()
			return nil
		}
		id, idErr := generateEntityID("import")
		if idErr != nil {
			return domain.NewError(domain.CodeStorage, idErr.Error())
		}
		plan.ID = id
		plan.ContentHash = hash
		plan.Filename = filename
		plan.BaseRevision = snapshot.Revision
		plan.CreatedAt = now
		plan.UpdatedAt = now
		snapshot.Imports[id] = plan
		snapshot.ImportHashIdx[hash] = id
		saved = plan.Clone()
		return nil
	})
	if err != nil {
		return domain.ImportSession{}, false, err
	}
	return saved, reused, nil
}

// ConfirmImport applies a ready import session's full plan in one atomic
// snapshot revision. Nothing is created when precheck was rejected or when the
// cellar changed in a way the plan did not anticipate.
func (s *Service) ConfirmImport(id string) (domain.ImportSession, error) {
	var saved domain.ImportSession
	err := s.store.Execute(func(snapshot *domain.Snapshot) error {
		session, ok := snapshot.Imports[id]
		if !ok {
			return domain.NewError(domain.CodeImportNotFound, "import session was not found")
		}
		if session.Status == domain.ImportCompleted {
			saved = session.Clone()
			return nil
		}
		if session.Status != domain.ImportReady {
			return domain.NewError(domain.CodeImportNotReady,
				"import precheck found blocking issues; nothing was created")
		}
		if err := guardDrift(session, snapshot); err != nil {
			return err
		}

		now := s.clock.Now()
		createdItems := make([]domain.ImportItem, 0, len(session.Items))

		for _, plan := range session.FormulaPlans {
			if _, exists := snapshot.NameIndex[plan.Key]; exists {
				return stateChanged()
			}
			formula := plan.Formula.Clone()
			formula.ID = plan.Formula.ID
			if formula.ID == "" {
				return stateChanged()
			}
			formula.CreatedAt = now
			formula.UpdatedAt = now
			formula.Version = 1
			if len(plan.Steps) > 0 {
				formula.Steps = append([]domain.FormulaStep(nil), plan.Steps...)
			}
			if plan.Approve {
				if err := formula.Approve(now); err != nil {
					return err
				}
			}
			if err := formula.Validate(); err != nil {
				return err
			}
			snapshot.Formulas[formula.ID] = formula
			snapshot.NameIndex[plan.Key] = formula.ID
		}

		for _, plan := range session.VesselPlans {
			vessel := plan.Vessel
			if vessel.ID == "" {
				return stateChanged()
			}
			if _, exists := snapshot.VesselIndex[vessel.Code]; exists {
				return stateChanged()
			}
			vessel.CreatedAt = now
			vessel.UpdatedAt = now
			vessel.Version = 1
			if err := vessel.Validate(); err != nil {
				return err
			}
			snapshot.Vessels[vessel.ID] = vessel
			snapshot.VesselIndex[vessel.Code] = vessel.ID
		}

		for _, plan := range session.RunPlans {
			run := plan.Run.Clone()
			if run.ID == "" || run.FormulaID == "" {
				return stateChanged()
			}
			if _, exists := snapshot.RunIndex[run.Code]; exists {
				return stateChanged()
			}
			formula, ok := snapshot.Formulas[run.FormulaID]
			if !ok {
				return stateChanged()
			}
			if formula.State != domain.FormulaApproved {
				return domain.NewError(domain.CodeFormulaState,
					"run requires an approved formula")
			}
			run.CreatedAt = now
			run.UpdatedAt = now
			run.Version = 1
			if err := run.Validate(); err != nil {
				return err
			}
			snapshot.Runs[run.ID] = run
			snapshot.RunIndex[run.Code] = run.ID
		}

		for _, item := range session.Items {
			item.State = domain.ImportItemCreated
			createdItems = append(createdItems, item)
		}
		stamp := now
		session.Items = createdItems
		session.Status = domain.ImportCompleted
		session.CommittedRevision = snapshot.Revision + 1
		session.ConfirmedAt = &stamp
		session.UpdatedAt = now
		snapshot.Imports[id] = session
		saved = session.Clone()
		return nil
	})
	if err != nil {
		return domain.ImportSession{}, err
	}
	return saved, nil
}

// GetImport returns a single import session so an interrupted upload or
// confirmation can be inspected.
func (s *Service) GetImport(id string) (domain.ImportSession, error) {
	var found domain.ImportSession
	err := s.store.View(func(snapshot domain.Snapshot) error {
		session, ok := snapshot.Imports[id]
		if !ok {
			return domain.NewError(domain.CodeImportNotFound, "import session was not found")
		}
		found = session.Clone()
		return nil
	})
	if err != nil {
		return domain.ImportSession{}, err
	}
	return found, nil
}

// ListImports returns import sessions newest first.
func (s *Service) ListImports() ([]domain.ImportSession, error) {
	var sessions []domain.ImportSession
	err := s.store.View(func(snapshot domain.Snapshot) error {
		sessions = make([]domain.ImportSession, 0, len(snapshot.Imports))
		for _, session := range snapshot.Imports {
			sessions = append(sessions, session.Clone())
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(sessions, func(i, j int) bool {
		if sessions[i].CreatedAt.Equal(sessions[j].CreatedAt) {
			return sessions[i].ID < sessions[j].ID
		}
		return sessions[i].CreatedAt.After(sessions[j].CreatedAt)
	})
	return sessions, nil
}

func currentState(snapshot domain.Snapshot) importplan.CurrentState {
	state := importplan.CurrentState{
		FormulaNames: make(map[string]importplan.ExistingFormula, len(snapshot.Formulas)),
		VesselCodes:  make(map[string]struct{}, len(snapshot.Vessels)),
		RunCodes:     make(map[string]struct{}, len(snapshot.Runs)),
	}
	for id, formula := range snapshot.Formulas {
		state.FormulaNames[domain.NameKey(formula.Name)] = importplan.ExistingFormula{
			ID:       id,
			Approved: formula.State == domain.FormulaApproved,
		}
	}
	for code := range snapshot.VesselIndex {
		state.VesselCodes[code] = struct{}{}
	}
	for code := range snapshot.RunIndex {
		state.RunCodes[code] = struct{}{}
	}
	return state
}

// guardDrift makes sure every key the precheck expected to stay absent is
// still absent and every referenced existing formula is still usable. Runs may
// also reference a formula created by this same import, which is expected.
// The surrounding single revision means a failure leaves the cellar untouched.
func guardDrift(session domain.ImportSession, snapshot *domain.Snapshot) error {
	newFormulas := make(map[string]struct{}, len(session.FormulaPlans))
	for _, plan := range session.FormulaPlans {
		newFormulas[plan.Formula.ID] = struct{}{}
		if existingID, found := snapshot.NameIndex[plan.Key]; found && existingID != plan.Formula.ID {
			return stateChanged()
		}
	}
	for _, plan := range session.VesselPlans {
		if existingID, found := snapshot.VesselIndex[plan.Vessel.Code]; found && existingID != plan.Vessel.ID {
			return stateChanged()
		}
	}
	for _, plan := range session.RunPlans {
		if existingID, found := snapshot.RunIndex[plan.Run.Code]; found && existingID != plan.Run.ID {
			return stateChanged()
		}
		if _, createdHere := newFormulas[plan.Run.FormulaID]; createdHere {
			continue
		}
		if _, found := snapshot.Formulas[plan.Run.FormulaID]; !found {
			return stateChanged()
		}
	}
	return nil
}

func stateChanged() error {
	return domain.NewError(domain.CodeImportStateChanged,
		"the cellar changed since precheck; re-upload the file to review it again")
}

func contentHash(content []byte) string {
	digest := sha256.Sum256(content)
	return hex.EncodeToString(digest[:])
}

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/clock"
	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/config"
	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/domain"
	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/service"
	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/storage"
)

func resolveConfig(dataDir string) (config.Config, error) {
	cfg, err := config.FromEnv()
	if err != nil {
		return config.Config{}, err
	}
	if strings.TrimSpace(dataDir) != "" {
		absolute, err := filepath.Abs(dataDir)
		if err != nil {
			return config.Config{}, fmt.Errorf("resolve data directory: %w", err)
		}
		cfg.DataDir = filepath.Clean(absolute)
	}
	if err := cfg.EnsureDataDir(); err != nil {
		return config.Config{}, err
	}
	return cfg, nil
}

func runImport(arguments []string) error {
	if len(arguments) < 1 {
		return errors.New("usage: fermentctl import <season.csv> [--data-dir DIR] [--yes]")
	}
	path := arguments[0]
	autoConfirm := false
	dataDir := ""
	for _, argument := range arguments[1:] {
		switch {
		case argument == "--yes":
			autoConfirm = true
		case strings.HasPrefix(argument, "--data-dir="):
			dataDir = strings.TrimPrefix(argument, "--data-dir=")
		case argument == "--data-dir":
			return errors.New("--data-dir requires a value")
		default:
			return fmt.Errorf("unknown import flag %q", argument)
		}
	}

	cfg, err := resolveConfig(dataDir)
	if err != nil {
		return err
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read CSV: %w", err)
	}

	store, err := storage.Open(cfg.DataDir)
	if err != nil {
		return err
	}
	app := service.New(store, clock.System{})

	session, reused, err := app.StartImport(path, content)
	if err != nil {
		return err
	}
	printSession(session, reused)

	if session.Status == domain.ImportCompleted {
		fmt.Println("\nthis exact file was already imported; no new entities were created.")
		return nil
	}
	if session.Status != domain.ImportReady {
		return fmt.Errorf("import precheck rejected: %d blocking issue(s); nothing was created",
			len(session.Issues))
	}
	if !autoConfirm {
		fmt.Println("\nprecheck passed. re-run with --yes to create the listed entities.")
		return nil
	}
	confirmed, err := app.ConfirmImport(session.ID)
	if err != nil {
		return err
	}
	fmt.Printf("\nimport %s completed: %d entities committed atomically at revision %d\n",
		confirmed.ID, len(confirmed.Items), confirmed.BaseRevision)
	return nil
}

func runImportStatus(arguments []string) error {
	dataDir := ""
	var sessionID string
	for _, argument := range arguments {
		switch {
		case strings.HasPrefix(argument, "--data-dir="):
			dataDir = strings.TrimPrefix(argument, "--data-dir=")
		case strings.HasPrefix(argument, "--"):
			return fmt.Errorf("unknown status flag %q", argument)
		case sessionID == "":
			sessionID = argument
		default:
			return errors.New("import-status accepts at most one import id")
		}
	}
	cfg, err := resolveConfig(dataDir)
	if err != nil {
		return err
	}
	store, err := storage.Open(cfg.DataDir)
	if err != nil {
		return err
	}
	app := service.New(store, clock.System{})

	if sessionID != "" {
		session, err := app.GetImport(sessionID)
		if err != nil {
			return err
		}
		printSession(session, false)
		return nil
	}
	sessions, err := app.ListImports()
	if err != nil {
		return err
	}
	if len(sessions) == 0 {
		fmt.Println("no import sessions recorded")
		return nil
	}
	for _, session := range sessions {
		fmt.Printf("%s  %-9s  formulas=%d vessels=%d runs=%d issues=%d  %s\n",
			session.ID, session.Status,
			session.Counts.Formulas, session.Counts.Vessels, session.Counts.Runs,
			len(session.Issues), session.Filename)
	}
	return nil
}

func runImportConfirm(arguments []string) error {
	if len(arguments) < 1 {
		return errors.New("usage: fermentctl import-confirm <import-id> [--data-dir DIR]")
	}
	dataDir := ""
	sessionID := arguments[0]
	for _, argument := range arguments[1:] {
		switch {
		case strings.HasPrefix(argument, "--data-dir="):
			dataDir = strings.TrimPrefix(argument, "--data-dir=")
		default:
			return fmt.Errorf("unknown confirm flag %q", argument)
		}
	}
	cfg, err := resolveConfig(dataDir)
	if err != nil {
		return err
	}
	store, err := storage.Open(cfg.DataDir)
	if err != nil {
		return err
	}
	app := service.New(store, clock.System{})
	session, err := app.ConfirmImport(sessionID)
	if err != nil {
		return err
	}
	fmt.Printf("import %s completed: %d entities committed atomically\n",
		session.ID, len(session.Items))
	return nil
}

func printSession(session domain.ImportSession, reused bool) {
	banner := "new import session"
	if reused {
		banner = "identical file already uploaded (reusing existing session)"
	}
	fmt.Printf("import %s  status=%s  %s\n", session.ID, session.Status, banner)
	if session.Filename != "" {
		fmt.Printf("file: %s (sha256 %s)\n", session.Filename, session.ContentHash)
	} else {
		fmt.Printf("file sha256: %s\n", session.ContentHash)
	}
	fmt.Printf("rows resolved: %d formulas, %d steps, %d vessels, %d runs\n",
		session.Counts.Formulas, session.Counts.Steps,
		session.Counts.Vessels, session.Counts.Runs)

	fmt.Printf("\nwould create %d entit%s:\n", len(session.Items), plural(len(session.Items), "y", "ies"))
	for _, item := range session.Items {
		switch item.Kind {
		case domain.ImportFormula:
			note := ""
			if item.Approved {
				note = " [approved]"
			}
			fmt.Printf("  line %3d  formula %-30s steps=%d%s\n", item.Line, item.Key, item.Steps, note)
		case domain.ImportVessel:
			fmt.Printf("  line %3d  vessel  %-30s\n", item.Line, item.Key)
		case domain.ImportRun:
			fmt.Printf("  line %3d  run     %-30s formula=%s\n", item.Line, item.Key, item.FormulaKey)
		}
	}

	if len(session.Conflicts) > 0 {
		fmt.Printf("\nconflicts with existing cellar data (%d, will be skipped):\n",
			len(session.Conflicts))
		for _, conflict := range session.Conflicts {
			fmt.Printf("  line %3d  %-7s  %-30s  %s\n",
				conflict.Line, conflict.Entity, conflict.Key, conflict.Detail)
		}
	}
	if len(session.Issues) > 0 {
		fmt.Printf("\nblocking issues (%d, confirmation refused until resolved):\n",
			len(session.Issues))
		for _, issue := range session.Issues {
			field := ""
			if issue.Field != "" {
				field = " [" + issue.Field + "]"
			}
			fmt.Printf("  line %3d%s  %s\n", issue.Line, field, issue.Detail)
		}
	}
}

func plural(n int, singular, pluralForm string) string {
	if n == 1 {
		return singular
	}
	return pluralForm
}

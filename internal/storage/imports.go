package storage

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"time"
)

// Import session lifecycle states.
const (
	ImportPreviewed = "previewed"
	ImportApplying  = "applying"
	ImportCompleted = "completed"
)

var importIDPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

// ImportItem records how one entity declaration in the uploaded file was
// handled when the session snapshot was taken.
type ImportItem struct {
	Kind   string `json:"kind"`
	Line   int    `json:"line"`
	Action string `json:"action"`
	ID     string `json:"id,omitempty"`
	Name   string `json:"name,omitempty"`
	Code   string `json:"code,omitempty"`
	Ref    string `json:"ref,omitempty"`
}

// ImportSession is the durable progress ledger for one uploaded file.
type ImportSession struct {
	ID               string          `json:"id"`
	Filename         string          `json:"filename"`
	ByteSize         int             `json:"byte_size"`
	SHA256           string          `json:"sha256"`
	Status           string          `json:"status"`
	RawContentBase64 string          `json:"raw_content_base64"`
	Plan             json.RawMessage `json:"plan,omitempty"`
	Items            []ImportItem    `json:"items,omitempty"`
	Revision         int64           `json:"revision,omitempty"`
	CreatedAt        time.Time       `json:"created_at"`
	UpdatedAt        time.Time       `json:"updated_at"`
	CompletedAt      *time.Time      `json:"completed_at,omitempty"`
	Failure          string          `json:"failure,omitempty"`
}

// ImportStore keeps durable import session ledgers in <data-dir>/imports.
// The snapshot itself remains the system of record for business entities;
// these files only track upload and progress state so an interrupted
// import can be inspected and reconciled after restart.
type ImportStore struct {
	mu  sync.Mutex
	dir string
	now func() time.Time
}

// OpenImportStore opens (creating if needed) the imports ledger directory.
func OpenImportStore(dataDir string) (*ImportStore, error) {
	dir := filepath.Join(dataDir, "imports")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create imports directory: %w", err)
	}
	return &ImportStore{
		dir: dir,
		now: func() time.Time { return time.Now().UTC() },
	}, nil
}

// ValidImportID reports whether id is a safe ledger file key.
func ValidImportID(id string) bool {
	return importIDPattern.MatchString(id)
}

func (s *ImportStore) path(id string) string {
	return filepath.Join(s.dir, id+".json")
}

// Get reads a session ledger.
func (s *ImportStore) Get(id string) (ImportSession, bool, error) {
	if !ValidImportID(id) {
		return ImportSession{}, false, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.load(id)
}

func (s *ImportStore) load(id string) (ImportSession, bool, error) {
	raw, err := os.ReadFile(s.path(id))
	if errors.Is(err, os.ErrNotExist) {
		return ImportSession{}, false, nil
	}
	if err != nil {
		return ImportSession{}, false, fmt.Errorf("read import session: %w", err)
	}
	var session ImportSession
	if err := json.Unmarshal(raw, &session); err != nil {
		return ImportSession{}, false, fmt.Errorf("decode import session: %w", err)
	}
	if session.ID != id {
		return ImportSession{}, false, fmt.Errorf("import session id does not match its file")
	}
	return session, true, nil
}

// Save atomically replaces a session ledger.
func (s *ImportStore) Save(session ImportSession) error {
	if !ValidImportID(session.ID) {
		return fmt.Errorf("invalid import session id")
	}
	if session.CreatedAt.IsZero() {
		session.CreatedAt = s.now()
	}
	session.UpdatedAt = s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, err := json.MarshalIndent(session, "", "  ")
	if err != nil {
		return fmt.Errorf("encode import session: %w", err)
	}
	if err := writeFileAtomic(s.path(session.ID), raw); err != nil {
		return err
	}
	return nil
}

// List returns every ledger ordered by creation time, newest first.
func (s *ImportStore) List() ([]ImportSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, fmt.Errorf("list import sessions: %w", err)
	}
	sessions := make([]ImportSession, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || filepath.Ext(name) != ".json" {
			continue
		}
		id := name[:len(name)-len(".json")]
		session, ok, err := s.load(id)
		if err != nil {
			return nil, err
		}
		if ok {
			sessions = append(sessions, session)
		}
	}
	sort.Slice(sessions, func(i, j int) bool {
		if sessions[i].CreatedAt.Equal(sessions[j].CreatedAt) {
			return sessions[i].ID > sessions[j].ID
		}
		return sessions[i].CreatedAt.After(sessions[j].CreatedAt)
	})
	return sessions, nil
}

func writeFileAtomic(path string, content []byte) error {
	directory := filepath.Dir(path)
	handle, err := os.CreateTemp(directory, ".import-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary import session: %w", err)
	}
	tempName := handle.Name()
	removeTemp := true
	defer func() {
		if removeTemp {
			_ = os.Remove(tempName)
		}
	}()
	if _, err := handle.Write(content); err != nil {
		_ = handle.Close()
		return fmt.Errorf("write import session: %w", err)
	}
	if err := handle.Sync(); err != nil {
		_ = handle.Close()
		return fmt.Errorf("commit import session bytes: %w", err)
	}
	if err := handle.Close(); err != nil {
		return fmt.Errorf("close import session: %w", err)
	}
	if err := os.Rename(tempName, path); err != nil {
		return fmt.Errorf("replace import session: %w", err)
	}
	removeTemp = false
	return nil
}

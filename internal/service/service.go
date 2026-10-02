package service

import (
	"strings"
	"sync"

	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/clock"
	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/domain"
	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/storage"
)

type Service struct {
	store        *storage.Store
	imports      *storage.ImportStore
	clock        clock.Clock
	confirmLocks sync.Map
}

// New builds a service. importLedger may be nil when the import feature is
// not configured (for example inside isolated workflow diagnostics); every
// import endpoint then fails with a stable storage error.
func New(store *storage.Store, importLedger *storage.ImportStore, timeSource clock.Clock) *Service {
	if timeSource == nil {
		timeSource = clock.System{}
	}
	return &Service{store: store, imports: importLedger, clock: timeSource}
}

// sessionLock serializes confirm/resume attempts for one import session
// within this process, mirroring the serialized snapshot writer.
func (s *Service) sessionLock(id string) *sync.Mutex {
	lock, _ := s.confirmLocks.LoadOrStore(id, &sync.Mutex{})
	return lock.(*sync.Mutex)
}

func (s *Service) Revision() int64 {
	return s.store.Revision()
}

func (s *Service) checkExpected(current int64, expected *int64) error {
	if expected != nil && current != *expected {
		return domain.NewError(domain.CodeRevisionConflict,
			"the aggregate version does not match X-Expected-Version")
	}
	return nil
}

func generateEntityID(prefix string) (string, error) {
	return domain.NewID(prefix)
}

func generatedRunCode() (string, error) {
	value, err := domain.NewID("run")
	if err != nil {
		return "", err
	}
	suffix := strings.ToUpper(strings.TrimPrefix(value, "run_"))
	return "RUN-" + suffix, nil
}

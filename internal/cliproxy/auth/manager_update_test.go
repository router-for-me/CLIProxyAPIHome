package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPIHome/internal/registry"
)

type blockingVersionedUpdateStore struct {
	mu              sync.Mutex
	persisted       *Auth
	version         int64
	calls           int
	saveErr         error
	deleteErr       error
	firstCommitted  chan struct{}
	releaseFirst    chan struct{}
	secondSave      chan struct{}
	secondCommitted chan struct{}
}

type perAuthBlockingUpdateStore struct {
	mu          sync.Mutex
	persisted   map[string]*Auth
	blockedID   string
	blockActive bool
	blockStart  chan struct{}
	release     chan struct{}
	blockDone   chan struct{}
}

func newPerAuthBlockingUpdateStore(auths []*Auth, blockedID string) *perAuthBlockingUpdateStore {
	persisted := make(map[string]*Auth, len(auths))
	for _, auth := range auths {
		if auth != nil {
			persisted[auth.ID] = auth.Clone()
		}
	}
	return &perAuthBlockingUpdateStore{
		persisted:  persisted,
		blockedID:  blockedID,
		blockStart: make(chan struct{}),
		release:    make(chan struct{}),
		blockDone:  make(chan struct{}),
	}
}

func (s *perAuthBlockingUpdateStore) List(context.Context) ([]*Auth, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	auths := make([]*Auth, 0, len(s.persisted))
	for _, auth := range s.persisted {
		auths = append(auths, auth.Clone())
	}
	return auths, nil
}

func (s *perAuthBlockingUpdateStore) Save(ctx context.Context, auth *Auth) (string, error) {
	id, _, errSave := s.SaveWithStateVersion(ctx, auth)
	return id, errSave
}

func (s *perAuthBlockingUpdateStore) SaveWithStateVersion(ctx context.Context, auth *Auth) (string, int64, error) {
	if auth == nil {
		return "", 0, nil
	}
	s.mu.Lock()
	shouldBlock := auth.ID == s.blockedID && !s.blockActive
	if shouldBlock {
		s.blockActive = true
		close(s.blockStart)
	}
	s.mu.Unlock()
	if shouldBlock {
		select {
		case <-s.release:
		case <-ctx.Done():
			return "", 0, ctx.Err()
		}
	}

	s.mu.Lock()
	current := s.persisted[auth.ID]
	if current != nil && auth.StateVersion > 0 && auth.StateVersion < current.StateVersion {
		s.mu.Unlock()
		return auth.ID, 0, nil
	}
	stateVersion := auth.StateVersion + 1
	if current != nil && stateVersion <= current.StateVersion {
		stateVersion = current.StateVersion + 1
	}
	persisted := auth.Clone()
	persisted.StateVersion = stateVersion
	s.persisted[auth.ID] = persisted
	if shouldBlock {
		close(s.blockDone)
	}
	s.mu.Unlock()
	return auth.ID, stateVersion, nil
}

func (s *perAuthBlockingUpdateStore) Delete(_ context.Context, id string) error {
	s.mu.Lock()
	delete(s.persisted, id)
	s.mu.Unlock()
	return nil
}

type retryingBackgroundPersistStore struct {
	mu        sync.Mutex
	calls     int
	deadlines []bool
	saved     chan *Auth
}

type workerLimitedPersistStore struct {
	mu          sync.Mutex
	active      int
	maximum     int
	calls       int
	started     chan string
	completed   chan string
	releaseSave chan struct{}
}

func newWorkerLimitedPersistStore(capacity int) *workerLimitedPersistStore {
	return &workerLimitedPersistStore{
		started:     make(chan string, capacity),
		completed:   make(chan string, capacity),
		releaseSave: make(chan struct{}),
	}
}

func (*workerLimitedPersistStore) List(context.Context) ([]*Auth, error) {
	return nil, nil
}

func (s *workerLimitedPersistStore) Save(ctx context.Context, auth *Auth) (string, error) {
	s.mu.Lock()
	s.active++
	s.calls++
	if s.active > s.maximum {
		s.maximum = s.active
	}
	s.mu.Unlock()

	s.started <- auth.ID
	select {
	case <-s.releaseSave:
	case <-ctx.Done():
		s.mu.Lock()
		s.active--
		s.mu.Unlock()
		return "", ctx.Err()
	}

	s.mu.Lock()
	s.active--
	s.mu.Unlock()
	s.completed <- auth.ID
	return auth.ID, nil
}

func (*workerLimitedPersistStore) Delete(context.Context, string) error {
	return nil
}

func (s *workerLimitedPersistStore) snapshot() (int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls, s.maximum
}

func newRetryingBackgroundPersistStore() *retryingBackgroundPersistStore {
	return &retryingBackgroundPersistStore{saved: make(chan *Auth, 1)}
}

func (s *retryingBackgroundPersistStore) List(context.Context) ([]*Auth, error) {
	return nil, nil
}

func (s *retryingBackgroundPersistStore) Save(ctx context.Context, auth *Auth) (string, error) {
	_, hasDeadline := ctx.Deadline()
	s.mu.Lock()
	s.calls++
	call := s.calls
	s.deadlines = append(s.deadlines, hasDeadline)
	s.mu.Unlock()
	if call == 1 {
		<-ctx.Done()
		return "", ctx.Err()
	}
	s.saved <- auth.Clone()
	return auth.ID, nil
}

func (s *retryingBackgroundPersistStore) Delete(context.Context, string) error {
	return nil
}

func (s *retryingBackgroundPersistStore) snapshot() (int, []bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls, append([]bool(nil), s.deadlines...)
}

func newBlockingVersionedUpdateStore(auth *Auth) *blockingVersionedUpdateStore {
	return &blockingVersionedUpdateStore{
		persisted:       auth.Clone(),
		version:         auth.StateVersion,
		firstCommitted:  make(chan struct{}),
		releaseFirst:    make(chan struct{}),
		secondSave:      make(chan struct{}, 1),
		secondCommitted: make(chan struct{}, 1),
	}
}

func (s *blockingVersionedUpdateStore) List(context.Context) ([]*Auth, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return []*Auth{s.persisted.Clone()}, nil
}

func (s *blockingVersionedUpdateStore) Save(ctx context.Context, auth *Auth) (string, error) {
	id, _, errSave := s.SaveWithStateVersion(ctx, auth)
	return id, errSave
}

func (s *blockingVersionedUpdateStore) SaveWithStateVersion(_ context.Context, auth *Auth) (string, int64, error) {
	s.mu.Lock()
	if s.saveErr != nil {
		errSave := s.saveErr
		s.mu.Unlock()
		return "", 0, errSave
	}
	s.calls++
	call := s.calls
	if call > 1 {
		select {
		case s.secondSave <- struct{}{}:
		default:
		}
	}
	if auth.StateVersion > 0 && auth.StateVersion < s.version {
		s.mu.Unlock()
		return auth.ID, 0, nil
	}
	s.version++
	stateVersion := s.version
	s.persisted = auth.Clone()
	s.persisted.StateVersion = stateVersion
	if call == 1 {
		close(s.firstCommitted)
		s.mu.Unlock()
		<-s.releaseFirst
		return auth.ID, stateVersion, nil
	}
	select {
	case s.secondCommitted <- struct{}{}:
	default:
	}
	s.mu.Unlock()
	return auth.ID, stateVersion, nil
}

type nonVersionedUpdateStore struct {
	mu        sync.Mutex
	persisted *Auth
	saveCalls int
}

func (s *nonVersionedUpdateStore) List(context.Context) ([]*Auth, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.persisted == nil {
		return nil, nil
	}
	return []*Auth{s.persisted.Clone()}, nil
}

func (s *nonVersionedUpdateStore) Save(_ context.Context, auth *Auth) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saveCalls++
	s.persisted = auth.Clone()
	return auth.ID, nil
}

func (s *nonVersionedUpdateStore) Delete(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.persisted != nil && s.persisted.ID == id {
		s.persisted = nil
	}
	return nil
}

func (s *nonVersionedUpdateStore) snapshot() (*Auth, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.persisted.Clone(), s.saveCalls
}

func (s *blockingVersionedUpdateStore) Delete(context.Context, string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.deleteErr
}

func (s *blockingVersionedUpdateStore) GetFullAuth(_ context.Context, id string) (*Auth, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.persisted == nil || s.persisted.ID != id {
		return nil, ErrFullAuthNotFound
	}
	return s.persisted.Clone(), nil
}

func (s *blockingVersionedUpdateStore) replacePersisted(auth *Auth) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.persisted = auth.Clone()
	s.version = auth.StateVersion
}

func (s *blockingVersionedUpdateStore) removePersisted(version int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.persisted = nil
	s.version = version
}

func (s *blockingVersionedUpdateStore) setSaveError(errSave error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saveErr = errSave
}

func (s *blockingVersionedUpdateStore) setDeleteError(errDelete error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deleteErr = errDelete
}

func (s *blockingVersionedUpdateStore) snapshot() *Auth {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.persisted.Clone()
}

func TestManagerUpdateRejectsOlderStateVersion(t *testing.T) {
	t.Parallel()

	const authID = "versioned-auth"
	manager := NewManager(nil, nil, nil)
	newer := &Auth{
		ID:           authID,
		Index:        authID,
		Provider:     "antigravity",
		Status:       StatusError,
		Unavailable:  true,
		StateVersion: 11,
		LastError: &Error{
			Message:    "credential unauthorized",
			HTTPStatus: http.StatusUnauthorized,
		},
	}
	if _, errRegister := manager.Register(WithSkipPersist(context.Background()), newer); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}

	older := newer.Clone()
	older.StateVersion = 10
	older.Status = StatusActive
	older.Unavailable = false
	older.LastError = nil
	updated, errUpdate := manager.Update(WithSkipPersist(context.Background()), older)
	if errUpdate != nil {
		t.Fatalf("Update() error = %v", errUpdate)
	}
	if updated.StateVersion != 11 || !updated.Unavailable || updated.LastError == nil || updated.LastError.HTTPStatus != http.StatusUnauthorized {
		t.Fatalf("Update() result = %#v, want version 11 unauthorized state", updated)
	}
	current, ok := manager.GetByID(authID)
	if !ok || current == nil || current.StateVersion != 11 || !current.Unavailable || current.LastError == nil || current.LastError.HTTPStatus != http.StatusUnauthorized {
		t.Fatalf("GetByID() = %#v, want version 11 unauthorized state", current)
	}
}

func TestManagerUpdateSerializesPersistenceBeforeAcceptingAnotherSnapshot(t *testing.T) {
	const authID = "serialized-versioned-auth"
	base := &Auth{
		ID:           authID,
		Index:        authID,
		Provider:     "antigravity",
		Status:       StatusActive,
		StateVersion: 10,
		Metadata:     map[string]any{"access_token": "stored-token"},
	}
	store := newBlockingVersionedUpdateStore(base)
	manager := NewManager(store, nil, nil)
	if _, errRegister := manager.Register(WithSkipPersist(context.Background()), base); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}

	type updateResult struct {
		auth *Auth
		err  error
	}
	first := base.Clone()
	first.Label = "first"
	firstDone := make(chan updateResult, 1)
	go func() {
		updated, errUpdate := manager.Update(context.Background(), first)
		firstDone <- updateResult{auth: updated, err: errUpdate}
	}()
	select {
	case <-store.firstCommitted:
	case <-time.After(2 * time.Second):
		t.Fatal("first Update() did not reach persistence")
	}
	currentBeforeCommit, okBeforeCommit := manager.GetByID(authID)
	if !okBeforeCommit || currentBeforeCommit == nil || currentBeforeCommit.Label != "" || currentBeforeCommit.StateVersion != base.StateVersion {
		t.Fatalf("GetByID() while persistence is pending = %#v, want original snapshot", currentBeforeCommit)
	}

	second := base.Clone()
	second.Label = "second"
	secondDone := make(chan updateResult, 1)
	go func() {
		updated, errUpdate := manager.Update(context.Background(), second)
		secondDone <- updateResult{auth: updated, err: errUpdate}
	}()
	select {
	case <-store.secondSave:
	case <-time.After(100 * time.Millisecond):
	}
	close(store.releaseFirst)

	for name, done := range map[string]<-chan updateResult{"first": firstDone, "second": secondDone} {
		select {
		case result := <-done:
			if result.err != nil {
				t.Fatalf("%s Update() error = %v", name, result.err)
			}
			if result.auth == nil || result.auth.Label != "first" || result.auth.StateVersion != 11 {
				t.Fatalf("%s Update() result = %#v, want persisted first snapshot at version 11", name, result.auth)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("%s Update() did not finish", name)
		}
	}

	current, ok := manager.GetByID(authID)
	if !ok || current == nil || current.Label != "first" || current.StateVersion != 11 {
		t.Fatalf("GetByID() = %#v, want persisted first snapshot at version 11", current)
	}
	persisted := store.snapshot()
	if persisted == nil || persisted.Label != current.Label || persisted.StateVersion != current.StateVersion {
		t.Fatalf("persisted auth = %#v, in-memory auth = %#v", persisted, current)
	}
}

func TestManagerUpdateSerializesConcurrentResultTransition(t *testing.T) {
	const authID = "serialized-result-auth"
	const model = "gemini-3-pro"
	base := &Auth{
		ID:           authID,
		Index:        authID,
		Provider:     "gemini",
		Status:       StatusActive,
		StateVersion: 10,
		Metadata:     map[string]any{"access_token": "stored-token"},
	}
	store := newBlockingVersionedUpdateStore(base)
	manager := NewManager(store, nil, nil)
	if _, errRegister := manager.Register(WithSkipPersist(context.Background()), base); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}

	updated := base.Clone()
	updated.Label = "persisted update"
	updateDone := make(chan error, 1)
	go func() {
		_, errUpdate := manager.Update(context.Background(), updated)
		updateDone <- errUpdate
	}()
	select {
	case <-store.firstCommitted:
	case <-time.After(2 * time.Second):
		t.Fatal("Update() did not reach persistence")
	}

	resultDone := make(chan struct{})
	go func() {
		manager.MarkResult(context.Background(), Result{
			AuthID:  authID,
			Model:   model,
			Success: false,
			Error: &Error{
				Message:    "upstream unavailable",
				HTTPStatus: http.StatusServiceUnavailable,
			},
		})
		close(resultDone)
	}()
	close(store.releaseFirst)

	select {
	case errUpdate := <-updateDone:
		if errUpdate != nil {
			t.Fatalf("Update() error = %v", errUpdate)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Update() did not finish")
	}
	select {
	case <-resultDone:
	case <-time.After(2 * time.Second):
		t.Fatal("MarkResult() did not finish")
	}
	select {
	case <-store.secondCommitted:
	case <-time.After(2 * time.Second):
		t.Fatal("result transition was not persisted")
	}

	current, ok := manager.GetByID(authID)
	if !ok || current == nil || current.Label != updated.Label || current.Failed != 1 {
		t.Fatalf("GetByID() = %#v, want updated snapshot with one failed result", current)
	}
	state := current.ModelStates[model]
	if state == nil || !state.Unavailable || state.Status != StatusError {
		t.Fatalf("in-memory model state = %#v, want persisted upstream failure", state)
	}
	persisted := store.snapshot()
	if persisted == nil || persisted.Label != updated.Label || persisted.Failed != 1 {
		t.Fatalf("persisted auth = %#v, want updated snapshot with one failed result", persisted)
	}
	persistedState := persisted.ModelStates[model]
	if persistedState == nil || !persistedState.Unavailable || persistedState.Status != StatusError {
		t.Fatalf("persisted model state = %#v, want upstream failure", persistedState)
	}
}

func TestManagerBackgroundPersistDoesNotBlockDifferentAuthUpdate(t *testing.T) {
	first := &Auth{
		ID:           "slow-background-auth",
		Index:        "slow-background-auth",
		Provider:     "gemini",
		Status:       StatusActive,
		StateVersion: 10,
		Metadata:     map[string]any{"access_token": "first-token"},
	}
	second := &Auth{
		ID:           "independent-update-auth",
		Index:        "independent-update-auth",
		Provider:     "gemini",
		Status:       StatusActive,
		StateVersion: 20,
		Metadata:     map[string]any{"access_token": "second-token"},
	}
	store := newPerAuthBlockingUpdateStore([]*Auth{first, second}, first.ID)
	manager := NewManager(store, nil, nil)
	t.Cleanup(manager.Shutdown)
	for _, auth := range []*Auth{first, second} {
		if _, errRegister := manager.Register(WithSkipPersist(context.Background()), auth); errRegister != nil {
			t.Fatalf("Register(%s) error = %v", auth.ID, errRegister)
		}
	}

	manager.enqueueResultPersist(context.Background(), first)
	select {
	case <-store.blockStart:
	case <-time.After(2 * time.Second):
		t.Fatal("background persistence did not reach the store")
	}

	updatedSecond := second.Clone()
	updatedSecond.Label = "updated while first auth is blocked"
	secondDone := make(chan error, 1)
	go func() {
		_, errUpdate := manager.Update(context.Background(), updatedSecond)
		secondDone <- errUpdate
	}()
	select {
	case errUpdate := <-secondDone:
		if errUpdate != nil {
			t.Fatalf("Update(second auth) error = %v", errUpdate)
		}
	case <-time.After(time.Second):
		t.Fatal("different auth update was blocked by background persistence")
	}

	close(store.release)
	select {
	case <-store.blockDone:
	case <-time.After(2 * time.Second):
		t.Fatal("background persistence did not finish after release")
	}
	currentSecond, okSecond := manager.GetByID(second.ID)
	if !okSecond || currentSecond == nil || currentSecond.Label != updatedSecond.Label {
		t.Fatalf("GetByID(second auth) = %#v, want independent update", currentSecond)
	}
}

func TestManagerBackgroundPersistUsesDeadlineAndRetries(t *testing.T) {
	const authID = "retry-background-auth"
	store := newRetryingBackgroundPersistStore()
	manager := NewManager(store, nil, nil)
	t.Cleanup(manager.Shutdown)
	manager.resultPersistTimeout = 25 * time.Millisecond
	manager.resultPersistRetryBackoff = 10 * time.Millisecond
	auth := &Auth{
		ID:       authID,
		Index:    authID,
		Provider: "gemini",
		Status:   StatusActive,
		Metadata: map[string]any{"access_token": "stored-token"},
	}
	if _, errRegister := manager.Register(WithSkipPersist(context.Background()), auth); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}

	manager.enqueueResultPersist(context.Background(), auth)
	select {
	case saved := <-store.saved:
		if saved == nil || saved.ID != authID {
			t.Fatalf("retried snapshot = %#v, want auth %s", saved, authID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("background persistence did not retry after its deadline")
	}

	calls, deadlines := store.snapshot()
	if calls != 2 {
		t.Fatalf("Save() calls = %d, want one timeout and one retry", calls)
	}
	if len(deadlines) != 2 || !deadlines[0] || !deadlines[1] {
		t.Fatalf("Save() deadline flags = %v, want bounded contexts for both attempts", deadlines)
	}
}

func TestManagerBackgroundPersistBoundsCrossAuthConcurrency(t *testing.T) {
	const (
		authCount   = 6
		workerCount = 2
	)
	store := newWorkerLimitedPersistStore(authCount)
	manager := NewManager(store, nil, nil)
	t.Cleanup(manager.Shutdown)
	manager.resultPersistWorkers = workerCount
	auths := make([]*Auth, 0, authCount)
	for index := 0; index < authCount; index++ {
		authID := fmt.Sprintf("worker-limited-auth-%d", index)
		auth := &Auth{
			ID:       authID,
			Index:    authID,
			Provider: "gemini",
			Status:   StatusActive,
			Metadata: map[string]any{"access_token": "stored-token"},
		}
		if _, errRegister := manager.Register(WithSkipPersist(context.Background()), auth); errRegister != nil {
			t.Fatalf("Register(%s) error = %v", authID, errRegister)
		}
		auths = append(auths, auth)
	}

	for _, auth := range auths {
		manager.enqueueResultPersist(context.Background(), auth)
	}
	for entered := 0; entered < workerCount; entered++ {
		select {
		case <-store.started:
		case <-time.After(2 * time.Second):
			t.Fatalf("background persistence started %d saves, want %d", entered, workerCount)
		}
	}
	select {
	case authID := <-store.started:
		t.Fatalf("background persistence exceeded worker limit with auth %s", authID)
	case <-time.After(100 * time.Millisecond):
	}

	close(store.releaseSave)
	for completed := 0; completed < authCount; completed++ {
		select {
		case <-store.completed:
		case <-time.After(2 * time.Second):
			t.Fatalf("background persistence completed %d saves, want %d", completed, authCount)
		}
	}
	calls, maximum := store.snapshot()
	if calls != authCount {
		t.Fatalf("Save() calls = %d, want %d", calls, authCount)
	}
	if maximum != workerCount {
		t.Fatalf("maximum concurrent Save() calls = %d, want %d", maximum, workerCount)
	}
}

func TestManagerShutdownCancelsInFlightBackgroundPersist(t *testing.T) {
	const authID = "shutdown-in-flight-background-auth"
	store := newWorkerLimitedPersistStore(1)
	manager := NewManager(store, nil, nil)
	manager.resultPersistWorkers = 1
	manager.resultPersistTimeout = time.Second
	auth := &Auth{
		ID:       authID,
		Index:    authID,
		Provider: "gemini",
		Status:   StatusActive,
		Metadata: map[string]any{"access_token": "stored-token"},
	}
	if _, errRegister := manager.Register(WithSkipPersist(context.Background()), auth); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}

	manager.enqueueResultPersist(context.Background(), auth)
	select {
	case <-store.started:
	case <-time.After(2 * time.Second):
		t.Fatal("background persistence did not start")
	}

	shutdownDone := make(chan struct{})
	go func() {
		manager.Shutdown()
		close(shutdownDone)
	}()
	select {
	case <-shutdownDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Shutdown() did not cancel the in-flight persistence")
	}

	manager.enqueueResultPersist(context.Background(), auth)
	select {
	case persistedAuthID := <-store.started:
		t.Fatalf("persistence restarted after shutdown for auth %s", persistedAuthID)
	case <-time.After(50 * time.Millisecond):
	}
	calls, maximum := store.snapshot()
	if calls != 1 || maximum != 1 {
		t.Fatalf("Save() calls/maximum = %d/%d, want 1/1", calls, maximum)
	}
}

func TestManagerShutdownCancelsScheduledBackgroundPersistRetry(t *testing.T) {
	const authID = "shutdown-scheduled-retry-auth"
	store := newRetryingBackgroundPersistStore()
	manager := NewManager(store, nil, nil)
	manager.resultPersistWorkers = 1
	manager.resultPersistTimeout = 20 * time.Millisecond
	manager.resultPersistRetryBackoff = 100 * time.Millisecond
	auth := &Auth{
		ID:       authID,
		Index:    authID,
		Provider: "gemini",
		Status:   StatusActive,
		Metadata: map[string]any{"access_token": "stored-token"},
	}
	if _, errRegister := manager.Register(WithSkipPersist(context.Background()), auth); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}

	manager.enqueueResultPersist(context.Background(), auth)
	retryScheduled := false
	deadline := time.NewTimer(2 * time.Second)
	ticker := time.NewTicker(time.Millisecond)
	defer deadline.Stop()
	defer ticker.Stop()
	for !retryScheduled {
		select {
		case <-deadline.C:
			t.Fatal("background persistence did not schedule a retry")
		case <-ticker.C:
			manager.resultPersistMu.Lock()
			retryScheduled = manager.resultPersistRetryTimers[authID] != nil
			manager.resultPersistMu.Unlock()
		}
	}

	manager.Shutdown()
	select {
	case saved := <-store.saved:
		t.Fatalf("retry persisted after shutdown: %#v", saved)
	case <-time.After(150 * time.Millisecond):
	}
	calls, _ := store.snapshot()
	if calls != 1 {
		t.Fatalf("Save() calls = %d, want only the initial failed attempt", calls)
	}
	manager.resultPersistMu.Lock()
	queued := len(manager.resultPersistQueue)
	pending := len(manager.resultPersistPending)
	active := len(manager.resultPersistActive)
	timers := len(manager.resultPersistRetryTimers)
	manager.resultPersistMu.Unlock()
	if queued != 0 || pending != 0 || active != 0 || timers != 0 {
		t.Fatalf("shutdown queue state = queued:%d pending:%d active:%d timers:%d, want all empty", queued, pending, active, timers)
	}
}

func TestManagerPersistSkipsQueuedSnapshotAfterDelete(t *testing.T) {
	base := &Auth{
		ID:       "deleted-queued-result-auth",
		Index:    "deleted-queued-result-auth",
		Provider: "gemini",
		Status:   StatusActive,
		Metadata: map[string]any{"access_token": "stored-token"},
	}
	store := &nonVersionedUpdateStore{}
	manager := NewManager(store, nil, nil)
	registered, errRegister := manager.Register(context.Background(), base)
	if errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	if errDelete := manager.Delete(context.Background(), base.ID); errDelete != nil {
		t.Fatalf("Delete() error = %v", errDelete)
	}

	if errPersist := manager.persist(context.Background(), registered); errPersist != nil {
		t.Fatalf("persist() error = %v", errPersist)
	}
	persisted, saveCalls := store.snapshot()
	if persisted != nil {
		t.Fatalf("persisted auth = %#v, want deleted auth to stay absent", persisted)
	}
	if saveCalls != 1 {
		t.Fatalf("Save() calls = %d, want only the initial Register() save", saveCalls)
	}
}

func TestRefreshSchedulerEntryRemovesLateSnapshotAfterDelete(t *testing.T) {
	const authID = "deleted-late-scheduler-auth"
	manager := NewManager(nil, nil, nil)
	registered, errRegister := manager.Register(WithSkipPersist(context.Background()), &Auth{
		ID:       authID,
		Index:    authID,
		Provider: "gemini",
		Status:   StatusActive,
	})
	if errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	if errDelete := manager.Delete(WithSkipPersist(context.Background()), authID); errDelete != nil {
		t.Fatalf("Delete() error = %v", errDelete)
	}

	manager.scheduler.upsertAuth(registered)
	manager.scheduler.mu.Lock()
	_, restored := manager.scheduler.authProviders[authID]
	manager.scheduler.mu.Unlock()
	if !restored {
		t.Fatal("test setup did not restore the stale scheduler snapshot")
	}

	manager.RefreshSchedulerEntry(authID)
	manager.scheduler.mu.Lock()
	_, scheduled := manager.scheduler.authProviders[authID]
	manager.scheduler.mu.Unlock()
	if scheduled {
		t.Fatal("RefreshSchedulerEntry kept a scheduler entry for a deleted auth")
	}
}

func TestManagerLocalMutationsWaitForUpdatePublication(t *testing.T) {
	now := time.Now().UTC()
	const model = "gemini-3-pro"
	for _, test := range []struct {
		name    string
		prepare func(*Auth)
		mutate  func(*Manager) bool
		check   func(*Auth) bool
	}{
		{
			name: "refresh pending",
			prepare: func(auth *Auth) {
				auth.Provider = "antigravity"
				auth.Metadata["expires_at"] = now.Add(10 * time.Minute).Format(time.RFC3339Nano)
			},
			mutate: func(manager *Manager) bool {
				return manager.markRefreshPending("refresh pending", now)
			},
			check: func(auth *Auth) bool {
				return auth.NextRefreshAfter.Equal(now.Add(refreshPendingBackoff))
			},
		},
		{
			name: "cooldown clear",
			prepare: func(auth *Auth) {
				retryAt := now.Add(time.Minute)
				auth.Status = StatusError
				auth.Unavailable = true
				auth.Quota = QuotaState{Exceeded: true, Scope: quotaScopeModel, Reason: "quota", NextRecoverAt: retryAt}
				auth.ModelStates = map[string]*ModelState{
					model: {
						Status:         StatusError,
						Unavailable:    true,
						NextRetryAfter: retryAt,
						Quota:          auth.Quota,
						LastError:      &Error{Message: "quota", HTTPStatus: http.StatusTooManyRequests},
					},
				}
			},
			mutate: func(manager *Manager) bool {
				_, changed := manager.clearLocalDisabledCooldownState(context.Background(), "cooldown clear", now)
				return changed
			},
			check: func(auth *Auth) bool {
				state := auth.ModelStates[model]
				return state != nil && !state.Unavailable && !state.Quota.Exceeded && state.NextRetryAfter.IsZero()
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			base := &Auth{
				ID:           test.name,
				Index:        test.name,
				Provider:     "gemini",
				Status:       StatusActive,
				StateVersion: 10,
				Metadata:     map[string]any{"access_token": "stored-token"},
			}
			store := newBlockingVersionedUpdateStore(base)
			manager := NewManager(store, nil, nil)
			if _, errRegister := manager.Register(WithSkipPersist(context.Background()), base); errRegister != nil {
				t.Fatalf("Register() error = %v", errRegister)
			}

			updated := base.Clone()
			test.prepare(updated)
			updateDone := make(chan error, 1)
			go func() {
				_, errUpdate := manager.Update(context.Background(), updated)
				updateDone <- errUpdate
			}()
			select {
			case <-store.firstCommitted:
			case <-time.After(2 * time.Second):
				t.Fatal("Update() did not reach persistence")
			}

			mutationDone := make(chan bool, 1)
			go func() {
				mutationDone <- test.mutate(manager)
			}()
			close(store.releaseFirst)
			select {
			case errUpdate := <-updateDone:
				if errUpdate != nil {
					t.Fatalf("Update() error = %v", errUpdate)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("Update() did not finish")
			}
			select {
			case changed := <-mutationDone:
				if !changed {
					t.Fatal("local mutation did not apply to the published update")
				}
			case <-time.After(2 * time.Second):
				t.Fatal("local mutation did not finish")
			}

			current, ok := manager.GetByID(base.ID)
			if !ok || current == nil || !test.check(current) {
				t.Fatalf("GetByID() = %#v, want local mutation on published update", current)
			}
		})
	}
}

func TestManagerRegisterDoesNotPublishWhenPersistenceFails(t *testing.T) {
	t.Parallel()

	base := &Auth{
		ID:           "register-persistence-error",
		Index:        "register-persistence-error",
		Provider:     "antigravity",
		Status:       StatusActive,
		StateVersion: 10,
		Metadata:     map[string]any{"access_token": "stored-token"},
	}
	store := newBlockingVersionedUpdateStore(base)
	store.setSaveError(errors.New("database unavailable"))
	manager := NewManager(store, nil, nil)

	registered, errRegister := manager.Register(context.Background(), base)
	if registered != nil || errRegister == nil {
		t.Fatalf("Register() = %#v, %v, want persistence error", registered, errRegister)
	}
	if current, ok := manager.GetByID(base.ID); ok || current != nil {
		t.Fatalf("GetByID() = %#v, %v, want no unpublished auth", current, ok)
	}
}

func TestManagerRegisterAdoptsAuthoritativeSnapshotWhenStorageRejectsCandidate(t *testing.T) {
	t.Parallel()

	const model = "register-authoritative-model"
	authoritative := &Auth{
		ID:           "register-authoritative-snapshot",
		Index:        "register-authoritative-snapshot",
		Provider:     "antigravity",
		Status:       StatusDisabled,
		Disabled:     true,
		Unavailable:  true,
		StateVersion: 11,
		Metadata:     map[string]any{"access_token": "rotated-token"},
	}
	store := newBlockingVersionedUpdateStore(authoritative)
	manager := NewManager(store, nil, nil)
	manager.SetFullAuthResolver(store)
	modelRegistry := registry.GetGlobalRegistry()
	modelRegistry.RegisterClient(authoritative.ID, authoritative.Provider, []*registry.ModelInfo{{ID: model, Object: "model", Type: "openai"}})
	t.Cleanup(func() { modelRegistry.UnregisterClient(authoritative.ID) })
	candidate := authoritative.Clone()
	candidate.StateVersion = 10
	candidate.Status = StatusActive
	candidate.Disabled = false
	candidate.Unavailable = false
	candidate.Metadata["access_token"] = "stale-token"

	registered, errRegister := manager.Register(context.Background(), candidate)
	if errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	if registered == nil || registered.StateVersion != authoritative.StateVersion || !registered.Disabled || registered.Metadata["access_token"] != "rotated-token" {
		t.Fatalf("Register() = %#v, want authoritative snapshot", registered)
	}
	current, ok := manager.GetByID(authoritative.ID)
	if !ok || current == nil || current.StateVersion != authoritative.StateVersion || !current.Disabled || current.Metadata["access_token"] != "rotated-token" {
		t.Fatalf("GetByID() = %#v, want authoritative snapshot", current)
	}
	if models := modelRegistry.GetModelsForClient(authoritative.ID); len(models) != 0 {
		t.Fatalf("registry models after authoritative disable = %#v, want none", models)
	}
}

func TestManagerDeleteRetainsAuthWhenPersistenceFails(t *testing.T) {
	t.Parallel()

	base := &Auth{
		ID:           "delete-persistence-error",
		Index:        "delete-persistence-error",
		Provider:     "antigravity",
		Status:       StatusActive,
		StateVersion: 10,
		Metadata:     map[string]any{"access_token": "stored-token"},
	}
	store := newBlockingVersionedUpdateStore(base)
	manager := NewManager(store, nil, nil)
	if _, errRegister := manager.Register(WithSkipPersist(context.Background()), base); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	store.setDeleteError(errors.New("database unavailable"))

	if errDelete := manager.Delete(context.Background(), base.ID); errDelete == nil {
		t.Fatal("Delete() error = nil, want persistence error")
	}
	current, ok := manager.GetByID(base.ID)
	if !ok || current == nil || current.StateVersion != base.StateVersion {
		t.Fatalf("GetByID() = %#v, want retained auth", current)
	}
}

func TestManagerUpdateAdoptsAuthoritativeSnapshotAfterStoreRejectsStaleSave(t *testing.T) {
	t.Parallel()

	const authID = "externally-versioned-auth"
	runtimeMarker := &struct{ name string }{name: "runtime"}
	storageMarker := &testTokenStorage{}
	localModelState := &ModelState{
		Status:         StatusError,
		StatusMessage:  "local transient failure",
		Unavailable:    true,
		NextRetryAfter: time.Now().UTC().Add(time.Minute),
		LastError:      &Error{Message: "upstream unavailable", HTTPStatus: http.StatusServiceUnavailable},
		UpdatedAt:      time.Now().UTC(),
	}
	base := &Auth{
		ID:           authID,
		Index:        authID,
		Provider:     "antigravity",
		Status:       StatusActive,
		StateVersion: 10,
		Metadata:     map[string]any{"access_token": "stale-access-token"},
		ModelStates:  map[string]*ModelState{"local-model": localModelState},
		Runtime:      runtimeMarker,
		Storage:      storageMarker,
		Success:      3,
		Failed:       2,
	}
	store := newBlockingVersionedUpdateStore(base)
	manager := NewManager(store, nil, nil)
	manager.SetFullAuthResolver(store)
	if _, errRegister := manager.Register(WithSkipPersist(context.Background()), base); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}

	authoritative := base.Clone()
	authoritative.StateVersion = 11
	authoritative.Disabled = true
	authoritative.Unavailable = true
	authoritative.Status = StatusDisabled
	authoritative.StatusMessage = "unauthorized"
	authoritative.LastError = newUnauthorizedRefreshError()
	authoritative.ModelStates = nil
	authoritative.Runtime = nil
	authoritative.Storage = nil
	authoritative.Success = 0
	authoritative.Failed = 0
	store.replacePersisted(authoritative)

	stale := base.Clone()
	stale.Label = "stale local update"
	updated, accepted, errUpdate := manager.updateWithAcceptance(context.Background(), stale)
	if errUpdate != nil {
		t.Fatalf("updateWithAcceptance() error = %v", errUpdate)
	}
	if accepted {
		t.Fatal("updateWithAcceptance() accepted a snapshot rejected by storage")
	}
	if updated == nil || updated.StateVersion != authoritative.StateVersion || !updated.Disabled || updated.Label == stale.Label {
		t.Fatalf("updateWithAcceptance() = %#v, want authoritative disabled snapshot", updated)
	}
	if updated.Runtime != runtimeMarker || updated.Storage != storageMarker || updated.Success != base.Success || updated.Failed != base.Failed {
		t.Fatalf("updateWithAcceptance() runtime state = Runtime %#v Storage %#v Success/Failed %d/%d, want local runtime state preserved", updated.Runtime, updated.Storage, updated.Success, updated.Failed)
	}
	if state := updated.ModelStates["local-model"]; state == nil || state.LastError == nil || state.LastError.HTTPStatus != http.StatusServiceUnavailable {
		t.Fatalf("updateWithAcceptance() local model state = %#v, want local execution state preserved", state)
	}
	current, ok := manager.GetByID(authID)
	if !ok || current == nil || current.StateVersion != authoritative.StateVersion || !current.Disabled || current.Label == stale.Label {
		t.Fatalf("GetByID() = %#v, want authoritative disabled snapshot", current)
	}
	persisted := store.snapshot()
	if persisted == nil || persisted.StateVersion != authoritative.StateVersion || !persisted.Disabled || persisted.Label == stale.Label {
		t.Fatalf("persisted auth = %#v, want unchanged authoritative snapshot", persisted)
	}
}

func TestManagerUpdateFailsWhenRejectedSnapshotCannotBeReconciled(t *testing.T) {
	t.Parallel()

	const authID = "unresolved-versioned-auth"
	base := &Auth{
		ID:           authID,
		Index:        authID,
		Provider:     "antigravity",
		Status:       StatusActive,
		StateVersion: 10,
		Metadata:     map[string]any{"access_token": "stale-access-token"},
	}
	store := newBlockingVersionedUpdateStore(base)
	manager := NewManager(store, nil, nil)
	if _, errRegister := manager.Register(WithSkipPersist(context.Background()), base); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}

	authoritative := base.Clone()
	authoritative.StateVersion = 11
	authoritative.Metadata["access_token"] = "rotated-access-token"
	store.replacePersisted(authoritative)

	stale := base.Clone()
	stale.Label = "must not survive"
	updated, errUpdate := manager.Update(context.Background(), stale)
	if updated != nil || errUpdate == nil {
		t.Fatalf("Update() = %#v, %v, want reconciliation error", updated, errUpdate)
	}
	current, ok := manager.GetByID(authID)
	if !ok || current == nil || current.StateVersion != base.StateVersion || current.Label == stale.Label {
		t.Fatalf("GetByID() = %#v, want pre-update snapshot retained", current)
	}
}

func TestManagerUpdateEvictsAuthWhenRejectedSnapshotWasDeletedAuthoritatively(t *testing.T) {
	t.Parallel()

	const (
		authID = "deleted-versioned-auth"
		model  = "deleted-versioned-model"
	)
	base := &Auth{
		ID:           authID,
		Index:        authID,
		Provider:     "antigravity",
		Status:       StatusActive,
		StateVersion: 10,
		Metadata:     map[string]any{"access_token": "stale-access-token"},
	}
	store := newBlockingVersionedUpdateStore(base)
	manager := NewManager(store, nil, nil)
	manager.SetFullAuthResolver(store)
	if _, errRegister := manager.Register(WithSkipPersist(context.Background()), base); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	modelRegistry := registry.GetGlobalRegistry()
	modelRegistry.RegisterClient(authID, base.Provider, []*registry.ModelInfo{{ID: model, Object: "model", Type: "openai"}})
	t.Cleanup(func() { modelRegistry.UnregisterClient(authID) })
	store.removePersisted(11)

	stale := base.Clone()
	stale.Label = "must not survive"
	updated, errUpdate := manager.Update(context.Background(), stale)
	if updated != nil || errUpdate == nil {
		t.Fatalf("Update() = %#v, %v, want authoritative deletion error", updated, errUpdate)
	}
	if current, ok := manager.GetByID(authID); ok || current != nil {
		t.Fatalf("GetByID() = %#v, %v, want authoritative deletion evicted", current, ok)
	}
	if models := modelRegistry.GetModelsForClient(authID); len(models) != 0 {
		t.Fatalf("registry models after authoritative deletion = %#v, want none", models)
	}
}

func TestRefreshNowObservedAdoptsAuthoritativeSnapshotAfterStoreRejectsOutcome(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name          string
		disableStored bool
		refresh       func(*Auth) (*Auth, bool, error)
		wantErrorCode string
	}{
		{
			name: "transient failure observes rotated token",
			refresh: func(*Auth) (*Auth, bool, error) {
				return nil, true, errors.New("proxy connection refused")
			},
		},
		{
			name: "unsupported refresh observes rotated token",
			refresh: func(*Auth) (*Auth, bool, error) {
				return nil, false, nil
			},
		},
		{
			name:          "successful refresh observes disabled credential",
			disableStored: true,
			refresh: func(target *Auth) (*Auth, bool, error) {
				updated := target.Clone()
				updated.Metadata["access_token"] = "provider-refreshed-access-token"
				return updated, true, nil
			},
			wantErrorCode: refreshAuthErrorCode,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			authID := "refresh-" + test.name
			base := &Auth{
				ID:           authID,
				Index:        authID,
				Provider:     "plugin-refresh-test",
				Status:       StatusActive,
				StateVersion: 10,
				Metadata: map[string]any{
					"access_token": "observed-access-token",
				},
			}
			store := newBlockingVersionedUpdateStore(base)
			manager := NewManager(store, nil, nil)
			manager.SetFullAuthResolver(store)
			if _, errRegister := manager.Register(WithSkipPersist(context.Background()), base); errRegister != nil {
				t.Fatalf("Register() error = %v", errRegister)
			}

			authoritative := base.Clone()
			authoritative.StateVersion = 11
			authoritative.Metadata["access_token"] = "rotated-access-token"
			if test.disableStored {
				authoritative.Disabled = true
				authoritative.Unavailable = true
				authoritative.Status = StatusDisabled
				authoritative.StatusMessage = "unauthorized"
				authoritative.LastError = newUnauthorizedRefreshError()
			}
			manager.SetPluginAuthRefresher(refreshTestPluginRefresher(func(_ context.Context, target *Auth) (*Auth, bool, error) {
				store.replacePersisted(authoritative)
				return test.refresh(target)
			}))

			updated, errRefresh := manager.RefreshNowObserved(context.Background(), authID, AccessTokenSHA256(base))
			if test.wantErrorCode != "" {
				if updated != nil {
					t.Fatalf("RefreshNowObserved() auth = %#v, want no disabled credential", updated)
				}
				var authErr *Error
				if !errors.As(errRefresh, &authErr) || authErr.Code != test.wantErrorCode {
					t.Fatalf("RefreshNowObserved() error = %#v, want code %q", errRefresh, test.wantErrorCode)
				}
			} else {
				if errRefresh != nil {
					t.Fatalf("RefreshNowObserved() error = %v", errRefresh)
				}
				if updated == nil || updated.StateVersion != authoritative.StateVersion || updated.Metadata["access_token"] != "rotated-access-token" {
					t.Fatalf("RefreshNowObserved() auth = %#v, want authoritative rotated token", updated)
				}
			}

			current, ok := manager.GetByID(authID)
			if !ok || current == nil || current.StateVersion != authoritative.StateVersion || current.Disabled != authoritative.Disabled || current.Metadata["access_token"] != authoritative.Metadata["access_token"] {
				t.Fatalf("GetByID() = %#v, want authoritative snapshot", current)
			}
		})
	}
}

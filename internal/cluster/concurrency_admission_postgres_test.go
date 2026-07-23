package cluster

import (
	"context"
	"database/sql"
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/gorm"
)

func TestPostgresAdmitCredentialConcurrencySerializesLastSlot(t *testing.T) {
	repo := newPostgresQuiescenceRepository(t)
	ctx, cancelCtx := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelCtx()
	seedConcurrencyAdmissionLifetime(t, repo, "fp-a", "home-a")
	seedConcurrencyAdmissionLifetime(t, repo, "fp-b", "home-b")
	seedConcurrencyAdmissionAuth(t, repo, "cred-1")
	seedConcurrencyAdmissionPolicy(t, repo, "cred-1", int64Pointer(1), map[string]int64{"gpt": 1})

	firstLocked := make(chan struct{})
	secondLocked := make(chan struct{})
	firstPID := make(chan postgresBackendPIDResult, 1)
	secondPID := make(chan postgresBackendPIDResult, 1)
	releaseFirst := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(releaseFirst) })
	})

	results := make(chan error, 2)
	go func() {
		admissionCtx := context.WithValue(ctx, concurrencyContenderBackendPIDContextKey{}, postgresContenderBackendPIDHook(firstPID))
		admissionCtx = context.WithValue(admissionCtx, concurrencyAdmissionPolicyLockAcquiredContextKey{}, func() {
			close(firstLocked)
			<-releaseFirst
		})
		_, errAdmit := repo.AdmitCredentialConcurrency(admissionCtx, ConcurrencyAdmissionRequest{
			CredentialID: "cred-1", Model: "gpt", Lifetime: concurrencyAdmissionLifetime(t, repo, "fp-a", "home-a"), ProtocolVersion: 1,
		})
		results <- errAdmit
	}()
	select {
	case <-firstLocked:
	case <-ctx.Done():
		t.Fatal("first admission did not reach the transaction linearization lock")
	}

	go func() {
		admissionCtx := context.WithValue(ctx, concurrencyContenderBackendPIDContextKey{}, postgresContenderBackendPIDHook(secondPID))
		admissionCtx = context.WithValue(admissionCtx, concurrencyAdmissionPolicyLockAcquiredContextKey{}, func() {
			close(secondLocked)
		})
		_, errAdmit := repo.AdmitCredentialConcurrency(admissionCtx, ConcurrencyAdmissionRequest{
			CredentialID: "cred-1", Model: "gpt(high)", Lifetime: concurrencyAdmissionLifetime(t, repo, "fp-b", "home-b"), ProtocolVersion: 1,
		})
		results <- errAdmit
	}()

	waitForPostgresContendersBlockedByHolder(t, ctx, repo, firstPID, secondPID)
	releaseOnce.Do(func() { close(releaseFirst) })
	select {
	case <-secondLocked:
	case <-ctx.Done():
		t.Fatal("second admission did not reach the transaction linearization lock")
	}

	passed, saturated := 0, 0
	for range 2 {
		select {
		case errResult := <-results:
			if errResult == nil {
				passed++
			} else if IsConcurrencySaturated(errResult) {
				saturated++
			} else {
				t.Fatalf("admission error = %v", errResult)
			}
		case <-ctx.Done():
			t.Fatal("admissions did not finish")
		}
	}
	if passed != 1 || saturated != 1 {
		t.Fatalf("passed=%d saturated=%d", passed, saturated)
	}
	if got := countConcurrencyAdmissionCounters(t, repo); got != 1 {
		t.Fatalf("counter total = %d, want 1", got)
	}
}

func TestPostgresPolicyAdmissionReleaseRace(t *testing.T) {
	for _, testCase := range []struct {
		name         string
		initialLimit int64
		patchedLimit int64
	}{
		{name: "lower", initialLimit: 2, patchedLimit: 1},
		{name: "raise", initialLimit: 1, patchedLimit: 2},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			repo := newPostgresQuiescenceRepository(t)
			ctx, cancelCtx := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancelCtx()
			seedConcurrencyAdmissionLifetime(t, repo, "fp-a", "home-a")
			seedConcurrencyAdmissionAuth(t, repo, "cred-1")
			seedConcurrencyAdmissionPolicy(t, repo, "cred-1", int64Pointer(testCase.initialLimit), nil)
			preparePostgresConcurrencyPolicyPatch(t, ctx, repo, "home-a")
			lifetime := concurrencyAdmissionLifetime(t, repo, "fp-a", "home-a")

			admissionLocked := make(chan struct{})
			holderPID := make(chan postgresBackendPIDResult, 1)
			releaseAdmission := make(chan struct{})
			var releaseOnce sync.Once
			t.Cleanup(func() {
				releaseOnce.Do(func() { close(releaseAdmission) })
			})
			admissionDone := make(chan error, 1)
			go func() {
				admissionCtx := context.WithValue(ctx, concurrencyContenderBackendPIDContextKey{}, postgresContenderBackendPIDHook(holderPID))
				admissionCtx = context.WithValue(admissionCtx, concurrencyAdmissionPolicyLockAcquiredContextKey{}, func() {
					close(admissionLocked)
					<-releaseAdmission
				})
				_, errAdmit := repo.AdmitCredentialConcurrency(admissionCtx, ConcurrencyAdmissionRequest{
					CredentialID: "cred-1", Model: "gpt", Lifetime: lifetime, ProtocolVersion: 1,
				})
				admissionDone <- errAdmit
			}()
			select {
			case <-admissionLocked:
			case <-ctx.Done():
				t.Fatal("admission did not reach the transaction linearization lock")
			}

			startContenders := make(chan struct{})
			patchPID := make(chan postgresBackendPIDResult, 1)
			releasePID := make(chan postgresBackendPIDResult, 1)
			patchDone := make(chan error, 1)
			releaseDone := make(chan error, 1)
			go func() {
				<-startContenders
				patchCtx := context.WithValue(ctx, concurrencyContenderBackendPIDContextKey{}, postgresContenderBackendPIDHook(patchPID))
				_, errPatch := repo.PatchCredentialConcurrencyPolicy(patchCtx, "cred-1", ConcurrencyPolicyPatch{
					MaxInFlight: OptionalLimit{Set: true, Value: testCase.patchedLimit},
				}, nil)
				patchDone <- errPatch
			}()
			go func() {
				<-startContenders
				releaseCtx := context.WithValue(ctx, concurrencyReleaseContenderBackendPIDContextKey{}, postgresContenderBackendPIDHook(releasePID))
				releaseDone <- repo.ApplyConcurrencyRelease(releaseCtx, ConcurrencyReleaseRequest{
					CredentialID: "cred-1", Model: "gpt", ReleaseSeq: 1, Lifetime: lifetime,
				})
			}()
			close(startContenders)
			waitForPostgresContendersBlockedByHolder(t, ctx, repo, holderPID, patchPID, releasePID)
			select {
			case errPatch := <-patchDone:
				t.Fatalf("policy patch completed before admission commit: %v", errPatch)
			default:
			}
			select {
			case errRelease := <-releaseDone:
				t.Fatalf("cumulative release completed before admission commit: %v", errRelease)
			default:
			}

			releaseOnce.Do(func() { close(releaseAdmission) })
			select {
			case errAdmit := <-admissionDone:
				if errAdmit != nil {
					t.Fatalf("admission error = %v", errAdmit)
				}
			case <-ctx.Done():
				t.Fatal("admission did not commit")
			}
			select {
			case errPatch := <-patchDone:
				if errPatch != nil {
					t.Fatalf("policy patch error = %v", errPatch)
				}
			case <-ctx.Done():
				t.Fatal("policy patch did not commit")
			}
			select {
			case errRelease := <-releaseDone:
				if errRelease != nil {
					t.Fatalf("cumulative release error = %v", errRelease)
				}
			case <-ctx.Done():
				t.Fatal("cumulative release did not commit")
			}

			policy, errPolicy := repo.GetCredentialConcurrencyPolicy(ctx, "cred-1")
			if errPolicy != nil {
				t.Fatalf("get patched policy: %v", errPolicy)
			}
			if policy.MaxInFlight == nil || *policy.MaxInFlight != testCase.patchedLimit || policy.Version != 2 {
				t.Fatalf("policy after race = %#v", policy)
			}
			counter := loadReleaseCounter(t, repo, "cred-1", "gpt", "fp-a")
			if counter.ActiveCount != 0 || counter.LastReleaseSeq != 1 {
				t.Fatalf("counter after policy/admission/release race = %#v", counter)
			}
			if total := countConcurrencyAdmissionCounters(t, repo); total != 0 {
				t.Fatalf("counter total after race = %d, want 0", total)
			}

			admission, errAdmit := repo.AdmitCredentialConcurrency(ctx, ConcurrencyAdmissionRequest{
				CredentialID: "cred-1", Model: "gpt", Lifetime: lifetime, ProtocolVersion: 1,
			})
			if errAdmit != nil || !admission.Accounted {
				t.Fatalf("subsequent admission = %#v, error = %v", admission, errAdmit)
			}
			if errRelease := repo.ApplyConcurrencyRelease(ctx, ConcurrencyReleaseRequest{
				CredentialID: "cred-1", Model: "gpt", ReleaseSeq: 2, Lifetime: lifetime,
			}); errRelease != nil {
				t.Fatalf("subsequent release error = %v", errRelease)
			}
			counter = loadReleaseCounter(t, repo, "cred-1", "gpt", "fp-a")
			if counter.ActiveCount != 0 || counter.LastReleaseSeq != 2 {
				t.Fatalf("counter after subsequent admission/release = %#v", counter)
			}
		})
	}
}

func TestPatchCredentialConcurrencyPolicyRemovalWaitsForAdmissionPostgres(t *testing.T) {
	repo := newPostgresQuiescenceRepository(t)
	ctx, cancelCtx := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelCtx()
	seedConcurrencyAdmissionLifetime(t, repo, "fp-a", "home-a")
	seedConcurrencyAdmissionAuth(t, repo, "cred-1")
	seedConcurrencyAdmissionPolicy(t, repo, "cred-1", int64Pointer(1), nil)
	if errCreate := repo.db.Create(&ConcurrencyActivationGateRecord{ID: 1, ActivePolicyCount: 1}).Error; errCreate != nil {
		t.Fatal(errCreate)
	}

	admissionLocked := make(chan struct{})
	holderPID := make(chan postgresBackendPIDResult, 1)
	releaseAdmission := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(releaseAdmission) })
	})
	admissionDone := make(chan error, 1)
	go func() {
		admissionCtx := context.WithValue(ctx, concurrencyContenderBackendPIDContextKey{}, postgresContenderBackendPIDHook(holderPID))
		admissionCtx = context.WithValue(admissionCtx, concurrencyAdmissionPolicyLockAcquiredContextKey{}, func() {
			close(admissionLocked)
			<-releaseAdmission
		})
		_, errAdmit := repo.AdmitCredentialConcurrency(admissionCtx, ConcurrencyAdmissionRequest{
			CredentialID: "cred-1", Model: "gpt", Lifetime: concurrencyAdmissionLifetime(t, repo, "fp-a", "home-a"), ProtocolVersion: 1,
		})
		admissionDone <- errAdmit
	}()
	select {
	case <-admissionLocked:
	case <-ctx.Done():
		t.Fatal("admission did not reach the transaction linearization lock")
	}

	patchPID := make(chan postgresBackendPIDResult, 1)
	patchDone := make(chan error, 1)
	go func() {
		patchCtx := context.WithValue(ctx, concurrencyContenderBackendPIDContextKey{}, postgresContenderBackendPIDHook(patchPID))
		_, errPatch := repo.PatchCredentialConcurrencyPolicy(patchCtx, "cred-1", ConcurrencyPolicyPatch{
			MaxInFlight: OptionalLimit{Set: true, Null: true},
		}, nil)
		patchDone <- errPatch
	}()
	waitForPostgresContendersBlockedByHolder(t, ctx, repo, holderPID, patchPID)
	select {
	case errPatch := <-patchDone:
		t.Fatalf("policy removal completed before admission commit: %v", errPatch)
	default:
	}

	releaseOnce.Do(func() { close(releaseAdmission) })
	select {
	case errAdmit := <-admissionDone:
		if errAdmit != nil {
			t.Fatalf("admission error = %v", errAdmit)
		}
	case <-ctx.Done():
		t.Fatal("admission did not commit")
	}
	select {
	case errPatch := <-patchDone:
		if errPatch != nil {
			t.Fatalf("policy removal error = %v", errPatch)
		}
	case <-ctx.Done():
		t.Fatal("policy removal did not commit")
	}

	var counterWrites atomic.Int64
	counterTable := (CredentialConcurrencyCounterRecord{}).TableName()
	if errRegister := repo.db.Callback().Create().Before("gorm:create").Register("limiter:record-counter-create", func(tx *gorm.DB) {
		if tx.Statement.Table == counterTable {
			counterWrites.Add(1)
		}
	}); errRegister != nil {
		t.Fatal(errRegister)
	}
	if errRegister := repo.db.Callback().Update().Before("gorm:update").Register("limiter:record-counter-update", func(tx *gorm.DB) {
		if tx.Statement.Table == counterTable {
			counterWrites.Add(1)
		}
	}); errRegister != nil {
		t.Fatal(errRegister)
	}

	result, errAdmit := repo.AdmitCredentialConcurrency(ctx, ConcurrencyAdmissionRequest{
		CredentialID: "cred-1", Model: "gpt", Lifetime: concurrencyAdmissionLifetime(t, repo, "fp-a", "home-a"), ProtocolVersion: 0,
	})
	if errAdmit != nil || result.Accounted {
		t.Fatalf("admission after removal result=%#v error=%v", result, errAdmit)
	}
	if got := counterWrites.Load(); got != 0 {
		t.Fatalf("counter writes after policy removal = %d, want 0", got)
	}
	if got := countConcurrencyAdmissionCounters(t, repo); got != 1 {
		t.Fatalf("counter total = %d, want original admission only", got)
	}
}

func TestAdmitCredentialConcurrencyFailsClosedWithoutMutationPostgres(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		counters []CredentialConcurrencyCounterRecord
	}{
		{
			name: "negative counter",
			counters: []CredentialConcurrencyCounterRecord{{
				CredentialID: "cred-1", Model: "gpt", CertificateFingerprint: "fp-a", ActiveCount: -1, UpdatedAt: databaseTestTime,
			}},
		},
		{
			name: "sql sum overflow",
			counters: []CredentialConcurrencyCounterRecord{
				{CredentialID: "cred-1", Model: "gpt", CertificateFingerprint: "fp-a", ActiveCount: math.MaxInt64, UpdatedAt: databaseTestTime},
				{CredentialID: "cred-1", Model: "gpt", CertificateFingerprint: "fp-b", ActiveCount: math.MaxInt64, UpdatedAt: databaseTestTime},
			},
		},
		{
			name: "target max int",
			counters: []CredentialConcurrencyCounterRecord{{
				CredentialID: "cred-1", Model: "gpt", CertificateFingerprint: "fp-a", ActiveCount: math.MaxInt64, UpdatedAt: databaseTestTime,
			}},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			repo := newPostgresQuiescenceRepository(t)
			seedConcurrencyAdmissionLifetime(t, repo, "fp-a", "home-a")
			seedConcurrencyAdmissionAuth(t, repo, "cred-1")
			seedConcurrencyAdmissionPolicy(t, repo, "cred-1", int64Pointer(math.MaxInt64), nil)
			for _, counter := range testCase.counters {
				if errCreate := repo.db.Create(&counter).Error; errCreate != nil {
					t.Fatal(errCreate)
				}
			}

			_, errAdmit := repo.AdmitCredentialConcurrency(context.Background(), ConcurrencyAdmissionRequest{
				CredentialID: "cred-1", Model: "gpt", Lifetime: concurrencyAdmissionLifetime(t, repo, "fp-a", "home-a"), ProtocolVersion: 1,
			})
			if errAdmit == nil {
				t.Fatal("admission succeeded with an invalid counter")
			}
			assertPostgresConcurrencyCountersUnchanged(t, repo, testCase.counters)
		})
	}
}

type postgresBackendPIDResult struct {
	pid int
	err error
}

func postgresContenderBackendPIDHook(results chan<- postgresBackendPIDResult) func(*gorm.DB) error {
	return func(tx *gorm.DB) error {
		var pid int
		errPID := tx.Raw("SELECT pg_backend_pid()").Scan(&pid).Error
		results <- postgresBackendPIDResult{pid: pid, err: errPID}
		return errPID
	}
}

func waitForPostgresContendersBlockedByHolder(t *testing.T, ctx context.Context, repo *Repository, holderResults <-chan postgresBackendPIDResult, contenderResults ...<-chan postgresBackendPIDResult) {
	t.Helper()
	holderPID := waitForPostgresBackendPID(t, ctx, holderResults, "holder")
	pids := make([]int, 0, len(contenderResults))
	for _, results := range contenderResults {
		pids = append(pids, waitForPostgresBackendPID(t, ctx, results, "contender"))
	}

	sqlDB, errDB := repo.db.DB()
	if errDB != nil {
		t.Fatalf("get PostgreSQL database: %v", errDB)
	}
	conn, errConn := sqlDB.Conn(ctx)
	if errConn != nil {
		t.Fatalf("open separate PostgreSQL connection: %v", errConn)
	}
	defer func() {
		if errClose := conn.Close(); errClose != nil {
			t.Errorf("close separate PostgreSQL connection: %v", errClose)
		}
	}()

	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		allBlockedByHolder := true
		for _, pid := range pids {
			var waitEventType sql.NullString
			var blocksHolder bool
			errQuery := conn.QueryRowContext(ctx, "SELECT wait_event_type, $2 = ANY(pg_blocking_pids(pid)) FROM pg_stat_activity WHERE pid = $1", pid, holderPID).Scan(&waitEventType, &blocksHolder)
			if errQuery != nil && errQuery != sql.ErrNoRows {
				t.Fatalf("query contender backend lock wait: %v", errQuery)
			}
			if errQuery != nil || !waitEventType.Valid || waitEventType.String != "Lock" || !blocksHolder {
				allBlockedByHolder = false
			}
		}
		if allBlockedByHolder {
			return
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatalf("contenders %v did not all wait on locks blocked by holder backend PID %d before deadline: %v", pids, holderPID, ctx.Err())
		}
	}
}

func waitForPostgresBackendPID(t *testing.T, ctx context.Context, results <-chan postgresBackendPIDResult, role string) int {
	t.Helper()
	var result postgresBackendPIDResult
	select {
	case result = <-results:
	case <-ctx.Done():
		t.Fatalf("%s backend PID was not recorded before deadline: %v", role, ctx.Err())
	}
	if result.err != nil {
		t.Fatalf("record %s backend PID: %v", role, result.err)
	}
	return result.pid
}

func preparePostgresConcurrencyPolicyPatch(t *testing.T, ctx context.Context, repo *Repository, homeIP string) {
	t.Helper()
	now, errNow := DatabaseNow(ctx, repo.db)
	if errNow != nil {
		t.Fatal(errNow)
	}
	if errUpdate := repo.db.Model(&HomeProcessIncarnationRecord{}).Where("home_ip = ?", homeIP).Updates(map[string]any{
		"capabilities": JSONB(`["credential_concurrency_limits_v2"]`),
		"last_seen_at": now,
	}).Error; errUpdate != nil {
		t.Fatal(errUpdate)
	}
}

func assertPostgresConcurrencyCountersUnchanged(t *testing.T, repo *Repository, want []CredentialConcurrencyCounterRecord) {
	t.Helper()
	var got []CredentialConcurrencyCounterRecord
	if errFind := repo.db.Order("certificate_fingerprint ASC").Find(&got).Error; errFind != nil {
		t.Fatal(errFind)
	}
	if len(got) != len(want) {
		t.Fatalf("counter rows = %#v, want %#v", got, want)
	}
	for _, expected := range want {
		var found *CredentialConcurrencyCounterRecord
		for index := range got {
			if got[index].CredentialID == expected.CredentialID && got[index].Model == expected.Model && got[index].CertificateFingerprint == expected.CertificateFingerprint {
				found = &got[index]
				break
			}
		}
		if found == nil || found.ActiveCount != expected.ActiveCount {
			t.Fatalf("counter rows = %#v, want %#v", got, want)
		}
	}
}

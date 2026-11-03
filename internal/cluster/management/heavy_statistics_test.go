package management

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
	"gorm.io/gorm"
)

var heavyStatisticsRoutes = []struct {
	path string
	code string
}{
	{"/usage/overview", "usage_overview_load_failed"},
	{"/usage/aggregates", "usage_aggregates_load_failed"},
	{"/billing/overview", "billing_overview_load_failed"},
}

const heavyStatisticsQuery = "?from=2026-06-10T01:00:00Z&to=2026-06-10T01:10:00Z&group_by=provider"

func heavyStatisticsTestRouter(h *Handler) *gin.Engine {
	engine := gin.New()
	for _, prefix := range []string{"/v8/management", "/v0/management"} {
		engine.GET(prefix+"/usage/overview", h.GetUsageOverview)
		engine.GET(prefix+"/usage/aggregates", h.ListUsageAggregates)
		engine.GET(prefix+"/billing/overview", h.GetBillingOverview)
	}
	engine.GET("/v8/management/billing/settings", h.GetBillingSettings)
	return engine
}

type heavyStatisticsProbeKey struct{}

type heavyStatisticsProbe struct {
	once         sync.Once
	entered      chan struct{}
	release      <-chan struct{}
	fail         error
	calls        atomic.Int64
	settingsRead chan struct{}
}

// observeHeavyStatistics pauses the first heavy SQL callback, before any connection is acquired.
func observeHeavyStatistics(t *testing.T, db *gorm.DB) {
	t.Helper()
	observe := func(tx *gorm.DB) {
		probe, ok := tx.Statement.Context.Value(heavyStatisticsProbeKey{}).(*heavyStatisticsProbe)
		if !ok || tx.Statement.Table == (cluster.KVRecord{}).TableName() || tx.DryRun {
			return
		}
		probe.calls.Add(1)
		probe.once.Do(func() {
			close(probe.entered)
			if probe.release != nil {
				select {
				case <-probe.release:
				case <-tx.Statement.Context.Done():
					tx.AddError(tx.Statement.Context.Err())
				}
			}
		})
		if probe.fail != nil {
			tx.AddError(probe.fail)
		}
	}
	if errRegister := db.Callback().Query().After("gorm:after_query").Register("test:statistics-settings-read", func(tx *gorm.DB) {
		probe, ok := tx.Statement.Context.Value(heavyStatisticsProbeKey{}).(*heavyStatisticsProbe)
		if ok && probe.settingsRead != nil && tx.Statement.Table == (cluster.KVRecord{}).TableName() {
			close(probe.settingsRead)
		}
	}); errRegister != nil {
		t.Fatal(errRegister)
	}
	if errRegister := db.Callback().Query().Before("gorm:query").Register("test:heavy-statistics-query", observe); errRegister != nil {
		t.Fatal(errRegister)
	}
	if errRegister := db.Callback().Row().Before("gorm:row").Register("test:heavy-statistics-row", observe); errRegister != nil {
		t.Fatal(errRegister)
	}
}

func waitHeavyStatisticsSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for statistics request")
	}
}

func startHeavyStatisticsRequest(t *testing.T, engine *gin.Engine, ctx context.Context, path string, probe *heavyStatisticsProbe) (*httptest.ResponseRecorder, <-chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithCancel(ctx)
	if probe != nil {
		ctx = context.WithValue(ctx, heavyStatisticsProbeKey{}, probe)
	}
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, path, nil)
	response, done := httptest.NewRecorder(), make(chan struct{})
	go func() {
		defer close(done)
		engine.ServeHTTP(response, req)
	}()
	t.Cleanup(func() {
		cancel()
		waitHeavyStatisticsSignal(t, done)
	})
	return response, done
}

func assertHeavyStatisticsError(t *testing.T, response *httptest.ResponseRecorder, code, message string) {
	t.Helper()
	var body map[string]string
	if errUnmarshal := json.Unmarshal(response.Body.Bytes(), &body); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	if response.Code != http.StatusInternalServerError || len(body) != 2 || body["error"] != code || body["message"] != message {
		t.Fatalf("response = %d %s, want existing error envelope %s / %s", response.Code, response.Body, code, message)
	}
}

func TestHeavyStatisticsHandlersShareTwoSlotsAndCancelWaiters(t *testing.T) {
	for waitingIndex, waitingRoute := range heavyStatisticsRoutes {
		t.Run(waitingRoute.path, func(t *testing.T) {
			handler, db, closeRepo := newInFlightManagementTestHandler(t)
			t.Cleanup(closeRepo)
			observeHeavyStatistics(t, db)
			engine := heavyStatisticsTestRouter(handler)
			release := make(chan struct{})
			var releaseOnce sync.Once
			finish := func() { releaseOnce.Do(func() { close(release) }) }
			t.Cleanup(finish)
			var responses []*httptest.ResponseRecorder
			var completed []<-chan struct{}
			for index, route := range heavyStatisticsRoutes {
				if index == waitingIndex {
					continue
				}
				probe := &heavyStatisticsProbe{entered: make(chan struct{}), release: release}
				response, done := startHeavyStatisticsRequest(t, engine, context.Background(), "/v8/management"+route.path+heavyStatisticsQuery, probe)
				responses = append(responses, response)
				completed = append(completed, done)
				waitHeavyStatisticsSignal(t, probe.entered)
			}
			if cap(handler.heavyStatisticsSlots) != 2 || len(handler.heavyStatisticsSlots) != 2 {
				t.Fatal("the two distinct handlers did not share the two-slot pool")
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			probe := &heavyStatisticsProbe{entered: make(chan struct{}), settingsRead: make(chan struct{})}
			response, done := startHeavyStatisticsRequest(t, engine, ctx, "/v8/management"+waitingRoute.path+heavyStatisticsQuery, probe)
			if waitingRoute.path == "/billing/overview" {
				waitHeavyStatisticsSignal(t, probe.settingsRead)
			}
			cancel()
			waitHeavyStatisticsSignal(t, done)
			assertHeavyStatisticsError(t, response, waitingRoute.code, context.Canceled.Error())
			if probe.calls.Load() != 0 || len(handler.heavyStatisticsSlots) != 2 {
				t.Fatal("canceled waiter executed a heavy query or released another request's slot")
			}
			sqlDB, errDB := db.DB()
			if errDB != nil {
				t.Fatal(errDB)
			}
			if inUse := sqlDB.Stats().InUse; inUse != 0 {
				t.Fatalf("connections held before heavy queries = %d, want 0", inUse)
			}
			// A new request must enter after a holder releases its slot.
			probe = &heavyStatisticsProbe{entered: make(chan struct{})}
			response, done = startHeavyStatisticsRequest(t, engine, context.Background(), "/v8/management"+waitingRoute.path+heavyStatisticsQuery, probe)
			finish()
			waitHeavyStatisticsSignal(t, done)
			if response.Code != http.StatusOK || probe.calls.Load() == 0 {
				t.Fatalf("waiting request did not recover: %d %s", response.Code, response.Body)
			}
			for index, finished := range completed {
				waitHeavyStatisticsSignal(t, finished)
				if responses[index].Code != http.StatusOK {
					t.Fatalf("holder failed: %d %s", responses[index].Code, responses[index].Body)
				}
			}
			if len(handler.heavyStatisticsSlots) != 0 {
				t.Fatal("completed handlers leaked slots")
			}
		})
	}
}

func TestHeavyStatisticsQueryErrorsReleaseSlots(t *testing.T) {
	handler, db, closeRepo := newInFlightManagementTestHandler(t)
	defer closeRepo()
	observeHeavyStatistics(t, db)
	engine := heavyStatisticsTestRouter(handler)
	wantErr := errors.New("injected heavy query failure")
	for _, route := range heavyStatisticsRoutes {
		for range 3 {
			probe := &heavyStatisticsProbe{entered: make(chan struct{}), fail: wantErr}
			response, done := startHeavyStatisticsRequest(t, engine, context.Background(), "/v8/management"+route.path+heavyStatisticsQuery, probe)
			waitHeavyStatisticsSignal(t, done)
			assertHeavyStatisticsError(t, response, route.code, wantErr.Error())
			if len(handler.heavyStatisticsSlots) != 0 {
				t.Fatal("query failure leaked a slot")
			}
		}
	}
}

type blockedStatisticsResponseWriter struct {
	*httptest.ResponseRecorder
	entered chan struct{}
	release <-chan struct{}
	once    sync.Once
}

func (w *blockedStatisticsResponseWriter) Write(data []byte) (int, error) {
	w.once.Do(func() { close(w.entered) })
	<-w.release
	return w.ResponseRecorder.Write(data)
}

func TestHeavyStatisticsCompletedQueriesReleaseSlotsBeforeResponseWrite(t *testing.T) {
	for _, route := range heavyStatisticsRoutes {
		for _, queryFails := range []bool{false, true} {
			outcome := "success"
			if queryFails {
				outcome = "query-error"
			}
			t.Run(route.path+"/"+outcome, func(t *testing.T) {
				handler, db, closeRepo := newInFlightManagementTestHandler(t)
				t.Cleanup(closeRepo)
				seedBillingManagementCharge(t, handler)
				observeHeavyStatistics(t, db)
				engine := heavyStatisticsTestRouter(handler)
				release := make(chan struct{})
				var releaseOnce sync.Once
				finish := func() { releaseOnce.Do(func() { close(release) }) }
				var responses []*blockedStatisticsResponseWriter
				var completed []<-chan struct{}
				t.Cleanup(func() {
					finish()
					for _, done := range completed {
						waitHeavyStatisticsSignal(t, done)
					}
				})
				for range 2 {
					probe := &heavyStatisticsProbe{entered: make(chan struct{})}
					if queryFails {
						probe.fail = errors.New("injected heavy query failure")
					}
					ctx := context.WithValue(t.Context(), heavyStatisticsProbeKey{}, probe)
					request := httptest.NewRequestWithContext(ctx, http.MethodGet, "/v8/management"+route.path+heavyStatisticsQuery, nil)
					response := &blockedStatisticsResponseWriter{ResponseRecorder: httptest.NewRecorder(), entered: make(chan struct{}), release: release}
					done := make(chan struct{})
					responses = append(responses, response)
					completed = append(completed, done)
					go func() {
						defer close(done)
						engine.ServeHTTP(response, request)
					}()
					waitHeavyStatisticsSignal(t, response.entered)
					if probe.calls.Load() == 0 {
						t.Fatal("response write began without executing the heavy query")
					}
				}
				if occupied := len(handler.heavyStatisticsSlots); occupied != 0 {
					t.Fatalf("completed queries retained %d slots while response writes were blocked", occupied)
				}
				// Other statistics requests must finish while both clients still block response writes.
				for _, nextRoute := range heavyStatisticsRoutes {
					response, done := startHeavyStatisticsRequest(t, engine, t.Context(), "/v8/management"+nextRoute.path+heavyStatisticsQuery, nil)
					waitHeavyStatisticsSignal(t, done)
					if response.Code != http.StatusOK {
						t.Fatalf("%s was blocked by completed queries: status=%d", nextRoute.path, response.Code)
					}
				}
				finish()
				for index, done := range completed {
					waitHeavyStatisticsSignal(t, done)
					response := responses[index]
					wantStatus := http.StatusOK
					if queryFails {
						wantStatus = http.StatusInternalServerError
						var body map[string]any
						if errUnmarshal := json.Unmarshal(response.Body.Bytes(), &body); errUnmarshal != nil {
							t.Fatal(errUnmarshal)
						}
						if len(body) != 2 || body["error"] != route.code {
							t.Fatalf("query error code or envelope changed: %v", body)
						}
					}
					if response.Code != wantStatus {
						t.Fatalf("blocked response status=%d, want %d", response.Code, wantStatus)
					}
				}
			})
		}
	}
}

func TestHeavyStatisticsValidationAndOtherRoutesBypassQueue(t *testing.T) {
	handler, _, closeRepo := newInFlightManagementTestHandler(t)
	defer closeRepo()
	seedBillingManagementCharge(t, handler)
	engine := heavyStatisticsTestRouter(handler)
	handler.heavyStatisticsSlots <- struct{}{}
	handler.heavyStatisticsSlots <- struct{}{}
	paths := []string{"/v8/management/billing/settings"}
	for _, route := range heavyStatisticsRoutes {
		paths = append(paths, "/v0/management"+route.path+heavyStatisticsQuery)
		paths = append(paths, "/v8/management"+route.path+"?timezone=invalid-zone")
	}
	for _, path := range paths {
		response, done := startHeavyStatisticsRequest(t, engine, context.Background(), path, nil)
		waitHeavyStatisticsSignal(t, done)
		wantStatus := http.StatusOK
		if strings.HasSuffix(path, "?timezone=invalid-zone") {
			wantStatus = http.StatusBadRequest
		}
		if response.Code != wantStatus {
			t.Fatalf("%s = %d %s, want %d", path, response.Code, response.Body, wantStatus)
		}
		if len(handler.heavyStatisticsSlots) != 2 {
			t.Fatal("bypassed request changed the pool")
		}
	}
}

func TestHeavyStatisticsV8ResultsMatchLegacy(t *testing.T) {
	handler, _, closeRepo := newInFlightManagementTestHandler(t)
	defer closeRepo()
	seedBillingManagementCharge(t, handler)
	engine := heavyStatisticsTestRouter(handler)
	for _, route := range heavyStatisticsRoutes {
		var bodies []map[string]any
		for _, prefix := range []string{"/v0/management", "/v8/management"} {
			response := httptest.NewRecorder()
			engine.ServeHTTP(response, httptest.NewRequest(http.MethodGet, prefix+route.path+heavyStatisticsQuery, nil))
			if response.Code != http.StatusOK {
				t.Fatalf("%s%s = %d %s", prefix, route.path, response.Code, response.Body)
			}
			var body map[string]any
			if errUnmarshal := json.Unmarshal(response.Body.Bytes(), &body); errUnmarshal != nil {
				t.Fatal(errUnmarshal)
			}
			bodies = append(bodies, body)
		}
		if !reflect.DeepEqual(bodies[0], bodies[1]) {
			t.Fatalf("%s changed its response: v0=%v v8=%v", route.path, bodies[0], bodies[1])
		}
	}
}

func TestHeavyStatisticsQueueUsesOriginalTenSecondBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		handler := NewHandler(nil, nil, "", 0)
		handler.heavyStatisticsSlots <- struct{}{}
		handler.heavyStatisticsSlots <- struct{}{}
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodGet, "/v8/management/usage/overview", nil)
		ctx, cancel := handler.requestContext(c)
		defer cancel()
		startedAt := time.Now()
		deadline, ok := ctx.Deadline()
		if !ok || deadline.Sub(startedAt) != 10*time.Second {
			t.Fatal("request budget is not ten seconds")
		}
		acquired := make(chan struct{})
		go func() {
			release, errAcquire := handler.acquireHeavyStatisticsSlot(ctx, c)
			if errAcquire != nil {
				t.Error(errAcquire)
				return
			}
			defer release()
			close(acquired)
			<-ctx.Done()
		}()
		synctest.Wait()
		select {
		case <-acquired:
			t.Fatal("third request acquired a full pool")
		default:
		}
		<-time.After(7 * time.Second)
		<-handler.heavyStatisticsSlots
		<-acquired
		if remaining := time.Until(deadline); remaining != 3*time.Second {
			t.Fatalf("remaining execution budget = %v, want 3s", remaining)
		}
		<-ctx.Done()
		synctest.Wait()
		if time.Since(startedAt) != 10*time.Second || len(handler.heavyStatisticsSlots) != 1 {
			t.Fatal("request gained a new budget or leaked a slot")
		}
	})
}

func TestHeavyStatisticsQueueTimeoutUsesExistingErrors(t *testing.T) {
	handler, db, closeRepo := newInFlightManagementTestHandler(t)
	defer closeRepo()
	queryCallback := db.Callback().Query().Get("gorm:query")
	// Keep billing's settings preflight in memory so synctest can control only the queue clock.
	if errReplace := db.Callback().Query().Replace("gorm:query", func(tx *gorm.DB) {
		if tx.Statement.Table == (cluster.KVRecord{}).TableName() {
			return
		}
		queryCallback(tx)
	}); errReplace != nil {
		t.Fatal(errReplace)
	}
	for _, route := range heavyStatisticsRoutes {
		t.Run(route.path, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				handler.heavyStatisticsSlots = make(chan struct{}, 2)
				handler.heavyStatisticsSlots <- struct{}{}
				handler.heavyStatisticsSlots <- struct{}{}
				engine := heavyStatisticsTestRouter(handler)
				response := httptest.NewRecorder()
				startedAt := time.Now()
				engine.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v8/management"+route.path+heavyStatisticsQuery, nil))
				assertHeavyStatisticsError(t, response, route.code, context.DeadlineExceeded.Error())
				if time.Since(startedAt) != 10*time.Second || len(handler.heavyStatisticsSlots) != 2 {
					t.Fatal("queue timeout changed the budget or pool")
				}
			})
		})
	}
}

func TestHeavyStatisticsQueuedHandlersKeepOriginalDeadline(t *testing.T) {
	handler, db, closeRepo := newInFlightManagementTestHandler(t)
	defer closeRepo()
	deadlines := make(chan time.Time, 1)
	wantErr := errors.New("stop after observing execution deadline")
	observe := func(tx *gorm.DB) {
		if tx.Statement.Table == (cluster.KVRecord{}).TableName() || tx.DryRun {
			return
		}
		deadline, _ := tx.Statement.Context.Deadline()
		deadlines <- deadline
		tx.AddError(wantErr)
	}
	queryCallback := db.Callback().Query().Get("gorm:query")
	if errReplace := db.Callback().Query().Replace("gorm:query", func(tx *gorm.DB) {
		if tx.Statement.Table == (cluster.KVRecord{}).TableName() {
			return
		}
		queryCallback(tx)
	}); errReplace != nil {
		t.Fatal(errReplace)
	}
	if errRegister := db.Callback().Query().Before("gorm:query").Register("test:statistics-query-deadline", observe); errRegister != nil {
		t.Fatal(errRegister)
	}
	if errRegister := db.Callback().Row().Before("gorm:row").Register("test:statistics-row-deadline", observe); errRegister != nil {
		t.Fatal(errRegister)
	}
	for _, route := range heavyStatisticsRoutes {
		t.Run(route.path, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				handler.heavyStatisticsSlots = make(chan struct{}, 2)
				handler.heavyStatisticsSlots <- struct{}{}
				handler.heavyStatisticsSlots <- struct{}{}
				engine := heavyStatisticsTestRouter(handler)
				response := httptest.NewRecorder()
				startedAt := time.Now()
				go engine.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v8/management"+route.path+heavyStatisticsQuery, nil))
				synctest.Wait()
				<-time.After(7 * time.Second)
				<-handler.heavyStatisticsSlots
				synctest.Wait()
				select {
				case deadline := <-deadlines:
					if !deadline.Equal(startedAt.Add(10*time.Second)) || time.Until(deadline) != 3*time.Second {
						t.Fatalf("execution deadline = %v, want original %v", deadline, startedAt.Add(10*time.Second))
					}
				default:
					t.Fatal("handler did not reach the heavy query")
				}
				assertHeavyStatisticsError(t, response, route.code, wantErr.Error())
				if len(handler.heavyStatisticsSlots) != 1 {
					t.Fatal("handler failed to release its execution slot")
				}
			})
		})
	}
}

type cancelAfterStatisticsSlotContext struct {
	context.Context
	cancel context.CancelFunc
	slots  chan struct{}
}

func (c cancelAfterStatisticsSlotContext) Err() error {
	if len(c.slots) > 0 {
		c.cancel()
	}
	return c.Context.Err()
}

func TestHeavyStatisticsSlotsAreLocalToEachHome(t *testing.T) {
	first := NewHandler(nil, nil, "home-a", 8327)
	second := NewHandler(nil, nil, "home-b", 8327)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/v8/management/usage/overview", nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for _, handler := range []*Handler{first, second} {
		for range 2 {
			release, errAcquire := handler.acquireHeavyStatisticsSlot(ctx, c)
			if errAcquire != nil {
				t.Fatal(errAcquire)
			}
			defer release()
		}
	}
	if len(first.heavyStatisticsSlots) != 2 || len(second.heavyStatisticsSlots) != 2 {
		t.Fatal("two Homes did not admit four requests independently")
	}
}

func TestHeavyStatisticsChecksCancellationAfterAcquiringSlot(t *testing.T) {
	handler := NewHandler(nil, nil, "", 0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/v8/management/usage/overview", nil)
	release, errAcquire := handler.acquireHeavyStatisticsSlot(cancelAfterStatisticsSlotContext{Context: ctx, cancel: cancel, slots: handler.heavyStatisticsSlots}, c)
	if !errors.Is(errAcquire, context.Canceled) || release != nil || len(handler.heavyStatisticsSlots) != 0 {
		t.Fatalf("post-acquisition cancellation: error=%v occupied=%d", errAcquire, len(handler.heavyStatisticsSlots))
	}
}

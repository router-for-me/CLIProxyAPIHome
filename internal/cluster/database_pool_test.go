package cluster

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	log "github.com/sirupsen/logrus"
)

type poolObservationHook struct{ messages chan string }

func (h *poolObservationHook) Levels() []log.Level { return []log.Level{log.InfoLevel} }
func (h *poolObservationHook) Fire(entry *log.Entry) error {
	if strings.HasPrefix(entry.Message, "cluster database pool ") {
		h.messages <- entry.Message
	}
	return nil
}

func TestDatabasePoolObservationDeltasAndCancellation(t *testing.T) {
	// The fake driver proves observation needs no server queries or extra connections.
	connector := &gateTraceConnector{}
	db := sql.OpenDB(connector)
	db.SetMaxOpenConns(3)
	db.SetMaxIdleConns(1)
	t.Cleanup(func() {
		if errClose := db.Close(); errClose != nil {
			t.Error(errClose)
		}
	})
	// Establish one historical closure before starting observation.
	first, errFirst := db.Conn(t.Context())
	if errFirst != nil {
		t.Fatal(errFirst)
	}
	second, errSecond := db.Conn(t.Context())
	if errSecond != nil {
		t.Fatal(errSecond)
	}
	if errClose := first.Close(); errClose != nil {
		t.Fatal(errClose)
	}
	if errClose := second.Close(); errClose != nil {
		t.Fatal(errClose)
	}
	logger := log.StandardLogger()
	oldHooks, oldLevel := logger.Hooks, logger.GetLevel()
	logger.ReplaceHooks(make(log.LevelHooks))
	hook := &poolObservationHook{messages: make(chan string, 4)}
	logger.AddHook(hook)
	logger.SetLevel(log.InfoLevel)
	t.Cleanup(func() { logger.ReplaceHooks(oldHooks); logger.SetLevel(oldLevel) })
	ctx, cancel := context.WithCancel(t.Context())
	ticks := make(chan time.Time)
	done := make(chan struct{})
	go func() { defer close(done); observeDatabasePool(ctx, db, "test", ticks) }()
	defer func() { cancel(); <-done }()
	select {
	case message := <-hook.messages:
		for _, field := range []string{"backend=test initial=true interval_ms=0.000", "max_open=3 open=1 in_use=0 idle=1", "max_idle_closed=1 max_idle_closed_delta=0"} {
			if !strings.Contains(message, field) {
				t.Fatalf("invalid initial pool sample: %s", message)
			}
		}
	case <-time.After(5 * time.Second):
		t.Fatal("initial sample not emitted")
	}
	beforeConnects := connector.connects
	first, errFirst = db.Conn(t.Context())
	if errFirst != nil {
		t.Fatal(errFirst)
	}
	second, errSecond = db.Conn(t.Context())
	if errSecond != nil {
		t.Fatal(errSecond)
	}
	if errClose := first.Close(); errClose != nil {
		t.Fatal(errClose)
	}
	if errClose := second.Close(); errClose != nil {
		t.Fatal(errClose)
	}
	ticks <- time.Now()
	select {
	case message := <-hook.messages:
		for _, field := range []string{"initial=false", "max_open=3 open=1 in_use=0 idle=1", "wait_count=0 wait_count_delta=0", "max_idle_closed=2 max_idle_closed_delta=1", "max_idle_time_closed=0 max_idle_time_closed_delta=0", "max_lifetime_closed=0 max_lifetime_closed_delta=0"} {
			if !strings.Contains(message, field) {
				t.Fatalf("invalid pool delta sample: %s", message)
			}
		}
	case <-time.After(5 * time.Second):
		t.Fatal("periodic sample not emitted")
	}
	if connector.connects != beforeConnects+1 || connector.begins != 0 {
		t.Fatal("observation acquired a connection or began a transaction")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("pool observer did not stop")
	}
}

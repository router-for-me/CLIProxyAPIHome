package cluster

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"gorm.io/gorm"
)

const (
	databaseQueryUnclassified    = "database.unclassified"
	databaseSQLIDNamespace       = "home-sql-id-v1\x00"
	databaseQueryLogCapacity     = 1024
	databaseSlowQueryLogInterval = time.Minute
)

type databaseQueryNameContextKey struct{}

// WithDatabaseQueryName attaches a stable semantic name to database work.
func WithDatabaseQueryName(ctx context.Context, query string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	query = normalizeDatabaseQueryName(query)
	if query == "" {
		return ctx
	}
	return context.WithValue(ctx, databaseQueryNameContextKey{}, query)
}

func databaseQueryName(ctx context.Context) string {
	if ctx != nil {
		if query, ok := ctx.Value(databaseQueryNameContextKey{}).(string); ok {
			if query = normalizeDatabaseQueryName(query); query != "" {
				return query
			}
		}
	}
	return databaseQueryUnclassified
}

func databaseQueryDB(db *gorm.DB, query string) *gorm.DB {
	if db == nil {
		return nil
	}
	ctx := context.Background()
	if db.Statement != nil && db.Statement.Context != nil {
		ctx = db.Statement.Context
	}
	return db.WithContext(WithDatabaseQueryName(ctx, query))
}

func normalizeDatabaseQueryName(query string) string {
	return strings.Join(strings.Fields(query), " ")
}

func canonicalDatabaseSQL(sql string) string {
	var canonical strings.Builder
	canonical.Grow(len(sql))
	pendingSpace := false
	for index := 0; index < len(sql); {
		r, size := utf8.DecodeRuneInString(sql[index:])
		if unicode.IsSpace(r) {
			pendingSpace = true
			index += size
			continue
		}
		if pendingSpace {
			if canonical.Len() > 0 {
				canonical.WriteByte(' ')
			}
			pendingSpace = false
		}
		switch sql[index] {
		case '\'', '"', '`':
			index = appendDatabaseSQLQuoted(&canonical, sql, index, sql[index])
		case '[':
			index = appendDatabaseSQLBracketQuoted(&canonical, sql, index)
		case '$':
			if end, ok := databaseSQLDollarQuotedEnd(sql, index); ok {
				canonical.WriteString(sql[index:end])
				index = end
				continue
			}
			canonical.WriteByte(sql[index])
			index++
		default:
			canonical.WriteString(sql[index : index+size])
			index += size
		}
	}
	return canonical.String()
}

func appendDatabaseSQLQuoted(builder *strings.Builder, sql string, start int, quote byte) int {
	builder.WriteByte(quote)
	for index := start + 1; index < len(sql); index++ {
		builder.WriteByte(sql[index])
		if sql[index] != quote {
			continue
		}
		if index+1 < len(sql) && sql[index+1] == quote {
			builder.WriteByte(sql[index+1])
			index++
			continue
		}
		return index + 1
	}
	return len(sql)
}

func appendDatabaseSQLBracketQuoted(builder *strings.Builder, sql string, start int) int {
	builder.WriteByte('[')
	for index := start + 1; index < len(sql); index++ {
		builder.WriteByte(sql[index])
		if sql[index] != ']' {
			continue
		}
		if index+1 < len(sql) && sql[index+1] == ']' {
			builder.WriteByte(sql[index+1])
			index++
			continue
		}
		return index + 1
	}
	return len(sql)
}

func databaseSQLDollarQuotedEnd(sql string, start int) (int, bool) {
	delimiterEnd := start + 1
	for delimiterEnd < len(sql) && sql[delimiterEnd] != '$' {
		value := sql[delimiterEnd]
		if delimiterEnd == start+1 {
			if value != '_' && (value < 'A' || value > 'Z') && (value < 'a' || value > 'z') {
				return 0, false
			}
		} else if value != '_' && (value < 'A' || value > 'Z') && (value < 'a' || value > 'z') && (value < '0' || value > '9') {
			return 0, false
		}
		delimiterEnd++
	}
	if delimiterEnd >= len(sql) {
		return 0, false
	}
	delimiter := sql[start : delimiterEnd+1]
	contentStart := delimiterEnd + 1
	closingOffset := strings.Index(sql[contentStart:], delimiter)
	if closingOffset < 0 {
		return 0, false
	}
	return contentStart + closingOffset + len(delimiter), true
}

func singleLineDatabaseSQL(sql string) string {
	return strings.NewReplacer("\r\n", `\n`, "\r", `\r`, "\n", `\n`, "\t", `\t`).Replace(canonicalDatabaseSQL(sql))
}

func databaseSQLID(sql string) string {
	canonicalSQL := canonicalDatabaseSQL(sql)
	hash := sha256.Sum256([]byte(databaseSQLIDNamespace + canonicalSQL))
	return hex.EncodeToString(hash[:])[:16]
}

type databaseQueryDefinition struct {
	SQLID             string
	Query             string
	SQL               string
	FirstRegisteredAt time.Time
}

type databaseSlowQueryDecision struct {
	Log        bool
	Suppressed int64
	Average    time.Duration
	Maximum    time.Duration
}

type databaseQueryLogEntry struct {
	databaseQueryDefinition
	LastUsedAt           time.Time
	lastUsedSequence     uint64
	lastSlowLogAt        time.Time
	suppressedCount      int64
	suppressedTotal      time.Duration
	suppressedMaximum    time.Duration
	latestSuppressedRows int64
}

type databaseQueryLog struct {
	mu           sync.Mutex
	capacity     int
	slowInterval time.Duration
	now          func() time.Time
	sequence     uint64
	entries      map[string]*databaseQueryLogEntry
}

func newDatabaseQueryLog(capacity int, slowInterval time.Duration) *databaseQueryLog {
	if capacity <= 0 {
		capacity = databaseQueryLogCapacity
	}
	if slowInterval <= 0 {
		slowInterval = databaseSlowQueryLogInterval
	}
	return &databaseQueryLog{
		capacity:     capacity,
		slowInterval: slowInterval,
		now:          time.Now,
		entries:      make(map[string]*databaseQueryLogEntry, capacity),
	}
}

func (l *databaseQueryLog) register(sqlID string, query string, sql string, emit func(databaseQueryDefinition)) bool {
	if l == nil {
		return false
	}
	now := l.currentTime()
	query = normalizeDatabaseQueryName(query)
	if query == "" {
		query = databaseQueryUnclassified
	}
	sql = singleLineDatabaseSQL(sql)

	l.mu.Lock()
	defer l.mu.Unlock()
	l.sequence++
	if entry, ok := l.entries[sqlID]; ok {
		entry.LastUsedAt = now
		entry.lastUsedSequence = l.sequence
		return false
	}
	if len(l.entries) >= l.capacity {
		l.evictLeastRecentlyUsed()
	}
	definition := databaseQueryDefinition{
		SQLID:             sqlID,
		Query:             query,
		SQL:               sql,
		FirstRegisteredAt: now,
	}
	l.entries[sqlID] = &databaseQueryLogEntry{
		databaseQueryDefinition: definition,
		LastUsedAt:              now,
		lastUsedSequence:        l.sequence,
		latestSuppressedRows:    -1,
	}
	if emit != nil {
		emit(definition)
	}
	return true
}

func (l *databaseQueryLog) observeSlow(sqlID string, elapsed time.Duration, rows int64) databaseSlowQueryDecision {
	if l == nil {
		return databaseSlowQueryDecision{Log: true}
	}
	now := l.currentTime()
	l.mu.Lock()
	defer l.mu.Unlock()
	entry, ok := l.entries[sqlID]
	if !ok {
		return databaseSlowQueryDecision{Log: true}
	}
	l.sequence++
	entry.LastUsedAt = now
	entry.lastUsedSequence = l.sequence
	if entry.lastSlowLogAt.IsZero() || now.Sub(entry.lastSlowLogAt) >= l.slowInterval {
		decision := databaseSlowQueryDecision{Log: true, Suppressed: entry.suppressedCount}
		if entry.suppressedCount > 0 {
			decision.Average = entry.suppressedTotal / time.Duration(entry.suppressedCount)
			decision.Maximum = entry.suppressedMaximum
		}
		entry.lastSlowLogAt = now
		entry.suppressedCount = 0
		entry.suppressedTotal = 0
		entry.suppressedMaximum = 0
		entry.latestSuppressedRows = -1
		return decision
	}
	entry.suppressedCount++
	entry.suppressedTotal += elapsed
	if elapsed > entry.suppressedMaximum {
		entry.suppressedMaximum = elapsed
	}
	entry.latestSuppressedRows = rows
	return databaseSlowQueryDecision{}
}

func (l *databaseQueryLog) currentTime() time.Time {
	if l.now == nil {
		return time.Now()
	}
	return l.now()
}

func (l *databaseQueryLog) evictLeastRecentlyUsed() {
	var oldestID string
	var oldestSequence uint64
	for sqlID, entry := range l.entries {
		if oldestID == "" || entry.lastUsedSequence < oldestSequence {
			oldestID = sqlID
			oldestSequence = entry.lastUsedSequence
		}
	}
	if oldestID != "" {
		delete(l.entries, oldestID)
	}
}

func (l *databaseQueryLog) lookup(sqlID string) (databaseQueryLogEntry, bool) {
	if l == nil {
		return databaseQueryLogEntry{}, false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	entry, ok := l.entries[sqlID]
	if !ok {
		return databaseQueryLogEntry{}, false
	}
	return *entry, true
}

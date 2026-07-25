package cluster

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	homelogging "github.com/router-for-me/CLIProxyAPIHome/internal/logging"
	log "github.com/sirupsen/logrus"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	gormlogger "gorm.io/gorm/logger"
)

const databaseSlowQueryThreshold = 200 * time.Millisecond

var (
	configureDatabaseGORMRecorderFilter sync.Once
	processDatabaseQueryLog             = newDatabaseQueryLog(databaseQueryLogCapacity, databaseSlowQueryLogInterval)
)

type homeGORMLogger struct {
	logger        *log.Logger
	level         gormlogger.LogLevel
	slowThreshold time.Duration
	queryLog      *databaseQueryLog
}

type parameterizedGORMLogger struct {
	inner gormlogger.Interface
}

func databaseGORMConfig() *gorm.Config {
	return &gorm.Config{Logger: newParameterizedGORMLogger(newHomeGORMLogger(log.StandardLogger()))}
}

func configureDatabaseGORMClauseBuilders(db *gorm.DB) {
	if db == nil || db.Dialector == nil || db.Dialector.Name() != "sqlite" {
		return
	}
	db.ClauseBuilders["LIMIT"] = func(sqlClause clause.Clause, builder clause.Builder) {
		limit, ok := sqlClause.Expression.(clause.Limit)
		if !ok {
			sqlClause.Builder = nil
			sqlClause.Build(builder)
			return
		}
		hasLimit := limit.Limit != nil && *limit.Limit >= 0
		if hasLimit || limit.Offset > 0 {
			builder.WriteString("LIMIT ")
			if hasLimit {
				builder.AddVar(builder, *limit.Limit)
			} else {
				builder.AddVar(builder, -1)
			}
		}
		if limit.Offset > 0 {
			builder.WriteString(" OFFSET ")
			builder.AddVar(builder, limit.Offset)
		}
	}
}

func newHomeGORMLogger(baseLogger *log.Logger) gormlogger.Interface {
	return newHomeGORMLoggerWithQueryLog(baseLogger, processDatabaseQueryLog)
}

func newHomeGORMLoggerWithQueryLog(baseLogger *log.Logger, queryLog *databaseQueryLog) gormlogger.Interface {
	if baseLogger == nil {
		baseLogger = log.StandardLogger()
	}
	if queryLog == nil {
		queryLog = processDatabaseQueryLog
	}
	return homeGORMLogger{
		logger:        baseLogger,
		level:         gormlogger.Warn,
		slowThreshold: databaseSlowQueryThreshold,
		queryLog:      queryLog,
	}
}

func (l homeGORMLogger) LogMode(level gormlogger.LogLevel) gormlogger.Interface {
	l.level = level
	return l
}

func (l homeGORMLogger) Info(ctx context.Context, message string, data ...interface{}) {
	if l.level >= gormlogger.Info {
		l.entry(ctx).Info(formatGORMMessage(message, data...))
	}
}

func (l homeGORMLogger) Warn(ctx context.Context, message string, data ...interface{}) {
	if l.level >= gormlogger.Warn {
		l.entry(ctx).Warn(formatGORMMessage(message, data...))
	}
}

func (l homeGORMLogger) Error(ctx context.Context, message string, data ...interface{}) {
	if l.level >= gormlogger.Error {
		l.entry(ctx).Error(formatGORMMessage(message, data...))
	}
}

func (l homeGORMLogger) Trace(ctx context.Context, begin time.Time, fc func() (string, int64), err error) {
	if l.level <= gormlogger.Silent {
		return
	}

	elapsed := time.Since(begin)
	switch {
	case err != nil && l.level >= gormlogger.Error:
		sql, rows := fc()
		query, sqlID := l.defineDatabaseQuery(ctx, sql)
		l.entry(ctx).WithField("error", singleLineGORMMessage(err.Error())).Error(formatDatabaseGORMTrace("SQL ERROR", elapsed, rows, query, sqlID))
	case elapsed > l.slowThreshold && l.slowThreshold != 0 && l.level >= gormlogger.Warn:
		sql, rows := fc()
		query, sqlID := l.defineDatabaseQuery(ctx, sql)
		decision := l.queryLog.observeSlow(sqlID, elapsed, rows)
		if !decision.Log {
			return
		}
		message := formatDatabaseGORMTrace(fmt.Sprintf("SLOW SQL >= %v", l.slowThreshold), elapsed, rows, query, sqlID)
		if decision.Suppressed > 0 {
			message += fmt.Sprintf(" suppressed=%d avg=%s max=%s", decision.Suppressed, formatDatabaseSummaryDuration(decision.Average), formatDatabaseSummaryDuration(decision.Maximum))
		}
		l.entry(ctx).Warn(message)
	case l.level == gormlogger.Info:
		sql, rows := fc()
		l.entry(ctx).Info(formatGORMTrace(elapsed, rows, sql))
	}
}

func (l homeGORMLogger) defineDatabaseQuery(ctx context.Context, sql string) (string, string) {
	query := databaseQueryName(ctx)
	canonicalSQL := canonicalDatabaseSQL(sql)
	sqlID := databaseSQLID(canonicalSQL)
	l.queryLog.register(sqlID, query, canonicalSQL, func(definition databaseQueryDefinition) {
		l.entry(ctx).Warnf("SQL DEF query=%s sql_id=%s sql=%s", definition.Query, definition.SQLID, definition.SQL)
	})
	return query, sqlID
}

func (l homeGORMLogger) entry(ctx context.Context) *log.Entry {
	entry := log.NewEntry(l.logger)
	if requestID := homelogging.GetRequestID(ctx); requestID != "" {
		entry = entry.WithField("request_id", requestID)
	}
	return entry
}

func formatGORMTrace(elapsed time.Duration, rows int64, sql string) string {
	rowsValue := interface{}(rows)
	if rows == -1 {
		rowsValue = "-"
	}
	return fmt.Sprintf("[%.3fms] [rows:%v] %s", float64(elapsed)/float64(time.Millisecond), rowsValue, singleLineGORMMessage(sql))
}

func formatDatabaseGORMTrace(prefix string, elapsed time.Duration, rows int64, query string, sqlID string) string {
	rowsValue := interface{}(rows)
	if rows == -1 {
		rowsValue = "-"
	}
	return fmt.Sprintf("%s [%.3fms] [rows:%v] query=%s sql_id=%s", prefix, float64(elapsed)/float64(time.Millisecond), rowsValue, query, sqlID)
}

func formatDatabaseSummaryDuration(duration time.Duration) string {
	return fmt.Sprintf("%.0fms", float64(duration)/float64(time.Millisecond))
}

func formatGORMMessage(message string, data ...interface{}) string {
	if len(data) > 0 {
		message = fmt.Sprintf(message, data...)
	}
	return singleLineGORMMessage(message)
}

func singleLineGORMMessage(message string) string {
	message = strings.NewReplacer("\r\n", " ", "\r", " ", "\n", " ").Replace(message)
	return strings.TrimSpace(message)
}

func newParameterizedGORMLogger(inner gormlogger.Interface) gormlogger.Interface {
	configureDatabaseGORMRecorderFilter.Do(func() {
		gormlogger.RecorderParamsFilter = filterDatabaseGORMParams
	})
	if inner == nil {
		inner = gormlogger.Default
	}
	return parameterizedGORMLogger{inner: inner}
}

func (l parameterizedGORMLogger) LogMode(level gormlogger.LogLevel) gormlogger.Interface {
	return parameterizedGORMLogger{inner: l.inner.LogMode(level)}
}

func (l parameterizedGORMLogger) Info(ctx context.Context, message string, data ...interface{}) {
	l.inner.Info(ctx, message, data...)
}

func (l parameterizedGORMLogger) Warn(ctx context.Context, message string, data ...interface{}) {
	l.inner.Warn(ctx, message, data...)
}

func (l parameterizedGORMLogger) Error(ctx context.Context, message string, data ...interface{}) {
	l.inner.Error(ctx, message, data...)
}

func (l parameterizedGORMLogger) Trace(ctx context.Context, begin time.Time, fc func() (string, int64), err error) {
	if errors.Is(err, gorm.ErrRecordNotFound) {
		err = nil
	}
	l.inner.Trace(ctx, begin, fc, err)
}

func (l parameterizedGORMLogger) ParamsFilter(ctx context.Context, sql string, _ ...interface{}) (string, []interface{}) {
	return filterDatabaseGORMParams(ctx, sql)
}

func filterDatabaseGORMParams(_ context.Context, sql string, _ ...interface{}) (string, []interface{}) {
	return sql, nil
}

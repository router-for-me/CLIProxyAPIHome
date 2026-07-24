package cluster

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	homelogging "github.com/router-for-me/CLIProxyAPIHome/internal/logging"
	log "github.com/sirupsen/logrus"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

const databaseSlowQueryThreshold = 200 * time.Millisecond

type homeGORMLogger struct {
	logger        *log.Logger
	level         gormlogger.LogLevel
	slowThreshold time.Duration
}

type parameterizedGORMLogger struct {
	inner gormlogger.Interface
}

func databaseGORMConfig() *gorm.Config {
	return &gorm.Config{Logger: newParameterizedGORMLogger(newHomeGORMLogger(log.StandardLogger()))}
}

func newHomeGORMLogger(baseLogger *log.Logger) gormlogger.Interface {
	if baseLogger == nil {
		baseLogger = log.StandardLogger()
	}
	return homeGORMLogger{
		logger:        baseLogger,
		level:         gormlogger.Warn,
		slowThreshold: databaseSlowQueryThreshold,
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
		l.entry(ctx).WithField("error", singleLineGORMMessage(err.Error())).Error(formatGORMTrace(elapsed, rows, sql))
	case elapsed > l.slowThreshold && l.slowThreshold != 0 && l.level >= gormlogger.Warn:
		sql, rows := fc()
		l.entry(ctx).Warnf("SLOW SQL >= %v %s", l.slowThreshold, formatGORMTrace(elapsed, rows, sql))
	case l.level == gormlogger.Info:
		sql, rows := fc()
		l.entry(ctx).Info(formatGORMTrace(elapsed, rows, sql))
	}
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

func (l parameterizedGORMLogger) ParamsFilter(_ context.Context, sql string, _ ...interface{}) (string, []interface{}) {
	return sql, nil
}

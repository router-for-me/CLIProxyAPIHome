package cluster

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

type parameterizedGORMLogger struct {
	inner gormlogger.Interface
}

func databaseGORMConfig() *gorm.Config {
	return &gorm.Config{Logger: newParameterizedGORMLogger(gormlogger.Default)}
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

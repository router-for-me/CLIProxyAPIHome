package cluster

import (
	"context"
	"fmt"
	"time"

	"gorm.io/gorm"
)

// DatabaseNow returns the current UTC timestamp reported by the database.
func DatabaseNow(ctx context.Context, tx *gorm.DB) (time.Time, error) {
	if tx == nil {
		return time.Time{}, fmt.Errorf("database connection is nil")
	}
	var value string
	if errScan := tx.WithContext(contextOrBackground(ctx)).Raw(databaseNowQuery(tx)).Scan(&value).Error; errScan != nil {
		return time.Time{}, errScan
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05.999999-07:00", "2006-01-02 15:04:05-07:00", "2006-01-02 15:04:05.999999", "2006-01-02 15:04:05"} {
		parsed, errParse := time.Parse(layout, value)
		if errParse == nil {
			return parsed.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("parse database timestamp %q", value)
}

// CurrentDatabaseTime returns the current UTC timestamp reported by the repository database.
func (r *Repository) CurrentDatabaseTime(ctx context.Context) (time.Time, error) {
	db, errDB := r.database()
	if errDB != nil {
		return time.Time{}, errDB
	}
	return DatabaseNow(ctx, db)
}

func databaseNowQuery(tx *gorm.DB) string {
	if tx != nil && tx.Dialector != nil && tx.Dialector.Name() == "postgres" {
		return "SELECT clock_timestamp()"
	}
	return "SELECT CURRENT_TIMESTAMP"
}

func databaseTimestampStep(tx *gorm.DB) time.Duration {
	if tx != nil && tx.Dialector != nil && tx.Dialector.Name() == "postgres" {
		return time.Microsecond
	}
	return time.Second
}

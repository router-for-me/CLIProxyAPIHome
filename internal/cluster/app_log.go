package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/tidwall/gjson"
	"gorm.io/gorm"
)

type AppLogRecord struct {
	ID        uint      `gorm:"column:id;primaryKey;autoIncrement;index:idx_log_time_order,priority:2"`
	Timestamp time.Time `gorm:"column:timestamp;not null;index:idx_log_timestamp;index:idx_log_time_order,priority:1,sort:desc;index:idx_log_client_time,priority:2,sort:desc;index:idx_log_level_time,priority:2,sort:desc"`
	ClientIP  string    `gorm:"column:client_ip;index:idx_log_client_ip;index:idx_log_client_time,priority:1"`
	RequestID string    `gorm:"column:request_id;index:idx_log_request_id;index:idx_log_home_request,priority:2"`
	HomeIP    string    `gorm:"column:home_ip;index:idx_log_home_ip;index:idx_log_home_request,priority:1"`
	Level     string    `gorm:"column:level;index:idx_log_level;index:idx_log_level_time,priority:1"`
	Line      string    `gorm:"column:line;type:text;not null"`
	CreatedAt time.Time `gorm:"column:created_at;not null;index:idx_log_created_at"`
}

type AppLogQuery struct {
	HomeIP    string
	ClientIP  string
	RequestID string
	Level     string
	After     *time.Time
	Before    *time.Time
	Limit     int
	Offset    int
}

type AppLogQueryResult struct {
	Records []AppLogRecord
	Total   int64
}

// TableName returns the database table name.
func (AppLogRecord) TableName() string {
	return "log"
}

// AppLogRecordFromPayload creates an app log record from a CPA payload.
func AppLogRecordFromPayload(clientIP string, homeIP string, payload string) (*AppLogRecord, error) {
	payload = strings.TrimSpace(payload)
	if payload == "" {
		return nil, fmt.Errorf("app log payload is empty")
	}
	if !json.Valid([]byte(payload)) {
		return nil, fmt.Errorf("app log payload is invalid json")
	}

	line := strings.TrimRight(gjson.Get(payload, "line").String(), "\r\n")
	if strings.TrimSpace(line) == "" {
		return nil, fmt.Errorf("app log line is required")
	}

	clientIP = strings.TrimSpace(clientIP)
	if clientIP == "" {
		clientIP = "unknown"
	}
	now := time.Now().UTC()

	return &AppLogRecord{
		Timestamp: appLogTimestampFromPayload(payload, now),
		ClientIP:  clientIP,
		RequestID: strings.TrimSpace(gjson.Get(payload, "request_id").String()),
		HomeIP:    strings.TrimSpace(homeIP),
		Level:     normalizeAppLogLevel(gjson.Get(payload, "level").String()),
		Line:      line,
		CreatedAt: now,
	}, nil
}

// AppendAppLog appends a CPA application log record.
func (r *Repository) AppendAppLog(ctx context.Context, clientIP string, homeIP string, payload string) (*AppLogRecord, error) {
	db, errDB := r.database()
	if errDB != nil {
		return nil, errDB
	}

	record, errRecord := AppLogRecordFromPayload(clientIP, homeIP, payload)
	if errRecord != nil {
		return nil, errRecord
	}
	if errCreate := db.WithContext(contextOrBackground(ctx)).Create(record).Error; errCreate != nil {
		return nil, errCreate
	}
	return record, nil
}

// DeleteAppLogs deletes all CPA application log records.
func (r *Repository) DeleteAppLogs(ctx context.Context) (int64, error) {
	db, errDB := r.database()
	if errDB != nil {
		return 0, errDB
	}

	result := db.WithContext(contextOrBackground(ctx)).Where("1 = 1").Delete(&AppLogRecord{})
	if result.Error != nil {
		return 0, result.Error
	}
	return result.RowsAffected, nil
}

// ListAppLogs returns application log records from the database.
func (r *Repository) ListAppLogs(ctx context.Context, opts AppLogQuery) (AppLogQueryResult, error) {
	db, errDB := r.database()
	if errDB != nil {
		return AppLogQueryResult{}, errDB
	}

	query := appLogQuery(db.WithContext(contextOrBackground(ctx)).Model(&AppLogRecord{}), opts)

	var total int64
	if errCount := query.Count(&total).Error; errCount != nil {
		return AppLogQueryResult{}, errCount
	}

	if opts.Limit > 0 {
		query = query.Limit(opts.Limit)
	}
	if opts.Offset > 0 {
		query = query.Offset(opts.Offset)
	}

	var records []AppLogRecord
	if errFind := query.Order("timestamp DESC, id DESC").Find(&records).Error; errFind != nil {
		return AppLogQueryResult{}, errFind
	}
	return AppLogQueryResult{Records: records, Total: total}, nil
}

func appLogQuery(query *gorm.DB, opts AppLogQuery) *gorm.DB {
	homeIP := strings.TrimSpace(opts.HomeIP)
	if homeIP != "" {
		query = query.Where("home_ip = ?", homeIP)
	}
	clientIP := strings.TrimSpace(opts.ClientIP)
	if clientIP != "" {
		query = query.Where("client_ip = ?", clientIP)
	}
	requestID := strings.TrimSpace(opts.RequestID)
	if requestID != "" {
		query = query.Where("request_id = ?", requestID)
	}
	level := strings.TrimSpace(opts.Level)
	if level != "" {
		query = query.Where("level = ?", normalizeAppLogLevel(level))
	}
	if opts.After != nil {
		query = query.Where("timestamp > ?", opts.After.UTC())
	}
	if opts.Before != nil {
		query = query.Where("timestamp < ?", opts.Before.UTC())
	}
	return query
}

func appLogTimestampFromPayload(payload string, fallback time.Time) time.Time {
	for _, key := range []string{"timestamp", "time", "ts"} {
		raw := strings.TrimSpace(gjson.Get(payload, key).String())
		if raw == "" {
			continue
		}
		if parsed, errParse := time.Parse(time.RFC3339Nano, raw); errParse == nil {
			return parsed.UTC()
		}
	}
	return fallback.UTC()
}

func normalizeAppLogLevel(level string) string {
	level = strings.ToLower(strings.TrimSpace(level))
	if level == "warning" {
		return "warn"
	}
	return level
}

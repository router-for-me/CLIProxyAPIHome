package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"gorm.io/gorm"
)

const defaultUsageServiceTier = "default"

type UsageRecord struct {
	ID                  uint      `gorm:"column:id;primaryKey;autoIncrement;index:idx_usage_time_order,priority:2"`
	Timestamp           time.Time `gorm:"column:timestamp;not null;index:idx_usage_timestamp;index:idx_usage_time_order,priority:1,sort:desc;index:idx_usage_source_time,priority:2,sort:desc;index:idx_usage_auth_time,priority:2,sort:desc;index:idx_usage_failed_time,priority:2,sort:desc;index:idx_usage_failed_status_time,priority:3,sort:desc;index:idx_usage_provider_model_time,priority:3,sort:desc;index:idx_usage_provider_time,priority:2,sort:desc;index:idx_usage_endpoint_time,priority:2,sort:desc;index:idx_usage_home_time,priority:2,sort:desc;index:idx_usage_auth_type_time,priority:2,sort:desc"`
	LatencyMS           int64     `gorm:"column:latency_ms;not null;default:0"`
	TTFTMS              int64     `gorm:"column:ttft_ms;not null;default:0"`
	Source              string    `gorm:"column:source;index:idx_usage_source;index:idx_usage_source_time,priority:1"`
	AuthIndex           string    `gorm:"column:auth_index;index:idx_usage_auth_index;index:idx_usage_auth_time,priority:1"`
	InputTokens         int64     `gorm:"column:input_tokens;not null;default:0"`
	OutputTokens        int64     `gorm:"column:output_tokens;not null;default:0"`
	ReasoningTokens     int64     `gorm:"column:reasoning_tokens;not null;default:0"`
	CachedTokens        int64     `gorm:"column:cached_tokens;not null;default:0"`
	CacheReadTokens     int64     `gorm:"column:cache_read_tokens;not null;default:0"`
	CacheCreationTokens int64     `gorm:"column:cache_creation_tokens;not null;default:0"`
	TotalTokens         int64     `gorm:"column:total_tokens;not null;default:0"`
	Failed              bool      `gorm:"column:failed;not null;default:false;index:idx_usage_failed;index:idx_usage_failed_time,priority:1;index:idx_usage_failed_status_time,priority:1"`
	FailStatusCode      int       `gorm:"column:fail_status_code;not null;default:0;index:idx_usage_failed_status_time,priority:2"`
	FailBody            string    `gorm:"column:fail_body;type:text"`
	Provider            string    `gorm:"column:provider;index:idx_usage_provider_model,priority:1;index:idx_usage_provider_model_time,priority:1;index:idx_usage_provider_time,priority:1"`
	ExecutorType        string    `gorm:"column:executor_type"`
	Model               string    `gorm:"column:model;index:idx_usage_provider_model,priority:2;index:idx_usage_provider_model_time,priority:2"`
	Alias               string    `gorm:"column:alias"`
	Effort              string    `gorm:"column:effort"`
	ServiceTier         string    `gorm:"column:service_tier"`
	Endpoint            string    `gorm:"column:endpoint;index:idx_usage_endpoint;index:idx_usage_endpoint_time,priority:1"`
	AuthType            string    `gorm:"column:auth_type;index:idx_usage_auth_type_time,priority:1"`
	APIKey              string    `gorm:"column:api_key;index:idx_usage_api_key"`
	RequestID           string    `gorm:"column:request_id;index:idx_usage_request_id"`
	UpstreamRequestID   string    `gorm:"column:upstream_request_id;index:idx_usage_upstream_request_id"`
	EventType           string    `gorm:"column:event_type;index:idx_usage_event_type;index:idx_usage_event_time,priority:1"`
	UpstreamStatusCode  int       `gorm:"column:upstream_status_code;not null;default:0;index:idx_usage_upstream_status_code"`
	HomeIP              string    `gorm:"column:home_ip;index:idx_usage_home_ip;index:idx_usage_home_time,priority:1;index:idx_usage_home_port_time,priority:1"`
	HomePort            int       `gorm:"column:home_port;not null;default:0;index:idx_usage_home_port_time,priority:2"`
	CPANodeID           string    `gorm:"column:cpa_node_id;index:idx_usage_cpa_node_id;index:idx_usage_cpa_node_time,priority:1"`
	CPAIP               string    `gorm:"column:cpa_ip;index:idx_usage_cpa_ip"`
	CPAPort             int       `gorm:"column:cpa_port;not null;default:0"`
	CPALabel            string    `gorm:"column:cpa_label;index:idx_usage_cpa_label"`
	TokensJSON          JSONB     `gorm:"column:tokens"`
	FailJSON            JSONB     `gorm:"column:fail"`
	PayloadJSON         JSONB     `gorm:"column:payload;not null"`
	CreatedAt           time.Time `gorm:"column:created_at;not null"`
}

type UsageRuntimeMetadata struct {
	HomeIP    string
	HomePort  int
	CPANodeID string
	CPAIP     string
	CPAPort   int
	CPALabel  string
}

// TableName returns the database table name.
func (UsageRecord) TableName() string {
	return "usage"
}

// UsageRecordFromPayload derives usage record from payload.
func UsageRecordFromPayload(payload string, homeIP string) (*UsageRecord, error) {
	return UsageRecordFromPayloadWithRuntime(payload, UsageRuntimeMetadata{HomeIP: homeIP})
}

// UsageRecordFromPayloadWithRuntime derives usage record from payload and trusted runtime metadata.
func UsageRecordFromPayloadWithRuntime(payload string, metadata UsageRuntimeMetadata) (*UsageRecord, error) {
	// Validate input data before converting it into runtime state.
	payload = strings.TrimSpace(payload)
	if payload == "" {
		return nil, fmt.Errorf("usage payload is empty")
	}
	if !json.Valid([]byte(payload)) {
		return nil, fmt.Errorf("usage payload is invalid json")
	}
	enrichedPayload, errEnrich := UsagePayloadWithRuntimeMetadata(payload, metadata)
	if errEnrich != nil {
		return nil, errEnrich
	}
	payload = enrichedPayload

	timestampRaw := strings.TrimSpace(gjson.Get(payload, "timestamp").String())
	if timestampRaw == "" {
		return nil, fmt.Errorf("usage timestamp is required")
	}
	timestamp, errTimestamp := time.Parse(time.RFC3339Nano, timestampRaw)
	if errTimestamp != nil {
		return nil, fmt.Errorf("parse usage timestamp: %w", errTimestamp)
	}

	record := &UsageRecord{
		Timestamp:           timestamp.UTC(),
		LatencyMS:           gjson.Get(payload, "latency_ms").Int(),
		TTFTMS:              gjson.Get(payload, "ttft_ms").Int(),
		Source:              strings.TrimSpace(gjson.Get(payload, "source").String()),
		AuthIndex:           strings.TrimSpace(gjson.Get(payload, "auth_index").String()),
		InputTokens:         gjson.Get(payload, "tokens.input_tokens").Int(),
		OutputTokens:        gjson.Get(payload, "tokens.output_tokens").Int(),
		ReasoningTokens:     gjson.Get(payload, "tokens.reasoning_tokens").Int(),
		CachedTokens:        gjson.Get(payload, "tokens.cached_tokens").Int(),
		CacheReadTokens:     gjson.Get(payload, "tokens.cache_read_tokens").Int(),
		CacheCreationTokens: gjson.Get(payload, "tokens.cache_creation_tokens").Int(),
		TotalTokens:         gjson.Get(payload, "tokens.total_tokens").Int(),
		Failed:              gjson.Get(payload, "failed").Bool(),
		FailStatusCode:      int(gjson.Get(payload, "fail.status_code").Int()),
		FailBody:            gjson.Get(payload, "fail.body").String(),
		Provider:            strings.TrimSpace(gjson.Get(payload, "provider").String()),
		ExecutorType:        strings.TrimSpace(gjson.Get(payload, "executor_type").String()),
		Model:               strings.TrimSpace(gjson.Get(payload, "model").String()),
		Alias:               strings.TrimSpace(gjson.Get(payload, "alias").String()),
		Effort:              strings.TrimSpace(gjson.Get(payload, "reasoning_effort").String()),
		ServiceTier:         usageServiceTierFromPayload(payload),
		Endpoint:            strings.TrimSpace(gjson.Get(payload, "endpoint").String()),
		AuthType:            strings.TrimSpace(gjson.Get(payload, "auth_type").String()),
		APIKey:              strings.TrimSpace(gjson.Get(payload, "api_key").String()),
		RequestID:           strings.TrimSpace(gjson.Get(payload, "request_id").String()),
		UpstreamRequestID:   usagePayloadString(payload, "upstream_request_id", "upstream.request_id", "response.request_id", "response.id"),
		UpstreamStatusCode:  int(usagePayloadInt(payload, "upstream_status_code", "upstream.status_code", "response.status_code")),
		HomeIP:              usageHomeIP(payload, metadata),
		HomePort:            int(usagePayloadInt(payload, "home_port", "home.port")),
		CPANodeID:           usagePayloadString(payload, "cpa_node_id", "cpa.node_id", "node_id"),
		CPAIP:               usagePayloadString(payload, "cpa_ip", "cpa.ip"),
		CPAPort:             int(usagePayloadInt(payload, "cpa_port", "cpa.port")),
		CPALabel:            usagePayloadString(payload, "cpa_label", "cpa.label"),
		TokensJSON:          jsonbFromPayloadField(payload, "tokens"),
		FailJSON:            jsonbFromPayloadField(payload, "fail"),
		PayloadJSON:         JSONB(payload),
		CreatedAt:           time.Now().UTC(),
	}
	record.EventType = usageEventTypeFromPayload(payload, record.Endpoint)
	if strings.TrimSpace(record.CPALabel) == "" {
		record.CPALabel = usageCPALabel(record.CPANodeID, record.CPAIP, record.CPAPort)
	}
	return record, nil
}

// usageServiceTierFromPayload returns the reported service tier or the default tier.
func usageServiceTierFromPayload(payload string) string {
	serviceTier := strings.TrimSpace(gjson.Get(payload, "service_tier").String())
	if serviceTier == "" {
		return defaultUsageServiceTier
	}
	return serviceTier
}

// AppendUsage appends an usage.
func (r *Repository) AppendUsage(ctx context.Context, payload string, homeIP string) (*UsageRecord, error) {
	return r.AppendUsageWithRuntime(ctx, payload, UsageRuntimeMetadata{HomeIP: homeIP})
}

// AppendUsageWithRuntime appends usage with trusted Home/CPA runtime metadata.
func (r *Repository) AppendUsageWithRuntime(ctx context.Context, payload string, metadata UsageRuntimeMetadata) (*UsageRecord, error) {
	record, errRecord := UsageRecordFromPayloadWithRuntime(payload, metadata)
	if errRecord != nil {
		return nil, errRecord
	}

	db, errDB := r.database()
	if errDB != nil {
		return nil, errDB
	}

	ctx = contextOrBackground(ctx)
	errTransaction := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if errCreate := tx.WithContext(ctx).Create(record).Error; errCreate != nil {
			return errCreate
		}
		return r.createBillingChargeForUsageTx(ctx, tx, record, payload)
	})
	if errTransaction != nil {
		return nil, errTransaction
	}
	return record, nil
}

// UsagePayloadWithRuntimeMetadata fills missing runtime ownership fields without overriding reported values.
func UsagePayloadWithRuntimeMetadata(payload string, metadata UsageRuntimeMetadata) (string, error) {
	payload = strings.TrimSpace(payload)
	if payload == "" {
		return "", fmt.Errorf("usage payload is empty")
	}
	if !json.Valid([]byte(payload)) {
		return "", fmt.Errorf("usage payload is invalid json")
	}

	out := payload
	var errSet error
	if metadata.HomePort > 0 && usagePayloadInt(out, "home_port", "home.port") <= 0 {
		out, errSet = sjson.Set(out, "home_port", metadata.HomePort)
		if errSet != nil {
			return "", errSet
		}
	}
	if value := strings.TrimSpace(metadata.CPANodeID); value != "" && usagePayloadString(out, "cpa_node_id", "cpa.node_id", "node_id") == "" {
		out, errSet = sjson.Set(out, "cpa_node_id", value)
		if errSet != nil {
			return "", errSet
		}
	}
	if value := strings.TrimSpace(metadata.CPAIP); value != "" && usagePayloadString(out, "cpa_ip", "cpa.ip") == "" {
		out, errSet = sjson.Set(out, "cpa_ip", value)
		if errSet != nil {
			return "", errSet
		}
	}
	if metadata.CPAPort > 0 && usagePayloadInt(out, "cpa_port", "cpa.port") <= 0 {
		out, errSet = sjson.Set(out, "cpa_port", metadata.CPAPort)
		if errSet != nil {
			return "", errSet
		}
	}
	label := strings.TrimSpace(metadata.CPALabel)
	if label == "" {
		label = usageRuntimeCPALabel(metadata)
	}
	if label != "" && usagePayloadString(out, "cpa_label", "cpa.label") == "" {
		out, errSet = sjson.Set(out, "cpa_label", label)
		if errSet != nil {
			return "", errSet
		}
	}
	return out, nil
}

func usageRuntimeCPALabel(metadata UsageRuntimeMetadata) string {
	return usageCPALabel(metadata.CPANodeID, metadata.CPAIP, metadata.CPAPort)
}

func usageCPALabel(nodeID string, ip string, port int) string {
	if nodeID := strings.TrimSpace(nodeID); nodeID != "" {
		return nodeID
	}
	ip = strings.TrimSpace(ip)
	if ip == "" {
		return ""
	}
	if port > 0 {
		return fmt.Sprintf("%s:%d", ip, port)
	}
	return ip
}

func usageHomeIP(payload string, metadata UsageRuntimeMetadata) string {
	homeIP := strings.TrimSpace(metadata.HomeIP)
	if homeIP != "" {
		return homeIP
	}
	return usagePayloadString(payload, "home_ip", "home.ip")
}

func usageEventTypeFromPayload(payload string, endpoint string) string {
	raw := usagePayloadString(payload, "event_type", "event.type", "type")
	if normalized := normalizeUsageObservabilityEventType(raw); normalized != "" {
		return normalized
	}
	return normalizeUsageObservabilityEndpointEventType(endpoint)
}

func usagePayloadString(payload string, paths ...string) string {
	for _, path := range paths {
		if value := strings.TrimSpace(gjson.Get(payload, path).String()); value != "" {
			return value
		}
	}
	return ""
}

func usagePayloadInt(payload string, paths ...string) int64 {
	for _, path := range paths {
		value := gjson.Get(payload, path)
		if !value.Exists() {
			continue
		}
		if value.Type == gjson.String {
			parsed, errParse := strconv.ParseInt(strings.TrimSpace(value.String()), 10, 64)
			if errParse == nil {
				return parsed
			}
			continue
		}
		return value.Int()
	}
	return 0
}

// jsonbFromPayloadField derives jsonb from payload field.
func jsonbFromPayloadField(payload string, path string) JSONB {
	value := gjson.Get(payload, path)
	if !value.Exists() || strings.TrimSpace(value.Raw) == "" {
		return nil
	}
	return JSONB(value.Raw)
}

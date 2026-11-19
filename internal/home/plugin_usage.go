package home

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// PluginUsageSink confirms callback completion, not an in-memory enqueue.
// A non-nil error leaves delivery incomplete and requires sender retry.
type PluginUsageSink interface {
	HandleUsage(context.Context, pluginapi.UsageRecord) error
}

// UsagePluginSink returns the host only when a plugin advertises usage delivery.
func (r *Runtime) UsagePluginSink() PluginUsageSink {
	if r == nil || r.pluginHost == nil || !r.pluginHost.HasUsagePlugins() {
		return nil
	}
	return r.pluginHost
}

// DeliverPluginUsage waits for the plugin boundary before committing completion.
// A nil sink records that this event had no usage consumers, without replaying it
// into plugins loaded after the original ingestion.
func (r *Runtime) DeliverPluginUsage(ctx context.Context, payload string, sink PluginUsageSink, record pluginapi.UsageRecord) error {
	if r == nil || r.clusterAdapter == nil || !r.clusterAdapter.Enabled() {
		if sink == nil {
			return nil
		}
		return fmt.Errorf("usage plugin delivery requires a database")
	}
	store, supported := r.clusterAdapter.(interface {
		DeliverPluginUsage(context.Context, string, func() error) error
	})
	if !supported {
		return fmt.Errorf("cluster usage plugin delivery store is unavailable")
	}
	var deliver func() error
	if sink != nil {
		deliver = func() error { return sink.HandleUsage(context.WithoutCancel(ctx), record) }
	}
	return store.DeliverPluginUsage(ctx, payload, deliver)
}

// DecodePluginUsagePayload translates the existing CPA usage wire format, not
// a new billing event. The execution ID and selection ID have distinct lifetimes.
func DecodePluginUsagePayload(payload string) (pluginapi.UsageRecord, error) {
	var wire *struct {
		RequestID            string      `json:"request_id"`
		ExecutionID          string      `json:"execution_id"`
		TraceID              string      `json:"trace_id"`
		SelectionID          string      `json:"selection_id"`
		Provider             string      `json:"provider"`
		BaseURL              string      `json:"base_url"`
		ExecutorType         string      `json:"executor_type"`
		Model                string      `json:"model"`
		Alias                string      `json:"alias"`
		APIKey               string      `json:"api_key"`
		AccessProvider       string      `json:"access_provider"`
		SessionID            string      `json:"session_id"`
		ParentSessionID      string      `json:"parent_session_id"`
		AuthID               string      `json:"auth_id"`
		AuthIndex            string      `json:"auth_index"`
		AuthType             string      `json:"auth_type"`
		Source               string      `json:"source"`
		ReasoningEffort      string      `json:"reasoning_effort"`
		ServiceTier          string      `json:"service_tier"`
		EffectiveServiceTier string      `json:"effective_service_tier"`
		ResponseServiceTier  string      `json:"response_service_tier"`
		ResponseModel        string      `json:"response_model"`
		IsNativeKey          bool        `json:"is_native_key"`
		Generate             *bool       `json:"generate"`
		Stream               bool        `json:"stream"`
		Failed               bool        `json:"failed"`
		Timestamp            time.Time   `json:"timestamp"`
		LatencyMS            int64       `json:"latency_ms"`
		TTFTMS               int64       `json:"ttft_ms"`
		ResponseHeaders      http.Header `json:"response_headers"`
		Fail                 struct {
			StatusCode int    `json:"status_code"`
			Body       string `json:"body"`
		} `json:"fail"`
		Tokens struct {
			InputTokens           int64  `json:"input_tokens"`
			OutputTokens          int64  `json:"output_tokens"`
			ReasoningTokens       int64  `json:"reasoning_tokens"`
			CachedTokens          int64  `json:"cached_tokens"`
			CacheReadTokens       int64  `json:"cache_read_tokens"`
			CacheCreationTokens   int64  `json:"cache_creation_tokens"`
			TotalTokens           int64  `json:"total_tokens"`
			CacheCreation5mTokens *int64 `json:"cache_creation_5m_tokens"`
			CacheCreation1hTokens *int64 `json:"cache_creation_1h_tokens"`
			TextInputTokens       *int64 `json:"text_input_tokens"`
			ImageInputTokens      *int64 `json:"image_input_tokens"`
			AudioInputTokens      *int64 `json:"audio_input_tokens"`
			VideoInputTokens      *int64 `json:"video_input_tokens"`
			FileInputTokens       *int64 `json:"file_input_tokens"`
			CachedTextTokens      *int64 `json:"cached_text_tokens"`
			CachedImageTokens     *int64 `json:"cached_image_tokens"`
			CachedAudioTokens     *int64 `json:"cached_audio_tokens"`
			TextOutputTokens      *int64 `json:"text_output_tokens"`
			ImageOutputTokens     *int64 `json:"image_output_tokens"`
			AudioOutputTokens     *int64 `json:"audio_output_tokens"`
		} `json:"tokens"`
	}
	if errDecode := json.Unmarshal([]byte(payload), &wire); errDecode != nil {
		return pluginapi.UsageRecord{}, fmt.Errorf("decode plugin usage: %w", errDecode)
	}
	if wire == nil {
		return pluginapi.UsageRecord{}, fmt.Errorf("invalid plugin usage object")
	}
	if wire.LatencyMS < 0 || wire.TTFTMS < 0 || wire.LatencyMS > math.MaxInt64/int64(time.Millisecond) || wire.TTFTMS > math.MaxInt64/int64(time.Millisecond) {
		return pluginapi.UsageRecord{}, fmt.Errorf("invalid plugin usage duration")
	}
	record := pluginapi.UsageRecord{
		RequestID:            strings.TrimSpace(wire.ExecutionID),
		TraceID:              wire.TraceID,
		SelectionID:          wire.SelectionID,
		Provider:             wire.Provider,
		BaseURL:              wire.BaseURL,
		ExecutorType:         wire.ExecutorType,
		Model:                wire.Model,
		Alias:                wire.Alias,
		APIKey:               wire.APIKey,
		AccessProvider:       wire.AccessProvider,
		SessionID:            wire.SessionID,
		ParentSessionID:      wire.ParentSessionID,
		AuthID:               wire.AuthID,
		AuthIndex:            wire.AuthIndex,
		AuthType:             wire.AuthType,
		Source:               wire.Source,
		ReasoningEffort:      wire.ReasoningEffort,
		ServiceTier:          wire.ServiceTier,
		EffectiveServiceTier: wire.EffectiveServiceTier,
		ResponseServiceTier:  wire.ResponseServiceTier,
		ResponseModel:        wire.ResponseModel,
		IsNativeKey:          wire.IsNativeKey,
		Generate:             wire.Generate == nil || *wire.Generate,
		Stream:               wire.Stream, Failed: wire.Failed,
		RequestedAt:     wire.Timestamp,
		Latency:         time.Duration(wire.LatencyMS) * time.Millisecond,
		TTFT:            time.Duration(wire.TTFTMS) * time.Millisecond,
		ResponseHeaders: wire.ResponseHeaders,
		Failure:         pluginapi.UsageFailure{StatusCode: wire.Fail.StatusCode, Body: wire.Fail.Body},
		Detail: pluginapi.UsageDetail{
			InputTokens:           wire.Tokens.InputTokens,
			OutputTokens:          wire.Tokens.OutputTokens,
			ReasoningTokens:       wire.Tokens.ReasoningTokens,
			CachedTokens:          wire.Tokens.CachedTokens,
			CacheReadTokens:       wire.Tokens.CacheReadTokens,
			CacheCreationTokens:   wire.Tokens.CacheCreationTokens,
			TotalTokens:           wire.Tokens.TotalTokens,
			CacheCreation5mTokens: wire.Tokens.CacheCreation5mTokens,
			CacheCreation1hTokens: wire.Tokens.CacheCreation1hTokens,
			TextInputTokens:       wire.Tokens.TextInputTokens,
			ImageInputTokens:      wire.Tokens.ImageInputTokens,
			AudioInputTokens:      wire.Tokens.AudioInputTokens,
			VideoInputTokens:      wire.Tokens.VideoInputTokens,
			FileInputTokens:       wire.Tokens.FileInputTokens,
			CachedTextTokens:      wire.Tokens.CachedTextTokens,
			CachedImageTokens:     wire.Tokens.CachedImageTokens,
			CachedAudioTokens:     wire.Tokens.CachedAudioTokens,
			TextOutputTokens:      wire.Tokens.TextOutputTokens,
			ImageOutputTokens:     wire.Tokens.ImageOutputTokens,
			AudioOutputTokens:     wire.Tokens.AudioOutputTokens,
		},
	}
	if record.TraceID == "" {
		record.TraceID = wire.RequestID
	}
	if record.AuthID == "" {
		record.AuthID = wire.AuthIndex
	}
	record.SelectionID = strings.TrimSpace(record.SelectionID)
	return record, nil
}

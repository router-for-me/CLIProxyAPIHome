package cluster

import (
	"encoding/json"
	"fmt"
	"strings"
)

const UsageTokenAccountingSchemaVersion = 2

const (
	UsageTokenAccountingQualityComplete     = "complete"
	UsageTokenAccountingQualityInconsistent = "inconsistent"
	UsageTokenAccountingQualityUnclassified = "unclassified"
)

type UsageTokenInputBreakdown struct {
	TotalTokens      int64 `json:"total_tokens"`
	UncachedTokens   int64 `json:"uncached_tokens"`
	CacheReadTokens  int64 `json:"cache_read_tokens"`
	CacheWriteTokens int64 `json:"cache_write_tokens"`
}

type UsageTokenOutputBreakdown struct {
	TotalTokens        int64 `json:"total_tokens"`
	NonReasoningTokens int64 `json:"non_reasoning_tokens"`
	ReasoningTokens    int64 `json:"reasoning_tokens"`
}

type UsageTokenBreakdown struct {
	SchemaVersion      int                       `json:"schema_version"`
	Quality            string                    `json:"quality"`
	TotalTokens        int64                     `json:"total_tokens"`
	Input              UsageTokenInputBreakdown  `json:"input"`
	Output             UsageTokenOutputBreakdown `json:"output"`
	UnclassifiedTokens int64                     `json:"unclassified_tokens"`
}

func (b UsageTokenBreakdown) Valid() bool {
	if b.SchemaVersion != UsageTokenAccountingSchemaVersion || !validUsageTokenAccountingQuality(b.Quality) {
		return false
	}
	if b.TotalTokens < 0 || b.UnclassifiedTokens < 0 ||
		b.Input.TotalTokens < 0 || b.Input.UncachedTokens < 0 ||
		b.Input.CacheReadTokens < 0 || b.Input.CacheWriteTokens < 0 ||
		b.Output.TotalTokens < 0 || b.Output.NonReasoningTokens < 0 ||
		b.Output.ReasoningTokens < 0 {
		return false
	}
	if b.Input.TotalTokens != b.Input.UncachedTokens+b.Input.CacheReadTokens+b.Input.CacheWriteTokens {
		return false
	}
	if b.Output.TotalTokens != b.Output.NonReasoningTokens+b.Output.ReasoningTokens {
		return false
	}
	if b.TotalTokens != b.Input.TotalTokens+b.Output.TotalTokens+b.UnclassifiedTokens {
		return false
	}
	return b.Quality != UsageTokenAccountingQualityComplete || b.UnclassifiedTokens == 0
}

func validUsageTokenAccountingQuality(quality string) bool {
	switch strings.TrimSpace(quality) {
	case UsageTokenAccountingQualityComplete, UsageTokenAccountingQualityInconsistent, UsageTokenAccountingQualityUnclassified:
		return true
	default:
		return false
	}
}

func usageTokenBreakdownFromPayload(payload string, legacy usageLegacyTokenCounters) (UsageTokenBreakdown, error) {
	var envelope struct {
		AccountingVersion int                 `json:"accounting_version"`
		TokenBreakdown    UsageTokenBreakdown `json:"token_breakdown"`
	}
	if errUnmarshal := json.Unmarshal([]byte(payload), &envelope); errUnmarshal != nil {
		return UsageTokenBreakdown{}, fmt.Errorf("decode usage token accounting: %w", errUnmarshal)
	}
	if envelope.AccountingVersion == 0 && envelope.TokenBreakdown.SchemaVersion == 0 {
		return usageTokenBreakdownFromLegacy(legacy), nil
	}
	if envelope.AccountingVersion != UsageTokenAccountingSchemaVersion || envelope.TokenBreakdown.SchemaVersion != UsageTokenAccountingSchemaVersion {
		return UsageTokenBreakdown{}, fmt.Errorf("unsupported usage token accounting version %d/%d", envelope.AccountingVersion, envelope.TokenBreakdown.SchemaVersion)
	}
	if !envelope.TokenBreakdown.Valid() {
		return UsageTokenBreakdown{}, fmt.Errorf("usage token breakdown violates schema v2 invariants")
	}
	return envelope.TokenBreakdown, nil
}

type usageLegacyTokenCounters struct {
	Provider            string
	ExecutorType        string
	InputTokens         int64
	OutputTokens        int64
	ReasoningTokens     int64
	CacheReadTokens     int64
	CacheCreationTokens int64
	TotalTokens         int64
}

func usageTokenBreakdownFromLegacy(input usageLegacyTokenCounters) UsageTokenBreakdown {
	semantics := usageLegacyTokenSemantics(input.Provider, input.ExecutorType)
	switch semantics {
	case "independent":
		return newIndependentUsageTokenBreakdown(
			input.InputTokens,
			input.CacheReadTokens,
			input.CacheCreationTokens,
			input.OutputTokens,
			input.ReasoningTokens,
			input.TotalTokens,
		)
	case "separate_reasoning":
		return newSeparateReasoningUsageTokenBreakdown(
			input.InputTokens,
			input.CacheReadTokens,
			input.CacheCreationTokens,
			input.OutputTokens,
			input.ReasoningTokens,
			input.TotalTokens,
		)
	case "subset":
		return newSubsetUsageTokenBreakdown(
			input.InputTokens,
			input.CacheReadTokens,
			input.CacheCreationTokens,
			input.OutputTokens,
			input.ReasoningTokens,
			input.TotalTokens,
		)
	default:
		total := input.TotalTokens
		if total == 0 {
			total = input.InputTokens + input.OutputTokens
		}
		return newUnclassifiedUsageTokenBreakdown(total)
	}
}

func usageLegacyTokenSemantics(provider string, executorType string) string {
	normalizedProvider := strings.ToLower(strings.TrimSpace(provider))
	normalizedExecutor := strings.ToLower(strings.TrimSpace(executorType))
	value := strings.TrimSpace(normalizedProvider + " " + normalizedExecutor)
	if value == "" || value == "unknown unknown" || value == "unknown" {
		return ""
	}
	if normalizedExecutor == "openaicompatexecutor" || normalizedProvider == "openai-compatibility" || strings.HasPrefix(normalizedProvider, "openai-compatible-") {
		return "subset"
	}
	if strings.Contains(value, "claude") || strings.Contains(value, "anthropic") {
		return "independent"
	}
	for _, marker := range []string{"gemini", "aistudio", "antigravity", "vertex", "interaction"} {
		if strings.Contains(value, marker) {
			return "separate_reasoning"
		}
	}
	for _, marker := range []string{"openai", "codex", "xai", "grok", "kimi", "qwen", "deepseek", "openrouter"} {
		if strings.Contains(value, marker) {
			return "subset"
		}
	}
	return ""
}

func newSubsetUsageTokenBreakdown(inputTotal, cacheRead, cacheWrite, outputTotal, reasoning, total int64) UsageTokenBreakdown {
	expected, okExpected := nonNegativeUsageTokenSum(inputTotal, outputTotal)
	if !okExpected || cacheRead < 0 || cacheWrite < 0 || reasoning < 0 || cacheRead+cacheWrite > inputTotal || reasoning > outputTotal {
		return newInconsistentUsageTokenBreakdown(total, expected)
	}
	resolved, okTotal := resolveUsageTokenTotal(total, expected)
	if !okTotal {
		return newInconsistentUsageTokenBreakdown(total, expected)
	}
	return UsageTokenBreakdown{
		SchemaVersion: UsageTokenAccountingSchemaVersion,
		Quality:       UsageTokenAccountingQualityComplete,
		TotalTokens:   resolved,
		Input: UsageTokenInputBreakdown{
			TotalTokens:      inputTotal,
			UncachedTokens:   inputTotal - cacheRead - cacheWrite,
			CacheReadTokens:  cacheRead,
			CacheWriteTokens: cacheWrite,
		},
		Output: UsageTokenOutputBreakdown{
			TotalTokens:        outputTotal,
			NonReasoningTokens: outputTotal - reasoning,
			ReasoningTokens:    reasoning,
		},
	}
}

func newIndependentUsageTokenBreakdown(uncachedInput, cacheRead, cacheWrite, nonReasoningOutput, reasoning, total int64) UsageTokenBreakdown {
	inputTotal, okInput := nonNegativeUsageTokenSum(uncachedInput, cacheRead, cacheWrite)
	outputTotal, okOutput := nonNegativeUsageTokenSum(nonReasoningOutput, reasoning)
	expected, okExpected := nonNegativeUsageTokenSum(inputTotal, outputTotal)
	if !okInput || !okOutput || !okExpected {
		return newInconsistentUsageTokenBreakdown(total, expected)
	}
	resolved, okTotal := resolveUsageTokenTotal(total, expected)
	if !okTotal {
		return newInconsistentUsageTokenBreakdown(total, expected)
	}
	return UsageTokenBreakdown{
		SchemaVersion: UsageTokenAccountingSchemaVersion,
		Quality:       UsageTokenAccountingQualityComplete,
		TotalTokens:   resolved,
		Input: UsageTokenInputBreakdown{
			TotalTokens:      inputTotal,
			UncachedTokens:   uncachedInput,
			CacheReadTokens:  cacheRead,
			CacheWriteTokens: cacheWrite,
		},
		Output: UsageTokenOutputBreakdown{
			TotalTokens:        outputTotal,
			NonReasoningTokens: nonReasoningOutput,
			ReasoningTokens:    reasoning,
		},
	}
}

func newSeparateReasoningUsageTokenBreakdown(inputTotal, cacheRead, cacheWrite, nonReasoningOutput, reasoning, total int64) UsageTokenBreakdown {
	if inputTotal < 0 || cacheRead < 0 || cacheWrite < 0 || cacheRead+cacheWrite > inputTotal {
		return newInconsistentUsageTokenBreakdown(total, 0)
	}
	outputTotal, okOutput := nonNegativeUsageTokenSum(nonReasoningOutput, reasoning)
	expected, okExpected := nonNegativeUsageTokenSum(inputTotal, outputTotal)
	if !okOutput || !okExpected {
		return newInconsistentUsageTokenBreakdown(total, expected)
	}
	resolved, okTotal := resolveUsageTokenTotal(total, expected)
	if !okTotal {
		return newInconsistentUsageTokenBreakdown(total, expected)
	}
	return UsageTokenBreakdown{
		SchemaVersion: UsageTokenAccountingSchemaVersion,
		Quality:       UsageTokenAccountingQualityComplete,
		TotalTokens:   resolved,
		Input: UsageTokenInputBreakdown{
			TotalTokens:      inputTotal,
			UncachedTokens:   inputTotal - cacheRead - cacheWrite,
			CacheReadTokens:  cacheRead,
			CacheWriteTokens: cacheWrite,
		},
		Output: UsageTokenOutputBreakdown{
			TotalTokens:        outputTotal,
			NonReasoningTokens: nonReasoningOutput,
			ReasoningTokens:    reasoning,
		},
	}
}

func newUnclassifiedUsageTokenBreakdown(total int64) UsageTokenBreakdown {
	if total <= 0 {
		quality := UsageTokenAccountingQualityComplete
		if total < 0 {
			quality = UsageTokenAccountingQualityInconsistent
		}
		return UsageTokenBreakdown{SchemaVersion: UsageTokenAccountingSchemaVersion, Quality: quality}
	}
	return UsageTokenBreakdown{
		SchemaVersion:      UsageTokenAccountingSchemaVersion,
		Quality:            UsageTokenAccountingQualityUnclassified,
		TotalTokens:        total,
		UnclassifiedTokens: total,
	}
}

func newInconsistentUsageTokenBreakdown(total int64, fallback int64) UsageTokenBreakdown {
	resolved := total
	if resolved <= 0 {
		resolved = fallback
	}
	if resolved < 0 {
		resolved = 0
	}
	return UsageTokenBreakdown{
		SchemaVersion:      UsageTokenAccountingSchemaVersion,
		Quality:            UsageTokenAccountingQualityInconsistent,
		TotalTokens:        resolved,
		UnclassifiedTokens: resolved,
	}
}

func resolveUsageTokenTotal(total int64, expected int64) (int64, bool) {
	if total < 0 || expected < 0 {
		return 0, false
	}
	if total == 0 {
		return expected, true
	}
	return total, total == expected
}

func nonNegativeUsageTokenSum(values ...int64) (int64, bool) {
	var total int64
	for _, value := range values {
		if value < 0 || total > int64(^uint64(0)>>1)-value {
			return 0, false
		}
		total += value
	}
	return total, true
}

func usageTokenBreakdownFromRecord(record *UsageRecord) UsageTokenBreakdown {
	if record == nil {
		return newUnclassifiedUsageTokenBreakdown(0)
	}
	return UsageTokenBreakdown{
		SchemaVersion: record.TokenAccountingVersion,
		Quality:       record.TokenAccountingQuality,
		TotalTokens:   record.AccountingTotalTokens,
		Input: UsageTokenInputBreakdown{
			TotalTokens:      record.AccountingInputTokens,
			UncachedTokens:   record.UncachedInputTokens,
			CacheReadTokens:  record.AccountingCacheReadTokens,
			CacheWriteTokens: record.AccountingCacheWriteTokens,
		},
		Output: UsageTokenOutputBreakdown{
			TotalTokens:        record.AccountingOutputTokens,
			NonReasoningTokens: record.NonReasoningOutputTokens,
			ReasoningTokens:    record.AccountingReasoningTokens,
		},
		UnclassifiedTokens: record.UnclassifiedTokens,
	}
}

func usageTokenBreakdownForRecord(record *UsageRecord) UsageTokenBreakdown {
	breakdown := usageTokenBreakdownFromRecord(record)
	if breakdown.Valid() {
		return breakdown
	}
	if record == nil {
		return newUnclassifiedUsageTokenBreakdown(0)
	}
	cacheReadTokens := normalizedUsageCacheReadTokens(record.Provider, record.ExecutorType, record.CachedTokens, record.CacheReadTokens, record.CacheReadTokensPresent)
	return usageTokenBreakdownFromLegacy(usageLegacyTokenCounters{
		Provider:            record.Provider,
		ExecutorType:        record.ExecutorType,
		InputTokens:         record.InputTokens,
		OutputTokens:        record.OutputTokens,
		ReasoningTokens:     record.ReasoningTokens,
		CacheReadTokens:     cacheReadTokens,
		CacheCreationTokens: record.CacheCreationTokens,
		TotalTokens:         record.TotalTokens,
	})
}

func usageTokenBreakdownFromValues(quality string, total, inputTotal, uncachedInput, cacheRead, cacheWrite, outputTotal, nonReasoningOutput, reasoning, unclassified int64) UsageTokenBreakdown {
	breakdown := UsageTokenBreakdown{
		SchemaVersion: UsageTokenAccountingSchemaVersion,
		Quality:       strings.TrimSpace(quality),
		TotalTokens:   total,
		Input: UsageTokenInputBreakdown{
			TotalTokens:      inputTotal,
			UncachedTokens:   uncachedInput,
			CacheReadTokens:  cacheRead,
			CacheWriteTokens: cacheWrite,
		},
		Output: UsageTokenOutputBreakdown{
			TotalTokens:        outputTotal,
			NonReasoningTokens: nonReasoningOutput,
			ReasoningTokens:    reasoning,
		},
		UnclassifiedTokens: unclassified,
	}
	if breakdown.Quality == "" {
		breakdown.Quality = UsageTokenAccountingQualityComplete
	}
	if breakdown.Valid() {
		return breakdown
	}
	return newInconsistentUsageTokenBreakdown(total, 0)
}

func mergeUsageTokenBreakdowns(left UsageTokenBreakdown, right UsageTokenBreakdown) UsageTokenBreakdown {
	if !left.Valid() {
		left = newUnclassifiedUsageTokenBreakdown(0)
	}
	if !right.Valid() {
		right = newInconsistentUsageTokenBreakdown(right.TotalTokens, 0)
	}
	quality := mergeUsageTokenAccountingQuality(left.Quality, left.UnclassifiedTokens, right.Quality, right.UnclassifiedTokens)
	return usageTokenBreakdownFromValues(
		quality,
		left.TotalTokens+right.TotalTokens,
		left.Input.TotalTokens+right.Input.TotalTokens,
		left.Input.UncachedTokens+right.Input.UncachedTokens,
		left.Input.CacheReadTokens+right.Input.CacheReadTokens,
		left.Input.CacheWriteTokens+right.Input.CacheWriteTokens,
		left.Output.TotalTokens+right.Output.TotalTokens,
		left.Output.NonReasoningTokens+right.Output.NonReasoningTokens,
		left.Output.ReasoningTokens+right.Output.ReasoningTokens,
		left.UnclassifiedTokens+right.UnclassifiedTokens,
	)
}

func mergeUsageTokenAccountingQuality(left string, leftUnclassified int64, right string, rightUnclassified int64) string {
	if left == UsageTokenAccountingQualityInconsistent || right == UsageTokenAccountingQualityInconsistent {
		return UsageTokenAccountingQualityInconsistent
	}
	if (left == UsageTokenAccountingQualityUnclassified && leftUnclassified > 0) ||
		(right == UsageTokenAccountingQualityUnclassified && rightUnclassified > 0) {
		return UsageTokenAccountingQualityUnclassified
	}
	return UsageTokenAccountingQualityComplete
}

func usageTokenBreakdownUpdates(breakdown UsageTokenBreakdown) map[string]any {
	return map[string]any{
		"token_accounting_version":      breakdown.SchemaVersion,
		"token_accounting_quality":      breakdown.Quality,
		"accounting_total_tokens":       breakdown.TotalTokens,
		"accounting_input_tokens":       breakdown.Input.TotalTokens,
		"uncached_input_tokens":         breakdown.Input.UncachedTokens,
		"accounting_cache_read_tokens":  breakdown.Input.CacheReadTokens,
		"accounting_cache_write_tokens": breakdown.Input.CacheWriteTokens,
		"accounting_output_tokens":      breakdown.Output.TotalTokens,
		"non_reasoning_output_tokens":   breakdown.Output.NonReasoningTokens,
		"accounting_reasoning_tokens":   breakdown.Output.ReasoningTokens,
		"unclassified_tokens":           breakdown.UnclassifiedTokens,
	}
}

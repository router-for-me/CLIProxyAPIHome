package home

import "time"

// concurrencyRetryError fixes a missing retry hint at the admission boundary so
// dispatch comparison and protocol serialization use the same sampled delay.
// Unwrap preserves the original typed admission error and its cause.
type concurrencyRetryError struct {
	error
	retryAfter time.Duration
}

func (e *concurrencyRetryError) Unwrap() error { return e.error }

func (e *concurrencyRetryError) RetryAfter() *time.Duration { return &e.retryAfter }

// ConcurrencyRetryAfterMS preserves explicit hints or samples the configured busy
// retry interval. Partial runtime configs retain the existing protocol defaults.
func (r *Runtime) ConcurrencyRetryAfterMS(retryAfterMS int64) int64 {
	if retryAfterMS > 0 {
		return retryAfterMS
	}
	min, max := int64(250), int64(1000)
	if r != nil {
		if runtimeConfig := r.Config(); runtimeConfig != nil {
			cfg := runtimeConfig.CredentialConcurrency
			if duration, errParse := time.ParseDuration(cfg.BusyRetryMin); errParse == nil && duration > 0 && duration%time.Millisecond == 0 {
				min = duration.Milliseconds()
			}
			if duration, errParse := time.ParseDuration(cfg.BusyRetryMax); errParse == nil && duration > 0 && duration%time.Millisecond == 0 {
				max = duration.Milliseconds()
			}
		}
	}
	if max < min {
		max = min
	}
	return min + time.Now().UnixNano()%(max-min+1)
}

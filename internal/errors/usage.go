package errors

import "errors"

// ErrUsageAPIKeyNotFound identifies a deleted or rotated key on completed usage.
// It is terminal for permission reconstruction, not a database outage.
var ErrUsageAPIKeyNotFound = errors.New("usage API key not found")

// ErrUsagePluginDeliveryPending requires retry without acknowledging the event.
var ErrUsagePluginDeliveryPending = errors.New("usage plugin delivery pending")

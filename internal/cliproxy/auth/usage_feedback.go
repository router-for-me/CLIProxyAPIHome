package auth

import "context"

// UsageFeedback connects one ingestion to its durable feedback batch. Ingestion
// fills Result and the delivery flags; the auth transaction loads Pending before
// invoking the result state machine. Each delivery owns a separate context slot.
type UsageFeedback struct {
	ScopeKey      string
	EventKey      string
	Result        Result
	Applied       bool
	NeedsFeedback bool
	Pending       []Result
}

type usageFeedbackContextKey struct{}

// WithUsageFeedback allocates a separate observation slot for this delivery.
func WithUsageFeedback(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, usageFeedbackContextKey{}, &UsageFeedback{})
}

func UsageFeedbackFromContext(ctx context.Context) *UsageFeedback {
	if ctx == nil {
		return nil
	}
	feedback, _ := ctx.Value(usageFeedbackContextKey{}).(*UsageFeedback)
	return feedback
}

// RequestScoped reports failures which must not participate in credential feedback.
func (r Result) RequestScoped() bool {
	return !r.Success && ((r.Error != nil && r.Error.Code == ErrorCodeRequestScoped) || isRequestScopedNotFoundResultError(r.Error))
}

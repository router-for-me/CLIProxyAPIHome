package auth

// DispatchDecision captures the scheduler selection for an incoming request.
type DispatchDecision struct {
	Auth          *Auth
	Provider      string
	UpstreamModel string
	RequestRetry  int
	SelectionID   string
	PooledModels  bool
	ForceMapping  bool
	OriginalAlias string
}

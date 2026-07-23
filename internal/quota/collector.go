package quota

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
	"github.com/router-for-me/CLIProxyAPIHome/internal/proxyutil"
	log "github.com/sirupsen/logrus"
)

const (
	defaultPollInterval        = time.Minute
	defaultProbeTimeout        = 20 * time.Second
	defaultProbeLeaseDuration  = time.Minute
	defaultSnapshotFreshness   = 30 * time.Minute
	defaultFailureBackoff      = 5 * time.Minute
	maxFailureBackoff          = time.Hour
	defaultProviderConcurrency = 3
	maxProbeResponseBytes      = 1 << 20
	codexUsageURL              = "https://chatgpt.com/backend-api/wham/usage"
	codexResetCreditsURL       = "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits"
	claudeUsageURL             = "https://api.anthropic.com/api/oauth/usage"
	claudeProfileURL           = "https://api.anthropic.com/api/oauth/profile"
	kimiUsageURL               = "https://api.kimi.com/coding/v1/usages"
	xaiBillingURL              = "https://cli-chat-proxy.grok.com/v1/billing"
	codexUserAgent             = "codex_cli_rs/0.76.0 (Debian 13.0.0; x86_64) WindowsTerminal"
	antigravityUserAgent       = "antigravity/1.11.5 windows/amd64"
	xaiTokenAuthHeader         = "X-XAI-Token-Auth"
	xaiTokenAuthValue          = "xai-grok-cli"
	xaiClientVersionHeader     = "x-grok-client-version"
	xaiClientVersionValue      = "0.2.93"
	xaiGrokUserAgent           = "xai-grok-workspace/0.2.93"
)

var defaultAntigravityURLs = []string{
	"https://daily-cloudcode-pa.googleapis.com/v1internal:retrieveUserQuotaSummary",
	"https://daily-cloudcode-pa.sandbox.googleapis.com/v1internal:retrieveUserQuotaSummary",
	"https://cloudcode-pa.googleapis.com/v1internal:retrieveUserQuotaSummary",
}

type Options struct {
	Owner                  string
	HomeID                 string
	GlobalProxyURL         string
	GlobalProxyURLProvider func() string
	PollInterval           time.Duration
	ProbeTimeout           time.Duration
	LeaseDuration          time.Duration
	SnapshotFreshness      time.Duration
	ProviderConcurrency    int
	CodexUsageURL          string
	CodexResetCreditsURL   string
	ClaudeUsageURL         string
	ClaudeProfileURL       string
	KimiUsageURL           string
	XAIBillingURL          string
	AntigravityURLs        []string
	Now                    func() time.Time
	HTTPClient             func(*coreauth.Auth, time.Duration) (*http.Client, error)
	ResolveAuth            func(context.Context, *coreauth.Auth) (*coreauth.Auth, error)
}

type Collector struct {
	repo          *cluster.Repository
	options       Options
	globalSem     chan struct{}
	providerSemMu sync.Mutex
	providerSem   map[string]chan struct{}
	onDemandMu    sync.Mutex
	onDemandJobs  map[string]struct{}
	background    sync.WaitGroup
}

func NewCollector(repo *cluster.Repository, options Options) *Collector {
	if repo == nil {
		return nil
	}
	if strings.TrimSpace(options.Owner) == "" {
		hostname, _ := os.Hostname()
		options.Owner = fmt.Sprintf("%s-%d", strings.TrimSpace(hostname), os.Getpid())
	}
	if options.PollInterval <= 0 {
		options.PollInterval = defaultPollInterval
	}
	if options.ProbeTimeout <= 0 {
		options.ProbeTimeout = defaultProbeTimeout
	}
	if options.LeaseDuration <= 0 {
		options.LeaseDuration = defaultProbeLeaseDuration
	}
	if options.SnapshotFreshness <= 0 {
		options.SnapshotFreshness = defaultSnapshotFreshness
	}
	if repo.DialectName() == "sqlite" {
		options.ProviderConcurrency = 1
	} else if options.ProviderConcurrency <= 0 {
		options.ProviderConcurrency = defaultProviderConcurrency
	}
	if strings.TrimSpace(options.CodexUsageURL) == "" {
		options.CodexUsageURL = codexUsageURL
	}
	if strings.TrimSpace(options.CodexResetCreditsURL) == "" {
		options.CodexResetCreditsURL = codexResetCreditsURL
	}
	if strings.TrimSpace(options.ClaudeUsageURL) == "" {
		options.ClaudeUsageURL = claudeUsageURL
	}
	if strings.TrimSpace(options.ClaudeProfileURL) == "" {
		options.ClaudeProfileURL = claudeProfileURL
	}
	if strings.TrimSpace(options.KimiUsageURL) == "" {
		options.KimiUsageURL = kimiUsageURL
	}
	if strings.TrimSpace(options.XAIBillingURL) == "" {
		options.XAIBillingURL = xaiBillingURL
	}
	if len(options.AntigravityURLs) == 0 {
		options.AntigravityURLs = append([]string(nil), defaultAntigravityURLs...)
	}
	if options.Now == nil {
		options.Now = func() time.Time { return time.Now().UTC() }
	}
	if options.HTTPClient == nil {
		options.HTTPClient = func(auth *coreauth.Auth, timeout time.Duration) (*http.Client, error) {
			globalProxyURL := strings.TrimSpace(options.GlobalProxyURL)
			if options.GlobalProxyURLProvider != nil {
				globalProxyURL = strings.TrimSpace(options.GlobalProxyURLProvider())
			}
			return quotaHTTPClient(auth, globalProxyURL, timeout)
		}
	}
	collector := &Collector{
		repo:         repo,
		options:      options,
		providerSem:  make(map[string]chan struct{}),
		onDemandJobs: make(map[string]struct{}),
	}
	if repo.DialectName() == "sqlite" {
		collector.globalSem = make(chan struct{}, options.ProviderConcurrency)
	}
	return collector
}

func (c *Collector) Start(ctx context.Context) {
	if c == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	go c.run(ctx)
	log.WithFields(log.Fields{"interval": c.options.PollInterval, "concurrency": c.options.ProviderConcurrency}).Info("quota collector started")
}

func (c *Collector) run(ctx context.Context) {
	c.collect(ctx)
	ticker := time.NewTicker(c.options.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.collect(ctx)
		}
	}
}

func (c *Collector) collect(ctx context.Context) {
	auths, errAuths := c.repo.ListAuths(ctx)
	if errAuths != nil {
		log.WithError(errAuths).Warn("quota collector: list credentials failed")
		return
	}
	c.collectCredentials(ctx, auths, false)
}

// TriggerCollection queues an asynchronous forced collection round for the
// matching credentials and returns how many new local jobs were accepted.
// Empty filters accept every eligible credential. The round runs on a detached
// context so it outlives the caller (for example an HTTP request).
func (c *Collector) TriggerCollection(ctx context.Context, credentialIDs map[string]struct{}, providers map[string]struct{}) (int, error) {
	if c == nil || c.repo == nil {
		return 0, fmt.Errorf("quota collector is unavailable")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	auths, errAuths := c.repo.ListAuths(ctx)
	if errAuths != nil {
		return 0, errAuths
	}
	accepted := make([]*coreauth.Auth, 0, len(auths))
	now := c.options.Now().UTC()
	c.onDemandMu.Lock()
	for _, auth := range auths {
		if !quotaProbeEligible(auth, now) {
			continue
		}
		if len(credentialIDs) > 0 {
			if _, ok := credentialIDs[strings.TrimSpace(auth.ID)]; !ok {
				continue
			}
		}
		if len(providers) > 0 {
			if _, ok := providers[normalizedQuotaProvider(auth.Provider)]; !ok {
				continue
			}
		}
		credentialID := strings.TrimSpace(auth.ID)
		if _, exists := c.onDemandJobs[credentialID]; exists {
			continue
		}
		c.onDemandJobs[credentialID] = struct{}{}
		accepted = append(accepted, auth)
	}
	c.onDemandMu.Unlock()
	if len(accepted) > 0 {
		c.background.Add(1)
		go func() {
			defer c.background.Done()
			c.collectCredentials(context.WithoutCancel(ctx), accepted, true)
		}()
	}
	return len(accepted), nil
}

// Wait waits for asynchronous on-demand collection rounds to finish. It is
// primarily useful for deterministic tests.
func (c *Collector) Wait() {
	if c == nil {
		return
	}
	c.background.Wait()
}

func (c *Collector) collectCredentials(ctx context.Context, auths []*coreauth.Auth, force bool) {
	var wait sync.WaitGroup
	for _, auth := range auths {
		if !quotaProbeEligible(auth, c.options.Now().UTC()) {
			if force {
				c.releaseOnDemandJob(auth)
			}
			continue
		}
		sem := c.quotaSemaphore(auth.Provider)
		wait.Add(1)
		go func(candidate *coreauth.Auth, release chan struct{}) {
			defer wait.Done()
			if force {
				defer c.releaseOnDemandJob(candidate)
			}
			select {
			case release <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-release }()
			c.collectCredential(ctx, candidate, force)
		}(auth, sem)
	}
	wait.Wait()
}

func (c *Collector) quotaSemaphore(provider string) chan struct{} {
	if c.globalSem != nil {
		return c.globalSem
	}
	provider = normalizedQuotaProvider(provider)
	c.providerSemMu.Lock()
	defer c.providerSemMu.Unlock()
	sem := c.providerSem[provider]
	if sem == nil {
		sem = make(chan struct{}, c.options.ProviderConcurrency)
		c.providerSem[provider] = sem
	}
	return sem
}

func (c *Collector) releaseOnDemandJob(auth *coreauth.Auth) {
	if c == nil || auth == nil {
		return
	}
	c.onDemandMu.Lock()
	delete(c.onDemandJobs, strings.TrimSpace(auth.ID))
	c.onDemandMu.Unlock()
}

func (c *Collector) collectCredential(ctx context.Context, auth *coreauth.Auth, force bool) {
	now := c.options.Now().UTC()
	var claimed bool
	var errClaim error
	if force {
		claimed, errClaim = c.repo.ForceClaimEligibleQuotaProbe(ctx, auth.ID, c.options.Owner, now, c.options.LeaseDuration)
	} else {
		claimed, errClaim = c.repo.ClaimEligibleQuotaProbe(ctx, auth.ID, c.options.Owner, now, c.options.LeaseDuration)
	}
	if errClaim != nil {
		log.WithError(errClaim).WithField("credential_id", auth.ID).Warn("quota collector: claim failed")
		return
	}
	if !claimed {
		return
	}
	if c.options.ResolveAuth != nil {
		resolveCtx, cancelResolve := context.WithTimeout(ctx, c.options.ProbeTimeout)
		resolved, errResolve := c.options.ResolveAuth(resolveCtx, auth)
		cancelResolve()
		if errResolve != nil || resolved == nil {
			c.failProbe(ctx, auth.ID, &probeError{code: "AUTH_REFRESH_FAILED", message: "Credential could not be refreshed for quota collection.", retryable: true})
			return
		}
		auth = resolved
	}
	result, errProbe := c.probeCredential(ctx, auth)
	if errProbe != nil {
		c.failProbe(ctx, auth.ID, errProbe)
		return
	}
	observedAt := c.options.Now().UTC()
	expiresAt := observedAt.Add(c.options.SnapshotFreshness)
	status := quotaWindowAggregateStatus(result.windows)
	collectionStatus := "success"
	if result.partial {
		collectionStatus = "partial"
	}
	snapshotVersion := cluster.QuotaSnapshotVersion(auth.Provider)
	input := cluster.QuotaSnapshotWrite{
		CredentialID: auth.ID, QuotaStatus: status, CollectionStatus: collectionStatus, Source: "active_probe",
		ObservedAt: &observedAt, MaxAcceptedObservedAt: &observedAt, ExpiresAt: &expiresAt, LastAttemptAt: &observedAt, LastSuccessAt: &observedAt,
		NextProbeAt: &expiresAt, ConsecutiveFailure: 0, Error: result.collectionError, Plan: result.plan, ReplacePlan: true, ParserVersion: snapshotVersion, CollectorVersion: snapshotVersion,
		ResetCredits: result.resetCredits, ReplaceResetCredits: result.replaceResetCredits,
		ExpectedProbeOwner: c.options.Owner, ClearProbeLease: true, ReplaceWindows: result.replaceWindows, Windows: result.windows,
	}
	if strings.TrimSpace(c.options.HomeID) != "" {
		input.Runtime = &cluster.QuotaRuntime{HomeID: c.options.HomeID, HomeLabel: c.options.HomeID}
	}
	updated, errUpsert := c.repo.UpsertQuotaSnapshot(ctx, input)
	if errUpsert != nil {
		log.WithError(errUpsert).WithField("credential_id", auth.ID).Warn("quota collector: persist snapshot failed")
		c.failProbe(ctx, auth.ID, &probeError{code: "SNAPSHOT_PERSIST_FAILED", message: "Quota snapshot could not be persisted.", retryable: true})
		return
	}
	if !updated {
		log.WithField("credential_id", auth.ID).Debug("quota collector: probe completion ignored after lease loss")
		return
	}
	if updated {
		log.WithFields(log.Fields{
			"credential_id": auth.ID,
			"provider":      normalizedQuotaProvider(auth.Provider),
			"window_count":  len(result.windows),
			"partial":       result.partial,
			"observed_at":   observedAt,
		}).Debug("quota collector: snapshot persisted")
	}
}

type probeResult struct {
	windows             []cluster.QuotaWindow
	plan                *cluster.QuotaPlan
	resetCredits        *cluster.QuotaResetCredits
	partial             bool
	replaceWindows      bool
	replaceResetCredits bool
	collectionError     *cluster.QuotaCollectionError
}

func (c *Collector) probeCredential(ctx context.Context, auth *coreauth.Auth) (probeResult, *probeError) {
	switch normalizedQuotaProvider(auth.Provider) {
	case "codex":
		return c.probeCodex(ctx, auth)
	case "claude":
		return c.probeClaude(ctx, auth)
	case "antigravity":
		windows, errProbe := c.probeAntigravity(ctx, auth)
		return probeResult{windows: windows, replaceWindows: true}, errProbe
	case "kimi":
		windows, errProbe := c.probeKimi(ctx, auth)
		return probeResult{windows: windows, replaceWindows: true}, errProbe
	case "xai":
		windows, plan, errProbe := c.probeXAI(ctx, auth)
		return probeResult{windows: windows, plan: plan, replaceWindows: true}, errProbe
	default:
		return probeResult{}, &probeError{code: "PROVIDER_UNSUPPORTED", message: "Credential provider does not have a quota collector.", retryable: false}
	}
}

func (c *Collector) probeCodex(ctx context.Context, auth *coreauth.Auth) (probeResult, *probeError) {
	headers := http.Header{"Content-Type": []string{"application/json"}, "User-Agent": []string{codexUserAgent}}
	if accountID := quotaMetadataString(auth.Metadata, "account_id", "accountId", "chatgpt_account_id", "chatgptAccountId"); accountID != "" {
		headers.Set("Chatgpt-Account-Id", accountID)
	}
	body, _, errRequest := c.probeRequest(ctx, auth, http.MethodGet, c.options.CodexUsageURL, nil, headers)
	if errRequest != nil {
		return probeResult{}, errRequest
	}
	observedAt := c.options.Now().UTC()
	usage, errParse := parseCodexUsage(body, observedAt)
	if errParse != nil || len(usage.windows) == 0 {
		return probeResult{}, &probeError{code: "UPSTREAM_RESPONSE_INVALID", message: "Upstream quota response did not contain usable windows.", retryable: true}
	}
	if usage.plan == nil {
		usage.plan = codexPlanFromAuth(auth)
	}
	result := probeResult{
		windows:             usage.windows,
		plan:                usage.plan,
		resetCredits:        codexResetCreditsCountFallback(usage.resetCreditsAvailableCount, observedAt),
		replaceWindows:      true,
		replaceResetCredits: true,
	}
	resetHeaders := headers.Clone()
	resetHeaders.Set("Accept", "application/json")
	resetBody, _, errResetRequest := c.probeRequest(ctx, auth, http.MethodGet, c.options.CodexResetCreditsURL, nil, resetHeaders)
	if errResetRequest != nil {
		if codexResetCreditDetailsRequired(usage.resetCreditsAvailableCount) {
			result.partial = true
			result.collectionError = probeCollectionError(errResetRequest, observedAt)
		}
		return result, nil
	}
	resetCredits, errResetParse := parseCodexResetCredits(resetBody, observedAt, usage.resetCreditsAvailableCount)
	if errResetParse != nil {
		if codexResetCreditDetailsRequired(usage.resetCreditsAvailableCount) {
			resetError := &probeError{code: "RESET_CREDITS_RESPONSE_INVALID", message: "Codex reset credits response could not be parsed.", retryable: true}
			result.partial = true
			result.collectionError = probeCollectionError(resetError, observedAt)
		}
		return result, nil
	}
	result.resetCredits = resetCredits
	result.replaceResetCredits = true
	return result, nil
}

func codexPlanFromAuth(auth *coreauth.Auth) *cluster.QuotaPlan {
	if auth == nil {
		return nil
	}
	if auth.Attributes != nil {
		if plan := codexPlan(auth.Attributes["plan_type"]); plan != nil {
			return plan
		}
	}
	return codexPlan(quotaMetadataString(auth.Metadata, "plan_type", "planType"))
}

func codexResetCreditsCountFallback(availableCount *int, observedAt time.Time) *cluster.QuotaResetCredits {
	if availableCount == nil {
		return nil
	}
	count := *availableCount
	return &cluster.QuotaResetCredits{AvailableCount: &count, ObservedAt: observedAt.UTC(), Credits: []cluster.QuotaResetCredit{}}
}

func codexResetCreditDetailsRequired(availableCount *int) bool {
	return availableCount != nil && *availableCount > 0
}

func (c *Collector) probeRequest(ctx context.Context, auth *coreauth.Auth, method string, targetURL string, body []byte, headers http.Header) ([]byte, http.Header, *probeError) {
	token := quotaAccessToken(auth)
	if token == "" {
		return nil, nil, &probeError{code: "AUTH_TOKEN_UNAVAILABLE", message: "Credential access token is unavailable.", retryable: true}
	}
	requestCtx, cancel := context.WithTimeout(ctx, c.options.ProbeTimeout)
	defer cancel()
	req, errRequest := http.NewRequestWithContext(requestCtx, method, targetURL, bytes.NewReader(body))
	if errRequest != nil {
		return nil, nil, &probeError{code: "PROBE_REQUEST_INVALID", message: "Quota probe request could not be created.", retryable: false}
	}
	for key, values := range headers {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}
	req.Header.Set("Authorization", "Bearer "+token)
	client, errClient := c.options.HTTPClient(auth, c.options.ProbeTimeout)
	if errClient != nil {
		return nil, nil, &probeError{code: "PROBE_TRANSPORT_INVALID", message: "Credential proxy configuration is invalid.", retryable: false}
	}
	resp, errDo := client.Do(req)
	if errDo != nil {
		return nil, nil, &probeError{code: "UPSTREAM_UNAVAILABLE", message: "Upstream quota endpoint is unavailable.", retryable: true}
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.WithError(errClose).Debug("quota collector: close response body failed")
		}
	}()
	payload, errRead := io.ReadAll(io.LimitReader(resp.Body, maxProbeResponseBytes+1))
	requestID := quotaResponseRequestID(resp.Header)
	if errRead != nil || len(payload) > maxProbeResponseBytes {
		return nil, resp.Header, &probeError{code: "UPSTREAM_RESPONSE_INVALID", message: "Upstream quota response could not be read safely.", retryable: true, statusCode: resp.StatusCode, requestID: requestID}
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		failure := quotaHTTPProbeError(resp.StatusCode, requestID)
		failure.retryAfter = quotaRetryAfter(resp.Header, c.options.Now().UTC())
		return nil, resp.Header, failure
	}
	return payload, resp.Header, nil
}

func (c *Collector) failProbe(ctx context.Context, credentialID string, failure *probeError) {
	if failure == nil {
		return
	}
	now := c.options.Now().UTC()
	delay := defaultFailureBackoff
	if !failure.retryable {
		delay = maxFailureBackoff
	} else if item, errGet := c.repo.GetQuotaCredential(ctx, credentialID, now); errGet == nil && item.ConsecutiveFailure > 0 {
		for index := 0; index < item.ConsecutiveFailure && delay < maxFailureBackoff; index++ {
			delay *= 2
		}
		if delay > maxFailureBackoff {
			delay = maxFailureBackoff
		}
	}
	if failure.retryAfter > delay {
		delay = failure.retryAfter
	}
	delay = quotaBackoffWithJitter(delay, credentialID)
	nextProbeAt := now.Add(delay)
	occurredAt := now
	collectionError := cluster.QuotaCollectionError{Code: failure.code, Message: failure.message, Retryable: failure.retryable, OccurredAt: &occurredAt}
	if failure.statusCode > 0 {
		collectionError.UpstreamStatusCode = &failure.statusCode
	}
	if failure.requestID != "" {
		collectionError.RequestID = &failure.requestID
	}
	if errFail := c.repo.FailQuotaProbeAt(ctx, credentialID, c.options.Owner, collectionError, nextProbeAt, now); errFail != nil {
		if errors.Is(errFail, cluster.ErrQuotaProbeLeaseLost) {
			log.WithField("credential_id", credentialID).Debug("quota collector: failure ignored after lease loss")
			return
		}
		log.WithError(errFail).WithField("credential_id", credentialID).Warn("quota collector: persist failure failed")
		return
	}
	log.WithFields(log.Fields{
		"credential_id": credentialID,
		"error_code":    failure.code,
		"retryable":     failure.retryable,
		"status_code":   failure.statusCode,
		"next_probe_at": nextProbeAt,
	}).Debug("quota collector: failure persisted")
}

func quotaBackoffWithJitter(delay time.Duration, credentialID string) time.Duration {
	if delay <= 0 {
		return delay
	}
	hash := fnv.New32a()
	_, _ = hash.Write([]byte(strings.TrimSpace(credentialID)))
	percent := time.Duration(hash.Sum32() % 21)
	return delay + delay*percent/100
}

func quotaProbeEligible(auth *coreauth.Auth, now time.Time) bool {
	if auth == nil || strings.TrimSpace(auth.ID) == "" || auth.Disabled || auth.Status == coreauth.StatusDisabled {
		return false
	}
	if !auth.NextRetryAfter.IsZero() && auth.NextRetryAfter.After(now) {
		return false
	}
	if quotaProviderAPIKeyAuth(auth) {
		return false
	}
	switch normalizedQuotaProvider(auth.Provider) {
	case "codex", "claude", "antigravity", "kimi", "xai":
		return true
	default:
		return false
	}
}

func quotaProviderAPIKeyAuth(auth *coreauth.Auth) bool {
	if auth == nil {
		return false
	}
	switch quotaExplicitAuthKind(auth) {
	case "provider_api_key":
		return true
	case "oauth":
		return false
	}
	if auth.Attributes == nil {
		return false
	}
	if strings.HasPrefix(strings.TrimSpace(auth.Attributes["source"]), "config:") {
		return true
	}
	return strings.TrimSpace(auth.Attributes["api_key"]) != "" && quotaMetadataString(auth.Metadata, "type") == ""
}

func quotaExplicitAuthKind(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	value := ""
	if auth.Attributes != nil {
		value = strings.TrimSpace(auth.Attributes["auth_kind"])
	}
	if value == "" {
		value = quotaMetadataString(auth.Metadata, "auth_kind")
	}
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.ReplaceAll(value, "-", "_")
	switch value {
	case "apikey", "api_key", "provider_api_key":
		return "provider_api_key"
	case "oauth", "oauth2":
		return "oauth"
	default:
		return ""
	}
}

func normalizedQuotaProvider(provider string) string {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "anthropic", "claude":
		return "claude"
	case "antigravity":
		return "antigravity"
	case "codex":
		return "codex"
	case "kimi":
		return "kimi"
	case "xai", "grok":
		return "xai"
	default:
		return strings.ToLower(strings.TrimSpace(provider))
	}
}

func quotaHTTPClient(auth *coreauth.Auth, globalProxyURL string, timeout time.Duration) (*http.Client, error) {
	client := &http.Client{Timeout: timeout}
	proxyURL := strings.TrimSpace(globalProxyURL)
	if auth != nil && strings.TrimSpace(auth.ProxyURL) != "" {
		proxyURL = strings.TrimSpace(auth.ProxyURL)
	}
	if proxyURL == "" {
		client.Transport = proxyutil.NewDirectTransport()
		return client, nil
	}
	transport, _, errTransport := proxyutil.BuildHTTPTransport(proxyURL)
	if errTransport != nil {
		return nil, errTransport
	}
	if transport != nil {
		client.Transport = transport
	}
	return client, nil
}

func quotaAccessToken(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	if value := quotaMetadataString(auth.Metadata, "access_token", "accessToken"); value != "" {
		return value
	}
	for _, key := range []string{"token", "Token"} {
		switch token := auth.Metadata[key].(type) {
		case map[string]any:
			if value := quotaMetadataString(token, "access_token", "accessToken"); value != "" {
				return value
			}
		case map[string]string:
			for _, candidate := range []string{"access_token", "accessToken"} {
				if value := strings.TrimSpace(token[candidate]); value != "" {
					return value
				}
			}
		}
	}
	return ""
}

func quotaMetadataString(metadata map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := metadata[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func quotaResponseRequestID(headers http.Header) string {
	for _, key := range []string{"X-Request-ID", "Request-ID", "OpenAI-Request-ID"} {
		if value := cluster.SafeQuotaRequestID(headers.Get(key)); value != "" {
			return value
		}
	}
	return ""
}

type probeError struct {
	code       string
	message    string
	retryable  bool
	statusCode int
	requestID  string
	retryAfter time.Duration
}

func quotaRetryAfter(headers http.Header, now time.Time) time.Duration {
	value := strings.TrimSpace(headers.Get("Retry-After"))
	if value == "" {
		return 0
	}
	if seconds, errSeconds := strconv.ParseInt(value, 10, 64); errSeconds == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	if retryAt, errTime := http.ParseTime(value); errTime == nil && retryAt.After(now) {
		return retryAt.Sub(now)
	}
	return 0
}

func quotaHTTPProbeError(status int, requestID string) *probeError {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return &probeError{code: "UPSTREAM_AUTH_REJECTED", message: "Upstream rejected the credential.", retryable: false, statusCode: status, requestID: requestID}
	case http.StatusTooManyRequests:
		return &probeError{code: "UPSTREAM_RATE_LIMITED", message: "Upstream quota endpoint rate limited the probe.", retryable: true, statusCode: status, requestID: requestID}
	default:
		return &probeError{code: "UPSTREAM_UNAVAILABLE", message: "Upstream quota endpoint returned an unavailable response.", retryable: status >= 500, statusCode: status, requestID: requestID}
	}
}

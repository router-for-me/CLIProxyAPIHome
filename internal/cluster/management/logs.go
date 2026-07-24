package management

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
)

const (
	defaultAppLogLimit = 100
	maxAppLogLimit     = 1000
	homeLogDirectory   = "logs"
)

var requestLogForwardRequestHeaderAllowlist = map[string]struct{}{
	"range":               {},
	"if-range":            {},
	"if-match":            {},
	"if-none-match":       {},
	"if-modified-since":   {},
	"if-unmodified-since": {},
}

var requestLogForwardResponseHeaderAllowlist = map[string]struct{}{
	"content-disposition": {},
	"content-type":        {},
	"content-length":      {},
	"last-modified":       {},
	"etag":                {},
	"accept-ranges":       {},
	"content-range":       {},
}

// GetLogs returns app log records stored in the database.
func (h *Handler) GetLogs(c *gin.Context) {
	limit, errLimit := appLogLimit(c.Query("limit"))
	if errLimit != nil {
		respondError(c, http.StatusBadRequest, "invalid_limit", errLimit)
		return
	}
	offset, errOffset := appLogOffset(c.Query("offset"))
	if errOffset != nil {
		respondError(c, http.StatusBadRequest, "invalid_offset", errOffset)
		return
	}
	after, errAfter := appLogTimeQuery(c.Query("after"))
	if errAfter != nil {
		respondError(c, http.StatusBadRequest, "invalid_after", errAfter)
		return
	}
	before, errBefore := appLogTimeQuery(c.Query("before"))
	if errBefore != nil {
		respondError(c, http.StatusBadRequest, "invalid_before", errBefore)
		return
	}

	ctx, cancel := h.requestContext(c)
	defer cancel()

	result, errLogs := h.repo.ListAppLogs(ctx, cluster.AppLogQuery{
		HomeIP:    firstNonEmptyQuery(c, "home_ip", "home-ip"),
		ClientIP:  firstNonEmptyQuery(c, "client_ip", "client-ip"),
		RequestID: firstNonEmptyQuery(c, "request_id", "request-id"),
		Level:     strings.TrimSpace(c.Query("level")),
		After:     after,
		Before:    before,
		Limit:     limit,
		Offset:    offset,
	})
	if errLogs != nil {
		respondError(c, http.StatusInternalServerError, "log_load_failed", errLogs)
		return
	}

	items := make([]gin.H, 0, len(result.Records))
	for _, record := range result.Records {
		items = append(items, appLogRecordToMap(&record))
	}
	c.JSON(http.StatusOK, gin.H{
		"logs":   items,
		"total":  result.Total,
		"limit":  limit,
		"offset": offset,
	})
}

// DeleteLogs deletes all app log records stored in the shared database.
func (h *Handler) DeleteLogs(c *gin.Context) {
	if h == nil || h.repo == nil {
		respondError(c, http.StatusInternalServerError, "repository_unavailable", nil)
		return
	}

	ctx, cancel := h.requestContext(c)
	defer cancel()

	removed, errDelete := h.repo.DeleteAppLogs(ctx)
	if errDelete != nil {
		respondError(c, http.StatusInternalServerError, "logs_clear_failed", errDelete)
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "Logs cleared successfully",
		"removed": removed,
	})
}

// DownloadRequestLogByID downloads a request log file by path request ID and optional query Home identity.
func (h *Handler) DownloadRequestLogByID(c *gin.Context) {
	requestID := strings.TrimSpace(c.Param("id"))
	if requestID == "" {
		requestID = firstNonEmptyQuery(c, "request_id", "request-id", "id")
	}
	homePort, ok := requestLogHomePortQuery(c)
	if !ok {
		return
	}
	h.downloadRequestLog(c, firstNonEmptyQuery(c, "home_ip", "home-ip"), homePort, requestID)
}

// DownloadLocalRequestLogByID downloads a request log file from this Home only.
func (h *Handler) DownloadLocalRequestLogByID(c *gin.Context) {
	requestID := strings.TrimSpace(c.Param("id"))
	if requestID == "" {
		requestID = firstNonEmptyQuery(c, "request_id", "request-id", "id")
	}
	homePort, ok := requestLogHomePortQuery(c)
	if !ok {
		return
	}
	h.downloadLocalRequestLog(c, firstNonEmptyQuery(c, "home_ip", "home-ip"), homePort, requestID)
}

func (h *Handler) downloadRequestLog(c *gin.Context, homeIP string, homePort int, requestID string) {
	homeIP = strings.TrimSpace(homeIP)
	requestID = strings.TrimSpace(requestID)
	if !validateRequestLogParams(c, homeIP, requestID) {
		return
	}
	if h.requestLogTargetIsRemote(homeIP, homePort) {
		h.forwardRequestLogByID(c, homeIP, homePort, requestID)
		return
	}

	h.downloadLocalRequestLog(c, homeIP, homePort, requestID)
}

func (h *Handler) downloadLocalRequestLog(c *gin.Context, homeIP string, homePort int, requestID string) {
	homeIP = strings.TrimSpace(homeIP)
	requestID = strings.TrimSpace(requestID)
	if !validateRequestLogParams(c, homeIP, requestID) {
		return
	}
	if h.requestLogTargetIsRemote(homeIP, homePort) {
		respondError(c, http.StatusNotFound, "not_found", fmt.Errorf("home log file is not local to this node"))
		return
	}

	name, path, errFind := findRequestLogFile(homeLogDirectory, requestID)
	if errFind != nil {
		if errors.Is(errFind, os.ErrNotExist) {
			respondError(c, http.StatusNotFound, "not_found", fmt.Errorf("request log file not found"))
			return
		}
		respondError(c, http.StatusInternalServerError, "request_log_load_failed", errFind)
		return
	}

	c.FileAttachment(path, name)
}

func requestLogHomePortQuery(c *gin.Context) (int, bool) {
	raw := firstNonEmptyQuery(c, "home_port", "home-port")
	if raw == "" {
		return 0, true
	}
	parsed, errParse := strconv.Atoi(raw)
	if errParse != nil || parsed <= 0 {
		respondError(c, http.StatusBadRequest, "invalid_home_port", fmt.Errorf("home_port must be a positive integer"))
		return 0, false
	}
	return parsed, true
}

func (h *Handler) requestLogTargetIsRemote(homeIP string, homePort int) bool {
	homeIP = strings.TrimSpace(homeIP)
	if homeIP != "" && h.nodeIP != "" && homeIP != h.nodeIP {
		return true
	}
	if homePort > 0 && h.nodePort > 0 && homePort != h.nodePort {
		return true
	}
	return false
}

func validateRequestLogParams(c *gin.Context, homeIP string, requestID string) bool {
	if requestID == "" {
		respondError(c, http.StatusBadRequest, "missing_request_id", fmt.Errorf("request_id is required"))
		return false
	}
	if strings.ContainsAny(requestID, `/\`) {
		respondError(c, http.StatusBadRequest, "invalid_request_id", fmt.Errorf("request_id contains a path separator"))
		return false
	}
	return true
}

func (h *Handler) forwardRequestLogByID(c *gin.Context, homeIP string, homePort int, requestID string) {
	if h == nil || h.repo == nil {
		respondError(c, http.StatusServiceUnavailable, "cluster_unavailable", fmt.Errorf("cluster repository is unavailable"))
		return
	}
	if h.forwardTLSConfig == nil {
		respondError(c, http.StatusBadGateway, "request_log_forward_failed", fmt.Errorf("cluster forwarding TLS is unavailable"))
		return
	}

	ctx, cancel := h.requestContext(c)
	defer cancel()

	target, errTarget := h.requestLogTargetNode(ctx, homeIP, homePort)
	if errTarget != nil {
		respondError(c, http.StatusInternalServerError, "node_lookup_failed", errTarget)
		return
	}
	if target == nil {
		respondError(c, http.StatusNotFound, "not_found", fmt.Errorf("home node not found"))
		return
	}

	tlsConfig, errTLS := cluster.HTTPClientTLSConfig(h.forwardTLSConfig, target.IP)
	if errTLS != nil {
		respondError(c, http.StatusBadGateway, "request_log_forward_failed", errTLS)
		return
	}

	targetURL := url.URL{
		Scheme: "https",
		Host:   net.JoinHostPort(target.IP, strconv.Itoa(target.Port)),
		Path:   "/v0/cluster/request-log-by-id/" + requestID,
	}
	query := targetURL.Query()
	query.Set("home_ip", homeIP)
	if homePort > 0 {
		query.Set("home_port", strconv.Itoa(homePort))
	}
	targetURL.RawQuery = query.Encode()

	req, errRequest := http.NewRequestWithContext(ctx, http.MethodGet, targetURL.String(), nil)
	if errRequest != nil {
		respondError(c, http.StatusInternalServerError, "request_log_forward_failed", errRequest)
		return
	}
	copyRequestLogForwardRequestHeaders(req.Header, c.Request.Header)

	client := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: tlsConfig,
		},
	}
	resp, errDo := client.Do(req)
	if errDo != nil {
		respondError(c, http.StatusBadGateway, "request_log_forward_failed", errDo)
		return
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	copyRequestLogForwardResponseHeaders(c.Writer.Header(), resp.Header)
	c.Status(resp.StatusCode)
	if _, errCopy := io.Copy(c.Writer, resp.Body); errCopy != nil {
		return
	}
}

func (h *Handler) requestLogTargetNode(ctx context.Context, homeIP string, homePort int) (*cluster.ClusterNodeRecord, error) {
	homeIP = strings.TrimSpace(homeIP)
	if homeIP == "" {
		return nil, nil
	}
	nodes, errNodes := h.repo.ListLiveClusterNodes(ctx, time.Time{})
	if errNodes != nil {
		return nil, errNodes
	}
	for i := range nodes {
		if strings.TrimSpace(nodes[i].IP) != homeIP || nodes[i].Port <= 0 {
			continue
		}
		if homePort <= 0 || nodes[i].Port == homePort {
			return &nodes[i], nil
		}
	}
	return nil, nil
}

func copyRequestLogForwardRequestHeaders(dst http.Header, src http.Header) {
	for key, values := range src {
		if !shouldForwardRequestLogRequestHeader(key) {
			continue
		}
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

func shouldForwardRequestLogRequestHeader(key string) bool {
	_, ok := requestLogForwardRequestHeaderAllowlist[strings.ToLower(strings.TrimSpace(key))]
	return ok
}

func copyRequestLogForwardResponseHeaders(dst http.Header, src http.Header) {
	for key, values := range src {
		if !shouldForwardRequestLogResponseHeader(key) {
			continue
		}
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

func shouldForwardRequestLogResponseHeader(key string) bool {
	_, ok := requestLogForwardResponseHeaderAllowlist[strings.ToLower(strings.TrimSpace(key))]
	return ok
}

func appLogRecordToMap(record *cluster.AppLogRecord) gin.H {
	if record == nil {
		return gin.H{}
	}
	return gin.H{
		"id":         record.ID,
		"timestamp":  record.Timestamp,
		"client_ip":  record.ClientIP,
		"request_id": record.RequestID,
		"home_ip":    record.HomeIP,
		"level":      record.Level,
		"line":       record.Line,
		"created_at": record.CreatedAt,
	}
}

func appLogLimit(raw string) (int, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return defaultAppLogLimit, nil
	}
	limit, errParse := strconv.Atoi(value)
	if errParse != nil || limit <= 0 {
		return 0, fmt.Errorf("limit must be a positive integer")
	}
	if limit > maxAppLogLimit {
		return maxAppLogLimit, nil
	}
	return limit, nil
}

func appLogOffset(raw string) (int, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return 0, nil
	}
	offset, errParse := strconv.Atoi(value)
	if errParse != nil || offset < 0 {
		return 0, fmt.Errorf("offset must be a non-negative integer")
	}
	return offset, nil
}

func appLogTimeQuery(raw string) (*time.Time, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return nil, nil
	}
	if unix, errUnix := strconv.ParseInt(value, 10, 64); errUnix == nil {
		if unix <= 0 {
			return nil, fmt.Errorf("timestamp must be greater than zero")
		}
		parsed := time.Unix(unix, 0).UTC()
		return &parsed, nil
	}
	parsed, errParse := time.Parse(time.RFC3339Nano, value)
	if errParse != nil {
		return nil, fmt.Errorf("timestamp must be Unix seconds or RFC3339")
	}
	parsed = parsed.UTC()
	return &parsed, nil
}

type requestLogCandidate struct {
	name     string
	modified time.Time
}

func findRequestLogFile(dir string, requestID string) (string, string, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return "", "", fmt.Errorf("log directory is not configured")
	}

	entries, errRead := os.ReadDir(dir)
	if errRead != nil {
		return "", "", errRead
	}

	suffix := "-" + requestID + ".log"
	candidates := make([]requestLogCandidate, 0, 1)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(name, suffix) {
			continue
		}
		info, errInfo := entry.Info()
		if errInfo != nil {
			return "", "", errInfo
		}
		candidates = append(candidates, requestLogCandidate{name: name, modified: info.ModTime()})
	}
	if len(candidates) == 0 {
		return "", "", os.ErrNotExist
	}

	sort.Slice(candidates, func(i, j int) bool { return candidates[i].modified.After(candidates[j].modified) })
	name := candidates[0].name
	path, errPath := safeLogFilePath(dir, name)
	if errPath != nil {
		return "", "", errPath
	}
	return name, path, nil
}

func safeLogFilePath(dir string, name string) (string, error) {
	if name == "" || strings.ContainsAny(name, `/\`) {
		return "", fmt.Errorf("invalid log file name")
	}

	dirAbs, errAbs := filepath.Abs(dir)
	if errAbs != nil {
		return "", errAbs
	}
	fullPath := filepath.Clean(filepath.Join(dirAbs, name))
	rel, errRel := filepath.Rel(dirAbs, fullPath)
	if errRel != nil {
		return "", errRel
	}
	if rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("invalid log file path")
	}

	info, errStat := os.Stat(fullPath)
	if errStat != nil {
		return "", errStat
	}
	if info.IsDir() {
		return "", fmt.Errorf("invalid log file")
	}
	return fullPath, nil
}

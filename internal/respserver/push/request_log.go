package push

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPIHome/internal/respserver/dispatch"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

var requestLogID atomic.Uint64

// handleRequestLog handles a request log.
func handleRequestLog(ctx context.Context, env dispatch.Env, args []string) dispatch.Reply {
	_ = ctx

	if len(args) != 3 {
		return dispatch.Err("wrong number of arguments for 'rpush' command")
	}

	if !strings.EqualFold(strings.TrimSpace(args[1]), "request-log") {
		return dispatch.Err("unsupported key")
	}

	payload := strings.TrimSpace(args[2])
	if payload == "" || !gjson.Valid(payload) {
		return dispatch.Err("invalid request-log json")
	}

	requestLog := strings.TrimSpace(gjson.Get(payload, "request_log").String())
	if requestLog == "" {
		return dispatch.Err("missing request_log")
	}

	requestID := strings.TrimSpace(gjson.Get(payload, "request_id").String())
	headersObj := gjson.Get(payload, "headers")
	if requestID == "" {
		requestID = strings.TrimSpace(extractHeaderValue(headersObj, "x-request-id"))
	}
	if requestID == "" {
		requestID = strings.TrimSpace(extractHeaderValue(headersObj, "x-cpa-request-id"))
	}
	if requestID == "" {
		requestID = generateRequestLogID()
	}

	clientIP := strings.TrimSpace(env.ClientIP)
	if clientIP == "" {
		clientIP = "unknown"
	}

	url := extractRequestLogURL(requestLog)
	filename := buildRequestLogFilename(clientIP, url, requestID, time.Now())

	if errWrite := writeRequestLogFile(filename, requestLog); errWrite != nil {
		log.Errorf("request log write error: %v", errWrite)
		return dispatch.Err("request log write failed")
	}

	// Redis' RPUSH returns the length of the list after the push (integer reply).
	// This server doesn't maintain a real list, so always return 1 as a stable
	// integer reply and avoid client retries.
	return dispatch.Integer(1)
}

// generateRequestLogID generates a request log id.
func generateRequestLogID() string {
	b := make([]byte, 4)
	if _, errRead := rand.Read(b); errRead == nil {
		return hex.EncodeToString(b)
	}

	id := requestLogID.Add(1)
	return fmt.Sprintf("%d", id)
}

// extractHeaderValue extracts a header value.
func extractHeaderValue(headersObj gjson.Result, wantKeyLower string) string {
	// Decode the wire frame before dispatching command handling.
	if !headersObj.Exists() || !headersObj.IsObject() {
		return ""
	}

	var out string
	headersObj.ForEach(func(k, v gjson.Result) bool {
		key := strings.ToLower(strings.TrimSpace(k.String()))
		if key != wantKeyLower {
			return true
		}

		switch {
		case v.Type == gjson.String:
			out = v.String()
		case v.IsArray():
			v.ForEach(func(_, entry gjson.Result) bool {
				if strings.TrimSpace(out) != "" {
					return false
				}
				out = entry.String()
				return true
			})
		default:
			out = v.String()
		}
		return false
	})
	return out
}

// extractRequestLogURL extracts a request log url.
func extractRequestLogURL(requestLog string) string {
	requestLog = strings.ReplaceAll(requestLog, "\r\n", "\n")
	for _, line := range strings.Split(requestLog, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "URL:") {
			return strings.TrimSpace(strings.TrimPrefix(line, "URL:"))
		}
	}
	return ""
}

// buildRequestLogFilename builds a request log filename.
func buildRequestLogFilename(clientIP string, url string, requestID string, now time.Time) string {
	sanitizedURL := sanitizeForFilename(url)
	sanitizedIP := sanitizeForFilename(clientIP)
	timestamp := now.Format("2006-01-02T150405")

	if sanitizedIP == "" {
		sanitizedIP = "unknown"
	}
	if sanitizedURL == "" {
		sanitizedURL = "root"
	}
	if requestID == "" {
		requestID = "0"
	}

	return fmt.Sprintf("%s-%s-%s-%s.log", sanitizedIP, sanitizedURL, timestamp, requestID)
}

// sanitizeForFilename sanitizes a for filename.
func sanitizeForFilename(url string) string {
	// Decode the wire frame before dispatching command handling.
	path := strings.TrimSpace(url)
	if strings.Contains(path, "?") {
		path = strings.Split(path, "?")[0]
	}
	if strings.HasPrefix(path, "/") {
		path = path[1:]
	}

	sanitized := strings.ReplaceAll(path, "/", "-")
	sanitized = strings.ReplaceAll(sanitized, ":", "-")

	regUnsafe := regexp.MustCompile(`[<>:"|?*\s]`)
	sanitized = regUnsafe.ReplaceAllString(sanitized, "-")

	regHyphens := regexp.MustCompile(`-+`)
	sanitized = regHyphens.ReplaceAllString(sanitized, "-")

	sanitized = strings.Trim(sanitized, "-")
	if sanitized == "" {
		return "root"
	}
	return sanitized
}

// writeRequestLogFile writes a request log file.
func writeRequestLogFile(filename string, content string) error {
	// Validate request inputs before mutating persisted state.
	filename = strings.TrimSpace(filename)
	if filename == "" {
		return fmt.Errorf("empty filename")
	}

	logFilesMu.RLock()
	defer logFilesMu.RUnlock()

	logDir := "logs"
	if errMk := os.MkdirAll(logDir, 0o755); errMk != nil {
		return fmt.Errorf("ensure logs dir: %w", errMk)
	}

	filePath := filepath.Join(logDir, filename)
	f, errOpen := os.OpenFile(filePath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if errOpen != nil {
		return fmt.Errorf("open request log: %w", errOpen)
	}
	defer func() {
		if errClose := f.Close(); errClose != nil {
			log.Errorf("request log close error: %v", errClose)
		}
	}()

	data := content
	if !strings.HasSuffix(data, "\n") {
		data += "\n"
	}

	for len(data) > 0 {
		n, errWrite := f.WriteString(data)
		if errWrite != nil {
			return fmt.Errorf("write request log: %w", errWrite)
		}
		data = data[n:]
	}

	return nil
}

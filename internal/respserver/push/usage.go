package push

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginhost"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
	homeerrors "github.com/router-for-me/CLIProxyAPIHome/internal/errors"
	"github.com/router-for-me/CLIProxyAPIHome/internal/home"
	"github.com/router-for-me/CLIProxyAPIHome/internal/respserver/dispatch"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

type usageLogFileState struct {
	size    int64
	modTime time.Time
	exists  bool
}

var (
	usageLogMu              sync.Mutex
	usageLogSanitizedStates = make(map[string]usageLogFileState)
)

// usageIngestionRuntime is the persistence and plugin boundary used by LPUSH usage.
type usageIngestionRuntime interface {
	UsagePluginSink() home.PluginUsageSink
	DeliverPluginUsage(context.Context, string, home.PluginUsageSink, pluginapi.UsageRecord) error
	PersistClusterUsagePayload(context.Context, string, time.Time) (bool, bool, error)
	RecordUsagePayload(context.Context, string) error
	ApplyUsagePayloadFeedback(context.Context, string) error
}

// handleUsage handles an usage.
func handleUsage(ctx context.Context, env dispatch.Env, args []string) dispatch.Reply {
	if len(args) != 3 {
		return dispatch.Err("wrong number of arguments for 'lprush' command")
	}

	if !strings.EqualFold(strings.TrimSpace(args[1]), "usage") {
		return dispatch.Err("unsupported key")
	}
	receivedAt := time.Now().UTC()

	var runtime usageIngestionRuntime
	if env.Runtime != nil {
		runtime = env.Runtime
	}
	return ingestUsagePayload(ctx, env, runtime, strings.TrimSpace(args[2]), receivedAt)
}

func ingestUsagePayload(ctx context.Context, env dispatch.Env, runtime usageIngestionRuntime, payload string, receivedAt time.Time) dispatch.Reply {
	if payload == "" || !gjson.Valid(payload) {
		return dispatch.Err("invalid usage json")
	}
	var sink home.PluginUsageSink
	var responseHeaders http.Header
	if runtime != nil {
		sink = runtime.UsagePluginSink()
		if sink != nil {
			// Response headers belong to in-memory plugin feedback only. Database
			// sanitization below must still remove them from the persisted payload.
			var snapshot struct {
				ResponseHeaders http.Header `json:"response_headers"`
			}
			if errDecode := json.Unmarshal([]byte(payload), &snapshot); errDecode != nil {
				return dispatch.Err("invalid plugin usage json")
			}
			responseHeaders = snapshot.ResponseHeaders
		}
	}
	sanitizedPayload, errSanitize := cluster.SanitizeUsagePayloadSecrets(payload)
	if errSanitize != nil {
		return dispatch.Err("invalid usage json")
	}
	payload = sanitizedPayload
	enrichedPayload, errEnrich := usagePayloadWithCPAIdentity(payload, env)
	if errEnrich != nil {
		return dispatch.Err("invalid usage json")
	}
	payload = enrichedPayload

	if runtime != nil {
		var pluginUsage pluginapi.UsageRecord
		if sink != nil {
			var errDecode error
			pluginUsage, errDecode = home.DecodePluginUsagePayload(payload)
			if errDecode != nil {
				return dispatch.Err("invalid plugin usage json")
			}
			pluginUsage.ResponseHeaders = responseHeaders
		}
		persisted, inserted, errPersist := runtime.PersistClusterUsagePayload(ctx, payload, receivedAt)
		if errPersist != nil {
			// Nothing is acknowledged: CPA redelivers and the dedupe key resolves
			// whether the usage row already committed.
			log.Errorf("usage database write error: %v", errPersist)
			return dispatch.Err("usage database write failed")
		}
		// Apply scheduler feedback before plugin delivery: a blocked or failed
		// callback must not delay credential cooldown. Replays are fenced by
		// execution identity and event time inside MarkResult.
		var errFeedback error
		if persisted && !inserted {
			errFeedback = runtime.ApplyUsagePayloadFeedback(ctx, payload)
		} else {
			errFeedback = runtime.RecordUsagePayload(ctx, payload)
		}
		if errFeedback != nil {
			log.Errorf("usage feedback error: %v", errFeedback)
			return dispatch.Err("usage feedback failed")
		}
		// Insertion deduplication does not prove delivery: another request may
		// still be blocked in the plugin, or the commit result may have been lost.
		if errDeliver := runtime.DeliverPluginUsage(ctx, payload, sink, pluginUsage); errDeliver != nil {
			if errors.Is(errDeliver, homeerrors.ErrUsagePluginDeliveryPending) || errors.Is(errDeliver, pluginhost.ErrUsageDeliveryPending) {
				return dispatch.Err("usage plugin delivery pending")
			}
			// Plugin errors may contain plugin-owned payloads; do not echo them
			// into Home logs or the RESP response. The event remains retryable.
			log.Error("usage plugin delivery failed")
			return dispatch.Err("usage plugin delivery failed")
		}
		if persisted {
			return dispatch.Integer(1)
		}
	}

	if errAppend := appendUsageLog(payload); errAppend != nil {
		log.Errorf("usage log write error: %v", errAppend)
		return dispatch.Err("usage log write failed")
	}

	// Redis' LPUSH returns the length of the list after the push (integer reply).
	// CPA forwards usage via go-redis LPush which expects an integer, not "+OK".
	// This server doesn't maintain a real list, so always return 1 as a stable
	// integer reply and avoid client retries.
	return dispatch.Integer(1)
}

func usagePayloadWithCPAIdentity(payload string, env dispatch.Env) (string, error) {
	return cluster.UsagePayloadWithRuntimeMetadata(payload, cluster.UsageRuntimeMetadata{
		CPANodeID: strings.TrimSpace(env.NodeID),
		CPAIP:     strings.TrimSpace(env.ClientIP),
	})
}

// SanitizeUsageLog removes provider API key secrets from the fallback usage log.
func SanitizeUsageLog() error {
	logFilesMu.RLock()
	defer logFilesMu.RUnlock()

	usageLogMu.Lock()
	defer usageLogMu.Unlock()
	return sanitizeUsageLogPathLocked(filepath.Join("logs", "usage.log"))
}

// appendUsageLog appends an usage log.
func appendUsageLog(payload string) error {
	// Decode the wire frame before dispatching command handling.
	if strings.TrimSpace(payload) == "" {
		return fmt.Errorf("empty payload")
	}

	logFilesMu.RLock()
	defer logFilesMu.RUnlock()

	usageLogMu.Lock()
	defer usageLogMu.Unlock()

	logDir := "logs"
	if errMk := os.MkdirAll(logDir, 0o755); errMk != nil {
		return fmt.Errorf("ensure logs dir: %w", errMk)
	}

	filePath := filepath.Join(logDir, "usage.log")
	if errSanitize := sanitizeUsageLogPathLocked(filePath); errSanitize != nil {
		return fmt.Errorf("sanitize usage log: %w", errSanitize)
	}
	f, errOpen := os.OpenFile(filePath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if errOpen != nil {
		return fmt.Errorf("open usage log: %w", errOpen)
	}
	defer func() {
		if errClose := f.Close(); errClose != nil {
			log.Errorf("usage log close error: %v", errClose)
		}
	}()

	line := payload
	if !strings.HasSuffix(line, "\n") {
		line += "\n"
	}

	for len(line) > 0 {
		n, errWrite := f.WriteString(line)
		if errWrite != nil {
			return fmt.Errorf("write usage log: %w", errWrite)
		}
		line = line[n:]
	}
	if errState := rememberUsageLogStateLocked(filePath); errState != nil {
		return fmt.Errorf("record usage log state: %w", errState)
	}

	return nil
}

func sanitizeUsageLogPathLocked(filePath string) error {
	cleanPath := usageLogCleanPath(filePath)
	current, errState := usageLogState(filePath)
	if errState != nil {
		return errState
	}
	if sanitized, ok := usageLogSanitizedStates[cleanPath]; ok && sanitized == current {
		return nil
	}
	if errSanitize := sanitizeExistingUsageLog(filePath); errSanitize != nil {
		return errSanitize
	}
	return rememberUsageLogStateLocked(filePath)
}

func rememberUsageLogStateLocked(filePath string) error {
	state, errState := usageLogState(filePath)
	if errState != nil {
		return errState
	}
	usageLogSanitizedStates[usageLogCleanPath(filePath)] = state
	return nil
}

func usageLogState(filePath string) (usageLogFileState, error) {
	info, errStat := os.Stat(filePath)
	if errStat != nil {
		if os.IsNotExist(errStat) {
			return usageLogFileState{}, nil
		}
		return usageLogFileState{}, errStat
	}
	return usageLogFileState{size: info.Size(), modTime: info.ModTime(), exists: true}, nil
}

func usageLogCleanPath(filePath string) string {
	cleanPath, errAbs := filepath.Abs(filePath)
	if errAbs != nil {
		return filepath.Clean(filePath)
	}
	return cleanPath
}

func sanitizeExistingUsageLog(filePath string) error {
	source, errOpen := os.Open(filePath)
	if errOpen != nil {
		if os.IsNotExist(errOpen) {
			return nil
		}
		return errOpen
	}
	sourceClosed := false
	defer func() {
		if !sourceClosed {
			if errClose := source.Close(); errClose != nil {
				log.Errorf("usage log source close error: %v", errClose)
			}
		}
	}()

	temp, errTemp := os.CreateTemp(filepath.Dir(filePath), ".usage-sanitize-*")
	if errTemp != nil {
		return errTemp
	}
	tempPath := temp.Name()
	tempClosed := false
	keepTemp := false
	defer func() {
		if !tempClosed {
			if errClose := temp.Close(); errClose != nil {
				log.Errorf("usage log temp close error: %v", errClose)
			}
		}
		if !keepTemp {
			if errRemove := os.Remove(tempPath); errRemove != nil && !os.IsNotExist(errRemove) {
				log.Errorf("usage log temp remove error: %v", errRemove)
			}
		}
	}()

	reader := bufio.NewReader(source)
	writer := bufio.NewWriter(temp)
	for {
		rawLine, errRead := reader.ReadString('\n')
		if rawLine != "" {
			line := strings.TrimSpace(rawLine)
			if line != "" {
				sanitized, errSanitize := cluster.SanitizeUsagePayloadSecrets(line)
				if errSanitize != nil {
					line = `{"event_type":"discarded_invalid_historical_usage"}`
				} else {
					line = sanitized
				}
			}
			if _, errWrite := writer.WriteString(line + "\n"); errWrite != nil {
				return errWrite
			}
		}
		if errRead != nil {
			if errRead == io.EOF {
				break
			}
			return errRead
		}
	}
	if errFlush := writer.Flush(); errFlush != nil {
		return errFlush
	}
	if errChmod := temp.Chmod(0o600); errChmod != nil {
		return errChmod
	}
	if errSync := temp.Sync(); errSync != nil {
		return errSync
	}
	if errClose := source.Close(); errClose != nil {
		return errClose
	}
	sourceClosed = true
	if errClose := temp.Close(); errClose != nil {
		return errClose
	}
	tempClosed = true
	if errRename := os.Rename(tempPath, filePath); errRename != nil {
		return errRename
	}
	keepTemp = true
	return nil
}

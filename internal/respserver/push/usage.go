package push

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
	"github.com/router-for-me/CLIProxyAPIHome/internal/respserver/dispatch"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

var usageLogMu sync.Mutex

// handleUsage handles an usage.
func handleUsage(ctx context.Context, env dispatch.Env, args []string) dispatch.Reply {
	if len(args) != 3 {
		return dispatch.Err("wrong number of arguments for 'lprush' command")
	}

	if !strings.EqualFold(strings.TrimSpace(args[1]), "usage") {
		return dispatch.Err("unsupported key")
	}

	payload := strings.TrimSpace(args[2])
	if payload == "" || !gjson.Valid(payload) {
		return dispatch.Err("invalid usage json")
	}
	enrichedPayload, errEnrich := usagePayloadWithCPAIdentity(payload, env)
	if errEnrich != nil {
		return dispatch.Err("invalid usage json")
	}
	payload = enrichedPayload

	// Always update scheduler state before queuing persistence so cooldowns are applied even if log persistence fails.
	if env.Runtime != nil {
		env.Runtime.RecordUsagePayload(ctx, payload)
		persisted, errPersist := env.Runtime.PersistClusterUsagePayload(ctx, payload)
		if errPersist != nil {
			log.Errorf("usage database write error: %v", errPersist)
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

// appendUsageLog appends an usage log.
func appendUsageLog(payload string) error {
	// Decode the wire frame before dispatching command handling.
	if strings.TrimSpace(payload) == "" {
		return fmt.Errorf("empty payload")
	}

	usageLogMu.Lock()
	defer usageLogMu.Unlock()

	logDir := "logs"
	if errMk := os.MkdirAll(logDir, 0o755); errMk != nil {
		return fmt.Errorf("ensure logs dir: %w", errMk)
	}

	filePath := filepath.Join(logDir, "usage.log")
	f, errOpen := os.OpenFile(filePath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
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

	return nil
}

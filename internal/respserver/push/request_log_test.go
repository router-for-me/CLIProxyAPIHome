package push

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPIHome/internal/respserver/dispatch"
)

func TestHandleRequestLogUsesPayloadRequestID(t *testing.T) {
	t.Chdir(t.TempDir())

	payload := `{"request_id":"payload-req-1","headers":{"x-request-id":["header-req-1"]},"request_log":"=== REQUEST INFO ===\nURL: /v1/responses?foo=bar\nMethod: POST\n"}`
	reply := handleRequestLog(context.Background(), dispatch.Env{ClientIP: "10.0.0.5"}, []string{"RPUSH", "request-log", payload})
	if reply.Kind != dispatch.ReplyKindInteger || reply.Integer != 1 {
		t.Fatalf("reply = %+v, want integer 1", reply)
	}

	files, errGlob := filepath.Glob("logs/*-payload-req-1.log")
	if errGlob != nil {
		t.Fatalf("glob request log: %v", errGlob)
	}
	if len(files) != 1 {
		t.Fatalf("payload request_id log files = %d, want 1", len(files))
	}
	if strings.Contains(filepath.Base(files[0]), "header-req-1") {
		t.Fatalf("filename %q used header request id instead of payload request_id", filepath.Base(files[0]))
	}
}

func TestHandleRequestLogFallsBackToHeaderRequestID(t *testing.T) {
	t.Chdir(t.TempDir())

	payload := `{"headers":{"x-cpa-request-id":["header-req-2"]},"request_log":"=== REQUEST INFO ===\nURL: /v1/chat/completions\nMethod: POST\n"}`
	reply := handleRequestLog(context.Background(), dispatch.Env{ClientIP: "10.0.0.5"}, []string{"RPUSH", "request-log", payload})
	if reply.Kind != dispatch.ReplyKindInteger || reply.Integer != 1 {
		t.Fatalf("reply = %+v, want integer 1", reply)
	}

	files, errGlob := filepath.Glob("logs/*-header-req-2.log")
	if errGlob != nil {
		t.Fatalf("glob request log: %v", errGlob)
	}
	if len(files) != 1 {
		t.Fatalf("header request id log files = %d, want 1", len(files))
	}
}

func TestHandleRequestLogExtractsURLFromJSON(t *testing.T) {
	t.Chdir(t.TempDir())

	payload := `{"request_id":"json-req-1","request_log":"{\"version\":\"dev\",\"url\":\"/v1/responses?stream=true\",\"method\":\"POST\"}"}`
	reply := handleRequestLog(context.Background(), dispatch.Env{ClientIP: "10.0.0.5"}, []string{"RPUSH", "request-log", payload})
	if reply.Kind != dispatch.ReplyKindInteger || reply.Integer != 1 {
		t.Fatalf("reply = %+v, want integer 1", reply)
	}

	files, errGlob := filepath.Glob("logs/10.0.0.5-v1-responses-*-json-req-1.log")
	if errGlob != nil {
		t.Fatalf("glob request log: %v", errGlob)
	}
	if len(files) != 1 {
		t.Fatalf("json url request log files = %d, want 1", len(files))
	}
}

func TestHandleRequestLogJSONFallsBackToRoot(t *testing.T) {
	t.Chdir(t.TempDir())

	payload := `{"request_id":"json-req-2","request_log":"{\"version\":\"dev\",\"method\":\"POST\"}"}`
	reply := handleRequestLog(context.Background(), dispatch.Env{ClientIP: "10.0.0.5"}, []string{"RPUSH", "request-log", payload})
	if reply.Kind != dispatch.ReplyKindInteger || reply.Integer != 1 {
		t.Fatalf("reply = %+v, want integer 1", reply)
	}

	files, errGlob := filepath.Glob("logs/10.0.0.5-root-*-json-req-2.log")
	if errGlob != nil {
		t.Fatalf("glob request log: %v", errGlob)
	}
	if len(files) != 1 {
		t.Fatalf("json root fallback log files = %d, want 1", len(files))
	}
}

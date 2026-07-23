package push

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
	"github.com/router-for-me/CLIProxyAPIHome/internal/concurrency"
	"github.com/router-for-me/CLIProxyAPIHome/internal/respserver/dispatch"
)

type recordingConcurrencyReleaseStore struct {
	request cluster.ConcurrencyReleaseRequest
	ctx     context.Context
	err     error
	calls   int
}

func (s *recordingConcurrencyReleaseStore) ApplyConcurrencyRelease(ctx context.Context, request cluster.ConcurrencyReleaseRequest) error {
	s.calls++
	s.ctx = ctx
	s.request = request
	return s.err
}

func TestConcurrencyReleaseHandlerUsesStrictJSONFixture(t *testing.T) {
	raw, errRead := os.ReadFile(filepath.Join("..", "testdata", "concurrency_release.json"))
	if errRead != nil {
		t.Fatal(errRead)
	}
	want := []byte("{\"credential_id\":\"cred-1\",\"model\":\"gpt\",\"release_seq\":1}\n")
	if !bytes.Equal(raw, want) {
		t.Fatalf("fixture = %q, want %q", raw, want)
	}

	store := &recordingConcurrencyReleaseStore{}
	connectedAt := time.Unix(100, 0).UTC()
	lifetime := cluster.ConnectionLifetime{Fingerprint: "trusted-fingerprint", ConnectedAt: connectedAt, Controlled: true}
	reply := handleConcurrencyRelease(context.Background(), dispatch.Env{
		ConcurrencyReleaseStore: store,
		ConnectionLifetime:      lifetime,
	}, []string{"LPUSH", "concurrency-release", string(raw)})
	if reply.Kind != dispatch.ReplyKindInteger || reply.Integer != 1 {
		t.Fatalf("reply = %#v", reply)
	}
	if store.request.CredentialID != "cred-1" || store.request.Model != "gpt" || store.request.ReleaseSeq != 1 {
		t.Fatalf("request = %#v", store.request)
	}
	if store.request.Lifetime != lifetime {
		t.Fatalf("lifetime = %#v, want %#v", store.request.Lifetime, lifetime)
	}
}

func TestConcurrencyReleaseHandlerSupportsLegacyArguments(t *testing.T) {
	store := &recordingConcurrencyReleaseStore{}
	lifetime := cluster.ConnectionLifetime{Fingerprint: "trusted-fingerprint", ConnectedAt: time.Unix(100, 0).UTC(), Controlled: true}
	reply := handleConcurrencyRelease(context.Background(), dispatch.Env{
		ConcurrencyReleaseStore: store,
		ConnectionLifetime:      lifetime,
	}, []string{"LPUSH", "concurrency-release", "credential-a", "gpt(high)", "2"})
	if reply.Kind != dispatch.ReplyKindInteger || reply.Integer != 1 {
		t.Fatalf("reply = %#v", reply)
	}
	if store.request.CredentialID != "credential-a" || store.request.Model != "gpt" || store.request.ReleaseSeq != 2 {
		t.Fatalf("request = %#v", store.request)
	}
}

func TestConcurrencyReleaseHandlerRejectsNonStrictJSON(t *testing.T) {
	store := &recordingConcurrencyReleaseStore{}
	env := dispatch.Env{
		ConcurrencyReleaseStore: store,
		ConnectionLifetime:      cluster.ConnectionLifetime{Fingerprint: "fp-a", ConnectedAt: time.Unix(100, 0).UTC(), Controlled: true},
	}
	for _, frame := range []string{
		`{"credential_id":"cred","model":"gpt","release_seq":1,"unknown":true}`,
		`{"credential_id":"cred","model":"gpt","release_seq":1}{}`,
	} {
		reply := handleConcurrencyRelease(context.Background(), env, []string{"LPUSH", "concurrency-release", frame})
		if reply.Kind != dispatch.ReplyKindRedisError || store.calls != 0 {
			t.Fatalf("frame %q: reply = %#v, store calls = %d", frame, reply, store.calls)
		}
	}
}

func TestConcurrencyReleaseHandlerRejectsInvalidInputAndUnavailableStore(t *testing.T) {
	store := &recordingConcurrencyReleaseStore{err: errors.New("database unavailable")}
	validEnv := dispatch.Env{
		ConcurrencyReleaseStore: store,
		ConnectionLifetime:      cluster.ConnectionLifetime{Fingerprint: "fp-a", ConnectedAt: time.Unix(100, 0).UTC(), Controlled: true},
	}
	tests := []struct {
		name string
		env  dispatch.Env
		args []string
	}{
		{name: "wrong command", env: validEnv, args: []string{"RPUSH", "concurrency-release", "cred", "gpt", "1"}},
		{name: "wrong argument count", env: validEnv, args: []string{"LPUSH", "concurrency-release", "cred", "gpt"}},
		{name: "empty credential", env: validEnv, args: []string{"LPUSH", "concurrency-release", "", "gpt", "1"}},
		{name: "invalid sequence", env: validEnv, args: []string{"LPUSH", "concurrency-release", "cred", "gpt", "0"}},
		{name: "missing lifetime", env: dispatch.Env{ConcurrencyReleaseStore: store, ConnectionLifetime: cluster.ConnectionLifetime{Controlled: true}}, args: []string{"LPUSH", "concurrency-release", "cred", "gpt", "1"}},
		{name: "unavailable store", env: dispatch.Env{ConnectionLifetime: validEnv.ConnectionLifetime}, args: []string{"LPUSH", "concurrency-release", "cred", "gpt", "1"}},
		{name: "store failure", env: validEnv, args: []string{"LPUSH", "concurrency-release", "cred", "gpt", "1"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			reply := handleConcurrencyRelease(context.Background(), test.env, test.args)
			if reply.Kind != dispatch.ReplyKindRedisError {
				t.Fatalf("reply = %#v, want redis error", reply)
			}
		})
	}
}

func TestConcurrencyReleaseHandlerEnforcesCanonicalModelKeyByteLimit(t *testing.T) {
	for _, testCase := range []struct {
		name  string
		model string
		valid bool
	}{
		{name: "ascii boundary", model: strings.Repeat("a", concurrency.MaxCanonicalModelKeyBytes), valid: true},
		{name: "multibyte boundary", model: strings.Repeat("界", 85) + "a", valid: true},
		{name: "ascii over", model: strings.Repeat("a", concurrency.MaxCanonicalModelKeyBytes+1)},
		{name: "multibyte over", model: strings.Repeat("界", 86)},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			store := &recordingConcurrencyReleaseStore{}
			env := dispatch.Env{
				ConcurrencyReleaseStore: store,
				ConnectionLifetime:      cluster.ConnectionLifetime{Fingerprint: "fp-a", ConnectedAt: time.Unix(100, 0).UTC(), Controlled: true},
			}
			reply := handleConcurrencyRelease(context.Background(), env, []string{"LPUSH", "concurrency-release", "cred", testCase.model + "(high)", "1"})
			if testCase.valid {
				if reply.Kind != dispatch.ReplyKindInteger || store.calls != 1 || store.request.Model != testCase.model {
					t.Fatalf("reply = %#v, store = %#v", reply, store)
				}
				return
			}
			if reply.Kind != dispatch.ReplyKindRedisError || store.calls != 0 {
				t.Fatalf("reply = %#v, store calls = %d", reply, store.calls)
			}
		})
	}
}

func TestConcurrencyReleaseHandlerRejectsMalformedUTF8BeforeStoreCall(t *testing.T) {
	store := &recordingConcurrencyReleaseStore{}
	env := dispatch.Env{
		ConcurrencyReleaseStore: store,
		ConnectionLifetime:      cluster.ConnectionLifetime{Fingerprint: "fp-a", ConnectedAt: time.Unix(100, 0).UTC(), Controlled: true},
	}
	reply := handleConcurrencyRelease(context.Background(), env, []string{"LPUSH", "concurrency-release", "cred", "GPT\xff(HIGH)", "1"})
	if reply.Kind != dispatch.ReplyKindRedisError || store.calls != 0 {
		t.Fatalf("reply = %#v, store calls = %d", reply, store.calls)
	}
}

func TestConcurrencyReleaseHandlerRejectsOversizedCredentialID(t *testing.T) {
	store := &recordingConcurrencyReleaseStore{}
	env := dispatch.Env{
		ConcurrencyReleaseStore: store,
		ConnectionLifetime:      cluster.ConnectionLifetime{Fingerprint: "fp-a", ConnectedAt: time.Unix(100, 0).UTC(), Controlled: true},
	}
	reply := handleConcurrencyRelease(context.Background(), env, []string{"LPUSH", "concurrency-release", string(make([]byte, 257)), "gpt", "1"})
	if reply.Kind != dispatch.ReplyKindRedisError || store.calls != 0 {
		t.Fatalf("reply = %#v, store calls = %d", reply, store.calls)
	}
}

func TestRegisterRoutesConcurrencyRelease(t *testing.T) {
	store := &recordingConcurrencyReleaseStore{}
	registry := dispatch.NewRegistry()
	Register(registry)
	reply := registry.Execute(context.Background(), dispatch.Env{
		ConcurrencyReleaseStore: store,
		ConnectionLifetime:      cluster.ConnectionLifetime{Fingerprint: "fp-a", ConnectedAt: time.Unix(100, 0).UTC(), Controlled: true},
	}, []string{"LPUSH", "concurrency-release", "cred", "gpt", "1"})
	if reply.Kind != dispatch.ReplyKindInteger || reply.Integer != 1 {
		t.Fatalf("reply = %#v", reply)
	}
}

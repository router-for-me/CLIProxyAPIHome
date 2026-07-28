package dispatch

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
	"github.com/router-for-me/CLIProxyAPIHome/internal/home"
	"github.com/tidwall/gjson"
)

type InFlightSnapshotStore interface {
	IngestInFlightFrame(context.Context, cluster.InFlightIngestIdentity, []byte, cluster.InFlightLimits) (cluster.InFlightIngestResult, error)
}

// ConcurrencyReleaseStore atomically applies cumulative credential concurrency releases.
type ConcurrencyReleaseStore interface {
	ApplyConcurrencyRelease(context.Context, cluster.ConcurrencyReleaseRequest) error
}

type Env struct {
	Runtime *home.Runtime
	Conn    *ConnEnv

	// InFlightStore persists in-flight observation frames.
	InFlightStore InFlightSnapshotStore

	// InFlightLimits bounds the in-flight observation frame accepted by InFlightStore.
	InFlightLimits cluster.InFlightLimits

	// ConcurrencyReleaseStore applies cumulative limiter releases from this connection.
	ConcurrencyReleaseStore ConcurrencyReleaseStore

	// ClientIP is the remote TCP client's IP address as resolved by the RESP server.
	// It can be empty when the address cannot be resolved.
	ClientIP string

	// NodeID is the mTLS client certificate common name when available.
	// It can be empty in unit tests or unauthenticated contexts.
	NodeID string

	// ClientCertificateFingerprint is the SHA-256 fingerprint of the mTLS leaf certificate.
	ClientCertificateFingerprint string

	// ConnectionLifetime is fixed when the connection is accepted and cannot be
	// upgraded by a subscription performed on the same socket. Its fingerprint and
	// connected time are the only in-flight snapshot ownership identity.
	ConnectionLifetime cluster.ConnectionLifetime
}

type ConnEnv struct {
	SubscribeConfigYAML            func() (int64, error)
	UnsubscribeConfigYAML          func() (int64, error)
	IsSubscribed                   func() bool
	SubscribeMembership            func(context.Context, int, int64, bool, string) (cluster.ConnectionLifetime, error)
	AttachSubscriptionLifetimeFunc func(cluster.ConnectionLifetime) error
	FenceFingerprint               func(context.Context, string) error
	CloseLocalFingerprint          func()
	PrepareDispatchReply           func() error
	AccountedReplyFailure          func() error

	lifetimeMu           sync.RWMutex
	subscriptionLifetime *cluster.ConnectionLifetime
}

// AttachSubscriptionLifetime records the lifetime attached to a subscription socket.
func (e *ConnEnv) AttachSubscriptionLifetime(lifetime cluster.ConnectionLifetime) error {
	if e == nil {
		return fmt.Errorf("connection environment is nil")
	}
	if strings.TrimSpace(lifetime.Fingerprint) == "" || lifetime.ConnectedAt.IsZero() {
		return fmt.Errorf("subscription lifetime is invalid")
	}
	if lifetime.Controlled || !lifetime.Subscription {
		return fmt.Errorf("subscription lifetime must be non-controlled")
	}
	if e.AttachSubscriptionLifetimeFunc != nil {
		if errAttach := e.AttachSubscriptionLifetimeFunc(lifetime); errAttach != nil {
			return errAttach
		}
	}
	e.lifetimeMu.Lock()
	defer e.lifetimeMu.Unlock()
	if e.subscriptionLifetime != nil && *e.subscriptionLifetime != lifetime {
		return fmt.Errorf("subscription lifetime is already attached")
	}
	attached := lifetime
	e.subscriptionLifetime = &attached
	return nil
}

// SubscriptionLifetime returns the attached subscription lifetime, when present.
func (e *ConnEnv) SubscriptionLifetime() (cluster.ConnectionLifetime, bool) {
	if e == nil {
		return cluster.ConnectionLifetime{}, false
	}
	e.lifetimeMu.RLock()
	defer e.lifetimeMu.RUnlock()
	if e.subscriptionLifetime == nil {
		return cluster.ConnectionLifetime{}, false
	}
	return *e.subscriptionLifetime, true
}

type ReplyKind int

const (
	ReplyKindSimpleString ReplyKind = iota
	ReplyKindBulkString
	ReplyKindRedisError
	ReplyKindInteger
	ReplyKindArray
)

type Reply struct {
	Kind ReplyKind

	// SubscriptionLifetime is server-only metadata and is never serialized.
	SubscriptionLifetime *cluster.ConnectionLifetime

	SimpleString       string
	BulkString         []byte
	RedisError         string
	Integer            int64
	Array              []Reply
	Sensitive          bool
	AccountedAdmission bool
	PreWriteError      error
}

func SensitiveBulkString(payload []byte) Reply {
	return Reply{Kind: ReplyKindBulkString, BulkString: payload, Sensitive: true}
}

func (r *Reply) ClearSensitive() {
	if r == nil {
		return
	}
	if r.Sensitive {
		for index := range r.BulkString {
			r.BulkString[index] = 0
		}
		r.BulkString = nil
	}
	for index := range r.Array {
		r.Array[index].ClearSensitive()
	}
}

// SimpleString builds a dispatch reply.
func SimpleString(value string) Reply {
	return Reply{
		Kind:         ReplyKindSimpleString,
		SimpleString: value,
	}
}

// BulkString builds a dispatch reply.
func BulkString(payload []byte) Reply {
	return Reply{
		Kind:       ReplyKindBulkString,
		BulkString: payload,
	}
}

// Integer builds a dispatch reply.
func Integer(value int64) Reply {
	return Reply{
		Kind:    ReplyKindInteger,
		Integer: value,
	}
}

// Array builds a dispatch reply.
func Array(elements ...Reply) Reply {
	return Reply{
		Kind:  ReplyKindArray,
		Array: elements,
	}
}

// RedisError builds a dispatch reply.
func RedisError(message string) Reply {
	message = strings.TrimSpace(message)
	if message == "" {
		message = "ERR error"
	}
	return Reply{
		Kind:       ReplyKindRedisError,
		RedisError: message,
	}
}

// Err builds a dispatch reply.
func Err(message string) Reply {
	message = strings.TrimSpace(message)
	if message == "" {
		message = "error"
	}
	return RedisError("ERR " + message)
}

type Handler func(ctx context.Context, env Env, args []string) Reply

type Registry struct {
	directHandlers        map[string]map[string]Handler
	directDefaultHandlers map[string]Handler
	dynamicHandlers       map[string]*dynamicHandlers
}

type dynamicHandlers struct {
	byType          map[string]Handler
	defaultHandler  Handler
	extractJSONFunc func(args []string) (string, bool)
}

// NewRegistry creates a new registry.
func NewRegistry() *Registry {
	return &Registry{
		directHandlers:        map[string]map[string]Handler{},
		directDefaultHandlers: map[string]Handler{},
		dynamicHandlers:       map[string]*dynamicHandlers{},
	}
}

// RegisterDirect handles a register direct.
func (r *Registry) RegisterDirect(command string, key string, handler Handler) error {
	if r == nil {
		return fmt.Errorf("registry is nil")
	}
	command = normalizeCommand(command)
	if command == "" {
		return fmt.Errorf("command is empty")
	}
	key = normalizeKey(key)
	if key == "" {
		return fmt.Errorf("key is empty")
	}
	if handler == nil {
		return fmt.Errorf("handler is nil")
	}

	if r.directHandlers[command] == nil {
		r.directHandlers[command] = map[string]Handler{}
	}
	r.directHandlers[command][key] = handler
	return nil
}

// SetDirectDefault sets a direct default.
func (r *Registry) SetDirectDefault(command string, handler Handler) error {
	if r == nil {
		return fmt.Errorf("registry is nil")
	}
	command = normalizeCommand(command)
	if command == "" {
		return fmt.Errorf("command is empty")
	}
	if handler == nil {
		return fmt.Errorf("handler is nil")
	}

	r.directDefaultHandlers[command] = handler
	return nil
}

// RegisterDynamic handles a register dynamic.
func (r *Registry) RegisterDynamic(command string, typeValue string, handler Handler) error {
	// Decode the wire frame before dispatching command handling.
	if r == nil {
		return fmt.Errorf("registry is nil")
	}
	command = normalizeCommand(command)
	if command == "" {
		return fmt.Errorf("command is empty")
	}
	typeValue = normalizeType(typeValue)
	if typeValue == "" {
		return fmt.Errorf("type is empty")
	}
	if handler == nil {
		return fmt.Errorf("handler is nil")
	}

	dyn := r.dynamicHandlers[command]
	if dyn == nil {
		dyn = &dynamicHandlers{
			byType: map[string]Handler{},
			extractJSONFunc: func(args []string) (string, bool) {
				return ExtractJSONArgument(args, 1)
			},
		}
		r.dynamicHandlers[command] = dyn
	}
	dyn.byType[typeValue] = handler
	return nil
}

// SetDynamicDefault sets a dynamic default.
func (r *Registry) SetDynamicDefault(command string, handler Handler) error {
	// Decode the wire frame before dispatching command handling.
	if r == nil {
		return fmt.Errorf("registry is nil")
	}
	command = normalizeCommand(command)
	if command == "" {
		return fmt.Errorf("command is empty")
	}
	if handler == nil {
		return fmt.Errorf("handler is nil")
	}

	dyn := r.dynamicHandlers[command]
	if dyn == nil {
		dyn = &dynamicHandlers{
			byType: map[string]Handler{},
			extractJSONFunc: func(args []string) (string, bool) {
				return ExtractJSONArgument(args, 1)
			},
		}
		r.dynamicHandlers[command] = dyn
	}
	dyn.defaultHandler = handler
	return nil
}

// Execute handles an execute.
func (r *Registry) Execute(ctx context.Context, env Env, args []string) Reply {
	// Decode the wire frame before dispatching command handling.
	if r == nil {
		return Err("registry not ready")
	}
	if len(args) == 0 {
		return Err("empty command")
	}

	command := normalizeCommand(args[0])
	if command == "" {
		return Err("empty command")
	}
	if commandRequiresControlledLifetime(args) && !env.ConnectionLifetime.Controlled {
		return Err("controlled connection required")
	}

	if direct := r.directHandlers[command]; direct != nil {
		if len(args) < 2 {
			if directDefault := r.directDefaultHandlers[command]; directDefault != nil {
				return directDefault(ctx, env, args)
			}
			return Err(fmt.Sprintf("wrong number of arguments for '%s' command", strings.ToLower(command)))
		}
		key := normalizeKey(args[1])
		handler := direct[key]
		if handler == nil {
			if directDefault := r.directDefaultHandlers[command]; directDefault != nil {
				return directDefault(ctx, env, args)
			}
			return Err("unsupported key")
		}
		return handler(ctx, env, args)
	}

	if dyn := r.dynamicHandlers[command]; dyn != nil {
		jsonArg, ok := dyn.extractJSONFunc(args)
		if !ok {
			if dyn.defaultHandler != nil {
				return dyn.defaultHandler(ctx, env, args)
			}
			return Err(fmt.Sprintf("wrong number of arguments for '%s' command", strings.ToLower(command)))
		}

		typeValue := normalizeType(extractTypeFromJSON(jsonArg))
		if typeValue == "" {
			return Err("unsupported type")
		}
		handler := dyn.byType[typeValue]
		if handler != nil {
			return handler(ctx, env, args)
		}
		return Err("unsupported type")
	}

	if directDefault := r.directDefaultHandlers[command]; directDefault != nil {
		return directDefault(ctx, env, args)
	}

	return RedisError(fmt.Sprintf("ERR unknown command '%s'", strings.ToLower(command)))
}

func commandRequiresControlledLifetime(args []string) bool {
	if len(args) == 0 {
		return false
	}
	switch strings.ToUpper(strings.TrimSpace(args[0])) {
	case "RPOP", "LPUSH", "RPUSH", "SET", "CAS", "DEL", "MSET", "INCRBY", "EXPIRE":
		return true
	default:
		return false
	}
}

// ExtractJSONArgument extracts a json argument.
func ExtractJSONArgument(args []string, jsonIndex int) (string, bool) {
	if len(args) == 2 && jsonIndex == 1 {
		return args[1], true
	}
	if len(args) == 3 && jsonIndex == 1 {
		return args[2], true
	}
	return "", false
}

// extractTypeFromJSON derives extract type from json.
func extractTypeFromJSON(jsonArg string) string {
	jsonArg = strings.TrimSpace(jsonArg)
	if jsonArg == "" || !gjson.Valid(jsonArg) {
		return ""
	}
	return gjson.Get(jsonArg, "type").String()
}

// normalizeCommand normalizes a command.
func normalizeCommand(value string) string {
	return strings.ToUpper(strings.TrimSpace(value))
}

// normalizeKey normalizes a key.
func normalizeKey(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

// normalizeType normalizes a type.
func normalizeType(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

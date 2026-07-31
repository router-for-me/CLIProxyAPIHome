package cluster

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	homeerrors "github.com/router-for-me/CLIProxyAPIHome/internal/errors"
)

const clusterRESPTimeout = 35 * time.Second

type RESPHandler struct {
	coordinator *Coordinator
	refresh     *RefreshController
	repo        *Repository
}

type clientNodePayload struct {
	IP          string    `json:"ip"`
	Port        int       `json:"port"`
	ClientCount int       `json:"client_count"`
	IsMaster    bool      `json:"is_master"`
	LastSeenAt  time.Time `json:"last_seen_at"`
}

// NewRESPHandler creates a new resp handler.
func NewRESPHandler(coordinator *Coordinator, refresh *RefreshController, repo *Repository) *RESPHandler {
	return &RESPHandler{
		coordinator: coordinator,
		refresh:     refresh,
		repo:        repo,
	}
}

// BeginFingerprintCancellationForLifetime starts distributed cancellation for an ambiguous dispatch delivery.
func (h *RESPHandler) BeginFingerprintCancellationForLifetime(ctx context.Context, lifetime ConnectionLifetime) (int64, error) {
	if h == nil || h.repo == nil {
		return 0, fmt.Errorf("cluster resp: membership handler is not ready")
	}
	return h.repo.BeginFingerprintCancellationForLifetime(ctx, lifetime)
}

// UpdateClientCount stores the current active CPA client count for this node.
func (h *RESPHandler) UpdateClientCount(ctx context.Context, clientCount int) error {
	if h == nil || h.coordinator == nil {
		return nil
	}
	return h.coordinator.UpdateClientCount(ctx, clientCount)
}

// ClassifyConnection classifies a CPA connection using this command Home incarnation.
func (h *RESPHandler) ClassifyConnection(ctx context.Context, fingerprint string) (ConnectionLifetime, error) {
	if h == nil || h.repo == nil || h.coordinator == nil {
		return ConnectionLifetime{}, fmt.Errorf("cluster resp: membership handler is not ready")
	}
	home, initialized := h.coordinator.HomeIncarnation()
	if !initialized {
		return ConnectionLifetime{}, fmt.Errorf("cluster resp: Home incarnation is not initialized")
	}
	return h.repo.ClassifyConnection(ctx, fingerprint, home)
}

// SubscribeMembership creates a membership owned by this Home incarnation.
// RefreshCPALiveness records a successful subscription heartbeat for the exact lifetime.
func (h *RESPHandler) RefreshCPALiveness(ctx context.Context, lifetime ConnectionLifetime) error {
	if h == nil || h.repo == nil || h.coordinator == nil {
		return fmt.Errorf("cluster resp: membership handler is not ready")
	}
	home, initialized := h.coordinator.HomeIncarnation()
	if !initialized {
		return fmt.Errorf("cluster resp: Home incarnation is not initialized")
	}
	if lifetime.Home != home {
		return ErrHomeIncarnationFenced
	}
	return h.repo.RefreshCPALiveness(ctx, lifetime)
}

// SubscribeMembership creates a membership owned by this Home incarnation.
func (h *RESPHandler) SubscribeMembership(ctx context.Context, fingerprint string, nodeID string, protocolVersion int, lifecycleConfigRevision int64, takeover bool, instanceID string) (ConnectionLifetime, error) {
	if h == nil || h.repo == nil || h.coordinator == nil {
		return ConnectionLifetime{}, fmt.Errorf("cluster resp: membership handler is not ready")
	}
	home, initialized := h.coordinator.HomeIncarnation()
	if !initialized {
		return ConnectionLifetime{}, fmt.Errorf("cluster resp: Home incarnation is not initialized")
	}
	member, errSubscribe := h.repo.SubscribeMembership(ctx, SubscribeMembershipRequest{
		Fingerprint:             fingerprint,
		NodeID:                  nodeID,
		Home:                    home,
		ProtocolVersion:         protocolVersion,
		LifecycleConfigRevision: lifecycleConfigRevision,
		Takeover:                takeover,
	})
	if errSubscribe != nil {
		return ConnectionLifetime{}, errSubscribe
	}
	return ConnectionLifetime{
		Fingerprint:  member.CertificateFingerprint,
		ConnectedAt:  member.ConnectedAt,
		InstanceID:   strings.TrimSpace(instanceID),
		Home:         home,
		Subscription: true,
	}, nil
}

// RequestClientCertificate signs a pending client certificate request.
func (h *RESPHandler) RequestClientCertificate(ctx context.Context, certificateID string, enrollmentSecret string, csrPEM []byte) ([]byte, error) {
	if h == nil || h.repo == nil {
		return nil, fmt.Errorf("cluster resp: repository is nil")
	}
	return h.repo.SignClientCertificateRequestJSON(ctx, certificateID, enrollmentSecret, csrPEM)
}

// Handle handles handle.
func (h *RESPHandler) Handle(ctx context.Context, args []string, remoteIP string) ([]byte, error) {
	// Validate request inputs before mutating persisted state.
	if h == nil {
		return nil, fmt.Errorf("cluster resp: handler is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if len(args) < 2 {
		return nil, fmt.Errorf("cluster resp: wrong number of arguments")
	}
	if !strings.EqualFold(strings.TrimSpace(args[0]), "CLUSTER") {
		return nil, fmt.Errorf("cluster resp: unsupported command")
	}

	switch strings.ToUpper(strings.TrimSpace(args[1])) {
	case "NODES":
		if len(args) != 2 {
			return nil, fmt.Errorf("cluster resp: wrong number of arguments for 'cluster nodes'")
		}
		return h.nodesPayload(ctx)
	case "PING":
		if len(args) != 3 {
			return nil, fmt.Errorf("cluster resp: wrong number of arguments for 'cluster ping'")
		}
		if _, errNode := h.authorizeNode(ctx, remoteIP, args[2]); errNode != nil {
			return nil, errNode
		}
		return json.Marshal(map[string]any{"ok": true})
	case "NODE":
		if len(args) != 3 {
			return nil, fmt.Errorf("cluster resp: wrong number of arguments for 'cluster node'")
		}
		if _, errNode := h.authorizeNode(ctx, remoteIP, args[2]); errNode != nil {
			return nil, errNode
		}
		return h.nodePayload(ctx)
	case "REFRESH":
		if len(args) < 4 || len(args) > 5 {
			return nil, fmt.Errorf("cluster resp: wrong number of arguments for 'cluster refresh'")
		}
		if _, errNode := h.authorizeNode(ctx, remoteIP, args[3]); errNode != nil {
			return nil, errNode
		}
		if h.refresh == nil {
			return nil, fmt.Errorf("cluster resp: refresh controller is nil")
		}
		observedAccessTokenSHA256 := ""
		if len(args) == 5 {
			observedAccessTokenSHA256 = strings.TrimSpace(args[4])
		}
		return h.refresh.RefreshNowObserved(ctx, args[2], observedAccessTokenSHA256)
	default:
		return nil, fmt.Errorf("cluster resp: unsupported subcommand")
	}
}

// nodesPayload returns live cluster nodes for authenticated CPA clients.
func (h *RESPHandler) nodesPayload(ctx context.Context) ([]byte, error) {
	if h == nil || h.repo == nil {
		return nil, fmt.Errorf("cluster resp: repository is nil")
	}
	timeout := defaultHeartbeatTimeout
	if h.coordinator != nil && h.coordinator.heartbeatTimeout > 0 {
		timeout = h.coordinator.heartbeatTimeout
	}
	now, errNow := h.databaseNow(ctx)
	if errNow != nil {
		return nil, errNow
	}
	cutoff := now.Add(-timeout)
	nodes, errNodes := h.repo.ListLiveClusterNodes(ctx, cutoff)
	if errNodes != nil {
		return nil, errNodes
	}
	var master *ClusterNodeRecord
	if h.coordinator != nil {
		currentMaster, errMaster := h.coordinator.CurrentMaster(ctx)
		if errMaster != nil {
			return nil, errMaster
		}
		master = currentMaster
	}
	sort.Slice(nodes, func(i, j int) bool {
		leftCount := nodes[i].ClientCount
		rightCount := nodes[j].ClientCount
		if leftCount < 0 {
			leftCount = 0
		}
		if rightCount < 0 {
			rightCount = 0
		}
		if leftCount != rightCount {
			return leftCount < rightCount
		}
		if !nodes[i].StartedAt.Equal(nodes[j].StartedAt) {
			return nodes[i].StartedAt.Before(nodes[j].StartedAt)
		}
		return nodeSortKey(nodes[i]) < nodeSortKey(nodes[j])
	})

	payloadNodes := make([]clientNodePayload, 0, len(nodes))
	for _, node := range nodes {
		clientCount := node.ClientCount
		if clientCount < 0 {
			clientCount = 0
		}
		payloadNodes = append(payloadNodes, clientNodePayload{
			IP:          node.IP,
			Port:        node.Port,
			ClientCount: clientCount,
			IsMaster:    master != nil && clusterNodeMatches(node, master.IP, master.Port),
			LastSeenAt:  node.LastSeenAt,
		})
	}
	return json.Marshal(map[string]any{
		"ok":    true,
		"nodes": payloadNodes,
	})
}

// authorizeNode authorizes a node.
func (h *RESPHandler) authorizeNode(ctx context.Context, remoteIP string, secret string) (*ClusterNodeRecord, error) {
	// Normalize auth state before updating runtime indexes.
	if h == nil || h.repo == nil {
		return nil, fmt.Errorf("cluster resp: repository is nil")
	}
	remoteIP = strings.TrimSpace(remoteIP)
	if remoteIP == "" {
		return nil, fmt.Errorf("cluster resp: remote ip is empty")
	}
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return nil, fmt.Errorf("cluster resp: node secret is required")
	}
	timeout := defaultHeartbeatTimeout
	if h.coordinator != nil && h.coordinator.heartbeatTimeout > 0 {
		timeout = h.coordinator.heartbeatTimeout
	}
	now, errNow := h.databaseNow(ctx)
	if errNow != nil {
		return nil, errNow
	}
	cutoff := now.Add(-timeout)
	node, errNode := h.repo.LiveClusterNodeByIPAndSecret(ctx, remoteIP, secret, cutoff)
	if errNode != nil {
		return nil, errNode
	}
	if node == nil || node.Port <= 0 {
		return nil, fmt.Errorf("cluster resp: remote node is not authorized")
	}
	return node, nil
}

func (h *RESPHandler) databaseNow(ctx context.Context) (time.Time, error) {
	if h == nil || h.repo == nil {
		return time.Time{}, fmt.Errorf("cluster resp: repository is nil")
	}
	db, errDB := h.repo.database()
	if errDB != nil {
		return time.Time{}, errDB
	}
	return DatabaseNow(ctx, db)
}

// nodePayload handles a node payload.
func (h *RESPHandler) nodePayload(ctx context.Context) ([]byte, error) {
	// Validate input data before converting it into runtime state.
	var master *ClusterNodeRecord
	if h.coordinator != nil {
		currentMaster, errMaster := h.coordinator.CurrentMaster(ctx)
		if errMaster != nil {
			return nil, errMaster
		}
		master = currentMaster
	}
	payload := map[string]any{
		"ok":        true,
		"is_master": h.coordinator != nil && h.coordinator.IsMaster(),
	}
	if h.coordinator != nil {
		payload["node"] = map[string]any{
			"ip":   h.coordinator.node.IP,
			"port": h.coordinator.node.Port,
		}
	}
	if master != nil {
		payload["master"] = map[string]any{
			"ip":   master.IP,
			"port": master.Port,
		}
	}
	return json.Marshal(payload)
}

// ForwardRefreshToMaster forwards a legacy refresh request to the master.
func ForwardRefreshToMaster(ctx context.Context, master *ClusterNodeRecord, authUUID string, secret string, tlsConfig *tls.Config) ([]byte, error) {
	return ForwardRefreshToMasterObserved(ctx, master, authUUID, secret, "", tlsConfig)
}

// ForwardRefreshToMasterObserved forwards a token-versioned refresh request.
func ForwardRefreshToMasterObserved(ctx context.Context, master *ClusterNodeRecord, authUUID string, secret string, observedAccessTokenSHA256 string, tlsConfig *tls.Config) ([]byte, error) {
	// Resolve credential context before calling upstream OAuth services.
	if ctx == nil {
		ctx = context.Background()
	}
	if master == nil {
		return nil, fmt.Errorf("cluster resp: master is nil")
	}
	host := strings.TrimSpace(master.IP)
	if host == "" || master.Port <= 0 {
		return nil, fmt.Errorf("cluster resp: master address is invalid")
	}
	authUUID = strings.TrimSpace(authUUID)
	if authUUID == "" {
		return nil, fmt.Errorf("cluster resp: auth uuid is required")
	}
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return nil, fmt.Errorf("cluster resp: node secret is required")
	}
	respTLSConfig, errTLSConfig := respClientTLSConfig(tlsConfig, host)
	if errTLSConfig != nil {
		return nil, errTLSConfig
	}

	dialer := &net.Dialer{Timeout: clusterRESPTimeout}
	conn, errDial := dialer.DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(master.Port)))
	if errDial != nil {
		return nil, errDial
	}
	tlsConn := tls.Client(conn, respTLSConfig)
	defer func() {
		_ = tlsConn.Close()
	}()
	if deadline, ok := ctx.Deadline(); ok {
		_ = tlsConn.SetDeadline(deadline)
	} else {
		_ = tlsConn.SetDeadline(time.Now().Add(clusterRESPTimeout))
	}
	if errHandshake := tlsConn.HandshakeContext(ctx); errHandshake != nil {
		return nil, errHandshake
	}

	args := []string{"CLUSTER", "REFRESH", authUUID, secret}
	observedAccessTokenSHA256 = strings.TrimSpace(observedAccessTokenSHA256)
	if observedAccessTokenSHA256 != "" {
		args = append(args, observedAccessTokenSHA256)
	}
	if _, errWrite := tlsConn.Write(encodeRESPArray(args...)); errWrite != nil {
		return nil, errWrite
	}
	payload, errRead := readRESPBulk(bufio.NewReader(tlsConn))
	if errRead != nil && len(args) == 5 && strings.Contains(strings.ToLower(errRead.Error()), "wrong number of arguments") {
		return ForwardRefreshToMaster(ctx, master, authUUID, secret, tlsConfig)
	}
	return payload, errRead
}

func respClientTLSConfig(tlsConfig *tls.Config, serverName string) (*tls.Config, error) {
	if tlsConfig == nil {
		return nil, fmt.Errorf("cluster resp: tls config is required")
	}
	cfg := tlsConfig.Clone()
	cfg.ClientAuth = tls.NoClientCert
	cfg.NextProtos = nil
	if strings.TrimSpace(cfg.ServerName) == "" {
		cfg.ServerName = strings.TrimSpace(serverName)
	}
	if cfg.RootCAs == nil {
		return nil, fmt.Errorf("cluster resp: tls root ca is required")
	}
	if len(cfg.Certificates) == 0 && cfg.GetClientCertificate == nil {
		return nil, fmt.Errorf("cluster resp: client certificate is required")
	}
	return cfg, nil
}

// HTTPClientTLSConfig derives an mTLS client config for cluster HTTP calls.
func HTTPClientTLSConfig(tlsConfig *tls.Config, serverName string) (*tls.Config, error) {
	if tlsConfig == nil {
		return nil, fmt.Errorf("cluster http: tls config is required")
	}
	cfg := tlsConfig.Clone()
	cfg.ClientAuth = tls.NoClientCert
	cfg.NextProtos = []string{"http/1.1"}
	if strings.TrimSpace(cfg.ServerName) == "" {
		cfg.ServerName = strings.TrimSpace(serverName)
	}
	if cfg.RootCAs == nil {
		return nil, fmt.Errorf("cluster http: tls root ca is required")
	}
	if len(cfg.Certificates) == 0 && cfg.GetClientCertificate == nil {
		return nil, fmt.Errorf("cluster http: client certificate is required")
	}
	return cfg, nil
}

// encodeRESPArray encodes a resp array.
func encodeRESPArray(args ...string) []byte {
	var buf bytes.Buffer
	buf.WriteString("*")
	buf.WriteString(strconv.Itoa(len(args)))
	buf.WriteString("\r\n")
	for _, arg := range args {
		buf.WriteString("$")
		buf.WriteString(strconv.Itoa(len(arg)))
		buf.WriteString("\r\n")
		buf.WriteString(arg)
		buf.WriteString("\r\n")
	}
	return buf.Bytes()
}

// readRESPBulk reads a resp bulk.
func readRESPBulk(reader *bufio.Reader) ([]byte, error) {
	// Validate input data before converting it into runtime state.
	prefix, errRead := reader.ReadByte()
	if errRead != nil {
		return nil, errRead
	}
	switch prefix {
	case '$':
		line, errLine := reader.ReadString('\n')
		if errLine != nil {
			return nil, errLine
		}
		size, errSize := strconv.Atoi(strings.TrimSpace(line))
		if errSize != nil {
			return nil, errSize
		}
		if size < 0 {
			return nil, fmt.Errorf("cluster resp: nil bulk response")
		}
		payload := make([]byte, size+2)
		if _, errFull := io.ReadFull(reader, payload); errFull != nil {
			return nil, errFull
		}
		return payload[:size], nil
	case '+':
		line, errLine := reader.ReadString('\n')
		if errLine != nil {
			return nil, errLine
		}
		return []byte(strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")), nil
	case '-':
		line, errLine := reader.ReadString('\n')
		if errLine != nil {
			return nil, errLine
		}
		return nil, parseClusterRESPError(line)
	default:
		return nil, fmt.Errorf("cluster resp: unsupported response prefix %q", prefix)
	}
}

// parseClusterRESPError restores structured application errors sent by another
// Home node while leaving transport and protocol failures as ordinary errors.
func parseClusterRESPError(line string) error {
	message := strings.TrimSpace(line)
	if len(message) > 4 && strings.EqualFold(message[:4], "ERR ") {
		message = strings.TrimSpace(message[4:])
	}
	errorType, errorMessage := homeerrors.SplitRedisErrorMessage(message)
	if errorType == homeerrors.TypeError {
		return fmt.Errorf("%s", message)
	}

	statusCode := 0
	if errorType == "authentication_error" || errorType == "unauthorized" {
		statusCode = http.StatusUnauthorized
	}
	return &coreauth.Error{
		Code:       errorType,
		Message:    errorMessage,
		HTTPStatus: statusCode,
	}
}

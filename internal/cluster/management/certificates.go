package management

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
)

// CreateClientCertificate creates a pending client certificate and returns its Home JWT.
func (h *Handler) CreateClientCertificate(c *gin.Context) {
	if c == nil {
		return
	}
	if h == nil || h.repo == nil {
		respondError(c, http.StatusServiceUnavailable, "cluster_unavailable", nil)
		return
	}
	ctx, cancel := h.requestContext(c)
	defer cancel()
	request, errDecode := decodeCPANodeNameRequest(c)
	if errDecode != nil {
		respondCPANodeNameRequestError(c, errDecode)
		return
	}
	nodeName, errNodeName := requestedCPANodeName(request, false)
	if errNodeName != nil {
		if errors.Is(errNodeName, cluster.ErrInvalidCPANodeName) {
			respondError(c, http.StatusUnprocessableEntity, "cpa_node_name_invalid", errNodeName)
		} else {
			respondError(c, http.StatusBadRequest, "invalid_request", errNodeName)
		}
		return
	}

	ip, port := h.certificateJWTTarget(ctx)
	if strings.TrimSpace(ip) == "" || port <= 0 {
		respondError(c, http.StatusInternalServerError, "certificate_jwt_target_invalid", nil)
		return
	}
	certificateID, enrollmentSecret, errCreate := h.repo.CreatePendingClientCertificate(ctx, nodeName)
	if errCreate != nil {
		if errors.Is(errCreate, cluster.ErrInvalidCPANodeName) {
			respondError(c, http.StatusUnprocessableEntity, "cpa_node_name_invalid", errCreate)
			return
		}
		respondError(c, http.StatusInternalServerError, "certificate_create_failed", errCreate)
		return
	}
	token, errJWT := h.repo.CreateHomeJWT(ctx, certificateID, ip, port, enrollmentSecret)
	if errJWT != nil {
		respondError(c, http.StatusInternalServerError, "certificate_jwt_failed", errJWT)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"id":        certificateID,
		"home_jwt":  token,
		"node_name": emptyStringAsNil(nodeName),
	})
}

func (h *Handler) certificateJWTTarget(ctx context.Context) (string, int) {
	if h == nil {
		return "", 0
	}
	ip := strings.TrimSpace(h.nodeIP)
	port := h.nodePort
	if h.repo == nil {
		return ip, port
	}
	node, errNode := h.repo.CurrentMasterNode(ctx)
	if errNode == nil && node != nil {
		if strings.TrimSpace(node.IP) != "" && node.Port > 0 {
			return strings.TrimSpace(node.IP), node.Port
		}
	}
	return ip, port
}

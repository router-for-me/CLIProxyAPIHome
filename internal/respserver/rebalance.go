package respserver

import (
	"context"

	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
)

type clusterRebalanceHandler interface {
	HandleRebalance(context.Context, cluster.ConnectionLifetime, []string) ([]byte, error)
	RebalanceInstruction(context.Context, cluster.ConnectionLifetime) ([]byte, error)
}

func (s *Server) writeRebalanceInstruction(ctx context.Context, lifetime cluster.ConnectionLifetime, writer *safeWriter) error {
	handler, supported := s.cluster.(clusterRebalanceHandler)
	if !supported || lifetime.Fingerprint == "" || lifetime.ConnectedAt.IsZero() {
		return nil
	}
	payload, errInstruction := handler.RebalanceInstruction(ctx, lifetime)
	if errInstruction != nil || len(payload) == 0 {
		return errInstruction
	}
	return writer.WriteDispatchReply(subscriptionMessage("rebalance", payload))
}

package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
	"github.com/router-for-me/CLIProxyAPIHome/internal/respserver"
)

type startupConfigRepository interface {
	LoadConfigAsRuntimeConfig(context.Context) (*config.Config, []byte, error)
	MaxEventID(context.Context) (int64, error)
}

func loadInitialRuntimeConfig(ctx context.Context, repo startupConfigRepository) (int64, *config.Config, error) {
	if repo == nil {
		return 0, nil, fmt.Errorf("startup config repository is unavailable")
	}
	highWater, errHighWater := repo.MaxEventID(ctx)
	if errHighWater != nil {
		return 0, nil, fmt.Errorf("get startup event high-water: %w", errHighWater)
	}
	cfg, _, errConfig := repo.LoadConfigAsRuntimeConfig(ctx)
	if errConfig != nil {
		return 0, nil, fmt.Errorf("load runtime config after event high-water: %w", errConfig)
	}
	return highWater, cfg, nil
}

func handleCPAFenceEvent(ctx context.Context, event cluster.ClusterEventRecord, repo *cluster.Repository, coordinator *cluster.Coordinator, server *respserver.Server) error {
	if coordinator == nil {
		return fmt.Errorf("CPA fence handler is unavailable")
	}
	home, initialized := coordinator.HomeIncarnation()
	if !initialized {
		return fmt.Errorf("Home incarnation is not initialized")
	}
	return handleCPAFence(ctx, event, repo, home, server)
}

func handleCPAFence(ctx context.Context, event cluster.ClusterEventRecord, repo *cluster.Repository, home cluster.HomeIncarnationID, server *respserver.Server) error {
	if repo == nil || server == nil {
		return fmt.Errorf("CPA fence handler is unavailable")
	}
	connectedAt, errConnectedAt := time.Parse(time.RFC3339Nano, strings.TrimSpace(event.Op))
	if errConnectedAt != nil || connectedAt.IsZero() || strings.TrimSpace(event.EntityUUID) == "" || event.Version <= 0 {
		return fmt.Errorf("invalid CPA fence event")
	}
	lifetime := cluster.ConnectionLifetime{Fingerprint: strings.TrimSpace(event.EntityUUID), ConnectedAt: connectedAt}
	if errFence := server.FenceFingerprint(ctx, lifetime, event.Version, func() error {
		return repo.AcknowledgeQuiescence(ctx, lifetime.Fingerprint, lifetime.ConnectedAt, event.Version, home)
	}); errFence != nil {
		if errors.Is(errFence, cluster.ErrQuiescenceRevisionMismatch) {
			return nil
		}
		return errFence
	}
	if errComplete := repo.CompleteFingerprintCancellation(ctx, lifetime.Fingerprint, lifetime.ConnectedAt, event.Version); errComplete != nil &&
		!errors.Is(errComplete, cluster.ErrQuiescenceRevisionMismatch) &&
		!errors.Is(errComplete, cluster.ErrFingerprintNotQuiescent) &&
		!errors.Is(errComplete, cluster.ErrFingerprintQuiescenceSetIncomplete) {
		return errComplete
	}
	return nil
}

func recoverCPAFences(ctx context.Context, repo *cluster.Repository, home cluster.HomeIncarnationID, server *respserver.Server) error {
	if repo == nil || server == nil {
		return fmt.Errorf("CPA fence recovery is unavailable")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	// A membership that cannot be recovered now must not starve this Home's pending fences.
	errRecover := repo.RecoverStaleQuiescence(ctx)
	if errContext := ctx.Err(); errContext != nil {
		return errors.Join(errRecover, errContext)
	}
	rows, errRows := repo.ListPendingQuiescence(ctx, home)
	if errRows != nil {
		return errors.Join(errRecover, errRows)
	}
	recoveryErrors := []error{errRecover}
	for _, row := range rows {
		if errContext := ctx.Err(); errContext != nil {
			return errors.Join(append(recoveryErrors, errContext)...)
		}
		event := cluster.ClusterEventRecord{
			Scope:      "cpa-fence",
			Op:         row.MembershipConnectedAt.UTC().Format(time.RFC3339Nano),
			EntityUUID: row.CertificateFingerprint,
			Version:    row.CancelRevision,
		}
		if errFence := handleCPAFence(ctx, event, repo, home, server); errFence != nil {
			recoveryErrors = append(recoveryErrors, fmt.Errorf("recover CPA fence %s: %w", row.CertificateFingerprint, errFence))
		}
	}
	return errors.Join(recoveryErrors...)
}

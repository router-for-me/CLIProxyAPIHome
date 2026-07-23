package cluster

import (
	"errors"
	"time"

	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
)

type InFlightFrameKind string
type InFlightAccountedStatus string

const (
	InFlightFramePart     InFlightFrameKind       = "part"
	InFlightFrameOverflow InFlightFrameKind       = "overflow"
	InFlightAccounted     InFlightAccountedStatus = "accounted"
	InFlightUnaccounted   InFlightAccountedStatus = "unaccounted"
)

type InFlightAggregate struct {
	CredentialID string                  `json:"credential_id"`
	Model        string                  `json:"model"`
	Status       InFlightAccountedStatus `json:"status"`
	Count        int64                   `json:"count"`
}

type InFlightRequestDetail struct {
	RequestID    string    `json:"request_id"`
	CredentialID string    `json:"credential_id"`
	Model        string    `json:"model"`
	RequestKind  string    `json:"request_kind"`
	StartedAt    time.Time `json:"started_at"`
}

type InFlightSnapshotFrame struct {
	Kind                InFlightFrameKind       `json:"kind"`
	Revision            int64                   `json:"revision"`
	ObservedAt          time.Time               `json:"observed_at"`
	BarrierRevision     int64                   `json:"barrier_revision"`
	PartIndex           *int                    `json:"part_index,omitempty"`
	PartCount           *int                    `json:"part_count,omitempty"`
	DetailsTruncated    bool                    `json:"details_truncated,omitempty"`
	Aggregates          []InFlightAggregate     `json:"aggregates,omitempty"`
	Details             []InFlightRequestDetail `json:"details,omitempty"`
	AggregateGroupCount int                     `json:"aggregate_group_count,omitempty"`
}

// InFlightIngestIdentity identifies the active CPA membership submitting a frame.
type InFlightIngestIdentity struct {
	CertificateFingerprint string
	NodeID                 string
	MembershipConnectedAt  time.Time
}

// InFlightIngestResult reports the outcome of an in-flight frame submission.
type InFlightIngestResult struct {
	Accepted  bool
	Published bool
	Revision  int64
	State     string
}

// InFlightSnapshotPayload is the complete, visible payload for one revision.
type InFlightSnapshotPayload struct {
	Aggregates       []InFlightAggregate     `json:"aggregates"`
	Details          []InFlightRequestDetail `json:"details"`
	DetailsTruncated bool                    `json:"details_truncated"`
}

var (
	ErrInFlightLifetimeMismatch = errors.New("in-flight snapshot lifetime mismatch")
	ErrInFlightFrameInvalid     = errors.New("invalid in-flight snapshot frame")
	ErrInFlightRevisionStale    = errors.New("stale in-flight snapshot revision")
	ErrInFlightRevisionConflict = errors.New("conflicting in-flight snapshot revision")
	ErrInFlightRevisionOverflow = errors.New("in-flight snapshot revision overflow")
)

// InFlightLimits bounds in-flight snapshot ingestion.
type InFlightLimits struct {
	MaxPartBytes       int
	MaxPartCount       int
	MaxRevisionBytes   int
	MaxAggregateGroups int
	MaxDetails         int
	MaxStringBytes     int
}

// DefaultInFlightLimits returns the default in-flight snapshot ingestion bounds.
func DefaultInFlightLimits() InFlightLimits {
	return InFlightLimits{
		MaxPartBytes:       config.DefaultInFlightMaxPartBytes,
		MaxPartCount:       config.DefaultInFlightMaxPartCount,
		MaxRevisionBytes:   config.DefaultInFlightMaxRevisionBytes,
		MaxAggregateGroups: config.DefaultInFlightMaxAggregateGroups,
		MaxDetails:         config.DefaultInFlightMaxDetails,
		MaxStringBytes:     config.DefaultInFlightMaxStringBytes,
	}
}

// InFlightLimitsFromConfig converts validated observation config into ingest limits.
func InFlightLimitsFromConfig(cfg config.CredentialInFlightConfig) (InFlightLimits, error) {
	if errValidate := cfg.Validate(); errValidate != nil {
		return InFlightLimits{}, errValidate
	}
	return InFlightLimits{
		MaxPartBytes:       cfg.MaxPartBytes,
		MaxPartCount:       cfg.MaxPartCount,
		MaxRevisionBytes:   cfg.MaxRevisionBytes,
		MaxAggregateGroups: cfg.MaxAggregateGroups,
		MaxDetails:         cfg.MaxDetails,
		MaxStringBytes:     cfg.MaxStringBytes,
	}, nil
}

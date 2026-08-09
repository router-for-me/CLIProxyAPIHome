package cluster

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const CPANodeNameMaxLength = 128

var (
	ErrInvalidCPANodeName = errors.New("CPA node_name is invalid")
	ErrCPANodeNotFound    = errors.New("CPA node not found")
)

// NormalizeCPANodeName trims and validates an operator-provided CPA node name.
func NormalizeCPANodeName(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	if !utf8.ValidString(value) {
		return "", fmt.Errorf("%w: invalid UTF-8", ErrInvalidCPANodeName)
	}
	if utf8.RuneCountInString(value) > CPANodeNameMaxLength {
		return "", fmt.Errorf("%w: length exceeds %d characters", ErrInvalidCPANodeName, CPANodeNameMaxLength)
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return "", fmt.Errorf("%w: control characters are not allowed", ErrInvalidCPANodeName)
		}
	}
	return value, nil
}

// ListCPANodeNames returns operator-provided names keyed by CPA node ID.
func (r *Repository) ListCPANodeNames(ctx context.Context, nodeIDs []string) (map[string]string, error) {
	normalizedIDs := normalizeCPANodeIDs(nodeIDs)
	names := make(map[string]string, len(normalizedIDs))
	if len(normalizedIDs) == 0 {
		return names, nil
	}
	db, errDB := r.database()
	if errDB != nil {
		return nil, errDB
	}
	var records []CPANodeMetadataRecord
	if errFind := db.WithContext(contextOrBackground(ctx)).
		Select("node_id", "node_name").
		Where("node_id IN ?", normalizedIDs).
		Find(&records).Error; errFind != nil {
		return nil, errFind
	}
	for _, record := range records {
		nodeID := strings.TrimSpace(record.NodeID)
		nodeName := strings.TrimSpace(record.NodeName)
		if nodeID != "" && nodeName != "" {
			names[nodeID] = nodeName
		}
	}
	return names, nil
}

// UpdateCPANodeName sets or clears the name of an existing CPA certificate.
func (r *Repository) UpdateCPANodeName(ctx context.Context, nodeID string, nodeName string) (string, error) {
	nodeID = strings.TrimSpace(nodeID)
	if nodeID == "" {
		return "", ErrCPANodeNotFound
	}
	normalizedName, errName := NormalizeCPANodeName(nodeName)
	if errName != nil {
		return "", errName
	}
	db, errDB := r.database()
	if errDB != nil {
		return "", errDB
	}
	ctx = contextOrBackground(ctx)
	var storedName string
	errTransaction := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		certificate := CertificateRecord{}
		errCertificate := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND is_client = ? AND is_ca = ? AND is_server = ?", nodeID, true, false, false).
			First(&certificate).Error
		if errors.Is(errCertificate, gorm.ErrRecordNotFound) {
			return ErrCPANodeNotFound
		}
		if errCertificate != nil {
			return errCertificate
		}

		if normalizedName == "" {
			if errDelete := tx.Where("node_id = ?", nodeID).Delete(&CPANodeMetadataRecord{}).Error; errDelete != nil {
				return errDelete
			}
			storedName = ""
			return nil
		}

		now, errNow := DatabaseNow(ctx, tx)
		if errNow != nil {
			return errNow
		}
		metadata := CPANodeMetadataRecord{}
		errMetadata := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("node_id = ?", nodeID).
			First(&metadata).Error
		switch {
		case errors.Is(errMetadata, gorm.ErrRecordNotFound):
			metadata = CPANodeMetadataRecord{
				NodeID:    nodeID,
				NodeName:  normalizedName,
				CreatedAt: now,
				UpdatedAt: now,
			}
			if errCreate := tx.Create(&metadata).Error; errCreate != nil {
				return errCreate
			}
		case errMetadata != nil:
			return errMetadata
		default:
			if errUpdate := tx.Model(&metadata).Updates(map[string]any{
				"node_name":  normalizedName,
				"updated_at": now,
			}).Error; errUpdate != nil {
				return errUpdate
			}
		}
		storedName = normalizedName
		return nil
	})
	if errTransaction != nil {
		return "", errTransaction
	}
	return storedName, nil
}

func normalizeCPANodeIDs(nodeIDs []string) []string {
	normalized := make([]string, 0, len(nodeIDs))
	seen := make(map[string]struct{}, len(nodeIDs))
	for _, nodeID := range nodeIDs {
		nodeID = strings.TrimSpace(nodeID)
		if nodeID == "" {
			continue
		}
		if _, ok := seen[nodeID]; ok {
			continue
		}
		seen[nodeID] = struct{}{}
		normalized = append(normalized, nodeID)
	}
	return normalized
}

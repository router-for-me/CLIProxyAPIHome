package cluster

import (
	"fmt"
	"time"

	"github.com/google/uuid"
	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"gorm.io/gorm"
)

// compareAndSwapPostgresSessionBindingTx uses one atomic statement, except for
// tracked cold bindings, which require two statements in the caller's transaction.
// Cold and warm commits both lock the binding before its shared counter.
func compareAndSwapPostgresSessionBindingTx(tx *gorm.DB, commit coreauth.SessionAffinityCommit) (bool, error) {
	// PostgreSQL stores microseconds; round positive sub-microsecond TTLs up.
	ttlMicros := commit.TTL.Microseconds()
	if commit.TTL%time.Microsecond != 0 {
		ttlMicros++
	}
	revision := uuid.NewString()
	if commit.ExpectedRevision == "" || commit.SessionKey == "" {
		var query string
		var args []any
		if commit.ExpectedRevision == "" {
			// Claim missing rows before touching the counter, matching the lock
			// order of warm commits even when another dispatch wins the insert.
			query = `INSERT INTO session_affinity (key, credential_id, revision, expires_at)
				VALUES (?, ?, ?, clock_timestamp() + (? * INTERVAL '1 microsecond'))
				ON CONFLICT (key) DO NOTHING RETURNING key`
			args = []any{commit.Key, commit.AuthID, revision, ttlMicros}
		} else {
			query = `UPDATE session_affinity SET
				revision = CASE WHEN credential_id = ? AND expires_at > clock_timestamp() THEN revision ELSE ? END,
				credential_id = ?, expires_at = clock_timestamp() + (? * INTERVAL '1 microsecond'),
				session_key = '', provider = '', selection_sequence = 0
				WHERE key = ? AND revision = ?`
			args = []any{commit.AuthID, revision, commit.AuthID, ttlMicros, commit.Key, commit.ExpectedRevision}
			if !commit.ExpiredAt.IsZero() {
				query += ` AND expires_at = ?`
				args = append(args, commit.ExpiredAt.UTC())
			}
			query += ` RETURNING key`
		}
		var claimed struct{ Key string }
		result := tx.Raw(query, args...).Scan(&claimed)
		if result.Error != nil || result.RowsAffected != 1 {
			return false, result.Error
		}
		if commit.SessionKey == "" {
			return true, nil
		}
	}

	lease := `SELECT key, clock_timestamp() AS now, (? * INTERVAL '1 microsecond') AS ttl
		FROM session_affinity WHERE key = ?`
	args := []any{ttlMicros, commit.Key}
	if commit.ExpectedRevision != "" {
		lease += ` AND revision = ?`
		args = append(args, commit.ExpectedRevision)
		if !commit.ExpiredAt.IsZero() {
			lease += ` AND expires_at = ?`
			args = append(args, commit.ExpiredAt.UTC())
		}
	}
	lease += ` FOR UPDATE`

	// The sequence INSERT depends on the locked lease row: a stale CAS produces
	// no row and therefore cannot advance the sequence. FOR UPDATE prevents CTE
	// inlining, and both leases use its single database timestamp. The sequence
	// and binding update commit atomically, including within admission transactions.
	args = append(args, sessionAffinitySequenceKey(commit.SessionKey), commit.AuthID, revision, commit.AuthID, commit.SessionKey, commit.Provider)
	result := tx.Exec(`WITH lease AS (`+lease+`), next_selection AS (
		INSERT INTO session_affinity AS counter (key, credential_id, revision, selection_sequence, expires_at)
		SELECT ?, '', '', 1, lease.now + lease.ttl FROM lease
		ON CONFLICT (key) DO UPDATE SET
			selection_sequence = counter.selection_sequence + 1,
			expires_at = GREATEST(counter.expires_at, EXCLUDED.expires_at)
		RETURNING selection_sequence
	)
	UPDATE session_affinity SET
		revision = CASE WHEN credential_id = ? AND expires_at > lease.now THEN revision ELSE ? END,
		credential_id = ?, expires_at = lease.now + lease.ttl,
		session_key = ?, provider = ?, selection_sequence = next_selection.selection_sequence
	FROM next_selection, lease WHERE session_affinity.key = lease.key`, args...)
	if result.Error != nil {
		return false, result.Error
	}
	if result.RowsAffected != 1 && commit.ExpectedRevision == "" {
		return false, fmt.Errorf("session affinity binding disappeared after locking")
	}
	return result.RowsAffected == 1, nil
}

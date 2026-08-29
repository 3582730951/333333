package storage

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// The marker is written only after every pre-existing SQLite row has been
// visited. New writes already pass through compressContextPayload, so rows
// created while this deferred migration is running need no second pass.
const contextPayloadCompressionMigrationMarker = "context_payload_compression_ctx2"

const (
	contextCompressionBatchRows  = 64
	contextCompressionBatchBytes = 8 << 20
)

type contextCompressionRow struct {
	rowID      int64
	goalID     string
	payload    string
	payloadAlt string
	hash       string
	plainBytes int64
}

func (s *Store) sealCompressedContext(payload string) (string, error) {
	if int64(len(payload)) > maxStoredContextPayloadBytes {
		return "", fmt.Errorf("context payload contains %d bytes, limit is %d", len(payload), maxStoredContextPayloadBytes)
	}
	durable := compressContextPayload(payload)
	if decoded, err := decompressContextPayloadChecked(durable, maxStoredContextPayloadBytes); err != nil {
		return "", fmt.Errorf("verify compressed context: %w", err)
	} else if decoded != payload {
		return "", errors.New("compressed context failed byte-exact verification")
	}
	sealed := s.sealToken(durable)
	if durable != "" && sealed == "" {
		if err := s.CryptoError(); err != nil {
			return "", err
		}
		return "", errors.New("compressed context encryption returned an empty payload")
	}
	return sealed, nil
}

func storedContextPayloadForMigration(stored string, maxBytes int64) (string, error) {
	switch {
	case stored == "":
		return "", nil
	case strings.HasPrefix(stored, compressedContextPrefix), strings.HasPrefix(stored, rawContextPrefix):
		if _, err := decompressContextPayloadChecked(stored, maxBytes); err == nil {
			return stored, nil
		}
		// ctx2 did not exist before this migration. A malformed look-alike can
		// therefore be preserved as literal legacy data instead of blocking all
		// remaining rows.
		if int64(len(stored)) > maxBytes {
			return "", fmt.Errorf("legacy context payload contains %d bytes, limit is %d", len(stored), maxBytes)
		}
		return compressContextPayload(stored), nil
	case strings.HasPrefix(stored, legacyCompressedContextPrefix):
		decoded, err := decompressContextPayloadChecked(stored, maxBytes)
		if err == nil {
			// Rewrite a valid legacy gzip into the length-bearing ctx2 envelope.
			return compressContextPayload(decoded), nil
		}
		// A malformed gz1 prefix may be literal pre-codec user data. Envelope it
		// as raw instead of making a single legacy collision block migration.
		if int64(len(stored)) > maxBytes {
			return "", fmt.Errorf("legacy context payload contains %d bytes, limit is %d", len(stored), maxBytes)
		}
		return compressContextPayload(stored), nil
	default:
		if int64(len(stored)) > maxBytes {
			return "", fmt.Errorf("legacy context payload contains %d bytes, limit is %d", len(stored), maxBytes)
		}
		return compressContextPayload(stored), nil
	}
}

func (s *Store) migrateGoalChunkCompression(ctx context.Context) error {
	var maxRowID int64
	if err := s.rdb.QueryRowContext(ctx, `SELECT COALESCE(MAX(rowid),0) FROM goal_payload_chunk`).Scan(&maxRowID); err != nil {
		return err
	}
	var after int64
	for after < maxRowID {
		if err := ctx.Err(); err != nil {
			return err
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, `SELECT rowid,goal_id,payload_hash,payload_bytes,encrypted_payload
FROM goal_payload_chunk WHERE rowid>? AND rowid<=? ORDER BY rowid LIMIT ?`, after, maxRowID, contextCompressionBatchRows)
		if err != nil {
			_ = tx.Rollback()
			return err
		}
		var batch []contextCompressionRow
		var scannedBytes int64
		for rows.Next() {
			var row contextCompressionRow
			if err = rows.Scan(&row.rowID, &row.goalID, &row.hash, &row.plainBytes, &row.payload); err != nil {
				_ = rows.Close()
				_ = tx.Rollback()
				return err
			}
			batch = append(batch, row)
			scannedBytes += int64(len(row.payload))
			after = row.rowID
			if scannedBytes >= contextCompressionBatchBytes {
				break
			}
		}
		if closeErr := rows.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			_ = tx.Rollback()
			return err
		}
		if len(batch) == 0 {
			_ = tx.Rollback()
			break
		}
		deltas := make(map[string]int64)
		for _, row := range batch {
			plain, openErr := s.openGoalChunkToken(row.payload)
			if openErr != nil {
				_ = tx.Rollback()
				return openErr
			}
			// Whole-payload gc2 rows are already compressed as one logical stream
			// and authenticated piece-by-piece.  The older ctx2 migration must leave
			// them untouched; attempting to decode each piece as an independent
			// context envelope would either fail startup or destroy the stream.
			if strings.HasPrefix(plain, goalChunkFormatV2Prefix) {
				continue
			}
			if row.plainBytes < 0 || row.plainBytes > goalPayloadChunkSize {
				_ = tx.Rollback()
				return fmt.Errorf("goal chunk rowid=%d declares invalid plaintext length %d", row.rowID, row.plainBytes)
			}
			decoded, decodeErr := decompressContextPayloadChecked(plain, goalPayloadChunkSize)
			valid := decodeErr == nil && int64(len(decoded)) == row.plainBytes && hashGoalPayload(decoded) == row.hash
			if !valid && int64(len(plain)) == row.plainBytes && hashGoalPayload(plain) == row.hash {
				// Any old literal that resembles a codec envelope is
				// distinguishable here by authoritative plaintext metadata.
				decoded, decodeErr = plain, nil
				valid = true
			}
			if !valid {
				_ = tx.Rollback()
				return fmt.Errorf("goal chunk rowid=%d failed plaintext length/hash verification: %v", row.rowID, decodeErr)
			}
			durable := compressContextPayload(decoded)
			if durable == plain {
				continue
			}
			sealed, sealErr := s.sealCompressedContext(decoded)
			if sealErr != nil {
				_ = tx.Rollback()
				return sealErr
			}
			result, updateErr := tx.ExecContext(ctx, `UPDATE goal_payload_chunk SET encrypted_payload=? WHERE rowid=? AND encrypted_payload=?`, sealed, row.rowID, row.payload)
			if updateErr != nil {
				_ = tx.Rollback()
				return updateErr
			}
			if changed, _ := result.RowsAffected(); changed == 1 {
				deltas[row.goalID] += int64(len(sealed) - len(row.payload))
			}
		}
		for goalID, delta := range deltas {
			if _, err = tx.ExecContext(ctx, `UPDATE goal_session SET storage_bytes=MAX(0,storage_bytes+?) WHERE id=?`, delta, goalID); err != nil {
				_ = tx.Rollback()
				return err
			}
		}
		if err = tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) migrateEncryptedContextTable(ctx context.Context, table, idColumn, payloadColumn string) error {
	// Identifiers are fixed internal constants supplied below, never user input.
	var maxRowID int64
	if err := s.rdb.QueryRowContext(ctx, `SELECT COALESCE(MAX(rowid),0) FROM `+table).Scan(&maxRowID); err != nil {
		return err
	}
	var after int64
	for after < maxRowID {
		if err := ctx.Err(); err != nil {
			return err
		}
		query := `SELECT rowid,` + idColumn + `,` + payloadColumn + ` FROM ` + table + ` WHERE rowid>? AND rowid<=? ORDER BY rowid LIMIT ?`
		rows, err := s.rdb.QueryContext(ctx, query, after, maxRowID, contextCompressionBatchRows)
		if err != nil {
			return err
		}
		var batch []contextCompressionRow
		var scannedBytes int64
		for rows.Next() {
			var row contextCompressionRow
			if err = rows.Scan(&row.rowID, &row.goalID, &row.payload); err != nil {
				_ = rows.Close()
				return err
			}
			batch = append(batch, row)
			scannedBytes += int64(len(row.payload))
			after = row.rowID
			if scannedBytes >= contextCompressionBatchBytes {
				break
			}
		}
		if err = rows.Close(); err != nil {
			return err
		}
		if len(batch) == 0 {
			break
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		for _, row := range batch {
			plain := s.openToken(row.payload)
			if err := s.CryptoError(); err != nil {
				_ = tx.Rollback()
				return err
			}
			durable, decodeErr := storedContextPayloadForMigration(plain, maxStoredContextPayloadBytes)
			if decodeErr != nil {
				_ = tx.Rollback()
				return fmt.Errorf("decode compressed %s rowid=%d: %w", table, row.rowID, decodeErr)
			}
			if durable == plain {
				continue
			}
			if _, verifyErr := decompressContextPayloadChecked(durable, maxStoredContextPayloadBytes); verifyErr != nil {
				_ = tx.Rollback()
				return fmt.Errorf("verify compressed %s rowid=%d: %w", table, row.rowID, verifyErr)
			} else {
				sealed := s.sealToken(durable)
				if durable != "" && sealed == "" {
					_ = tx.Rollback()
					if cryptoErr := s.CryptoError(); cryptoErr != nil {
						return cryptoErr
					}
					return errors.New("compressed context encryption returned an empty payload")
				}
				update := `UPDATE ` + table + ` SET ` + payloadColumn + `=? WHERE rowid=? AND ` + idColumn + `=? AND ` + payloadColumn + `=?`
				if _, err = tx.ExecContext(ctx, update, sealed, row.rowID, row.goalID, row.payload); err != nil {
					_ = tx.Rollback()
					return err
				}
			}
		}
		if err = tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) migrateVirtualLedgerCompression(ctx context.Context) error {
	var maxRowID int64
	if err := s.rdb.QueryRowContext(ctx, `SELECT COALESCE(MAX(rowid),0) FROM virtual_context_ledger`).Scan(&maxRowID); err != nil {
		return err
	}
	var after int64
	for after < maxRowID {
		if err := ctx.Err(); err != nil {
			return err
		}
		rows, err := s.rdb.QueryContext(ctx, `SELECT rowid,content,raw_json FROM virtual_context_ledger
WHERE rowid>? AND rowid<=? ORDER BY rowid LIMIT ?`, after, maxRowID, contextCompressionBatchRows)
		if err != nil {
			return err
		}
		var batch []contextCompressionRow
		var scannedBytes int64
		for rows.Next() {
			var row contextCompressionRow
			if err = rows.Scan(&row.rowID, &row.payload, &row.payloadAlt); err != nil {
				_ = rows.Close()
				return err
			}
			batch = append(batch, row)
			scannedBytes += int64(len(row.payload) + len(row.payloadAlt))
			after = row.rowID
			if scannedBytes >= contextCompressionBatchBytes {
				break
			}
		}
		if err = rows.Close(); err != nil {
			return err
		}
		if len(batch) == 0 {
			break
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		for _, row := range batch {
			content, contentErr := storedContextPayloadForMigration(row.payload, maxStoredContextPayloadBytes)
			if contentErr != nil {
				_ = tx.Rollback()
				return fmt.Errorf("decode virtual context content rowid=%d: %w", row.rowID, contentErr)
			}
			raw, rawErr := storedContextPayloadForMigration(row.payloadAlt, maxStoredContextPayloadBytes)
			if rawErr != nil {
				_ = tx.Rollback()
				return fmt.Errorf("decode virtual context raw_json rowid=%d: %w", row.rowID, rawErr)
			}
			if content == row.payload && raw == row.payloadAlt {
				continue
			}
			if _, err = tx.ExecContext(ctx, `UPDATE virtual_context_ledger SET content=?,raw_json=? WHERE rowid=? AND content=? AND raw_json=?`,
				content, raw, row.rowID, row.payload, row.payloadAlt); err != nil {
				_ = tx.Rollback()
				return err
			}
		}
		if err = tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) migrateStoredContextCompression(ctx context.Context) error {
	if s == nil || s.driver == "postgres" {
		return nil
	}
	var completed int
	if err := s.rdb.QueryRowContext(ctx, `SELECT COUNT(*) FROM settings WHERE key=?`, contextPayloadCompressionMigrationMarker).Scan(&completed); err != nil {
		return err
	}
	if completed > 0 {
		return nil
	}
	if err := s.migrateGoalChunkCompression(ctx); err != nil {
		return err
	}
	if err := s.migrateEncryptedContextTable(ctx, "goal_session", "id", "encrypted_working_state"); err != nil {
		return err
	}
	if err := s.migrateEncryptedContextTable(ctx, "context_journal", "response_id", "encrypted_payload"); err != nil {
		return err
	}
	if err := s.migrateVirtualLedgerCompression(ctx); err != nil {
		return err
	}
	now := Now()
	_, err := s.db.ExecContext(ctx, `INSERT INTO settings(key,value,updated_at) VALUES(?,?,?) ON CONFLICT(key) DO NOTHING`,
		contextPayloadCompressionMigrationMarker, "1", now)
	return err
}

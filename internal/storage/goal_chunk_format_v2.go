package storage

// Opt-in whole-payload goal chunk codec.
//
// The first goal implementation compressed and encrypted every 64 KiB slice
// independently. This file adds a versioned representation that compresses one
// logical payload once, then splits the durable stream into independently
// authenticated pieces. The setting is deliberately off by default; readers
// remain compatible with both representations so a rollout can be stopped at
// any point without rewriting the database back.

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"codex-account-pool/internal/secretbox"
)

// GoalChunkFormatV2Setting is the runtime settings-table key used for the
// lossless whole-payload goal chunk codec.
const GoalChunkFormatV2Setting = "goal_chunk_format_v2"

const (
	// gc2 is stored inside the plaintext of each encrypted piece. Looking at the
	// decrypted value (rather than the SQL value) keeps the marker useful when
	// the store has a master key configured.
	goalChunkFormatV2Prefix = "gc2:"
	// gcs2 carries the original byte length, the original byte hash, and one
	// ctx2-compressed durable payload. The durable field is last because ctx2 and
	// legacy gzip envelopes can contain colons themselves.
	goalChunkStreamV2Prefix       = "gcs2:"
	goalChunkFormatV2HeaderFields = 3
)

// goalChunkFormatV2Enabled reads the process-wide rollout switch. The persisted
// runtime setting takes precedence over the boot-config fallback.
func (s *Store) goalChunkFormatV2Enabled(ctx context.Context) bool {
	if s == nil {
		return false
	}
	value, ok, err := s.GetSetting(ctx, GoalChunkFormatV2Setting)
	if err != nil || !ok {
		return s.goalChunkFormatV2Default
	}
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "on", "yes":
		return true
	default:
		return false
	}
}

func (s *Store) prepareGoalChunksWithFormat(payload string, formatV2 bool) ([]preparedGoalChunk, int64) {
	if formatV2 {
		return s.prepareGoalChunksV2(payload)
	}
	return s.prepareGoalChunksLegacy(payload)
}

// prepareGoalChunksLegacy is the deployed per-chunk representation.
func (s *Store) prepareGoalChunksLegacy(payload string) ([]preparedGoalChunk, int64) {
	if payload == "" {
		return nil, 0
	}
	chunks := make([]preparedGoalChunk, 0, (len(payload)+goalPayloadChunkSize-1)/goalPayloadChunkSize)
	var stored int64
	for offset, index := 0, 0; offset < len(payload); index++ {
		end := offset + goalPayloadChunkSize
		if end > len(payload) {
			end = len(payload)
		}
		part := payload[offset:end]
		encrypted := s.sealToken(compressContextPayload(part))
		chunks = append(chunks, preparedGoalChunk{
			index: index, payloadHash: hashGoalPayload(part), plainBytes: len(part), encrypted: encrypted,
		})
		stored += int64(len(encrypted))
		offset = end
	}
	return chunks, stored
}

// prepareGoalChunksV2 compresses one logical payload exactly once, then splits
// the durable stream into bounded pieces. Each piece is independently sealed so
// a damaged SQL value fails authenticated verification instead of changing the
// replay bytes silently.
func (s *Store) prepareGoalChunksV2(payload string) ([]preparedGoalChunk, int64) {
	if payload == "" {
		return nil, 0
	}
	stream := goalChunkStreamV2(payload)
	pieceSize := goalPayloadChunkSize - len(goalChunkFormatV2Prefix)
	if pieceSize <= 0 {
		return nil, 0
	}
	chunks := make([]preparedGoalChunk, 0, (len(stream)+pieceSize-1)/pieceSize)
	var stored int64
	for offset, index := 0, 0; offset < len(stream); index++ {
		end := offset + pieceSize
		if end > len(stream) {
			end = len(stream)
		}
		piece := stream[offset:end]
		marked := goalChunkFormatV2Prefix + piece
		encrypted := s.sealToken(marked)
		chunks = append(chunks, preparedGoalChunk{
			index:       index,
			payloadHash: hashGoalPayload(piece),
			// In gc2 rows payload_bytes describes the durable stream piece,
			// not a slice of the original plaintext.
			plainBytes: len(piece),
			encrypted:  encrypted,
		})
		stored += int64(len(encrypted))
		offset = end
	}
	return chunks, stored
}

func goalChunkStreamV2(payload string) string {
	return goalChunkStreamV2Prefix + strconv.FormatInt(int64(len(payload)), 10) + ":" +
		hashGoalPayload(payload) + ":" + compressContextPayload(payload)
}

// openGoalChunkToken is the local, error-returning counterpart to openToken.
// openToken intentionally has a string-only API for legacy callers and records a
// process-wide CryptoError; using that error as a per-row result can mistake a
// stale error from an unrelated read for the current chunk. This helper preserves
// strict-encryption semantics while making V2 migration/read decisions exact.
func (s *Store) openGoalChunkToken(value string) (string, error) {
	if value == "" || len(s.tokenKey) == 0 {
		return value, nil
	}
	if s.cryptoStrict && !secretbox.IsSealed(value) {
		err := errors.New("plaintext secret encountered after encryption migration")
		s.recordCryptoError(err)
		return "", err
	}
	keys := s.tokenKeys
	if len(keys) == 0 {
		keys = [][]byte{s.tokenKey}
	}
	plain, err := secretbox.OpenDomainWithKeys(keys, secretbox.DefaultDomain, value)
	if err != nil {
		s.recordCryptoError(err)
		return "", err
	}
	return plain, nil
}

// decodeGoalChunkStreamV2 validates the stream header, decompresses the durable
// ctx2 envelope once, and checks both length and byte-for-byte hash.
func decodeGoalChunkStreamV2(stream string, maxBytes int64) (string, error) {
	if maxBytes < 0 {
		return "", errors.New("invalid goal chunk decompression limit")
	}
	if !strings.HasPrefix(stream, goalChunkStreamV2Prefix) {
		return "", errors.New("goal chunk v2 stream prefix is missing")
	}
	rest := strings.TrimPrefix(stream, goalChunkStreamV2Prefix)
	fields := make([]string, 0, goalChunkFormatV2HeaderFields)
	for len(fields) < goalChunkFormatV2HeaderFields-1 {
		colon := strings.IndexByte(rest, ':')
		if colon <= 0 {
			return "", errors.New("goal chunk v2 stream header is malformed")
		}
		fields = append(fields, rest[:colon])
		rest = rest[colon+1:]
	}
	fields = append(fields, rest)
	plainBytes, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil || plainBytes < 0 {
		return "", errors.New("goal chunk v2 stream plaintext length is invalid")
	}
	if plainBytes > maxBytes {
		return "", fmt.Errorf("goal chunk v2 stream declares %d bytes, limit is %d", plainBytes, maxBytes)
	}
	plainHash := strings.TrimSpace(fields[1])
	if len(plainHash) != 64 {
		return "", errors.New("goal chunk v2 stream plaintext hash is invalid")
	}
	durable := fields[2]
	if durable == "" {
		return "", errors.New("goal chunk v2 stream durable payload is empty")
	}
	decoded, err := decompressContextPayloadChecked(durable, maxBytes)
	if err != nil {
		return "", fmt.Errorf("decode goal chunk v2 durable payload: %w", err)
	}
	if int64(len(decoded)) != plainBytes {
		return "", fmt.Errorf("goal chunk v2 stream decoded %d bytes, expected %d", len(decoded), plainBytes)
	}
	if hashGoalPayload(decoded) != plainHash {
		return "", errors.New("goal chunk v2 stream failed plaintext hash verification")
	}
	return decoded, nil
}

type goalChunkReadRow struct {
	index       int
	payloadHash string
	plainBytes  int64
	encrypted   string // decrypted plaintext after readGoalChunkRows
}

// decodeGoalChunkRows validates a complete chunk group and returns its logical
// payload plus whether it used gc2. A gc2 marker in the first row is authoritative:
// malformed/mixed V2 groups are errors, never silently reinterpreted as legacy.
func (s *Store) decodeGoalChunkRows(rows []goalChunkReadRow, maxBytes int64) (string, bool, error) {
	if len(rows) == 0 {
		return "", false, ErrGoalNotFound
	}
	var stream bytes.Buffer
	firstV2 := strings.HasPrefix(rows[0].encrypted, goalChunkFormatV2Prefix)
	for index := range rows {
		row := &rows[index]
		if row.index != index {
			return "", firstV2, fmt.Errorf("goal payload chunk index %d is out of sequence (want %d)", row.index, index)
		}
		isV2 := strings.HasPrefix(row.encrypted, goalChunkFormatV2Prefix)
		if firstV2 {
			if !isV2 {
				return "", true, errors.New("goal chunk v2 group is missing a marker")
			}
			piece := strings.TrimPrefix(row.encrypted, goalChunkFormatV2Prefix)
			if row.plainBytes < 0 || row.plainBytes > int64(goalPayloadChunkSize-len(goalChunkFormatV2Prefix)) || int64(len(piece)) != row.plainBytes || hashGoalPayload(piece) != row.payloadHash {
				return "", true, fmt.Errorf("goal chunk v2 row %d failed piece length/hash verification", row.index)
			}
			if int64(stream.Len()+len(piece)) > maxBytes+goalPayloadChunkSize {
				return "", true, fmt.Errorf("goal chunk v2 stream exceeds %d-byte reconstruction limit", maxBytes)
			}
			stream.WriteString(piece)
			continue
		}
		if isV2 {
			return "", false, errors.New("legacy goal chunk group contains a V2 marker")
		}
		if row.plainBytes < 0 || row.plainBytes > goalPayloadChunkSize {
			return "", false, fmt.Errorf("goal payload chunk declares invalid plaintext length %d", row.plainBytes)
		}
		decoded, err := decompressContextPayloadChecked(row.encrypted, goalPayloadChunkSize)
		if err != nil {
			return "", false, fmt.Errorf("decode goal payload chunk: %w", err)
		}
		if int64(len(decoded)) != row.plainBytes || hashGoalPayload(decoded) != row.payloadHash {
			return "", false, errors.New("goal payload chunk failed plaintext length/hash verification")
		}
		if int64(stream.Len()+len(decoded)) > maxBytes {
			return "", false, fmt.Errorf("goal payload exceeds %d-byte reconstruction limit", maxBytes)
		}
		stream.WriteString(decoded)
	}
	if firstV2 {
		decoded, err := decodeGoalChunkStreamV2(stream.String(), maxBytes)
		return decoded, true, err
	}
	if stream.Len() == 0 {
		return "", false, ErrGoalNotFound
	}
	return stream.String(), false, nil
}

func (s *Store) readGoalChunksCompat(ctx context.Context, goalID, kind string, segmentSequence int64) (string, error) {
	rows, err := s.rdb.QueryContext(ctx, `SELECT chunk_index,payload_hash,payload_bytes,encrypted_payload
FROM goal_payload_chunk p JOIN goal_session s ON s.id=p.goal_id
WHERE p.goal_id=? AND p.payload_kind=? AND p.segment_sequence=? AND s.state<>'reclaiming'
ORDER BY p.chunk_index`, goalID, kind, segmentSequence)
	if err != nil {
		return "", err
	}
	decodedRows := make([]goalChunkReadRow, 0)
	for rows.Next() {
		var row goalChunkReadRow
		if err = rows.Scan(&row.index, &row.payloadHash, &row.plainBytes, &row.encrypted); err != nil {
			_ = rows.Close()
			return "", err
		}
		row.encrypted, err = s.openGoalChunkToken(row.encrypted)
		if err != nil {
			_ = rows.Close()
			return "", err
		}
		decodedRows = append(decodedRows, row)
	}
	if err = rows.Err(); err != nil {
		_ = rows.Close()
		return "", err
	}
	if err = rows.Close(); err != nil {
		return "", err
	}
	payload, _, err := s.decodeGoalChunkRows(decodedRows, maxStoredContextPayloadBytes)
	return payload, err
}

func verifyGoalPayloadMetadata(payload, expectedHash string, expectedBytes int64) error {
	if expectedHash != "" && hashGoalPayload(payload) != expectedHash {
		return errors.New("goal payload failed parent hash verification")
	}
	if expectedBytes >= 0 && int64(len(payload)) != expectedBytes {
		return fmt.Errorf("goal payload decoded to %d bytes, expected %d", len(payload), expectedBytes)
	}
	return nil
}

// migrateGoalChunkRowsToV2 rewrites one old chunk group after decoding and
// validating every original piece. It is called only by bounded background
// maintenance, never by the append hot path.
func (s *Store) migrateGoalChunkRowsToV2(ctx context.Context, tx *sql.Tx, goalID, kind string, sequence int64, expectedHash string, expectedBytes int64) (bool, int64, error) {
	rows, err := tx.QueryContext(ctx, `SELECT chunk_index,payload_hash,payload_bytes,encrypted_payload
FROM goal_payload_chunk WHERE goal_id=? AND payload_kind=? AND segment_sequence=? ORDER BY chunk_index`, goalID, kind, sequence)
	if err != nil {
		return false, 0, err
	}
	stored := make([]goalChunkReadRow, 0)
	for rows.Next() {
		var row goalChunkReadRow
		if err = rows.Scan(&row.index, &row.payloadHash, &row.plainBytes, &row.encrypted); err != nil {
			_ = rows.Close()
			return false, 0, err
		}
		row.encrypted, err = s.openGoalChunkToken(row.encrypted)
		if err != nil {
			_ = rows.Close()
			return false, 0, err
		}
		stored = append(stored, row)
	}
	if err = rows.Err(); err != nil {
		_ = rows.Close()
		return false, 0, err
	}
	if err = rows.Close(); err != nil {
		return false, 0, err
	}
	if len(stored) == 0 {
		return false, 0, nil
	}
	payload, isV2, err := s.decodeGoalChunkRows(stored, maxStoredContextPayloadBytes)
	if err != nil {
		return false, 0, fmt.Errorf("decode goal chunk group %s/%d: %w", kind, sequence, err)
	}
	if isV2 {
		return false, 0, nil
	}
	if err = verifyGoalPayloadMetadata(payload, expectedHash, expectedBytes); err != nil {
		return false, 0, err
	}
	chunks, storedBytes := s.prepareGoalChunksV2(payload)
	if decoded, verifyErr := decodePreparedGoalChunksV2(s, chunks, maxStoredContextPayloadBytes); verifyErr != nil || decoded != payload {
		if verifyErr != nil {
			return false, 0, fmt.Errorf("verify migrated goal chunks: %w", verifyErr)
		}
		return false, 0, errors.New("migrated goal chunks failed byte-exact verification")
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM goal_payload_chunk WHERE goal_id=? AND payload_kind=? AND segment_sequence=?`, goalID, kind, sequence); err != nil {
		return false, 0, err
	}
	inserted, err := s.insertPreparedGoalChunks(ctx, tx, goalID, kind, sequence, Now(), chunks)
	if err != nil {
		return false, 0, err
	}
	if inserted != storedBytes {
		storedBytes = inserted
	}
	return true, storedBytes, nil
}

func decodePreparedGoalChunksV2(s *Store, chunks []preparedGoalChunk, maxBytes int64) (string, error) {
	rows := make([]goalChunkReadRow, 0, len(chunks))
	for _, chunk := range chunks {
		plain, err := s.openGoalChunkToken(chunk.encrypted)
		if err != nil {
			return "", err
		}
		rows = append(rows, goalChunkReadRow{index: chunk.index, payloadHash: chunk.payloadHash, plainBytes: int64(chunk.plainBytes), encrypted: plain})
	}
	payload, isV2, err := s.decodeGoalChunkRows(rows, maxBytes)
	if err != nil {
		return "", err
	}
	if !isV2 {
		return "", errors.New("prepared chunks are not gc2")
	}
	return payload, nil
}

// MigrateGoalChunkFormatV2 performs one atomic, lossless migration for a goal.
// It handles old whole-value rows, deployed per-chunk rows, and compacted groups
// whose parent segment row has already been deleted. FormatVersion=2 denotes the
// original chunked schema and is therefore not sufficient to identify gc2; the
// decrypted marker is the codec discriminator.
func (s *Store) MigrateGoalChunkFormatV2(ctx context.Context, goalID string) (bool, error) {
	goalID = strings.TrimSpace(goalID)
	if goalID == "" {
		return false, errors.New("goal id is empty")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	locked, err := tx.ExecContext(ctx, `UPDATE goal_session SET updated_at=updated_at WHERE id=? AND state<>'reclaiming'`, goalID)
	if err != nil {
		return false, err
	}
	if affected, _ := locked.RowsAffected(); affected != 1 {
		return false, ErrGoalNotFound
	}

	type parentRow struct {
		kind, id, hash, encrypted string
		sequence, plainBytes      int64
	}
	parents := make([]parentRow, 0)
	rows, err := tx.QueryContext(ctx, `SELECT 'checkpoint',c.id,0,c.payload_hash,c.payload_bytes,c.encrypted_payload
FROM goal_session s JOIN goal_checkpoint c ON c.id=s.current_checkpoint_id WHERE s.id=?
UNION ALL
SELECT 'segment',g.id,g.sequence,g.payload_hash,g.payload_bytes,g.encrypted_payload
FROM goal_segment g WHERE g.goal_id=? ORDER BY 1,3`, goalID, goalID)
	if err != nil {
		return false, err
	}
	for rows.Next() {
		var row parentRow
		if err = rows.Scan(&row.kind, &row.id, &row.sequence, &row.hash, &row.plainBytes, &row.encrypted); err != nil {
			_ = rows.Close()
			return false, err
		}
		parents = append(parents, row)
	}
	if err = rows.Err(); err != nil {
		_ = rows.Close()
		return false, err
	}
	if err = rows.Close(); err != nil {
		return false, err
	}

	type groupKey struct {
		kind string
		seq  int64
	}
	groups := make([]groupKey, 0)
	groupSet := make(map[groupKey]bool)
	rows, err = tx.QueryContext(ctx, `SELECT payload_kind,segment_sequence FROM goal_payload_chunk WHERE goal_id=? GROUP BY payload_kind,segment_sequence ORDER BY payload_kind,segment_sequence`, goalID)
	if err != nil {
		return false, err
	}
	for rows.Next() {
		var key groupKey
		if err = rows.Scan(&key.kind, &key.seq); err != nil {
			_ = rows.Close()
			return false, err
		}
		groups = append(groups, key)
		groupSet[key] = true
	}
	if err = rows.Err(); err != nil {
		_ = rows.Close()
		return false, err
	}
	if err = rows.Close(); err != nil {
		return false, err
	}

	changed := false
	// Convert old whole-value rows that do not already have a chunk group. The
	// parent payload uses the normal context envelope, so this also handles old
	// rows that were written after gzip compression was introduced.
	for _, parent := range parents {
		key := groupKey{kind: parent.kind, seq: parent.sequence}
		if parent.kind == "checkpoint" {
			key.seq = 0
		}
		if groupSet[key] || strings.TrimSpace(parent.encrypted) == "" {
			continue
		}
		plain, openErr := s.openContextPayload(parent.encrypted, maxStoredContextPayloadBytes)
		if openErr != nil {
			return false, fmt.Errorf("decode legacy %s %s: %w", parent.kind, parent.id, openErr)
		}
		if err = verifyGoalPayloadMetadata(plain, parent.hash, parent.plainBytes); err != nil {
			return false, err
		}
		chunks, _ := s.prepareGoalChunksV2(plain)
		if decoded, verifyErr := decodePreparedGoalChunksV2(s, chunks, maxStoredContextPayloadBytes); verifyErr != nil || decoded != plain {
			if verifyErr != nil {
				return false, fmt.Errorf("verify migrated %s %s: %w", parent.kind, parent.id, verifyErr)
			}
			return false, errors.New("migrated legacy goal payload changed bytes")
		}
		if _, err = tx.ExecContext(ctx, `UPDATE `+parent.kind+` SET encrypted_payload='',format_version=? WHERE id=?`, goalPayloadFormatV2, parent.id); err != nil {
			return false, err
		}
		if _, err = s.insertPreparedGoalChunks(ctx, tx, goalID, parent.kind, key.seq, Now(), chunks); err != nil {
			return false, err
		}
		changed = true
	}

	for _, key := range groups {
		var parentHash string
		var parentBytes int64 = -1
		for _, parent := range parents {
			if parent.kind == key.kind && ((key.kind == "checkpoint" && key.seq == 0) || parent.sequence == key.seq) {
				parentHash, parentBytes = parent.hash, parent.plainBytes
				break
			}
		}
		migrated, _, migrateErr := s.migrateGoalChunkRowsToV2(ctx, tx, goalID, key.kind, key.seq, parentHash, parentBytes)
		if migrateErr != nil {
			return false, migrateErr
		}
		changed = changed || migrated
	}
	if changed {
		if _, err = tx.ExecContext(ctx, `UPDATE goal_session SET storage_bytes=
COALESCE((SELECT SUM(LENGTH(encrypted_payload)) FROM goal_checkpoint WHERE goal_id=goal_session.id),0)+
COALESCE((SELECT SUM(LENGTH(encrypted_payload)) FROM goal_segment WHERE goal_id=goal_session.id),0)+
COALESCE((SELECT SUM(LENGTH(encrypted_payload)) FROM goal_payload_chunk WHERE goal_id=goal_session.id),0),updated_at=updated_at WHERE id=?`, goalID); err != nil {
			return false, err
		}
	}
	if err = tx.Commit(); err != nil {
		return false, err
	}
	return changed, nil
}

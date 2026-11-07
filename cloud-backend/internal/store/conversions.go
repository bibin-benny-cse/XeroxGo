package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// ConversionStatus represents the lifecycle state of an asynchronous conversion job.
type ConversionStatus string

const (
	ConversionStatusQueued     ConversionStatus = "queued"
	ConversionStatusProcessing ConversionStatus = "processing"
	ConversionStatusCompleted  ConversionStatus = "completed"
	ConversionStatusFailed     ConversionStatus = "failed"
)

// IsValidConversionStatus validates whether status string is an accepted ConversionStatus.
func IsValidConversionStatus(s ConversionStatus) bool {
	switch s {
	case ConversionStatusQueued, ConversionStatusProcessing, ConversionStatusCompleted, ConversionStatusFailed:
		return true
	default:
		return false
	}
}

// ConversionJob encapsulates the state and metadata of an Office document conversion job.
type ConversionJob struct {
	ID               string           `json:"job_id"`
	Token            string           `json:"token,omitempty"`
	OriginalFilename string           `json:"original_filename"`
	FileSize         int64            `json:"file_size"`
	Status           ConversionStatus `json:"status"`
	ErrorMessage     string           `json:"error_message,omitempty"`
	InputPath        string           `json:"-"`
	OutputPath       string           `json:"-"`
	CreatedAt        time.Time        `json:"created_at"`
	UpdatedAt        time.Time        `json:"updated_at"`
	ExpiresAt        time.Time        `json:"expires_at"`
}

// CreateConversion persists a new conversion job.
func (s *Store) CreateConversion(ctx context.Context, job ConversionJob) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("create conversion: store is closed or nil")
	}
	if ctx == nil {
		return fmt.Errorf("create conversion: context is nil")
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("create conversion: %w", err)
	}

	if strings.TrimSpace(job.ID) == "" {
		return fmt.Errorf("create conversion: id cannot be empty")
	}
	if strings.TrimSpace(job.Token) == "" {
		return fmt.Errorf("create conversion: token cannot be empty")
	}
	if strings.TrimSpace(job.OriginalFilename) == "" {
		return fmt.Errorf("create conversion: original_filename cannot be empty")
	}
	if strings.TrimSpace(job.InputPath) == "" {
		return fmt.Errorf("create conversion: input_path cannot be empty")
	}
	if !IsValidConversionStatus(job.Status) {
		return fmt.Errorf("create conversion: invalid status %q", job.Status)
	}

	createdAt := job.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	} else {
		createdAt = createdAt.UTC()
	}

	updatedAt := job.UpdatedAt
	if updatedAt.IsZero() {
		updatedAt = createdAt
	} else {
		updatedAt = updatedAt.UTC()
	}

	expiresAt := job.ExpiresAt
	if expiresAt.IsZero() {
		expiresAt = createdAt.Add(1 * time.Hour)
	} else {
		expiresAt = expiresAt.UTC()
	}

	createdNS := createdAt.UnixNano()
	updatedNS := updatedAt.UnixNano()
	expiresNS := expiresAt.UnixNano()

	query := `
INSERT INTO conversions (
    id, token, original_filename, file_size, status, error_message, input_path, output_path, created_at_ns, updated_at_ns, expires_at_ns
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);
`
	_, err := s.db.ExecContext(ctx, s.rebind(query),
		job.ID,
		job.Token,
		job.OriginalFilename,
		job.FileSize,
		string(job.Status),
		job.ErrorMessage,
		job.InputPath,
		job.OutputPath,
		createdNS,
		updatedNS,
		expiresNS,
	)
	if err != nil {
		return fmt.Errorf("create conversion: %w", err)
	}
	return nil
}

// GetConversion retrieves a conversion job by its ID. Returns (nil, nil) if not found.
func (s *Store) GetConversion(ctx context.Context, id string) (*ConversionJob, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("get conversion: store is closed or nil")
	}
	if ctx == nil {
		return nil, fmt.Errorf("get conversion: context is nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("get conversion: %w", err)
	}
	if strings.TrimSpace(id) == "" {
		return nil, fmt.Errorf("get conversion: id cannot be empty")
	}

	query := `
SELECT id, token, original_filename, file_size, status, error_message, input_path, output_path, created_at_ns, updated_at_ns, expires_at_ns
FROM conversions
WHERE id = ?;
`
	row := s.db.QueryRowContext(ctx, s.rebind(query), id)

	var (
		resID      string
		token      string
		origName   string
		fileSize   int64
		statusStr  string
		errMsg     string
		inputPath  string
		outputPath string
		createdNS  int64
		updatedNS  int64
		expiresNS  int64
	)

	err := row.Scan(
		&resID,
		&token,
		&origName,
		&fileSize,
		&statusStr,
		&errMsg,
		&inputPath,
		&outputPath,
		&createdNS,
		&updatedNS,
		&expiresNS,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get conversion: %w", err)
	}

	return &ConversionJob{
		ID:               resID,
		Token:            token,
		OriginalFilename: origName,
		FileSize:         fileSize,
		Status:           ConversionStatus(statusStr),
		ErrorMessage:     errMsg,
		InputPath:        inputPath,
		OutputPath:       outputPath,
		CreatedAt:        time.Unix(0, createdNS).UTC(),
		UpdatedAt:        time.Unix(0, updatedNS).UTC(),
		ExpiresAt:        time.Unix(0, expiresNS).UTC(),
	}, nil
}

// UpdateConversionStatus updates status, error message, and output path for a conversion job.
func (s *Store) UpdateConversionStatus(ctx context.Context, id string, status ConversionStatus, errorMsg string, outputPath string) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("update conversion: store is closed or nil")
	}
	if ctx == nil {
		return fmt.Errorf("update conversion: context is nil")
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("update conversion: %w", err)
	}
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("update conversion: id cannot be empty")
	}
	if !IsValidConversionStatus(status) {
		return fmt.Errorf("update conversion: invalid status %q", status)
	}

	updatedNS := time.Now().UTC().UnixNano()

	query := `
UPDATE conversions
SET status = ?, error_message = ?, output_path = CASE WHEN ? != '' THEN ? ELSE output_path END, updated_at_ns = ?
WHERE id = ?;
`
	res, err := s.db.ExecContext(ctx, s.rebind(query), string(status), errorMsg, outputPath, outputPath, updatedNS, id)
	if err != nil {
		return fmt.Errorf("update conversion: %w", err)
	}

	rows, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("update conversion: check rows affected: %w", err)
	}
	if rows == 0 {
		return fmt.Errorf("update conversion: job %q not found", id)
	}
	return nil
}

// ClaimNextQueuedConversion atomically finds and claims the oldest queued unexpired conversion job,
// setting its status to ConversionStatusProcessing. Returns (nil, nil) if no queued jobs are available.
func (s *Store) ClaimNextQueuedConversion(ctx context.Context) (*ConversionJob, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("claim next conversion: store is closed or nil")
	}
	if ctx == nil {
		return nil, fmt.Errorf("claim next conversion: context is nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("claim next conversion: %w", err)
	}

	nowNS := time.Now().UTC().UnixNano()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("claim next conversion: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	findQuery := `
SELECT id, token, original_filename, file_size, status, error_message, input_path, output_path, created_at_ns, updated_at_ns, expires_at_ns
FROM conversions
WHERE status = 'queued' AND expires_at_ns > ?
ORDER BY created_at_ns ASC
LIMIT 1;
`
	row := tx.QueryRowContext(ctx, s.rebind(findQuery), nowNS)

	var (
		resID      string
		token      string
		origName   string
		fileSize   int64
		statusStr  string
		errMsg     string
		inputPath  string
		outputPath string
		createdNS  int64
		updatedNS  int64
		expiresNS  int64
	)

	err = row.Scan(
		&resID,
		&token,
		&origName,
		&fileSize,
		&statusStr,
		&errMsg,
		&inputPath,
		&outputPath,
		&createdNS,
		&updatedNS,
		&expiresNS,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("claim next conversion: scan: %w", err)
	}

	claimQuery := `
UPDATE conversions
SET status = 'processing', updated_at_ns = ?
WHERE id = ? AND status = 'queued';
`
	updateRes, err := tx.ExecContext(ctx, s.rebind(claimQuery), nowNS, resID)
	if err != nil {
		return nil, fmt.Errorf("claim next conversion: update status: %w", err)
	}

	rowsAff, err := updateRes.RowsAffected()
	if err != nil || rowsAff == 0 {
		// Another worker claimed it concurrently; return nil to let caller retry
		return nil, nil
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("claim next conversion: commit tx: %w", err)
	}

	return &ConversionJob{
		ID:               resID,
		Token:            token,
		OriginalFilename: origName,
		FileSize:         fileSize,
		Status:           ConversionStatusProcessing,
		ErrorMessage:     errMsg,
		InputPath:        inputPath,
		OutputPath:       outputPath,
		CreatedAt:        time.Unix(0, createdNS).UTC(),
		UpdatedAt:        time.Unix(0, nowNS).UTC(),
		ExpiresAt:        time.Unix(0, expiresNS).UTC(),
	}, nil
}

// RecoverStaleConversions resets any job that was interrupted in 'processing' status
// back to 'queued' so it can be re-run following a server restart.
func (s *Store) RecoverStaleConversions(ctx context.Context) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("recover stale conversions: store is closed or nil")
	}
	if ctx == nil {
		return fmt.Errorf("recover stale conversions: context is nil")
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("recover stale conversions: %w", err)
	}

	nowNS := time.Now().UTC().UnixNano()
	query := `
UPDATE conversions
SET status = 'queued', updated_at_ns = ?
WHERE status = 'processing';
`
	_, err := s.db.ExecContext(ctx, s.rebind(query), nowNS)
	if err != nil {
		return fmt.Errorf("recover stale conversions: %w", err)
	}
	return nil
}

// DeleteExpiredConversions deletes conversion records whose expires_at_ns is <= now,
// returning the file paths of all deleted jobs so disk cleanup can occur safely.
func (s *Store) DeleteExpiredConversions(ctx context.Context, now time.Time) ([]string, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("delete expired conversions: store is closed or nil")
	}
	if ctx == nil {
		return nil, fmt.Errorf("delete expired conversions: context is nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("delete expired conversions: %w", err)
	}

	nowNS := now.UTC().UnixNano()

	// Query paths to delete first
	queryPaths := `
SELECT input_path, output_path
FROM conversions
WHERE expires_at_ns <= ?;
`
	rows, err := s.db.QueryContext(ctx, s.rebind(queryPaths), nowNS)
	if err != nil {
		return nil, fmt.Errorf("delete expired conversions: query paths: %w", err)
	}
	defer rows.Close()

	var paths []string
	for rows.Next() {
		var inPath, outPath string
		if err := rows.Scan(&inPath, &outPath); err == nil {
			if inPath != "" {
				paths = append(paths, inPath)
			}
			if outPath != "" {
				paths = append(paths, outPath)
			}
		}
	}
	_ = rows.Close()

	deleteQuery := `
DELETE FROM conversions
WHERE expires_at_ns <= ?;
`
	if _, err := s.db.ExecContext(ctx, s.rebind(deleteQuery), nowNS); err != nil {
		return nil, fmt.Errorf("delete expired conversions: delete rows: %w", err)
	}

	return paths, nil
}

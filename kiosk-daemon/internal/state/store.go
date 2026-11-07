package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"xeroxgo/pkg/contracts"
)

type Store struct {
	db *sql.DB
}

type StoredJob struct {
	Command       contracts.PrintJobCommand
	Status        contracts.JobStatus
	PagesPrinted  int
	SheetsPrinted int
	ErrorReason   string
	UpdatedAt     time.Time
}

func isValidStatus(status contracts.JobStatus) bool {
	switch status {
	case contracts.JobStatusUploaded,
		contracts.JobStatusNormalized,
		contracts.JobStatusPendingPayment,
		contracts.JobStatusPaidWaiting,
		contracts.JobStatusDispatched,
		contracts.JobStatusSpooling,
		contracts.JobStatusPrinting,
		contracts.JobStatusCompleted,
		contracts.JobStatusPausedError,
		contracts.JobStatusFailedRefundable:
		return true
	default:
		return false
	}
}

func isValidDuplex(duplex contracts.DuplexMode) bool {
	switch duplex {
	case contracts.DuplexSimplex,
		contracts.DuplexLongEdge,
		contracts.DuplexShortEdge:
		return true
	default:
		return false
	}
}

func Open(dbPath string) (*Store, error) {
	if dbPath == "" {
		return nil, errors.New("database path cannot be empty")
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open sqlite database at %s: %w", dbPath, err)
	}

	// Single connection so connection-specific SQLite settings remain predictable
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	cleanup := func() {
		_ = db.Close()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Apply and verify PRAGMA journal_mode=WAL;
	var journalMode string
	if err := db.QueryRowContext(ctx, "PRAGMA journal_mode=WAL;").Scan(&journalMode); err != nil {
		cleanup()
		return nil, fmt.Errorf("failed to execute PRAGMA journal_mode=WAL: %w", err)
	}
	if !strings.EqualFold(journalMode, "wal") {
		cleanup()
		return nil, fmt.Errorf("unexpected journal_mode: got %s, want wal", journalMode)
	}

	// Apply and verify PRAGMA synchronous=FULL;
	if _, err := db.ExecContext(ctx, "PRAGMA synchronous=FULL;"); err != nil {
		cleanup()
		return nil, fmt.Errorf("failed to set PRAGMA synchronous=FULL: %w", err)
	}
	var syncVal int
	if err := db.QueryRowContext(ctx, "PRAGMA synchronous;").Scan(&syncVal); err != nil {
		cleanup()
		return nil, fmt.Errorf("failed to read PRAGMA synchronous: %w", err)
	}
	if syncVal != 2 {
		cleanup()
		return nil, fmt.Errorf("unexpected synchronous value: got %d, want 2 (FULL)", syncVal)
	}

	// Create tables
	schema := `
CREATE TABLE IF NOT EXISTS jobs (
    job_id TEXT PRIMARY KEY,
    kiosk_id TEXT NOT NULL,
    display_code TEXT NOT NULL,
    download_url TEXT NOT NULL,
    sha256_checksum TEXT NOT NULL,
    total_pages INTEGER NOT NULL CHECK (total_pages > 0),
    total_sheets INTEGER NOT NULL CHECK (total_sheets > 0),
    copies INTEGER NOT NULL CHECK (copies > 0),
    duplex TEXT NOT NULL,
    start_from_page INTEGER NOT NULL CHECK (start_from_page >= 1),
    status TEXT NOT NULL,
    pages_printed INTEGER NOT NULL DEFAULT 0 CHECK (pages_printed >= 0),
    sheets_printed INTEGER NOT NULL DEFAULT 0 CHECK (sheets_printed >= 0),
    error_reason TEXT NOT NULL DEFAULT '',
    updated_at_ns INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS printer_state (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    estimated_sheets INTEGER NOT NULL
        CHECK (estimated_sheets >= 0 AND estimated_sheets <= 250)
);

INSERT OR IGNORE INTO printer_state (id, estimated_sheets)
VALUES (1, 250);
`
	if _, err := db.ExecContext(ctx, schema); err != nil {
		cleanup()
		return nil, fmt.Errorf("failed to initialize database schema: %w", err)
	}

	return &Store{db: db}, nil
}

func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *Store) SaveJob(
	ctx context.Context,
	cmd contracts.PrintJobCommand,
	status contracts.JobStatus,
) error {
	if s == nil || s.db == nil {
		return errors.New("store is not open")
	}
	if ctx == nil {
		return errors.New("context cannot be nil")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if cmd.JobID == "" {
		return errors.New("job_id cannot be empty")
	}
	if cmd.KioskID == "" {
		return errors.New("kiosk_id cannot be empty")
	}
	if cmd.TotalPages < 1 {
		return fmt.Errorf("total_pages must be >= 1, got %d", cmd.TotalPages)
	}
	if cmd.TotalSheets < 1 {
		return fmt.Errorf("total_sheets must be >= 1, got %d", cmd.TotalSheets)
	}
	if cmd.Copies < 1 {
		return fmt.Errorf("copies must be >= 1, got %d", cmd.Copies)
	}
	if cmd.StartFromPage < 1 {
		return fmt.Errorf("start_from_page must be >= 1, got %d", cmd.StartFromPage)
	}
	if cmd.StartFromPage > cmd.TotalPages {
		return fmt.Errorf("start_from_page (%d) cannot exceed total_pages (%d)", cmd.StartFromPage, cmd.TotalPages)
	}
	if !isValidDuplex(cmd.Duplex) {
		return fmt.Errorf("invalid duplex mode: %q", cmd.Duplex)
	}
	if !isValidStatus(status) {
		return fmt.Errorf("invalid job status: %q", status)
	}

	nowNS := time.Now().UTC().UnixNano()

	query := `
INSERT INTO jobs (
    job_id, kiosk_id, display_code, download_url, sha256_checksum,
    total_pages, total_sheets, copies, duplex, start_from_page,
    status, pages_printed, sheets_printed, error_reason, updated_at_ns
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, 0, '', ?);`

	_, err := s.db.ExecContext(ctx, query,
		cmd.JobID,
		cmd.KioskID,
		cmd.DisplayCode,
		cmd.DownloadURL,
		cmd.SHA256Checksum,
		cmd.TotalPages,
		cmd.TotalSheets,
		cmd.Copies,
		string(cmd.Duplex),
		cmd.StartFromPage,
		string(status),
		nowNS,
	)
	if err != nil {
		return fmt.Errorf("failed to save job %s: %w", cmd.JobID, err)
	}

	return nil
}

func (s *Store) UpdateProgress(
	ctx context.Context,
	event contracts.JobProgressEvent,
) error {
	if s == nil || s.db == nil {
		return errors.New("store is not open")
	}
	if ctx == nil {
		return errors.New("context cannot be nil")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if event.JobID == "" {
		return errors.New("event.job_id cannot be empty")
	}
	if !isValidStatus(event.Status) {
		return fmt.Errorf("invalid status in progress event: %q", event.Status)
	}
	if event.PagesPrinted < 0 {
		return fmt.Errorf("pages_printed cannot be negative, got %d", event.PagesPrinted)
	}
	if event.SheetsPrinted < 0 {
		return fmt.Errorf("sheets_printed cannot be negative, got %d", event.SheetsPrinted)
	}

	updatedAtNS := event.Timestamp.UTC().UnixNano()

	query := `
UPDATE jobs
SET status = ?,
    pages_printed = ?,
    sheets_printed = ?,
    error_reason = ?,
    updated_at_ns = ?
WHERE job_id = ?;`

	res, err := s.db.ExecContext(ctx, query,
		string(event.Status),
		event.PagesPrinted,
		event.SheetsPrinted,
		event.ErrorReason,
		updatedAtNS,
		event.JobID,
	)
	if err != nil {
		return fmt.Errorf("failed to update progress for job %s: %w", event.JobID, err)
	}

	rows, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to check affected rows for job %s: %w", event.JobID, err)
	}
	if rows == 0 {
		return fmt.Errorf("job not found: %s", event.JobID)
	}

	return nil
}

func (s *Store) GetIncompleteJob(
	ctx context.Context,
) (*StoredJob, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("store is not open")
	}
	if ctx == nil {
		return nil, errors.New("context cannot be nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	query := `
SELECT job_id, kiosk_id, display_code, download_url, sha256_checksum,
       total_pages, total_sheets, copies, duplex, start_from_page,
       status, pages_printed, sheets_printed, error_reason, updated_at_ns
FROM jobs
WHERE status IN ('SPOOLING', 'PRINTING', 'PAUSED_ERROR')
ORDER BY updated_at_ns DESC, job_id ASC
LIMIT 1;`

	row := s.db.QueryRowContext(ctx, query)

	var job StoredJob
	var duplexStr string
	var statusStr string
	var updatedAtNS int64

	err := row.Scan(
		&job.Command.JobID,
		&job.Command.KioskID,
		&job.Command.DisplayCode,
		&job.Command.DownloadURL,
		&job.Command.SHA256Checksum,
		&job.Command.TotalPages,
		&job.Command.TotalSheets,
		&job.Command.Copies,
		&duplexStr,
		&job.Command.StartFromPage,
		&statusStr,
		&job.PagesPrinted,
		&job.SheetsPrinted,
		&job.ErrorReason,
		&updatedAtNS,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to query incomplete job: %w", err)
	}

	job.Command.Duplex = contracts.DuplexMode(duplexStr)
	job.Status = contracts.JobStatus(statusStr)
	job.UpdatedAt = time.Unix(0, updatedAtNS).UTC()

	return &job, nil
}

func (s *Store) GetEstimatedSheets(
	ctx context.Context,
) (int, error) {
	if s == nil || s.db == nil {
		return 0, errors.New("store is not open")
	}
	if ctx == nil {
		return 0, errors.New("context cannot be nil")
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	var sheets int
	err := s.db.QueryRowContext(ctx, "SELECT estimated_sheets FROM printer_state WHERE id = 1;").Scan(&sheets)
	if err != nil {
		return 0, fmt.Errorf("failed to query estimated sheets: %w", err)
	}

	return sheets, nil
}

func (s *Store) DecrementSheets(
	ctx context.Context,
	sheets int,
) error {
	if s == nil || s.db == nil {
		return errors.New("store is not open")
	}
	if ctx == nil {
		return errors.New("context cannot be nil")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if sheets <= 0 {
		return fmt.Errorf("sheets to decrement must be > 0, got %d", sheets)
	}

	query := `
UPDATE printer_state
SET estimated_sheets = estimated_sheets - ?
WHERE id = 1
  AND estimated_sheets >= ?;`

	res, err := s.db.ExecContext(ctx, query, sheets, sheets)
	if err != nil {
		return fmt.Errorf("failed to decrement sheets: %w", err)
	}

	rows, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to check affected rows during decrement: %w", err)
	}
	if rows == 0 {
		return errors.New("insufficient sheets in paper tray")
	}

	return nil
}

func (s *Store) ResetPaperTray(
	ctx context.Context,
	sheets int,
) error {
	if s == nil || s.db == nil {
		return errors.New("store is not open")
	}
	if ctx == nil {
		return errors.New("context cannot be nil")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if sheets < 0 || sheets > 250 {
		return fmt.Errorf("sheets must be between 0 and 250 inclusive, got %d", sheets)
	}

	query := `UPDATE printer_state SET estimated_sheets = ? WHERE id = 1;`
	res, err := s.db.ExecContext(ctx, query, sheets)
	if err != nil {
		return fmt.Errorf("failed to reset paper tray: %w", err)
	}

	rows, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to check affected rows during reset: %w", err)
	}
	if rows == 0 {
		return errors.New("printer_state record not found")
	}

	return nil
}

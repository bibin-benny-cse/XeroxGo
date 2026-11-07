package state

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"xeroxgo/pkg/contracts"
)

func newTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	return s, dbPath
}

func sampleCommand(jobID string) contracts.PrintJobCommand {
	return contracts.PrintJobCommand{
		JobID:          jobID,
		KioskID:        "kiosk-01",
		DisplayCode:    "A1B2",
		DownloadURL:    "https://example.invalid/print.pdf",
		SHA256Checksum: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		TotalPages:     10,
		TotalSheets:    5,
		Copies:         1,
		Duplex:         contracts.DuplexLongEdge,
		StartFromPage:  1,
	}
}

func TestOpenInitializesDatabase(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "init.db")
	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}
	defer s.Close()

	ctx := context.Background()
	sheets, err := s.GetEstimatedSheets(ctx)
	if err != nil {
		t.Fatalf("GetEstimatedSheets failed: %v", err)
	}
	if sheets != 250 {
		t.Errorf("initial estimated sheets = %d, want 250", sheets)
	}

	var journalMode string
	if err := s.db.QueryRow("PRAGMA journal_mode;").Scan(&journalMode); err != nil {
		t.Fatalf("failed to query journal_mode: %v", err)
	}
	if journalMode != "wal" {
		t.Errorf("journal_mode = %s, want wal", journalMode)
	}

	var syncVal int
	if err := s.db.QueryRow("PRAGMA synchronous;").Scan(&syncVal); err != nil {
		t.Fatalf("failed to query synchronous: %v", err)
	}
	if syncVal != 2 {
		t.Errorf("synchronous = %d, want 2", syncVal)
	}

	if err := s.Close(); err != nil {
		t.Fatalf("failed to close store: %v", err)
	}
}

func TestCrashRecoveryAfterReopen(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "crash_recovery.db")

	store1, err := Open(dbPath)
	if err != nil {
		t.Fatalf("initial Open failed: %v", err)
	}

	ctx := context.Background()
	cmd := sampleCommand("job-crash-001")
	cmd.TotalPages = 8
	cmd.TotalSheets = 4
	cmd.Copies = 2
	cmd.Duplex = contracts.DuplexShortEdge
	cmd.StartFromPage = 2

	if err := store1.SaveJob(ctx, cmd, contracts.JobStatusSpooling); err != nil {
		t.Fatalf("SaveJob failed: %v", err)
	}

	fixedTime := time.Date(2026, 10, 21, 14, 0, 0, 0, time.UTC)
	progress := contracts.JobProgressEvent{
		JobID:         "job-crash-001",
		KioskID:       "kiosk-01",
		Status:        contracts.JobStatusPrinting,
		PagesPrinted:  4,
		SheetsPrinted: 2,
		ErrorReason:   "printer warming up",
		Timestamp:     fixedTime,
	}

	if err := store1.UpdateProgress(ctx, progress); err != nil {
		t.Fatalf("UpdateProgress failed: %v", err)
	}

	if err := store1.DecrementSheets(ctx, 3); err != nil {
		t.Fatalf("DecrementSheets failed: %v", err)
	}

	if err := store1.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	// Reopen the database to simulate process restart
	store2, err := Open(dbPath)
	if err != nil {
		t.Fatalf("reopening database failed: %v", err)
	}
	defer store2.Close()

	recovered, err := store2.GetIncompleteJob(ctx)
	if err != nil {
		t.Fatalf("GetIncompleteJob failed: %v", err)
	}
	if recovered == nil {
		t.Fatal("expected recovered job, got nil")
	}

	// Verify command fields
	if recovered.Command.JobID != cmd.JobID {
		t.Errorf("JobID = %s, want %s", recovered.Command.JobID, cmd.JobID)
	}
	if recovered.Command.KioskID != cmd.KioskID {
		t.Errorf("KioskID = %s, want %s", recovered.Command.KioskID, cmd.KioskID)
	}
	if recovered.Command.DisplayCode != cmd.DisplayCode {
		t.Errorf("DisplayCode = %s, want %s", recovered.Command.DisplayCode, cmd.DisplayCode)
	}
	if recovered.Command.DownloadURL != cmd.DownloadURL {
		t.Errorf("DownloadURL = %s, want %s", recovered.Command.DownloadURL, cmd.DownloadURL)
	}
	if recovered.Command.SHA256Checksum != cmd.SHA256Checksum {
		t.Errorf("SHA256Checksum = %s, want %s", recovered.Command.SHA256Checksum, cmd.SHA256Checksum)
	}
	if recovered.Command.TotalPages != cmd.TotalPages {
		t.Errorf("TotalPages = %d, want %d", recovered.Command.TotalPages, cmd.TotalPages)
	}
	if recovered.Command.TotalSheets != cmd.TotalSheets {
		t.Errorf("TotalSheets = %d, want %d", recovered.Command.TotalSheets, cmd.TotalSheets)
	}
	if recovered.Command.Copies != cmd.Copies {
		t.Errorf("Copies = %d, want %d", recovered.Command.Copies, cmd.Copies)
	}
	if recovered.Command.Duplex != cmd.Duplex {
		t.Errorf("Duplex = %s, want %s", recovered.Command.Duplex, cmd.Duplex)
	}
	if recovered.Command.StartFromPage != cmd.StartFromPage {
		t.Errorf("StartFromPage = %d, want %d", recovered.Command.StartFromPage, cmd.StartFromPage)
	}

	// Verify progress fields
	if recovered.Status != contracts.JobStatusPrinting {
		t.Errorf("Status = %s, want %s", recovered.Status, contracts.JobStatusPrinting)
	}
	if recovered.PagesPrinted != 4 {
		t.Errorf("PagesPrinted = %d, want 4", recovered.PagesPrinted)
	}
	if recovered.SheetsPrinted != 2 {
		t.Errorf("SheetsPrinted = %d, want 2", recovered.SheetsPrinted)
	}
	if recovered.ErrorReason != "printer warming up" {
		t.Errorf("ErrorReason = %q, want %q", recovered.ErrorReason, "printer warming up")
	}
	if !recovered.UpdatedAt.Equal(fixedTime) {
		t.Errorf("UpdatedAt = %v, want %v", recovered.UpdatedAt, fixedTime)
	}

	// Verify paper tray preserved
	sheets, err := store2.GetEstimatedSheets(ctx)
	if err != nil {
		t.Fatalf("GetEstimatedSheets failed: %v", err)
	}
	if sheets != 247 {
		t.Errorf("estimated sheets after reopen = %d, want 247", sheets)
	}
}

func TestGetIncompleteJobStatuses(t *testing.T) {
	statuses := []contracts.JobStatus{
		contracts.JobStatusSpooling,
		contracts.JobStatusPrinting,
		contracts.JobStatusPausedError,
	}

	for _, st := range statuses {
		t.Run(string(st), func(t *testing.T) {
			s, _ := newTestStore(t)
			defer s.Close()

			ctx := context.Background()
			cmd := sampleCommand("job-" + string(st))
			if err := s.SaveJob(ctx, cmd, st); err != nil {
				t.Fatalf("SaveJob failed: %v", err)
			}

			job, err := s.GetIncompleteJob(ctx)
			if err != nil {
				t.Fatalf("GetIncompleteJob failed: %v", err)
			}
			if job == nil {
				t.Fatalf("expected job with status %s, got nil", st)
			}
			if job.Status != st {
				t.Errorf("status = %s, want %s", job.Status, st)
			}
			if job.Command.JobID != cmd.JobID {
				t.Errorf("job_id = %s, want %s", job.Command.JobID, cmd.JobID)
			}
		})
	}
}

func TestGetIncompleteJobExcludesTerminalAndPendingJobs(t *testing.T) {
	nonIncompleteStatuses := []contracts.JobStatus{
		contracts.JobStatusUploaded,
		contracts.JobStatusNormalized,
		contracts.JobStatusPendingPayment,
		contracts.JobStatusPaidWaiting,
		contracts.JobStatusDispatched,
		contracts.JobStatusCompleted,
		contracts.JobStatusFailedRefundable,
	}

	for _, st := range nonIncompleteStatuses {
		t.Run(string(st), func(t *testing.T) {
			s, _ := newTestStore(t)
			defer s.Close()

			ctx := context.Background()
			cmd := sampleCommand("job-" + string(st))
			if err := s.SaveJob(ctx, cmd, st); err != nil {
				t.Fatalf("SaveJob failed: %v", err)
			}

			job, err := s.GetIncompleteJob(ctx)
			if err != nil {
				t.Fatalf("GetIncompleteJob failed: %v", err)
			}
			if job != nil {
				t.Errorf("expected nil for status %s, got job_id=%s with status %s", st, job.Command.JobID, job.Status)
			}
		})
	}
}

func TestUpdateProgressRejectsUnknownJob(t *testing.T) {
	s, _ := newTestStore(t)
	defer s.Close()

	ctx := context.Background()
	event := contracts.JobProgressEvent{
		JobID:         "nonexistent-job-999",
		KioskID:       "kiosk-01",
		Status:        contracts.JobStatusPrinting,
		PagesPrinted:  1,
		SheetsPrinted: 1,
		Timestamp:     time.Now().UTC(),
	}

	err := s.UpdateProgress(ctx, event)
	if err == nil {
		t.Fatal("expected error updating unknown job, got nil")
	}

	// Verify no job was inserted
	job, err := s.GetIncompleteJob(ctx)
	if err != nil {
		t.Fatalf("GetIncompleteJob failed: %v", err)
	}
	if job != nil {
		t.Errorf("expected no job in database, found %s", job.Command.JobID)
	}
}

func TestDecrementSheets(t *testing.T) {
	s, _ := newTestStore(t)
	defer s.Close()

	ctx := context.Background()

	// 1. Initial count is 250
	sheets, err := s.GetEstimatedSheets(ctx)
	if err != nil {
		t.Fatalf("GetEstimatedSheets failed: %v", err)
	}
	if sheets != 250 {
		t.Fatalf("initial sheets = %d, want 250", sheets)
	}

	// 2. Decrement by 3 succeeds
	if err := s.DecrementSheets(ctx, 3); err != nil {
		t.Fatalf("DecrementSheets(3) failed: %v", err)
	}

	// 3. Count is 247
	sheets, err = s.GetEstimatedSheets(ctx)
	if err != nil {
		t.Fatalf("GetEstimatedSheets failed: %v", err)
	}
	if sheets != 247 {
		t.Errorf("sheets after decrement = %d, want 247", sheets)
	}

	// 4. Decrement by 247 succeeds and yields 0
	if err := s.DecrementSheets(ctx, 247); err != nil {
		t.Fatalf("DecrementSheets(247) failed: %v", err)
	}
	sheets, err = s.GetEstimatedSheets(ctx)
	if err != nil {
		t.Fatalf("GetEstimatedSheets failed: %v", err)
	}
	if sheets != 0 {
		t.Errorf("sheets after decrement = %d, want 0", sheets)
	}

	// 5. Attempting another decrement fails
	if err := s.DecrementSheets(ctx, 1); err == nil {
		t.Fatal("expected error decrementing below 0, got nil")
	}

	// 6. Count remains 0 after failed decrement
	sheets, err = s.GetEstimatedSheets(ctx)
	if err != nil {
		t.Fatalf("GetEstimatedSheets failed: %v", err)
	}
	if sheets != 0 {
		t.Errorf("sheets after failed decrement = %d, want 0", sheets)
	}

	// 7. Decrementing by zero or negative count fails
	if err := s.DecrementSheets(ctx, 0); err == nil {
		t.Error("expected error for DecrementSheets(0), got nil")
	}
	if err := s.DecrementSheets(ctx, -5); err == nil {
		t.Error("expected error for DecrementSheets(-5), got nil")
	}
}

func TestResetPaperTray(t *testing.T) {
	s, _ := newTestStore(t)
	defer s.Close()

	ctx := context.Background()

	// Reset to 250 succeeds
	if err := s.ResetPaperTray(ctx, 250); err != nil {
		t.Fatalf("ResetPaperTray(250) failed: %v", err)
	}
	sheets, _ := s.GetEstimatedSheets(ctx)
	if sheets != 250 {
		t.Errorf("sheets = %d, want 250", sheets)
	}

	// Reset to 120 succeeds
	if err := s.ResetPaperTray(ctx, 120); err != nil {
		t.Fatalf("ResetPaperTray(120) failed: %v", err)
	}
	sheets, _ = s.GetEstimatedSheets(ctx)
	if sheets != 120 {
		t.Errorf("sheets = %d, want 120", sheets)
	}

	// Reset to 0 succeeds
	if err := s.ResetPaperTray(ctx, 0); err != nil {
		t.Fatalf("ResetPaperTray(0) failed: %v", err)
	}
	sheets, _ = s.GetEstimatedSheets(ctx)
	if sheets != 0 {
		t.Errorf("sheets = %d, want 0", sheets)
	}

	// Counts outside 0-250 are rejected
	if err := s.ResetPaperTray(ctx, -1); err == nil {
		t.Error("expected error for ResetPaperTray(-1), got nil")
	}
	if err := s.ResetPaperTray(ctx, 251); err == nil {
		t.Error("expected error for ResetPaperTray(251), got nil")
	}

	// Value was not corrupted by failed resets
	sheets, _ = s.GetEstimatedSheets(ctx)
	if sheets != 0 {
		t.Errorf("sheets after failed resets = %d, want 0", sheets)
	}
}

func TestPersistenceOfMultipleJobs(t *testing.T) {
	s, _ := newTestStore(t)
	defer s.Close()

	ctx := context.Background()

	cmdA := sampleCommand("job-A")
	cmdB := sampleCommand("job-B")
	cmdC := sampleCommand("job-C")

	if err := s.SaveJob(ctx, cmdA, contracts.JobStatusSpooling); err != nil {
		t.Fatalf("SaveJob A failed: %v", err)
	}
	if err := s.SaveJob(ctx, cmdB, contracts.JobStatusSpooling); err != nil {
		t.Fatalf("SaveJob B failed: %v", err)
	}
	if err := s.SaveJob(ctx, cmdC, contracts.JobStatusSpooling); err != nil {
		t.Fatalf("SaveJob C failed: %v", err)
	}

	baseTime := time.Now().UTC()

	// Update job A at T+10
	eventA := contracts.JobProgressEvent{
		JobID:         "job-A",
		KioskID:       "kiosk-01",
		Status:        contracts.JobStatusPrinting,
		PagesPrinted:  2,
		SheetsPrinted: 1,
		Timestamp:     baseTime.Add(10 * time.Minute),
	}
	if err := s.UpdateProgress(ctx, eventA); err != nil {
		t.Fatalf("UpdateProgress A failed: %v", err)
	}

	// Update job B at T+20
	eventB := contracts.JobProgressEvent{
		JobID:         "job-B",
		KioskID:       "kiosk-01",
		Status:        contracts.JobStatusPausedError,
		PagesPrinted:  5,
		SheetsPrinted: 3,
		ErrorReason:   "paper jam",
		Timestamp:     baseTime.Add(20 * time.Minute),
	}
	if err := s.UpdateProgress(ctx, eventB); err != nil {
		t.Fatalf("UpdateProgress B failed: %v", err)
	}

	// Most recently updated is job B
	mostRecent, err := s.GetIncompleteJob(ctx)
	if err != nil {
		t.Fatalf("GetIncompleteJob failed: %v", err)
	}
	if mostRecent == nil || mostRecent.Command.JobID != "job-B" {
		t.Fatalf("expected job-B to be most recent, got: %#v", mostRecent)
	}
	if mostRecent.PagesPrinted != 5 || mostRecent.ErrorReason != "paper jam" {
		t.Errorf("job-B progress corrupted: %#v", mostRecent)
	}

	// Now complete job B at T+30
	eventBComplete := contracts.JobProgressEvent{
		JobID:         "job-B",
		KioskID:       "kiosk-01",
		Status:        contracts.JobStatusCompleted,
		PagesPrinted:  10,
		SheetsPrinted: 5,
		Timestamp:     baseTime.Add(30 * time.Minute),
	}
	if err := s.UpdateProgress(ctx, eventBComplete); err != nil {
		t.Fatalf("UpdateProgress B complete failed: %v", err)
	}

	// Now most recently updated incomplete job should be job A (since job B is COMPLETED and job C was saved at base creation time)
	nextIncomplete, err := s.GetIncompleteJob(ctx)
	if err != nil {
		t.Fatalf("GetIncompleteJob failed: %v", err)
	}
	if nextIncomplete == nil || nextIncomplete.Command.JobID != "job-A" {
		t.Fatalf("expected job-A to be next incomplete, got: %#v", nextIncomplete)
	}
	if nextIncomplete.PagesPrinted != 2 || nextIncomplete.Status != contracts.JobStatusPrinting {
		t.Errorf("job-A progress corrupted: %#v", nextIncomplete)
	}
}

func TestRejectsInvalidInput(t *testing.T) {
	// 1. Empty database path
	if _, err := Open(""); err == nil {
		t.Error("expected error for Open(\"\"), got nil")
	}

	s, _ := newTestStore(t)
	defer s.Close()

	ctx := context.Background()

	// 2. Invalid SaveJob inputs
	tests := []struct {
		name   string
		ctx    context.Context
		cmd    contracts.PrintJobCommand
		status contracts.JobStatus
	}{
		{
			name:   "nil_context",
			ctx:    nil,
			cmd:    sampleCommand("job-1"),
			status: contracts.JobStatusSpooling,
		},
		{
			name: "canceled_context",
			ctx: func() context.Context {
				c, cancel := context.WithCancel(ctx)
				cancel()
				return c
			}(),
			cmd:    sampleCommand("job-1"),
			status: contracts.JobStatusSpooling,
		},
		{
			name: "empty_job_id",
			ctx:  ctx,
			cmd: func() contracts.PrintJobCommand {
				c := sampleCommand("")
				return c
			}(),
			status: contracts.JobStatusSpooling,
		},
		{
			name: "empty_kiosk_id",
			ctx:  ctx,
			cmd: func() contracts.PrintJobCommand {
				c := sampleCommand("job-1")
				c.KioskID = ""
				return c
			}(),
			status: contracts.JobStatusSpooling,
		},
		{
			name: "zero_total_pages",
			ctx:  ctx,
			cmd: func() contracts.PrintJobCommand {
				c := sampleCommand("job-1")
				c.TotalPages = 0
				return c
			}(),
			status: contracts.JobStatusSpooling,
		},
		{
			name: "negative_total_pages",
			ctx:  ctx,
			cmd: func() contracts.PrintJobCommand {
				c := sampleCommand("job-1")
				c.TotalPages = -1
				return c
			}(),
			status: contracts.JobStatusSpooling,
		},
		{
			name: "zero_total_sheets",
			ctx:  ctx,
			cmd: func() contracts.PrintJobCommand {
				c := sampleCommand("job-1")
				c.TotalSheets = 0
				return c
			}(),
			status: contracts.JobStatusSpooling,
		},
		{
			name: "zero_copies",
			ctx:  ctx,
			cmd: func() contracts.PrintJobCommand {
				c := sampleCommand("job-1")
				c.Copies = 0
				return c
			}(),
			status: contracts.JobStatusSpooling,
		},
		{
			name: "start_from_page_zero",
			ctx:  ctx,
			cmd: func() contracts.PrintJobCommand {
				c := sampleCommand("job-1")
				c.StartFromPage = 0
				return c
			}(),
			status: contracts.JobStatusSpooling,
		},
		{
			name: "start_from_page_exceeds_total_pages",
			ctx:  ctx,
			cmd: func() contracts.PrintJobCommand {
				c := sampleCommand("job-1")
				c.TotalPages = 5
				c.StartFromPage = 6
				return c
			}(),
			status: contracts.JobStatusSpooling,
		},
		{
			name: "invalid_duplex",
			ctx:  ctx,
			cmd: func() contracts.PrintJobCommand {
				c := sampleCommand("job-1")
				c.Duplex = "INVALID"
				return c
			}(),
			status: contracts.JobStatusSpooling,
		},
		{
			name:   "invalid_status",
			ctx:    ctx,
			cmd:    sampleCommand("job-1"),
			status: "UNKNOWN_STATUS",
		},
	}

	for _, tt := range tests {
		t.Run("SaveJob_"+tt.name, func(t *testing.T) {
			err := s.SaveJob(tt.ctx, tt.cmd, tt.status)
			if err == nil {
				t.Fatalf("expected error for %s, got nil", tt.name)
			}
		})
	}

	// 3. Duplicate job ID
	validCmd := sampleCommand("job-unique")
	if err := s.SaveJob(ctx, validCmd, contracts.JobStatusSpooling); err != nil {
		t.Fatalf("initial SaveJob failed: %v", err)
	}
	if err := s.SaveJob(ctx, validCmd, contracts.JobStatusSpooling); err == nil {
		t.Error("expected error saving duplicate job ID, got nil")
	}

	// 4. UpdateProgress invalid inputs
	progressTests := []struct {
		name  string
		ctx   context.Context
		event contracts.JobProgressEvent
	}{
		{
			name: "nil_context",
			ctx:  nil,
			event: contracts.JobProgressEvent{
				JobID:     "job-unique",
				Status:    contracts.JobStatusPrinting,
				Timestamp: time.Now().UTC(),
			},
		},
		{
			name: "canceled_context",
			ctx: func() context.Context {
				c, cancel := context.WithCancel(ctx)
				cancel()
				return c
			}(),
			event: contracts.JobProgressEvent{
				JobID:     "job-unique",
				Status:    contracts.JobStatusPrinting,
				Timestamp: time.Now().UTC(),
			},
		},
		{
			name: "empty_job_id",
			ctx:  ctx,
			event: contracts.JobProgressEvent{
				JobID:     "",
				Status:    contracts.JobStatusPrinting,
				Timestamp: time.Now().UTC(),
			},
		},
		{
			name: "invalid_status",
			ctx:  ctx,
			event: contracts.JobProgressEvent{
				JobID:     "job-unique",
				Status:    "INVALID_STATUS",
				Timestamp: time.Now().UTC(),
			},
		},
		{
			name: "negative_pages_printed",
			ctx:  ctx,
			event: contracts.JobProgressEvent{
				JobID:        "job-unique",
				Status:       contracts.JobStatusPrinting,
				PagesPrinted: -1,
				Timestamp:    time.Now().UTC(),
			},
		},
		{
			name: "negative_sheets_printed",
			ctx:  ctx,
			event: contracts.JobProgressEvent{
				JobID:         "job-unique",
				Status:        contracts.JobStatusPrinting,
				SheetsPrinted: -1,
				Timestamp:     time.Now().UTC(),
			},
		},
	}

	for _, tt := range progressTests {
		t.Run("UpdateProgress_"+tt.name, func(t *testing.T) {
			err := s.UpdateProgress(tt.ctx, tt.event)
			if err == nil {
				t.Fatalf("expected error for %s, got nil", tt.name)
			}
		})
	}
}

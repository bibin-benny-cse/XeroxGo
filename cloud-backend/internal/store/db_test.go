package store

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"xeroxgo/pkg/contracts"
)

func newTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test_cloud.db")
	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open test store: %v", err)
	}
	t.Cleanup(func() {
		_ = s.Close()
	})
	return s, dbPath
}

func sampleOrder(orderID, jobID string) Order {
	fixedTime := time.Date(2026, time.October, 21, 10, 0, 0, 0, time.UTC)
	return Order{
		OrderID: orderID,
		Command: contracts.PrintJobCommand{
			JobID:          jobID,
			KioskID:        "kiosk-01",
			DisplayCode:    "A1B2",
			DownloadURL:    "https://storage.xeroxgo.edu/pdf/job.pdf",
			SHA256Checksum: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
			TotalPages:     10,
			TotalSheets:    5,
			Copies:         2,
			Duplex:         contracts.DuplexLongEdge,
			StartFromPage:  1,
		},
		Status:     contracts.JobStatusNormalized,
		PricePaise: 2500, // 25 INR
		CreatedAt:  fixedTime,
		UpdatedAt:  fixedTime,
	}
}

// Test A — TestOpenInitializesDatabase
func TestOpenInitializesDatabase(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test_open.db")
	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	var journalMode string
	if err := s.db.QueryRow("PRAGMA journal_mode;").Scan(&journalMode); err != nil {
		t.Fatalf("failed to query journal_mode: %v", err)
	}
	if journalMode != "wal" {
		t.Fatalf("expected journal_mode=wal, got %q", journalMode)
	}

	var synchronous int
	if err := s.db.QueryRow("PRAGMA synchronous;").Scan(&synchronous); err != nil {
		t.Fatalf("failed to query synchronous: %v", err)
	}
	if synchronous != 2 {
		t.Fatalf("expected synchronous=2, got %d", synchronous)
	}

	var foreignKeys int
	if err := s.db.QueryRow("PRAGMA foreign_keys;").Scan(&foreignKeys); err != nil {
		t.Fatalf("failed to query foreign_keys: %v", err)
	}
	if foreignKeys != 1 {
		t.Fatalf("expected foreign_keys=1, got %d", foreignKeys)
	}

	// Insert an order, close and reopen to verify persistence
	ctx := context.Background()
	ord := sampleOrder("ord-persist-1", "job-persist-1")
	if err := s.CreateOrder(ctx, ord); err != nil {
		t.Fatalf("CreateOrder failed: %v", err)
	}

	if err := s.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	s2, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Reopen failed: %v", err)
	}
	defer s2.Close()

	readOrd, err := s2.GetOrder(ctx, "ord-persist-1")
	if err != nil {
		t.Fatalf("GetOrder after reopen failed: %v", err)
	}
	if readOrd == nil || readOrd.OrderID != "ord-persist-1" {
		t.Fatalf("order not found or mismatched after reopen: %+v", readOrd)
	}
}

// Test B — TestCreateAndGetOrder
func TestCreateAndGetOrder(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	ord := sampleOrder("order-101", "job-202")
	if err := s.CreateOrder(ctx, ord); err != nil {
		t.Fatalf("CreateOrder failed: %v", err)
	}

	readOrd, err := s.GetOrder(ctx, "order-101")
	if err != nil {
		t.Fatalf("GetOrder failed: %v", err)
	}
	if readOrd == nil {
		t.Fatalf("GetOrder returned nil for existing order")
	}

	if readOrd.OrderID != ord.OrderID {
		t.Fatalf("OrderID mismatch: got %q, want %q", readOrd.OrderID, ord.OrderID)
	}
	if readOrd.Status != ord.Status {
		t.Fatalf("Status mismatch: got %q, want %q", readOrd.Status, ord.Status)
	}
	if readOrd.PricePaise != ord.PricePaise {
		t.Fatalf("PricePaise mismatch: got %d, want %d", readOrd.PricePaise, ord.PricePaise)
	}
	if !readOrd.CreatedAt.Equal(ord.CreatedAt) {
		t.Fatalf("CreatedAt mismatch: got %v, want %v", readOrd.CreatedAt, ord.CreatedAt)
	}
	if !readOrd.UpdatedAt.Equal(ord.UpdatedAt) {
		t.Fatalf("UpdatedAt mismatch: got %v, want %v", readOrd.UpdatedAt, ord.UpdatedAt)
	}
	if readOrd.CreatedAt.Location() != time.UTC {
		t.Fatalf("CreatedAt is not in UTC: %v", readOrd.CreatedAt.Location())
	}

	// Verify all command fields
	cmd := readOrd.Command
	wantCmd := ord.Command
	if cmd.JobID != wantCmd.JobID ||
		cmd.KioskID != wantCmd.KioskID ||
		cmd.DisplayCode != wantCmd.DisplayCode ||
		cmd.DownloadURL != wantCmd.DownloadURL ||
		cmd.SHA256Checksum != wantCmd.SHA256Checksum ||
		cmd.TotalPages != wantCmd.TotalPages ||
		cmd.TotalSheets != wantCmd.TotalSheets ||
		cmd.Copies != wantCmd.Copies ||
		cmd.Duplex != wantCmd.Duplex ||
		cmd.StartFromPage != wantCmd.StartFromPage {
		t.Fatalf("PrintJobCommand field mismatch: got %+v, want %+v", cmd, wantCmd)
	}

	// Unknown order ID returns (nil, nil)
	unknown, err := s.GetOrder(ctx, "nonexistent-order")
	if err != nil {
		t.Fatalf("GetOrder for unknown order failed: %v", err)
	}
	if unknown != nil {
		t.Fatalf("expected nil for unknown order, got %+v", unknown)
	}
}

// Test C — TestCreateOrderRejectsDuplicates
func TestCreateOrderRejectsDuplicates(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	ord1 := sampleOrder("order-dup-1", "job-dup-1")
	if err := s.CreateOrder(ctx, ord1); err != nil {
		t.Fatalf("initial CreateOrder failed: %v", err)
	}

	// 1. Same OrderID with different job ID
	ordSameID := sampleOrder("order-dup-1", "job-diff-2")
	if err := s.CreateOrder(ctx, ordSameID); err == nil {
		t.Fatalf("expected error creating order with duplicate OrderID, got nil")
	}

	// 2. Different OrderID with duplicate JobID
	ordSameJob := sampleOrder("order-diff-2", "job-dup-1")
	if err := s.CreateOrder(ctx, ordSameJob); err == nil {
		t.Fatalf("expected error creating order with duplicate JobID, got nil")
	}

	// Verify original order unchanged
	original, err := s.GetOrder(ctx, "order-dup-1")
	if err != nil || original == nil {
		t.Fatalf("original order corrupted: %v, %+v", err, original)
	}
	if original.Command.JobID != "job-dup-1" {
		t.Fatalf("original order JobID changed: got %q, want %q", original.Command.JobID, "job-dup-1")
	}
}

// Test D — TestUpdateOrderStatus
func TestUpdateOrderStatus(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	ord := sampleOrder("order-status-test", "job-status-test")
	ord.Status = contracts.JobStatusUploaded
	if err := s.CreateOrder(ctx, ord); err != nil {
		t.Fatalf("CreateOrder failed: %v", err)
	}

	validStatuses := []contracts.JobStatus{
		contracts.JobStatusPendingPayment,
		contracts.JobStatusPaidWaiting,
		contracts.JobStatusDispatched,
		contracts.JobStatusPrinting,
		contracts.JobStatusCompleted,
		contracts.JobStatusFailedRefundable,
	}

	for _, status := range validStatuses {
		if err := s.UpdateOrderStatus(ctx, "order-status-test", status); err != nil {
			t.Fatalf("UpdateOrderStatus to %q failed: %v", status, err)
		}
		updated, err := s.GetOrder(ctx, "order-status-test")
		if err != nil {
			t.Fatalf("GetOrder failed: %v", err)
		}
		if updated.Status != status {
			t.Fatalf("expected status %q, got %q", status, updated.Status)
		}
	}

	// Unknown status rejected
	if err := s.UpdateOrderStatus(ctx, "order-status-test", "INVALID_STATUS"); err == nil {
		t.Fatalf("expected error for invalid status, got nil")
	}

	// Nonexistent order ID rejected
	if err := s.UpdateOrderStatus(ctx, "order-nonexistent", contracts.JobStatusCompleted); err == nil {
		t.Fatalf("expected error for nonexistent orderID, got nil")
	}
}

// Test E — TestPricingPersistence
func TestPricingPersistence(t *testing.T) {
	s, dbPath := newTestStore(t)
	ctx := context.Background()

	// 1. Error before pricing configured
	if _, err := s.GetPricePerSheetPaise(ctx); err == nil {
		t.Fatalf("expected error for unconfigured pricing, got nil")
	}

	// 2. Set valid positive integer price
	if err := s.SetPricePerSheetPaise(ctx, 350); err != nil {
		t.Fatalf("SetPricePerSheetPaise(350) failed: %v", err)
	}

	// 3. Read back price
	price, err := s.GetPricePerSheetPaise(ctx)
	if err != nil {
		t.Fatalf("GetPricePerSheetPaise failed: %v", err)
	}
	if price != 350 {
		t.Fatalf("expected price 350, got %d", price)
	}

	// 4. Replace price
	if err := s.SetPricePerSheetPaise(ctx, 400); err != nil {
		t.Fatalf("SetPricePerSheetPaise(400) failed: %v", err)
	}
	price, err = s.GetPricePerSheetPaise(ctx)
	if err != nil || price != 400 {
		t.Fatalf("expected price 400, got %d (err: %v)", price, err)
	}

	// 5. Reopen preserves configured price
	_ = s.Close()
	s2, err := Open(dbPath)
	if err != nil {
		t.Fatalf("reopen store failed: %v", err)
	}
	defer s2.Close()

	price, err = s2.GetPricePerSheetPaise(ctx)
	if err != nil || price != 400 {
		t.Fatalf("price after reopen mismatch: got %d (err: %v)", price, err)
	}

	// 6. Zero and negative prices rejected
	if err := s2.SetPricePerSheetPaise(ctx, 0); err == nil {
		t.Fatalf("expected error for price=0, got nil")
	}
	if err := s2.SetPricePerSheetPaise(ctx, -50); err == nil {
		t.Fatalf("expected error for negative price, got nil")
	}

	// 7. Failed update did not change previous price
	price, err = s2.GetPricePerSheetPaise(ctx)
	if err != nil || price != 400 {
		t.Fatalf("price changed after rejected updates: got %d, want 400", price)
	}
}

// Test F — TestRecordWebhookIdempotency
func TestRecordWebhookIdempotency(t *testing.T) {
	s, dbPath := newTestStore(t)
	ctx := context.Background()

	// 1. First record returns true, nil
	first, err := s.RecordWebhook(ctx, "evt_12345")
	if err != nil {
		t.Fatalf("RecordWebhook failed: %v", err)
	}
	if !first {
		t.Fatalf("expected true for first recording, got false")
	}

	// 2. Duplicate returns false, nil
	dup, err := s.RecordWebhook(ctx, "evt_12345")
	if err != nil {
		t.Fatalf("RecordWebhook duplicate failed: %v", err)
	}
	if dup {
		t.Fatalf("expected false for duplicate recording, got true")
	}

	// 3. Different ID returns true, nil
	diff, err := s.RecordWebhook(ctx, "evt_67890")
	if err != nil || !diff {
		t.Fatalf("expected true for different webhook ID, got %v (err: %v)", diff, err)
	}

	// 4. Case-sensitivity
	upper, err := s.RecordWebhook(ctx, "EVT_12345")
	if err != nil || !upper {
		t.Fatalf("expected true for case-distinct webhook ID, got %v (err: %v)", upper, err)
	}

	// 5. Empty and whitespace-only rejected
	if _, err := s.RecordWebhook(ctx, ""); err == nil {
		t.Fatalf("expected error for empty webhook ID, got nil")
	}
	if _, err := s.RecordWebhook(ctx, "   \t\n  "); err == nil {
		t.Fatalf("expected error for whitespace-only webhook ID, got nil")
	}

	// 6. Reopening preserves idempotency
	_ = s.Close()
	s2, err := Open(dbPath)
	if err != nil {
		t.Fatalf("reopen store failed: %v", err)
	}
	defer s2.Close()

	afterReopen, err := s2.RecordWebhook(ctx, "evt_12345")
	if err != nil || afterReopen {
		t.Fatalf("expected false after reopen for existing webhook ID, got %v (err: %v)", afterReopen, err)
	}
}

// Test G — TestUpdateAndGetJobProgress
func TestUpdateAndGetJobProgress(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	ord := sampleOrder("ord-prog-1", "job-prog-1")
	if err := s.CreateOrder(ctx, ord); err != nil {
		t.Fatalf("CreateOrder failed: %v", err)
	}

	time1 := time.Date(2026, time.October, 21, 10, 15, 0, 0, time.UTC)
	event1 := contracts.JobProgressEvent{
		JobID:         "job-prog-1",
		KioskID:       "kiosk-01",
		Status:        contracts.JobStatusPrinting,
		PagesPrinted:  3,
		SheetsPrinted: 2,
		ErrorReason:   "",
		Timestamp:     time1,
	}

	if err := s.UpdateJobProgress(ctx, event1); err != nil {
		t.Fatalf("UpdateJobProgress event1 failed: %v", err)
	}

	readEvt, err := s.GetJobProgress(ctx, "job-prog-1")
	if err != nil {
		t.Fatalf("GetJobProgress failed: %v", err)
	}
	if readEvt == nil {
		t.Fatalf("GetJobProgress returned nil")
	}

	if readEvt.JobID != event1.JobID ||
		readEvt.KioskID != event1.KioskID ||
		readEvt.Status != event1.Status ||
		readEvt.PagesPrinted != event1.PagesPrinted ||
		readEvt.SheetsPrinted != event1.SheetsPrinted ||
		readEvt.ErrorReason != event1.ErrorReason ||
		!readEvt.Timestamp.Equal(event1.Timestamp) {
		t.Fatalf("JobProgressEvent mismatch: got %+v, want %+v", readEvt, event1)
	}

	// Check associated order status was updated
	updatedOrd, err := s.GetOrder(ctx, "ord-prog-1")
	if err != nil || updatedOrd.Status != contracts.JobStatusPrinting {
		t.Fatalf("order status not updated to PRINTING: got %q (err: %v)", updatedOrd.Status, err)
	}

	// Update with later timestamp
	time2 := time.Date(2026, time.October, 21, 10, 16, 0, 0, time.UTC)
	event2 := contracts.JobProgressEvent{
		JobID:         "job-prog-1",
		KioskID:       "kiosk-01",
		Status:        contracts.JobStatusCompleted,
		PagesPrinted:  10,
		SheetsPrinted: 5,
		ErrorReason:   "",
		Timestamp:     time2,
	}
	if err := s.UpdateJobProgress(ctx, event2); err != nil {
		t.Fatalf("UpdateJobProgress event2 failed: %v", err)
	}

	readEvt2, err := s.GetJobProgress(ctx, "job-prog-1")
	if err != nil || readEvt2.PagesPrinted != 10 || readEvt2.Status != contracts.JobStatusCompleted {
		t.Fatalf("progress update 2 failed to persist: %+v (err: %v)", readEvt2, err)
	}
}

// Test H — TestJobProgressRejectsInvalidEvents
func TestJobProgressRejectsInvalidEvents(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	ord := sampleOrder("ord-h-1", "job-h-1")
	ord.Command.TotalPages = 10
	ord.Command.TotalSheets = 5
	ord.Command.KioskID = "kiosk-01"
	if err := s.CreateOrder(ctx, ord); err != nil {
		t.Fatalf("CreateOrder failed: %v", err)
	}

	validTime := time.Date(2026, time.October, 21, 11, 0, 0, 0, time.UTC)

	// Persist initial valid progress
	baseEvt := contracts.JobProgressEvent{
		JobID:         "job-h-1",
		KioskID:       "kiosk-01",
		Status:        contracts.JobStatusPrinting,
		PagesPrinted:  2,
		SheetsPrinted: 1,
		Timestamp:     validTime,
	}
	if err := s.UpdateJobProgress(ctx, baseEvt); err != nil {
		t.Fatalf("initial UpdateJobProgress failed: %v", err)
	}

	testCases := []struct {
		name  string
		event contracts.JobProgressEvent
	}{
		{
			name: "empty job ID",
			event: contracts.JobProgressEvent{
				JobID:     "",
				KioskID:   "kiosk-01",
				Status:    contracts.JobStatusPrinting,
				Timestamp: validTime.Add(time.Minute),
			},
		},
		{
			name: "empty kiosk ID",
			event: contracts.JobProgressEvent{
				JobID:     "job-h-1",
				KioskID:   "",
				Status:    contracts.JobStatusPrinting,
				Timestamp: validTime.Add(time.Minute),
			},
		},
		{
			name: "unknown status",
			event: contracts.JobProgressEvent{
				JobID:     "job-h-1",
				KioskID:   "kiosk-01",
				Status:    "UNKNOWN_STATUS",
				Timestamp: validTime.Add(time.Minute),
			},
		},
		{
			name: "negative pages printed",
			event: contracts.JobProgressEvent{
				JobID:        "job-h-1",
				KioskID:      "kiosk-01",
				Status:       contracts.JobStatusPrinting,
				PagesPrinted: -1,
				Timestamp:    validTime.Add(time.Minute),
			},
		},
		{
			name: "negative sheets printed",
			event: contracts.JobProgressEvent{
				JobID:         "job-h-1",
				KioskID:       "kiosk-01",
				Status:        contracts.JobStatusPrinting,
				SheetsPrinted: -1,
				Timestamp:     validTime.Add(time.Minute),
			},
		},
		{
			name: "zero timestamp",
			event: contracts.JobProgressEvent{
				JobID:        "job-h-1",
				KioskID:      "kiosk-01",
				Status:       contracts.JobStatusPrinting,
				PagesPrinted: 3,
				Timestamp:    time.Time{},
			},
		},
		{
			name: "unknown job ID",
			event: contracts.JobProgressEvent{
				JobID:        "job-nonexistent",
				KioskID:      "kiosk-01",
				Status:       contracts.JobStatusPrinting,
				PagesPrinted: 3,
				Timestamp:    validTime.Add(time.Minute),
			},
		},
		{
			name: "mismatched kiosk ID",
			event: contracts.JobProgressEvent{
				JobID:        "job-h-1",
				KioskID:      "kiosk-wrong",
				Status:       contracts.JobStatusPrinting,
				PagesPrinted: 3,
				Timestamp:    validTime.Add(time.Minute),
			},
		},
		{
			name: "pages printed exceeding total pages",
			event: contracts.JobProgressEvent{
				JobID:        "job-h-1",
				KioskID:      "kiosk-01",
				Status:       contracts.JobStatusPrinting,
				PagesPrinted: 11, // total is 10
				Timestamp:    validTime.Add(time.Minute),
			},
		},
		{
			name: "sheets printed exceeding total sheets",
			event: contracts.JobProgressEvent{
				JobID:         "job-h-1",
				KioskID:       "kiosk-01",
				Status:        contracts.JobStatusPrinting,
				SheetsPrinted: 6, // total is 5
				Timestamp:     validTime.Add(time.Minute),
			},
		},
		{
			name: "stale timestamp older than current progress",
			event: contracts.JobProgressEvent{
				JobID:        "job-h-1",
				KioskID:      "kiosk-01",
				Status:       contracts.JobStatusPrinting,
				PagesPrinted: 3,
				Timestamp:    validTime.Add(-time.Minute),
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			err := s.UpdateJobProgress(ctx, tc.event)
			if err == nil {
				t.Fatalf("expected error for %s, got nil", tc.name)
			}
		})
	}

	// Verify original valid progress unchanged
	current, err := s.GetJobProgress(ctx, "job-h-1")
	if err != nil || current.PagesPrinted != 2 {
		t.Fatalf("existing valid progress corrupted: %+v (err: %v)", current, err)
	}
}

// Test I — TestJobProgressPersistenceAfterReopen
func TestJobProgressPersistenceAfterReopen(t *testing.T) {
	s, dbPath := newTestStore(t)
	ctx := context.Background()

	ord := sampleOrder("ord-i-1", "job-i-1")
	if err := s.CreateOrder(ctx, ord); err != nil {
		t.Fatalf("CreateOrder failed: %v", err)
	}

	ts := time.Date(2026, time.October, 21, 12, 0, 0, 0, time.UTC)
	event := contracts.JobProgressEvent{
		JobID:         "job-i-1",
		KioskID:       "kiosk-01",
		Status:        contracts.JobStatusCompleted,
		PagesPrinted:  10,
		SheetsPrinted: 5,
		ErrorReason:   "",
		Timestamp:     ts,
	}

	if err := s.UpdateJobProgress(ctx, event); err != nil {
		t.Fatalf("UpdateJobProgress failed: %v", err)
	}

	_ = s.Close()

	s2, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Reopen failed: %v", err)
	}
	defer s2.Close()

	readEvt, err := s2.GetJobProgress(ctx, "job-i-1")
	if err != nil || readEvt == nil {
		t.Fatalf("GetJobProgress after reopen failed: %v", err)
	}

	if readEvt.Status != contracts.JobStatusCompleted || readEvt.PagesPrinted != 10 {
		t.Fatalf("unexpected progress after reopen: %+v", readEvt)
	}

	readOrd, err := s2.GetOrder(ctx, "ord-i-1")
	if err != nil || readOrd == nil || readOrd.Status != contracts.JobStatusCompleted {
		t.Fatalf("unexpected order after reopen: %+v (err: %v)", readOrd, err)
	}
}

// Test J — TestConcurrentWebhookIdempotency
func TestConcurrentWebhookIdempotency(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	const concurrency = 24
	webhookID := "evt_concurrent_test_999"

	var wg sync.WaitGroup
	results := make(chan bool, concurrency)
	errors := make(chan error, concurrency)

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			inserted, err := s.RecordWebhook(ctx, webhookID)
			if err != nil {
				errors <- err
				return
			}
			results <- inserted
		}()
	}

	wg.Wait()
	close(results)
	close(errors)

	for err := range errors {
		t.Fatalf("concurrent RecordWebhook produced error: %v", err)
	}

	trueCount := 0
	falseCount := 0
	for res := range results {
		if res {
			trueCount++
		} else {
			falseCount++
		}
	}

	if trueCount != 1 {
		t.Fatalf("expected exactly 1 call to return true, got %d", trueCount)
	}
	if falseCount != concurrency-1 {
		t.Fatalf("expected %d calls to return false, got %d", concurrency-1, falseCount)
	}
}

// Test K — TestRejectsInvalidInput
func TestRejectsInvalidInput(t *testing.T) {
	s, _ := newTestStore(t)

	// 1. Open with empty path
	if _, err := Open(""); err == nil {
		t.Fatalf("expected error for empty dbPath, got nil")
	}

	// 2. Nil context
	if err := s.CreateOrder(nil, sampleOrder("ord-k-nil", "job-k-nil")); err == nil {
		t.Fatalf("expected error for nil context in CreateOrder, got nil")
	}
	if _, err := s.GetOrder(nil, "some-order"); err == nil {
		t.Fatalf("expected error for nil context in GetOrder, got nil")
	}
	if err := s.UpdateOrderStatus(nil, "some-order", contracts.JobStatusPrinting); err == nil {
		t.Fatalf("expected error for nil context in UpdateOrderStatus, got nil")
	}
	if err := s.SetPricePerSheetPaise(nil, 100); err == nil {
		t.Fatalf("expected error for nil context in SetPricePerSheetPaise, got nil")
	}
	if _, err := s.GetPricePerSheetPaise(nil); err == nil {
		t.Fatalf("expected error for nil context in GetPricePerSheetPaise, got nil")
	}
	if _, err := s.RecordWebhook(nil, "evt-nil"); err == nil {
		t.Fatalf("expected error for nil context in RecordWebhook, got nil")
	}
	if err := s.UpdateJobProgress(nil, contracts.JobProgressEvent{JobID: "j"}); err == nil {
		t.Fatalf("expected error for nil context in UpdateJobProgress, got nil")
	}
	if _, err := s.GetJobProgress(nil, "j"); err == nil {
		t.Fatalf("expected error for nil context in GetJobProgress, got nil")
	}

	// 3. Canceled context
	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := s.CreateOrder(canceledCtx, sampleOrder("ord-k-c", "job-k-c")); err == nil {
		t.Fatalf("expected error for canceled context in CreateOrder, got nil")
	}
	if _, err := s.GetOrder(canceledCtx, "some-order"); err == nil {
		t.Fatalf("expected error for canceled context in GetOrder, got nil")
	}
	if err := s.UpdateOrderStatus(canceledCtx, "some-order", contracts.JobStatusPrinting); err == nil {
		t.Fatalf("expected error for canceled context in UpdateOrderStatus, got nil")
	}

	// 4. Invalid Order commands
	ctx := context.Background()
	invalidOrders := []struct {
		name  string
		order Order
	}{
		{
			name: "empty OrderID",
			order: func() Order {
				o := sampleOrder("", "job-inv-1")
				return o
			}(),
		},
		{
			name: "empty JobID",
			order: func() Order {
				o := sampleOrder("ord-inv-2", "")
				return o
			}(),
		},
		{
			name: "empty KioskID",
			order: func() Order {
				o := sampleOrder("ord-inv-3", "job-inv-3")
				o.Command.KioskID = ""
				return o
			}(),
		},
		{
			name: "empty DisplayCode",
			order: func() Order {
				o := sampleOrder("ord-inv-4", "job-inv-4")
				o.Command.DisplayCode = ""
				return o
			}(),
		},
		{
			name: "zero TotalPages",
			order: func() Order {
				o := sampleOrder("ord-inv-5", "job-inv-5")
				o.Command.TotalPages = 0
				return o
			}(),
		},
		{
			name: "zero TotalSheets",
			order: func() Order {
				o := sampleOrder("ord-inv-6", "job-inv-6")
				o.Command.TotalSheets = 0
				return o
			}(),
		},
		{
			name: "zero Copies",
			order: func() Order {
				o := sampleOrder("ord-inv-7", "job-inv-7")
				o.Command.Copies = 0
				return o
			}(),
		},
		{
			name: "zero StartFromPage",
			order: func() Order {
				o := sampleOrder("ord-inv-8", "job-inv-8")
				o.Command.StartFromPage = 0
				return o
			}(),
		},
		{
			name: "StartFromPage exceeds TotalPages",
			order: func() Order {
				o := sampleOrder("ord-inv-9", "job-inv-9")
				o.Command.TotalPages = 5
				o.Command.StartFromPage = 6
				return o
			}(),
		},
		{
			name: "invalid Duplex",
			order: func() Order {
				o := sampleOrder("ord-inv-10", "job-inv-10")
				o.Command.Duplex = "TRIPLEX"
				return o
			}(),
		},
		{
			name: "invalid Status",
			order: func() Order {
				o := sampleOrder("ord-inv-11", "job-inv-11")
				o.Status = "BOGUS_STATUS"
				return o
			}(),
		},
		{
			name: "negative PricePaise",
			order: func() Order {
				o := sampleOrder("ord-inv-12", "job-inv-12")
				o.PricePaise = -10
				return o
			}(),
		},
	}

	for _, tc := range invalidOrders {
		t.Run(tc.name, func(t *testing.T) {
			if err := s.CreateOrder(ctx, tc.order); err == nil {
				t.Fatalf("expected error for invalid order %s, got nil", tc.name)
			}
		})
	}
}

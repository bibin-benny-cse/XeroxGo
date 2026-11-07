package store

import (
	"context"
	"testing"
	"time"
)

func TestConversionJobCRUDAndTransitions(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	now := time.Now().UTC()
	job := ConversionJob{
		ID:               "conv_test01",
		Token:            "tok_secret123",
		OriginalFilename: "document.docx",
		FileSize:         102400,
		Status:           ConversionStatusQueued,
		InputPath:        "/tmp/input.docx",
		OutputPath:       "/tmp/output.pdf",
		CreatedAt:        now,
		UpdatedAt:        now,
		ExpiresAt:        now.Add(1 * time.Hour),
	}

	// 1. Create conversion job
	if err := s.CreateConversion(ctx, job); err != nil {
		t.Fatalf("CreateConversion failed: %v", err)
	}

	// 2. Retrieve conversion job
	fetched, err := s.GetConversion(ctx, job.ID)
	if err != nil {
		t.Fatalf("GetConversion failed: %v", err)
	}
	if fetched == nil {
		t.Fatalf("expected job to be found, got nil")
	}
	if fetched.ID != job.ID || fetched.Token != job.Token || fetched.Status != ConversionStatusQueued {
		t.Fatalf("unexpected fetched job: %+v", fetched)
	}

	// 3. Atomically claim queued job
	claimed, err := s.ClaimNextQueuedConversion(ctx)
	if err != nil {
		t.Fatalf("ClaimNextQueuedConversion failed: %v", err)
	}
	if claimed == nil {
		t.Fatalf("expected to claim job, got nil")
	}
	if claimed.ID != job.ID || claimed.Status != ConversionStatusProcessing {
		t.Fatalf("expected claimed job in processing state, got %+v", claimed)
	}

	// Calling claim again should return nil since no more queued jobs
	claimed2, err := s.ClaimNextQueuedConversion(ctx)
	if err != nil {
		t.Fatalf("ClaimNextQueuedConversion second call failed: %v", err)
	}
	if claimed2 != nil {
		t.Fatalf("expected nil when no jobs are queued, got %+v", claimed2)
	}

	// 4. Update status to completed
	if err := s.UpdateConversionStatus(ctx, job.ID, ConversionStatusCompleted, "", "/tmp/final.pdf"); err != nil {
		t.Fatalf("UpdateConversionStatus failed: %v", err)
	}

	fetchedUpdated, err := s.GetConversion(ctx, job.ID)
	if err != nil {
		t.Fatalf("GetConversion after update failed: %v", err)
	}
	if fetchedUpdated.Status != ConversionStatusCompleted || fetchedUpdated.OutputPath != "/tmp/final.pdf" {
		t.Fatalf("unexpected updated job: %+v", fetchedUpdated)
	}
}

func TestConversionStaleRecovery(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	now := time.Now().UTC()
	job := ConversionJob{
		ID:               "conv_stale01",
		Token:            "tok_stale",
		OriginalFilename: "report.pptx",
		FileSize:         50000,
		Status:           ConversionStatusProcessing, // Left in processing from crash
		InputPath:        "/tmp/stale.pptx",
		OutputPath:       "/tmp/stale.pdf",
		CreatedAt:        now.Add(-5 * time.Minute),
		UpdatedAt:        now.Add(-5 * time.Minute),
		ExpiresAt:        now.Add(1 * time.Hour),
	}

	if err := s.CreateConversion(ctx, job); err != nil {
		t.Fatalf("CreateConversion failed: %v", err)
	}

	// Run recovery
	if err := s.RecoverStaleConversions(ctx); err != nil {
		t.Fatalf("RecoverStaleConversions failed: %v", err)
	}

	// Verify it was reset to queued
	fetched, err := s.GetConversion(ctx, job.ID)
	if err != nil {
		t.Fatalf("GetConversion failed: %v", err)
	}
	if fetched.Status != ConversionStatusQueued {
		t.Fatalf("expected status 'queued', got %q", fetched.Status)
	}

	// Should now be claimable
	claimed, err := s.ClaimNextQueuedConversion(ctx)
	if err != nil || claimed == nil || claimed.ID != job.ID {
		t.Fatalf("expected claimed job %q, got: %+v (err: %v)", job.ID, claimed, err)
	}
}

func TestConversionExpirationCleanup(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	now := time.Now().UTC()
	jobExpired := ConversionJob{
		ID:               "conv_exp01",
		Token:            "tok_exp",
		OriginalFilename: "old.xlsx",
		FileSize:         1000,
		Status:           ConversionStatusCompleted,
		InputPath:        "/tmp/expired.xlsx",
		OutputPath:       "/tmp/expired.pdf",
		CreatedAt:        now.Add(-2 * time.Hour),
		UpdatedAt:        now.Add(-2 * time.Hour),
		ExpiresAt:        now.Add(-1 * time.Hour), // Expired 1 hour ago
	}
	jobActive := ConversionJob{
		ID:               "conv_active01",
		Token:            "tok_active",
		OriginalFilename: "active.docx",
		FileSize:         2000,
		Status:           ConversionStatusQueued,
		InputPath:        "/tmp/active.docx",
		OutputPath:       "/tmp/active.pdf",
		CreatedAt:        now,
		UpdatedAt:        now,
		ExpiresAt:        now.Add(1 * time.Hour), // Valid
	}

	if err := s.CreateConversion(ctx, jobExpired); err != nil {
		t.Fatalf("CreateConversion expired failed: %v", err)
	}
	if err := s.CreateConversion(ctx, jobActive); err != nil {
		t.Fatalf("CreateConversion active failed: %v", err)
	}

	// Run cleanup
	paths, err := s.DeleteExpiredConversions(ctx, now)
	if err != nil {
		t.Fatalf("DeleteExpiredConversions failed: %v", err)
	}

	if len(paths) != 2 {
		t.Fatalf("expected 2 paths returned for expired job, got %v", paths)
	}

	// Expired job should no longer exist
	fExp, err := s.GetConversion(ctx, jobExpired.ID)
	if err != nil {
		t.Fatalf("GetConversion failed: %v", err)
	}
	if fExp != nil {
		t.Fatalf("expected expired job to be deleted, got: %+v", fExp)
	}

	// Active job should still exist
	fAct, err := s.GetConversion(ctx, jobActive.ID)
	if err != nil || fAct == nil {
		t.Fatalf("expected active job to remain, got: %+v (err: %v)", fAct, err)
	}
}

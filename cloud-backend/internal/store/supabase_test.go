package store

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"xeroxgo/pkg/contracts"
)

func TestSupabaseIntegration(t *testing.T) {
	uri := os.Getenv("SUPABASE_URI")
	if uri == "" {
		t.Skip("skipping Supabase live integration test (SUPABASE_URI not set)")
	}

	s, err := Open(uri)
	if err != nil {
		t.Fatalf("failed to open store with Supabase URI: %v", err)
	}
	defer s.Close()

	ctx := context.Background()

	// 1. Pricing test
	if err := s.SetPricePerSheetPaise(ctx, 500); err != nil {
		t.Fatalf("SetPricePerSheetPaise failed: %v", err)
	}
	price, err := s.GetPricePerSheetPaise(ctx)
	if err != nil || price != 500 {
		t.Fatalf("GetPricePerSheetPaise failed: %v, got %d", err, price)
	}

	// 2. Order creation test
	testOrderID := fmt.Sprintf("order-it-%d", time.Now().Unix())
	testJobID := fmt.Sprintf("job-it-%d", time.Now().Unix())
	order := Order{
		OrderID: testOrderID,
		Command: contracts.PrintJobCommand{
			JobID:          testJobID,
			KioskID:        "kiosk-demo",
			DisplayCode:    "1234",
			DownloadURL:    "https://example.com/test.pdf",
			SHA256Checksum: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
			TotalPages:     2,
			TotalSheets:    1,
			Copies:         1,
			Duplex:         contracts.DuplexLongEdge,
			StartFromPage:  1,
		},
		Status:     contracts.JobStatusNormalized,
		PricePaise: 500,
	}

	if err := s.CreateOrder(ctx, order); err != nil {
		t.Fatalf("CreateOrder failed: %v", err)
	}

	retrieved, err := s.GetOrder(ctx, testOrderID)
	if err != nil || retrieved == nil {
		t.Fatalf("GetOrder failed: %v", err)
	}
	if retrieved.Command.DisplayCode != "1234" {
		t.Fatalf("retrieved display code mismatch: %s", retrieved.Command.DisplayCode)
	}

	// 3. Webhook idempotency test
	webhookID := fmt.Sprintf("wh-it-%d", time.Now().Unix())
	first, err := s.RecordWebhook(ctx, webhookID)
	if err != nil || !first {
		t.Fatalf("expected first RecordWebhook to succeed, got %v, err=%v", first, err)
	}
	second, err := s.RecordWebhook(ctx, webhookID)
	if err != nil || second {
		t.Fatalf("expected second RecordWebhook to return false, got %v, err=%v", second, err)
	}

	// 4. Job progress update test
	event := contracts.JobProgressEvent{
		JobID:         testJobID,
		KioskID:       "kiosk-demo",
		Status:        contracts.JobStatusCompleted,
		PagesPrinted:  2,
		SheetsPrinted: 1,
		Timestamp:     time.Now().UTC(),
	}
	if err := s.UpdateJobProgress(ctx, event); err != nil {
		t.Fatalf("UpdateJobProgress failed: %v", err)
	}

	prog, err := s.GetJobProgress(ctx, testJobID)
	if err != nil || prog == nil {
		t.Fatalf("GetJobProgress failed: %v", err)
	}
	if prog.Status != contracts.JobStatusCompleted || prog.PagesPrinted != 2 {
		t.Fatalf("unexpected progress state: %+v", prog)
	}

	// 5. Promotional Ads Fallback Test
	if err := s.SeedDefaultAds(ctx); err != nil {
		t.Fatalf("SeedDefaultAds cleanup failed: %v", err)
	}
	allAds, err := s.GetAllAds(ctx)
	if err != nil {
		t.Fatalf("GetAllAds failed: %v", err)
	}
	t.Logf("Supabase custom ads count: %d", len(allAds))
	for _, ad := range allAds {
		t.Logf("  Custom Ad: id=%s title=%s", ad.ID, ad.Title)
	}

	activeAds, err := s.GetActiveAds(ctx)
	if err != nil {
		t.Fatalf("GetActiveAds failed: %v", err)
	}
	if len(allAds) == 0 {
		if len(activeAds) != 3 {
			t.Fatalf("expected 3 default fallback ads when 0 custom ads, got %d", len(activeAds))
		}
		if activeAds[0].Title != "Quick & Easy" {
			t.Errorf("expected default ad title 'Quick & Easy', got %s", activeAds[0].Title)
		}
	}
}

package store_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"xeroxgo/cloud-backend/internal/store"
	"xeroxgo/pkg/contracts"
)

func TestPricingDual(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "pricing_test.db")

	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer s.Close()

	// 1. Initial pricing unconfigured error
	if _, err := s.GetPricing(ctx); err == nil {
		t.Fatalf("expected error for unconfigured pricing, got nil")
	}

	// 2. Set custom dual pricing
	if err := s.SetPricing(ctx, 350, 1500); err != nil {
		t.Fatalf("SetPricing failed: %v", err)
	}

	p2, err := s.GetPricing(ctx)
	if err != nil {
		t.Fatalf("GetPricing after set failed: %v", err)
	}
	if p2.BWPricePerSheetPaise != 350 || p2.ColorPricePerSheetPaise != 1500 {
		t.Fatalf("expected (350, 1500), got (%d, %d)", p2.BWPricePerSheetPaise, p2.ColorPricePerSheetPaise)
	}

	// 3. Legacy bridge check
	legacyBW, err := s.GetPricePerSheetPaise(ctx)
	if err != nil {
		t.Fatalf("GetPricePerSheetPaise failed: %v", err)
	}
	if legacyBW != 350 {
		t.Fatalf("expected legacy BW price 350, got %d", legacyBW)
	}

	// 4. Validation: reject negative/zero prices
	if err := s.SetPricing(ctx, 0, 1000); err == nil {
		t.Fatalf("expected error for bwPrice=0, got nil")
	}
	if err := s.SetPricing(ctx, 200, -50); err == nil {
		t.Fatalf("expected error for colorPrice < 0, got nil")
	}

	// 5. Test detailed 4-tier pricing and colorEnabled flag
	if err := s.SetDetailedPricing(ctx, 200, 150, 1000, 750, true); err != nil {
		t.Fatalf("SetDetailedPricing failed: %v", err)
	}
	p3, err := s.GetPricing(ctx)
	if err != nil {
		t.Fatalf("GetPricing failed: %v", err)
	}
	if p3.BWSimplexPricePaise != 200 || p3.BWDuplexPricePaise != 150 ||
		p3.ColorSimplexPricePaise != 1000 || p3.ColorDuplexPricePaise != 750 ||
		!p3.ColorEnabled {
		t.Fatalf("unexpected p3: %+v", p3)
	}
}

func TestAnalyticsAndOrderHistory(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "analytics_test.db")

	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer s.Close()

	now := time.Now().UTC()

	// Seed completed order 1 (B&W)
	ord1 := store.Order{
		OrderID: "ord_analytics_1",
		Command: contracts.PrintJobCommand{
			JobID:          "job_analytics_1",
			KioskID:        "kiosk-demo",
			DisplayCode:    "A101",
			DownloadURL:    "http://example.com/doc1.pdf",
			SHA256Checksum: "abc1",
			TotalPages:     4,
			TotalSheets:    2,
			Copies:         1,
			Duplex:         contracts.DuplexLongEdge,
			StartFromPage:  1,
		},
		Status:     contracts.JobStatusCompleted,
		PricePaise: 400,
		ColorMode:  "BW",
		CreatedAt:  now.Add(-2 * time.Hour),
	}
	if err := s.CreateOrder(ctx, ord1); err != nil {
		t.Fatalf("CreateOrder 1 failed: %v", err)
	}

	// Seed completed order 2 (Color)
	ord2 := store.Order{
		OrderID: "ord_analytics_2",
		Command: contracts.PrintJobCommand{
			JobID:          "job_analytics_2",
			KioskID:        "kiosk-demo",
			DisplayCode:    "B202",
			DownloadURL:    "http://example.com/doc2.pdf",
			SHA256Checksum: "abc2",
			TotalPages:     5,
			TotalSheets:    5,
			Copies:         1,
			Duplex:         contracts.DuplexSimplex,
			StartFromPage:  1,
		},
		Status:     contracts.JobStatusCompleted,
		PricePaise: 5000,
		ColorMode:  "COLOR",
		CreatedAt:  now.Add(-1 * time.Hour),
	}
	if err := s.CreateOrder(ctx, ord2); err != nil {
		t.Fatalf("CreateOrder 2 failed: %v", err)
	}

	// Seed failed order 3
	ord3 := store.Order{
		OrderID: "ord_analytics_3",
		Command: contracts.PrintJobCommand{
			JobID:          "job_analytics_3",
			KioskID:        "kiosk-demo",
			DisplayCode:    "C303",
			DownloadURL:    "http://example.com/doc3.pdf",
			SHA256Checksum: "abc3",
			TotalPages:     2,
			TotalSheets:    1,
			Copies:         1,
			Duplex:         contracts.DuplexSimplex,
			StartFromPage:  1,
		},
		Status:     contracts.JobStatusFailedRefundable,
		PricePaise: 200,
		ColorMode:  "BW",
		CreatedAt:  now.Add(-30 * time.Minute),
	}
	if err := s.CreateOrder(ctx, ord3); err != nil {
		t.Fatalf("CreateOrder 3 failed: %v", err)
	}

	// 1. Test GetAnalytics
	stats, err := s.GetAnalytics(ctx, "7d")
	if err != nil {
		t.Fatalf("GetAnalytics failed: %v", err)
	}

	if stats.TotalRevenuePaise != 5400 {
		t.Errorf("expected TotalRevenuePaise 5400, got %d", stats.TotalRevenuePaise)
	}
	if stats.TotalSheetsPrinted != 7 {
		t.Errorf("expected TotalSheetsPrinted 7, got %d", stats.TotalSheetsPrinted)
	}
	if stats.TotalPagesPrinted != 9 {
		t.Errorf("expected TotalPagesPrinted 9, got %d", stats.TotalPagesPrinted)
	}
	if stats.TotalOrders != 3 {
		t.Errorf("expected TotalOrders 3, got %d", stats.TotalOrders)
	}
	if stats.CompletedOrders != 2 {
		t.Errorf("expected CompletedOrders 2, got %d", stats.CompletedOrders)
	}
	if stats.FailedOrders != 1 {
		t.Errorf("expected FailedOrders 1, got %d", stats.FailedOrders)
	}
	if stats.AverageOrderPaise != 2700 {
		t.Errorf("expected AverageOrderPaise 2700, got %d", stats.AverageOrderPaise)
	}
	if len(stats.DailyTrends) != 7 {
		t.Errorf("expected 7 daily trend entries, got %d", len(stats.DailyTrends))
	}

	// 2. Test GetOrderHistory (all)
	resAll, err := s.GetOrderHistory(ctx, store.OrderHistoryFilter{Limit: 10})
	if err != nil {
		t.Fatalf("GetOrderHistory all failed: %v", err)
	}
	if resAll.Total != 3 {
		t.Errorf("expected Total 3, got %d", resAll.Total)
	}
	if len(resAll.Orders) != 3 {
		t.Errorf("expected 3 items in Orders, got %d", len(resAll.Orders))
	}

	// 3. Test filter by Status
	resComp, err := s.GetOrderHistory(ctx, store.OrderHistoryFilter{Status: "COMPLETED"})
	if err != nil {
		t.Fatalf("GetOrderHistory status failed: %v", err)
	}
	if resComp.Total != 2 {
		t.Errorf("expected 2 completed orders, got %d", resComp.Total)
	}

	// 4. Test Search filter
	resSearch, err := s.GetOrderHistory(ctx, store.OrderHistoryFilter{Search: "B202"})
	if err != nil {
		t.Fatalf("GetOrderHistory search failed: %v", err)
	}
	if resSearch.Total != 1 || resSearch.Orders[0].DisplayCode != "B202" {
		t.Errorf("expected 1 match for search 'B202', got %+v", resSearch)
	}
}

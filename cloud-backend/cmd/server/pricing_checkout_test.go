package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"xeroxgo/cloud-backend/internal/store"
	"xeroxgo/cloud-backend/internal/ws"
	"xeroxgo/pkg/contracts"
)

func setupPricingTestServer(t *testing.T) (*appServer, *http.ServeMux, string) {
	t.Helper()
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "pricing_test.db")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open test store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	jobsDir := filepath.Join(tmpDir, "jobs")
	if err := os.MkdirAll(jobsDir, 0700); err != nil {
		t.Fatalf("failed to create jobs dir: %v", err)
	}

	webhookSecret := []byte("whsec_test_secret_32_characters_ok")
	cfg := &serverConfig{
		addr:               "localhost:0",
		dbPath:             dbPath,
		jobsDir:            jobsDir,
		pricePerSheetPaise: 200,
		downloadSecret:     []byte("test_secret_key_1234567890"),
		webhookSecret:      webhookSecret,
		devMode:            true,
	}

	hub := ws.NewHub(s)
	app := &appServer{
		cfg:      cfg,
		db:       s,
		hub:      hub,
		convWake: make(chan struct{}, 10),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/k/", app.handleUpload)
	mux.HandleFunc("/api/pricing", app.handlePublicPricing)
	mux.HandleFunc("/api/webhooks/razorpay", app.handleRazorpayWebhook)
	mux.HandleFunc("/api/orders/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path[len(r.URL.Path)-7:] == "/status" {
			app.handleOrderStatus(w, r)
		} else {
			http.NotFound(w, r)
		}
	})

	return app, mux, string(webhookSecret)
}

func signRazorpayPayload(payload []byte, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(payload)
	return hex.EncodeToString(mac.Sum(nil))
}

func uploadDocForPricing(t *testing.T, mux *http.ServeMux, pdfPath string, copies string, duplex string, colorMode string, pageRange string) map[string]interface{} {
	t.Helper()
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	fileBytes, err := os.ReadFile(pdfPath)
	if err != nil {
		t.Fatalf("failed to read test pdf: %v", err)
	}

	part, _ := writer.CreateFormFile("file", filepath.Base(pdfPath))
	_, _ = part.Write(fileBytes)
	_ = writer.WriteField("copies", copies)
	_ = writer.WriteField("duplex", duplex)
	_ = writer.WriteField("color_mode", colorMode)
	if pageRange != "" {
		_ = writer.WriteField("page_range", pageRange)
	}
	writer.Close()

	req := httptest.NewRequest(http.MethodPost, "/api/k/kiosk_test/upload", body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	rr := httptest.NewRecorder()

	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("upload failed: status %d, body: %s", rr.Code, rr.Body.String())
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal upload response: %v", err)
	}
	return resp
}

// Requirement 17: Test 1/3/5-page jobs, multiple copies, custom ranges, simplex/duplex, and B&W/color.
func TestPricing_TiersAndConfigurations(t *testing.T) {
	app, mux, _ := setupPricingTestServer(t)
	tmpDir := t.TempDir()

	// Configure explicit 4-tier pricing in database:
	// BW Simplex: 200 paise, BW Duplex: 150 paise
	// Color Simplex: 1000 paise, Color Duplex: 750 paise
	ctx := context.Background()
	if err := app.db.SetDetailedPricing(ctx, 200, 150, 1000, 750, true); err != nil {
		t.Fatalf("SetDetailedPricing failed: %v", err)
	}

	doc1p := createTestPDFWithPages(t, tmpDir, 1)
	doc3p := createTestPDFWithPages(t, tmpDir, 3)
	doc5p := createTestPDFWithPages(t, tmpDir, 5)

	tests := []struct {
		name          string
		pdfPath       string
		copies        string
		duplex        string
		colorMode     string
		pageRange     string
		wantPages     int
		wantSheets    int
		wantUnitPrice int64
		wantTotalCost int64
	}{
		{
			name:          "1-page B&W Simplex 1 copy",
			pdfPath:       doc1p,
			copies:        "1",
			duplex:        "SIMPLEX",
			colorMode:     "BW",
			wantPages:     1,
			wantSheets:    1,
			wantUnitPrice: 200,
			wantTotalCost: 200, // 1 * 1 * 200
		},
		{
			name:          "3-page B&W Duplex 2 copies",
			pdfPath:       doc3p,
			copies:        "2",
			duplex:        "DUPLEX_LONG_EDGE",
			colorMode:     "BW",
			wantPages:     3,
			wantSheets:    4,   // ceil(3/2) * 2 = 4
			wantUnitPrice: 150, // Duplex rate
			wantTotalCost: 900, // 6 printed sides * 150 paise = 900 paise (₹9.00)
		},
		{
			name:          "5-page Color Simplex 1 copy",
			pdfPath:       doc5p,
			copies:        "1",
			duplex:        "SIMPLEX",
			colorMode:     "COLOR",
			wantPages:     5,
			wantSheets:    5,
			wantUnitPrice: 1000,
			wantTotalCost: 5000, // 5 * 1 * 1000
		},
		{
			name:          "5-page Color Duplex 2 copies",
			pdfPath:       doc5p,
			copies:        "2",
			duplex:        "DUPLEX_LONG_EDGE",
			colorMode:     "COLOR",
			wantPages:     5,
			wantSheets:    6,    // ceil(5/2) * 2 = 6
			wantUnitPrice: 750,  // Color duplex rate
			wantTotalCost: 7500, // 10 printed sides * 750 paise = 7500 paise (₹75.00)
		},
		{
			name:          "5-page custom range 1,3,5 B&W Duplex 2 copies",
			pdfPath:       doc5p,
			copies:        "2",
			duplex:        "DUPLEX_LONG_EDGE",
			colorMode:     "BW",
			pageRange:     "1,3,5",
			wantPages:     3,
			wantSheets:    4,   // ceil(3/2) * 2 = 4
			wantUnitPrice: 150, // B&W duplex rate
			wantTotalCost: 900, // 3 pages * 2 copies = 6 sides * 150 = 900 paise
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp := uploadDocForPricing(t, mux, tc.pdfPath, tc.copies, tc.duplex, tc.colorMode, tc.pageRange)

			gotPages := int(resp["total_pages"].(float64))
			gotSheets := int(resp["total_sheets"].(float64))
			gotUnitPrice := int64(resp["unit_price_paise"].(float64))
			gotTotalCost := int64(resp["total_price_paise"].(float64))

			if gotPages != tc.wantPages {
				t.Errorf("total_pages = %d, want %d", gotPages, tc.wantPages)
			}
			if gotSheets != tc.wantSheets {
				t.Errorf("total_sheets = %d, want %d", gotSheets, tc.wantSheets)
			}
			if gotUnitPrice != tc.wantUnitPrice {
				t.Errorf("unit_price_paise = %d, want %d", gotUnitPrice, tc.wantUnitPrice)
			}
			if gotTotalCost != tc.wantTotalCost {
				t.Errorf("total_price_paise = %d, want %d", gotTotalCost, tc.wantTotalCost)
			}
		})
	}
}

// Requirement 18: Test fractional prices such as ₹4.50 (450 paise).
func TestPricing_FractionalRupeesCalculation(t *testing.T) {
	app, mux, _ := setupPricingTestServer(t)
	tmpDir := t.TempDir()

	ctx := context.Background()
	// Set BW Simplex to 150 paise (₹1.50 per side)
	if err := app.db.SetDetailedPricing(ctx, 150, 100, 500, 400, false); err != nil {
		t.Fatalf("SetDetailedPricing failed: %v", err)
	}

	// 3 content pages, 1 copy simplex = 3 sides * 150 paise = 450 paise (₹4.50)
	doc3p := createTestPDFWithPages(t, tmpDir, 3)
	resp := uploadDocForPricing(t, mux, doc3p, "1", "SIMPLEX", "BW", "")

	orderID := resp["order_id"].(string)
	totalCost := int64(resp["total_price_paise"].(float64))
	unitPrice := int64(resp["unit_price_paise"].(float64))

	if unitPrice != 150 {
		t.Fatalf("unit_price_paise = %d, want 150", unitPrice)
	}
	if totalCost != 450 {
		t.Fatalf("total_price_paise = %d, want 450 (₹4.50)", totalCost)
	}

	// Verify database record has exactly 450 paise
	order, err := app.db.GetOrder(ctx, orderID)
	if err != nil {
		t.Fatalf("GetOrder failed: %v", err)
	}
	if order.PricePaise != 450 {
		t.Fatalf("order.PricePaise in DB = %d, want 450", order.PricePaise)
	}
}

// Requirement 11 & 18: Preserve the agreed price after order creation, even if admin pricing changes.
func TestPricing_PreservesAgreedPriceWhenAdminPricingChanges(t *testing.T) {
	app, mux, _ := setupPricingTestServer(t)
	tmpDir := t.TempDir()

	ctx := context.Background()
	// Initial admin pricing: BW Simplex = 200 paise
	if err := app.db.SetDetailedPricing(ctx, 200, 150, 1000, 750, false); err != nil {
		t.Fatalf("SetDetailedPricing failed: %v", err)
	}

	doc3p := createTestPDFWithPages(t, tmpDir, 3)
	// Order 1: 3 pages * 1 copy * 200 paise = 600 paise
	resp1 := uploadDocForPricing(t, mux, doc3p, "1", "SIMPLEX", "BW", "")
	orderID1 := resp1["order_id"].(string)
	if int64(resp1["total_price_paise"].(float64)) != 600 {
		t.Fatalf("order 1 total_price_paise = %v, want 600", resp1["total_price_paise"])
	}

	// Now admin changes rate: BW Simplex doubled to 400 paise!
	if err := app.db.SetDetailedPricing(ctx, 400, 300, 2000, 1500, false); err != nil {
		t.Fatalf("admin price change failed: %v", err)
	}

	// Order 1's price MUST NOT CHANGE: query status endpoint
	req := httptest.NewRequest(http.MethodGet, "/api/orders/"+orderID1+"/status", nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status request failed: %d", rr.Code)
	}

	var statusResp map[string]interface{}
	_ = json.Unmarshal(rr.Body.Bytes(), &statusResp)
	if int64(statusResp["price_paise"].(float64)) != 600 {
		t.Errorf("Order 1 price_paise after admin rate increase = %v, want preserved 600", statusResp["price_paise"])
	}

	// Stored DB order must still have 600 paise
	order1, err := app.db.GetOrder(ctx, orderID1)
	if err != nil {
		t.Fatalf("GetOrder failed: %v", err)
	}
	if order1.PricePaise != 600 {
		t.Errorf("Order 1 DB price_paise = %d, want 600", order1.PricePaise)
	}

	// New Order 2 created after admin update uses the new 400 paise rate:
	// 3 pages * 1 copy * 400 paise = 1200 paise
	resp2 := uploadDocForPricing(t, mux, doc3p, "1", "SIMPLEX", "BW", "")
	if int64(resp2["total_price_paise"].(float64)) != 1200 {
		t.Errorf("Order 2 total_price_paise = %v, want 1200", resp2["total_price_paise"])
	}
}

// Requirement 10 & 19: Verify the actual Razorpay order amount equals the displayed and stored amount in paise.
func TestPricing_RazorpayWebhookAmountVerification(t *testing.T) {
	app, mux, webhookSecret := setupPricingTestServer(t)
	tmpDir := t.TempDir()

	ctx := context.Background()
	if err := app.db.SetDetailedPricing(ctx, 200, 150, 1000, 750, false); err != nil {
		t.Fatalf("SetDetailedPricing failed: %v", err)
	}

	// Create order for 3 pages simplex = 600 paise
	doc3p := createTestPDFWithPages(t, tmpDir, 3)
	resp := uploadDocForPricing(t, mux, doc3p, "1", "SIMPLEX", "BW", "")
	orderID := resp["order_id"].(string)

	// Scenario A: Razorpay webhook with TAMPERED amount (300 paise instead of 600 paise)
	mismatchPayload := []byte(fmt.Sprintf(`{
		"event": "payment.captured",
		"payload": {
			"payment": {
				"entity": {
					"id": "pay_tampered_123",
					"order_id": "%s",
					"amount": 300,
					"status": "captured",
					"notes": { "order_id": "%s" }
				}
			}
		}
	}`, orderID, orderID))

	sigMismatch := signRazorpayPayload(mismatchPayload, webhookSecret)
	reqMismatch := httptest.NewRequest(http.MethodPost, "/api/webhooks/razorpay", bytes.NewReader(mismatchPayload))
	reqMismatch.Header.Set("X-Razorpay-Signature", sigMismatch)
	rrMismatch := httptest.NewRecorder()
	mux.ServeHTTP(rrMismatch, reqMismatch)

	if rrMismatch.Code != http.StatusBadRequest {
		t.Errorf("expected mismatch payment webhook to return 400 Bad Request, got %d (body: %s)", rrMismatch.Code, rrMismatch.Body.String())
	}

	// Verify order status was NOT changed to paid
	order, _ := app.db.GetOrder(ctx, orderID)
	if order.Status != contracts.JobStatusPendingPayment {
		t.Errorf("order status after underpaid webhook = %v, want %v", order.Status, contracts.JobStatusPendingPayment)
	}

	// Scenario B: Razorpay webhook with EXACT matching amount (600 paise)
	exactPayload := []byte(fmt.Sprintf(`{
		"event": "payment.captured",
		"payload": {
			"payment": {
				"entity": {
					"id": "pay_exact_456",
					"order_id": "%s",
					"amount": 600,
					"status": "captured",
					"notes": { "order_id": "%s" }
				}
			}
		}
	}`, orderID, orderID))

	sigExact := signRazorpayPayload(exactPayload, webhookSecret)
	reqExact := httptest.NewRequest(http.MethodPost, "/api/webhooks/razorpay", bytes.NewReader(exactPayload))
	reqExact.Header.Set("X-Razorpay-Signature", sigExact)
	rrExact := httptest.NewRecorder()
	mux.ServeHTTP(rrExact, reqExact)

	if rrExact.Code != http.StatusOK {
		t.Fatalf("expected exact payment webhook to return 200 OK, got %d (body: %s)", rrExact.Code, rrExact.Body.String())
	}

	// Verify order status transitioned to PAID_WAITING or DISPATCHED
	orderAfter, _ := app.db.GetOrder(ctx, orderID)
	if orderAfter.Status != contracts.JobStatusPaidWaiting && orderAfter.Status != contracts.JobStatusDispatched {
		t.Errorf("order status after valid payment = %v, want PAID_WAITING or DISPATCHED", orderAfter.Status)
	}
}

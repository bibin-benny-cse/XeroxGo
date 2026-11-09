package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image/png"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"xeroxgo/cloud-backend/internal/payments"
	"xeroxgo/cloud-backend/internal/pins"
	"xeroxgo/cloud-backend/internal/store"
	"xeroxgo/cloud-backend/internal/ws"
	"xeroxgo/pkg/contracts"
)

// mockRazorpayClient provides an in-memory test double for payments.RazorpayClient.
type mockRazorpayClient struct {
	mu             sync.Mutex
	createQRFn     func(ctx context.Context, req payments.CreateQRCodeRequest) (*payments.QRCodeResponse, error)
	createLinkFn   func(ctx context.Context, req payments.CreatePaymentLinkRequest) (*payments.PaymentLinkResponse, error)
	fetchPaymentFn func(ctx context.Context, paymentID string) (*payments.PaymentEntity, error)
	callCount      int
	lastLinkReq    payments.CreatePaymentLinkRequest
}

func (m *mockRazorpayClient) getLastLinkReq() payments.CreatePaymentLinkRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastLinkReq
}

func (m *mockRazorpayClient) CreateUPIQRCode(ctx context.Context, req payments.CreateQRCodeRequest) (*payments.QRCodeResponse, error) {
	m.mu.Lock()
	m.callCount++
	count := m.callCount
	m.mu.Unlock()

	if m.createQRFn != nil {
		return m.createQRFn(ctx, req)
	}
	return &payments.QRCodeResponse{
		ID:          fmt.Sprintf("qr_mock_%d", count),
		Entity:      "qr_code",
		ImageURL:    fmt.Sprintf("https://rzp.io/i/mock_%d", count),
		AmountPaise: req.AmountPaise,
		Status:      "active",
		CloseBy:     req.CloseByUnix,
	}, nil
}

func (m *mockRazorpayClient) CreatePaymentLink(ctx context.Context, req payments.CreatePaymentLinkRequest) (*payments.PaymentLinkResponse, error) {
	m.mu.Lock()
	m.callCount++
	count := m.callCount
	m.lastLinkReq = req
	m.mu.Unlock()

	if m.createLinkFn != nil {
		return m.createLinkFn(ctx, req)
	}
	if m.createQRFn != nil {
		// Delegate error if test configured createQRFn
		_, err := m.createQRFn(ctx, payments.CreateQRCodeRequest{AmountPaise: req.AmountPaise})
		if err != nil {
			return nil, err
		}
	}
	return &payments.PaymentLinkResponse{
		ID:          fmt.Sprintf("plink_mock_%d", count),
		Entity:      "payment_link",
		AmountPaise: req.AmountPaise,
		Currency:    "INR",
		Status:      "created",
		ShortURL:    fmt.Sprintf("https://rzp.io/i/mock_link_%d", count),
		ReferenceID: req.ReferenceID,
		Description: req.Description,
		ExpireBy:    req.ExpireBy,
		Notes:       req.Notes,
	}, nil
}

func (m *mockRazorpayClient) getCallCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.callCount
}

func (m *mockRazorpayClient) FetchPayment(ctx context.Context, paymentID string) (*payments.PaymentEntity, error) {
	if m.fetchPaymentFn != nil {
		return m.fetchPaymentFn(ctx, paymentID)
	}
	return &payments.PaymentEntity{
		ID:       paymentID,
		Entity:   "payment",
		Amount:   500,
		Currency: "INR",
		Status:   "captured",
	}, nil
}

func setupPaymentWorkflowServer(t *testing.T, mockClient *mockRazorpayClient) (*appServer, *http.ServeMux, string, string, string) {
	t.Helper()
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "payment_workflow_test.db")
	jobsDir := filepath.Join(tmpDir, "jobs")

	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open test store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	hmacSecret := []byte("test_pickup_pin_hmac_secret_32_bytes_1234")
	webhookSecret := "test_webhook_secret_32_bytes_long_1234"
	cfg := &serverConfig{
		addr:                  "localhost:0",
		dbPath:                dbPath,
		jobsDir:               jobsDir,
		pricePerSheetPaise:    250,
		downloadSecret:        []byte("test_secret_key_1234567890"),
		webhookSecret:         []byte(webhookSecret),
		pickupPINHMACSecret:   hmacSecret,
		maxActivePINsPerKiosk: 50,
		paymentSessionTTL:     30 * time.Minute,
		devMode:               true,
	}

	hub := ws.NewHub(s)
	app := &appServer{
		cfg:            cfg,
		db:             s,
		hub:            hub,
		pinLimiter:     pins.NewRateLimiter(pins.DefaultCooldownConfig()),
		convWake:       make(chan struct{}, 10),
		razorpayClient: mockClient,
		sessionLocks:   newKeyedMutex(),
		webhookLocks:   newKeyedMutex(),
	}

	ctx := context.Background()
	kioskSecret := "kiosk-demo-super-secret-machine-key"
	secretHash := sha256.Sum256([]byte(kioskSecret))

	err = s.RegisterKiosk(ctx, store.Kiosk{
		KioskID:    "kiosk-demo",
		SecretHash: hex.EncodeToString(secretHash[:]),
		Name:       "Demo Kiosk",
		IsActive:   true,
	})
	if err != nil {
		t.Fatalf("failed to register demo kiosk: %v", err)
	}

	err = s.RegisterKiosk(ctx, store.Kiosk{
		KioskID:    "kiosk-other",
		SecretHash: hex.EncodeToString(secretHash[:]),
		Name:       "Other Kiosk",
		IsActive:   true,
	})
	if err != nil {
		t.Fatalf("failed to register other kiosk: %v", err)
	}

	err = s.RegisterKiosk(ctx, store.Kiosk{
		KioskID:    "kiosk-inactive",
		SecretHash: hex.EncodeToString(secretHash[:]),
		Name:       "Inactive Kiosk",
		IsActive:   false,
	})
	if err != nil {
		t.Fatalf("failed to register inactive kiosk: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/orders/", func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case strings.HasSuffix(path, "/status"):
			app.handleOrderStatus(w, r)
		case strings.HasSuffix(path, "/pin/refresh") || strings.HasSuffix(path, "/refresh-pin"):
			app.handleOrderRefreshPIN(w, r)
		}
	})
	mux.HandleFunc("/api/kiosk/pin/redeem", app.handleKioskRedeemPIN)
	mux.HandleFunc("/api/kiosk/payment/initiate-qr", app.handleKioskInitiateQR)
	mux.HandleFunc("/api/kiosk/payment/qr.png", app.handlePaymentQRImage)
	mux.HandleFunc("/api/kiosk/payment/status", app.handleKioskPaymentStatus)
	mux.HandleFunc("/api/webhooks/razorpay", app.handleRazorpayWebhook)

	return app, mux, "kiosk-demo", kioskSecret, webhookSecret
}

// helper to create an order and redeem a PIN to get an active session
func setupOrderAndRedeemPIN(t *testing.T, app *appServer, mux *http.ServeMux, kioskID, kioskSecret string, pricePaise int64) (orderID string, sessionID string) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()

	orderHex, _ := randomHex(12)
	orderID = "ord_" + orderHex
	jobHex, _ := randomHex(12)
	jobID := "job_" + jobHex

	rawSecret, secretHash, err := pins.GenerateOrderOwnershipSecret()
	if err != nil {
		t.Fatalf("failed to generate order secret: %v", err)
	}
	_ = rawSecret

	order := store.Order{
		OrderID:         orderID,
		OrderSecretHash: secretHash,
		Status:          contracts.JobStatusPendingPayment,
		PricePaise:      pricePaise,
		ColorMode:       "BW",
		Command: contracts.PrintJobCommand{
			JobID:          jobID,
			KioskID:        kioskID,
			DisplayCode:    "PIN-PROTECTED",
			DownloadURL:    "http://test/download",
			SHA256Checksum: "dummy_checksum",
			TotalPages:     2,
			TotalSheets:    2,
			Copies:         1,
			Duplex:         contracts.DuplexSimplex,
			StartFromPage:  1,
		},
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := app.db.CreateOrder(ctx, order); err != nil {
		t.Fatalf("failed to create order: %v", err)
	}

	pinStr := "5432"
	pinHMAC, err := pins.ComputePINHMAC(app.cfg.pickupPINHMACSecret, kioskID, pinStr)
	if err != nil {
		t.Fatalf("failed to compute PIN HMAC: %v", err)
	}

	pinIDHex, _ := randomHex(12)
	pinID := "pin_" + pinIDHex
	err = app.db.CreatePickupPIN(ctx, store.PickupPIN{
		ID:        pinID,
		OrderID:   orderID,
		KioskID:   kioskID,
		PINHMAC:   pinHMAC,
		CreatedAt: now,
		ExpiresAt: now.Add(15 * time.Minute),
	}, 50)
	if err != nil {
		t.Fatalf("failed to create pickup PIN: %v", err)
	}

	// Redeem PIN via kiosk endpoint
	redeemBody := map[string]string{"pin": pinStr}
	reqBytes, _ := json.Marshal(redeemBody)
	req := httptest.NewRequest(http.MethodPost, "/api/kiosk/pin/redeem", bytes.NewReader(reqBytes))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Kiosk-ID", kioskID)
	req.Header.Set("X-Kiosk-Secret", kioskSecret)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected redeem 200 OK, got %d: %s", w.Code, w.Body.String())
	}

	var redeemResp pins.PINRedemptionResponse
	if err := json.Unmarshal(w.Body.Bytes(), &redeemResp); err != nil {
		t.Fatalf("failed to parse redeem response: %v", err)
	}

	return orderID, redeemResp.SessionID
}

func computeWebhookSignature(payload []byte, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(payload)
	return hex.EncodeToString(mac.Sum(nil))
}

func TestKioskInitiateQR_Success(t *testing.T) {
	mockClient := &mockRazorpayClient{}
	app, mux, kioskID, kioskSecret, _ := setupPaymentWorkflowServer(t, mockClient)

	orderID, sessionID := setupOrderAndRedeemPIN(t, app, mux, kioskID, kioskSecret, 500)

	bodyBytes, _ := json.Marshal(InitiateQRRequest{SessionID: sessionID})
	req := httptest.NewRequest(http.MethodPost, "/api/kiosk/payment/initiate-qr", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Kiosk-ID", kioskID)
	req.Header.Set("X-Kiosk-Secret", kioskSecret)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", w.Code, w.Body.String())
	}

	var resp InitiateQRResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse initiate-qr response: %v", err)
	}

	if resp.OrderID != orderID {
		t.Errorf("expected OrderID %s, got %s", orderID, resp.OrderID)
	}
	if resp.SessionID != sessionID {
		t.Errorf("expected SessionID %s, got %s", sessionID, resp.SessionID)
	}
	if !strings.HasPrefix(resp.AttemptID, "att_") {
		t.Errorf("expected AttemptID prefix att_, got %s", resp.AttemptID)
	}
	if resp.AmountPaise != 500 {
		t.Errorf("expected AmountPaise 500, got %d", resp.AmountPaise)
	}
	if resp.Currency != "INR" {
		t.Errorf("expected Currency INR, got %s", resp.Currency)
	}
	if resp.Status != "CREATED" {
		t.Errorf("expected Status CREATED, got %s", resp.Status)
	}
	if resp.RazorpayQRID == "" || resp.QRImageURL == "" {
		t.Errorf("expected non-empty QR id and image url: %+v", resp)
	}

	// Verify payment attempt is persisted in database
	attempt, err := app.db.GetPaymentAttemptByID(context.Background(), resp.AttemptID)
	if err != nil || attempt == nil {
		t.Fatalf("payment attempt not found in db: %v", err)
	}
	if attempt.RazorpayQRID != resp.RazorpayQRID {
		t.Errorf("db RazorpayQRID %s != response %s", attempt.RazorpayQRID, resp.RazorpayQRID)
	}
}

func TestKioskInitiateQR_AuthenticationAndAuthorization(t *testing.T) {
	mockClient := &mockRazorpayClient{}
	app, mux, kioskID, kioskSecret, _ := setupPaymentWorkflowServer(t, mockClient)

	_, sessionID := setupOrderAndRedeemPIN(t, app, mux, kioskID, kioskSecret, 500)
	bodyBytes, _ := json.Marshal(InitiateQRRequest{SessionID: sessionID})

	// 1. Missing credentials -> 401
	req1 := httptest.NewRequest(http.MethodPost, "/api/kiosk/payment/initiate-qr", bytes.NewReader(bodyBytes))
	req1.Header.Set("Content-Type", "application/json")
	w1 := httptest.NewRecorder()
	mux.ServeHTTP(w1, req1)
	if w1.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for missing credentials, got %d", w1.Code)
	}

	// 2. Unknown kiosk -> 401
	req2 := httptest.NewRequest(http.MethodPost, "/api/kiosk/payment/initiate-qr", bytes.NewReader(bodyBytes))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("X-Kiosk-ID", "kiosk-unknown")
	req2.Header.Set("X-Kiosk-Secret", "some-secret")
	w2 := httptest.NewRecorder()
	mux.ServeHTTP(w2, req2)
	if w2.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for unknown kiosk, got %d", w2.Code)
	}

	// 3. Inactive kiosk -> 403
	req3 := httptest.NewRequest(http.MethodPost, "/api/kiosk/payment/initiate-qr", bytes.NewReader(bodyBytes))
	req3.Header.Set("Content-Type", "application/json")
	req3.Header.Set("X-Kiosk-ID", "kiosk-inactive")
	req3.Header.Set("X-Kiosk-Secret", kioskSecret)
	w3 := httptest.NewRecorder()
	mux.ServeHTTP(w3, req3)
	if w3.Code != http.StatusForbidden {
		t.Errorf("expected 403 for inactive kiosk, got %d", w3.Code)
	}

	// 4. Other active kiosk accessing session belonging to kiosk-demo -> 403 Forbidden
	req4 := httptest.NewRequest(http.MethodPost, "/api/kiosk/payment/initiate-qr", bytes.NewReader(bodyBytes))
	req4.Header.Set("Content-Type", "application/json")
	req4.Header.Set("X-Kiosk-ID", "kiosk-other")
	req4.Header.Set("X-Kiosk-Secret", kioskSecret)
	w4 := httptest.NewRecorder()
	mux.ServeHTTP(w4, req4)
	if w4.Code != http.StatusForbidden {
		t.Errorf("expected 403 for other kiosk accessing session, got %d", w4.Code)
	}
}

func TestKioskInitiateQR_DuplicateAttemptPrevention(t *testing.T) {
	mockClient := &mockRazorpayClient{}
	app, mux, kioskID, kioskSecret, _ := setupPaymentWorkflowServer(t, mockClient)

	_, sessionID := setupOrderAndRedeemPIN(t, app, mux, kioskID, kioskSecret, 500)

	// First call to initiate QR
	bodyBytes, _ := json.Marshal(InitiateQRRequest{SessionID: sessionID})
	req1 := httptest.NewRequest(http.MethodPost, "/api/kiosk/payment/initiate-qr", bytes.NewReader(bodyBytes))
	req1.Header.Set("Content-Type", "application/json")
	req1.Header.Set("X-Kiosk-ID", kioskID)
	req1.Header.Set("X-Kiosk-Secret", kioskSecret)

	w1 := httptest.NewRecorder()
	mux.ServeHTTP(w1, req1)
	if w1.Code != http.StatusOK {
		t.Fatalf("first initiate call failed: %d", w1.Code)
	}

	var resp1 InitiateQRResponse
	_ = json.Unmarshal(w1.Body.Bytes(), &resp1)

	// Second call with the same active session (e.g. kiosk page refresh or retry)
	req2 := httptest.NewRequest(http.MethodPost, "/api/kiosk/payment/initiate-qr", bytes.NewReader(bodyBytes))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("X-Kiosk-ID", kioskID)
	req2.Header.Set("X-Kiosk-Secret", kioskSecret)

	w2 := httptest.NewRecorder()
	mux.ServeHTTP(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("second initiate call failed: %d", w2.Code)
	}

	var resp2 InitiateQRResponse
	_ = json.Unmarshal(w2.Body.Bytes(), &resp2)

	// Verifies the existing payment attempt and QR code are reused without creating duplicates
	if resp1.AttemptID != resp2.AttemptID {
		t.Errorf("expected same attempt ID reused: got %s vs %s", resp1.AttemptID, resp2.AttemptID)
	}
	if resp1.RazorpayQRID != resp2.RazorpayQRID {
		t.Errorf("expected same Razorpay QR reused: got %s vs %s", resp1.RazorpayQRID, resp2.RazorpayQRID)
	}
	if mockClient.getCallCount() != 1 {
		t.Errorf("expected Razorpay API to be called only once, but was called %d times", mockClient.getCallCount())
	}
}

func TestKioskInitiateQR_ProviderFailureDoesNotCreateBogusAttempt(t *testing.T) {
	mockClient := &mockRazorpayClient{
		createQRFn: func(ctx context.Context, req payments.CreateQRCodeRequest) (*payments.QRCodeResponse, error) {
			return nil, errors.New("upstream gateway timeout")
		},
	}
	app, mux, kioskID, kioskSecret, _ := setupPaymentWorkflowServer(t, mockClient)

	orderID, sessionID := setupOrderAndRedeemPIN(t, app, mux, kioskID, kioskSecret, 500)

	bodyBytes, _ := json.Marshal(InitiateQRRequest{SessionID: sessionID})
	req := httptest.NewRequest(http.MethodPost, "/api/kiosk/payment/initiate-qr", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Kiosk-ID", kioskID)
	req.Header.Set("X-Kiosk-Secret", kioskSecret)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadGateway {
		t.Errorf("expected 502 Bad Gateway on provider failure, got %d: %s", w.Code, w.Body.String())
	}

	// Verify no bogus payment attempt was created
	attempt, err := app.db.GetLatestPaymentAttempt(context.Background(), orderID)
	if err != nil {
		t.Fatalf("error checking latest attempt: %v", err)
	}
	if attempt != nil {
		t.Errorf("expected no attempt created on provider failure, got %+v", attempt)
	}
}

func createOrderAndSessionWithExpiry(t *testing.T, app *appServer, kioskID string, expiresAt time.Time) (string, string) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()

	orderHex, _ := randomHex(12)
	orderID := "ord_" + orderHex
	jobHex, _ := randomHex(12)
	jobID := "job_" + jobHex

	_, secretHash, err := pins.GenerateOrderOwnershipSecret()
	if err != nil {
		t.Fatalf("failed to generate order secret: %v", err)
	}

	order := store.Order{
		OrderID:         orderID,
		OrderSecretHash: secretHash,
		Status:          contracts.JobStatusPendingPayment,
		PricePaise:      500,
		ColorMode:       "BW",
		Command: contracts.PrintJobCommand{
			JobID:          jobID,
			KioskID:        kioskID,
			DisplayCode:    "PIN-PROTECTED",
			DownloadURL:    "http://test/download",
			SHA256Checksum: "dummy_checksum",
			TotalPages:     2,
			TotalSheets:    2,
			Copies:         1,
			Duplex:         contracts.DuplexSimplex,
			StartFromPage:  1,
		},
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := app.db.CreateOrder(ctx, order); err != nil {
		t.Fatalf("failed to create order: %v", err)
	}

	sessHex, _ := randomHex(16)
	sessionID := "sess_" + sessHex
	sess := store.PaymentSession{
		SessionID: sessionID,
		OrderID:   orderID,
		KioskID:   kioskID,
		Status:    store.PaymentSessionActive,
		CreatedAt: now,
		ExpiresAt: expiresAt,
	}
	if err := app.db.CreatePaymentSession(ctx, sess); err != nil {
		t.Fatalf("failed to create payment session: %v", err)
	}

	return orderID, sessionID
}

func TestPaymentLink_Expiry_Normal20Minutes(t *testing.T) {
	mockClient := &mockRazorpayClient{}
	app, mux, kioskID, kioskSecret, _ := setupPaymentWorkflowServer(t, mockClient)

	// Session with generous 30 minutes TTL
	now := time.Now().UTC()
	_, sessionID := createOrderAndSessionWithExpiry(t, app, kioskID, now.Add(30*time.Minute))

	bodyBytes, _ := json.Marshal(InitiateQRRequest{SessionID: sessionID})
	req := httptest.NewRequest(http.MethodPost, "/api/kiosk/payment/initiate-qr", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Kiosk-ID", kioskID)
	req.Header.Set("X-Kiosk-Secret", kioskSecret)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", w.Code, w.Body.String())
	}

	var resp InitiateQRResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	lastReq := mockClient.getLastLinkReq()
	expectedMin := now.Add(20*time.Minute - 2*time.Second).Unix()
	expectedMax := now.Add(20*time.Minute + 2*time.Second).Unix()

	if lastReq.ExpireBy < expectedMin || lastReq.ExpireBy > expectedMax {
		t.Errorf("expected ExpireBy around %d (+20m), got %d", now.Add(20*time.Minute).Unix(), lastReq.ExpireBy)
	}
	if resp.ExpiresAt != lastReq.ExpireBy {
		t.Errorf("expected response ExpiresAt %d == provider ExpireBy %d", resp.ExpiresAt, lastReq.ExpireBy)
	}
}

func TestPaymentLink_Expiry_SessionEarlierThan20Minutes(t *testing.T) {
	mockClient := &mockRazorpayClient{}
	app, mux, kioskID, kioskSecret, _ := setupPaymentWorkflowServer(t, mockClient)

	// Session expires in 18 minutes (less than the 20-minute default, but >= 16 minutes)
	now := time.Now().UTC()
	sessionExpiresAt := now.Add(18 * time.Minute)
	_, sessionID := createOrderAndSessionWithExpiry(t, app, kioskID, sessionExpiresAt)

	bodyBytes, _ := json.Marshal(InitiateQRRequest{SessionID: sessionID})
	req := httptest.NewRequest(http.MethodPost, "/api/kiosk/payment/initiate-qr", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Kiosk-ID", kioskID)
	req.Header.Set("X-Kiosk-Secret", kioskSecret)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", w.Code, w.Body.String())
	}

	var resp InitiateQRResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	lastReq := mockClient.getLastLinkReq()
	// Must be capped at sessionExpiresAt and never exceed it
	if lastReq.ExpireBy > sessionExpiresAt.Unix() {
		t.Errorf("expected ExpireBy capped at session expiry %d, but got %d", sessionExpiresAt.Unix(), lastReq.ExpireBy)
	}
	if resp.ExpiresAt != lastReq.ExpireBy {
		t.Errorf("expected response ExpiresAt %d == provider ExpireBy %d", resp.ExpiresAt, lastReq.ExpireBy)
	}
}

func TestPaymentLink_Expiry_LessThan16MinutesRemaining(t *testing.T) {
	mockClient := &mockRazorpayClient{}
	app, mux, kioskID, kioskSecret, _ := setupPaymentWorkflowServer(t, mockClient)

	// Session expires in 15 minutes (under the 16-minute minimum required ahead)
	now := time.Now().UTC()
	sessionExpiresAt := now.Add(15 * time.Minute)
	_, sessionID := createOrderAndSessionWithExpiry(t, app, kioskID, sessionExpiresAt)

	bodyBytes, _ := json.Marshal(InitiateQRRequest{SessionID: sessionID})
	req := httptest.NewRequest(http.MethodPost, "/api/kiosk/payment/initiate-qr", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Kiosk-ID", kioskID)
	req.Header.Set("X-Kiosk-Secret", kioskSecret)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 Conflict for insufficient session lifetime, got %d: %s", w.Code, w.Body.String())
	}

	var errResp map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &errResp)
	if !strings.Contains(errResp["error"], "insufficient remaining lifetime") {
		t.Errorf("expected error message mentioning insufficient remaining lifetime, got: %s", errResp["error"])
	}

	if mockClient.getCallCount() != 0 {
		t.Errorf("expected provider NOT to be called when session lifetime is insufficient, got %d calls", mockClient.getCallCount())
	}
}

func TestPaymentLink_Expiry_ExactBoundaryAndTimestampRounding(t *testing.T) {
	// Sub-test A: Exactly 16 minutes remaining -> ALLOWED
	t.Run("exactly_16_minutes_allowed", func(t *testing.T) {
		mockClient := &mockRazorpayClient{}
		app, mux, kioskID, kioskSecret, _ := setupPaymentWorkflowServer(t, mockClient)

		now := time.Now().UTC()
		// Safe whole-second alignment for boundary: 16 minutes + 500ms
		sessionExpiresAt := now.Add(16*time.Minute + 500*time.Millisecond)
		_, sessionID := createOrderAndSessionWithExpiry(t, app, kioskID, sessionExpiresAt)

		bodyBytes, _ := json.Marshal(InitiateQRRequest{SessionID: sessionID})
		req := httptest.NewRequest(http.MethodPost, "/api/kiosk/payment/initiate-qr", bytes.NewReader(bodyBytes))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Kiosk-ID", kioskID)
		req.Header.Set("X-Kiosk-Secret", kioskSecret)

		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 OK at exactly 16m boundary, got %d: %s", w.Code, w.Body.String())
		}
		if mockClient.getCallCount() != 1 {
			t.Errorf("expected provider called once, got %d", mockClient.getCallCount())
		}
	})

	// Sub-test B: Under 16 minutes remaining (15m 58s) -> REJECTED
	t.Run("under_16_minutes_rejected", func(t *testing.T) {
		mockClient := &mockRazorpayClient{}
		app, mux, kioskID, kioskSecret, _ := setupPaymentWorkflowServer(t, mockClient)

		now := time.Now().UTC()
		sessionExpiresAt := now.Add(16*time.Minute - 2*time.Second)
		_, sessionID := createOrderAndSessionWithExpiry(t, app, kioskID, sessionExpiresAt)

		bodyBytes, _ := json.Marshal(InitiateQRRequest{SessionID: sessionID})
		req := httptest.NewRequest(http.MethodPost, "/api/kiosk/payment/initiate-qr", bytes.NewReader(bodyBytes))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Kiosk-ID", kioskID)
		req.Header.Set("X-Kiosk-Secret", kioskSecret)

		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)

		if w.Code != http.StatusConflict {
			t.Fatalf("expected 409 Conflict at under 16m boundary, got %d: %s", w.Code, w.Body.String())
		}
		if mockClient.getCallCount() != 0 {
			t.Errorf("expected provider NOT called, got %d", mockClient.getCallCount())
		}
	})

	// Sub-test C: Sub-second precision does not cause rounding underflow
	t.Run("subsecond_timestamp_precision", func(t *testing.T) {
		mockClient := &mockRazorpayClient{}
		app, mux, kioskID, kioskSecret, _ := setupPaymentWorkflowServer(t, mockClient)

		now := time.Now().UTC()
		sessionExpiresAt := now.Add(18*time.Minute + 750*time.Millisecond)
		_, sessionID := createOrderAndSessionWithExpiry(t, app, kioskID, sessionExpiresAt)

		bodyBytes, _ := json.Marshal(InitiateQRRequest{SessionID: sessionID})
		req := httptest.NewRequest(http.MethodPost, "/api/kiosk/payment/initiate-qr", bytes.NewReader(bodyBytes))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Kiosk-ID", kioskID)
		req.Header.Set("X-Kiosk-Secret", kioskSecret)

		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 OK with subsecond timestamp, got %d: %s", w.Code, w.Body.String())
		}
		lastReq := mockClient.getLastLinkReq()
		if lastReq.ExpireBy != sessionExpiresAt.Unix() {
			t.Errorf("expected ExpireBy %d == sessionExpiresAt.Unix() %d", lastReq.ExpireBy, sessionExpiresAt.Unix())
		}
	})
}

func TestPaymentLink_ProviderRejectionAndSafeErrorLogging(t *testing.T) {
	mockClient := &mockRazorpayClient{
		createLinkFn: func(ctx context.Context, req payments.CreatePaymentLinkRequest) (*payments.PaymentLinkResponse, error) {
			return nil, &payments.APIError{
				HTTPStatus:  http.StatusBadRequest,
				Code:        "BAD_REQUEST_ERROR",
				Description: "expire_by should be at least 15 minutes greater than current time",
			}
		},
	}
	app, mux, kioskID, kioskSecret, _ := setupPaymentWorkflowServer(t, mockClient)

	orderID, sessionID := setupOrderAndRedeemPIN(t, app, mux, kioskID, kioskSecret, 500)

	var logBuf bytes.Buffer
	origOutput := log.Writer()
	log.SetOutput(&logBuf)
	defer log.SetOutput(origOutput)

	bodyBytes, _ := json.Marshal(InitiateQRRequest{SessionID: sessionID})
	req := httptest.NewRequest(http.MethodPost, "/api/kiosk/payment/initiate-qr", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Kiosk-ID", kioskID)
	req.Header.Set("X-Kiosk-Secret", kioskSecret)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("expected 502 Bad Gateway, got %d: %s", w.Code, w.Body.String())
	}

	var errResp map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &errResp)
	if errResp["error"] != "failed to generate payment link from provider" {
		t.Errorf("expected generic client error, got: %s", errResp["error"])
	}

	logOutput := logBuf.String()
	if !strings.Contains(logOutput, orderID) {
		t.Errorf("expected log to contain order ID %s, got: %s", orderID, logOutput)
	}
	if !strings.Contains(logOutput, "HTTP 400") {
		t.Errorf("expected log to contain HTTP 400, got: %s", logOutput)
	}
	if !strings.Contains(logOutput, "code=BAD_REQUEST_ERROR") {
		t.Errorf("expected log to contain code=BAD_REQUEST_ERROR, got: %s", logOutput)
	}
	if !strings.Contains(logOutput, "expire_by should be at least 15 minutes greater than current time") {
		t.Errorf("expected log to contain description, got: %s", logOutput)
	}

	// Verify no secrets, credentials, or sensitive headers leaked
	if strings.Contains(logOutput, kioskSecret) || strings.Contains(logOutput, "Bearer") || strings.Contains(logOutput, "Basic") {
		t.Errorf("CRITICAL SECURITY: sensitive credentials detected in log output: %s", logOutput)
	}
}

func TestPaymentRecovery_InsufficientSessionLifetime_RefreshPINFlow(t *testing.T) {
	mockClient := &mockRazorpayClient{}
	app, mux, kioskID, kioskSecret, webhookSecret := setupPaymentWorkflowServer(t, mockClient)
	ctx := context.Background()

	// 1. Create unpaid order with known order_secret
	rawSecret, secretHash, err := pins.GenerateOrderOwnershipSecret()
	if err != nil {
		t.Fatalf("failed to generate order ownership secret: %v", err)
	}

	orderHex, _ := randomHex(12)
	orderID := "ord_" + orderHex
	jobHex, _ := randomHex(12)
	jobID := "job_" + jobHex
	now := time.Now().UTC()

	order := store.Order{
		OrderID:         orderID,
		OrderSecretHash: secretHash,
		Status:          contracts.JobStatusPendingPayment,
		PricePaise:      600,
		ColorMode:       "BW",
		Command: contracts.PrintJobCommand{
			JobID:          jobID,
			KioskID:        kioskID,
			DisplayCode:    "PIN-PROTECTED",
			DownloadURL:    "http://test/download",
			SHA256Checksum: "dummy_checksum",
			TotalPages:     2,
			TotalSheets:    2,
			Copies:         1,
			Duplex:         contracts.DuplexSimplex,
			StartFromPage:  1,
		},
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := app.db.CreateOrder(ctx, order); err != nil {
		t.Fatalf("failed to create order: %v", err)
	}

	// 2. Issue initial PIN (e.g. "1234")
	pin1 := "1234"
	pin1HMAC, err := pins.ComputePINHMAC(app.cfg.pickupPINHMACSecret, kioskID, pin1)
	if err != nil {
		t.Fatalf("failed to compute PIN HMAC: %v", err)
	}
	pin1IDHex, _ := randomHex(12)
	err = app.db.CreatePickupPIN(ctx, store.PickupPIN{
		ID:        "pin_" + pin1IDHex,
		OrderID:   orderID,
		KioskID:   kioskID,
		PINHMAC:   pin1HMAC,
		CreatedAt: now,
		ExpiresAt: now.Add(15 * time.Minute),
	}, 50)
	if err != nil {
		t.Fatalf("failed to create initial pickup PIN: %v", err)
	}

	// 3. Customer redeems pin1 at the kiosk -> creates session 1
	redeemReqBytes, _ := json.Marshal(pins.PINRedemptionRequest{PIN: pin1})
	req := httptest.NewRequest(http.MethodPost, "/api/kiosk/pin/redeem", bytes.NewReader(redeemReqBytes))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Kiosk-ID", kioskID)
	req.Header.Set("X-Kiosk-Secret", kioskSecret)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK redeeming pin1, got %d: %s", w.Code, w.Body.String())
	}

	var redeemResp pins.PINRedemptionResponse
	if err := json.Unmarshal(w.Body.Bytes(), &redeemResp); err != nil {
		t.Fatalf("failed to parse redeem response: %v", err)
	}
	session1ID := redeemResp.SessionID
	if session1ID == "" {
		t.Fatalf("expected non-empty session ID")
	}

	// 4. Invariant: Re-entering pin1 MUST be rejected (single-use PIN protection)
	reqReenter := httptest.NewRequest(http.MethodPost, "/api/kiosk/pin/redeem", bytes.NewReader(redeemReqBytes))
	reqReenter.Header.Set("Content-Type", "application/json")
	reqReenter.Header.Set("X-Kiosk-ID", kioskID)
	reqReenter.Header.Set("X-Kiosk-Secret", kioskSecret)
	wReenter := httptest.NewRecorder()
	mux.ServeHTTP(wReenter, reqReenter)
	if wReenter.Code != http.StatusBadRequest {
		t.Fatalf("security violation: re-entering consumed pin1 should return 400 Bad Request, got %d", wReenter.Code)
	}

	// 5. Simulate session 1 aging until < 16 minutes remain (e.g. 15 minutes left)
	if err := app.db.UpdatePaymentSessionExpiry(ctx, session1ID, now.Add(15*time.Minute)); err != nil {
		t.Fatalf("failed to update session expiry: %v", err)
	}

	// 6. Kiosk calls initiate-qr on session1 -> MUST return HTTP 409 Conflict with accurate actionable error
	initBodyBytes, _ := json.Marshal(InitiateQRRequest{SessionID: session1ID})
	reqInit := httptest.NewRequest(http.MethodPost, "/api/kiosk/payment/initiate-qr", bytes.NewReader(initBodyBytes))
	reqInit.Header.Set("Content-Type", "application/json")
	reqInit.Header.Set("X-Kiosk-ID", kioskID)
	reqInit.Header.Set("X-Kiosk-Secret", kioskSecret)
	wInit := httptest.NewRecorder()
	mux.ServeHTTP(wInit, reqInit)

	if wInit.Code != http.StatusConflict {
		t.Fatalf("expected 409 Conflict for insufficient session lifetime, got %d: %s", wInit.Code, wInit.Body.String())
	}

	var errResp map[string]string
	_ = json.Unmarshal(wInit.Body.Bytes(), &errResp)
	expectedErrMsg := "payment session has insufficient remaining lifetime; tap 'Refresh PIN' on your order screen and enter the new PIN"
	if errResp["error"] != expectedErrMsg {
		t.Fatalf("unexpected error message.\nexpected: %q\ngot:      %q", expectedErrMsg, errResp["error"])
	}

	// Provider must NOT have been called
	if mockClient.getCallCount() != 0 {
		t.Fatalf("expected provider not to be called, got %d calls", mockClient.getCallCount())
	}

	// 7. Customer follows instruction: taps 'Refresh PIN' on their mobile order screen
	// Sends POST /api/orders/{order_id}/pin/refresh with proof of ownership (order_secret)
	refreshBody, _ := json.Marshal(pins.PINRefreshRequest{OrderSecret: rawSecret})
	reqRefresh := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/orders/%s/pin/refresh", orderID), bytes.NewReader(refreshBody))
	reqRefresh.Header.Set("Content-Type", "application/json")
	wRefresh := httptest.NewRecorder()
	mux.ServeHTTP(wRefresh, reqRefresh)

	if wRefresh.Code != http.StatusOK {
		t.Fatalf("expected 200 OK on PIN refresh, got %d: %s", wRefresh.Code, wRefresh.Body.String())
	}

	var refreshResp pins.PINRefreshResponse
	if err := json.Unmarshal(wRefresh.Body.Bytes(), &refreshResp); err != nil {
		t.Fatalf("failed to decode refresh response: %v", err)
	}
	pin2 := refreshResp.PickupPIN
	if len(pin2) != 4 {
		t.Fatalf("expected 4-digit refreshed PIN, got %q", pin2)
	}

	// 8. Session 1 is now invalidated/expired
	sess1AfterRefresh, err := app.db.GetPaymentSession(ctx, session1ID)
	if err != nil || sess1AfterRefresh == nil {
		t.Fatalf("failed to query session 1: %v", err)
	}
	if sess1AfterRefresh.Status != store.PaymentSessionExpired {
		t.Fatalf("expected session 1 status to be EXPIRED, got %s", sess1AfterRefresh.Status)
	}

	// Attempting initiate-qr on session1 now returns 404 Not Found (expired)
	reqInitDead := httptest.NewRequest(http.MethodPost, "/api/kiosk/payment/initiate-qr", bytes.NewReader(initBodyBytes))
	reqInitDead.Header.Set("Content-Type", "application/json")
	reqInitDead.Header.Set("X-Kiosk-ID", kioskID)
	reqInitDead.Header.Set("X-Kiosk-Secret", kioskSecret)
	wInitDead := httptest.NewRecorder()
	mux.ServeHTTP(wInitDead, reqInitDead)
	if wInitDead.Code != http.StatusNotFound {
		t.Fatalf("expected 404 Not Found for expired session 1, got %d: %s", wInitDead.Code, wInitDead.Body.String())
	}

	// 9. Customer enters pin2 on kiosk touchscreen
	redeem2Bytes, _ := json.Marshal(pins.PINRedemptionRequest{PIN: pin2})
	reqRedeem2 := httptest.NewRequest(http.MethodPost, "/api/kiosk/pin/redeem", bytes.NewReader(redeem2Bytes))
	reqRedeem2.Header.Set("Content-Type", "application/json")
	reqRedeem2.Header.Set("X-Kiosk-ID", kioskID)
	reqRedeem2.Header.Set("X-Kiosk-Secret", kioskSecret)
	wRedeem2 := httptest.NewRecorder()
	mux.ServeHTTP(wRedeem2, reqRedeem2)

	if wRedeem2.Code != http.StatusOK {
		t.Fatalf("expected 200 OK redeeming pin2, got %d: %s", wRedeem2.Code, wRedeem2.Body.String())
	}

	var redeem2Resp pins.PINRedemptionResponse
	if err := json.Unmarshal(wRedeem2.Body.Bytes(), &redeem2Resp); err != nil {
		t.Fatalf("failed to decode redemption 2 response: %v", err)
	}
	session2ID := redeem2Resp.SessionID
	if session2ID == "" || session2ID == session1ID {
		t.Fatalf("expected fresh new session ID, got %s (session1 was %s)", session2ID, session1ID)
	}

	// Invariant: pin2 is also strictly single-use
	reqReenter2 := httptest.NewRequest(http.MethodPost, "/api/kiosk/pin/redeem", bytes.NewReader(redeem2Bytes))
	reqReenter2.Header.Set("Content-Type", "application/json")
	reqReenter2.Header.Set("X-Kiosk-ID", kioskID)
	reqReenter2.Header.Set("X-Kiosk-Secret", kioskSecret)
	wReenter2 := httptest.NewRecorder()
	mux.ServeHTTP(wReenter2, reqReenter2)
	if wReenter2.Code != http.StatusBadRequest {
		t.Fatalf("security violation: re-entering consumed pin2 should return 400 Bad Request, got %d", wReenter2.Code)
	}

	// 10. Kiosk calls initiate-qr on session2 -> SUCCEEDS!
	init2Bytes, _ := json.Marshal(InitiateQRRequest{SessionID: session2ID})
	reqInit2 := httptest.NewRequest(http.MethodPost, "/api/kiosk/payment/initiate-qr", bytes.NewReader(init2Bytes))
	reqInit2.Header.Set("Content-Type", "application/json")
	reqInit2.Header.Set("X-Kiosk-ID", kioskID)
	reqInit2.Header.Set("X-Kiosk-Secret", kioskSecret)
	wInit2 := httptest.NewRecorder()
	mux.ServeHTTP(wInit2, reqInit2)

	if wInit2.Code != http.StatusOK {
		t.Fatalf("expected 200 OK on initiate-qr with session 2, got %d: %s", wInit2.Code, wInit2.Body.String())
	}

	var qrResp InitiateQRResponse
	if err := json.Unmarshal(wInit2.Body.Bytes(), &qrResp); err != nil {
		t.Fatalf("failed to parse initiate-qr response: %v", err)
	}
	if qrResp.SessionID != session2ID {
		t.Fatalf("expected session ID %s, got %s", session2ID, qrResp.SessionID)
	}
	if qrResp.AmountPaise != 600 {
		t.Fatalf("expected amount 600 paise, got %d", qrResp.AmountPaise)
	}

	// Verify provider was called with normal 20m expiry
	if mockClient.getCallCount() != 1 {
		t.Fatalf("expected exactly 1 provider call, got %d", mockClient.getCallCount())
	}

	// 11. Customer pays via Razorpay -> Webhook confirms payment
	attemptID := qrResp.AttemptID
	eventID := "evt_recovery_test_1"
	webhookPayload := map[string]interface{}{
		"entity": "event",
		"event":  "payment_link.paid",
		"payload": map[string]interface{}{
			"payment_link": map[string]interface{}{
				"entity": map[string]interface{}{
					"id":          qrResp.RazorpayQRID,
					"amount":      600,
					"amount_paid": 600,
					"currency":    "INR",
					"status":      "paid",
					"notes": map[string]interface{}{
						"order_id":   orderID,
						"kiosk_id":   kioskID,
						"session_id": session2ID,
						"attempt_id": attemptID,
					},
				},
			},
			"payment": map[string]interface{}{
				"entity": map[string]interface{}{
					"id":       "pay_recovery_test_1",
					"amount":   600,
					"currency": "INR",
					"status":   "captured",
					"notes": map[string]interface{}{
						"order_id":   orderID,
						"kiosk_id":   kioskID,
						"session_id": session2ID,
					},
				},
			},
		},
	}
	payloadBytes, _ := json.Marshal(webhookPayload)
	sig := computeWebhookSignature(payloadBytes, webhookSecret)

	reqHook := httptest.NewRequest(http.MethodPost, "/api/webhooks/razorpay", bytes.NewReader(payloadBytes))
	reqHook.Header.Set("Content-Type", "application/json")
	reqHook.Header.Set("X-Razorpay-Signature", sig)
	reqHook.Header.Set("X-Razorpay-Event-Id", eventID)
	wHook := httptest.NewRecorder()
	mux.ServeHTTP(wHook, reqHook)

	if wHook.Code != http.StatusOK {
		t.Fatalf("expected webhook 200 OK, got %d: %s", wHook.Code, wHook.Body.String())
	}

	// 12. Verify order transitioned to PAID_WAITING
	paidOrder, err := app.db.GetOrder(ctx, orderID)
	if err != nil || paidOrder == nil {
		t.Fatalf("failed to query paid order: %v", err)
	}
	if paidOrder.Status != contracts.JobStatusPaidWaiting {
		t.Fatalf("expected order status PAID_WAITING, got %s", paidOrder.Status)
	}

	// 13. Invariant: Customer CANNOT refresh PIN after payment is completed
	reqRefreshAfterPay := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/orders/%s/pin/refresh", orderID), bytes.NewReader(refreshBody))
	reqRefreshAfterPay.Header.Set("Content-Type", "application/json")
	wRefreshAfterPay := httptest.NewRecorder()
	mux.ServeHTTP(wRefreshAfterPay, reqRefreshAfterPay)
	if wRefreshAfterPay.Code != http.StatusBadRequest {
		t.Fatalf("security violation: PIN refresh on paid order must return 400 Bad Request, got %d", wRefreshAfterPay.Code)
	}

	// 14. Invariant: Duplicate webhook delivery is idempotent and does NOT re-process
	reqHookDup := httptest.NewRequest(http.MethodPost, "/api/webhooks/razorpay", bytes.NewReader(payloadBytes))
	reqHookDup.Header.Set("Content-Type", "application/json")
	reqHookDup.Header.Set("X-Razorpay-Signature", sig)
	reqHookDup.Header.Set("X-Razorpay-Event-Id", eventID)
	wHookDup := httptest.NewRecorder()
	mux.ServeHTTP(wHookDup, reqHookDup)
	if wHookDup.Code != http.StatusOK {
		t.Fatalf("expected duplicate webhook 200 OK, got %d", wHookDup.Code)
	}
	if !strings.Contains(wHookDup.Body.String(), "duplicate") {
		t.Fatalf("expected duplicate status in response, got %s", wHookDup.Body.String())
	}
}

func TestRazorpayWebhook_QRCodeCredited_Success(t *testing.T) {
	mockClient := &mockRazorpayClient{}
	app, mux, kioskID, kioskSecret, webhookSecret := setupPaymentWorkflowServer(t, mockClient)

	orderID, sessionID := setupOrderAndRedeemPIN(t, app, mux, kioskID, kioskSecret, 500)

	// Initiate QR code
	bodyBytes, _ := json.Marshal(InitiateQRRequest{SessionID: sessionID})
	req := httptest.NewRequest(http.MethodPost, "/api/kiosk/payment/initiate-qr", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Kiosk-ID", kioskID)
	req.Header.Set("X-Kiosk-Secret", kioskSecret)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("failed to initiate QR: %d %s", w.Code, w.Body.String())
	}

	var qrResp InitiateQRResponse
	_ = json.Unmarshal(w.Body.Bytes(), &qrResp)

	// Build official Razorpay qr_code.credited webhook payload
	webhookPayload := map[string]interface{}{
		"entity": "event",
		"event":  "qr_code.credited",
		"payload": map[string]interface{}{
			"qr_code": map[string]interface{}{
				"entity": map[string]interface{}{
					"id":             qrResp.RazorpayQRID,
					"payment_amount": 500,
					"status":         "closed",
				},
			},
			"payment": map[string]interface{}{
				"entity": map[string]interface{}{
					"id":       "pay_test_qr_12345",
					"amount":   500,
					"currency": "INR",
					"status":   "captured",
					"method":   "upi",
				},
			},
		},
	}

	payloadRaw, _ := json.Marshal(webhookPayload)
	sig := computeWebhookSignature(payloadRaw, webhookSecret)

	webhookReq := httptest.NewRequest(http.MethodPost, "/api/webhooks/razorpay", bytes.NewReader(payloadRaw))
	webhookReq.Header.Set("Content-Type", "application/json")
	webhookReq.Header.Set("X-Razorpay-Signature", sig)
	webhookReq.Header.Set("X-Razorpay-Event-Id", "evt_qr_credit_001")

	whResp := httptest.NewRecorder()
	mux.ServeHTTP(whResp, webhookReq)

	if whResp.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for valid webhook, got %d: %s", whResp.Code, whResp.Body.String())
	}

	ctx := context.Background()

	// 1. Order status strictly transitions to PAID_WAITING
	order, err := app.db.GetOrder(ctx, orderID)
	if err != nil || order == nil {
		t.Fatalf("order not found: %v", err)
	}
	if order.Status != contracts.JobStatusPaidWaiting {
		t.Errorf("expected order status PAID_WAITING, got %s", order.Status)
	}

	// 2. Payment attempt transitions to CAPTURED
	attempt, err := app.db.GetPaymentAttemptByID(ctx, qrResp.AttemptID)
	if err != nil || attempt == nil {
		t.Fatalf("attempt not found: %v", err)
	}
	if attempt.Status != store.PaymentAttemptCaptured {
		t.Errorf("expected attempt status CAPTURED, got %s", attempt.Status)
	}
	if attempt.RazorpayPaymentID != "pay_test_qr_12345" {
		t.Errorf("expected RazorpayPaymentID 'pay_test_qr_12345', got %s", attempt.RazorpayPaymentID)
	}

	// 3. Payment session transitions to COMPLETED
	session, err := app.db.GetPaymentSession(ctx, sessionID)
	if err != nil || session == nil {
		t.Fatalf("session not found: %v", err)
	}
	if session.Status != store.PaymentSessionCompleted {
		t.Errorf("expected session status COMPLETED, got %s", session.Status)
	}

	// 4. Webhook event is marked PROCESSED
	evt, err := app.db.GetWebhookEvent(ctx, "evt_qr_credit_001")
	if err != nil || evt == nil {
		t.Fatalf("webhook event not found: %v", err)
	}
	if evt.Status != store.WebhookEventProcessed {
		t.Errorf("expected webhook event PROCESSED, got %s", evt.Status)
	}

	// 5. Check payment status endpoint reports paid
	statusReq := httptest.NewRequest(http.MethodGet, "/api/kiosk/payment/status?session_id="+sessionID, nil)
	statusReq.Header.Set("X-Kiosk-ID", kioskID)
	statusReq.Header.Set("X-Kiosk-Secret", kioskSecret)
	statusRec := httptest.NewRecorder()
	mux.ServeHTTP(statusRec, statusReq)
	if statusRec.Code != http.StatusOK {
		t.Fatalf("payment status endpoint returned %d: %s", statusRec.Code, statusRec.Body.String())
	}
	var stResp PaymentStatusResponse
	_ = json.Unmarshal(statusRec.Body.Bytes(), &stResp)
	if !stResp.IsPaid || stResp.OrderStatus != string(contracts.JobStatusPaidWaiting) {
		t.Errorf("unexpected status endpoint response: %+v", stResp)
	}
}

func TestRazorpayWebhook_Idempotency(t *testing.T) {
	mockClient := &mockRazorpayClient{}
	app, mux, kioskID, kioskSecret, webhookSecret := setupPaymentWorkflowServer(t, mockClient)

	orderID, sessionID := setupOrderAndRedeemPIN(t, app, mux, kioskID, kioskSecret, 500)

	bodyBytes, _ := json.Marshal(InitiateQRRequest{SessionID: sessionID})
	req := httptest.NewRequest(http.MethodPost, "/api/kiosk/payment/initiate-qr", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Kiosk-ID", kioskID)
	req.Header.Set("X-Kiosk-Secret", kioskSecret)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	var qrResp InitiateQRResponse
	_ = json.Unmarshal(w.Body.Bytes(), &qrResp)

	webhookPayload := map[string]interface{}{
		"entity": "event",
		"event":  "qr_code.credited",
		"payload": map[string]interface{}{
			"qr_code": map[string]interface{}{
				"entity": map[string]interface{}{
					"id": qrResp.RazorpayQRID,
				},
			},
			"payment": map[string]interface{}{
				"entity": map[string]interface{}{
					"id":       "pay_idempotent_1",
					"amount":   500,
					"currency": "INR",
					"status":   "captured",
				},
			},
		},
	}
	payloadRaw, _ := json.Marshal(webhookPayload)
	sig := computeWebhookSignature(payloadRaw, webhookSecret)

	// First webhook delivery
	req1 := httptest.NewRequest(http.MethodPost, "/api/webhooks/razorpay", bytes.NewReader(payloadRaw))
	req1.Header.Set("Content-Type", "application/json")
	req1.Header.Set("X-Razorpay-Signature", sig)
	req1.Header.Set("X-Razorpay-Event-Id", "evt_idempotent_1")
	w1 := httptest.NewRecorder()
	mux.ServeHTTP(w1, req1)
	if w1.Code != http.StatusOK {
		t.Fatalf("first webhook failed: %d %s", w1.Code, w1.Body.String())
	}

	// Duplicate delivery with identical event ID
	req2 := httptest.NewRequest(http.MethodPost, "/api/webhooks/razorpay", bytes.NewReader(payloadRaw))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("X-Razorpay-Signature", sig)
	req2.Header.Set("X-Razorpay-Event-Id", "evt_idempotent_1")
	w2 := httptest.NewRecorder()
	mux.ServeHTTP(w2, req2)

	if w2.Code != http.StatusOK {
		t.Fatalf("duplicate webhook delivery failed: %d", w2.Code)
	}
	if !strings.Contains(w2.Body.String(), "duplicate") {
		t.Errorf("expected duplicate response status, got: %s", w2.Body.String())
	}

	// Confirm order is still PAID_WAITING and not altered
	order, _ := app.db.GetOrder(context.Background(), orderID)
	if order.Status != contracts.JobStatusPaidWaiting {
		t.Errorf("expected order status PAID_WAITING, got %s", order.Status)
	}
}

func TestRazorpayWebhook_SafeRetryAfterFailure(t *testing.T) {
	mockClient := &mockRazorpayClient{}
	app, mux, kioskID, kioskSecret, webhookSecret := setupPaymentWorkflowServer(t, mockClient)

	// Send webhook for a QR ID that doesn't exist yet
	webhookPayload := map[string]interface{}{
		"entity": "event",
		"event":  "qr_code.credited",
		"payload": map[string]interface{}{
			"qr_code": map[string]interface{}{
				"entity": map[string]interface{}{
					"id": "qr_future_123",
				},
			},
			"payment": map[string]interface{}{
				"entity": map[string]interface{}{
					"id":       "pay_retry_1",
					"amount":   500,
					"currency": "INR",
					"status":   "captured",
				},
			},
		},
	}
	payloadRaw, _ := json.Marshal(webhookPayload)
	sig := computeWebhookSignature(payloadRaw, webhookSecret)

	eventID := "evt_retry_test_999"

	// First attempt: QR ID not found -> fails
	req1 := httptest.NewRequest(http.MethodPost, "/api/webhooks/razorpay", bytes.NewReader(payloadRaw))
	req1.Header.Set("Content-Type", "application/json")
	req1.Header.Set("X-Razorpay-Signature", sig)
	req1.Header.Set("X-Razorpay-Event-Id", eventID)
	w1 := httptest.NewRecorder()
	mux.ServeHTTP(w1, req1)

	if w1.Code != http.StatusNotFound {
		t.Fatalf("expected 404 on missing attempt, got %d: %s", w1.Code, w1.Body.String())
	}

	// Event is stored with status FAILED
	evt, err := app.db.GetWebhookEvent(context.Background(), eventID)
	if err != nil || evt == nil {
		t.Fatalf("failed event not recorded: %v", err)
	}
	if evt.Status != store.WebhookEventFailed {
		t.Errorf("expected event status FAILED, got %s", evt.Status)
	}

	// Now register order and attempt with that QR ID
	orderID, sessionID := setupOrderAndRedeemPIN(t, app, mux, kioskID, kioskSecret, 500)
	now := time.Now().UTC()
	err = app.db.CreatePaymentAttempt(context.Background(), store.PaymentAttempt{
		ID:            "att_retry_123",
		OrderID:       orderID,
		SessionID:     sessionID,
		KioskID:       kioskID,
		PaymentMethod: store.PaymentMethodRazorpayUPIQR,
		RazorpayQRID:  "qr_future_123",
		AmountPaise:   500,
		Currency:      "INR",
		Status:        store.PaymentAttemptCreated,
		CreatedAt:     now,
		ExpiresAt:     now.Add(15 * time.Minute),
		UpdatedAt:     now,
	})
	if err != nil {
		t.Fatalf("failed to create payment attempt: %v", err)
	}

	// Resend the webhook with the same event ID -> must safely retry and succeed
	req2 := httptest.NewRequest(http.MethodPost, "/api/webhooks/razorpay", bytes.NewReader(payloadRaw))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("X-Razorpay-Signature", sig)
	req2.Header.Set("X-Razorpay-Event-Id", eventID)
	w2 := httptest.NewRecorder()
	mux.ServeHTTP(w2, req2)

	if w2.Code != http.StatusOK {
		t.Fatalf("expected 200 OK on safe retry, got %d: %s", w2.Code, w2.Body.String())
	}

	// Order is now PAID_WAITING and event is PROCESSED
	order, _ := app.db.GetOrder(context.Background(), orderID)
	if order.Status != contracts.JobStatusPaidWaiting {
		t.Errorf("expected order status PAID_WAITING, got %s", order.Status)
	}

	evtUpdated, _ := app.db.GetWebhookEvent(context.Background(), eventID)
	if evtUpdated.Status != store.WebhookEventProcessed {
		t.Errorf("expected webhook event PROCESSED, got %s", evtUpdated.Status)
	}
}

func TestRazorpayWebhook_Validations(t *testing.T) {
	mockClient := &mockRazorpayClient{}
	app, mux, kioskID, kioskSecret, webhookSecret := setupPaymentWorkflowServer(t, mockClient)

	orderID, sessionID := setupOrderAndRedeemPIN(t, app, mux, kioskID, kioskSecret, 500)

	bodyBytes, _ := json.Marshal(InitiateQRRequest{SessionID: sessionID})
	req := httptest.NewRequest(http.MethodPost, "/api/kiosk/payment/initiate-qr", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Kiosk-ID", kioskID)
	req.Header.Set("X-Kiosk-Secret", kioskSecret)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	var qrResp InitiateQRResponse
	_ = json.Unmarshal(w.Body.Bytes(), &qrResp)

	// 1. Invalid signature -> 400
	payload := []byte(`{"event":"qr_code.credited"}`)
	reqBadSig := httptest.NewRequest(http.MethodPost, "/api/webhooks/razorpay", bytes.NewReader(payload))
	reqBadSig.Header.Set("X-Razorpay-Signature", "invalid_signature_hex_1234567890abcdef")
	wBadSig := httptest.NewRecorder()
	mux.ServeHTTP(wBadSig, reqBadSig)
	if wBadSig.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for bad signature, got %d", wBadSig.Code)
	}

	// 2. Amount mismatch (order is 500 paise, payment says 300 paise) -> 400
	mismatchPayload := map[string]interface{}{
		"entity": "event",
		"event":  "qr_code.credited",
		"payload": map[string]interface{}{
			"qr_code": map[string]interface{}{
				"entity": map[string]interface{}{
					"id": qrResp.RazorpayQRID,
				},
			},
			"payment": map[string]interface{}{
				"entity": map[string]interface{}{
					"id":       "pay_mismatch_1",
					"amount":   300, // wrong amount!
					"currency": "INR",
					"status":   "captured",
				},
			},
		},
	}
	mismatchBytes, _ := json.Marshal(mismatchPayload)
	mismatchSig := computeWebhookSignature(mismatchBytes, webhookSecret)

	reqMismatch := httptest.NewRequest(http.MethodPost, "/api/webhooks/razorpay", bytes.NewReader(mismatchBytes))
	reqMismatch.Header.Set("Content-Type", "application/json")
	reqMismatch.Header.Set("X-Razorpay-Signature", mismatchSig)
	reqMismatch.Header.Set("X-Razorpay-Event-Id", "evt_mismatch_1")
	wMismatch := httptest.NewRecorder()
	mux.ServeHTTP(wMismatch, reqMismatch)

	if wMismatch.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for amount mismatch, got %d: %s", wMismatch.Code, wMismatch.Body.String())
	}

	// 3. Currency mismatch (e.g. USD) -> 400
	usdPayload := map[string]interface{}{
		"entity": "event",
		"event":  "qr_code.credited",
		"payload": map[string]interface{}{
			"qr_code": map[string]interface{}{
				"entity": map[string]interface{}{
					"id": qrResp.RazorpayQRID,
				},
			},
			"payment": map[string]interface{}{
				"entity": map[string]interface{}{
					"id":       "pay_usd_1",
					"amount":   500,
					"currency": "USD", // wrong currency!
					"status":   "captured",
				},
			},
		},
	}
	usdBytes, _ := json.Marshal(usdPayload)
	usdSig := computeWebhookSignature(usdBytes, webhookSecret)

	reqUSD := httptest.NewRequest(http.MethodPost, "/api/webhooks/razorpay", bytes.NewReader(usdBytes))
	reqUSD.Header.Set("Content-Type", "application/json")
	reqUSD.Header.Set("X-Razorpay-Signature", usdSig)
	reqUSD.Header.Set("X-Razorpay-Event-Id", "evt_usd_1")
	wUSD := httptest.NewRecorder()
	mux.ServeHTTP(wUSD, reqUSD)

	if wUSD.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for non-INR currency, got %d: %s", wUSD.Code, wUSD.Body.String())
	}

	// 4. Payment status not captured -> 400
	failedStatusPayload := map[string]interface{}{
		"entity": "event",
		"event":  "qr_code.credited",
		"payload": map[string]interface{}{
			"qr_code": map[string]interface{}{
				"entity": map[string]interface{}{
					"id": qrResp.RazorpayQRID,
				},
			},
			"payment": map[string]interface{}{
				"entity": map[string]interface{}{
					"id":       "pay_failed_1",
					"amount":   500,
					"currency": "INR",
					"status":   "failed", // not captured!
				},
			},
		},
	}
	failedStatusBytes, _ := json.Marshal(failedStatusPayload)
	failedStatusSig := computeWebhookSignature(failedStatusBytes, webhookSecret)

	reqFailed := httptest.NewRequest(http.MethodPost, "/api/webhooks/razorpay", bytes.NewReader(failedStatusBytes))
	reqFailed.Header.Set("Content-Type", "application/json")
	reqFailed.Header.Set("X-Razorpay-Signature", failedStatusSig)
	reqFailed.Header.Set("X-Razorpay-Event-Id", "evt_failed_status_1")
	wFailed := httptest.NewRecorder()
	mux.ServeHTTP(wFailed, reqFailed)

	if wFailed.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for uncaptured payment status, got %d: %s", wFailed.Code, wFailed.Body.String())
	}

	// Verify order status was never modified from PENDING_PAYMENT
	order, _ := app.db.GetOrder(context.Background(), orderID)
	if order.Status != contracts.JobStatusPendingPayment {
		t.Errorf("order status was corrupted after validation failures: got %s, want PENDING_PAYMENT", order.Status)
	}
}

func TestRazorpayWebhook_PaymentCaptured_Success(t *testing.T) {
	mockClient := &mockRazorpayClient{}
	app, mux, kioskID, kioskSecret, webhookSecret := setupPaymentWorkflowServer(t, mockClient)

	orderID, _ := setupOrderAndRedeemPIN(t, app, mux, kioskID, kioskSecret, 500)

	// Standard checkout webhook payload
	webhookPayload := map[string]interface{}{
		"entity": "event",
		"event":  "payment.captured",
		"payload": map[string]interface{}{
			"payment": map[string]interface{}{
				"entity": map[string]interface{}{
					"id":       "pay_standard_123",
					"amount":   500,
					"currency": "INR",
					"status":   "captured",
					"order_id": orderID,
					"notes": map[string]interface{}{
						"order_id": orderID,
					},
				},
			},
		},
	}
	payloadRaw, _ := json.Marshal(webhookPayload)
	sig := computeWebhookSignature(payloadRaw, webhookSecret)

	req := httptest.NewRequest(http.MethodPost, "/api/webhooks/razorpay", bytes.NewReader(payloadRaw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Razorpay-Signature", sig)
	req.Header.Set("X-Razorpay-Event-Id", "evt_standard_123")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for payment.captured, got %d: %s", w.Code, w.Body.String())
	}

	order, _ := app.db.GetOrder(context.Background(), orderID)
	if order.Status != contracts.JobStatusPaidWaiting {
		t.Errorf("expected order status PAID_WAITING, got %s", order.Status)
	}
}

// TestKioskInitiateQR_ConcurrentRequestsSingleQRCreation verifies that multiple simultaneous
// requests for the same payment session result in exactly one Razorpay QR code creation,
// and all concurrent callers receive the identical payment attempt details.
func TestKioskInitiateQR_ConcurrentRequestsSingleQRCreation(t *testing.T) {
	mockClient := &mockRazorpayClient{}
	app, mux, kioskID, kioskSecret, _ := setupPaymentWorkflowServer(t, mockClient)

	orderID, sessionID := setupOrderAndRedeemPIN(t, app, mux, kioskID, kioskSecret, 500)
	_ = orderID

	bodyBytes, _ := json.Marshal(InitiateQRRequest{SessionID: sessionID})

	const concurrency = 10
	var wg sync.WaitGroup
	wg.Add(concurrency)

	responses := make([]*httptest.ResponseRecorder, concurrency)
	parsedResps := make([]InitiateQRResponse, concurrency)

	for i := 0; i < concurrency; i++ {
		idx := i
		go func() {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodPost, "/api/kiosk/payment/initiate-qr", bytes.NewReader(bodyBytes))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Kiosk-ID", kioskID)
			req.Header.Set("X-Kiosk-Secret", kioskSecret)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			responses[idx] = rec

			if rec.Code == http.StatusOK {
				_ = json.Unmarshal(rec.Body.Bytes(), &parsedResps[idx])
			}
		}()
	}

	wg.Wait()

	// 1. Verify every concurrent caller received HTTP 200
	for i := 0; i < concurrency; i++ {
		if responses[i].Code != http.StatusOK {
			t.Fatalf("concurrent request %d failed with status %d: %s", i, responses[i].Code, responses[i].Body.String())
		}
	}

	// 2. Verify Razorpay API was called EXACTLY ONCE
	if mockClient.getCallCount() != 1 {
		t.Fatalf("expected exactly 1 Razorpay QR API call across %d concurrent requests, got %d", concurrency, mockClient.getCallCount())
	}

	// 3. Verify all concurrent callers received the identical attempt and QR ID
	expectedAttemptID := parsedResps[0].AttemptID
	expectedQRID := parsedResps[0].RazorpayQRID
	for i := 1; i < concurrency; i++ {
		if parsedResps[i].AttemptID != expectedAttemptID {
			t.Errorf("request %d received attempt ID %s, expected %s", i, parsedResps[i].AttemptID, expectedAttemptID)
		}
		if parsedResps[i].RazorpayQRID != expectedQRID {
			t.Errorf("request %d received QR ID %s, expected %s", i, parsedResps[i].RazorpayQRID, expectedQRID)
		}
	}
}

// TestRazorpayWebhook_MissingEventIDHeader verifies that webhooks lacking the X-Razorpay-Event-Id
// header are strictly rejected with 400 Bad Request before processing.
func TestRazorpayWebhook_MissingEventIDHeader(t *testing.T) {
	mockClient := &mockRazorpayClient{}
	_, mux, _, _, webhookSecret := setupPaymentWorkflowServer(t, mockClient)

	payload := []byte(`{"event":"qr_code.credited"}`)
	sig := computeWebhookSignature(payload, webhookSecret)

	req := httptest.NewRequest(http.MethodPost, "/api/webhooks/razorpay", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Razorpay-Signature", sig)
	// Notice: X-Razorpay-Event-Id header is intentionally missing!

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 Bad Request for missing event ID header, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "missing required X-Razorpay-Event-Id header") {
		t.Errorf("unexpected error message: %s", w.Body.String())
	}
}

// TestRazorpayWebhook_ConcurrentDeliveriesDeduplication verifies that simultaneous deliveries
// of the exact same webhook event are serialized, exactly one processes the order, and the rest
// return {"status":"duplicate"} without double processing or database conflicts.
func TestRazorpayWebhook_ConcurrentDeliveriesDeduplication(t *testing.T) {
	mockClient := &mockRazorpayClient{}
	app, mux, kioskID, kioskSecret, webhookSecret := setupPaymentWorkflowServer(t, mockClient)

	orderID, sessionID := setupOrderAndRedeemPIN(t, app, mux, kioskID, kioskSecret, 500)

	// Initiate QR code
	bodyBytes, _ := json.Marshal(InitiateQRRequest{SessionID: sessionID})
	req := httptest.NewRequest(http.MethodPost, "/api/kiosk/payment/initiate-qr", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Kiosk-ID", kioskID)
	req.Header.Set("X-Kiosk-Secret", kioskSecret)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	var qrResp InitiateQRResponse
	_ = json.Unmarshal(w.Body.Bytes(), &qrResp)

	webhookPayload := map[string]interface{}{
		"entity": "event",
		"event":  "qr_code.credited",
		"payload": map[string]interface{}{
			"qr_code": map[string]interface{}{
				"entity": map[string]interface{}{
					"id": qrResp.RazorpayQRID,
				},
			},
			"payment": map[string]interface{}{
				"entity": map[string]interface{}{
					"id":       "pay_concurrent_dup_123",
					"amount":   500,
					"currency": "INR",
					"status":   "captured",
				},
			},
		},
	}
	payloadRaw, _ := json.Marshal(webhookPayload)
	sig := computeWebhookSignature(payloadRaw, webhookSecret)

	const concurrency = 10
	var wg sync.WaitGroup
	wg.Add(concurrency)

	responses := make([]*httptest.ResponseRecorder, concurrency)
	eventID := "evt_concurrent_multi_999"

	for i := 0; i < concurrency; i++ {
		idx := i
		go func() {
			defer wg.Done()
			whReq := httptest.NewRequest(http.MethodPost, "/api/webhooks/razorpay", bytes.NewReader(payloadRaw))
			whReq.Header.Set("Content-Type", "application/json")
			whReq.Header.Set("X-Razorpay-Signature", sig)
			whReq.Header.Set("X-Razorpay-Event-Id", eventID)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, whReq)
			responses[idx] = rec
		}()
	}

	wg.Wait()

	// Count responses: exactly 1 must be {"status":"ok"} and concurrency-1 must be {"status":"duplicate"}
	okCount := 0
	duplicateCount := 0
	for i := 0; i < concurrency; i++ {
		if responses[i].Code != http.StatusOK {
			t.Fatalf("webhook call %d failed with status %d: %s", i, responses[i].Code, responses[i].Body.String())
		}
		body := responses[i].Body.String()
		if strings.Contains(body, `"status":"ok"`) {
			okCount++
		} else if strings.Contains(body, `"status":"duplicate"`) {
			duplicateCount++
		} else {
			t.Errorf("unexpected webhook response: %s", body)
		}
	}

	if okCount != 1 {
		t.Errorf("expected exactly 1 OK response among concurrent webhooks, got %d", okCount)
	}
	if duplicateCount != concurrency-1 {
		t.Errorf("expected %d duplicate responses, got %d", concurrency-1, duplicateCount)
	}

	// Verify order status is cleanly PAID_WAITING
	order, err := app.db.GetOrder(context.Background(), orderID)
	if err != nil || order == nil {
		t.Fatalf("failed to get order: %v", err)
	}
	if order.Status != contracts.JobStatusPaidWaiting {
		t.Errorf("expected order status PAID_WAITING, got %s", order.Status)
	}

	// Verify event status is PROCESSED
	evt, err := app.db.GetWebhookEvent(context.Background(), eventID)
	if err != nil || evt == nil {
		t.Fatalf("failed to get webhook event: %v", err)
	}
	if evt.Status != store.WebhookEventProcessed {
		t.Errorf("expected webhook event PROCESSED, got %s", evt.Status)
	}
}

// -----------------------------------------------------------------------------
// [MOCKED TEST DOUBLES] - Razorpay Standard Payment Link Integration Tests
// The following tests use in-memory test doubles (mockRazorpayClient) to verify
// Standard Payment Link generation, QR code image rendering, and webhook verification
// without external network access or mutating live payment provider state.
// -----------------------------------------------------------------------------

func TestPaymentLink_Initiate_Success(t *testing.T) {
	mockClient := &mockRazorpayClient{}
	app, mux, kioskID, kioskSecret, _ := setupPaymentWorkflowServer(t, mockClient)

	// Order for ₹3.00 (300 paise) matching manual checkout test reference
	orderID, sessionID := setupOrderAndRedeemPIN(t, app, mux, kioskID, kioskSecret, 300)

	bodyBytes, _ := json.Marshal(InitiateQRRequest{SessionID: sessionID})
	req := httptest.NewRequest(http.MethodPost, "/api/kiosk/payment/initiate-qr", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Kiosk-ID", kioskID)
	req.Header.Set("X-Kiosk-Secret", kioskSecret)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("failed to initiate payment link: %d %s", w.Code, w.Body.String())
	}

	var resp InitiateQRResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse initiate response: %v", err)
	}

	if resp.SessionID != sessionID {
		t.Errorf("expected SessionID %s, got %s", sessionID, resp.SessionID)
	}
	if resp.OrderID != orderID {
		t.Errorf("expected OrderID %s, got %s", orderID, resp.OrderID)
	}
	if !strings.HasPrefix(resp.AttemptID, "att_") {
		t.Errorf("expected AttemptID prefix 'att_', got %s", resp.AttemptID)
	}
	if !strings.HasPrefix(resp.RazorpayQRID, "plink_") {
		t.Errorf("expected RazorpayQRID prefix 'plink_', got %s", resp.RazorpayQRID)
	}
	if !strings.HasPrefix(resp.PaymentLink, "https://rzp.io/i/") {
		t.Errorf("expected PaymentLink starting with 'https://rzp.io/i/', got %s", resp.PaymentLink)
	}
	if !strings.Contains(resp.QRImageURL, "/api/kiosk/payment/qr.png?attempt_id=") {
		t.Errorf("expected QRImageURL to contain '/api/kiosk/payment/qr.png?attempt_id=', got %s", resp.QRImageURL)
	}
	if resp.AmountPaise != 300 {
		t.Errorf("expected AmountPaise 300, got %d", resp.AmountPaise)
	}
	if resp.Currency != "INR" {
		t.Errorf("expected Currency INR, got %s", resp.Currency)
	}
	if resp.Status != "CREATED" {
		t.Errorf("expected Status CREATED, got %s", resp.Status)
	}

	// Verify persistence in database
	attempt, err := app.db.GetPaymentAttemptByID(context.Background(), resp.AttemptID)
	if err != nil || attempt == nil {
		t.Fatalf("payment attempt not found in db: %v", err)
	}
	if attempt.PaymentMethod != store.PaymentMethodRazorpayStandard {
		t.Errorf("expected PaymentMethod RAZORPAY_STANDARD, got %s", attempt.PaymentMethod)
	}
	if attempt.RazorpayQRID != resp.RazorpayQRID {
		t.Errorf("expected RazorpayQRID %s, got %s", resp.RazorpayQRID, attempt.RazorpayQRID)
	}
	if attempt.AmountPaise != 300 {
		t.Errorf("expected AmountPaise 300, got %d", attempt.AmountPaise)
	}
}

func TestPaymentLink_QRCodeImageEndpoint_SuccessAndDecodable(t *testing.T) {
	mockClient := &mockRazorpayClient{}
	app, mux, kioskID, kioskSecret, _ := setupPaymentWorkflowServer(t, mockClient)

	_, sessionID := setupOrderAndRedeemPIN(t, app, mux, kioskID, kioskSecret, 300)

	bodyBytes, _ := json.Marshal(InitiateQRRequest{SessionID: sessionID})
	req := httptest.NewRequest(http.MethodPost, "/api/kiosk/payment/initiate-qr", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Kiosk-ID", kioskID)
	req.Header.Set("X-Kiosk-Secret", kioskSecret)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("failed to initiate payment link: %d %s", w.Code, w.Body.String())
	}

	var resp InitiateQRResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)

	// 1. Fetch generated QR image without credentials -> 401 Unauthorized
	imgReqUnauth := httptest.NewRequest(http.MethodGet, "/api/kiosk/payment/qr.png?attempt_id="+resp.AttemptID, nil)
	imgRecUnauth := httptest.NewRecorder()
	mux.ServeHTTP(imgRecUnauth, imgReqUnauth)
	if imgRecUnauth.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 Unauthorized without kiosk credentials, got %d", imgRecUnauth.Code)
	}

	// 2. Fetch with cross-kiosk credentials (kiosk-other trying to access kiosk-demo's attempt) -> 403 Forbidden
	otherSecret := "kiosk-demo-super-secret-machine-key"
	imgReqCross := httptest.NewRequest(http.MethodGet, "/api/kiosk/payment/qr.png?attempt_id="+resp.AttemptID, nil)
	imgReqCross.Header.Set("X-Kiosk-ID", "kiosk-other")
	imgReqCross.Header.Set("X-Kiosk-Secret", otherSecret)
	imgRecCross := httptest.NewRecorder()
	mux.ServeHTTP(imgRecCross, imgReqCross)
	if imgRecCross.Code != http.StatusForbidden {
		t.Fatalf("expected 403 Forbidden for cross-kiosk attempt access, got %d", imgRecCross.Code)
	}

	// 3. Authorized kiosk fetches its own attempt -> 200 OK with strict Cache-Control
	imgReq := httptest.NewRequest(http.MethodGet, "/api/kiosk/payment/qr.png?attempt_id="+resp.AttemptID, nil)
	imgReq.Header.Set("X-Kiosk-ID", kioskID)
	imgReq.Header.Set("X-Kiosk-Secret", kioskSecret)
	imgRec := httptest.NewRecorder()
	mux.ServeHTTP(imgRec, imgReq)

	if imgRec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for qr.png with authorized kiosk, got %d: %s", imgRec.Code, imgRec.Body.String())
	}

	contentType := imgRec.Header().Get("Content-Type")
	if contentType != "image/png" {
		t.Errorf("expected Content-Type 'image/png', got %s", contentType)
	}

	cacheControl := imgRec.Header().Get("Cache-Control")
	if !strings.Contains(cacheControl, "no-store") || !strings.Contains(cacheControl, "private") {
		t.Errorf("expected strict private no-store Cache-Control, got %s", cacheControl)
	}

	// Verify image is a valid, decodable PNG image
	img, err := png.Decode(imgRec.Body)
	if err != nil {
		t.Fatalf("failed to decode returned image bytes as PNG: %v", err)
	}
	if img.Bounds().Dx() <= 0 || img.Bounds().Dy() <= 0 {
		t.Fatalf("invalid image bounds: %v", img.Bounds())
	}

	// 4. Fetch by session_id also enforces kiosk ownership
	imgReqSess := httptest.NewRequest(http.MethodGet, "/api/kiosk/payment/qr.png?session_id="+sessionID, nil)
	imgReqSess.Header.Set("X-Kiosk-ID", kioskID)
	imgReqSess.Header.Set("X-Kiosk-Secret", kioskSecret)
	imgRecSess := httptest.NewRecorder()
	mux.ServeHTTP(imgRecSess, imgReqSess)
	if imgRecSess.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for qr.png by session_id, got %d: %s", imgRecSess.Code, imgRecSess.Body.String())
	}
}

func TestRazorpayWebhook_PaymentLinkPaid_Success(t *testing.T) {
	mockClient := &mockRazorpayClient{}
	app, mux, kioskID, kioskSecret, webhookSecret := setupPaymentWorkflowServer(t, mockClient)

	orderID, sessionID := setupOrderAndRedeemPIN(t, app, mux, kioskID, kioskSecret, 300)

	// Initiate Payment Link
	bodyBytes, _ := json.Marshal(InitiateQRRequest{SessionID: sessionID})
	req := httptest.NewRequest(http.MethodPost, "/api/kiosk/payment/initiate-qr", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Kiosk-ID", kioskID)
	req.Header.Set("X-Kiosk-Secret", kioskSecret)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("failed to initiate: %d %s", w.Code, w.Body.String())
	}

	var qrResp InitiateQRResponse
	_ = json.Unmarshal(w.Body.Bytes(), &qrResp)

	// Build official Razorpay payment_link.paid webhook payload
	webhookPayload := map[string]interface{}{
		"entity": "event",
		"event":  "payment_link.paid",
		"payload": map[string]interface{}{
			"payment_link": map[string]interface{}{
				"entity": map[string]interface{}{
					"id":           qrResp.RazorpayQRID,
					"amount":       300,
					"amount_paid":  300,
					"currency":     "INR",
					"status":       "paid",
					"short_url":    qrResp.PaymentLink,
					"reference_id": "XG-TEST-001",
					"notes": map[string]string{
						"order_id":   orderID,
						"session_id": sessionID,
						"kiosk_id":   kioskID,
					},
				},
			},
			"payment": map[string]interface{}{
				"entity": map[string]interface{}{
					"id":       "pay_TxnrgB8J7cbi3X",
					"amount":   300,
					"currency": "INR",
					"status":   "captured",
					"method":   "netbanking",
					"notes": map[string]string{
						"order_id":   orderID,
						"session_id": sessionID,
						"kiosk_id":   kioskID,
					},
				},
			},
		},
	}

	payloadRaw, _ := json.Marshal(webhookPayload)
	sig := computeWebhookSignature(payloadRaw, webhookSecret)

	webhookReq := httptest.NewRequest(http.MethodPost, "/api/webhooks/razorpay", bytes.NewReader(payloadRaw))
	webhookReq.Header.Set("Content-Type", "application/json")
	webhookReq.Header.Set("X-Razorpay-Signature", sig)
	webhookReq.Header.Set("X-Razorpay-Event-Id", "evt_plink_paid_001")

	whResp := httptest.NewRecorder()
	mux.ServeHTTP(whResp, webhookReq)

	if whResp.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for payment_link.paid webhook, got %d: %s", whResp.Code, whResp.Body.String())
	}

	ctx := context.Background()

	// 1. Order status strictly transitions to PAID_WAITING
	order, err := app.db.GetOrder(ctx, orderID)
	if err != nil || order == nil {
		t.Fatalf("order not found: %v", err)
	}
	if order.Status != contracts.JobStatusPaidWaiting {
		t.Errorf("expected order status PAID_WAITING, got %s", order.Status)
	}

	// 2. Payment attempt transitions to CAPTURED
	attempt, err := app.db.GetPaymentAttemptByID(ctx, qrResp.AttemptID)
	if err != nil || attempt == nil {
		t.Fatalf("attempt not found: %v", err)
	}
	if attempt.Status != store.PaymentAttemptCaptured {
		t.Errorf("expected attempt status CAPTURED, got %s", attempt.Status)
	}
	if attempt.RazorpayPaymentID != "pay_TxnrgB8J7cbi3X" {
		t.Errorf("expected RazorpayPaymentID 'pay_TxnrgB8J7cbi3X', got %s", attempt.RazorpayPaymentID)
	}

	// 3. Payment session transitions to COMPLETED
	session, err := app.db.GetPaymentSession(ctx, sessionID)
	if err != nil || session == nil {
		t.Fatalf("session not found: %v", err)
	}
	if session.Status != store.PaymentSessionCompleted {
		t.Errorf("expected session status COMPLETED, got %s", session.Status)
	}

	// 4. Webhook event marked PROCESSED
	evt, err := app.db.GetWebhookEvent(ctx, "evt_plink_paid_001")
	if err != nil || evt == nil {
		t.Fatalf("webhook event not found: %v", err)
	}
	if evt.Status != store.WebhookEventProcessed {
		t.Errorf("expected webhook event PROCESSED, got %s", evt.Status)
	}

	// 5. Check payment status endpoint reports paid
	statusReq := httptest.NewRequest(http.MethodGet, "/api/kiosk/payment/status?session_id="+sessionID, nil)
	statusReq.Header.Set("X-Kiosk-ID", kioskID)
	statusReq.Header.Set("X-Kiosk-Secret", kioskSecret)
	statusRec := httptest.NewRecorder()
	mux.ServeHTTP(statusRec, statusReq)
	if statusRec.Code != http.StatusOK {
		t.Fatalf("payment status endpoint returned %d: %s", statusRec.Code, statusRec.Body.String())
	}
	var stResp PaymentStatusResponse
	_ = json.Unmarshal(statusRec.Body.Bytes(), &stResp)
	if !stResp.IsPaid {
		t.Errorf("expected is_paid == true")
	}
	if stResp.OrderStatus != "PAID_WAITING" {
		t.Errorf("expected order_status 'PAID_WAITING', got %s", stResp.OrderStatus)
	}
}

func TestRazorpayWebhook_PaymentLinkPaid_AmountAndCurrencyValidation(t *testing.T) {
	mockClient := &mockRazorpayClient{}
	app, mux, kioskID, kioskSecret, webhookSecret := setupPaymentWorkflowServer(t, mockClient)

	orderID, sessionID := setupOrderAndRedeemPIN(t, app, mux, kioskID, kioskSecret, 300)

	bodyBytes, _ := json.Marshal(InitiateQRRequest{SessionID: sessionID})
	req := httptest.NewRequest(http.MethodPost, "/api/kiosk/payment/initiate-qr", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Kiosk-ID", kioskID)
	req.Header.Set("X-Kiosk-Secret", kioskSecret)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	var qrResp InitiateQRResponse
	_ = json.Unmarshal(w.Body.Bytes(), &qrResp)

	// 1. Currency mismatch (USD instead of INR)
	whPayloadUSD := map[string]interface{}{
		"entity": "event",
		"event":  "payment_link.paid",
		"payload": map[string]interface{}{
			"payment_link": map[string]interface{}{
				"entity": map[string]interface{}{
					"id":          qrResp.RazorpayQRID,
					"amount":      300,
					"amount_paid": 300,
					"currency":    "USD",
					"status":      "paid",
				},
			},
			"payment": map[string]interface{}{
				"entity": map[string]interface{}{
					"id":       "pay_usd_001",
					"amount":   300,
					"currency": "USD",
					"status":   "captured",
				},
			},
		},
	}
	rawUSD, _ := json.Marshal(whPayloadUSD)
	reqUSD := httptest.NewRequest(http.MethodPost, "/api/webhooks/razorpay", bytes.NewReader(rawUSD))
	reqUSD.Header.Set("Content-Type", "application/json")
	reqUSD.Header.Set("X-Razorpay-Signature", computeWebhookSignature(rawUSD, webhookSecret))
	reqUSD.Header.Set("X-Razorpay-Event-Id", "evt_usd_001")
	recUSD := httptest.NewRecorder()
	mux.ServeHTTP(recUSD, reqUSD)
	if recUSD.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for non-INR currency, got %d: %s", recUSD.Code, recUSD.Body.String())
	}

	// 2. Amount mismatch (100 paise instead of 300 paise)
	whPayloadAmount := map[string]interface{}{
		"entity": "event",
		"event":  "payment_link.paid",
		"payload": map[string]interface{}{
			"payment_link": map[string]interface{}{
				"entity": map[string]interface{}{
					"id":          qrResp.RazorpayQRID,
					"amount":      100,
					"amount_paid": 100,
					"currency":    "INR",
					"status":      "paid",
				},
			},
			"payment": map[string]interface{}{
				"entity": map[string]interface{}{
					"id":       "pay_mismatch_001",
					"amount":   100,
					"currency": "INR",
					"status":   "captured",
				},
			},
		},
	}
	rawAmt, _ := json.Marshal(whPayloadAmount)
	reqAmt := httptest.NewRequest(http.MethodPost, "/api/webhooks/razorpay", bytes.NewReader(rawAmt))
	reqAmt.Header.Set("Content-Type", "application/json")
	reqAmt.Header.Set("X-Razorpay-Signature", computeWebhookSignature(rawAmt, webhookSecret))
	reqAmt.Header.Set("X-Razorpay-Event-Id", "evt_amt_001")
	recAmt := httptest.NewRecorder()
	mux.ServeHTTP(recAmt, reqAmt)
	if recAmt.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for amount mismatch, got %d: %s", recAmt.Code, recAmt.Body.String())
	}

	// 3. Status not captured
	whPayloadStatus := map[string]interface{}{
		"entity": "event",
		"event":  "payment_link.paid",
		"payload": map[string]interface{}{
			"payment_link": map[string]interface{}{
				"entity": map[string]interface{}{
					"id":          qrResp.RazorpayQRID,
					"amount":      300,
					"amount_paid": 300,
					"currency":    "INR",
					"status":      "paid",
				},
			},
			"payment": map[string]interface{}{
				"entity": map[string]interface{}{
					"id":       "pay_fail_001",
					"amount":   300,
					"currency": "INR",
					"status":   "failed",
				},
			},
		},
	}
	rawStat, _ := json.Marshal(whPayloadStatus)
	reqStat := httptest.NewRequest(http.MethodPost, "/api/webhooks/razorpay", bytes.NewReader(rawStat))
	reqStat.Header.Set("Content-Type", "application/json")
	reqStat.Header.Set("X-Razorpay-Signature", computeWebhookSignature(rawStat, webhookSecret))
	reqStat.Header.Set("X-Razorpay-Event-Id", "evt_stat_001")
	recStat := httptest.NewRecorder()
	mux.ServeHTTP(recStat, reqStat)
	if recStat.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for non-captured payment status, got %d: %s", recStat.Code, recStat.Body.String())
	}

	// 4. Razorpay Payment Link ID mismatch
	whPayloadMismatchID := map[string]interface{}{
		"entity": "event",
		"event":  "payment_link.paid",
		"payload": map[string]interface{}{
			"payment_link": map[string]interface{}{
				"entity": map[string]interface{}{
					"id":          "plink_unknown_id_999",
					"amount":      300,
					"amount_paid": 300,
					"currency":    "INR",
					"status":      "paid",
				},
			},
			"payment": map[string]interface{}{
				"entity": map[string]interface{}{
					"id":       "pay_mismatch_plink_001",
					"amount":   300,
					"currency": "INR",
					"status":   "captured",
				},
			},
		},
	}
	rawMisID, _ := json.Marshal(whPayloadMismatchID)
	reqMisID := httptest.NewRequest(http.MethodPost, "/api/webhooks/razorpay", bytes.NewReader(rawMisID))
	reqMisID.Header.Set("Content-Type", "application/json")
	reqMisID.Header.Set("X-Razorpay-Signature", computeWebhookSignature(rawMisID, webhookSecret))
	reqMisID.Header.Set("X-Razorpay-Event-Id", "evt_mis_id_001")
	recMisID := httptest.NewRecorder()
	mux.ServeHTTP(recMisID, reqMisID)
	if recMisID.Code != http.StatusNotFound && recMisID.Code != http.StatusBadRequest {
		t.Errorf("expected 400 or 404 for unknown/mismatched plink ID, got %d: %s", recMisID.Code, recMisID.Body.String())
	}

	// 5. Order ID mismatch in notes
	whPayloadOrderMis := map[string]interface{}{
		"entity": "event",
		"event":  "payment_link.paid",
		"payload": map[string]interface{}{
			"payment_link": map[string]interface{}{
				"entity": map[string]interface{}{
					"id":          qrResp.RazorpayQRID,
					"amount":      300,
					"amount_paid": 300,
					"currency":    "INR",
					"status":      "paid",
					"notes": map[string]string{
						"order_id":   "ord_fraudulent_999",
						"session_id": sessionID,
					},
				},
			},
			"payment": map[string]interface{}{
				"entity": map[string]interface{}{
					"id":       "pay_order_mis_001",
					"amount":   300,
					"currency": "INR",
					"status":   "captured",
				},
			},
		},
	}
	rawOrdMis, _ := json.Marshal(whPayloadOrderMis)
	reqOrdMis := httptest.NewRequest(http.MethodPost, "/api/webhooks/razorpay", bytes.NewReader(rawOrdMis))
	reqOrdMis.Header.Set("Content-Type", "application/json")
	reqOrdMis.Header.Set("X-Razorpay-Signature", computeWebhookSignature(rawOrdMis, webhookSecret))
	reqOrdMis.Header.Set("X-Razorpay-Event-Id", "evt_ord_mis_001")
	recOrdMis := httptest.NewRecorder()
	mux.ServeHTTP(recOrdMis, reqOrdMis)
	if recOrdMis.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for mismatched order_id in notes, got %d: %s", recOrdMis.Code, recOrdMis.Body.String())
	}

	// 6. Session ID mismatch in notes
	whPayloadSessMis := map[string]interface{}{
		"entity": "event",
		"event":  "payment_link.paid",
		"payload": map[string]interface{}{
			"payment_link": map[string]interface{}{
				"entity": map[string]interface{}{
					"id":          qrResp.RazorpayQRID,
					"amount":      300,
					"amount_paid": 300,
					"currency":    "INR",
					"status":      "paid",
					"notes": map[string]string{
						"order_id":   orderID,
						"session_id": "sess_fraudulent_999",
					},
				},
			},
			"payment": map[string]interface{}{
				"entity": map[string]interface{}{
					"id":       "pay_sess_mis_001",
					"amount":   300,
					"currency": "INR",
					"status":   "captured",
				},
			},
		},
	}
	rawSessMis, _ := json.Marshal(whPayloadSessMis)
	reqSessMis := httptest.NewRequest(http.MethodPost, "/api/webhooks/razorpay", bytes.NewReader(rawSessMis))
	reqSessMis.Header.Set("Content-Type", "application/json")
	reqSessMis.Header.Set("X-Razorpay-Signature", computeWebhookSignature(rawSessMis, webhookSecret))
	reqSessMis.Header.Set("X-Razorpay-Event-Id", "evt_sess_mis_001")
	recSessMis := httptest.NewRecorder()
	mux.ServeHTTP(recSessMis, reqSessMis)
	if recSessMis.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for mismatched session_id in notes, got %d: %s", recSessMis.Code, recSessMis.Body.String())
	}

	// Confirm order is STILL in PENDING_PAYMENT
	order, _ := app.db.GetOrder(context.Background(), orderID)
	if order.Status != contracts.JobStatusPendingPayment {
		t.Errorf("order status should still be PENDING_PAYMENT, got %s", order.Status)
	}
}

func TestRazorpayWebhook_PaymentLinkExpired_Success(t *testing.T) {
	mockClient := &mockRazorpayClient{}
	app, mux, kioskID, kioskSecret, webhookSecret := setupPaymentWorkflowServer(t, mockClient)

	_, sessionID := setupOrderAndRedeemPIN(t, app, mux, kioskID, kioskSecret, 300)

	bodyBytes, _ := json.Marshal(InitiateQRRequest{SessionID: sessionID})
	req := httptest.NewRequest(http.MethodPost, "/api/kiosk/payment/initiate-qr", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Kiosk-ID", kioskID)
	req.Header.Set("X-Kiosk-Secret", kioskSecret)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	var qrResp InitiateQRResponse
	_ = json.Unmarshal(w.Body.Bytes(), &qrResp)

	// Send payment_link.expired webhook
	whPayload := map[string]interface{}{
		"entity": "event",
		"event":  "payment_link.expired",
		"payload": map[string]interface{}{
			"payment_link": map[string]interface{}{
				"entity": map[string]interface{}{
					"id":     qrResp.RazorpayQRID,
					"status": "expired",
				},
			},
		},
	}

	raw, _ := json.Marshal(whPayload)
	whReq := httptest.NewRequest(http.MethodPost, "/api/webhooks/razorpay", bytes.NewReader(raw))
	whReq.Header.Set("Content-Type", "application/json")
	whReq.Header.Set("X-Razorpay-Signature", computeWebhookSignature(raw, webhookSecret))
	whReq.Header.Set("X-Razorpay-Event-Id", "evt_expire_001")
	whRec := httptest.NewRecorder()
	mux.ServeHTTP(whRec, whReq)

	if whRec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for expired event, got %d: %s", whRec.Code, whRec.Body.String())
	}

	attempt, err := app.db.GetPaymentAttemptByID(context.Background(), qrResp.AttemptID)
	if err != nil || attempt == nil {
		t.Fatalf("attempt not found: %v", err)
	}
	if attempt.Status != store.PaymentAttemptExpired {
		t.Errorf("expected attempt status EXPIRED, got %s", attempt.Status)
	}
}

func TestRazorpayWebhook_PaymentFailed_Success(t *testing.T) {
	mockClient := &mockRazorpayClient{}
	app, mux, kioskID, kioskSecret, webhookSecret := setupPaymentWorkflowServer(t, mockClient)

	orderID, sessionID := setupOrderAndRedeemPIN(t, app, mux, kioskID, kioskSecret, 300)

	bodyBytes, _ := json.Marshal(InitiateQRRequest{SessionID: sessionID})
	req := httptest.NewRequest(http.MethodPost, "/api/kiosk/payment/initiate-qr", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Kiosk-ID", kioskID)
	req.Header.Set("X-Kiosk-Secret", kioskSecret)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	var qrResp InitiateQRResponse
	_ = json.Unmarshal(w.Body.Bytes(), &qrResp)

	// Send payment.failed webhook
	whPayload := map[string]interface{}{
		"entity": "event",
		"event":  "payment.failed",
		"payload": map[string]interface{}{
			"payment": map[string]interface{}{
				"entity": map[string]interface{}{
					"id":                "pay_failed_123",
					"amount":            300,
					"currency":          "INR",
					"status":            "failed",
					"error_code":        "BAD_REQUEST_ERROR",
					"error_description": "Payment was declined by customer bank",
					"notes": map[string]string{
						"order_id":   orderID,
						"session_id": sessionID,
					},
				},
			},
		},
	}

	raw, _ := json.Marshal(whPayload)
	whReq := httptest.NewRequest(http.MethodPost, "/api/webhooks/razorpay", bytes.NewReader(raw))
	whReq.Header.Set("Content-Type", "application/json")
	whReq.Header.Set("X-Razorpay-Signature", computeWebhookSignature(raw, webhookSecret))
	whReq.Header.Set("X-Razorpay-Event-Id", "evt_fail_001")
	whRec := httptest.NewRecorder()
	mux.ServeHTTP(whRec, whReq)

	if whRec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for payment.failed event, got %d: %s", whRec.Code, whRec.Body.String())
	}

	attempt, err := app.db.GetPaymentAttemptByID(context.Background(), qrResp.AttemptID)
	if err != nil || attempt == nil {
		t.Fatalf("attempt not found: %v", err)
	}
	if attempt.Status != store.PaymentAttemptFailed {
		t.Errorf("expected attempt status FAILED, got %s", attempt.Status)
	}
	if attempt.ErrorReason != "Payment was declined by customer bank" {
		t.Errorf("expected ErrorReason 'Payment was declined by customer bank', got %s", attempt.ErrorReason)
	}
}

func TestRazorpayWebhook_PaymentLink_DuplicateIdempotency(t *testing.T) {
	mockClient := &mockRazorpayClient{}
	app, mux, kioskID, kioskSecret, webhookSecret := setupPaymentWorkflowServer(t, mockClient)

	orderID, sessionID := setupOrderAndRedeemPIN(t, app, mux, kioskID, kioskSecret, 300)

	bodyBytes, _ := json.Marshal(InitiateQRRequest{SessionID: sessionID})
	req := httptest.NewRequest(http.MethodPost, "/api/kiosk/payment/initiate-qr", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Kiosk-ID", kioskID)
	req.Header.Set("X-Kiosk-Secret", kioskSecret)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	var qrResp InitiateQRResponse
	_ = json.Unmarshal(w.Body.Bytes(), &qrResp)

	webhookPayload := map[string]interface{}{
		"entity": "event",
		"event":  "payment_link.paid",
		"payload": map[string]interface{}{
			"payment_link": map[string]interface{}{
				"entity": map[string]interface{}{
					"id":          qrResp.RazorpayQRID,
					"amount":      300,
					"amount_paid": 300,
					"currency":    "INR",
					"status":      "paid",
					"short_url":   qrResp.PaymentLink,
					"notes": map[string]string{
						"order_id":   orderID,
						"session_id": sessionID,
					},
				},
			},
			"payment": map[string]interface{}{
				"entity": map[string]interface{}{
					"id":       "pay_dupe_123",
					"amount":   300,
					"currency": "INR",
					"status":   "captured",
				},
			},
		},
	}

	payloadRaw, _ := json.Marshal(webhookPayload)
	sig := computeWebhookSignature(payloadRaw, webhookSecret)

	// Delivery 1: initial delivery
	req1 := httptest.NewRequest(http.MethodPost, "/api/webhooks/razorpay", bytes.NewReader(payloadRaw))
	req1.Header.Set("Content-Type", "application/json")
	req1.Header.Set("X-Razorpay-Signature", sig)
	req1.Header.Set("X-Razorpay-Event-Id", "evt_dupe_test_001")
	rec1 := httptest.NewRecorder()
	mux.ServeHTTP(rec1, req1)

	if rec1.Code != http.StatusOK || !strings.Contains(rec1.Body.String(), `"status":"ok"`) {
		t.Fatalf("first delivery failed: %d %s", rec1.Code, rec1.Body.String())
	}

	// Delivery 2: identical duplicate delivery
	req2 := httptest.NewRequest(http.MethodPost, "/api/webhooks/razorpay", bytes.NewReader(payloadRaw))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("X-Razorpay-Signature", sig)
	req2.Header.Set("X-Razorpay-Event-Id", "evt_dupe_test_001")
	rec2 := httptest.NewRecorder()
	mux.ServeHTTP(rec2, req2)

	if rec2.Code != http.StatusOK || !strings.Contains(rec2.Body.String(), `"status":"duplicate"`) {
		t.Fatalf("second delivery should return duplicate: %d %s", rec2.Code, rec2.Body.String())
	}
}

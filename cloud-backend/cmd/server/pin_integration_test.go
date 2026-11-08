package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"xeroxgo/cloud-backend/internal/pins"
	"xeroxgo/cloud-backend/internal/store"
	"xeroxgo/cloud-backend/internal/ws"
	"xeroxgo/pkg/contracts"
)

func setupPINWorkflowServer(t *testing.T) (*appServer, *http.ServeMux, string, string) {
	t.Helper()
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "pin_workflow_test.db")
	jobsDir := filepath.Join(tmpDir, "jobs")

	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open test store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	hmacSecret := []byte("test_pickup_pin_hmac_secret_32_bytes_1234")
	cfg := &serverConfig{
		addr:                  "localhost:0",
		dbPath:                dbPath,
		jobsDir:               jobsDir,
		pricePerSheetPaise:    200,
		downloadSecret:        []byte("test_secret_key_1234567890"),
		pickupPINHMACSecret:   hmacSecret,
		maxActivePINsPerKiosk: 50,
		devMode:               true,
	}

	hub := ws.NewHub(s)
	app := &appServer{
		cfg: cfg,
		db:  s,
		hub: hub,
		pinLimiter: pins.NewRateLimiter(pins.CooldownConfig{
			GraceAttempts:  3,
			WindowDuration: 5 * time.Minute,
			MaxCooldown:    10 * time.Second,
		}),
		convWake:     make(chan struct{}, 10),
		sessionLocks: newKeyedMutex(),
		webhookLocks: newKeyedMutex(),
	}

	// Register test kiosks
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

	// Register inactive kiosk
	err = s.RegisterKiosk(ctx, store.Kiosk{
		KioskID:    "kiosk-inactive",
		SecretHash: hex.EncodeToString(secretHash[:]),
		Name:       "Inactive Kiosk",
		IsActive:   false,
	})
	if err != nil {
		t.Fatalf("failed to register inactive kiosk: %v", err)
	}

	// Register second active kiosk
	err = s.RegisterKiosk(ctx, store.Kiosk{
		KioskID:    "kiosk-other",
		SecretHash: hex.EncodeToString(secretHash[:]),
		Name:       "Other Kiosk",
		IsActive:   true,
	})
	if err != nil {
		t.Fatalf("failed to register second kiosk: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/k/", app.handleUpload)
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

	return app, mux, "kiosk-demo", kioskSecret
}

// helper to create a minimal valid 1-page PDF file in memory
func createMinimalPDFBytes() []byte {
	return []byte("%PDF-1.4\n1 0 obj\n<< /Type /Catalog /Pages 2 0 R >>\nendobj\n" +
		"2 0 obj\n<< /Type /Pages /Kids [3 0 R] /Count 1 >>\nendobj\n" +
		"3 0 obj\n<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] >>\nendobj\n" +
		"xref\n0 4\n0000000000 65535 f \n0000000010 00000 n \n0000000060 00000 n \n0000000117 00000 n \n" +
		"trailer\n<< /Size 4 /Root 1 0 R >>\nstartxref\n188\n%%EOF")
}

// createTestOrderDirect persists a test order directly with ownership secret and 4-digit pickup PIN.
func createTestOrderDirect(t *testing.T, app *appServer, kioskID string) (orderID string, rawSecret string, pin string) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()

	orderHex, _ := randomHex(12)
	orderID = "ord_" + orderHex
	jobHex, _ := randomHex(12)
	jobID := "job_" + jobHex

	var err error
	rawSecret, secretHash, err := pins.GenerateOrderOwnershipSecret()
	if err != nil {
		t.Fatalf("failed to generate order secret: %v", err)
	}

	order := store.Order{
		OrderID: orderID,
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
		Status:          contracts.JobStatusPendingPayment,
		PricePaise:      400,
		ColorMode:       "BW",
		OrderSecretHash: secretHash,
		CreatedAt:       now,
		UpdatedAt:       now,
	}

	if err := app.db.CreateOrder(ctx, order); err != nil {
		t.Fatalf("failed to create order: %v", err)
	}

	pin, err = pins.GenerateUniformPIN()
	if err != nil {
		t.Fatalf("failed to generate PIN: %v", err)
	}

	pinHMAC, err := pins.ComputePINHMAC(app.cfg.pickupPINHMACSecret, kioskID, pin)
	if err != nil {
		t.Fatalf("failed to compute pin HMAC: %v", err)
	}

	pinHex, _ := randomHex(12)
	pinRecord := store.PickupPIN{
		ID:        "pin_" + pinHex,
		OrderID:   orderID,
		KioskID:   kioskID,
		PINHMAC:   pinHMAC,
		Status:    store.PickupPINActive,
		CreatedAt: now,
		ExpiresAt: now.Add(15 * time.Minute),
	}

	if err := app.db.CreatePickupPIN(ctx, pinRecord, 50); err != nil {
		t.Fatalf("failed to create pickup pin: %v", err)
	}

	return orderID, rawSecret, pin
}

func TestKioskAuthentication_ValidationAndRejection(t *testing.T) {
	app, mux, kioskID, kioskSecret := setupPINWorkflowServer(t)

	// 1. Missing credentials entirely -> 401
	body := `{"pin": "1234"}`
	req := httptest.NewRequest(http.MethodPost, "/api/kiosk/pin/redeem", strings.NewReader(body))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for missing credentials, got %d: %s", rec.Code, rec.Body.String())
	}

	// 2. Unknown kiosk ID -> 401
	req = httptest.NewRequest(http.MethodPost, "/api/kiosk/pin/redeem", strings.NewReader(body))
	req.Header.Set("X-Kiosk-ID", "kiosk-nonexistent")
	req.Header.Set("X-Kiosk-Secret", kioskSecret)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for unknown kiosk, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "unknown kiosk") {
		t.Errorf("expected error message to mention unknown kiosk, got %s", rec.Body.String())
	}

	// 3. Inactive kiosk -> 403 Forbidden
	req = httptest.NewRequest(http.MethodPost, "/api/kiosk/pin/redeem", strings.NewReader(body))
	req.Header.Set("X-Kiosk-ID", "kiosk-inactive")
	req.Header.Set("X-Kiosk-Secret", kioskSecret)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for inactive kiosk, got %d: %s", rec.Code, rec.Body.String())
	}

	// 4. Invalid kiosk secret -> 401
	req = httptest.NewRequest(http.MethodPost, "/api/kiosk/pin/redeem", strings.NewReader(body))
	req.Header.Set("X-Kiosk-ID", kioskID)
	req.Header.Set("X-Kiosk-Secret", "wrong-secret-token")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for invalid kiosk secret, got %d: %s", rec.Code, rec.Body.String())
	}

	// 5. Valid credentials -> passes auth check (even if PIN is invalid, passes auth and gets 400)
	req = httptest.NewRequest(http.MethodPost, "/api/kiosk/pin/redeem", strings.NewReader(body))
	req.Header.Set("X-Kiosk-ID", kioskID)
	req.Header.Set("X-Kiosk-Secret", kioskSecret)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code == http.StatusUnauthorized || rec.Code == http.StatusForbidden {
		t.Fatalf("expected auth to succeed with valid credentials, got %d: %s", rec.Code, rec.Body.String())
	}
	_ = app
}

func TestPINRedemption_SingleUseAndPaymentSession(t *testing.T) {
	app, mux, kioskID, kioskSecret := setupPINWorkflowServer(t)

	orderID, _, pin := createTestOrderDirect(t, app, kioskID)

	// 1. Redeem PIN with authenticated kiosk
	reqBody, _ := json.Marshal(pins.PINRedemptionRequest{PIN: pin})
	req := httptest.NewRequest(http.MethodPost, "/api/kiosk/pin/redeem", bytes.NewReader(reqBody))
	req.Header.Set("X-Kiosk-ID", kioskID)
	req.Header.Set("X-Kiosk-Secret", kioskSecret)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK on redemption, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp pins.PINRedemptionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode redemption response: %v", err)
	}

	if resp.Status != "ready_for_payment" {
		t.Fatalf("expected status 'ready_for_payment', got %q", resp.Status)
	}
	if resp.OrderID != orderID {
		t.Fatalf("expected order ID %q, got %q", orderID, resp.OrderID)
	}
	if resp.KioskID != kioskID {
		t.Fatalf("expected kiosk ID %q, got %q", kioskID, resp.KioskID)
	}
	if resp.SessionID == "" {
		t.Fatalf("expected non-empty payment session ID")
	}

	// 2. Verify payment session was created in DB
	sess, err := app.db.GetPaymentSession(context.Background(), resp.SessionID)
	if err != nil || sess == nil {
		t.Fatalf("failed to fetch created payment session from DB: %v", err)
	}
	if sess.OrderID != orderID || sess.KioskID != kioskID {
		t.Fatalf("payment session mismatch: %+v", sess)
	}

	// 3. Verify order status has NOT changed to PRINTING or PAID
	ord, err := app.db.GetOrder(context.Background(), orderID)
	if err != nil || ord == nil {
		t.Fatalf("failed to fetch order: %v", err)
	}
	if ord.Status != contracts.JobStatusPendingPayment {
		t.Fatalf("order status must remain PENDING_PAYMENT after PIN redemption; got %s", ord.Status)
	}

	// 4. Second redemption of same PIN must fail (single-use)
	req2 := httptest.NewRequest(http.MethodPost, "/api/kiosk/pin/redeem", bytes.NewReader(reqBody))
	req2.Header.Set("X-Kiosk-ID", kioskID)
	req2.Header.Set("X-Kiosk-Secret", kioskSecret)
	rec2 := httptest.NewRecorder()
	mux.ServeHTTP(rec2, req2)

	if rec2.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request on second redemption, got %d: %s", rec2.Code, rec2.Body.String())
	}
	if !strings.Contains(rec2.Body.String(), "invalid or expired pickup PIN") {
		t.Fatalf("expected generic error message, got %s", rec2.Body.String())
	}
}

func TestPINRedemption_KioskSeparation(t *testing.T) {
	app, mux, kioskID, kioskSecret := setupPINWorkflowServer(t)

	// Order belongs to kiosk-demo
	_, _, pin := createTestOrderDirect(t, app, kioskID)

	// Attempt redemption from kiosk-other using same PIN
	reqBody, _ := json.Marshal(pins.PINRedemptionRequest{PIN: pin})
	req := httptest.NewRequest(http.MethodPost, "/api/kiosk/pin/redeem", bytes.NewReader(reqBody))
	req.Header.Set("X-Kiosk-ID", "kiosk-other")
	req.Header.Set("X-Kiosk-Secret", kioskSecret)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request for mismatched kiosk PIN, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestPINRefresh_WithOrderOwnershipSecret(t *testing.T) {
	app, mux, kioskID, kioskSecret := setupPINWorkflowServer(t)

	orderID, rawSecret, oldPIN := createTestOrderDirect(t, app, kioskID)

	// 1. Refresh without secret -> 400
	req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/orders/%s/pin/refresh", orderID), strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing secret, got %d", rec.Code)
	}

	// 2. Refresh with invalid secret -> 401
	badBody := `{"order_secret": "invalid_secret_token_123"}`
	req = httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/orders/%s/pin/refresh", orderID), strings.NewReader(badBody))
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for invalid secret, got %d: %s", rec.Code, rec.Body.String())
	}

	// 3. Refresh with valid secret -> 200 OK with new 4-digit PIN
	validBody, _ := json.Marshal(pins.PINRefreshRequest{OrderSecret: rawSecret})
	req = httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/orders/%s/pin/refresh", orderID), bytes.NewReader(validBody))
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for valid refresh, got %d: %s", rec.Code, rec.Body.String())
	}

	var refreshResp pins.PINRefreshResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &refreshResp); err != nil {
		t.Fatalf("failed to decode refresh response: %v", err)
	}
	if len(refreshResp.PickupPIN) != 4 {
		t.Fatalf("expected 4-digit PIN, got %q", refreshResp.PickupPIN)
	}
	if refreshResp.OrderID != orderID {
		t.Fatalf("expected order ID %q, got %q", orderID, refreshResp.OrderID)
	}

	// 4. Old PIN must now fail redemption (superseded)
	oldBody, _ := json.Marshal(pins.PINRedemptionRequest{PIN: oldPIN})
	redeemReq := httptest.NewRequest(http.MethodPost, "/api/kiosk/pin/redeem", bytes.NewReader(oldBody))
	redeemReq.Header.Set("X-Kiosk-ID", kioskID)
	redeemReq.Header.Set("X-Kiosk-Secret", kioskSecret)
	redeemRec := httptest.NewRecorder()
	mux.ServeHTTP(redeemRec, redeemReq)
	if redeemRec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for superseded old PIN, got %d", redeemRec.Code)
	}

	// 5. New refreshed PIN must succeed redemption
	newBody, _ := json.Marshal(pins.PINRedemptionRequest{PIN: refreshResp.PickupPIN})
	redeemReq2 := httptest.NewRequest(http.MethodPost, "/api/kiosk/pin/redeem", bytes.NewReader(newBody))
	redeemReq2.Header.Set("X-Kiosk-ID", kioskID)
	redeemReq2.Header.Set("X-Kiosk-Secret", kioskSecret)
	redeemRec2 := httptest.NewRecorder()
	mux.ServeHTTP(redeemRec2, redeemReq2)
	if redeemRec2.Code != http.StatusOK {
		t.Fatalf("expected 200 OK redeeming new refreshed PIN, got %d: %s", redeemRec2.Code, redeemRec2.Body.String())
	}
}

func TestSanitizedPublicOrderStatus_NoSecretLeakage(t *testing.T) {
	app, mux, kioskID, _ := setupPINWorkflowServer(t)

	orderID, rawSecret, pin := createTestOrderDirect(t, app, kioskID)

	// Fetch public order status
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/orders/%s/status", orderID), nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK on status fetch, got %d", rec.Code)
	}

	rawJSON := rec.Body.String()

	// Verify plaintext PIN is NOT returned
	if strings.Contains(rawJSON, pin) {
		t.Fatalf("security violation: public order status exposes plaintext PIN: %s", rawJSON)
	}
	// Verify raw order ownership secret is NOT returned
	if strings.Contains(rawJSON, rawSecret) {
		t.Fatalf("security violation: public order status exposes order secret: %s", rawJSON)
	}
	// Verify order secret hash is NOT returned
	if strings.Contains(rawJSON, "order_secret_hash") {
		t.Fatalf("security violation: public order status exposes order_secret_hash: %s", rawJSON)
	}
	// Verify display_code and pickup_pin are completely absent from public status response
	var statusMap map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &statusMap); err != nil {
		t.Fatalf("failed to decode status response: %v", err)
	}
	if val, exists := statusMap["display_code"]; exists && val != nil {
		t.Fatalf("security violation: public order status exposes display_code: %v", val)
	}
	if val, exists := statusMap["pickup_pin"]; exists && val != nil {
		t.Fatalf("security violation: public order status exposes pickup_pin: %v", val)
	}
}

func TestRateLimiter_RedemptionCooldownProgression(t *testing.T) {
	_, mux, kioskID, kioskSecret := setupPINWorkflowServer(t)

	body := `{"pin": "9999"}` // non-existent PIN
	for i := 1; i <= 3; i++ {
		req := httptest.NewRequest(http.MethodPost, "/api/kiosk/pin/redeem", strings.NewReader(body))
		req.Header.Set("X-Kiosk-ID", kioskID)
		req.Header.Set("X-Kiosk-Secret", kioskSecret)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("attempt %d: expected 400, got %d: %s", i, rec.Code, rec.Body.String())
		}
	}

	// 4th consecutive failure -> must trigger 429 Too Many Requests
	req := httptest.NewRequest(http.MethodPost, "/api/kiosk/pin/redeem", strings.NewReader(body))
	req.Header.Set("X-Kiosk-ID", kioskID)
	req.Header.Set("X-Kiosk-Secret", kioskSecret)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("attempt 4: expected 429 Too Many Requests, got %d: %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Fatalf("expected Retry-After header on 429 response")
	}
}

func TestPINRedemption_ClientSuppliedSessionIDIgnored(t *testing.T) {
	app, mux, kioskID, kioskSecret := setupPINWorkflowServer(t)

	_, _, pin := createTestOrderDirect(t, app, kioskID)

	maliciousSessionID := "malicious_client_supplied_session_9999"
	reqBody, _ := json.Marshal(pins.PINRedemptionRequest{
		PIN:       pin,
		SessionID: maliciousSessionID,
	})

	req := httptest.NewRequest(http.MethodPost, "/api/kiosk/pin/redeem", bytes.NewReader(reqBody))
	req.Header.Set("X-Kiosk-ID", kioskID)
	req.Header.Set("X-Kiosk-Secret", kioskSecret)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp pins.PINRedemptionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	// Client-supplied session ID must be strictly ignored
	if resp.SessionID == maliciousSessionID {
		t.Fatalf("security violation: server accepted client-supplied session ID: %s", resp.SessionID)
	}
	if !strings.HasPrefix(resp.SessionID, "sess_") {
		t.Fatalf("expected backend-generated session ID starting with 'sess_', got %s", resp.SessionID)
	}

	// Verify database record has backend-generated ID and NOT malicious ID
	ctx := context.Background()
	maliciousSess, _ := app.db.GetPaymentSession(ctx, maliciousSessionID)
	if maliciousSess != nil {
		t.Fatalf("database contains malicious session ID record")
	}

	actualSess, err := app.db.GetPaymentSession(ctx, resp.SessionID)
	if err != nil || actualSess == nil {
		t.Fatalf("failed to fetch backend-generated session from DB: %v", err)
	}
}

func TestPaymentSession_IndependentExpiryAndRetryWithoutPIN(t *testing.T) {
	app, mux, kioskID, kioskSecret := setupPINWorkflowServer(t)
	app.cfg.paymentSessionTTL = 30 * time.Minute

	orderID, _, pin := createTestOrderDirect(t, app, kioskID)
	startTime := time.Now().UTC()

	// 1. Redeem PIN at t=0
	reqBody, _ := json.Marshal(pins.PINRedemptionRequest{PIN: pin})
	req := httptest.NewRequest(http.MethodPost, "/api/kiosk/pin/redeem", bytes.NewReader(reqBody))
	req.Header.Set("X-Kiosk-ID", kioskID)
	req.Header.Set("X-Kiosk-Secret", kioskSecret)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("redemption failed: %d: %s", rec.Code, rec.Body.String())
	}

	var resp pins.PINRedemptionResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	sessionID := resp.SessionID

	ctx := context.Background()

	// 2. Advance time to t=20m (where original 15m PIN would have expired)
	tPlus20m := startTime.Add(20 * time.Minute)

	// Verify PIN is redeemed and its original expiry was at t+15m
	pinRecord, err := app.db.GetPickupPINByOrder(ctx, orderID)
	if err != nil || pinRecord == nil {
		t.Fatalf("failed to get pin record: %v", err)
	}
	if pinRecord.Status != store.PickupPINRedeemed {
		t.Fatalf("expected pin status REDEEMED, got %s", pinRecord.Status)
	}
	if !tPlus20m.After(pinRecord.ExpiresAt) {
		t.Fatalf("expected tPlus20m to be past original PIN expiry")
	}

	// 3. Independent payment session must STILL be active at t=20m
	activeSess, err := app.db.GetActivePaymentSession(ctx, sessionID, tPlus20m)
	if err != nil || activeSess == nil {
		t.Fatalf("payment session should remain ACTIVE at t=20m despite PIN expiry: %v", err)
	}
	if activeSess.SessionID != sessionID {
		t.Fatalf("session mismatch: expected %s, got %s", sessionID, activeSess.SessionID)
	}

	// 4. Verify payment retries can retrieve this active session without re-entering PIN
	retriedSess, err := app.db.GetActivePaymentSessionByOrder(ctx, orderID, kioskID, tPlus20m)
	if err != nil || retriedSess == nil {
		t.Fatalf("expected order payment retry to retrieve active session at t=20m without PIN re-entry: %v", err)
	}
	if retriedSess.SessionID != sessionID {
		t.Fatalf("retried session mismatch: %s vs %s", retriedSess.SessionID, sessionID)
	}

	// 5. At t=35m (past 30m payment session TTL), session expires
	tPlus35m := startTime.Add(35 * time.Minute)
	expiredSess, err := app.db.GetActivePaymentSession(ctx, sessionID, tPlus35m)
	if err != store.ErrSessionExpired || expiredSess != nil {
		t.Fatalf("expected ErrSessionExpired at t=35m, got sess=%v, err=%v", expiredSess, err)
	}
}

func TestRateLimiter_ForwardedIPTrustAndSpoofing(t *testing.T) {
	// Case A: Untrusted direct peer tries to spoof X-Forwarded-For
	untrustedPeer := &http.Request{
		RemoteAddr: "198.51.100.25:45678",
		Header: http.Header{
			"X-Forwarded-For": []string{"1.2.3.4, 5.6.7.8"},
		},
	}
	// With no trusted proxies configured:
	ip := extractClientIP(untrustedPeer, nil)
	if ip != "198.51.100.25" {
		t.Fatalf("expected extractClientIP to use RemoteAddr %q, got spoofed %q", "198.51.100.25", ip)
	}

	// Case B: With trusted proxy configured, but direct peer is NOT in trusted proxy list
	trustedProxies := []string{"127.0.0.1/32", "10.0.0.0/8"}
	ip = extractClientIP(untrustedPeer, trustedProxies)
	if ip != "198.51.100.25" {
		t.Fatalf("untrusted peer spoofing X-Forwarded-For must be ignored, got %q", ip)
	}

	// Case C: Direct peer IS a trusted proxy (e.g. 10.0.0.5)
	trustedPeer := &http.Request{
		RemoteAddr: "10.0.0.5:12345",
		Header: http.Header{
			"X-Forwarded-For": []string{"203.0.113.88, 10.0.0.5"},
		},
	}
	ip = extractClientIP(trustedPeer, trustedProxies)
	if ip != "203.0.113.88" {
		t.Fatalf("trusted proxy should resolve client IP '203.0.113.88', got %q", ip)
	}
}

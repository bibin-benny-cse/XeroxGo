package main

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"xeroxgo/cloud-backend/internal/pins"
	"xeroxgo/cloud-backend/internal/store"
	"xeroxgo/pkg/contracts"
)

// ensureToolingEnvironment ensures qpdf and gs are present in PATH for the test,
// compiling lightweight test helpers if not installed on the host.
func ensureToolingEnvironment(t *testing.T) {
	t.Helper()
	hasQPDF := false
	hasGS := false
	if _, err := exec.LookPath("qpdf"); err == nil {
		hasQPDF = true
	}
	if _, err := exec.LookPath("gs"); err == nil {
		hasGS = true
	}
	if hasQPDF && hasGS {
		return
	}

	tmpDir := t.TempDir()

	if !hasQPDF {
		srcQPDF := filepath.Join(tmpDir, "mock_qpdf.go")
		binQPDF := filepath.Join(tmpDir, "qpdf.exe")
		codeQPDF := `package main

import (
	"fmt"
	"io"
	"os"
)

func main() {
	args := os.Args[1:]
	if len(args) == 0 {
		os.Exit(0)
	}

	switch args[0] {
	case "--check":
		os.Exit(0)
	case "--is-encrypted":
		// qpdf returns 2 when not encrypted
		os.Exit(2)
	case "--show-npages":
		fmt.Println("1")
		os.Exit(0)
	default:
		// page extraction: qpdf input --pages input range -- output
		if len(args) >= 3 {
			src := args[0]
			dst := args[len(args)-1]
			if src != dst {
				copyFile(src, dst)
			}
		}
		os.Exit(0)
	}
}

func copyFile(src, dst string) {
	in, err := os.Open(src)
	if err != nil {
		return
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return
	}
	defer out.Close()
	_, _ = io.Copy(out, in)
}
`
		if err := os.WriteFile(srcQPDF, []byte(codeQPDF), 0644); err != nil {
			t.Fatalf("failed to write mock qpdf source: %v", err)
		}
		cmd := exec.Command("go", "build", "-o", binQPDF, srcQPDF)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("failed to build test qpdf: %s: %v", string(out), err)
		}
	}

	if !hasGS {
		srcGS := filepath.Join(tmpDir, "mock_gs.go")
		binGS := filepath.Join(tmpDir, "gs.exe")
		codeGS := `package main

import (
	"io"
	"os"
	"strings"
)

func main() {
	var outFile, inFile string
	for _, arg := range os.Args[1:] {
		if strings.HasPrefix(arg, "-sOutputFile=") {
			outFile = strings.TrimPrefix(arg, "-sOutputFile=")
		} else if !strings.HasPrefix(arg, "-") {
			inFile = arg
		}
	}
	if outFile != "" {
		written := false
		if inFile != "" && outFile != inFile {
			if in, err := os.Open(inFile); err == nil {
				if out, err := os.Create(outFile); err == nil {
					_, _ = io.Copy(out, in)
					_ = out.Close()
					written = true
				}
				_ = in.Close()
			}
		}
		if !written {
			_ = os.WriteFile(outFile, []byte("%PDF-1.4\n1 0 obj<</Type/Catalog>>endobj\n%%EOF\n"), 0644)
		}
	}
	os.Exit(0)
}
`
		if err := os.WriteFile(srcGS, []byte(codeGS), 0644); err != nil {
			t.Fatalf("failed to write mock gs source: %v", err)
		}
		cmd := exec.Command("go", "build", "-o", binGS, srcGS)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("failed to build test gs: %s: %v", string(out), err)
		}
	}

	origPath := os.Getenv("PATH")
	t.Setenv("PATH", tmpDir+string(filepath.ListSeparator)+origPath)
}

// TestPortal_UploadAndPINContract verifies that customer upload returns the 4-digit PIN (as string),
// order_secret, expiry timestamps, and strictly avoids leaking internal secrets.
func TestPortal_UploadAndPINContract(t *testing.T) {
	ensureToolingEnvironment(t)

	app, mux, kioskID, _ := setupPINWorkflowServer(t)

	var b bytes.Buffer
	w := multipart.NewWriter(&b)
	part, err := w.CreateFormFile("file", "document.pdf")
	if err != nil {
		t.Fatalf("CreateFormFile error: %v", err)
	}
	if _, err := part.Write(createMinimalPDFBytes()); err != nil {
		t.Fatalf("Write part error: %v", err)
	}
	_ = w.WriteField("copies", "1")
	_ = w.WriteField("color_mode", "BW")
	_ = w.WriteField("duplex", "SIMPLEX")
	w.Close()

	req := httptest.NewRequest(http.MethodPost, "/api/k/"+kioskID+"/upload", &b)
	req.Header.Set("Content-Type", w.FormDataContentType())
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("upload failed: status %d, body: %s", rec.Code, rec.Body.String())
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode upload response: %v", err)
	}

	// 1. Verify order_id
	orderID, ok := resp["order_id"].(string)
	if !ok || orderID == "" {
		t.Fatalf("expected non-empty string order_id, got %v", resp["order_id"])
	}

	// 2. Verify pickup_pin is a 4-digit string (preserves leading zeros)
	pickupPIN, ok := resp["pickup_pin"].(string)
	if !ok {
		t.Fatalf("expected string pickup_pin, got %T (%v)", resp["pickup_pin"], resp["pickup_pin"])
	}
	matched, _ := regexp.MatchString(`^\d{4}$`, pickupPIN)
	if !matched {
		t.Fatalf("expected exactly 4 numeric digits for pickup_pin, got %q", pickupPIN)
	}

	// 3. Verify order_secret
	orderSecret, ok := resp["order_secret"].(string)
	if !ok || len(orderSecret) < 32 {
		t.Fatalf("expected robust order_secret string, got %v", resp["order_secret"])
	}

	// 4. Verify pin_expires_at and pin_expires_in_seconds
	expiresAt, ok := resp["pin_expires_at"].(float64)
	if !ok || int64(expiresAt) <= time.Now().Unix() {
		t.Fatalf("expected future pin_expires_at, got %v", resp["pin_expires_at"])
	}
	expiresInSec, ok := resp["pin_expires_in_seconds"].(float64)
	if !ok || expiresInSec <= 0 || expiresInSec > 905 {
		t.Fatalf("expected pin_expires_in_seconds around 900, got %v", resp["pin_expires_in_seconds"])
	}

	// 5. Verify status is PENDING_PAYMENT
	status, ok := resp["status"].(string)
	if !ok || status != string(contracts.JobStatusPendingPayment) {
		t.Fatalf("expected status %s, got %v", contracts.JobStatusPendingPayment, resp["status"])
	}

	// 6. Verify display_code and sensitive secrets are never in upload response
	if val, exists := resp["display_code"]; exists && val != nil {
		t.Errorf("CRITICAL SECURITY VIOLATION: display_code must be completely removed from upload response, got %v", val)
	}
	if _, exists := resp["hmac_secret"]; exists {
		t.Error("hmac_secret leaked in upload response")
	}
	if _, exists := resp["kiosk_secret"]; exists {
		t.Error("kiosk_secret leaked in upload response")
	}
	if _, exists := resp["pin_hmac"]; exists {
		t.Error("pin_hmac leaked in upload response")
	}

	// 7. Verify database compatibility column stores only non-sensitive placeholder
	dbOrder, err := app.db.GetOrder(context.Background(), orderID)
	if err != nil {
		t.Fatalf("failed to retrieve stored order from database: %v", err)
	}
	if dbOrder.Command.DisplayCode != "PIN-PROTECTED" {
		t.Errorf("expected database display_code compatibility placeholder 'PIN-PROTECTED', got %q", dbOrder.Command.DisplayCode)
	}
}

// TestPortal_Upload_MalformedAndErrorResponses tests edge cases and error responses
// during file upload to verify the client-server error contract.
func TestPortal_Upload_MalformedAndErrorResponses(t *testing.T) {
	ensureToolingEnvironment(t)
	_, mux, kioskID, _ := setupPINWorkflowServer(t)

	// 1. Missing file in multipart form
	var b1 bytes.Buffer
	w1 := multipart.NewWriter(&b1)
	_ = w1.WriteField("copies", "1")
	w1.Close()

	req1 := httptest.NewRequest(http.MethodPost, "/api/k/"+kioskID+"/upload", &b1)
	req1.Header.Set("Content-Type", w1.FormDataContentType())
	rec1 := httptest.NewRecorder()
	mux.ServeHTTP(rec1, req1)

	if rec1.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request for missing file, got %d: %s", rec1.Code, rec1.Body.String())
	}

	// 2. Invalid duplex mode
	var b2 bytes.Buffer
	w2 := multipart.NewWriter(&b2)
	part2, _ := w2.CreateFormFile("file", "document.pdf")
	_, _ = part2.Write(createMinimalPDFBytes())
	_ = w2.WriteField("copies", "1")
	_ = w2.WriteField("duplex", "INVALID_DUPLEX_MODE")
	w2.Close()

	req2 := httptest.NewRequest(http.MethodPost, "/api/k/"+kioskID+"/upload", &b2)
	req2.Header.Set("Content-Type", w2.FormDataContentType())
	rec2 := httptest.NewRecorder()
	mux.ServeHTTP(rec2, req2)

	if rec2.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request for invalid duplex mode, got %d: %s", rec2.Code, rec2.Body.String())
	}

	// 3. Upload to unknown kiosk
	var b3 bytes.Buffer
	w3 := multipart.NewWriter(&b3)
	part3, _ := w3.CreateFormFile("file", "document.pdf")
	_, _ = part3.Write(createMinimalPDFBytes())
	w3.Close()

	req3 := httptest.NewRequest(http.MethodPost, "/api/k/unknown-kiosk-999/upload", &b3)
	req3.Header.Set("Content-Type", w3.FormDataContentType())
	rec3 := httptest.NewRecorder()
	mux.ServeHTTP(rec3, req3)

	if rec3.Code != http.StatusBadRequest && rec3.Code != http.StatusNotFound {
		t.Fatalf("expected 400 or 404 for unknown kiosk, got %d: %s", rec3.Code, rec3.Body.String())
	}
}

// TestPortal_PINRefresh_MalformedAndMissingRequests tests edge cases and error contracts
// for the PIN refresh endpoint.
func TestPortal_PINRefresh_MalformedAndMissingRequests(t *testing.T) {
	app, mux, kioskID, _ := setupPINWorkflowServer(t)
	orderID, _, _ := createTestOrderDirect(t, app, kioskID)

	// 1. Non-existent order ID: returns 401 (or 404) to avoid order enumeration
	badOrderReq := httptest.NewRequest(http.MethodPost, "/api/orders/ord_nonexistent12345/pin/refresh", strings.NewReader(`{"order_secret":"12345678901234567890123456789012"}`))
	badOrderReq.Header.Set("Content-Type", "application/json")
	rec1 := httptest.NewRecorder()
	mux.ServeHTTP(rec1, badOrderReq)

	if rec1.Code != http.StatusNotFound && rec1.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 or 404 for non-existent order, got %d: %s", rec1.Code, rec1.Body.String())
	}

	// 2. Corrupt/invalid JSON body
	corruptReq := httptest.NewRequest(http.MethodPost, "/api/orders/"+orderID+"/pin/refresh", strings.NewReader(`{not valid json`))
	corruptReq.Header.Set("Content-Type", "application/json")
	rec2 := httptest.NewRecorder()
	mux.ServeHTTP(rec2, corruptReq)

	if rec2.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request for corrupt JSON, got %d: %s", rec2.Code, rec2.Body.String())
	}

	// 3. GET method instead of POST
	getReq := httptest.NewRequest(http.MethodGet, "/api/orders/"+orderID+"/pin/refresh", nil)
	rec3 := httptest.NewRecorder()
	mux.ServeHTTP(rec3, getReq)

	if rec3.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405 Method Not Allowed for GET /pin/refresh, got %d: %s", rec3.Code, rec3.Body.String())
	}
}

// TestPortal_PINRefresh_ValidOwnershipSecret tests refreshing the pickup PIN
// when the customer provides their valid order_secret.
func TestPortal_PINRefresh_ValidOwnershipSecret(t *testing.T) {
	app, mux, kioskID, _ := setupPINWorkflowServer(t)
	orderID, rawSecret, oldPIN := createTestOrderDirect(t, app, kioskID)

	refreshBody, _ := json.Marshal(map[string]string{
		"order_secret": rawSecret,
	})
	req := httptest.NewRequest(http.MethodPost, "/api/orders/"+orderID+"/pin/refresh", bytes.NewReader(refreshBody))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK on pin refresh, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode refresh response: %v", err)
	}

	newPIN, ok := resp["pickup_pin"].(string)
	if !ok {
		t.Fatalf("expected string pickup_pin in refresh response, got %T (%v)", resp["pickup_pin"], resp["pickup_pin"])
	}
	matched, _ := regexp.MatchString(`^\d{4}$`, newPIN)
	if !matched {
		t.Fatalf("expected 4-digit new PIN, got %q", newPIN)
	}

	// The refreshed PIN may be different or renewed
	t.Logf("Refreshed PIN: old=%s, new=%s", oldPIN, newPIN)

	expiresAt, ok := resp["pin_expires_at"].(float64)
	if !ok || int64(expiresAt) <= time.Now().Unix() {
		t.Fatalf("expected future pin_expires_at, got %v", resp["pin_expires_at"])
	}
}

// TestPortal_PINRefresh_InvalidOwnershipSecret_Rejected tests rejection when
// wrong or missing order_secret is provided.
func TestPortal_PINRefresh_InvalidOwnershipSecret_Rejected(t *testing.T) {
	app, mux, kioskID, _ := setupPINWorkflowServer(t)
	orderID, _, _ := createTestOrderDirect(t, app, kioskID)

	// Subtest 1: Invalid secret
	badBody, _ := json.Marshal(map[string]string{
		"order_secret": "invalid_wrong_secret_12345",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/orders/"+orderID+"/pin/refresh", bytes.NewReader(badBody))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 Unauthorized for invalid secret, got %d: %s", rec.Code, rec.Body.String())
	}

	// Subtest 2: Empty secret
	emptyBody, _ := json.Marshal(map[string]string{
		"order_secret": "",
	})
	reqEmpty := httptest.NewRequest(http.MethodPost, "/api/orders/"+orderID+"/pin/refresh", bytes.NewReader(emptyBody))
	reqEmpty.Header.Set("Content-Type", "application/json")
	recEmpty := httptest.NewRecorder()
	mux.ServeHTTP(recEmpty, reqEmpty)

	if recEmpty.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request for empty secret, got %d: %s", recEmpty.Code, recEmpty.Body.String())
	}
}

// TestPortal_PINRefresh_PaidOrder_Rejected confirms that an order that has already
// been paid cannot refresh its PIN.
func TestPortal_PINRefresh_PaidOrder_Rejected(t *testing.T) {
	app, mux, kioskID, _ := setupPINWorkflowServer(t)
	orderID, rawSecret, _ := createTestOrderDirect(t, app, kioskID)

	// Mark order as PAID_WAITING
	ctx := context.Background()
	err := app.db.UpdateOrderStatus(ctx, orderID, contracts.JobStatusPaidWaiting)
	if err != nil {
		t.Fatalf("failed to update order status to paid: %v", err)
	}

	refreshBody, _ := json.Marshal(map[string]string{
		"order_secret": rawSecret,
	})
	req := httptest.NewRequest(http.MethodPost, "/api/orders/"+orderID+"/pin/refresh", bytes.NewReader(refreshBody))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request when refreshing PIN for paid order, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "not eligible") {
		t.Fatalf("expected 'not eligible' in error response, got %s", rec.Body.String())
	}
}

// TestPortal_PublicOrderStatus_NoSensitiveDataLeak verifies the public order status
// endpoint (/api/orders/{id}/status) does not leak pickup_pin, HMAC secrets, or hashes.
func TestPortal_PublicOrderStatus_NoSensitiveDataLeak(t *testing.T) {
	app, mux, kioskID, _ := setupPINWorkflowServer(t)
	orderID, _, pin := createTestOrderDirect(t, app, kioskID)

	req := httptest.NewRequest(http.MethodGet, "/api/orders/"+orderID+"/status", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for order status, got %d: %s", rec.Code, rec.Body.String())
	}

	// Verify plaintext PIN is never exposed in the response body
	if strings.Contains(rec.Body.String(), pin) {
		t.Fatalf("CRITICAL SECURITY VIOLATION: public order status exposes raw PIN %q: %s", pin, rec.Body.String())
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode status response: %v", err)
	}

	// Verify required public tracking fields
	if resp["order_id"] != orderID {
		t.Errorf("expected order_id %q, got %v", orderID, resp["order_id"])
	}
	if resp["status"] != string(contracts.JobStatusPendingPayment) {
		t.Errorf("expected status %s, got %v", contracts.JobStatusPendingPayment, resp["status"])
	}

	// Verify display_code is completely absent from public status response
	if val, exists := resp["display_code"]; exists && val != nil {
		t.Errorf("CRITICAL SECURITY VIOLATION: display_code must be completely absent from public order status, got %v", val)
	}

	// Explicit check: pickup_pin must NOT be in public status
	if _, exists := resp["pickup_pin"]; exists {
		t.Errorf("CRITICAL: pickup_pin field leaked in public status response: %v", resp["pickup_pin"])
	}

	// Explicit check: order_secret or hash must NOT be in public status
	if _, exists := resp["order_secret"]; exists {
		t.Errorf("CRITICAL: order_secret leaked in public status response: %v", resp["order_secret"])
	}
	if _, exists := resp["order_secret_hash"]; exists {
		t.Errorf("CRITICAL: order_secret_hash leaked in public status response: %v", resp["order_secret_hash"])
	}
	if _, exists := resp["pin_hmac"]; exists {
		t.Errorf("CRITICAL: pin_hmac leaked in public status response: %v", resp["pin_hmac"])
	}
}

// TestPortal_HTMLTemplate_MarkupAndClasses verifies that portalHTML includes
// all necessary Stage 5 customer portal elements and removes obsolete buttons.
func TestPortal_HTMLTemplate_MarkupAndClasses(t *testing.T) {
	app, _, _, _ := setupPINWorkflowServer(t)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/k/kiosk-demo", nil)
	app.handlePortal(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK from handlePortal, got %d", rec.Code)
	}

	html := rec.Body.String()

	// 1. Verify key Stage 5 elements exist
	stage5IDs := []string{
		"status-order-id",
		"status-kiosk-name",
		"status-pickup-pin",
		"pin-timer-badge",
		"pin-countdown-text",
		"btn-refresh-pin",
		"pin-refresh-error",
		"status-badge",
		"status-progress-fill",
	}

	for _, id := range stage5IDs {
		if !strings.Contains(html, `id="`+id+`"`) {
			t.Errorf("expected HTML to contain element id=%q", id)
		}
	}

	// 2. Verify payment clarification text is in HTML
	if !strings.Contains(html, "Pay with UPI directly on the physical kiosk screen") {
		t.Errorf("expected HTML to clarify physical kiosk UPI payment")
	}

	// 3. Verify dev-simulate-payment-btn is NOT present
	if strings.Contains(html, "dev-simulate-payment-btn") {
		t.Errorf("dev-simulate-payment-btn should be removed from portalHTML in Stage 5")
	}
}

// TestPortal_LeadingZeroPINPreservation verifies that PINs with leading zeros (e.g. "0482")
// are strictly preserved as 4-character strings throughout serialization.
func TestPortal_LeadingZeroPINPreservation(t *testing.T) {
	app, _, kioskID, _ := setupPINWorkflowServer(t)

	// Create order with leading zero PIN directly
	ctx := context.Background()
	now := time.Now().UTC()
	leadingZeroPIN := "0482"
	orderHex, _ := randomHex(12)
	orderID := "ord_" + orderHex
	_, secretHash, _ := pins.GenerateOrderOwnershipSecret()

	pinHMAC, err := pins.ComputePINHMAC(app.cfg.pickupPINHMACSecret, kioskID, leadingZeroPIN)
	if err != nil {
		t.Fatalf("failed to compute pin HMAC: %v", err)
	}
	expiresAt := now.Add(15 * time.Minute)

	order := store.Order{
		OrderID: orderID,
		Command: contracts.PrintJobCommand{
			JobID:          "job_" + orderHex,
			KioskID:        kioskID,
			DisplayCode:    "PIN-PROTECTED",
			DownloadURL:    "http://test/download",
			SHA256Checksum: "dummy_checksum",
			TotalPages:     1,
			TotalSheets:    1,
			Copies:         1,
			Duplex:         contracts.DuplexSimplex,
			StartFromPage:  1,
		},
		Status:          contracts.JobStatusPendingPayment,
		PricePaise:      200,
		ColorMode:       "BW",
		OrderSecretHash: secretHash,
		CreatedAt:       now,
		UpdatedAt:       now,
	}

	if err := app.db.CreateOrder(ctx, order); err != nil {
		t.Fatalf("failed to create order: %v", err)
	}

	pinHex, _ := randomHex(12)
	pinRecord := store.PickupPIN{
		ID:        "pin_" + pinHex,
		OrderID:   orderID,
		KioskID:   kioskID,
		PINHMAC:   pinHMAC,
		Status:    store.PickupPINActive,
		CreatedAt: now,
		ExpiresAt: expiresAt,
	}

	if err := app.db.CreatePickupPIN(ctx, pinRecord, app.cfg.maxActivePINsPerKiosk); err != nil {
		t.Fatalf("failed to create order with leading-zero PIN: %v", err)
	}

	// Verify PIN refresh response contract preserves string type and leading zeros
	refreshResp := pins.PINRefreshResponse{
		OrderID:             orderID,
		PickupPIN:           leadingZeroPIN,
		PINExpiresAt:        expiresAt.Unix(),
		PINExpiresInSeconds: 900,
		Status:              string(contracts.JobStatusPendingPayment),
	}

	encoded, err := json.Marshal(refreshResp)
	if err != nil {
		t.Fatalf("failed to marshal refresh response: %v", err)
	}

	// Verify JSON text contains literal quotes around "0482"
	if !strings.Contains(string(encoded), `"pickup_pin":"0482"`) {
		t.Fatalf("expected literal quoted string \"pickup_pin\":\"0482\", got: %s", string(encoded))
	}

	var decoded map[string]interface{}
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("failed to unmarshal JSON: %v", err)
	}

	pinVal, ok := decoded["pickup_pin"].(string)
	if !ok {
		t.Fatalf("pickup_pin decoded as %T instead of string", decoded["pickup_pin"])
	}
	if pinVal != "0482" {
		t.Fatalf("expected pickup_pin to be %q, got %q", "0482", pinVal)
	}
	if len(pinVal) != 4 {
		t.Fatalf("expected exactly 4 characters, got %d", len(pinVal))
	}
}

// TestPortal_DualSourceFileSynchronization verifies that frontend/ files match
// backend static/ copies byte-for-byte.
func TestPortal_DualSourceFileSynchronization(t *testing.T) {
	// 1. Verify CSS files match
	frontCSS, err := os.ReadFile(filepath.Join("..", "..", "..", "frontend", "css", "portal.css"))
	if err != nil {
		t.Fatalf("failed to read frontend/css/portal.css: %v", err)
	}
	staticCSS, err := os.ReadFile(filepath.Join("static", "css", "portal.css"))
	if err != nil {
		t.Fatalf("failed to read static/css/portal.css: %v", err)
	}
	if !bytes.Equal(frontCSS, staticCSS) {
		t.Error("frontend/css/portal.css and static/css/portal.css are not identical!")
	}

	// 2. Verify JS files match
	frontJS, err := os.ReadFile(filepath.Join("..", "..", "..", "frontend", "js", "portal.js"))
	if err != nil {
		t.Fatalf("failed to read frontend/js/portal.js: %v", err)
	}
	staticJS, err := os.ReadFile(filepath.Join("static", "js", "portal.js"))
	if err != nil {
		t.Fatalf("failed to read static/js/portal.js: %v", err)
	}
	if !bytes.Equal(frontJS, staticJS) {
		t.Error("frontend/js/portal.js and static/js/portal.js are not identical!")
	}

	// 3. Verify Stage 5 classes exist in CSS
	cssStr := string(frontCSS)
	stage5Classes := []string{
		".pin-card-wrapper",
		".pin-display-large",
		".pin-timer-badge",
		".pin-timer-badge.expiring-soon",
		".pin-timer-badge.expired",
		".btn-refresh-pin",
		".order-meta-info",
	}
	for _, cls := range stage5Classes {
		if !strings.Contains(cssStr, cls) {
			t.Errorf("expected CSS to define class %q", cls)
		}
	}

	// 4. Verify Stage 5 functions/handlers exist in JS
	jsStr := string(frontJS)
	stage5Keywords := []string{
		"xeroxgo_secret_",
		"startPinCountdown",
		"stopPinCountdown",
		"btn-refresh-pin",
		"pin-refresh-error",
		"/api/orders/",
		"/pin/refresh",
		"Pending Payment at Kiosk",
	}
	for _, kw := range stage5Keywords {
		if !strings.Contains(jsStr, kw) {
			t.Errorf("expected JS to contain keyword %q", kw)
		}
	}
}

// TestPortal_BrowserRenderingAndResponsiveContract verifies that HTML and CSS
// conform to desktop and mobile layout rules and omit any client-side payment modals.
func TestPortal_BrowserRenderingAndResponsiveContract(t *testing.T) {
	app, _, _, _ := setupPINWorkflowServer(t)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/k/kiosk-demo", nil)
	app.handlePortal(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", rec.Code)
	}

	html := rec.Body.String()

	// 1. Mobile viewport meta tag
	if !strings.Contains(html, `<meta name="viewport" content="width=device-width, initial-scale=1.0`) {
		t.Error("missing responsive viewport meta tag")
	}

	// 2. Responsive app container
	if !strings.Contains(html, `class="app-container"`) {
		t.Error("missing .app-container root element")
	}

	// 3. Section 3 action button reads "Generate Pickup PIN"
	if !strings.Contains(html, "Generate Pickup PIN") {
		t.Error("Section 3 button text should be 'Generate Pickup PIN'")
	}

	// 4. Pickup PIN card layout
	if !strings.Contains(html, `class="pin-display-large"`) {
		t.Error("missing .pin-display-large element for mobile PIN readability")
	}
	if !strings.Contains(html, `id="status-pickup-pin"`) {
		t.Error("missing #status-pickup-pin element")
	}
	if !strings.Contains(html, `id="pin-timer-badge"`) {
		t.Error("missing #pin-timer-badge element")
	}
	if !strings.Contains(html, `id="btn-refresh-pin"`) {
		t.Error("missing #btn-refresh-pin element")
	}

	// 5. Razorpay web modal script strictly absent
	if strings.Contains(html, "checkout.razorpay.com") {
		t.Error("checkout.razorpay.com script tag should NOT be in customer portal")
	}

	// 6. Dev mode simulate button strictly absent
	if strings.Contains(html, "dev-simulate-payment-btn") {
		t.Error("dev-simulate-payment-btn should NOT be in customer portal")
	}
}

package spooler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"xeroxgo/kiosk-daemon/internal/state"
	"xeroxgo/kiosk-daemon/internal/ui_ipc"
	"xeroxgo/pkg/contracts"
)

// roundTripFunc implements http.RoundTripper for pure in-memory HTTP mocking without TCP sockets.
type roundTripFunc func(req *http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func newMockHTTPClient(handler func(req *http.Request) (*http.Response, error)) *http.Client {
	return &http.Client{
		Transport: roundTripFunc(handler),
		Timeout:   5 * time.Second,
	}
}

func jsonResponse(statusCode int, body interface{}) *http.Response {
	data, _ := json.Marshal(body)
	return &http.Response{
		StatusCode: statusCode,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader(data)),
	}
}

// Test 1: PIN redemption preserves leading zeros and attaches kiosk authentication headers.
func TestWorkflow_RedeemPIN_PreservesLeadingZerosAndSendsHeaders(t *testing.T) {
	const expectedKioskID = "kiosk-test-01"
	const expectedSecret = "sec_test_random_secret_12345"
	const expectedPIN = "0482"

	called := false
	client := newMockHTTPClient(func(req *http.Request) (*http.Response, error) {
		called = true
		if req.URL.Path != "/api/kiosk/pin/redeem" {
			t.Fatalf("unexpected URL path: %s", req.URL.Path)
		}
		if req.Header.Get("X-Kiosk-ID") != expectedKioskID {
			t.Fatalf("unexpected X-Kiosk-ID: got %q, want %q", req.Header.Get("X-Kiosk-ID"), expectedKioskID)
		}
		if req.Header.Get("X-Kiosk-Secret") != expectedSecret {
			t.Fatalf("unexpected X-Kiosk-Secret: got %q, want %q", req.Header.Get("X-Kiosk-Secret"), expectedSecret)
		}

		var body map[string]string
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Fatalf("failed to decode request body: %v", err)
		}
		if body["pin"] != expectedPIN {
			t.Fatalf("leading zero lost in PIN: got %q, want %q", body["pin"], expectedPIN)
		}

		return jsonResponse(http.StatusOK, PINRedeemResponse{
			Status:           "ready_for_payment",
			SessionID:        "sess_test_123456",
			OrderID:          "ord_test_987654",
			KioskID:          expectedKioskID,
			PricePaise:       1000,
			TotalPages:       5,
			TotalSheets:      5,
			Copies:           1,
			Duplex:           "SIMPLEX",
			ColorMode:        "BW",
			SessionExpiresAt: time.Now().Add(30 * time.Minute).Unix(),
		}), nil
	})

	wf, err := NewPaymentWorkflow("http://mock-backend", expectedKioskID, expectedSecret, client, 100*time.Millisecond)
	if err != nil {
		t.Fatalf("NewPaymentWorkflow failed: %v", err)
	}

	resp, err := wf.RedeemPIN(context.Background(), expectedPIN)
	if err != nil {
		t.Fatalf("RedeemPIN failed: %v", err)
	}
	if !called {
		t.Fatalf("mock HTTP handler was not called")
	}
	if resp.SessionID != "sess_test_123456" {
		t.Fatalf("unexpected session ID: %s", resp.SessionID)
	}
	if resp.PricePaise != 1000 {
		t.Fatalf("unexpected price paise: %d", resp.PricePaise)
	}
}

// Test 2: PIN redemption rejects malformed inputs and handles rate limiting (429).
func TestWorkflow_RedeemPIN_ValidationAndErrors(t *testing.T) {
	wf, _ := NewPaymentWorkflow("http://mock-backend", "kiosk-01", "sec-01", http.DefaultClient, 100*time.Millisecond)

	// Local validation
	invalidPINs := []string{"", "12", "123", "12345", "04a2", "abcd"}
	for _, p := range invalidPINs {
		if _, err := wf.RedeemPIN(context.Background(), p); err == nil {
			t.Fatalf("expected error for invalid PIN %q, got nil", p)
		}
	}

	// Backend rate limiting (429)
	client429 := newMockHTTPClient(func(req *http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusTooManyRequests, map[string]interface{}{
			"error":               "too many failed attempts, progressive cooldown active",
			"retry_after_seconds": 60,
		}), nil
	})
	wf429, _ := NewPaymentWorkflow("http://mock-backend", "kiosk-01", "sec-01", client429, 100*time.Millisecond)
	_, err := wf429.RedeemPIN(context.Background(), "0482")
	if err == nil || !strings.Contains(err.Error(), "rate limited") {
		t.Fatalf("expected rate limited error, got: %v", err)
	}
}

// Test 3: InitiatePaymentQR creates or retrieves UPI QR and handles auth headers.
func TestWorkflow_InitiatePaymentQR_SuccessAndError(t *testing.T) {
	const expectedSession = "sess_abc_xyz"

	client := newMockHTTPClient(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path != "/api/kiosk/payment/initiate-qr" {
			t.Fatalf("unexpected path: %s", req.URL.Path)
		}
		var body map[string]string
		_ = json.NewDecoder(req.Body).Decode(&body)
		if body["session_id"] != expectedSession {
			t.Fatalf("unexpected session_id: got %q, want %q", body["session_id"], expectedSession)
		}

		return jsonResponse(http.StatusOK, PaymentQRResponse{
			AttemptID:    "att_test_1",
			SessionID:    expectedSession,
			OrderID:      "ord_test_1",
			RazorpayQRID: "qr_test_1",
			QRImageURL:   "https://api.razorpay.com/v1/payments/qr_codes/qr_test_1/image",
			AmountPaise:  1500,
			Currency:     "INR",
			Status:       "active",
			ExpiresAt:    time.Now().Add(15 * time.Minute).Unix(),
		}), nil
	})

	wf, _ := NewPaymentWorkflow("http://mock-backend", "kiosk-01", "sec-01", client, 100*time.Millisecond)

	// Empty session
	if _, err := wf.InitiatePaymentQR(context.Background(), ""); err == nil {
		t.Fatalf("expected error on empty session_id")
	}

	qrResp, err := wf.InitiatePaymentQR(context.Background(), expectedSession)
	if err != nil {
		t.Fatalf("InitiatePaymentQR failed: %v", err)
	}
	if qrResp.QRImageURL == "" || qrResp.AmountPaise != 1500 {
		t.Fatalf("unexpected qr response: %+v", qrResp)
	}
}

// Test 4: FetchQRImage downloads and decodes real image bytes.
func TestWorkflow_FetchQRImage(t *testing.T) {
	// Create sample test PNG image in memory
	testImg := image.NewRGBA(image.Rect(0, 0, 100, 100))
	for y := 0; y < 100; y++ {
		for x := 0; x < 100; x++ {
			testImg.Set(x, y, color.Black)
		}
	}
	var imgBuf bytes.Buffer
	if err := png.Encode(&imgBuf, testImg); err != nil {
		t.Fatalf("png.Encode failed: %v", err)
	}

	client := newMockHTTPClient(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"image/png"}},
			Body:       io.NopCloser(bytes.NewReader(imgBuf.Bytes())),
		}, nil
	})

	wf, _ := NewPaymentWorkflow("http://mock-backend", "kiosk-01", "sec-01", client, 100*time.Millisecond)
	img, err := wf.FetchQRImage(context.Background(), "https://example.invalid/qr.png")
	if err != nil {
		t.Fatalf("FetchQRImage failed: %v", err)
	}
	if img == nil || img.Bounds().Dx() != 100 {
		t.Fatalf("unexpected image bounds: %v", img.Bounds())
	}
}

// Test 5: PollPaymentStatus loops until payment is confirmed (is_paid == true).
func TestWorkflow_PollPaymentStatus_Success(t *testing.T) {
	var callCount int32

	client := newMockHTTPClient(func(req *http.Request) (*http.Response, error) {
		count := atomic.AddInt32(&callCount, 1)
		if count < 3 {
			// First two polls: not yet paid
			return jsonResponse(http.StatusOK, PaymentStatusResponse{
				SessionID:     "sess_poll_1",
				OrderID:       "ord_poll_1",
				OrderStatus:   "PENDING_PAYMENT",
				SessionStatus: "active",
				IsPaid:        false,
			}), nil
		}
		// Third poll: paid verified!
		return jsonResponse(http.StatusOK, PaymentStatusResponse{
			SessionID:     "sess_poll_1",
			OrderID:       "ord_poll_1",
			OrderStatus:   "PAID_WAITING",
			SessionStatus: "active",
			IsPaid:        true,
		}), nil
	})

	wf, _ := NewPaymentWorkflow("http://mock-backend", "kiosk-01", "sec-01", client, 10*time.Millisecond)

	updates := 0
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	status, err := wf.PollPaymentStatus(ctx, "sess_poll_1", func(st *PaymentStatusResponse) {
		updates++
	})
	if err != nil {
		t.Fatalf("PollPaymentStatus failed: %v", err)
	}
	if !status.IsPaid {
		t.Fatalf("expected IsPaid to be true, got false")
	}
	if updates < 3 {
		t.Fatalf("expected at least 3 poll updates, got %d", updates)
	}
}

// Test 6: PollPaymentStatus terminates immediately on session expiration or failure.
func TestWorkflow_PollPaymentStatus_Expired(t *testing.T) {
	client := newMockHTTPClient(func(req *http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusOK, PaymentStatusResponse{
			SessionID:     "sess_poll_exp",
			OrderID:       "ord_poll_exp",
			OrderStatus:   "PENDING_PAYMENT",
			SessionStatus: "expired",
			IsPaid:        false,
		}), nil
	})

	wf, _ := NewPaymentWorkflow("http://mock-backend", "kiosk-01", "sec-01", client, 10*time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	_, err := wf.PollPaymentStatus(ctx, "sess_poll_exp", nil)
	if err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expected session expired error, got: %v", err)
	}
}

// Test 7: Payment retry reuses existing session_id without re-asking for PIN.
func TestWorkflow_PaymentRetry_ReusesSessionWithoutPIN(t *testing.T) {
	const sessionID = "sess_retry_101"
	var qrAttempts int32

	client := newMockHTTPClient(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == "/api/kiosk/payment/initiate-qr" {
			att := atomic.AddInt32(&qrAttempts, 1)
			return jsonResponse(http.StatusOK, PaymentQRResponse{
				AttemptID:    fmt.Sprintf("att_retry_%d", att),
				SessionID:    sessionID,
				OrderID:      "ord_retry_1",
				RazorpayQRID: fmt.Sprintf("qr_retry_%d", att),
				QRImageURL:   fmt.Sprintf("https://api.razorpay.com/qr_%d.png", att),
				AmountPaise:  1200,
				Currency:     "INR",
				Status:       "active",
				ExpiresAt:    time.Now().Add(15 * time.Minute).Unix(),
			}), nil
		}
		return jsonResponse(http.StatusNotFound, map[string]string{"error": "not found"}), nil
	})

	wf, _ := NewPaymentWorkflow("http://mock-backend", "kiosk-01", "sec-01", client, 100*time.Millisecond)

	// First attempt
	resp1, err := wf.InitiatePaymentQR(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("first attempt failed: %v", err)
	}

	// Retry using SAME session ID (no PIN needed!)
	resp2, err := wf.InitiatePaymentQR(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("retry attempt failed: %v", err)
	}

	if resp1.SessionID != resp2.SessionID {
		t.Fatalf("retry did not reuse session ID: got %s vs %s", resp1.SessionID, resp2.SessionID)
	}
	if qrAttempts != 2 {
		t.Fatalf("expected 2 QR calls, got %d", qrAttempts)
	}
}

// Test 8: Duplicate print prevention across daemon restarts and repeated job dispatches.
func TestSpooler_DuplicatePrintingPrevention(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "kiosk_state.db")
	st, err := state.Open(dbPath)
	if err != nil {
		t.Fatalf("state.Open failed: %v", err)
	}
	defer st.Close()

	ctx := context.Background()
	cmd := contracts.PrintJobCommand{
		JobID:          "job-duplicate-test",
		KioskID:        "kiosk-01",
		DisplayCode:    "0482",
		DownloadURL:    "http://example.invalid/job.pdf",
		SHA256Checksum: strings.Repeat("a", 64),
		TotalPages:     3,
		TotalSheets:    3,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		StartFromPage:  1,
	}

	// 1. Initial job completed in state store
	if err := st.SaveJob(ctx, cmd, contracts.JobStatusSpooling); err != nil {
		t.Fatalf("SaveJob failed: %v", err)
	}
	if err := st.UpdateProgress(ctx, contracts.JobProgressEvent{
		JobID:         cmd.JobID,
		KioskID:       cmd.KioskID,
		Status:        contracts.JobStatusCompleted,
		PagesPrinted:  cmd.TotalPages,
		SheetsPrinted: cmd.TotalSheets,
		Timestamp:     time.Now().UTC(),
	}); err != nil {
		t.Fatalf("UpdateProgress completed failed: %v", err)
	}

	// 2. Initialize Spooler Client
	client, err := NewClient(ClientConfig{
		CloudURL:      "ws://127.0.0.1:8080/ws",
		KioskID:       "kiosk-01",
		KioskSecret:   "sec-01",
		StateStore:    st,
		PrinterAddr:   "127.0.0.1:9100",
		DownloadDir:   tmpDir,
		IdleQRContent: "https://xeroxgo-frontend.vercel.app",
		ScreenSink:    func(*image.RGBA) error { return nil },
		HTTPClient:    http.DefaultClient,
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	// 3. Dispatch the same job again
	client.handleIncomingJob(cmd)

	// Verify job was NOT queued for re-printing
	select {
	case queuedCmd := <-client.jobQueue:
		t.Fatalf("duplicate job was unexpectedly queued for printing: %v", queuedCmd.JobID)
	default:
		// Passed! Job was correctly skipped
	}
}

// Test 9: Complete Kiosk Workflow Session executes states and enforces verified payment.
func TestWorkflow_KioskSession_VerifiedPrintingEnforced(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "kiosk_session.db")
	st, err := state.Open(dbPath)
	if err != nil {
		t.Fatalf("state.Open failed: %v", err)
	}
	defer st.Close()

	var statesRendered []ui_ipc.ScreenMode
	sink := func(img *image.RGBA) error {
		return nil
	}

	client, err := NewClient(ClientConfig{
		CloudURL:      "ws://127.0.0.1:8080/ws",
		KioskID:       "kiosk-01",
		KioskSecret:   "sec-01",
		StateStore:    st,
		PrinterAddr:   "127.0.0.1:9100",
		DownloadDir:   tmpDir,
		IdleQRContent: "https://xeroxgo-frontend.vercel.app",
		ScreenSink:    sink,
		HTTPClient: newMockHTTPClient(func(req *http.Request) (*http.Response, error) {
			switch req.URL.Path {
			case "/api/kiosk/pin/redeem":
				return jsonResponse(http.StatusOK, PINRedeemResponse{
					Status:           "ready_for_payment",
					SessionID:        "sess_wf_1",
					OrderID:          "ord_wf_1",
					KioskID:          "kiosk-01",
					PricePaise:       800,
					TotalPages:       4,
					TotalSheets:      4,
					Copies:           1,
					Duplex:           "SIMPLEX",
					ColorMode:        "BW",
					SessionExpiresAt: time.Now().Add(30 * time.Minute).Unix(),
				}), nil
			case "/api/kiosk/payment/initiate-qr":
				return jsonResponse(http.StatusOK, PaymentQRResponse{
					AttemptID:    "att_wf_1",
					SessionID:    "sess_wf_1",
					OrderID:      "ord_wf_1",
					RazorpayQRID: "qr_wf_1",
					QRImageURL:   "upi://pay?pa=xeroxgo@icici&am=8.00",
					AmountPaise:  800,
					Currency:     "INR",
					Status:       "active",
					ExpiresAt:    time.Now().Add(15 * time.Minute).Unix(),
				}), nil
			case "/api/kiosk/payment/status":
				return jsonResponse(http.StatusOK, PaymentStatusResponse{
					SessionID:     "sess_wf_1",
					OrderID:       "ord_wf_1",
					OrderStatus:   "PAID_WAITING",
					SessionStatus: "active",
					IsPaid:        true,
				}), nil
			default:
				return jsonResponse(http.StatusNotFound, map[string]string{"error": "not found"}), nil
			}
		}),
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	session := NewKioskWorkflowSession(client)
	err = session.ExecuteWorkflow(context.Background(), "0482")
	if err != nil {
		t.Fatalf("ExecuteWorkflow failed: %v", err)
	}

	history := client.GetRenderHistory()
	for _, h := range history {
		statesRendered = append(statesRendered, h.Mode)
	}

	// Verify states were traversed
	hasPIN := false
	hasQR := false
	for _, m := range statesRendered {
		if m == ui_ipc.ModePINEntry {
			hasPIN = true
		}
		if m == ui_ipc.ModePaymentQR {
			hasQR = true
		}
	}
	if !hasPIN {
		t.Fatalf("expected ModePINEntry in history, got %v", statesRendered)
	}
	if !hasQR {
		t.Fatalf("expected ModePaymentQR in history, got %v", statesRendered)
	}
}

// Test 10: Screen rendering produces valid non-nil RGBA images for all 4 workflow modes.
func TestWorkflow_ScreenRenderingAllModes(t *testing.T) {
	modes := []ui_ipc.ScreenState{
		{
			Mode:    ui_ipc.ModePINEntry,
			PIN:     "0482",
			Message: "Enter 4-digit PIN",
		},
		{
			Mode:             ui_ipc.ModePaymentQR,
			OrderID:          "ord_render_1",
			AmountPaise:      1000,
			PaymentQRURL:     "upi://pay?pa=test@upi&am=10.00",
			PaymentExpiresAt: time.Now().Add(10 * time.Minute),
		},
		{
			Mode:          ui_ipc.ModeWaitingForPayment,
			OrderID:       "ord_render_1",
			AmountPaise:   1000,
			PaymentStatus: "Processing payment...",
		},
		{
			Mode:            ui_ipc.ModePaymentFailed,
			OrderID:         "ord_render_1",
			AmountPaise:     1000,
			PaymentError:    "UPI transfer was declined by customer bank",
			CanRetryPayment: true,
		},
	}

	for _, state := range modes {
		t.Run(string(state.Mode), func(t *testing.T) {
			img, err := ui_ipc.Render(state)
			if err != nil {
				t.Fatalf("Render failed for %s: %v", state.Mode, err)
			}
			if img == nil {
				t.Fatalf("Render returned nil image for %s", state.Mode)
			}
			if img.Bounds().Dx() != ui_ipc.ScreenWidth || img.Bounds().Dy() != ui_ipc.ScreenHeight {
				t.Fatalf("unexpected canvas dimensions: %v", img.Bounds())
			}
		})
	}
}

// Test 11: Customer physical keypad/keyboard input preserves leading zeros ("0482"),
// reflects typed digits on UI, and auto-submits to the payment workflow.
func TestWorkflow_CustomerKeypadInput_PreservesLeadingZerosAndTriggersWorkflow(t *testing.T) {
	tempDir := t.TempDir()
	store, err := state.Open(filepath.Join(tempDir, "state.db"))
	if err != nil {
		t.Fatalf("failed to open test state store: %v", err)
	}
	defer store.Close()

	submittedPIN := ""
	redeemCalled := false

	client, err := NewClient(ClientConfig{
		CloudURL:       "ws://127.0.0.1:8080/ws",
		KioskID:        "kiosk-keypad-01",
		KioskSecret:    "sec_kiosk_keypad_test",
		BackendBaseURL: "http://mock-backend",
		StateStore:     store,
		PrinterAddr:    "127.0.0.1:9100",
		DownloadDir:    tempDir,
		IdleQRContent:  "https://xeroxgo.app/kiosk-keypad-01",
		ScreenSink: ui_ipc.FramebufferSinkWithGeometry(filepath.Join(tempDir, "fb.raw"), ui_ipc.FramebufferGeometry{
			Width:        ui_ipc.ScreenWidth,
			Height:       ui_ipc.ScreenHeight,
			BitsPerPixel: 32,
			Stride:       ui_ipc.ScreenWidth * 4,
		}),
		HTTPClient: newMockHTTPClient(func(req *http.Request) (*http.Response, error) {
			switch req.URL.Path {
			case "/api/kiosk/pin/redeem":
				redeemCalled = true
				var body map[string]string
				_ = json.NewDecoder(req.Body).Decode(&body)
				submittedPIN = body["pin"]
				return jsonResponse(http.StatusOK, PINRedeemResponse{
					Status:           "ready_for_payment",
					SessionID:        "sess_keypad_123",
					OrderID:          "ord_keypad_456",
					PricePaise:       1000,
					SessionExpiresAt: time.Now().Add(30 * time.Minute).Unix(),
				}), nil
			case "/api/kiosk/payment/initiate-qr":
				return jsonResponse(http.StatusOK, PaymentQRResponse{
					AttemptID:   "att_keypad_001",
					SessionID:   "sess_keypad_123",
					OrderID:     "ord_keypad_456",
					AmountPaise: 1000,
					ExpiresAt:   time.Now().Add(15 * time.Minute).Unix(),
				}), nil
			case "/api/kiosk/payment/status":
				return jsonResponse(http.StatusOK, PaymentStatusResponse{
					SessionID:     "sess_keypad_123",
					OrderID:       "ord_keypad_456",
					OrderStatus:   "PAID_WAITING",
					SessionStatus: "active",
					IsPaid:        true,
				}), nil
			default:
				return jsonResponse(http.StatusNotFound, map[string]string{"error": "not found"}), nil
			}
		}),
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	keypad := client.Keypad()
	if keypad == nil {
		t.Fatalf("client.Keypad() returned nil")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Simulate physical key presses: '0', '4', '8', '2'
	keys := []rune{'0', '4', '8', '2'}
	for _, k := range keys {
		if err := keypad.HandleRune(ctx, k); err != nil {
			t.Fatalf("HandleRune(%c) failed: %v", k, err)
		}
	}

	if !redeemCalled {
		t.Fatalf("expected RedeemPIN to be called upon entering 4 digits")
	}
	if submittedPIN != "0482" {
		t.Fatalf("leading zero lost or PIN altered: got %q, want '0482'", submittedPIN)
	}

	// Verify screen history transitioned to PINEntry with "0482"
	history := client.GetRenderHistory()
	foundPINState := false
	for _, h := range history {
		if h.Mode == ui_ipc.ModePINEntry && h.PIN == "0482" {
			foundPINState = true
			break
		}
	}
	if !foundPINState {
		t.Fatalf("expected ScreenState with ModePINEntry and PIN '0482' in render history")
	}
}

// Test 12: Simultaneous notifications for the same job cannot enqueue the job twice.
func TestWorkflow_SimultaneousNotifications_CannotEnqueueJobTwice(t *testing.T) {
	tempDir := t.TempDir()
	store, err := state.Open(filepath.Join(tempDir, "state.db"))
	if err != nil {
		t.Fatalf("failed to open test state store: %v", err)
	}
	defer store.Close()

	client, err := NewClient(ClientConfig{
		CloudURL:       "ws://127.0.0.1:8080/ws",
		KioskID:        "kiosk-simul-01",
		KioskSecret:    "sec_kiosk_simul_test",
		BackendBaseURL: "http://mock-backend",
		StateStore:     store,
		PrinterAddr:    "127.0.0.1:9100",
		DownloadDir:    tempDir,
		IdleQRContent:  "https://xeroxgo.app/kiosk-simul-01",
		ScreenSink: ui_ipc.FramebufferSinkWithGeometry(filepath.Join(tempDir, "fb.raw"), ui_ipc.FramebufferGeometry{
			Width:        ui_ipc.ScreenWidth,
			Height:       ui_ipc.ScreenHeight,
			BitsPerPixel: 32,
			Stride:       ui_ipc.ScreenWidth * 4,
		}),
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	cmd := contracts.PrintJobCommand{
		JobID:          "job-simul-test-001",
		KioskID:        "kiosk-simul-01",
		DisplayCode:    "PIN-PROTECTED",
		DownloadURL:    "http://mock-backend/jobs/simul.pdf",
		SHA256Checksum: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		TotalPages:     2,
		TotalSheets:    1,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		StartFromPage:  1,
	}

	// Dispatch 10 simultaneous notifications concurrently
	const goroutines = 10
	startSignal := make(chan struct{})
	doneSignal := make(chan struct{}, goroutines)

	for i := 0; i < goroutines; i++ {
		go func() {
			<-startSignal
			client.handleIncomingJob(cmd)
			doneSignal <- struct{}{}
		}()
	}

	close(startSignal)
	for i := 0; i < goroutines; i++ {
		<-doneSignal
	}

	// Verify that exactly ONE job exists in client.jobQueue
	if len(client.jobQueue) != 1 {
		t.Fatalf("expected exactly 1 job in jobQueue, got %d", len(client.jobQueue))
	}
}

// Test 13: Completed job is dropped immediately and cannot be re-enqueued.
func TestWorkflow_CompletedJob_Rejected(t *testing.T) {
	tempDir := t.TempDir()
	store, err := state.Open(filepath.Join(tempDir, "state.db"))
	if err != nil {
		t.Fatalf("failed to open test state store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	cmd := contracts.PrintJobCommand{
		JobID:          "job-completed-001",
		KioskID:        "kiosk-completed-01",
		DisplayCode:    "PIN-PROTECTED",
		DownloadURL:    "http://mock-backend/jobs/comp.pdf",
		SHA256Checksum: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		TotalPages:     2,
		TotalSheets:    1,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		StartFromPage:  1,
	}

	// Pre-save as COMPLETED in state store
	if err := store.SaveJob(ctx, cmd, contracts.JobStatusCompleted); err != nil {
		t.Fatalf("SaveJob failed: %v", err)
	}

	client, err := NewClient(ClientConfig{
		CloudURL:       "ws://127.0.0.1:8080/ws",
		KioskID:        "kiosk-completed-01",
		KioskSecret:    "sec_kiosk_completed_test",
		BackendBaseURL: "http://mock-backend",
		StateStore:     store,
		PrinterAddr:    "127.0.0.1:9100",
		DownloadDir:    tempDir,
		IdleQRContent:  "https://xeroxgo.app/kiosk-completed-01",
		ScreenSink: ui_ipc.FramebufferSinkWithGeometry(filepath.Join(tempDir, "fb.raw"), ui_ipc.FramebufferGeometry{
			Width:        ui_ipc.ScreenWidth,
			Height:       ui_ipc.ScreenHeight,
			BitsPerPixel: 32,
			Stride:       ui_ipc.ScreenWidth * 4,
		}),
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	client.handleIncomingJob(cmd)

	if len(client.jobQueue) != 0 {
		t.Fatalf("expected 0 jobs in jobQueue for completed job, got %d", len(client.jobQueue))
	}
}

// Test 14: Interrupted PRINTING job upon daemon restart is transitioned to PAUSED_ERROR
// and is NOT automatically re-enqueued to prevent duplicate physical prints.
func TestWorkflow_InterruptedPrintingRestart_MarksPausedError_DoesNotReprint(t *testing.T) {
	tempDir := t.TempDir()
	store, err := state.Open(filepath.Join(tempDir, "state.db"))
	if err != nil {
		t.Fatalf("failed to open test state store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	cmd := contracts.PrintJobCommand{
		JobID:          "job-interrupted-001",
		KioskID:        "kiosk-restart-01",
		DisplayCode:    "PIN-PROTECTED",
		DownloadURL:    "http://mock-backend/jobs/interrupted.pdf",
		SHA256Checksum: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		TotalPages:     10,
		TotalSheets:    5,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		StartFromPage:  1,
	}

	// Pre-save job and update progress to PRINTING with 3 pages printed
	if err := store.SaveJob(ctx, cmd, contracts.JobStatusSpooling); err != nil {
		t.Fatalf("SaveJob failed: %v", err)
	}
	if err := store.UpdateProgress(ctx, contracts.JobProgressEvent{
		JobID:         cmd.JobID,
		KioskID:       cmd.KioskID,
		Status:        contracts.JobStatusPrinting,
		PagesPrinted:  3,
		SheetsPrinted: 3,
		Timestamp:     time.Now().UTC(),
	}); err != nil {
		t.Fatalf("UpdateProgress failed: %v", err)
	}

	client, err := NewClient(ClientConfig{
		CloudURL:       "ws://127.0.0.1:8080/ws",
		KioskID:        "kiosk-restart-01",
		KioskSecret:    "sec_kiosk_restart_test",
		BackendBaseURL: "http://mock-backend",
		StateStore:     store,
		PrinterAddr:    "127.0.0.1:9100",
		DownloadDir:    tempDir,
		IdleQRContent:  "https://xeroxgo.app/kiosk-restart-01",
		ScreenSink: ui_ipc.FramebufferSinkWithGeometry(filepath.Join(tempDir, "fb.raw"), ui_ipc.FramebufferGeometry{
			Width:        ui_ipc.ScreenWidth,
			Height:       ui_ipc.ScreenHeight,
			BitsPerPixel: 32,
			Stride:       ui_ipc.ScreenWidth * 4,
		}),
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	runCtx, runCancel := context.WithCancel(context.Background())
	// Cancel immediately so Run performs startup recovery and exits
	runCancel()
	_ = client.Run(runCtx)

	// Verify that the job was NOT added to the job queue
	if len(client.jobQueue) != 0 {
		t.Fatalf("expected jobQueue to be empty after interrupted job startup check, got %d", len(client.jobQueue))
	}

	// Verify that the job in SQLite was updated to PAUSED_ERROR with explanation
	persistedJob, err := store.GetJob(context.Background(), cmd.JobID)
	if err != nil {
		t.Fatalf("GetJob failed: %v", err)
	}
	if persistedJob.Status != contracts.JobStatusPausedError {
		t.Fatalf("expected job status to be PAUSED_ERROR, got %s", persistedJob.Status)
	}
	if !strings.Contains(persistedJob.ErrorReason, "interrupted during printing") {
		t.Fatalf("expected error reason to contain 'interrupted during printing', got %q", persistedJob.ErrorReason)
	}

	// Verify subsequent incoming notification for this interrupted job is also rejected
	client.handleIncomingJob(cmd)
	if len(client.jobQueue) != 0 {
		t.Fatalf("expected incoming notification for interrupted job to be rejected, got %d in jobQueue", len(client.jobQueue))
	}
}

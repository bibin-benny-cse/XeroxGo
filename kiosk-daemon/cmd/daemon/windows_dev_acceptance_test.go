package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"xeroxgo/kiosk-daemon/internal/input"
	"xeroxgo/kiosk-daemon/internal/printer"
	"xeroxgo/kiosk-daemon/internal/spooler"
	"xeroxgo/kiosk-daemon/internal/state"
	"xeroxgo/kiosk-daemon/internal/ui_ipc"
	"xeroxgo/pkg/contracts"
)

// syncBuffer is a goroutine-safe io.Writer backed by bytes.Buffer.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// acceptanceTrace records sanitized endpoint status and screen mode transitions
// safely across background handlers without calling t.Log from worker goroutines.
type acceptanceTrace struct {
	mu               sync.Mutex
	wsRequests       int
	wsStatus         int
	wsErr            string
	pinRedeemCount   int
	pinRedeemStatus  int
	pinRedeemErr     string
	qrInitCount      int
	qrInitStatus     int
	qrInitErr        string
	qrFetchCount     int
	qrFetchStatus    int
	payPollCount     int
	payPollPaidCount int
	payPollStatus    int
	modes            []ui_ipc.ScreenMode
}

func (tr *acceptanceTrace) recordWS(status int, errMsg string) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.wsRequests++
	tr.wsStatus = status
	tr.wsErr = errMsg
}

func (tr *acceptanceTrace) recordPINRedeem(status int, errMsg string) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.pinRedeemCount++
	tr.pinRedeemStatus = status
	tr.pinRedeemErr = errMsg
}

func (tr *acceptanceTrace) recordQRInit(status int, errMsg string) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.qrInitCount++
	tr.qrInitStatus = status
	tr.qrInitErr = errMsg
}

func (tr *acceptanceTrace) recordQRFetch(status int) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.qrFetchCount++
	tr.qrFetchStatus = status
}

func (tr *acceptanceTrace) recordPayPoll(status int, paid bool) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.payPollCount++
	tr.payPollStatus = status
	if paid && status == http.StatusOK {
		tr.payPollPaidCount++
	}
}

func (tr *acceptanceTrace) recordMode(mode ui_ipc.ScreenMode) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.modes = append(tr.modes, mode)
}

func (tr *acceptanceTrace) hasMode(target ui_ipc.ScreenMode) bool {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	for _, m := range tr.modes {
		if m == target {
			return true
		}
	}
	return false
}

func (tr *acceptanceTrace) summary() string {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	modeNames := make([]string, len(tr.modes))
	for i, m := range tr.modes {
		modeNames[i] = string(m)
	}
	return fmt.Sprintf(
		"ws(req=%d,status=%d,err=%q) pin_redeem(calls=%d,status=%d,err=%q) qr_init(calls=%d,status=%d,err=%q) qr_fetch(calls=%d,status=%d) pay_poll(calls=%d,paid=%d,status=%d) modes=%v",
		tr.wsRequests, tr.wsStatus, tr.wsErr,
		tr.pinRedeemCount, tr.pinRedeemStatus, tr.pinRedeemErr,
		tr.qrInitCount, tr.qrInitStatus, tr.qrInitErr,
		tr.qrFetchCount, tr.qrFetchStatus,
		tr.payPollCount, tr.payPollPaidCount, tr.payPollStatus,
		modeNames,
	)
}

// TestWindowsDevMode_FullAcceptanceWorkflow tests the complete end-to-end Windows development workflow:
// Customer Uploads PDF -> PIN appears -> Daemon accepts PIN from Keyboard -> Payment Link created
// -> QR rendered on laptop screen -> Test Mode payment succeeds -> Webhook verified -> PDF downloaded
// -> Simulated printer records the job -> Reaches simulated completion state.
func TestWindowsDevMode_FullAcceptanceWorkflow(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	ui_ipc.SetDevelopmentMode(true)
	defer ui_ipc.SetDevelopmentMode(false)

	const testKioskID = "kiosk-windows-dev"
	const testKioskSecret = "sec_windows_dev_secret_32_chars_ok"
	const testPIN = "0482"
	const testOrderID = "ord_win_dev_001"
	const testSessionID = "sess_win_dev_001"
	const testJobID = "job_win_dev_001"

	// 1. Create realistic test PDF payload
	pdfBytes := []byte("%PDF-1.4\n1 0 obj << /Type /Catalog /Pages 2 0 R >> endobj\n2 0 obj << /Type /Pages /Kids [3 0 R] /Count 1 >> endobj\n3 0 obj << /Type /Page >> endobj\nxref\ntrailer\nstartxref\n%%EOF")
	pdfHash := sha256.Sum256(pdfBytes)
	pdfChecksum := hex.EncodeToString(pdfHash[:])

	// 2. Mock Backend State & Sanitized Observability
	var isPaid atomic.Bool
	var printJobDispatched atomic.Bool
	var activeWSConn atomic.Pointer[websocket.Conn]
	var acceptedWSCount atomic.Int32
	trace := &acceptanceTrace{}

	// Create test QR PNG
	qrImg := image.NewRGBA(image.Rect(0, 0, 100, 100))
	for y := 0; y < 100; y++ {
		for x := 0; x < 100; x++ {
			qrImg.Set(x, y, color.Black)
		}
	}
	var qrPNGBuffer bytes.Buffer
	_ = png.Encode(&qrPNGBuffer, qrImg)

	mux := http.NewServeMux()

	// Kiosk PIN redemption endpoint
	mux.HandleFunc("/api/kiosk/pin/redeem", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Kiosk-ID") != testKioskID || r.Header.Get("X-Kiosk-Secret") != testKioskSecret {
			trace.recordPINRedeem(http.StatusUnauthorized, "unauthorized")
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["pin"] != testPIN {
			trace.recordPINRedeem(http.StatusBadRequest, "invalid_pin")
			http.Error(w, `{"error":"invalid_pin"}`, http.StatusBadRequest)
			return
		}
		trace.recordPINRedeem(http.StatusOK, "")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"status":             "ready_for_payment",
			"session_id":         testSessionID,
			"order_id":           testOrderID,
			"kiosk_id":           testKioskID,
			"price_paise":        300,
			"total_pages":        1,
			"total_sheets":       1,
			"copies":             1,
			"duplex":             "simplex",
			"session_expires_at": time.Now().Add(10 * time.Minute).Unix(),
		})
	})

	// Kiosk Initiate Payment QR endpoint
	mux.HandleFunc("/api/kiosk/payment/initiate-qr", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Kiosk-ID") != testKioskID || r.Header.Get("X-Kiosk-Secret") != testKioskSecret {
			trace.recordQRInit(http.StatusUnauthorized, "unauthorized")
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		trace.recordQRInit(http.StatusOK, "")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"attempt_id":     "att_dev_001",
			"session_id":     testSessionID,
			"order_id":       testOrderID,
			"razorpay_qr_id": "plink_dev_001",
			"qr_image_url":   "/api/kiosk/payment/qr.png",
			"payment_link":   "https://rzp.io/i/XG-DEV-001",
			"amount_paise":   300,
			"currency":       "INR",
			"status":         "created",
			"expires_at":     time.Now().Add(10 * time.Minute).Unix(),
		})
	})

	// QR Image download endpoint
	mux.HandleFunc("/api/kiosk/payment/qr.png", func(w http.ResponseWriter, r *http.Request) {
		trace.recordQRFetch(http.StatusOK)
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(qrPNGBuffer.Bytes())
	})

	// Payment Status check endpoint (polled by kiosk)
	mux.HandleFunc("/api/kiosk/payment/status", func(w http.ResponseWriter, r *http.Request) {
		paid := isPaid.Load()
		orderSt := "PENDING_PAYMENT"
		if paid {
			orderSt = "PAID_WAITING"
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]interface{}{
			"session_id":     testSessionID,
			"order_id":       testOrderID,
			"order_status":   orderSt,
			"session_status": "active",
			"is_paid":        paid,
		}); err != nil {
			trace.recordPayPoll(http.StatusInternalServerError, false)
			return
		}
		trace.recordPayPoll(http.StatusOK, paid)
	})

	// PDF Download endpoint
	mux.HandleFunc("/download/job.pdf", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		_, _ = w.Write(pdfBytes)
	})

	// WebSocket Hub endpoint
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		kioskID := r.URL.Query().Get("kiosk_id")
		if kioskID != testKioskID {
			trace.recordWS(http.StatusBadRequest, "missing or invalid kiosk_id")
			http.Error(w, "missing or invalid kiosk_id", http.StatusBadRequest)
			return
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			trace.recordWS(http.StatusInternalServerError, err.Error())
			return
		}
		trace.recordWS(http.StatusSwitchingProtocols, "")
		acceptedWSCount.Add(1)
		activeWSConn.Store(conn)
		defer func() {
			activeWSConn.Store(nil)
			_ = conn.Close(websocket.StatusNormalClosure, "done")
		}()

		for {
			var msg map[string]interface{}
			if err := wsjson.Read(ctx, conn, &msg); err != nil {
				return
			}
		}
	})

	server := httptest.NewServer(mux)
	defer func() {
		cancel()
		server.Close()
	}()

	u, _ := url.Parse(server.URL)
	wsURL := "ws://" + u.Host + "/ws"

	// 3. Initialize local State Database and Download Directory
	tmpDir := t.TempDir()
	stateDBPath := filepath.Join(tmpDir, "kiosk_state.db")
	downloadDir := filepath.Join(tmpDir, "jobs")

	stateStore, err := state.Open(stateDBPath)
	if err != nil {
		t.Fatalf("failed to open state store: %v", err)
	}
	defer stateStore.Close()

	// 4. Initialize Dev Display Adapter (captures terminal output cleanly; PNG encoding skipped in headless test)
	terminalBuf := &syncBuffer{}
	devDisplay, err := ui_ipc.NewDevDisplayAdapter(ui_ipc.DevDisplayConfig{
		PreviewFile:   "",
		WebPort:       0,
		DisablePNG:    true,
		ConsoleWriter: terminalBuf,
	})
	if err != nil {
		t.Fatalf("failed to create DevDisplayAdapter: %v", err)
	}
	defer devDisplay.Close()

	// 5. Initialize Dev Printer Adapter (PJL Loopback Simulation)
	var devPrinterWg sync.WaitGroup
	printerAddr, cleanupPrinter, err := printer.StartDevPrinter(ctx, downloadDir, &devPrinterWg)
	if err != nil {
		t.Fatalf("failed to start dev printer: %v", err)
	}
	defer func() {
		cleanupPrinter()
		devPrinterWg.Wait()
	}()

	// 6. Build Spooler Client with Dev Adapters
	clientCfg := spooler.ClientConfig{
		CloudURL:            wsURL,
		KioskID:             testKioskID,
		KioskSecret:         testKioskSecret,
		BackendBaseURL:      server.URL,
		HTTPClient:          server.Client(),
		MinBackoff:          20 * time.Millisecond,
		MaxBackoff:          100 * time.Millisecond,
		HeartbeatInterval:   100 * time.Millisecond,
		PaymentPollInterval: 100 * time.Millisecond,
		StateStore:          stateStore,
		PrinterAddr:         printerAddr,
		PrinterModel:        "Brother HL-L5210DN (Simulated Dev Mode)",
		DownloadDir:         downloadDir,
		IdleQRContent:       server.URL + "/k/" + testKioskID,
		ScreenSink:          devDisplay.Sink,
		StateObserver: func(st ui_ipc.ScreenState) {
			trace.recordMode(st.Mode)
			devDisplay.ObserveState(st)
		},
		MockHealth:               printer.DevPrinterHealth,
		CompletedDisplayDuration: 1 * time.Second,
	}

	client, err := spooler.NewClient(clientCfg)
	if err != nil {
		t.Fatalf("failed to create spooler client: %v", err)
	}

	var bgWg sync.WaitGroup
	defer func() {
		cancel()
		bgWg.Wait()
	}()

	// 7. Start Spooler in Background
	t.Logf("[DIAG-TEST] Initializing Spooler in background, target wsURL=%s, backendURL=%s", wsURL, server.URL)
	clientErrCh := make(chan error, 1)
	bgWg.Add(1)
	go func() {
		defer bgWg.Done()
		clientErrCh <- client.Run(ctx)
	}()

	// Wait for WebSocket connection to establish
	requireCondition(t, 5*time.Second, func() bool {
		select {
		case clientErr := <-clientErrCh:
			t.Fatalf("spooler client exited prematurely: %v (trace: %s)", clientErr, trace.summary())
		default:
		}
		return activeWSConn.Load() != nil
	}, "kiosk failed to connect to cloud WebSocket hub", trace)

	// Verify mock hub recorded the accepted connection
	if count := acceptedWSCount.Load(); count < 1 {
		t.Fatalf("expected mock hub to accept at least 1 kiosk connection, got %d (trace: %s)", count, trace.summary())
	}

	// 8. Test Keyboard Input: Customer types PIN '0482'
	keypadCtrl := client.Keypad()
	if keypadCtrl == nil {
		t.Fatal("keypad controller is nil")
	}

	devInputLog := &syncBuffer{}
	devKeyboard := input.NewDevKeyboardAdapter(keypadCtrl, strings.NewReader("0482\n"), devInputLog)

	bgWg.Add(1)
	go func() {
		defer bgWg.Done()
		_ = devKeyboard.Run(ctx)
	}()

	// 9. Verify Payment Link & QR are displayed on laptop screen
	requireCondition(t, 3*time.Second, func() bool {
		st := devDisplay.GetLastState()
		return st.Mode == ui_ipc.ModePaymentQR && st.PaymentQRURL == "https://rzp.io/i/XG-DEV-001"
	}, "kiosk failed to display Razorpay payment QR screen", trace)

	// Verify sanitized observability recorded the expected workflow progression
	if !trace.hasMode(ui_ipc.ModePaymentQR) {
		t.Fatalf("expected mode transition %s in trace: %s", ui_ipc.ModePaymentQR, trace.summary())
	}
	trace.mu.Lock()
	pinCount, pinStatus, pinErr := trace.pinRedeemCount, trace.pinRedeemStatus, trace.pinRedeemErr
	qrCount, qrStatus, qrErr := trace.qrInitCount, trace.qrInitStatus, trace.qrInitErr
	fetchCount, fetchStatus := trace.qrFetchCount, trace.qrFetchStatus
	trace.mu.Unlock()
	if pinCount < 1 || pinStatus != http.StatusOK || pinErr != "" {
		t.Fatalf("PIN redemption step incomplete or failed: count=%d status=%d err=%q (%s)", pinCount, pinStatus, pinErr, trace.summary())
	}
	if qrCount < 1 || qrStatus != http.StatusOK || qrErr != "" {
		t.Fatalf("payment initiation step incomplete or failed: count=%d status=%d err=%q (%s)", qrCount, qrStatus, qrErr, trace.summary())
	}
	if fetchCount < 1 || fetchStatus != http.StatusOK {
		t.Fatalf("QR retrieval step incomplete or failed: count=%d status=%d (%s)", fetchCount, fetchStatus, trace.summary())
	}

	// Check terminal output contains development indicators and amount
	termOutput := terminalBuf.String()
	if !strings.Contains(termOutput, "XEROXGO KIOSK [DEVELOPMENT MODE - WINDOWS LAPTOP]") {
		t.Error("expected development mode banner in terminal output")
	}
	if !strings.Contains(termOutput, "₹3.00 (300 paise)") {
		t.Errorf("expected ₹3.00 in terminal output: %s", termOutput)
	}
	if !strings.Contains(termOutput, "RAZORPAY PAYMENT QR") {
		t.Error("expected Razorpay QR section in terminal output")
	}

	// 10. Simulate Real Webhook Arrival: payment_link.paid verified by backend
	isPaid.Store(true)

	// Wait until at least one successful payment status poll response observes isPaid == true
	requireCondition(t, 2*time.Second, func() bool {
		trace.mu.Lock()
		pollPaidCount := trace.payPollPaidCount
		trace.mu.Unlock()
		return pollPaidCount >= 1
	}, "kiosk failed to poll paid payment status response", trace)

	// Backend dispatches PrintJobCommand over WebSocket
	wsConn := activeWSConn.Load()
	if wsConn == nil {
		t.Fatal("websocket connection dropped")
	}

	printCmd := contracts.PrintJobCommand{
		JobID:          testJobID,
		KioskID:        testKioskID,
		DisplayCode:    "A74X",
		DownloadURL:    server.URL + "/download/job.pdf",
		SHA256Checksum: pdfChecksum,
		TotalPages:     1,
		TotalSheets:    1,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		StartFromPage:  1,
	}

	err = wsjson.Write(ctx, wsConn, map[string]interface{}{
		"type": "job",
		"job":  printCmd,
	})
	if err != nil {
		t.Fatalf("failed to send print job over websocket: %v", err)
	}
	printJobDispatched.Store(true)

	// 11. Verify PDF Downloaded & Verified
	requireCondition(t, 5*time.Second, func() bool {
		h := sha256.Sum256([]byte(testJobID))
		finalName := hex.EncodeToString(h[:]) + ".pdf"
		downloadedPath := filepath.Join(downloadDir, finalName)
		info, err := filepath.Glob(downloadedPath)
		return err == nil && len(info) > 0
	}, "kiosk failed to download and verify PDF payload", trace)

	// 12. Verify Simulated Printer Execution and Completion
	requireCondition(t, 5*time.Second, func() bool {
		completed, err := stateStore.IsJobCompleted(ctx, testJobID)
		return err == nil && completed
	}, "print job did not reach JobStatusCompleted in state store", trace)

	// 13. Verify Simulated Paper Deduction
	sheets, err := stateStore.GetEstimatedSheets(ctx)
	if err != nil {
		t.Fatalf("failed to get estimated sheets: %v", err)
	}
	if sheets != 249 { // 250 initial - 1 sheet printed
		t.Errorf("expected 249 sheets remaining, got %d", sheets)
	}

	// 14. Verify Final Terminal Output reflects Simulated Completion
	finalTerm := terminalBuf.String()
	if !strings.Contains(finalTerm, "COMPLETED [SIMULATED]") {
		t.Errorf("expected simulated completion in terminal: %s", finalTerm)
	}
	if !strings.Contains(finalTerm, "Simulated print on Windows. Zero physical paper was used.") {
		t.Errorf("expected physical vs simulated distinction in terminal: %s", finalTerm)
	}

	t.Logf("[ACCEPTANCE TRACE] %s", trace.summary())

	// Stop client cleanly
	cancel()
}

func requireCondition(t *testing.T, timeout time.Duration, condition func() bool, errMsg string, trace *acceptanceTrace) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	if trace != nil {
		t.Fatalf("condition timed out after %s: %s (trace: %s)", timeout, errMsg, trace.summary())
	}
	t.Fatalf("condition timed out after %s: %s", timeout, errMsg)
}

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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

	// 2. Mock Backend State
	var isPaid atomic.Bool
	var printJobDispatched atomic.Bool
	var activeWSConn atomic.Pointer[websocket.Conn]

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
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["pin"] != testPIN {
			http.Error(w, `{"error":"invalid_pin"}`, http.StatusBadRequest)
			return
		}
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
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
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
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"session_id":     testSessionID,
			"order_id":       testOrderID,
			"order_status":   orderSt,
			"session_status": "active",
			"is_paid":        paid,
		})
	})

	// PDF Download endpoint
	mux.HandleFunc("/download/job.pdf", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		_, _ = w.Write(pdfBytes)
	})

	// WebSocket Hub endpoint
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		activeWSConn.Store(conn)
		defer activeWSConn.Store(nil)
		defer conn.Close(websocket.StatusNormalClosure, "done")

		for {
			var msg map[string]interface{}
			if err := wsjson.Read(r.Context(), conn, &msg); err != nil {
				return
			}
		}
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	u, _ := url.Parse(server.URL)
	wsURL := "ws://" + u.Host + "/ws"

	// 3. Initialize local State Database and Download Directory
	tmpDir := t.TempDir()
	stateDBPath := filepath.Join(tmpDir, "kiosk_state.db")
	downloadDir := filepath.Join(tmpDir, "jobs")
	previewPNG := filepath.Join(tmpDir, "dev_screen.png")

	stateStore, err := state.Open(stateDBPath)
	if err != nil {
		t.Fatalf("failed to open state store: %v", err)
	}
	defer stateStore.Close()

	// 4. Initialize Dev Display Adapter (captures terminal output & PNG)
	var terminalBuf bytes.Buffer
	devDisplay, err := ui_ipc.NewDevDisplayAdapter(ui_ipc.DevDisplayConfig{
		PreviewFile:   previewPNG,
		WebPort:       0, // disabled in test
		ConsoleWriter: &terminalBuf,
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
	defer cleanupPrinter()

	// 6. Build Spooler Client with Dev Adapters
	clientCfg := spooler.ClientConfig{
		CloudURL:                 wsURL,
		KioskID:                  testKioskID,
		KioskSecret:              testKioskSecret,
		BackendBaseURL:           server.URL,
		PaymentPollInterval:      100 * time.Millisecond,
		StateStore:               stateStore,
		PrinterAddr:              printerAddr,
		PrinterModel:             "Brother HL-L5210DN (Simulated Dev Mode)",
		DownloadDir:              downloadDir,
		IdleQRContent:            server.URL + "/k/" + testKioskID,
		ScreenSink:               devDisplay.Sink,
		StateObserver:            devDisplay.ObserveState,
		MockHealth:               printer.DevPrinterHealth,
		CompletedDisplayDuration: 1 * time.Second,
	}

	client, err := spooler.NewClient(clientCfg)
	if err != nil {
		t.Fatalf("failed to create spooler client: %v", err)
	}

	// 7. Start Spooler in Background
	clientErrCh := make(chan error, 1)
	go func() {
		clientErrCh <- client.Run(ctx)
	}()

	// Wait for WebSocket connection to establish
	requireCondition(t, 2*time.Second, func() bool {
		return activeWSConn.Load() != nil
	}, "kiosk failed to connect to cloud WebSocket hub")

	// 8. Test Keyboard Input: Customer types PIN '0482'
	keypadCtrl := client.Keypad()
	if keypadCtrl == nil {
		t.Fatal("keypad controller is nil")
	}

	var devInputLog bytes.Buffer
	devKeyboard := input.NewDevKeyboardAdapter(keypadCtrl, strings.NewReader("0482\n"), &devInputLog)

	go func() {
		_ = devKeyboard.Run(ctx)
	}()

	// 9. Verify Payment Link & QR are displayed on laptop screen
	requireCondition(t, 3*time.Second, func() bool {
		st := devDisplay.GetLastState()
		return st.Mode == ui_ipc.ModePaymentQR && st.PaymentQRURL == "https://rzp.io/i/XG-DEV-001"
	}, "kiosk failed to display Razorpay payment QR screen")

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
	}, "kiosk failed to download and verify PDF payload")

	// 12. Verify Simulated Printer Execution and Completion
	requireCondition(t, 5*time.Second, func() bool {
		completed, err := stateStore.IsJobCompleted(ctx, testJobID)
		return err == nil && completed
	}, "print job did not reach JobStatusCompleted in state store")

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

	// Stop client cleanly
	cancel()
}

func requireCondition(t *testing.T, timeout time.Duration, condition func() bool, errMsg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("condition timed out after %s: %s", timeout, errMsg)
}

package ui_ipc

import (
	"bytes"
	"image"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDevDisplayAdapter_ObserveState_TerminalOutput(t *testing.T) {
	var buf bytes.Buffer
	adapter, err := NewDevDisplayAdapter(DevDisplayConfig{
		ConsoleWriter: &buf,
	})
	if err != nil {
		t.Fatalf("failed to create DevDisplayAdapter: %v", err)
	}
	defer adapter.Close()

	// 1. Idle state
	adapter.ObserveState(ScreenState{
		Mode:      ModeIdle,
		QRContent: "https://xeroxgo-frontend.vercel.app",
		Message:   "Ready to print",
	})
	output := buf.String()
	if !strings.Contains(output, "XEROXGO KIOSK [DEVELOPMENT MODE - WINDOWS LAPTOP]") {
		t.Errorf("expected development mode banner in output")
	}
	if !strings.Contains(output, "READY TO PRINT") {
		t.Errorf("expected ready to print status in output")
	}

	// 2. Payment QR state with UPI URL
	buf.Reset()
	adapter.ObserveState(ScreenState{
		Mode:             ModePaymentQR,
		OrderID:          "ord_test123",
		AmountPaise:      300,
		PaymentQRURL:     "https://rzp.io/i/testlink",
		PaymentExpiresAt: time.Now().Add(5 * time.Minute),
		Message:          "Scan with UPI app",
	})
	output = buf.String()
	if !strings.Contains(output, "ord_test123") {
		t.Errorf("expected order ID in output")
	}
	if !strings.Contains(output, "₹3.00 (300 paise)") {
		t.Errorf("expected amount in output")
	}
	if !strings.Contains(output, "RAZORPAY PAYMENT QR") {
		t.Errorf("expected QR header in output")
	}

	// 3. Printing state
	buf.Reset()
	adapter.ObserveState(ScreenState{
		Mode:            ModePrinting,
		OrderID:         "ord_test123",
		CurrentPage:     2,
		TotalPages:      4,
		ProgressPercent: 50,
	})
	output = buf.String()
	if !strings.Contains(output, "PRINTING [SIMULATED]") {
		t.Errorf("expected simulated printing state in output")
	}
	if !strings.Contains(output, "Page 2 / 4 (50%)") {
		t.Errorf("expected page progress in output")
	}
	if !strings.Contains(output, "SIMULATED PRINTER") {
		t.Errorf("expected simulated printer mention in output")
	}
}

func TestDevDisplayAdapter_Sink_SavesPNG(t *testing.T) {
	tmpDir := t.TempDir()
	pngPath := filepath.Join(tmpDir, "dev_screen.png")

	adapter, err := NewDevDisplayAdapter(DevDisplayConfig{
		PreviewFile:     pngPath,
		DisableTerminal: true,
	})
	if err != nil {
		t.Fatalf("failed to create DevDisplayAdapter: %v", err)
	}
	defer adapter.Close()

	img := image.NewRGBA(image.Rect(0, 0, 100, 100))
	if err := adapter.Sink(img); err != nil {
		t.Fatalf("Sink failed: %v", err)
	}

	pngBytes := adapter.GetLatestPNG()
	if len(pngBytes) == 0 {
		t.Fatalf("expected non-empty latestPNG bytes")
	}

	if _, err := os.Stat(pngPath); err != nil {
		t.Fatalf("preview file was not created at %s: %v", pngPath, err)
	}
}

func TestDevDisplayAdapter_WebDashboard(t *testing.T) {
	adapter, err := NewDevDisplayAdapter(DevDisplayConfig{
		DisableTerminal: true,
	})
	if err != nil {
		t.Fatalf("failed to create DevDisplayAdapter: %v", err)
	}
	defer adapter.Close()

	adapter.ObserveState(ScreenState{
		Mode:         ModePaymentQR,
		OrderID:      "ord_webtest",
		AmountPaise:  500,
		PaymentQRURL: "https://rzp.io/i/webtestlink",
	})

	img := image.NewRGBA(image.Rect(0, 0, 50, 50))
	_ = adapter.Sink(img)

	// Test GET /
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	adapter.handleWebDashboard(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK from web dashboard, got %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "DEVELOPMENT MODE (WINDOWS)") {
		t.Errorf("expected development mode badge in HTML")
	}
	if !strings.Contains(body, "ord_webtest") {
		t.Errorf("expected order ID in HTML")
	}
	if !strings.Contains(body, "https://rzp.io/i/webtestlink") {
		t.Errorf("expected payment link in HTML")
	}
}

func TestDevelopmentMode_BadgeRendered(t *testing.T) {
	SetDevelopmentMode(true)
	defer SetDevelopmentMode(false)

	if !IsDevelopmentMode() {
		t.Fatal("expected IsDevelopmentMode() to be true")
	}

	canvas := image.NewRGBA(image.Rect(0, 0, ScreenWidth, ScreenHeight))
	err := RenderTo(canvas, ScreenState{
		Mode:      ModeIdle,
		QRContent: "https://example.com",
	})
	if err != nil {
		t.Fatalf("RenderTo failed: %v", err)
	}
}

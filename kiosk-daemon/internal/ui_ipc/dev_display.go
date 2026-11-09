package ui_ipc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/skip2/go-qrcode"
)

// DevDisplayConfig configures the development display adapter.
type DevDisplayConfig struct {
	PreviewFile     string    // Path to write preview PNG (e.g. data/dev_screen.png)
	WebPort         int       // Local HTTP port for web preview (e.g. 8085; 0 to disable)
	ConsoleWriter   io.Writer // Writer for terminal dashboard (defaults to os.Stdout)
	DisableTerminal bool      // If true, suppresses terminal output (useful for tests)
}

// DevDisplayAdapter displays kiosk screen states on a development laptop:
// 1. Outputs a clean ASCII/Unicode dashboard to the console with live QR code and progress bars.
// 2. Writes the rendered 1920x1080 graphic frame to a local PNG file.
// 3. Hosts an optional local web preview at http://127.0.0.1:<WebPort> for side-by-side browser testing.
type DevDisplayAdapter struct {
	cfg        DevDisplayConfig
	mu         sync.Mutex
	lastState  ScreenState
	latestPNG  []byte
	webServer  *http.Server
	webAddr    string
	writer     io.Writer
	fileSink   Sink
	activeLink string
}

// NewDevDisplayAdapter initializes and returns a development display adapter.
func NewDevDisplayAdapter(cfg DevDisplayConfig) (*DevDisplayAdapter, error) {
	if cfg.ConsoleWriter == nil {
		cfg.ConsoleWriter = os.Stdout
	}

	adapter := &DevDisplayAdapter{
		cfg:    cfg,
		writer: cfg.ConsoleWriter,
	}

	if cfg.PreviewFile != "" {
		if err := os.MkdirAll(filepath.Dir(cfg.PreviewFile), 0755); err != nil {
			return nil, fmt.Errorf("failed to create preview directory: %w", err)
		}
		adapter.fileSink = FilePreviewSink(cfg.PreviewFile)
	}

	if cfg.WebPort > 0 {
		if err := adapter.startWebServer(cfg.WebPort); err != nil {
			log.Printf("[DEV DISPLAY] Warning: failed to start web preview on port %d: %v", cfg.WebPort, err)
		}
	}

	return adapter, nil
}

// encodeFastPNG encodes an image into PNG format using BestSpeed compression level.
// A local png.Encoder value is used on each call to prevent shared mutable state across goroutines.
func encodeFastPNG(w io.Writer, m image.Image) error {
	enc := png.Encoder{CompressionLevel: png.BestSpeed}
	return enc.Encode(w, m)
}

// Sink receives a rendered RGBA canvas, saves it to the preview PNG, and updates web bytes.
func (a *DevDisplayAdapter) Sink(img *image.RGBA) error {
	if img == nil {
		return fmt.Errorf("image cannot be nil")
	}

	var buf bytes.Buffer
	if err := encodeFastPNG(&buf, img); err != nil {
		return fmt.Errorf("failed to encode PNG: %w", err)
	}

	pngBytes := buf.Bytes()

	a.mu.Lock()
	a.latestPNG = pngBytes
	a.mu.Unlock()

	if a.fileSink != nil {
		_ = a.fileSink(img)
	}

	return nil
}

// ObserveState is notified whenever the kiosk renders a new ScreenState.
// It formats and displays the state on the laptop screen and terminal.
func (a *DevDisplayAdapter) ObserveState(state ScreenState) {
	a.mu.Lock()
	a.lastState = state
	if state.PaymentQRURL != "" {
		a.activeLink = state.PaymentQRURL
	}
	a.mu.Unlock()

	if a.cfg.DisableTerminal {
		return
	}

	a.renderTerminalDashboard(state)
}

// GetLatestPNG returns the most recent rendered PNG bytes.
func (a *DevDisplayAdapter) GetLatestPNG() []byte {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.latestPNG
}

// GetLastState returns the most recent screen state observed.
func (a *DevDisplayAdapter) GetLastState() ScreenState {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.lastState
}

// WebAddr returns the listening address of the web preview server (e.g. "127.0.0.1:8085").
func (a *DevDisplayAdapter) WebAddr() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.webAddr
}

// Close gracefully stops the web server and cleans up resources.
func (a *DevDisplayAdapter) Close() error {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.webServer != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = a.webServer.Shutdown(ctx)
		a.webServer = nil
	}
	return nil
}

func (a *DevDisplayAdapter) startWebServer(port int) error {
	mux := http.NewServeMux()

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		a.handleWebDashboard(w, r)
	})

	mux.HandleFunc("/screen.png", func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		pngData := a.latestPNG
		a.mu.Unlock()

		if len(pngData) == 0 {
			http.Error(w, "Screen not yet rendered", http.StatusNotFound)
			return
		}

		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
		w.Header().Set("Pragma", "no-cache")
		w.Header().Set("Expires", "0")
		_, _ = w.Write(pngData)
	})

	mux.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		st := a.lastState
		a.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"mode":             st.Mode,
			"order_id":         st.OrderID,
			"amount_paise":     st.AmountPaise,
			"payment_qr_url":   st.PaymentQRURL,
			"current_page":     st.CurrentPage,
			"total_pages":      st.TotalPages,
			"progress_percent": st.ProgressPercent,
			"message":          st.Message,
			"detail":           st.Detail,
			"printer_model":    st.PrinterModel,
		})
	})

	addr := fmt.Sprintf("127.0.0.1:%d", port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}

	a.webAddr = ln.Addr().String()
	a.webServer = &http.Server{Handler: mux}

	go func() {
		_ = a.webServer.Serve(ln)
	}()

	log.Printf("[DEV DISPLAY] Live screen preview available at http://%s", a.webAddr)
	return nil
}

func (a *DevDisplayAdapter) handleWebDashboard(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	st := a.lastState
	link := a.activeLink
	a.mu.Unlock()

	modeName := modeDisplayName(st.Mode)
	amountRupees := float64(st.AmountPaise) / 100.0

	var payButtonHTML string
	if link != "" && (st.Mode == ModePaymentQR || st.Mode == ModeWaitingForPayment) {
		payButtonHTML = fmt.Sprintf(`
			<div style="margin: 20px 0; text-align: center;">
				<a href="%s" target="_blank" style="display: inline-block; background: #2563EB; color: white; padding: 14px 28px; font-weight: 700; border-radius: 8px; text-decoration: none; font-size: 18px; box-shadow: 0 4px 6px -1px rgba(0,0,0,0.1);">
					💳 Open Razorpay Test Payment Link (₹%.2f) &rarr;
				</a>
				<div style="color: #64748B; font-size: 13px; margin-top: 8px;">Direct link for laptop browser testing. In production, customers scan QR on kiosk.</div>
			</div>`, link, amountRupees)
	}

	html := fmt.Sprintf(`<!DOCTYPE html>
<html lang="en">
<head>
	<meta charset="UTF-8">
	<meta name="viewport" content="width=device-width, initial-scale=1.0">
	<title>XeroxGo Kiosk — Development Display (Windows)</title>
	<style>
		body { font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif; background: #0F172A; color: #F8FAFC; margin: 0; padding: 20px; }
		.container { max-width: 1100px; margin: 0 auto; }
		.header { display: flex; justify-content: space-between; align-items: center; border-bottom: 1px solid #334155; padding-bottom: 16px; margin-bottom: 20px; }
		.brand { font-size: 24px; font-weight: 800; color: #38BDF8; }
		.badge { background: #D97706; color: white; padding: 6px 14px; border-radius: 9999px; font-weight: 700; font-size: 12px; letter-spacing: 0.05em; }
		.status-card { background: #1E293B; border: 1px solid #334155; border-radius: 12px; padding: 16px 24px; margin-bottom: 20px; display: grid; grid-template-columns: repeat(auto-fit, minmax(200px, 1fr)); gap: 16px; }
		.field-label { font-size: 12px; color: #94A3B8; text-transform: uppercase; font-weight: 600; margin-bottom: 4px; }
		.field-val { font-size: 18px; font-weight: 700; color: #FFFFFF; }
		.screen-wrap { background: #000; border: 2px solid #334155; border-radius: 12px; overflow: hidden; box-shadow: 0 20px 25px -5px rgba(0,0,0,0.5); }
		.screen-img { width: 100%%; height: auto; display: block; }
	</style>
</head>
<body>
	<div class="container">
		<div class="header">
			<div class="brand">XeroxGo Kiosk Display</div>
			<div class="badge">DEVELOPMENT MODE (WINDOWS)</div>
		</div>

		<div class="status-card">
			<div>
				<div class="field-label">Screen State</div>
				<div class="field-val">%s</div>
			</div>
			<div>
				<div class="field-label">Order ID</div>
				<div class="field-val">%s</div>
			</div>
			<div>
				<div class="field-label">Payable Amount</div>
				<div class="field-val">₹%.2f</div>
			</div>
			<div>
				<div class="field-label">Simulated Hardware</div>
				<div class="field-val">Brother HL-L5210DN</div>
			</div>
		</div>

		%s

		<div class="screen-wrap">
			<img id="kiosk-img" class="screen-img" src="/screen.png" alt="Kiosk Screen" />
		</div>
	</div>

	<script>
		// Auto-refresh kiosk screen image smoothly every 1000ms
		setInterval(function() {
			var img = document.getElementById('kiosk-img');
			if (img) {
				img.src = '/screen.png?t=' + new Date().getTime();
			}
		}, 1000);
	</script>
</body>
</html>`, modeName, valOrDash(st.OrderID), amountRupees, payButtonHTML)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(html))
}

func (a *DevDisplayAdapter) renderTerminalDashboard(st ScreenState) {
	var b strings.Builder

	b.WriteString("\n")
	b.WriteString("================================================================================\n")
	b.WriteString("             XEROXGO KIOSK [DEVELOPMENT MODE - WINDOWS LAPTOP]                  \n")
	b.WriteString("================================================================================\n")
	fmt.Fprintf(&b, "  Screen Mode:     %s\n", modeDisplayName(st.Mode))

	if st.OrderID != "" {
		fmt.Fprintf(&b, "  Order ID:        %s\n", st.OrderID)
	}
	if st.AmountPaise > 0 {
		fmt.Fprintf(&b, "  Payable Amount:  ₹%.2f (%d paise)\n", float64(st.AmountPaise)/100.0, st.AmountPaise)
	}

	switch st.Mode {
	case ModeIdle:
		fmt.Fprintf(&b, "  Status:          READY TO PRINT\n")
		fmt.Fprintf(&b, "  Idle Portal URL: %s\n", st.QRContent)
		fmt.Fprintf(&b, "  Action:          Upload a document via the portal, then type your 4-digit PIN below\n")

	case ModePINEntry:
		fmt.Fprintf(&b, "  Buffered PIN:    [%s]\n", st.PIN)
		if st.PINError != "" {
			fmt.Fprintf(&b, "  PIN Error:       %s\n", st.PINError)
		}

	case ModePaymentQR, ModeWaitingForPayment:
		fmt.Fprintf(&b, "  Payment Status:  AWAITING CUSTOMER PAYMENT\n")
		if st.PaymentQRURL != "" {
			fmt.Fprintf(&b, "  Payment Link:    %s\n", st.PaymentQRURL)
		}
		if !st.PaymentExpiresAt.IsZero() {
			rem := time.Until(st.PaymentExpiresAt).Round(time.Second)
			if rem > 0 {
				fmt.Fprintf(&b, "  Expires In:      %s\n", rem)
			}
		}

		// Generate terminal QR code using half-blocks
		if st.PaymentQRURL != "" {
			if qrObj, err := qrcode.New(st.PaymentQRURL, qrcode.Medium); err == nil {
				b.WriteString("  ---------------------------- RAZORPAY PAYMENT QR -----------------------------\n")
				qrStr := qrObj.ToSmallString(false)
				for _, line := range strings.Split(qrStr, "\n") {
					if strings.TrimSpace(line) != "" {
						b.WriteString("  " + line + "\n")
					}
				}
				b.WriteString("  ------------------------------------------------------------------------------\n")
				b.WriteString("  (Scan QR with UPI app on phone, or click Payment Link on your laptop)\n")
			}
		}

	case ModeDownloading:
		bar := renderProgressBar(st.ProgressPercent, 30)
		fmt.Fprintf(&b, "  Download:        %s %d%%\n", bar, st.ProgressPercent)
		if st.Detail != "" {
			fmt.Fprintf(&b, "  Detail:          %s\n", st.Detail)
		}

	case ModePrinting:
		bar := renderProgressBar(st.ProgressPercent, 30)
		fmt.Fprintf(&b, "  Print Progress:  %s Page %d / %d (%d%%)\n", bar, st.CurrentPage, st.TotalPages, st.ProgressPercent)
		fmt.Fprintf(&b, "  Printer:         SIMULATED PRINTER (Emulating Brother HL-L5210DN PJL)\n")

	case ModeCompleted:
		fmt.Fprintf(&b, "  Print Status:    COMPLETED [SIMULATED]\n")
		fmt.Fprintf(&b, "  Simulated State: Job successfully finished in simulated printer tray.\n")
		fmt.Fprintf(&b, "  Note:            Simulated print on Windows. Zero physical paper was used.\n")

	case ModePaymentFailed, ModeError:
		errText := st.PaymentError
		if errText == "" {
			errText = st.Detail
		}
		if errText == "" {
			errText = st.Message
		}
		fmt.Fprintf(&b, "  Error:           %s\n", errText)
	}

	if st.Message != "" && st.Mode != ModeIdle {
		fmt.Fprintf(&b, "  Message:         %s\n", st.Message)
	}

	b.WriteString("================================================================================\n")

	_, _ = io.WriteString(a.writer, b.String())
}

func renderProgressBar(percent, width int) string {
	if percent < 0 {
		percent = 0
	}
	if percent > 100 {
		percent = 100
	}
	filled := (percent * width) / 100
	empty := width - filled

	return "[" + strings.Repeat("=", filled) + strings.Repeat(" ", empty) + "]"
}

func modeDisplayName(m ScreenMode) string {
	switch m {
	case ModeIdle:
		return "IDLE (Ready for Customer)"
	case ModePINEntry:
		return "PIN ENTRY"
	case ModePaymentQR:
		return "PAYMENT QR (Awaiting Payment)"
	case ModeWaitingForPayment:
		return "WAITING FOR WEBHOOK CONFIRMATION"
	case ModePaymentFailed:
		return "PAYMENT FAILED / RETRY"
	case ModeDownloading:
		return "DOWNLOADING DOCUMENT"
	case ModePrinting:
		return "PRINTING [SIMULATED]"
	case ModeCompleted:
		return "COMPLETED [SIMULATED]"
	case ModeError:
		return "ERROR"
	default:
		return string(m)
	}
}

func valOrDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}

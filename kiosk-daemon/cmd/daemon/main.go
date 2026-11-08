package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"xeroxgo/kiosk-daemon/internal/spooler"
	"xeroxgo/kiosk-daemon/internal/state"
	"xeroxgo/kiosk-daemon/internal/ui_ipc"
	"xeroxgo/pkg/contracts"
)

func sendSystemdNotify(state string) error {
	socketPath := os.Getenv("NOTIFY_SOCKET")
	if socketPath == "" {
		return nil
	}
	addr := &net.UnixAddr{
		Name: socketPath,
		Net:  "unixgram",
	}
	if strings.HasPrefix(socketPath, "@") {
		addr.Name = "\x00" + socketPath[1:]
	}
	conn, err := net.DialUnix("unixgram", nil, addr)
	if err != nil {
		return err
	}
	defer conn.Close()
	_, err = conn.Write([]byte(state))
	return err
}

func startSystemdWatchdog(ctx context.Context, wg *sync.WaitGroup) {
	usecStr := os.Getenv("WATCHDOG_USEC")
	if usecStr == "" {
		return
	}
	usec, err := strconv.ParseInt(usecStr, 10, 64)
	if err != nil || usec <= 0 {
		return
	}

	interval := time.Duration(usec/2) * time.Microsecond
	if interval < 100*time.Millisecond {
		interval = 100 * time.Millisecond
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				_ = sendSystemdNotify("WATCHDOG=1")
			}
		}
	}()
}

func startMockPrinter(ctx context.Context, downloadDir string, wg *sync.WaitGroup) (string, func(), error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, fmt.Errorf("failed to bind mock printer: %w", err)
	}

	var closeOnce sync.Once
	cleanup := func() {
		closeOnce.Do(func() {
			_ = ln.Close()
		})
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go handleMockPrinterConn(conn, downloadDir)
		}
	}()

	return ln.Addr().String(), cleanup, nil
}

func handleMockPrinterConn(conn net.Conn, downloadDir string) {
	defer conn.Close()

	var buf bytes.Buffer
	const maxRead = 300 * 1024 * 1024 // 300 MiB safety bound
	limited := io.LimitReader(conn, maxRead)
	if _, err := io.Copy(&buf, limited); err != nil {
		return
	}

	streamBytes := buf.Bytes()
	headerMarker := []byte("@PJL ENTER LANGUAGE=PDF")
	headerIdx := bytes.Index(streamBytes, headerMarker)
	if headerIdx == -1 {
		return
	}

	pdfStart := headerIdx + len(headerMarker)
	if pdfStart < len(streamBytes) && streamBytes[pdfStart] == '\r' {
		pdfStart++
	}
	if pdfStart < len(streamBytes) && streamBytes[pdfStart] == '\n' {
		pdfStart++
	}

	trailerMarker := []byte("\x1b%-12345X@PJL EOJ")
	trailerIdx := bytes.Index(streamBytes[pdfStart:], trailerMarker)
	var pdfBytes []byte
	if trailerIdx != -1 {
		pdfBytes = streamBytes[pdfStart : pdfStart+trailerIdx]
	} else {
		pdfBytes = streamBytes[pdfStart:]
	}

	tmpFile, err := os.CreateTemp(downloadDir, "mock_printer_*.pdf")
	if err != nil {
		return
	}
	tmpName := tmpFile.Name()
	defer func() {
		_ = tmpFile.Close()
		_ = os.Remove(tmpName)
	}()

	if _, err := tmpFile.Write(pdfBytes); err != nil {
		return
	}
	_ = tmpFile.Close()

	cmd := exec.Command("qpdf", "--show-npages", tmpName)
	out, err := cmd.CombinedOutput()
	pages := 1
	if err == nil {
		if p, perr := strconv.Atoi(strings.TrimSpace(string(out))); perr == nil && p > 0 {
			pages = p
		}
	}

	copies := 1
	reCopies := regexp.MustCompile(`@PJL\s+SET\s+COPIES\s*=\s*(\d+)`)
	if m := reCopies.FindSubmatch(streamBytes[:headerIdx]); len(m) > 1 {
		if c, cerr := strconv.Atoi(string(m[1])); cerr == nil && c > 0 {
			copies = c
		}
	}

	isDuplex := bytes.Contains(streamBytes[:headerIdx], []byte("@PJL SET DUPLEX=ON"))

	totalSheets := 0
	if !isDuplex {
		totalSheets = pages * copies
	} else {
		totalSheets = ((pages + 1) / 2) * copies
	}
	if totalSheets < 1 {
		totalSheets = 1
	}

	for sheet := 1; sheet <= totalSheets; sheet++ {
		time.Sleep(80 * time.Millisecond)
		msg := fmt.Sprintf("@PJL USTATUS PAGE\r\n%d\r\n", sheet)
		if _, werr := conn.Write([]byte(msg)); werr != nil {
			return
		}
	}

	time.Sleep(50 * time.Millisecond)
	_, _ = conn.Write([]byte("@PJL USTATUS JOB\r\nEND\r\n\f"))
}

func suppressConsoleCursor() {
	// 1. Disable cursor blinking in kernel fbcon sysfs if available
	_ = os.WriteFile("/sys/class/graphics/fbcon/cursor_blink", []byte("0\n"), 0644)

	// 2. Hide cursor on virtual terminals via VT100 escape sequence
	for _, dev := range []string{"/dev/tty1", "/dev/tty0", "/dev/console"} {
		if f, err := os.OpenFile(dev, os.O_WRONLY, 0); err == nil {
			_, _ = f.Write([]byte("\x1b[?25l"))
			_ = f.Close()
		}
	}
}

func restoreConsoleCursor() {
	for _, dev := range []string{"/dev/tty1", "/dev/tty0", "/dev/console"} {
		if f, err := os.OpenFile(dev, os.O_WRONLY, 0); err == nil {
			_, _ = f.Write([]byte("\x1b[?25h"))
			_ = f.Close()
		}
	}
	_ = os.WriteFile("/sys/class/graphics/fbcon/cursor_blink", []byte("1\n"), 0644)
}

func main() {
	mockPrinter := flag.Bool("mock-printer", false, "Run in mock printer mode on local loopback without physical device")
	previewPNG := flag.String("preview-png", "", "Path to write preview PNG instead of writing to framebuffer")
	cloudURL := flag.String("cloud-url", "ws://127.0.0.1:8080/ws", "Cloud WebSocket hub endpoint")
	kioskID := flag.String("kiosk-id", "kiosk-001", "Kiosk identifier")
	kioskSecret := flag.String("kiosk-secret", "", "[Deprecated] Secret credential for kiosk authentication; prefer XEROXGO_KIOSK_SECRET env or protected credentials file")
	credentialsFile := flag.String("credentials-file", "", "Path to protected credentials file containing kiosk secret (defaults to XEROXGO_CREDENTIALS_FILE env or /etc/xeroxgo/kiosk.secret)")
	backendURL := flag.String("backend-url", "", "HTTP base URL for cloud backend REST API (defaults to XEROXGO_BACKEND_URL env or derived from -cloud-url)")
	stateDB := flag.String("state-db", "/var/kiosk/state.db", "SQLite state database path")
	downloadDir := flag.String("download-dir", "/var/kiosk/jobs", "Directory for downloaded PDF jobs")
	printerAddr := flag.String("printer-addr", "192.168.99.2:9100", "Physical printer TCP address")
	snmpTarget := flag.String("snmp-target", "", "SNMP target address (defaults to host from -printer-addr)")
	snmpCommunity := flag.String("snmp-community", "public", "SNMP community string for printer monitoring (read-only)")
	snmpPort := flag.Int("snmp-port", 161, "SNMP port (default 161)")
	cupsPrinter := flag.String("cups-printer", "", "CUPS queue name (e.g. DCPT310). When set, jobs are sent via CUPS instead of raw TCP port 9100")
	cupsQueue := flag.String("cups-queue", "", "Alias for -cups-printer")
	printerModel := flag.String("printer-model", "", "Printer model name displayed on screen (default: Brother DCP-T310 for CUPS, Brother HL-L5210DN for PJL)")
	completedTimeout := flag.Duration("completed-timeout", 7*time.Second, "Duration to display completion screen before returning to idle QR screen")
	framebuffer := flag.String("framebuffer", "/dev/fb0", "Linux framebuffer device path")
	screenRes := flag.String("screen-res", "", "Override screen resolution WIDTHxHEIGHT (e.g. 1280x720). Defaults to auto-detect")
	screenBPP := flag.Int("screen-bpp", 0, "Override screen bits per pixel (e.g. 16 or 32). Defaults to auto-detect")
	idleQRContent := flag.String("idle-qr-content", "", "URL or content rendered in idle screen QR code")
	idleQR := flag.String("idle-qr", "", "Alias for -idle-qr-content")
	adsCachePath := flag.String("ads-cache-path", "/var/kiosk/ads_cache.json", "Local cache file path for promotional ads")
	caCert := flag.String("ca-cert", "", "Path to custom trusted CA certificate PEM file for TLS verification (defaults to XEROXGO_CA_CERT env)")
	flag.Parse()

	if *cupsPrinter == "" && *cupsQueue != "" {
		*cupsPrinter = *cupsQueue
	}
	if *idleQRContent == "" && *idleQR != "" {
		*idleQRContent = *idleQR
	}

	if strings.TrimSpace(*kioskID) == "" {
		log.Fatalf("Fatal: --kiosk-id cannot be empty")
	}

	if *idleQRContent == "" {
		u, err := url.Parse(*cloudURL)
		if err == nil && u.Host != "" {
			scheme := "http"
			if u.Scheme == "wss" {
				scheme = "https"
			}
			*idleQRContent = fmt.Sprintf("%s://%s/k/%s", scheme, u.Host, url.PathEscape(*kioskID))
		} else {
			*idleQRContent = "https://xeroxgo-frontend.vercel.app"
		}
	}

	if err := os.MkdirAll(filepath.Dir(*stateDB), 0755); err != nil {
		log.Fatalf("Fatal: failed to create state database directory: %v", err)
	}
	if err := os.MkdirAll(*downloadDir, 0700); err != nil {
		log.Fatalf("Fatal: failed to create download directory: %v", err)
	}

	stateStore, err := state.Open(*stateDB)
	if err != nil {
		log.Fatalf("Fatal: failed to open state database at %s: %v", *stateDB, err)
	}
	defer stateStore.Close()

	var screenSink ui_ipc.Sink
	if *previewPNG != "" {
		if *screenRes != "" {
			parts := strings.Split(*screenRes, "x")
			if len(parts) == 2 {
				if w, err := strconv.Atoi(parts[0]); err == nil && w > 0 {
					if h, err := strconv.Atoi(parts[1]); err == nil && h > 0 {
						ui_ipc.ConfigureTarget(ui_ipc.FramebufferGeometry{Width: w, Height: h, BitsPerPixel: 32, Stride: w * 4})
					}
				}
			}
		}
		if err := os.MkdirAll(filepath.Dir(*previewPNG), 0755); err != nil {
			log.Fatalf("Fatal: failed to create preview png directory: %v", err)
		}
		screenSink = ui_ipc.FilePreviewSink(*previewPNG)
	} else {
		geom := ui_ipc.DetectFramebufferGeometry(*framebuffer)
		if *screenRes != "" {
			parts := strings.Split(*screenRes, "x")
			if len(parts) == 2 {
				if w, err := strconv.Atoi(parts[0]); err == nil && w > 0 {
					geom.Width = w
				}
				if h, err := strconv.Atoi(parts[1]); err == nil && h > 0 {
					geom.Height = h
				}
				bytesPerPixel := geom.BitsPerPixel / 8
				if bytesPerPixel < 1 {
					bytesPerPixel = 2
				}
				geom.Stride = geom.Width * bytesPerPixel
			}
		}
		if *screenBPP > 0 {
			geom.BitsPerPixel = *screenBPP
			bytesPerPixel := geom.BitsPerPixel / 8
			if bytesPerPixel < 1 {
				bytesPerPixel = 2
			}
			geom.Stride = geom.Width * bytesPerPixel
		}
		log.Printf("XeroxGo display: output=%s, resolution=%dx%d, bpp=%d, stride=%d",
			*framebuffer, geom.Width, geom.Height, geom.BitsPerPixel, geom.Stride)
		suppressConsoleCursor()
		ui_ipc.ConfigureTarget(geom)
		screenSink = ui_ipc.FramebufferSinkWithGeometry(*framebuffer, geom)
	}

	var rootWg sync.WaitGroup
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	actualPrinterAddr := *printerAddr
	var mockHealthPoller func(ctx context.Context, estimatedSheets int) contracts.PrinterHealth
	var cleanupMock func()

	if *mockPrinter {
		addr, cleanup, err := startMockPrinter(ctx, *downloadDir, &rootWg)
		if err != nil {
			log.Fatalf("Fatal: failed to start mock printer: %v", err)
		}
		cleanupMock = cleanup
		actualPrinterAddr = addr
		log.Printf("XeroxGo kiosk daemon: mock printer active on %s", actualPrinterAddr)

		mockHealthPoller = func(ctx context.Context, estimatedSheets int) contracts.PrinterHealth {
			return contracts.PrinterHealth{
				Online:          true,
				State:           "IDLE",
				PaperTrayStatus: "OK",
				EstimatedSheets: estimatedSheets,
				UpdatedAt:       time.Now().UTC(),
			}
		}
	}

	effectivePrinterModel := strings.TrimSpace(*printerModel)
	if effectivePrinterModel == "" {
		if strings.TrimSpace(*cupsPrinter) != "" {
			effectivePrinterModel = "Brother DCP-T310"
		} else {
			effectivePrinterModel = "Brother HL-L5210DN"
		}
	}

	// 1. Prefer environment variable XEROXGO_KIOSK_SECRET
	effectiveKioskSecret := strings.TrimSpace(os.Getenv("XEROXGO_KIOSK_SECRET"))

	// 2. Protected configuration / credentials file
	if effectiveKioskSecret == "" {
		credPath := strings.TrimSpace(*credentialsFile)
		if credPath == "" {
			credPath = strings.TrimSpace(os.Getenv("XEROXGO_CREDENTIALS_FILE"))
		}
		if credPath == "" {
			defaultPath := "/etc/xeroxgo/kiosk.secret"
			if _, err := os.Stat(defaultPath); err == nil {
				credPath = defaultPath
			}
		}
		if credPath != "" {
			if secretBytes, err := os.ReadFile(credPath); err == nil {
				effectiveKioskSecret = strings.TrimSpace(string(secretBytes))
			}
		}
	}

	// 3. Fallback: CLI flag (warn against passing secrets on CLI to prevent exposure in ps/history)
	if effectiveKioskSecret == "" && strings.TrimSpace(*kioskSecret) != "" {
		log.Printf("[SECURITY] Notice: Prefer setting XEROXGO_KIOSK_SECRET or using a protected credentials file instead of passing --kiosk-secret via CLI")
		effectiveKioskSecret = strings.TrimSpace(*kioskSecret)
	}

	effectiveBackendURL := strings.TrimSpace(*backendURL)
	if effectiveBackendURL == "" {
		effectiveBackendURL = strings.TrimSpace(os.Getenv("XEROXGO_BACKEND_URL"))
	}
	if effectiveBackendURL == "" {
		u, err := url.Parse(*cloudURL)
		if err == nil && u.Host != "" {
			scheme := "http"
			if u.Scheme == "wss" {
				scheme = "https"
			}
			effectiveBackendURL = fmt.Sprintf("%s://%s", scheme, u.Host)
		} else {
			effectiveBackendURL = "http://127.0.0.1:8080"
		}
	}

	clientCfg := spooler.ClientConfig{
		CloudURL:                 *cloudURL,
		KioskID:                  *kioskID,
		KioskSecret:              effectiveKioskSecret,
		BackendBaseURL:           effectiveBackendURL,
		StateStore:               stateStore,
		PrinterAddr:              actualPrinterAddr,
		SNMPTarget:               *snmpTarget,
		SNMPCommunity:            *snmpCommunity,
		SNMPPort:                 *snmpPort,
		CUPSQueue:                *cupsPrinter,
		PrinterModel:             effectivePrinterModel,
		DownloadDir:              *downloadDir,
		IdleQRContent:            *idleQRContent,
		AdsCachePath:             *adsCachePath,
		CompletedDisplayDuration: *completedTimeout,
		ScreenSink:               screenSink,
		HTTPClient:               nil,
		RootCAPath:               strings.TrimSpace(*caCert),
		MockHealth:               mockHealthPoller,
	}

	client, err := spooler.NewClient(clientCfg)
	if err != nil {
		log.Fatalf("Fatal: failed to create spooler client: %v", err)
	}

	keypad := client.Keypad()
	if keypad != nil {
		go func() {
			_ = keypad.ReadLoop(ctx, os.Stdin)
		}()
	}

	startSystemdWatchdog(ctx, &rootWg)
	_ = sendSystemdNotify("READY=1")

	targetDesc := actualPrinterAddr
	if *cupsPrinter != "" {
		targetDesc = "cups:" + *cupsPrinter
	}
	log.Printf("XeroxGo kiosk daemon started for kiosk %s (cloud=%s, target=%s, model=%s)", *kioskID, *cloudURL, targetDesc, effectivePrinterModel)

	if err := client.Run(ctx); err != nil {
		log.Printf("spooler client exited with error: %v", err)
	}

	cancel()
	if cleanupMock != nil {
		cleanupMock()
	}
	rootWg.Wait()
	restoreConsoleCursor()
	log.Printf("XeroxGo kiosk daemon shut down cleanly.")
}

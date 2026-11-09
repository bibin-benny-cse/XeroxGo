package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"xeroxgo/kiosk-daemon/internal/input"
	"xeroxgo/kiosk-daemon/internal/printer"
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
	return printer.StartDevPrinter(ctx, downloadDir, wg)
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
	devMode := flag.Bool("dev", runtime.GOOS == "windows", "Run in development mode (Windows-friendly defaults, terminal dashboard, simulated printer)")
	devWebPort := flag.Int("dev-web-port", 8085, "Local HTTP port for development screen preview (0 to disable)")
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

	if *devMode {
		ui_ipc.SetDevelopmentMode(true)
		if *stateDB == "/var/kiosk/state.db" {
			*stateDB = filepath.Join("data", "kiosk_state.db")
		}
		if *downloadDir == "/var/kiosk/jobs" {
			*downloadDir = filepath.Join("data", "jobs")
		}
		if *adsCachePath == "/var/kiosk/ads_cache.json" {
			*adsCachePath = filepath.Join("data", "ads_cache.json")
		}
		if !*mockPrinter && *printerAddr == "192.168.99.2:9100" && *cupsPrinter == "" {
			*mockPrinter = true
		}
	}

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
	var stateObserver func(ui_ipc.ScreenState)
	var devDisplay *ui_ipc.DevDisplayAdapter

	if *devMode {
		previewPath := *previewPNG
		if previewPath == "" {
			previewPath = filepath.Join("data", "dev_screen.png")
		}
		var err error
		devDisplay, err = ui_ipc.NewDevDisplayAdapter(ui_ipc.DevDisplayConfig{
			PreviewFile:   previewPath,
			WebPort:       *devWebPort,
			ConsoleWriter: os.Stdout,
		})
		if err != nil {
			log.Fatalf("Fatal: failed to initialize dev display adapter: %v", err)
		}
		screenSink = devDisplay.Sink
		stateObserver = devDisplay.ObserveState
	} else if *previewPNG != "" {
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
		addr, cleanup, err := printer.StartDevPrinter(ctx, *downloadDir, &rootWg)
		if err != nil {
			log.Fatalf("Fatal: failed to start mock printer: %v", err)
		}
		cleanupMock = cleanup
		actualPrinterAddr = addr
		log.Printf("XeroxGo kiosk daemon: simulated printer active on %s (Emulating Brother HL-L5210DN)", actualPrinterAddr)

		mockHealthPoller = printer.DevPrinterHealth
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
		StateObserver:            stateObserver,
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
		if *devMode {
			devKeyboard := input.NewDevKeyboardAdapter(keypad, os.Stdin, os.Stdout)
			go func() {
				_ = devKeyboard.Run(ctx)
			}()
		} else {
			go func() {
				_ = keypad.ReadLoop(ctx, os.Stdin)
			}()
		}
	}

	if !*devMode && runtime.GOOS != "windows" {
		startSystemdWatchdog(ctx, &rootWg)
		_ = sendSystemdNotify("READY=1")
	}

	targetDesc := actualPrinterAddr
	if *cupsPrinter != "" {
		targetDesc = "cups:" + *cupsPrinter
	}

	if *devMode {
		log.Printf("================================================================================")
		log.Printf("         XEROXGO KIOSK DAEMON [DEVELOPMENT MODE (WINDOWS)] ACTIVE                ")
		log.Printf("================================================================================")
		log.Printf("  Kiosk ID:          %s", *kioskID)
		log.Printf("  Cloud Hub:         %s", *cloudURL)
		log.Printf("  Backend REST:      %s", effectiveBackendURL)
		log.Printf("  Simulated Printer: %s", targetDesc)
		if devDisplay != nil && devDisplay.WebAddr() != "" {
			log.Printf("  Screen Preview:    http://%s", devDisplay.WebAddr())
		}
		log.Printf("  Keyboard Input:    Interactive stdin active (type 4-digit PINs, 'cancel')")
		log.Printf("================================================================================")
	} else {
		log.Printf("XeroxGo kiosk daemon started for kiosk %s (cloud=%s, target=%s, model=%s)", *kioskID, *cloudURL, targetDesc, effectivePrinterModel)
	}

	if err := client.Run(ctx); err != nil {
		log.Printf("spooler client exited with error: %v", err)
	}

	cancel()
	if cleanupMock != nil {
		cleanupMock()
	}
	if devDisplay != nil {
		_ = devDisplay.Close()
	}
	rootWg.Wait()
	if !*devMode && runtime.GOOS != "windows" {
		restoreConsoleCursor()
	}
	log.Printf("XeroxGo kiosk daemon shut down cleanly.")
}

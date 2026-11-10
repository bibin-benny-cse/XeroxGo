package spooler

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"xeroxgo/kiosk-daemon/internal/input"
	"xeroxgo/kiosk-daemon/internal/printer"
	"xeroxgo/kiosk-daemon/internal/state"
	"xeroxgo/kiosk-daemon/internal/ui_ipc"
	"xeroxgo/pkg/contracts"
)

type wireMessage struct {
	Type     string                      `json:"type"`
	Job      *contracts.PrintJobCommand  `json:"job,omitempty"`
	Progress *contracts.JobProgressEvent `json:"progress,omitempty"`
	Health   *contracts.PrinterHealth    `json:"health,omitempty"`
	Error    string                      `json:"error,omitempty"`
	Ads      []contracts.AdItem          `json:"ads,omitempty"`
}

// ClientConfig holds the runtime dependencies and parameters for the kiosk spooler client.
type ClientConfig struct {
	CloudURL                 string
	KioskID                  string
	KioskSecret              string
	BackendBaseURL           string
	PaymentPollInterval      time.Duration
	StateStore               *state.Store
	PrinterAddr              string
	SNMPTarget               string
	SNMPCommunity            string
	SNMPPort                 int
	CUPSQueue                string
	PrinterModel             string
	DownloadDir              string
	IdleQRContent            string
	AdsCachePath             string
	IdleCarouselInterval     time.Duration
	CompletedDisplayDuration time.Duration
	ErrorDisplayDuration     time.Duration
	ScreenSink               ui_ipc.Sink
	StateObserver            func(state ui_ipc.ScreenState)
	HTTPClient               *http.Client
	RootCAPath               string
	MinBackoff               time.Duration
	MaxBackoff               time.Duration
	HeartbeatInterval        time.Duration
	MockHealth               func(ctx context.Context, estimatedSheets int) contracts.PrinterHealth
	MockCUPSPrint            func(ctx context.Context, queue string, cmd contracts.PrintJobCommand, pdfPath string, onPage func(sheetsCompleted int)) error
}

// Client connects to the cloud WebSocket hub, handles heartbeats, downloads and verifies PDFs,
// streams print jobs via PJL, renders status screens, and recovers incomplete jobs.
type Client struct {
	cfg           ClientConfig
	normalizedURL string
	workflow      *PaymentWorkflow
	keypadCtrl    *input.Controller

	connMu sync.Mutex
	conn   *websocket.Conn
	sendMu sync.Mutex

	jobQueue   chan contracts.PrintJobCommand
	activeMu   sync.Mutex
	activeJobs map[string]bool

	idleMu    sync.Mutex
	idleTimer *time.Timer

	minBackoff        time.Duration
	maxBackoff        time.Duration
	heartbeatInterval time.Duration
	maxDownloadBytes  int64

	lastRenderErrMu sync.Mutex
	lastRenderErr   error

	renderHistoryMu sync.Mutex
	renderHistory   []ui_ipc.ScreenState

	currentModeMu sync.Mutex
	currentMode   ui_ipc.ScreenMode
	adSlideIndex  int

	renderMu sync.Mutex
	canvas   *image.RGBA

	healthPoller func(ctx context.Context, estimatedSheets int) contracts.PrinterHealth
}

// BuildTLSHTTPClient constructs an *http.Client enforcing TLS 1.2+ and standard certificate/hostname
// verification. If rootCAPath is provided, it loads the specified CA certificates into the client trust pool;
// otherwise, it uses the host system's trusted root CA pool. It strictly enforces standard verification
// and NEVER sets InsecureSkipVerify to true.
func BuildTLSHTTPClient(rootCAPath string, timeout time.Duration) (*http.Client, error) {
	tlsConfig := &tls.Config{
		MinVersion: tls.VersionTLS12,
		// Standard certificate and hostname verification are strictly enabled by default.
		// InsecureSkipVerify is NEVER set to true.
	}

	if rootCAPath != "" {
		pemData, err := os.ReadFile(rootCAPath)
		if err != nil {
			return nil, fmt.Errorf("failed to read root CA certificate from %s: %w", rootCAPath, err)
		}
		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(pemData) {
			return nil, fmt.Errorf("failed to parse PEM root CA certificate from %s", rootCAPath)
		}
		tlsConfig.RootCAs = pool
	}

	transport := &http.Transport{
		TLSClientConfig: tlsConfig,
		Proxy:           http.ProxyFromEnvironment,
	}

	return &http.Client{
		Transport: transport,
		Timeout:   timeout,
	}, nil
}

// NewClient validates the configuration and initializes a new spooler client.
func NewClient(cfg ClientConfig) (*Client, error) {
	if strings.TrimSpace(cfg.CloudURL) == "" {
		return nil, errors.New("client config: cloud URL cannot be empty")
	}
	u, err := url.Parse(cfg.CloudURL)
	if err != nil {
		return nil, fmt.Errorf("client config: invalid cloud URL: %w", err)
	}
	if u.Scheme != "ws" && u.Scheme != "wss" {
		return nil, fmt.Errorf("client config: cloud URL scheme must be ws or wss, got %q", u.Scheme)
	}
	if u.Host == "" || u.Hostname() == "" {
		return nil, errors.New("client config: cloud URL missing host")
	}

	if strings.TrimSpace(cfg.KioskID) == "" {
		return nil, errors.New("client config: kiosk ID cannot be empty")
	}
	if cfg.StateStore == nil {
		return nil, errors.New("client config: state store cannot be nil")
	}
	if strings.TrimSpace(cfg.PrinterAddr) == "" && strings.TrimSpace(cfg.CUPSQueue) == "" {
		return nil, errors.New("client config: either printer address or CUPS queue must be provided")
	}
	if strings.TrimSpace(cfg.DownloadDir) == "" {
		return nil, errors.New("client config: download directory cannot be empty")
	}
	if strings.TrimSpace(cfg.IdleQRContent) == "" {
		return nil, errors.New("client config: idle QR content cannot be empty")
	}
	if cfg.ScreenSink == nil {
		return nil, errors.New("client config: screen sink cannot be nil")
	}

	q := u.Query()
	q.Set("kiosk_id", cfg.KioskID)
	u.RawQuery = q.Encode()
	normalizedURL := u.String()

	httpClient := cfg.HTTPClient
	if httpClient == nil {
		rootCA := strings.TrimSpace(cfg.RootCAPath)
		if rootCA == "" {
			rootCA = strings.TrimSpace(os.Getenv("XEROXGO_CA_CERT"))
		}
		if rootCA == "" {
			rootCA = strings.TrimSpace(os.Getenv("XEROXGO_ROOT_CA"))
		}
		builtClient, err := BuildTLSHTTPClient(rootCA, 60*time.Second)
		if err != nil {
			return nil, fmt.Errorf("failed to configure TLS HTTP client: %w", err)
		}
		httpClient = builtClient
	}
	cfg.HTTPClient = httpClient

	poller := cfg.MockHealth
	if poller == nil {
		targetHost := strings.TrimSpace(cfg.SNMPTarget)
		if targetHost == "" {
			if h, _, err := net.SplitHostPort(cfg.PrinterAddr); err == nil && h != "" {
				targetHost = h
			} else if strings.TrimSpace(cfg.PrinterAddr) != "" {
				targetHost = strings.TrimSpace(cfg.PrinterAddr)
			}
		}

		snmpCfg := printer.DefaultSNMPConfig()
		if targetHost != "" {
			snmpCfg.Target = targetHost
		}
		if cfg.SNMPPort > 0 {
			snmpCfg.Port = cfg.SNMPPort
		}
		if cfg.SNMPCommunity != "" {
			snmpCfg.Community = cfg.SNMPCommunity
		}

		cupsQ := strings.TrimSpace(cfg.CUPSQueue)
		poller = func(ctx context.Context, estimatedSheets int) contracts.PrinterHealth {
			return printer.PollCombinedHealth(ctx, nil, cupsQ, snmpCfg, estimatedSheets)
		}
	}

	if cfg.CompletedDisplayDuration <= 0 {
		cfg.CompletedDisplayDuration = 7 * time.Second
	}
	if cfg.ErrorDisplayDuration <= 0 {
		cfg.ErrorDisplayDuration = 10 * time.Second
	}

	if cfg.AdsCachePath == "" {
		cfg.AdsCachePath = "/var/kiosk/ads_cache.json"
	}
	if cached, err := loadAdsCache(cfg.AdsCachePath); err == nil && len(cached) > 0 {
		log.Printf("[SPOOLER] Loaded %d promotional ads from local cache %s", len(cached), cfg.AdsCachePath)
		ui_ipc.ConfigureAds(cached)
	}

	backendURL := strings.TrimSpace(cfg.BackendBaseURL)
	if backendURL == "" {
		scheme := "http"
		if u.Scheme == "wss" {
			scheme = "https"
		}
		backendURL = fmt.Sprintf("%s://%s", scheme, u.Host)
	}
	cfg.BackendBaseURL = backendURL

	var wf *PaymentWorkflow
	if strings.TrimSpace(cfg.KioskSecret) != "" {
		wf, _ = NewPaymentWorkflow(cfg.BackendBaseURL, cfg.KioskID, cfg.KioskSecret, cfg.HTTPClient, cfg.PaymentPollInterval)
	}

	minBackoff := cfg.MinBackoff
	if minBackoff <= 0 {
		minBackoff = 1 * time.Second
	}
	maxBackoff := cfg.MaxBackoff
	if maxBackoff <= 0 {
		maxBackoff = 30 * time.Second
	}
	hbInterval := cfg.HeartbeatInterval
	if hbInterval <= 0 {
		hbInterval = 15 * time.Second
	}

	client := &Client{
		cfg:               cfg,
		normalizedURL:     normalizedURL,
		workflow:          wf,
		jobQueue:          make(chan contracts.PrintJobCommand, 16),
		activeJobs:        make(map[string]bool),
		minBackoff:        minBackoff,
		maxBackoff:        maxBackoff,
		heartbeatInterval: hbInterval,
		maxDownloadBytes:  250 * 1024 * 1024,
		healthPoller:      poller,
	}

	client.keypadCtrl = input.NewController(input.Config{
		OnChange: func(currentPIN string) {
			_ = client.renderScreen(ui_ipc.ScreenState{
				Mode:    ui_ipc.ModePINEntry,
				PIN:     currentPIN,
				Message: "Enter your 4-digit pickup PIN",
			})
		},
		OnCancel: func() {
			_ = client.renderScreen(ui_ipc.ScreenState{
				Mode:      ui_ipc.ModeIdle,
				QRContent: client.cfg.IdleQRContent,
				Message:   "Ready to print",
			})
		},
		OnSubmit: func(ctx context.Context, pin string) error {
			session := NewKioskWorkflowSession(client)
			return session.ExecuteWorkflow(ctx, pin)
		},
	})

	go client.fetchInitialAds()

	return client, nil
}

// Keypad returns the customer input controller for physical keypad, keyboard, or touchscreen interaction.
func (c *Client) Keypad() *input.Controller {
	return c.keypadCtrl
}

// Run executes the spooler client loops until the context is canceled.
func (c *Client) Run(ctx context.Context) error {
	log.Printf("[DIAG-SPOOLER] Client.Run entered")
	defer func() {
		log.Printf("[DIAG-SPOOLER] Client.Run exiting")
		c.idleMu.Lock()
		if c.idleTimer != nil {
			c.idleTimer.Stop()
			c.idleTimer = nil
		}
		c.idleMu.Unlock()
	}()

	var wg sync.WaitGroup

	// 1. Initial idle screen transition (rendered asynchronously so screen rendering does not delay network connection)
	wg.Add(1)
	go func() {
		defer wg.Done()
		c.renderInitialIdleIfUnset(ctx)
	}()

	// 2. Startup recovery check: handle interrupted PRINTING state safely
	recoveryCtx, recCancel := context.WithTimeout(context.Background(), 3*time.Second)
	incomplete, err := c.cfg.StateStore.GetIncompleteJob(recoveryCtx)
	log.Printf("[DIAG-SPOOLER] GetIncompleteJob completed (incomplete=%v, err=%v)", incomplete != nil, err)
	if err == nil && incomplete != nil {
		if incomplete.Status == contracts.JobStatusPrinting {
			// Job was interrupted mid-print! Do NOT automatically reprint an ambiguous job to avoid double printing.
			log.Printf("[SPOOLER] Job %s was interrupted during PRINTING (%d pages printed). Marking PAUSED_ERROR to prevent ambiguous duplicate print.",
				incomplete.Command.JobID, incomplete.PagesPrinted)
			pauseEvt := contracts.JobProgressEvent{
				JobID:         incomplete.Command.JobID,
				KioskID:       incomplete.Command.KioskID,
				Status:        contracts.JobStatusPausedError,
				PagesPrinted:  incomplete.PagesPrinted,
				SheetsPrinted: incomplete.SheetsPrinted,
				ErrorReason:   "interrupted during printing: stopped to prevent ambiguous duplicate print",
				Timestamp:     time.Now().UTC(),
			}
			_ = c.cfg.StateStore.UpdateProgress(recoveryCtx, pauseEvt)
		} else {
			c.activeMu.Lock()
			c.activeJobs[incomplete.Command.JobID] = true
			c.activeMu.Unlock()
			select {
			case c.jobQueue <- incomplete.Command:
			default:
			}
		}
	}
	recCancel()

	// 3. Start sequential job processing worker
	log.Printf("[DIAG-SPOOLER] Starting jobWorker")
	wg.Add(1)
	go func() {
		defer wg.Done()
		c.jobWorker(ctx)
	}()

	// 3b. Start idle carousel ticker
	if c.cfg.IdleCarouselInterval == 0 {
		c.cfg.IdleCarouselInterval = 5 * time.Second
	}
	if c.cfg.IdleCarouselInterval > 0 {
		log.Printf("[DIAG-SPOOLER] Starting idleCarouselLoop")
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.idleCarouselLoop(ctx)
		}()
	}

	// 4. Connection & reconnection loop
	log.Printf("[DIAG-SPOOLER] Entering connection loop (minBackoff=%v, maxBackoff=%v, url=%s)", c.minBackoff, c.maxBackoff, c.normalizedURL)
	delay := c.minBackoff
	for {
		select {
		case <-ctx.Done():
			log.Printf("[DIAG-SPOOLER] ctx.Done() triggered in connection loop: %v", ctx.Err())
			wg.Wait()
			return nil
		default:
		}

		dialOpts := &websocket.DialOptions{
			HTTPClient: c.cfg.HTTPClient,
		}
		log.Printf("[SPOOLER] Attempting WebSocket dial to %s (delay=%v)...", c.normalizedURL, delay)
		conn, resp, err := websocket.Dial(ctx, c.normalizedURL, dialOpts)
		if err != nil {
			var respStatus string
			if resp != nil {
				respStatus = resp.Status
			}
			log.Printf("[SPOOLER] WebSocket dial failed for %s (status=%s): %v", c.normalizedURL, respStatus, err)
			select {
			case <-ctx.Done():
				wg.Wait()
				return nil
			case <-time.After(delay):
				delay *= 2
				if delay > c.maxBackoff {
					delay = c.maxBackoff
				}
				continue
			}
		}

		log.Printf("[SPOOLER] WebSocket connected successfully to %s", c.normalizedURL)
		// Connected successfully: reset backoff delay
		delay = c.minBackoff
		c.handleConnection(ctx, conn)
	}
}

func (c *Client) handleConnection(ctx context.Context, conn *websocket.Conn) {
	conn.SetReadLimit(1024 * 1024)

	c.connMu.Lock()
	c.conn = conn
	c.connMu.Unlock()

	connCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	defer func() {
		c.connMu.Lock()
		if c.conn == conn {
			c.conn = nil
		}
		c.connMu.Unlock()
		_ = conn.Close(websocket.StatusNormalClosure, "disconnecting")
	}()

	// Start heartbeat and telemetry loop
	var hbWg sync.WaitGroup
	hbWg.Add(1)
	go func() {
		defer hbWg.Done()
		c.heartbeatLoop(connCtx, conn)
	}()

	// Reader loop
	for {
		var msg wireMessage
		err := wsjson.Read(connCtx, conn, &msg)
		if err != nil {
			break
		}
		if msg.Type == "job" && msg.Job != nil {
			c.handleIncomingJob(*msg.Job)
		} else if msg.Type == "sync_ads" && len(msg.Ads) > 0 {
			c.handleSyncAds(msg.Ads)
		}
	}

	cancel()
	hbWg.Wait()
}

func (c *Client) heartbeatLoop(ctx context.Context, conn *websocket.Conn) {
	ticker := time.NewTicker(c.heartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// Send heartbeat
			_ = c.sendWire(ctx, conn, wireMessage{Type: "heartbeat"})

			// Relay printer health
			sheets, _ := c.cfg.StateStore.GetEstimatedSheets(ctx)
			h := c.healthPoller(ctx, sheets)
			_ = c.sendWire(ctx, conn, wireMessage{Type: "health", Health: &h})
		}
	}
}

func (c *Client) sendWire(ctx context.Context, conn *websocket.Conn, msg wireMessage) error {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()

	c.connMu.Lock()
	activeConn := c.conn
	c.connMu.Unlock()

	if activeConn == nil || activeConn != conn {
		return errors.New("websocket connection not active")
	}

	return wsjson.Write(ctx, activeConn, msg)
}

func (c *Client) sendProgress(ctx context.Context, event contracts.JobProgressEvent) {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()

	c.connMu.Lock()
	conn := c.conn
	c.connMu.Unlock()

	if conn == nil {
		return
	}

	_ = wsjson.Write(ctx, conn, wireMessage{Type: "progress", Progress: &event})
}

func (c *Client) handleIncomingJob(cmd contracts.PrintJobCommand) {
	if cmd.KioskID != c.cfg.KioskID {
		return
	}
	if strings.TrimSpace(cmd.JobID) == "" {
		return
	}
	u, err := url.Parse(cmd.DownloadURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil {
		return
	}
	if len(cmd.SHA256Checksum) != 64 {
		return
	}
	if _, err := hex.DecodeString(cmd.SHA256Checksum); err != nil {
		return
	}
	if cmd.TotalPages < 1 || cmd.TotalSheets < 1 || cmd.Copies < 1 || cmd.StartFromPage < 1 || cmd.StartFromPage > cmd.TotalPages {
		return
	}

	c.activeMu.Lock()
	defer c.activeMu.Unlock()

	// 1. Simultaneous notification guard: if already in queue or processing, drop duplicate immediately
	if c.activeJobs[cmd.JobID] {
		return
	}

	// 2. State store check: verify job hasn't completed or been interrupted previously
	if c.cfg.StateStore != nil {
		existingJob, err := c.cfg.StateStore.GetJob(context.Background(), cmd.JobID)
		if err == nil && existingJob != nil {
			if existingJob.Status == contracts.JobStatusCompleted {
				log.Printf("[SPOOLER] Job %s has already completed on kiosk %s; skipping duplicate print", cmd.JobID, c.cfg.KioskID)
				return
			}
			if existingJob.Status == contracts.JobStatusPausedError && (existingJob.PagesPrinted > 0 || strings.Contains(existingJob.ErrorReason, "interrupted")) {
				log.Printf("[SPOOLER] Job %s was previously interrupted (%d pages printed); skipping automatic reprint to avoid ambiguous duplicate", cmd.JobID, existingJob.PagesPrinted)
				return
			}
		}
	}

	if len(c.activeJobs) > 0 {
		c.connMu.Lock()
		conn := c.conn
		c.connMu.Unlock()
		if conn != nil {
			_ = c.sendWire(context.Background(), conn, wireMessage{
				Type:  "error",
				Error: "kiosk is busy processing an existing job",
			})
		}
		return
	}
	c.activeJobs[cmd.JobID] = true
	log.Printf("[SPOOLER] Queued print job %s (pages=%d, sheets=%d, copies=%d, duplex=%s)", cmd.JobID, cmd.TotalPages, cmd.TotalSheets, cmd.Copies, cmd.Duplex)

	select {
	case c.jobQueue <- cmd:
	default:
		delete(c.activeJobs, cmd.JobID)
		c.connMu.Lock()
		conn := c.conn
		c.connMu.Unlock()
		if conn != nil {
			_ = c.sendWire(context.Background(), conn, wireMessage{
				Type:  "error",
				Error: "kiosk job queue is full",
			})
		}
	}
}

func (c *Client) jobWorker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case cmd := <-c.jobQueue:
			c.processJob(ctx, cmd)
			c.activeMu.Lock()
			delete(c.activeJobs, cmd.JobID)
			c.activeMu.Unlock()
		}
	}
}

func (c *Client) processJob(ctx context.Context, cmd contracts.PrintJobCommand) {
	c.idleMu.Lock()
	if c.idleTimer != nil {
		c.idleTimer.Stop()
		c.idleTimer = nil
	}
	c.idleMu.Unlock()

	log.Printf("[SPOOLER] Processing job %s: downloading from %s", cmd.JobID, cmd.DownloadURL)
	// 1. Initial screen transition to DOWNLOADING
	_ = c.renderScreen(ui_ipc.ScreenState{
		Mode:            ui_ipc.ModeDownloading,
		ProgressPercent: 0,
		Message:         "Downloading document...",
		Detail:          "Job ID: " + cmd.JobID,
	})

	// 2. Persist initial job record if not already stored
	savedJob, _ := c.cfg.StateStore.GetIncompleteJob(ctx)
	if savedJob == nil || savedJob.Command.JobID != cmd.JobID {
		_ = c.cfg.StateStore.SaveJob(ctx, cmd, contracts.JobStatusPausedError)
	}

	// 3. Download & verify PDF
	h := sha256.Sum256([]byte(cmd.JobID))
	finalName := hex.EncodeToString(h[:]) + ".pdf"
	finalPath := filepath.Join(c.cfg.DownloadDir, finalName)

	needDownload := true
	if existingData, err := os.ReadFile(finalPath); err == nil {
		existingH := sha256.Sum256(existingData)
		existingSum := hex.EncodeToString(existingH[:])
		if existingSum == strings.ToLower(cmd.SHA256Checksum) {
			needDownload = false
		}
	}

	if needDownload {
		if err := c.downloadAndVerify(ctx, cmd, finalPath); err != nil {
			c.handleJobError(ctx, cmd, 0, 0, fmt.Sprintf("download failed: %v", err))
			return
		}
	}

	// Screen: Download 100%
	_ = c.renderScreen(ui_ipc.ScreenState{
		Mode:            ui_ipc.ModeDownloading,
		ProgressPercent: 100,
		Message:         "Download verified",
		Detail:          "Job ID: " + cmd.JobID,
	})

	// 4. Update progress: SPOOLING
	pagesPrinted := 0
	sheetsPrinted := 0
	if savedJob != nil && savedJob.Command.JobID == cmd.JobID {
		pagesPrinted = savedJob.PagesPrinted
		sheetsPrinted = savedJob.SheetsPrinted
	}

	spoolEvt := contracts.JobProgressEvent{
		JobID:         cmd.JobID,
		KioskID:       cmd.KioskID,
		Status:        contracts.JobStatusSpooling,
		PagesPrinted:  pagesPrinted,
		SheetsPrinted: sheetsPrinted,
		Timestamp:     time.Now().UTC(),
	}
	_ = c.cfg.StateStore.UpdateProgress(ctx, spoolEvt)
	c.sendProgress(ctx, spoolEvt)

	// 5. Calculate start page and check paper supply
	startPage := pagesPrinted + 1
	if startPage < cmd.StartFromPage {
		startPage = cmd.StartFromPage
	}

	if startPage > cmd.TotalPages {
		c.completeJob(ctx, cmd)
		return
	}

	remainingPages := cmd.TotalPages - startPage + 1
	remainingSheets := 0
	if cmd.Duplex == contracts.DuplexSimplex {
		remainingSheets = remainingPages * cmd.Copies
	} else {
		remainingSheets = ((remainingPages + 1) / 2) * cmd.Copies
	}

	estimatedSheets, err := c.cfg.StateStore.GetEstimatedSheets(ctx)
	if err != nil || estimatedSheets < remainingSheets {
		c.handleJobError(ctx, cmd, pagesPrinted, sheetsPrinted, "paper tray supply insufficient for remaining print job")
		return
	}

	// 6. Page Slicing if resuming partway
	printPath := finalPath
	streamCmd := cmd
	if startPage > 1 {
		slicedPath := filepath.Join(c.cfg.DownloadDir, fmt.Sprintf("slice_%s_%d.pdf", finalName, startPage))
		if err := printer.SlicePDFForResume(ctx, finalPath, slicedPath, startPage); err != nil {
			c.handleJobError(ctx, cmd, pagesPrinted, sheetsPrinted, fmt.Sprintf("slicing failed: %v", err))
			return
		}
		defer os.Remove(slicedPath)
		printPath = slicedPath

		streamCmd.StartFromPage = 1
		streamCmd.TotalPages = remainingPages
		streamCmd.TotalSheets = remainingSheets
	}

	// 7. Update progress: PRINTING
	printEvt := contracts.JobProgressEvent{
		JobID:         cmd.JobID,
		KioskID:       cmd.KioskID,
		Status:        contracts.JobStatusPrinting,
		PagesPrinted:  pagesPrinted,
		SheetsPrinted: sheetsPrinted,
		Timestamp:     time.Now().UTC(),
	}
	_ = c.cfg.StateStore.UpdateProgress(ctx, printEvt)
	c.sendProgress(ctx, printEvt)

	_ = c.renderScreen(ui_ipc.ScreenState{
		Mode:            ui_ipc.ModePrinting,
		CurrentPage:     pagesPrinted,
		TotalPages:      cmd.TotalPages,
		ProgressPercent: (pagesPrinted * 100) / cmd.TotalPages,
		Message:         "Printing document...",
		Detail:          "Job ID: " + cmd.JobID,
	})

	// 8. Stream PDF via PJL
	f, err := os.Open(printPath)
	if err != nil {
		c.handleJobError(ctx, cmd, pagesPrinted, sheetsPrinted, fmt.Sprintf("failed to open PDF for print: %v", err))
		return
	}
	defer f.Close()

	lastReportedStreamSheets := 0
	initialSheetsOffset := sheetsPrinted

	jobCtx, cancelJob := context.WithCancel(ctx)
	defer cancelJob()

	onPage := func(streamSheetsCompleted int) {
		if streamSheetsCompleted > lastReportedStreamSheets {
			delta := streamSheetsCompleted - lastReportedStreamSheets
			lastReportedStreamSheets = streamSheetsCompleted

			if err := c.cfg.StateStore.DecrementSheets(jobCtx, delta); err != nil {
				cancelJob()
				return
			}
			sheetsPrinted = initialSheetsOffset + streamSheetsCompleted

			if cmd.Duplex == contracts.DuplexSimplex {
				pagesPrinted = (startPage - 1) + streamSheetsCompleted/cmd.Copies
			} else {
				pagesPrinted = (startPage - 1) + (streamSheetsCompleted*2)/cmd.Copies
			}

			if pagesPrinted > cmd.TotalPages {
				pagesPrinted = cmd.TotalPages
			}
			if sheetsPrinted > cmd.TotalSheets {
				sheetsPrinted = cmd.TotalSheets
			}

			evt := contracts.JobProgressEvent{
				JobID:         cmd.JobID,
				KioskID:       cmd.KioskID,
				Status:        contracts.JobStatusPrinting,
				PagesPrinted:  pagesPrinted,
				SheetsPrinted: sheetsPrinted,
				Timestamp:     time.Now().UTC(),
			}
			if err := c.cfg.StateStore.UpdateProgress(jobCtx, evt); err != nil {
				cancelJob()
				return
			}
			c.sendProgress(jobCtx, evt)

			_ = c.renderScreen(ui_ipc.ScreenState{
				Mode:            ui_ipc.ModePrinting,
				CurrentPage:     pagesPrinted,
				TotalPages:      cmd.TotalPages,
				ProgressPercent: (pagesPrinted * 100) / cmd.TotalPages,
				Message:         "Printing document...",
				Detail:          "Job ID: " + cmd.JobID,
			})
		}
	}

	var printErr error
	if strings.TrimSpace(c.cfg.CUPSQueue) != "" {
		log.Printf("[SPOOLER] Sending job %s to CUPS queue %s", cmd.JobID, c.cfg.CUPSQueue)
		if c.cfg.MockCUPSPrint != nil {
			printErr = c.cfg.MockCUPSPrint(jobCtx, c.cfg.CUPSQueue, streamCmd, printPath, onPage)
		} else {
			printErr = printer.PrintPDFWithCUPS(jobCtx, c.cfg.CUPSQueue, streamCmd, printPath, onPage)
		}
	} else {
		log.Printf("[SPOOLER] Streaming job %s to PJL printer %s", cmd.JobID, c.cfg.PrinterAddr)
		printErr = printer.StreamPDFWithPJL(jobCtx, c.cfg.PrinterAddr, streamCmd, f, onPage)
	}

	if printErr != nil {
		c.handleJobError(ctx, cmd, pagesPrinted, sheetsPrinted, fmt.Sprintf("printing failed: %v", printErr))
		return
	}

	// Reconcile remaining sheets if not already deducted
	if sheetsPrinted < cmd.TotalSheets {
		remainingToDeduct := cmd.TotalSheets - sheetsPrinted
		_ = c.cfg.StateStore.DecrementSheets(ctx, remainingToDeduct)
	}

	// 9. Complete Job
	c.completeJob(ctx, cmd)
}

func (c *Client) completeJob(ctx context.Context, cmd contracts.PrintJobCommand) {
	log.Printf("[SPOOLER] Job %s successfully printed (%d pages, %d sheets)", cmd.JobID, cmd.TotalPages, cmd.TotalSheets)
	evt := contracts.JobProgressEvent{
		JobID:         cmd.JobID,
		KioskID:       cmd.KioskID,
		Status:        contracts.JobStatusCompleted,
		PagesPrinted:  cmd.TotalPages,
		SheetsPrinted: cmd.TotalSheets,
		Timestamp:     time.Now().UTC(),
	}
	_ = c.cfg.StateStore.UpdateProgress(ctx, evt)
	c.sendProgress(ctx, evt)

	_ = c.renderScreen(ui_ipc.ScreenState{
		Mode:            ui_ipc.ModeCompleted,
		CurrentPage:     cmd.TotalPages,
		TotalPages:      cmd.TotalPages,
		ProgressPercent: 100,
		Message:         "Print complete",
		Detail:          "Job ID: " + cmd.JobID,
	})

	c.scheduleResetToIdle(c.cfg.CompletedDisplayDuration)
}

func (c *Client) handleJobError(ctx context.Context, cmd contracts.PrintJobCommand, pagesPrinted, sheetsPrinted int, reason string) {
	log.Printf("[SPOOLER] Job %s error: %s", cmd.JobID, reason)
	evt := contracts.JobProgressEvent{
		JobID:         cmd.JobID,
		KioskID:       cmd.KioskID,
		Status:        contracts.JobStatusPausedError,
		PagesPrinted:  pagesPrinted,
		SheetsPrinted: sheetsPrinted,
		ErrorReason:   reason,
		Timestamp:     time.Now().UTC(),
	}
	_ = c.cfg.StateStore.UpdateProgress(ctx, evt)
	c.sendProgress(ctx, evt)

	_ = c.renderScreen(ui_ipc.ScreenState{
		Mode:    ui_ipc.ModeError,
		Message: reason,
		Detail:  "Job ID: " + cmd.JobID,
	})

	c.scheduleResetToIdle(c.cfg.ErrorDisplayDuration)
}

func (c *Client) scheduleResetToIdle(d time.Duration) {
	if d <= 0 {
		return
	}
	c.idleMu.Lock()
	defer c.idleMu.Unlock()

	if c.idleTimer != nil {
		c.idleTimer.Stop()
	}

	c.idleTimer = time.AfterFunc(d, func() {
		c.activeMu.Lock()
		activeCount := len(c.activeJobs)
		c.activeMu.Unlock()

		if activeCount == 0 {
			log.Printf("[SPOOLER] Resetting screen to idle state for next user")
			_ = c.renderScreen(ui_ipc.ScreenState{
				Mode:      ui_ipc.ModeIdle,
				QRContent: c.cfg.IdleQRContent,
				Message:   "Ready to print",
			})
		}
	})
}

func (c *Client) downloadAndVerify(ctx context.Context, cmd contracts.PrintJobCommand, finalPath string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cmd.DownloadURL, nil)
	if err != nil {
		return fmt.Errorf("create download request: %w", err)
	}

	resp, err := c.cfg.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("execute download request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download returned non-200 status: %d", resp.StatusCode)
	}

	limit := c.maxDownloadBytes
	if limit <= 0 {
		limit = 250 * 1024 * 1024
	}
	if resp.ContentLength > limit {
		return fmt.Errorf("declared content length %d exceeds maximum limit of %d bytes", resp.ContentLength, limit)
	}

	tmpFile, err := os.CreateTemp(c.cfg.DownloadDir, "job_download_*.tmp")
	if err != nil {
		return fmt.Errorf("create temp download file: %w", err)
	}
	tmpName := tmpFile.Name()

	cleanup := true
	defer func() {
		if cleanup {
			_ = tmpFile.Close()
			_ = os.Remove(tmpName)
		}
	}()

	hasher := sha256.New()
	limitedReader := io.LimitReader(resp.Body, limit+1)

	buf := make([]byte, 32*1024)
	var written int64
	lastPct := 0

	for {
		nr, rerr := limitedReader.Read(buf)
		if nr > 0 {
			written += int64(nr)
			if written > limit {
				return fmt.Errorf("downloaded bytes %d exceeded limit of %d bytes", written, limit)
			}
			hasher.Write(buf[:nr])
			if _, werr := tmpFile.Write(buf[:nr]); werr != nil {
				return fmt.Errorf("write temp file: %w", werr)
			}

			if resp.ContentLength > 0 {
				pct := int((written * 100) / resp.ContentLength)
				if pct > 100 {
					pct = 100
				}
				if (pct-lastPct >= 10 || pct == 100) && pct > lastPct {
					lastPct = pct
					_ = c.renderScreen(ui_ipc.ScreenState{
						Mode:            ui_ipc.ModeDownloading,
						ProgressPercent: pct,
						Message:         "Downloading document...",
						Detail:          "Job ID: " + cmd.JobID,
					})
				}
			}
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				break
			}
			return fmt.Errorf("streaming download failed: %w", rerr)
		}
	}

	calcSum := hex.EncodeToString(hasher.Sum(nil))
	if calcSum != strings.ToLower(cmd.SHA256Checksum) {
		return fmt.Errorf("checksum mismatch: got %s, want %s", calcSum, cmd.SHA256Checksum)
	}

	if err := tmpFile.Sync(); err != nil {
		return fmt.Errorf("sync temp file: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}

	if err := os.Rename(tmpName, finalPath); err != nil {
		return fmt.Errorf("rename to final path: %w", err)
	}

	cleanup = false
	return nil
}

// renderInitialIdleIfUnset renders the initial idle screen only if no screen transition
// has occurred yet and ctx has not been canceled. The mode check and render execute
// atomically under renderMu so a late startup render can never overwrite a newer screen.
func (c *Client) renderInitialIdleIfUnset(ctx context.Context) {
	c.renderMu.Lock()
	defer c.renderMu.Unlock()

	if ctx.Err() != nil {
		return
	}

	c.currentModeMu.Lock()
	alreadyRendered := c.currentMode != ""
	c.currentModeMu.Unlock()
	if alreadyRendered {
		return
	}

	_ = c.renderScreenLocked(ui_ipc.ScreenState{
		Mode:      ui_ipc.ModeIdle,
		QRContent: c.cfg.IdleQRContent,
		Message:   "Ready to print",
	})
}

func (c *Client) renderScreen(state ui_ipc.ScreenState) error {
	c.renderMu.Lock()
	defer c.renderMu.Unlock()
	return c.renderScreenLocked(state)
}

// renderScreenLocked executes the screen transition while c.renderMu is held by the caller.
func (c *Client) renderScreenLocked(state ui_ipc.ScreenState) error {
	if state.PrinterModel == "" && c.cfg.PrinterModel != "" {
		state.PrinterModel = c.cfg.PrinterModel
	}

	// Only append logical state transitions to history, omitting intermediate animation frames
	isIntermediateAnim := (state.TransitionFrom >= 0 &&
		state.TransitionFrom != state.AdSlideIndex &&
		state.TransitionProg > 0.0 &&
		state.TransitionProg < 1.0)

	if !isIntermediateAnim {
		c.renderHistoryMu.Lock()
		c.renderHistory = append(c.renderHistory, state)
		c.renderHistoryMu.Unlock()
	}

	c.currentModeMu.Lock()
	c.currentMode = state.Mode
	c.currentModeMu.Unlock()

	if c.cfg.StateObserver != nil {
		c.cfg.StateObserver(state)
	}

	targetW, targetH := ui_ipc.GetTargetResolution()
	if c.canvas == nil || c.canvas.Bounds().Dx() != targetW || c.canvas.Bounds().Dy() != targetH {
		c.canvas = image.NewRGBA(image.Rect(0, 0, targetW, targetH))
	}

	if err := ui_ipc.RenderTo(c.canvas, state); err != nil {
		c.lastRenderErrMu.Lock()
		c.lastRenderErr = err
		c.lastRenderErrMu.Unlock()
		return err
	}
	if err := c.cfg.ScreenSink(c.canvas); err != nil {
		c.lastRenderErrMu.Lock()
		c.lastRenderErr = err
		c.lastRenderErrMu.Unlock()
		return err
	}
	return nil
}

func (c *Client) idleCarouselLoop(ctx context.Context) {
	interval := c.cfg.IdleCarouselInterval
	if interval <= 0 {
		interval = 5 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			slideCount := ui_ipc.GetAdSlideCount()
			if slideCount <= 1 {
				continue
			}

			c.currentModeMu.Lock()
			isIdle := (c.currentMode == ui_ipc.ModeIdle)
			fromIdx := c.adSlideIndex % slideCount
			toIdx := (c.adSlideIndex + 1) % slideCount
			c.adSlideIndex = toIdx
			c.currentModeMu.Unlock()

			if !isIdle {
				continue
			}

			// Wall-clock synchronized 500ms transition at locked 60 FPS (~16.6ms per frame)
			const (
				animDuration  = 500 * time.Millisecond
				targetFPS     = 60
				frameDuration = 1000 * time.Millisecond / targetFPS
			)

			startTime := time.Now()
			frameTimer := time.NewTimer(frameDuration)
			if !frameTimer.Stop() {
				select {
				case <-frameTimer.C:
				default:
				}
			}

			for {
				select {
				case <-ctx.Done():
					frameTimer.Stop()
					return
				default:
				}

				c.currentModeMu.Lock()
				stillIdle := (c.currentMode == ui_ipc.ModeIdle)
				c.currentModeMu.Unlock()

				if !stillIdle {
					break
				}

				elapsed := time.Since(startTime)
				if elapsed >= animDuration {
					// Final static frame
					_ = c.renderScreen(ui_ipc.ScreenState{
						Mode:           ui_ipc.ModeIdle,
						QRContent:      c.cfg.IdleQRContent,
						Message:        "Ready to print",
						AdSlideIndex:   toIdx,
						TransitionFrom: -1,
						TransitionProg: 1.0,
					})
					break
				}

				prog := float64(elapsed) / float64(animDuration)
				_ = c.renderScreen(ui_ipc.ScreenState{
					Mode:           ui_ipc.ModeIdle,
					QRContent:      c.cfg.IdleQRContent,
					Message:        "Ready to print",
					AdSlideIndex:   toIdx,
					TransitionFrom: fromIdx,
					TransitionProg: prog,
				})

				// Sleep precisely until next frame boundary based on wall-clock time
				nextFrameTime := startTime.Add(time.Duration((elapsed/frameDuration)+1) * frameDuration)
				sleepDuration := time.Until(nextFrameTime)
				if sleepDuration > 0 {
					frameTimer.Reset(sleepDuration)
					select {
					case <-ctx.Done():
						frameTimer.Stop()
						return
					case <-frameTimer.C:
					}
				}
			}
			frameTimer.Stop()
		}
	}
}

func (c *Client) handleSyncAds(ads []contracts.AdItem) {
	log.Printf("[SPOOLER] Received sync_ads push with %d items", len(ads))
	ui_ipc.ConfigureAds(ads)
	go saveAdsCache(c.cfg.AdsCachePath, ads)
}

func (c *Client) fetchInitialAds() {
	if c.cfg.CloudURL == "" {
		return
	}
	httpURL := strings.Replace(c.cfg.CloudURL, "wss://", "https://", 1)
	httpURL = strings.Replace(httpURL, "ws://", "http://", 1)
	httpURL = strings.TrimSuffix(httpURL, "/ws") + "/api/ads"

	u, err := url.Parse(httpURL)
	if err != nil {
		return
	}
	u.RawQuery = ""

	resp, err := c.cfg.HTTPClient.Get(u.String())
	if err != nil {
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return
	}

	var ads []contracts.AdItem
	if err := json.NewDecoder(resp.Body).Decode(&ads); err == nil && len(ads) > 0 {
		c.handleSyncAds(ads)
	}
}

func saveAdsCache(path string, ads []contracts.AdItem) {
	if path == "" {
		return
	}
	dir := filepath.Dir(path)
	_ = os.MkdirAll(dir, 0755)
	data, err := json.MarshalIndent(ads, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(path, data, 0644)
}

func loadAdsCache(path string) ([]contracts.AdItem, error) {
	if path == "" {
		return nil, os.ErrNotExist
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var ads []contracts.AdItem
	if err := json.Unmarshal(data, &ads); err != nil {
		return nil, err
	}
	return ads, nil
}

// Workflow returns the configured PaymentWorkflow client.
func (c *Client) Workflow() *PaymentWorkflow {
	return c.workflow
}

// RedeemPIN submits a 4-digit pickup PIN to the backend using authenticated kiosk credentials.
func (c *Client) RedeemPIN(ctx context.Context, pin string) (*PINRedeemResponse, error) {
	if c.workflow == nil {
		return nil, errors.New("payment workflow is not configured (missing kiosk secret)")
	}
	return c.workflow.RedeemPIN(ctx, pin)
}

// InitiatePaymentQR initiates a single-use Razorpay UPI QR code for the active payment session.
func (c *Client) InitiatePaymentQR(ctx context.Context, sessionID string) (*PaymentQRResponse, error) {
	if c.workflow == nil {
		return nil, errors.New("payment workflow is not configured")
	}
	return c.workflow.InitiatePaymentQR(ctx, sessionID)
}

// GetPaymentStatus queries payment status for the session.
func (c *Client) GetPaymentStatus(ctx context.Context, sessionID string) (*PaymentStatusResponse, error) {
	if c.workflow == nil {
		return nil, errors.New("payment workflow is not configured")
	}
	return c.workflow.GetPaymentStatus(ctx, sessionID)
}

// PollPaymentStatus polls until payment is confirmed or failed.
func (c *Client) PollPaymentStatus(ctx context.Context, sessionID string, onUpdate func(*PaymentStatusResponse)) (*PaymentStatusResponse, error) {
	if c.workflow == nil {
		return nil, errors.New("payment workflow is not configured")
	}
	return c.workflow.PollPaymentStatus(ctx, sessionID, onUpdate)
}

// GetRenderHistory returns a thread-safe snapshot of all rendered screen states.
func (c *Client) GetRenderHistory() []ui_ipc.ScreenState {
	c.renderHistoryMu.Lock()
	defer c.renderHistoryMu.Unlock()
	copied := make([]ui_ipc.ScreenState, len(c.renderHistory))
	copy(copied, c.renderHistory)
	return copied
}

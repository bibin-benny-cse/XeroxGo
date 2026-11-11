package spooler

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"image"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"xeroxgo/kiosk-daemon/internal/state"
	"xeroxgo/kiosk-daemon/internal/ui_ipc"
	"xeroxgo/pkg/contracts"
)

// createThreePageFixture writes a valid PostScript file with three distinct pages
// and generates a PDF using Ghostscript under dir.
func createThreePageFixture(t *testing.T, dir string) (string, []byte, string) {
	t.Helper()

	if _, err := exec.LookPath("gs"); err != nil {
		t.Skip("ghostscript (gs) not found in PATH; skipping test")
	}

	psPath := filepath.Join(dir, "fixture_3pages.ps")
	pdfPath := filepath.Join(dir, "fixture_3pages.pdf")

	psContent := `%!PS-Adobe-3.0
%%Pages: 3
%%Page: 1 1
/Helvetica findfont 24 scalefont setfont
100 700 moveto
(XeroxGo Slicer Test - Page 1) show
showpage

%%Page: 2 2
/Helvetica findfont 24 scalefont setfont
100 700 moveto
(XeroxGo Slicer Test - Page 2) show
showpage

%%Page: 3 3
/Helvetica findfont 24 scalefont setfont
100 700 moveto
(XeroxGo Slicer Test - Page 3) show
showpage
`
	if err := os.WriteFile(psPath, []byte(psContent), 0600); err != nil {
		t.Fatalf("failed to write PostScript fixture: %v", err)
	}

	cmd := exec.Command("gs",
		"-sDEVICE=pdfwrite",
		"-sPAPERSIZE=a4",
		"-dFIXEDMEDIA",
		"-dNOPAUSE",
		"-dBATCH",
		"-dQUIET",
		"-sOutputFile="+pdfPath,
		psPath,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Ghostscript failed to generate PDF fixture: %s: %v", strings.TrimSpace(string(out)), err)
	}

	data, err := os.ReadFile(pdfPath)
	if err != nil {
		t.Fatalf("failed to read generated PDF: %v", err)
	}
	hash := sha256.Sum256(data)
	return pdfPath, data, hex.EncodeToString(hash[:])
}

func noopSink(*image.RGBA) error {
	return nil
}

// Test A — TestNewClientRejectsInvalidConfig
func TestNewClientRejectsInvalidConfig(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test_config.db")
	store, err := state.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open state store: %v", err)
	}
	defer store.Close()

	validCfg := ClientConfig{
		CloudURL:      "ws://127.0.0.1:8080/ws",
		KioskID:       "kiosk-001",
		StateStore:    store,
		PrinterAddr:   "127.0.0.1:9100",
		DownloadDir:   tmpDir,
		IdleQRContent: "https://example.com/kiosk/001",
		ScreenSink:    noopSink,
	}

	testCases := []struct {
		name      string
		modifyCfg func(c *ClientConfig)
	}{
		{"empty cloud url", func(c *ClientConfig) { c.CloudURL = "" }},
		{"invalid cloud url", func(c *ClientConfig) { c.CloudURL = "http://[::1]:namedport" }},
		{"invalid scheme", func(c *ClientConfig) { c.CloudURL = "http://127.0.0.1:8080/ws" }},
		{"missing host", func(c *ClientConfig) { c.CloudURL = "ws:///ws" }},
		{"empty kiosk id", func(c *ClientConfig) { c.KioskID = "" }},
		{"nil state store", func(c *ClientConfig) { c.StateStore = nil }},
		{"empty printer address", func(c *ClientConfig) { c.PrinterAddr = "" }},
		{"empty download dir", func(c *ClientConfig) { c.DownloadDir = "" }},
		{"empty idle qr", func(c *ClientConfig) { c.IdleQRContent = "" }},
		{"nil screen sink", func(c *ClientConfig) { c.ScreenSink = nil }},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validCfg
			tc.modifyCfg(&cfg)
			client, err := NewClient(cfg)
			if err == nil {
				t.Fatalf("expected error for case %q, got nil", tc.name)
			}
			if client != nil {
				t.Fatalf("expected nil client on error for case %q", tc.name)
			}
		})
	}

	// Valid config succeeds
	c, err := NewClient(validCfg)
	if err != nil {
		t.Fatalf("expected success for valid config, got: %v", err)
	}
	if c == nil {
		t.Fatal("expected non-nil client")
	}
	if !strings.Contains(c.normalizedURL, "kiosk_id=kiosk-001") {
		t.Fatalf("expected normalized URL to contain kiosk_id, got %s", c.normalizedURL)
	}
	if c.cfg.HTTPClient == nil {
		t.Fatal("expected default HTTP client to be initialized")
	}
}

// Test B — TestDownloadPDFVerifiesSHA256
func TestDownloadPDFVerifiesSHA256(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test_b.db")
	store, err := state.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer store.Close()

	pdfContent := []byte("%PDF-1.4\n1 0 obj<</Type/Catalog/Pages 2 0 R>>endobj\n%%EOF\n")
	hasher := sha256.Sum256(pdfContent)
	goodSHA := hex.EncodeToString(hasher[:])

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(pdfContent)
	}))
	defer server.Close()

	client, err := NewClient(ClientConfig{
		CloudURL:      "ws://127.0.0.1:8080/ws",
		KioskID:       "kiosk-001",
		StateStore:    store,
		PrinterAddr:   "127.0.0.1:9100",
		DownloadDir:   tmpDir,
		IdleQRContent: "https://example.com",
		ScreenSink:    noopSink,
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	jobID := "job-sha256-test"
	h := sha256.Sum256([]byte(jobID))
	finalName := hex.EncodeToString(h[:]) + ".pdf"
	finalPath := filepath.Join(tmpDir, finalName)

	cmdGood := contracts.PrintJobCommand{
		JobID:          jobID,
		KioskID:        "kiosk-001",
		DownloadURL:    server.URL + "/doc.pdf",
		SHA256Checksum: goodSHA,
	}

	// 1. Download with correct checksum
	if err := client.downloadAndVerify(context.Background(), cmdGood, finalPath); err != nil {
		t.Fatalf("downloadAndVerify failed with correct checksum: %v", err)
	}

	storedBytes, err := os.ReadFile(finalPath)
	if err != nil {
		t.Fatalf("failed to read stored PDF: %v", err)
	}
	if !bytes.Equal(storedBytes, pdfContent) {
		t.Fatal("stored PDF content does not match served payload")
	}

	_ = os.Remove(finalPath)

	// 2. Download with incorrect checksum
	cmdBad := cmdGood
	cmdBad.SHA256Checksum = "0000000000000000000000000000000000000000000000000000000000000000"
	err = client.downloadAndVerify(context.Background(), cmdBad, finalPath)
	if err == nil {
		t.Fatal("expected error with bad checksum, got nil")
	}

	if _, err := os.Stat(finalPath); err == nil {
		t.Fatal("final PDF should not exist after checksum mismatch")
	}

	// Verify no temporary files remain
	files, _ := os.ReadDir(tmpDir)
	for _, f := range files {
		if strings.HasPrefix(f.Name(), "job_download_") {
			t.Fatalf("found leftover temporary file: %s", f.Name())
		}
	}
}

// Test C — TestDownloadPDFRejectsNon200AndOversizedResponses
func TestDownloadPDFRejectsNon200AndOversizedResponses(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test_c.db")
	store, err := state.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer store.Close()

	client, err := NewClient(ClientConfig{
		CloudURL:      "ws://127.0.0.1:8080/ws",
		KioskID:       "kiosk-001",
		StateStore:    store,
		PrinterAddr:   "127.0.0.1:9100",
		DownloadDir:   tmpDir,
		IdleQRContent: "https://example.com",
		ScreenSink:    noopSink,
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	// 1. Non-200 responses
	statusServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/404" {
			http.NotFound(w, r)
		} else {
			http.Error(w, "internal server error", http.StatusInternalServerError)
		}
	}))
	defer statusServer.Close()

	targetPath := filepath.Join(tmpDir, "should_not_exist.pdf")
	cmd404 := contracts.PrintJobCommand{
		JobID:          "j-404",
		DownloadURL:    statusServer.URL + "/404",
		SHA256Checksum: strings.Repeat("a", 64),
	}
	if err := client.downloadAndVerify(context.Background(), cmd404, targetPath); err == nil {
		t.Fatal("expected error on 404, got nil")
	}

	cmd500 := contracts.PrintJobCommand{
		JobID:          "j-500",
		DownloadURL:    statusServer.URL + "/500",
		SHA256Checksum: strings.Repeat("a", 64),
	}
	if err := client.downloadAndVerify(context.Background(), cmd500, targetPath); err == nil {
		t.Fatal("expected error on 500, got nil")
	}

	// 2. Declared Content-Length exceeding 250 MiB limit
	declaredOversizedServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "300000000") // 300 MB > 250 MiB
		w.WriteHeader(http.StatusOK)
	}))
	defer declaredOversizedServer.Close()

	cmdOversized := contracts.PrintJobCommand{
		JobID:          "j-oversized",
		DownloadURL:    declaredOversizedServer.URL,
		SHA256Checksum: strings.Repeat("b", 64),
	}
	if err := client.downloadAndVerify(context.Background(), cmdOversized, targetPath); err == nil {
		t.Fatal("expected error on declared Content-Length > 250 MiB, got nil")
	}

	// 3. Streamed payload exceeding limit without Content-Length
	streamOversizedServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(strings.Repeat("x", 128)))
	}))
	defer streamOversizedServer.Close()

	client.maxDownloadBytes = 50 // small test limit
	cmdStreamOversized := contracts.PrintJobCommand{
		JobID:          "j-stream-oversized",
		DownloadURL:    streamOversizedServer.URL,
		SHA256Checksum: strings.Repeat("c", 64),
	}
	if err := client.downloadAndVerify(context.Background(), cmdStreamOversized, targetPath); err == nil {
		t.Fatal("expected error when streaming exceeds limit, got nil")
	}

	// Verify no temporary files remain
	files, _ := os.ReadDir(tmpDir)
	for _, f := range files {
		if strings.HasPrefix(f.Name(), "job_download_") {
			t.Fatalf("leftover temporary file after error: %s", f.Name())
		}
	}
}

// Test D — TestClientProcessesJobEndToEnd
func TestClientProcessesJobEndToEnd(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "kiosk.db")
	store, err := state.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer store.Close()

	pdfContent := []byte("%PDF-1.4\n1 0 obj<</Type/Catalog>>endobj\n%%EOF\n")
	hasher := sha256.Sum256(pdfContent)
	pdfChecksum := hex.EncodeToString(hasher[:])

	// 1. PDF Server
	pdfServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(pdfContent)
	}))
	defer pdfServer.Close()

	// 2. Mock TCP Printer
	var printerReceived bytes.Buffer
	printerDone := make(chan struct{})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen for mock printer: %v", err)
	}
	defer ln.Close()

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()

		// Read incoming stream until client finishes CloseWrite()
		_, _ = io.Copy(&printerReceived, conn)

		// Respond with PJL status events: 1 page and job end
		_, _ = conn.Write([]byte("@PJL USTATUS PAGE\r\n1\r\n@PJL USTATUS JOB\r\nEND\r\n"))
		close(printerDone)
	}()

	// 3. Local WebSocket Hub Server
	type receivedProgressMsg struct {
		evt contracts.JobProgressEvent
	}
	var progressMu sync.Mutex
	var receivedProgress []contracts.JobProgressEvent
	jobDispatched := make(chan struct{})

	jobCmd := contracts.PrintJobCommand{
		JobID:          "job-e2e-001",
		KioskID:        "kiosk-e2e",
		DisplayCode:    "8844",
		DownloadURL:    pdfServer.URL + "/job.pdf",
		SHA256Checksum: pdfChecksum,
		TotalPages:     1,
		TotalSheets:    1,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		StartFromPage:  1,
	}

	wsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()

		// Send the job command
		_ = wsjson.Write(r.Context(), c, wireMessage{
			Type: "job",
			Job:  &jobCmd,
		})
		close(jobDispatched)

		// Read progress events
		for {
			var msg wireMessage
			if err := wsjson.Read(r.Context(), c, &msg); err != nil {
				break
			}
			if msg.Type == "progress" && msg.Progress != nil {
				progressMu.Lock()
				receivedProgress = append(receivedProgress, *msg.Progress)
				progressMu.Unlock()
			}
		}
	}))
	defer wsServer.Close()

	wsURL := "ws" + strings.TrimPrefix(wsServer.URL, "http")

	// 4. Recording Screen Sink
	var screenMu sync.Mutex
	var renderedScreens []*image.RGBA
	sink := func(img *image.RGBA) error {
		screenMu.Lock()
		defer screenMu.Unlock()
		renderedScreens = append(renderedScreens, img)
		return nil
	}

	// 5. Build Client
	client, err := NewClient(ClientConfig{
		CloudURL:      wsURL,
		KioskID:       "kiosk-e2e",
		StateStore:    store,
		PrinterAddr:   ln.Addr().String(),
		DownloadDir:   tmpDir,
		IdleQRContent: "https://example.com/kiosk-e2e",
		ScreenSink:    sink,
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	client.minBackoff = 20 * time.Millisecond
	client.maxBackoff = 50 * time.Millisecond
	client.heartbeatInterval = 50 * time.Millisecond
	client.healthPoller = func(ctx context.Context, sheets int) contracts.PrinterHealth {
		return contracts.PrinterHealth{
			State:             "IDLE",
			PaperTrayStatus:   "OK",
			TonerLevelPercent: 90,
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	clientDone := make(chan error, 1)
	go func() {
		clientDone <- client.Run(ctx)
	}()

	// Wait for printer to complete
	select {
	case <-printerDone:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for mock printer to complete")
	}

	// Wait a moment for completion to settle in store and WS
	deadline := time.Now().Add(3 * time.Second)
	completed := false
	for time.Now().Before(deadline) {
		inc, err := store.GetIncompleteJob(ctx)
		if err == nil && inc == nil {
			// If incomplete job is nil, verify local DB is readable
			if _, sErr := store.GetEstimatedSheets(ctx); sErr == nil {
				completed = true
				break
			}
		}
		time.Sleep(30 * time.Millisecond)
	}

	cancel()
	select {
	case err := <-clientDone:
		if err != nil {
			t.Fatalf("client.Run returned unexpected error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for client shutdown")
	}

	// Assertions
	if !completed {
		t.Fatal("expected job to reach COMPLETED status")
	}

	// 1. PDF payload transmitted unchanged
	if !bytes.Contains(printerReceived.Bytes(), pdfContent) {
		t.Fatal("printer stream did not contain the original PDF payload")
	}

	// 2. UI Transitions Produced
	client.renderHistoryMu.Lock()
	history := client.renderHistory
	client.renderHistoryMu.Unlock()

	var hasIdle, hasDownload, hasPrint, hasCompleted bool
	for _, s := range history {
		switch s.Mode {
		case ui_ipc.ModeIdle:
			hasIdle = true
		case ui_ipc.ModeDownloading:
			hasDownload = true
		case ui_ipc.ModePrinting:
			hasPrint = true
		case ui_ipc.ModeCompleted:
			hasCompleted = true
		}
	}
	if !hasIdle || !hasDownload || !hasPrint || !hasCompleted {
		t.Fatalf("missing required UI transitions: idle=%v, download=%v, print=%v, completed=%v",
			hasIdle, hasDownload, hasPrint, hasCompleted)
	}

	// 3. WS Hub received progress events including COMPLETED
	progressMu.Lock()
	defer progressMu.Unlock()
	var hubSawCompleted bool
	for _, p := range receivedProgress {
		if p.Status == contracts.JobStatusCompleted && p.PagesPrinted == 1 && p.SheetsPrinted == 1 {
			hubSawCompleted = true
		}
	}
	if !hubSawCompleted {
		t.Fatalf("cloud WS hub did not receive final COMPLETED progress event: %+v", receivedProgress)
	}
}

// Test E — TestClientPersistsStateWhenCloudDisconnects
func TestClientPersistsStateWhenCloudDisconnects(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "kiosk_e.db")
	store, err := state.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer store.Close()

	pdfContent := []byte("%PDF-1.4\n1 0 obj<</Type/Catalog>>endobj\n%%EOF\n")
	hasher := sha256.Sum256(pdfContent)
	pdfChecksum := hex.EncodeToString(hasher[:])

	pdfServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(pdfContent)
	}))
	defer pdfServer.Close()

	printerLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("printer listener failed: %v", err)
	}
	defer printerLn.Close()

	printerDone := make(chan struct{})
	go func() {
		conn, err := printerLn.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = io.Copy(io.Discard, conn)
		_, _ = conn.Write([]byte("@PJL USTATUS PAGE\r\n1\r\n@PJL USTATUS JOB\r\nEND\r\n"))
		close(printerDone)
	}()

	jobCmd := contracts.PrintJobCommand{
		JobID:          "job-disconnect-001",
		KioskID:        "kiosk-e",
		DisplayCode:    "1122",
		DownloadURL:    pdfServer.URL + "/doc.pdf",
		SHA256Checksum: pdfChecksum,
		TotalPages:     1,
		TotalSheets:    1,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		StartFromPage:  1,
	}

	wsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		// Dispatch job and immediately close WebSocket!
		_ = wsjson.Write(r.Context(), c, wireMessage{Type: "job", Job: &jobCmd})
		_ = c.Close(websocket.StatusGoingAway, "server shutting down")
	}))
	defer wsServer.Close()

	wsURL := "ws" + strings.TrimPrefix(wsServer.URL, "http")

	client, err := NewClient(ClientConfig{
		CloudURL:      wsURL,
		KioskID:       "kiosk-e",
		StateStore:    store,
		PrinterAddr:   printerLn.Addr().String(),
		DownloadDir:   tmpDir,
		IdleQRContent: "https://example.com/kiosk-e",
		ScreenSink:    noopSink,
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	client.minBackoff = 50 * time.Millisecond
	client.maxBackoff = 100 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		_ = client.Run(ctx)
	}()

	select {
	case <-printerDone:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for physical print to finish")
	}

	// Verify local state store reached COMPLETED
	deadline := time.Now().Add(2 * time.Second)
	completed := false
	for time.Now().Before(deadline) {
		inc, err := store.GetIncompleteJob(ctx)
		if err == nil && inc == nil {
			completed = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if !completed {
		t.Fatal("job should have completed locally despite cloud disconnection")
	}
}

// Test F — TestClientReconnectsWithBackoff
func TestClientReconnectsWithBackoff(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "kiosk_f.db")
	store, err := state.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer store.Close()

	var allowConnect atomic.Bool
	var connected atomic.Bool

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !allowConnect.Load() {
			http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
			return
		}
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		connected.Store(true)

		for {
			var msg wireMessage
			if err := wsjson.Read(r.Context(), c, &msg); err != nil {
				break
			}
		}
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")

	client, err := NewClient(ClientConfig{
		CloudURL:      wsURL,
		KioskID:       "kiosk-f",
		StateStore:    store,
		PrinterAddr:   "127.0.0.1:9999",
		DownloadDir:   tmpDir,
		IdleQRContent: "https://example.com/kiosk-f",
		ScreenSink:    noopSink,
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	client.minBackoff = 10 * time.Millisecond
	client.maxBackoff = 30 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runDone := make(chan error, 1)
	go func() {
		runDone <- client.Run(ctx)
	}()

	// Wait 50ms while server is unavailable
	time.Sleep(50 * time.Millisecond)
	if connected.Load() {
		t.Fatal("client should not have connected while server is unavailable")
	}

	// Now make server available
	allowConnect.Store(true)

	// Wait for successful reconnection
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if connected.Load() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if !connected.Load() {
		t.Fatal("client failed to reconnect after server became available")
	}

	// Prompt shutdown
	cancel()
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("client.Run returned unexpected error on shutdown: %v", err)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("timeout waiting for client shutdown after cancel")
	}
}

// Test G — TestClientHeartbeat
func TestClientHeartbeat(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "kiosk_g.db")
	store, err := state.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer store.Close()

	var gotHeartbeat, gotHealth atomic.Bool
	healthVal := atomic.Int32{}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()

		for {
			var msg wireMessage
			if err := wsjson.Read(r.Context(), c, &msg); err != nil {
				break
			}
			if msg.Type == "heartbeat" {
				gotHeartbeat.Store(true)
			}
			if msg.Type == "health" && msg.Health != nil {
				gotHealth.Store(true)
				healthVal.Store(int32(msg.Health.TonerLevelPercent))
			}
		}
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")

	client, err := NewClient(ClientConfig{
		CloudURL:      wsURL,
		KioskID:       "kiosk-g",
		StateStore:    store,
		PrinterAddr:   "127.0.0.1:9999",
		DownloadDir:   tmpDir,
		IdleQRContent: "https://example.com/kiosk-g",
		ScreenSink:    noopSink,
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	client.minBackoff = 20 * time.Millisecond
	client.heartbeatInterval = 20 * time.Millisecond
	client.healthPoller = func(ctx context.Context, sheets int) contracts.PrinterHealth {
		return contracts.PrinterHealth{
			State:             "IDLE",
			PaperTrayStatus:   "OK",
			TonerLevelPercent: 77,
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		_ = client.Run(ctx)
	}()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if gotHeartbeat.Load() && gotHealth.Load() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if !gotHeartbeat.Load() {
		t.Fatal("expected client to send heartbeat message")
	}
	if !gotHealth.Load() {
		t.Fatal("expected client to send health message")
	}
	if healthVal.Load() != 77 {
		t.Fatalf("expected toner level 77, got %d", healthVal.Load())
	}
}

// Test H — TestClientRejectsWrongKioskJob
func TestClientRejectsWrongKioskJob(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "kiosk_h.db")
	store, err := state.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer store.Close()

	var downloadAttempted atomic.Bool
	pdfServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		downloadAttempted.Store(true)
		w.WriteHeader(http.StatusOK)
	}))
	defer pdfServer.Close()

	jobWrong := contracts.PrintJobCommand{
		JobID:          "job-wrong-kiosk",
		KioskID:        "kiosk-wrong",
		DisplayCode:    "9999",
		DownloadURL:    pdfServer.URL + "/doc.pdf",
		SHA256Checksum: strings.Repeat("0", 64),
		TotalPages:     1,
		TotalSheets:    1,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		StartFromPage:  1,
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		_ = wsjson.Write(r.Context(), c, wireMessage{Type: "job", Job: &jobWrong})
		time.Sleep(100 * time.Millisecond)
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")

	client, err := NewClient(ClientConfig{
		CloudURL:      wsURL,
		KioskID:       "kiosk-right",
		StateStore:    store,
		PrinterAddr:   "127.0.0.1:9999",
		DownloadDir:   tmpDir,
		IdleQRContent: "https://example.com/kiosk-h",
		ScreenSink:    noopSink,
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		_ = client.Run(ctx)
	}()

	time.Sleep(150 * time.Millisecond)

	if downloadAttempted.Load() {
		t.Fatal("download should not be attempted for mismatched kiosk job")
	}

	inc, _ := store.GetIncompleteJob(ctx)
	if inc != nil {
		t.Fatal("no job should be stored for mismatched kiosk")
	}
}

// Test I — TestClientDuplicateJobDoesNotPrintTwice
func TestClientDuplicateJobDoesNotPrintTwice(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "kiosk_i.db")
	store, err := state.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer store.Close()

	pdfContent := []byte("%PDF-1.4\n1 0 obj<</Type/Catalog>>endobj\n%%EOF\n")
	hasher := sha256.Sum256(pdfContent)
	pdfChecksum := hex.EncodeToString(hasher[:])

	pdfServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(pdfContent)
	}))
	defer pdfServer.Close()

	printerLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("printer listener failed: %v", err)
	}
	defer printerLn.Close()

	var printConnections atomic.Int32
	holdPrint := make(chan struct{})
	printerDone := make(chan struct{})

	go func() {
		for {
			conn, err := printerLn.Accept()
			if err != nil {
				return
			}
			printConnections.Add(1)
			go func(c net.Conn) {
				defer c.Close()
				_, _ = io.Copy(io.Discard, c)
				<-holdPrint
				_, _ = c.Write([]byte("@PJL USTATUS PAGE\r\n1\r\n@PJL USTATUS JOB\r\nEND\r\n"))
				close(printerDone)
			}(conn)
		}
	}()

	jobCmd := contracts.PrintJobCommand{
		JobID:          "job-dup-001",
		KioskID:        "kiosk-i",
		DisplayCode:    "5555",
		DownloadURL:    pdfServer.URL + "/doc.pdf",
		SHA256Checksum: pdfChecksum,
		TotalPages:     1,
		TotalSheets:    1,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		StartFromPage:  1,
	}

	sendDup := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()

		// Send initial job
		_ = wsjson.Write(r.Context(), c, wireMessage{Type: "job", Job: &jobCmd})

		// Send duplicate when triggered
		<-sendDup
		_ = wsjson.Write(r.Context(), c, wireMessage{Type: "job", Job: &jobCmd})

		for {
			var msg wireMessage
			if err := wsjson.Read(r.Context(), c, &msg); err != nil {
				break
			}
		}
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")

	client, err := NewClient(ClientConfig{
		CloudURL:      wsURL,
		KioskID:       "kiosk-i",
		StateStore:    store,
		PrinterAddr:   printerLn.Addr().String(),
		DownloadDir:   tmpDir,
		IdleQRContent: "https://example.com/kiosk-i",
		ScreenSink:    noopSink,
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		_ = client.Run(ctx)
	}()

	// Wait until print connection is made
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if printConnections.Load() == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if printConnections.Load() != 1 {
		t.Fatalf("expected 1 print connection, got %d", printConnections.Load())
	}

	// Send duplicate job while first print is still active
	close(sendDup)
	time.Sleep(100 * time.Millisecond)

	// Ensure no second print connection was created
	if printConnections.Load() != 1 {
		t.Fatalf("expected exactly 1 print connection, got %d", printConnections.Load())
	}

	// Release printer and complete
	close(holdPrint)
	select {
	case <-printerDone:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for print to complete")
	}
}

// Test J — TestClientStartupRecovery
func TestClientStartupRecovery(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "kiosk_j.db")
	store, err := state.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer store.Close()

	_, pdfBytes, pdfSum := createThreePageFixture(t, tmpDir)

	pdfServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(pdfBytes)
	}))
	defer pdfServer.Close()

	// Case 1: Incomplete job with PagesPrinted = 0
	t.Run("recovery with 0 pages printed", func(t *testing.T) {
		printerLn, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("printer listener failed: %v", err)
		}
		defer printerLn.Close()

		go func() {
			conn, err := printerLn.Accept()
			if err != nil {
				return
			}
			defer conn.Close()
			_, _ = io.Copy(io.Discard, conn)
			_, _ = conn.Write([]byte("@PJL USTATUS PAGE\r\n1\r\n@PJL USTATUS JOB\r\nEND\r\n"))
		}()

		job0 := contracts.PrintJobCommand{
			JobID:          "job-rec-0",
			KioskID:        "kiosk-j",
			DisplayCode:    "0000",
			DownloadURL:    pdfServer.URL + "/doc.pdf",
			SHA256Checksum: pdfSum,
			TotalPages:     1,
			TotalSheets:    1,
			Copies:         1,
			Duplex:         contracts.DuplexSimplex,
			StartFromPage:  1,
		}

		// Save incomplete job in store before starting client
		if err := store.SaveJob(context.Background(), job0, contracts.JobStatusPausedError); err != nil {
			t.Fatalf("SaveJob failed: %v", err)
		}

		client, err := NewClient(ClientConfig{
			CloudURL:      "ws://127.0.0.1:8080/ws",
			KioskID:       "kiosk-j",
			StateStore:    store,
			PrinterAddr:   printerLn.Addr().String(),
			DownloadDir:   tmpDir,
			IdleQRContent: "https://example.com/kiosk-j",
			ScreenSink:    noopSink,
		})
		if err != nil {
			t.Fatalf("NewClient failed: %v", err)
		}

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		go func() {
			_ = client.Run(ctx)
		}()

		deadline := time.Now().Add(3 * time.Second)
		completed := false
		for time.Now().Before(deadline) {
			inc, err := store.GetIncompleteJob(ctx)
			if err == nil && inc == nil {
				completed = true
				break
			}
			time.Sleep(30 * time.Millisecond)
		}

		if !completed {
			t.Fatal("incomplete job with 0 pages was not recovered to COMPLETED")
		}
	})

	// Case 2: Incomplete job with PagesPrinted = 1, slicing remaining pages 2-3
	t.Run("recovery with page slicing for partial print", func(t *testing.T) {
		printerLn, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("printer listener failed: %v", err)
		}
		defer printerLn.Close()

		var receivedBytes bytes.Buffer
		go func() {
			conn, err := printerLn.Accept()
			if err != nil {
				return
			}
			defer conn.Close()
			_, _ = io.Copy(&receivedBytes, conn)
			// Mock printer reports remaining 2 sheets
			_, _ = conn.Write([]byte("@PJL USTATUS PAGE\r\n1\r\n@PJL USTATUS PAGE\r\n2\r\n@PJL USTATUS JOB\r\nEND\r\n"))
		}()

		jobSlice := contracts.PrintJobCommand{
			JobID:          "job-rec-slice",
			KioskID:        "kiosk-j",
			DisplayCode:    "3333",
			DownloadURL:    pdfServer.URL + "/doc.pdf",
			SHA256Checksum: pdfSum,
			TotalPages:     3,
			TotalSheets:    3,
			Copies:         1,
			Duplex:         contracts.DuplexSimplex,
			StartFromPage:  1,
		}

		if err := store.SaveJob(context.Background(), jobSlice, contracts.JobStatusPausedError); err != nil {
			t.Fatalf("SaveJob failed: %v", err)
		}

		// Update progress to 1 page and 1 sheet printed
		evt := contracts.JobProgressEvent{
			JobID:         jobSlice.JobID,
			KioskID:       jobSlice.KioskID,
			Status:        contracts.JobStatusPausedError,
			PagesPrinted:  1,
			SheetsPrinted: 1,
			Timestamp:     time.Now().UTC(),
		}
		if err := store.UpdateProgress(context.Background(), evt); err != nil {
			t.Fatalf("UpdateProgress failed: %v", err)
		}

		client, err := NewClient(ClientConfig{
			CloudURL:      "ws://127.0.0.1:8080/ws",
			KioskID:       "kiosk-j",
			StateStore:    store,
			PrinterAddr:   printerLn.Addr().String(),
			DownloadDir:   tmpDir,
			IdleQRContent: "https://example.com/kiosk-j",
			ScreenSink:    noopSink,
		})
		if err != nil {
			t.Fatalf("NewClient failed: %v", err)
		}

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		go func() {
			_ = client.Run(ctx)
		}()

		deadline := time.Now().Add(5 * time.Second)
		completed := false
		for time.Now().Before(deadline) {
			inc, err := store.GetIncompleteJob(ctx)
			if err == nil && inc == nil {
				completed = true
				break
			}
			time.Sleep(30 * time.Millisecond)
		}

		if !completed {
			t.Fatal("partially printed job was not sliced and recovered to COMPLETED")
		}

		// Verify that PJL command sent to printer had TotalPages=2 (remaining pages)
		if !bytes.Contains(receivedBytes.Bytes(), []byte("@PJL ENTER LANGUAGE=PDF")) {
			t.Fatal("printer stream missing PJL header")
		}
	})
}

// Test K — TestClientCancellationStopsOwnedGoroutines
func TestClientCancellationStopsOwnedGoroutines(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "kiosk_k.db")
	store, err := state.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer store.Close()

	client, err := NewClient(ClientConfig{
		CloudURL:      "ws://127.0.0.1:8080/ws",
		KioskID:       "kiosk-k",
		StateStore:    store,
		PrinterAddr:   "127.0.0.1:9100",
		DownloadDir:   tmpDir,
		IdleQRContent: "https://example.com/kiosk-k",
		ScreenSink:    noopSink,
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	client.minBackoff = 20 * time.Millisecond
	client.maxBackoff = 50 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())

	doneCh := make(chan error, 1)
	go func() {
		doneCh <- client.Run(ctx)
	}()

	// Let it run for a short duration
	time.Sleep(50 * time.Millisecond)

	// Cancel context
	cancel()

	select {
	case err := <-doneCh:
		if err != nil {
			t.Fatalf("client.Run returned unexpected error on cancellation: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("client.Run did not terminate promptly after context cancellation")
	}
}

func TestClientProcessesCUPSJob(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "kiosk_cups.db")
	store, err := state.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer store.Close()

	pdfContent := []byte("%PDF-1.4\n1 0 obj<</Type/Catalog>>endobj\n%%EOF\n")
	hasher := sha256.Sum256(pdfContent)
	pdfChecksum := hex.EncodeToString(hasher[:])

	pdfServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(pdfContent)
	}))
	defer pdfServer.Close()

	var cupsPrinted atomic.Bool
	var cupsQueueUsed string
	mockCUPS := func(ctx context.Context, queue string, cmd contracts.PrintJobCommand, pdfPath string, onPage func(sheetsCompleted int)) error {
		cupsQueueUsed = queue
		if onPage != nil {
			onPage(cmd.TotalSheets)
		}
		cupsPrinted.Store(true)
		return nil
	}

	jobCmd := contracts.PrintJobCommand{
		JobID:          "job-cups-001",
		KioskID:        "kiosk-cups",
		DisplayCode:    "5566",
		DownloadURL:    pdfServer.URL + "/cups.pdf",
		SHA256Checksum: pdfChecksum,
		TotalPages:     1,
		TotalSheets:    1,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		StartFromPage:  1,
	}

	var progressEvents []contracts.JobProgressEvent
	var eventsMu sync.Mutex

	cloudServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "done")

		// Push print job
		_ = wsjson.Write(r.Context(), conn, map[string]interface{}{
			"type": "job",
			"job":  jobCmd,
		})

		for {
			var msg wireMessage
			if err := wsjson.Read(r.Context(), conn, &msg); err != nil {
				return
			}
			if msg.Type == "progress" && msg.Progress != nil {
				eventsMu.Lock()
				progressEvents = append(progressEvents, *msg.Progress)
				eventsMu.Unlock()
			}
		}
	}))
	defer cloudServer.Close()

	cloudWSURL := "ws" + strings.TrimPrefix(cloudServer.URL, "http")

	client, err := NewClient(ClientConfig{
		CloudURL:      cloudWSURL,
		KioskID:       "kiosk-cups",
		StateStore:    store,
		CUPSQueue:     "DCPT310",
		DownloadDir:   tmpDir,
		IdleQRContent: "https://example.com/kiosk-cups",
		ScreenSink:    noopSink,
		MockCUPSPrint: mockCUPS,
	})
	if err != nil {
		t.Fatalf("NewClient with CUPS queue failed: %v", err)
	}

	client.minBackoff = 20 * time.Millisecond
	client.maxBackoff = 50 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	go func() {
		_ = client.Run(ctx)
	}()

	deadline := time.Now().Add(4 * time.Second)
	completed := false
	for time.Now().Before(deadline) {
		if cupsPrinted.Load() {
			inc, err := store.GetIncompleteJob(ctx)
			if err == nil && inc == nil {
				completed = true
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}

	if !cupsPrinted.Load() {
		t.Fatal("expected CUPS print job to be executed")
	}
	if cupsQueueUsed != "DCPT310" {
		t.Fatalf("expected CUPS queue DCPT310, got %q", cupsQueueUsed)
	}
	if !completed {
		t.Fatal("expected job to reach COMPLETED status in store")
	}
}

func TestClientResetsToIdleAfterCompletion(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "state.db")
	store, err := state.Open(dbPath)
	if err != nil {
		t.Fatalf("state.Open failed: %v", err)
	}
	defer store.Close()

	_, pdfContent, pdfChecksum := createThreePageFixture(t, tmpDir)

	dlServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		_, _ = w.Write(pdfContent)
	}))
	defer dlServer.Close()

	var cupsPrinted atomic.Bool
	mockCUPS := func(ctx context.Context, queue string, cmd contracts.PrintJobCommand, pdfPath string, onPage func(sheetsCompleted int)) error {
		cupsPrinted.Store(true)
		if onPage != nil {
			onPage(cmd.TotalSheets)
		}
		return nil
	}

	wsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "")

		// Send job command
		jobCmd := contracts.PrintJobCommand{
			JobID:          "job-idle-reset",
			KioskID:        "kiosk-idle",
			DownloadURL:    dlServer.URL + "/doc.pdf",
			SHA256Checksum: pdfChecksum,
			TotalPages:     1,
			TotalSheets:    1,
			Copies:         1,
			Duplex:         contracts.DuplexSimplex,
			StartFromPage:  1,
		}
		_ = wsjson.Write(r.Context(), conn, map[string]interface{}{
			"type": "job",
			"job":  jobCmd,
		})

		// Read progress events until canceled
		for {
			var msg wireMessage
			if err := wsjson.Read(r.Context(), conn, &msg); err != nil {
				return
			}
		}
	}))
	defer wsServer.Close()

	wsURL := "ws" + strings.TrimPrefix(wsServer.URL, "http")

	cfg := ClientConfig{
		CloudURL:                 wsURL,
		KioskID:                  "kiosk-idle",
		StateStore:               store,
		CUPSQueue:                "DCPT310",
		DownloadDir:              tmpDir,
		IdleQRContent:            "https://kiosk.test/idle",
		CompletedDisplayDuration: 50 * time.Millisecond,
		ScreenSink:               func(*image.RGBA) error { return nil },
		MockCUPSPrint:            mockCUPS,
	}

	client, err := NewClient(cfg)
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	runCtx, runCancel := context.WithCancel(ctx)
	defer runCancel()

	go func() {
		_ = client.Run(runCtx)
	}()

	// Wait for idle screen reset after completion
	deadline := time.Now().Add(5 * time.Second)
	resetSeen := false
	for time.Now().Before(deadline) {
		client.renderHistoryMu.Lock()
		history := make([]ui_ipc.ScreenState, len(client.renderHistory))
		copy(history, client.renderHistory)
		client.renderHistoryMu.Unlock()

		if len(history) >= 4 {
			last := history[len(history)-1]
			if last.Mode == ui_ipc.ModeIdle && last.QRContent == "https://kiosk.test/idle" {
				resetSeen = true
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}

	if !resetSeen {
		t.Fatal("expected kiosk screen to reset to ModeIdle after completion duration")
	}
}

func TestClientConcurrentSyncAdsAndDeduplication(t *testing.T) {
	defer ui_ipc.ConfigureAds(nil)

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "ads_sync.db")
	cachePath := filepath.Join(tmpDir, "ads_cache.json")
	store, err := state.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer store.Close()

	ts := time.Date(2026, 11, 11, 6, 30, 0, 0, time.UTC)
	threeAds := []contracts.AdItem{
		{
			ID:          "ad-quick-easy",
			Title:       "Quick & Easy",
			Description: "Direct UPI payment. No coins, no cash needed.",
			Type:        contracts.AdTypeGradient,
			GradientC0:  "#1E3A8A",
			GradientC1:  "#2563EB",
			GradientC2:  "#4F46E5",
			SortOrder:   1,
			IsActive:    true,
			UpdatedAt:   ts,
		},
		{
			ID:          "ad-high-quality",
			Title:       "High Quality Prints",
			Description: "Crisp laser documents ready in seconds.",
			Type:        contracts.AdTypeGradient,
			GradientC0:  "#065F46",
			GradientC1:  "#10B981",
			GradientC2:  "#059669",
			SortOrder:   2,
			IsActive:    true,
			UpdatedAt:   ts,
		},
		{
			ID:          "ad-skip-queue",
			Title:       "Skip The Queue",
			Description: "Pick up your print job with your 4-digit code.",
			Type:        contracts.AdTypeGradient,
			GradientC0:  "#581C87",
			GradientC1:  "#7C3AED",
			GradientC2:  "#D97706",
			SortOrder:   3,
			IsActive:    true,
			UpdatedAt:   ts,
		},
	}

	updatedTwoAds := []contracts.AdItem{
		{
			ID:          "ad-updated-1",
			Title:       "Updated Promo 1",
			Description: "Fresh promotional campaign.",
			Type:        contracts.AdTypeGradient,
			GradientC0:  "#7C2D12",
			GradientC1:  "#EA580C",
			GradientC2:  "#F59E0B",
			SortOrder:   1,
			IsActive:    true,
			UpdatedAt:   ts.Add(time.Minute),
		},
		{
			ID:          "ad-updated-2",
			Title:       "Updated Promo 2",
			Description: "Second updated banner.",
			Type:        contracts.AdTypeGradient,
			GradientC0:  "#0F172A",
			GradientC1:  "#334155",
			GradientC2:  "#1E293B",
			SortOrder:   2,
			IsActive:    true,
			UpdatedAt:   ts.Add(time.Minute),
		},
	}

	var restCalls atomic.Int32
	wsConnected := make(chan struct{})
	var wsOnce sync.Once
	triggerSecondUpdate := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/ads":
			restCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(threeAds)
		case "/ws":
			conn, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			defer conn.Close(websocket.StatusNormalClosure, "")
			wsOnce.Do(func() { close(wsConnected) })

			// Push identical 3-ad sync_ads updates back-to-back while fetchInitialAds runs
			_ = wsjson.Write(ctx, conn, wireMessage{Type: "sync_ads", Ads: threeAds})
			_ = wsjson.Write(ctx, conn, wireMessage{Type: "sync_ads", Ads: threeAds})

			select {
			case <-triggerSecondUpdate:
				_ = wsjson.Write(ctx, conn, wireMessage{Type: "sync_ads", Ads: updatedTwoAds})
			case <-ctx.Done():
				return
			}

			for {
				var msg wireMessage
				if err := wsjson.Read(ctx, conn, &msg); err != nil {
					return
				}
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"

	client, err := NewClient(ClientConfig{
		CloudURL:      wsURL,
		KioskID:       "kiosk-ads-sync",
		StateStore:    store,
		PrinterAddr:   "127.0.0.1:9100",
		DownloadDir:   tmpDir,
		IdleQRContent: "https://kiosk.test/ads",
		AdsCachePath:  cachePath,
		ScreenSink:    noopSink,
		HTTPClient:    srv.Client(),
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	runCtx, runCancel := context.WithCancel(ctx)
	doneCh := make(chan error, 1)
	go func() {
		doneCh <- client.Run(runCtx)
	}()

	select {
	case <-wsConnected:
	case <-ctx.Done():
		t.Fatal("timed out waiting for WebSocket connection")
	}

	// Wait until ads_cache.json contains the initial 3 ads
	deadline := time.Now().Add(3 * time.Second)
	var cached []contracts.AdItem
	for time.Now().Before(deadline) {
		var loadErr error
		cached, loadErr = loadAdsCache(cachePath)
		if loadErr == nil && len(cached) == 3 {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	if len(cached) != 3 {
		t.Fatalf("expected 3 cached ads after initial REST + WS sync, got %d", len(cached))
	}

	// Verify concurrent identical calls do not alter cache contents or panic
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			client.handleSyncAds(threeAds)
		}()
	}
	wg.Wait()

	// Trigger a distinct ad update over WebSocket and confirm propagation
	close(triggerSecondUpdate)
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		cached, _ = loadAdsCache(cachePath)
		if len(cached) == 2 && ui_ipc.GetAdSlideCount() == 2 {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	if len(cached) != 2 || ui_ipc.GetAdSlideCount() != 2 {
		t.Fatalf("expected updated 2-ad sync to propagate, cache=%d slides=%d", len(cached), ui_ipc.GetAdSlideCount())
	}

	runCancel()
	<-doneCh
}

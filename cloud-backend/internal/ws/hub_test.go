package ws

import (
	"context"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"xeroxgo/cloud-backend/internal/store"
	"xeroxgo/pkg/contracts"
)

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "hub_test.db")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open test store: %v", err)
	}
	t.Cleanup(func() {
		_ = s.Close()
	})
	return s
}

func sampleOrder(orderID, jobID, kioskID string) store.Order {
	return store.Order{
		OrderID: orderID,
		Command: contracts.PrintJobCommand{
			JobID:          jobID,
			KioskID:        kioskID,
			DisplayCode:    "C123",
			DownloadURL:    "http://example.com/pdf.pdf",
			SHA256Checksum: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
			TotalPages:     10,
			TotalSheets:    5,
			Copies:         1,
			Duplex:         contracts.DuplexSimplex,
			StartFromPage:  1,
		},
		Status:     contracts.JobStatusDispatched,
		PricePaise: 1000,
		CreatedAt:  time.Now().UTC(),
		UpdatedAt:  time.Now().UTC(),
	}
}

func connectTestKiosk(t *testing.T, hub *Hub, serverURL, kioskID string) (*websocket.Conn, context.CancelFunc) {
	t.Helper()
	u, err := url.Parse(serverURL)
	if err != nil {
		t.Fatalf("invalid server URL: %v", err)
	}
	u.Scheme = "ws"
	u.Path = "/ws"
	q := u.Query()
	q.Set("kiosk_id", kioskID)
	u.RawQuery = q.Encode()

	ctx, cancel := context.WithCancel(context.Background())
	conn, _, err := websocket.Dial(ctx, u.String(), nil)
	if err != nil {
		cancel()
		t.Fatalf("websocket.Dial failed for %s: %v", kioskID, err)
	}

	t.Cleanup(func() {
		_ = conn.Close(websocket.StatusNormalClosure, "test cleanup")
		cancel()
	})

	if hub != nil {
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if hub.IsConnected(kioskID) {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
	}

	return conn, cancel
}

// Test A — TestHubRegistersAndSendsJob
func TestHubRegistersAndSendsJob(t *testing.T) {
	db := newTestStore(t)
	hub := NewHub(db)
	server := httptest.NewServer(hub)
	defer server.Close()

	conn, _ := connectTestKiosk(t, hub, server.URL, "kiosk-001")

	cmd := contracts.PrintJobCommand{
		JobID:          "job-test-001",
		KioskID:        "kiosk-001",
		DisplayCode:    "A42B",
		DownloadURL:    "https://storage.xeroxgo.edu/files/job-test-001.pdf",
		SHA256Checksum: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		TotalPages:     5,
		TotalSheets:    3,
		Copies:         1,
		Duplex:         contracts.DuplexLongEdge,
		StartFromPage:  1,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if err := hub.SendJob(ctx, cmd); err != nil {
		t.Fatalf("hub.SendJob failed: %v", err)
	}

	var received wireMessage
	if err := wsjson.Read(ctx, conn, &received); err != nil {
		t.Fatalf("failed to read dispatched job from websocket: %v", err)
	}

	if received.Type != "job" {
		t.Fatalf("expected message type 'job', got %q", received.Type)
	}
	if received.Job == nil {
		t.Fatalf("received nil Job payload")
	}
	if *received.Job != cmd {
		t.Fatalf("received job command mismatch: got %+v, want %+v", *received.Job, cmd)
	}
}

// Test B — TestHubPersistsProgress
func TestHubPersistsProgress(t *testing.T) {
	db := newTestStore(t)
	hub := NewHub(db)
	server := httptest.NewServer(hub)
	defer server.Close()

	// Seed cloud order
	ord := sampleOrder("order-p-1", "job-p-1", "kiosk-p-1")
	if err := db.CreateOrder(context.Background(), ord); err != nil {
		t.Fatalf("failed to create seed order: %v", err)
	}

	conn, _ := connectTestKiosk(t, hub, server.URL, "kiosk-p-1")

	ts := time.Date(2026, time.October, 21, 14, 0, 0, 0, time.UTC)
	progressMsg := wireMessage{
		Type: "progress",
		Progress: &contracts.JobProgressEvent{
			JobID:         "job-p-1",
			KioskID:       "kiosk-p-1",
			Status:        contracts.JobStatusPrinting,
			PagesPrinted:  3,
			SheetsPrinted: 2,
			Timestamp:     ts,
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if err := wsjson.Write(ctx, conn, progressMsg); err != nil {
		t.Fatalf("failed to write progress event: %v", err)
	}

	// Poll store until progress is persisted
	var persisted *contracts.JobProgressEvent
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		p, err := db.GetJobProgress(ctx, "job-p-1")
		if err == nil && p != nil {
			persisted = p
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if persisted == nil {
		t.Fatalf("timed out waiting for progress to be persisted in db")
	}
	if persisted.Status != contracts.JobStatusPrinting || persisted.PagesPrinted != 3 {
		t.Fatalf("persisted progress mismatch: %+v", persisted)
	}
}

// Test C — TestHubRejectsMismatchedKioskProgress
func TestHubRejectsMismatchedKioskProgress(t *testing.T) {
	db := newTestStore(t)
	hub := NewHub(db)
	server := httptest.NewServer(hub)
	defer server.Close()

	// Seed order for kiosk-002
	ord := sampleOrder("order-m-2", "job-m-2", "kiosk-002")
	if err := db.CreateOrder(context.Background(), ord); err != nil {
		t.Fatalf("failed to create seed order: %v", err)
	}

	// Connect as kiosk-001
	conn, _ := connectTestKiosk(t, hub, server.URL, "kiosk-001")

	// Try to submit progress claiming to be for kiosk-002
	ts := time.Now().UTC()
	progressMsg := wireMessage{
		Type: "progress",
		Progress: &contracts.JobProgressEvent{
			JobID:         "job-m-2",
			KioskID:       "kiosk-002", // Mismatch with kiosk-001 connection
			Status:        contracts.JobStatusPrinting,
			PagesPrinted:  1,
			SheetsPrinted: 1,
			Timestamp:     ts,
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := wsjson.Write(ctx, conn, progressMsg); err != nil {
		t.Fatalf("failed to write progress message: %v", err)
	}

	// Give server time to process and verify db is not updated
	time.Sleep(100 * time.Millisecond)

	p, err := db.GetJobProgress(ctx, "job-m-2")
	if err != nil {
		t.Fatalf("GetJobProgress failed: %v", err)
	}
	if p != nil {
		t.Fatalf("mismatched progress was unexpectedly persisted in db: %+v", p)
	}
}

// Test D — TestHubTracksLatestHealth
func TestHubTracksLatestHealth(t *testing.T) {
	hub := NewHub(nil)
	server := httptest.NewServer(hub)
	defer server.Close()

	conn, _ := connectTestKiosk(t, hub, server.URL, "kiosk-health-1")

	health1 := contracts.PrinterHealth{
		Online:            true,
		State:             "IDLE",
		PaperTrayStatus:   "OK",
		EstimatedSheets:   200,
		TonerLevelPercent: 85,
		DrumLifePercent:   90,
		UpdatedAt:         time.Now().UTC(),
	}

	health2 := contracts.PrinterHealth{
		Online:            true,
		State:             "PRINTING",
		PaperTrayStatus:   "LOW",
		EstimatedSheets:   190,
		TonerLevelPercent: 84,
		DrumLifePercent:   89,
		UpdatedAt:         time.Now().UTC(),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	// Send observation 1
	if err := wsjson.Write(ctx, conn, wireMessage{Type: "health", Health: &health1}); err != nil {
		t.Fatalf("failed to write health1: %v", err)
	}

	// Send observation 2
	if err := wsjson.Write(ctx, conn, wireMessage{Type: "health", Health: &health2}); err != nil {
		t.Fatalf("failed to write health2: %v", err)
	}

	// Poll until health2 is reflected
	deadline := time.Now().Add(2 * time.Second)
	var latest contracts.PrinterHealth
	var ok bool
	for time.Now().Before(deadline) {
		latest, ok = hub.LatestHealth("kiosk-health-1")
		if ok && latest.State == "PRINTING" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if !ok || latest.State != "PRINTING" || latest.PaperTrayStatus != "LOW" {
		t.Fatalf("expected latest health to be health2, got %+v (ok=%v)", latest, ok)
	}

	// Unknown kiosk returns false
	_, exists := hub.LatestHealth("kiosk-unknown")
	if exists {
		t.Fatalf("expected false for unknown kiosk, got true")
	}
}

// Test E — TestHubSendJobUnavailable
func TestHubSendJobUnavailable(t *testing.T) {
	hub := NewHub(nil)
	cmd := contracts.PrintJobCommand{
		JobID:         "job-1",
		KioskID:       "kiosk-offline",
		DownloadURL:   "http://example.com/file.pdf",
		TotalPages:    1,
		TotalSheets:   1,
		Copies:        1,
		Duplex:        contracts.DuplexSimplex,
		StartFromPage: 1,
	}

	err := hub.SendJob(context.Background(), cmd)
	if err == nil {
		t.Fatalf("expected error sending job to offline kiosk, got nil")
	}
}

// Test F — TestHubConnectionCleanup
func TestHubConnectionCleanup(t *testing.T) {
	hub := NewHub(nil)
	server := httptest.NewServer(hub)
	defer server.Close()

	conn, cancelConn := connectTestKiosk(t, hub, server.URL, "kiosk-cleanup-1")

	cmd := contracts.PrintJobCommand{
		JobID:         "job-cleanup-1",
		KioskID:       "kiosk-cleanup-1",
		DownloadURL:   "http://example.com/file.pdf",
		TotalPages:    1,
		TotalSheets:   1,
		Copies:        1,
		Duplex:        contracts.DuplexSimplex,
		StartFromPage: 1,
	}

	ctx := context.Background()

	// Verify job can be sent while connected
	if err := hub.SendJob(ctx, cmd); err != nil {
		t.Fatalf("SendJob failed while connected: %v", err)
	}

	// Close connection from client
	_ = conn.Close(websocket.StatusNormalClosure, "client closing")
	cancelConn()

	// Poll until SendJob reports unavailable
	deadline := time.Now().Add(2 * time.Second)
	cleanedUp := false
	for time.Now().Before(deadline) {
		err := hub.SendJob(ctx, cmd)
		if err != nil {
			cleanedUp = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if !cleanedUp {
		t.Fatalf("hub connection was not cleaned up after client disconnect")
	}
}

// Test G — TestHubConcurrentConnections
func TestHubConcurrentConnections(t *testing.T) {
	hub := NewHub(nil)
	server := httptest.NewServer(hub)
	defer server.Close()

	conn1, _ := connectTestKiosk(t, hub, server.URL, "kiosk-A")
	conn2, _ := connectTestKiosk(t, hub, server.URL, "kiosk-B")

	cmdA := contracts.PrintJobCommand{
		JobID:         "job-A",
		KioskID:       "kiosk-A",
		DownloadURL:   "http://example.com/a.pdf",
		TotalPages:    1,
		TotalSheets:   1,
		Copies:        1,
		Duplex:        contracts.DuplexSimplex,
		StartFromPage: 1,
	}

	cmdB := contracts.PrintJobCommand{
		JobID:         "job-B",
		KioskID:       "kiosk-B",
		DownloadURL:   "http://example.com/b.pdf",
		TotalPages:    2,
		TotalSheets:   2,
		Copies:        1,
		Duplex:        contracts.DuplexSimplex,
		StartFromPage: 1,
	}

	var wg sync.WaitGroup
	wg.Add(2)

	errChan := make(chan error, 2)

	go func() {
		defer wg.Done()
		if err := hub.SendJob(context.Background(), cmdA); err != nil {
			errChan <- err
		}
	}()

	go func() {
		defer wg.Done()
		if err := hub.SendJob(context.Background(), cmdB); err != nil {
			errChan <- err
		}
	}()

	wg.Wait()
	close(errChan)

	for err := range errChan {
		t.Fatalf("SendJob concurrent failed: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var msgA wireMessage
	if err := wsjson.Read(ctx, conn1, &msgA); err != nil {
		t.Fatalf("read conn1 failed: %v", err)
	}
	if msgA.Job == nil || msgA.Job.JobID != "job-A" {
		t.Fatalf("conn1 received wrong message: %+v", msgA)
	}

	var msgB wireMessage
	if err := wsjson.Read(ctx, conn2, &msgB); err != nil {
		t.Fatalf("read conn2 failed: %v", err)
	}
	if msgB.Job == nil || msgB.Job.JobID != "job-B" {
		t.Fatalf("conn2 received wrong message: %+v", msgB)
	}
}

package main

import (
	"context"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"xeroxgo/cloud-backend/internal/store"
	"xeroxgo/pkg/contracts"
)

// TestOfflineRecovery verifies the full offline recovery lifecycle:
// 1. Kiosk connects initially.
// 2. Kiosk disconnects (offline).
// 3. Payment is completed and verified while kiosk is offline; order enters PAID_WAITING.
// 4. Kiosk reconnects to the cloud hub.
// 5. Cloud hub automatically dispatches the paid job to the reconnected kiosk.
// 6. Kiosk receives the job and completes printing.
// 7. Kiosk disconnects and reconnects again; no duplicate reprint occurs (printed exactly once).
func TestOfflineRecovery_KioskReconnectReceivesPaidJobExactlyOnce(t *testing.T) {
	mockClient := &mockRazorpayClient{}
	app, mux, _, _, _ := setupPaymentWorkflowServer(t, mockClient)
	mux.Handle("/ws", app.hub)

	server := httptest.NewServer(mux)
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	const testKioskID = "kiosk-demo"
	const orderID = "ord_offline_rec_001"
	const jobID = "job_offline_rec_001"

	// 1. Seed an order with print configuration in PENDING_PAYMENT status
	ord := store.Order{
		OrderID: orderID,
		Command: contracts.PrintJobCommand{
			JobID:          jobID,
			KioskID:        testKioskID,
			DisplayCode:    "PIN-PROTECTED",
			DownloadURL:    server.URL + "/jobs/" + jobID + ".pdf",
			SHA256Checksum: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
			TotalPages:     4,
			TotalSheets:    2,
			Copies:         1,
			Duplex:         contracts.DuplexLongEdge,
			StartFromPage:  1,
		},
		Status:     contracts.JobStatusPendingPayment,
		PricePaise: 1000,
		CreatedAt:  time.Now().UTC(),
		UpdatedAt:  time.Now().UTC(),
	}

	if err := app.db.CreateOrder(ctx, ord); err != nil {
		t.Fatalf("CreateOrder failed: %v", err)
	}

	// 2. Kiosk connects initially
	wsURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("parse server url: %v", err)
	}
	wsURL.Scheme = "ws"
	wsURL.Path = "/ws"
	q := wsURL.Query()
	q.Set("kiosk_id", testKioskID)
	wsURL.RawQuery = q.Encode()

	conn1, _, err := websocket.Dial(ctx, wsURL.String(), nil)
	if err != nil {
		t.Fatalf("websocket.Dial failed for initial connection: %v", err)
	}

	// Verify connected
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if app.hub.IsConnected(testKioskID) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !app.hub.IsConnected(testKioskID) {
		t.Fatalf("expected kiosk %s to be connected initially", testKioskID)
	}

	// 3. Kiosk disconnects (network drop / power loss / offline)
	_ = conn1.Close(websocket.StatusNormalClosure, "disconnecting")

	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if !app.hub.IsConnected(testKioskID) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if app.hub.IsConnected(testKioskID) {
		t.Fatalf("expected kiosk %s to be disconnected", testKioskID)
	}

	// 4. Payment completed and verified while kiosk is OFFLINE
	// (Simulate webhook verification updating order to PAID_WAITING)
	if err := app.db.UpdateOrderStatus(ctx, orderID, contracts.JobStatusPaidWaiting); err != nil {
		t.Fatalf("UpdateOrderStatus to PAID_WAITING failed: %v", err)
	}

	// Verify order is now in PAID_WAITING in the store
	retrieved, err := app.db.GetOrder(ctx, orderID)
	if err != nil {
		t.Fatalf("GetOrder failed: %v", err)
	}
	if retrieved.Status != contracts.JobStatusPaidWaiting {
		t.Fatalf("expected order status PAID_WAITING, got %s", retrieved.Status)
	}

	// 5. Kiosk reconnects to the cloud hub
	conn2, _, err := websocket.Dial(ctx, wsURL.String(), nil)
	if err != nil {
		t.Fatalf("websocket.Dial failed for reconnect: %v", err)
	}
	defer conn2.Close(websocket.StatusNormalClosure, "test finish")

	// 6. Hub must proactively dispatch the pending PAID_WAITING job upon reconnection
	type wireMessage struct {
		Type     string                      `json:"type"`
		Job      *contracts.PrintJobCommand  `json:"job,omitempty"`
		Progress *contracts.JobProgressEvent `json:"progress,omitempty"`
	}

	var receivedJob *contracts.PrintJobCommand
	readDeadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(readDeadline) {
		var msg wireMessage
		readCtx, readCancel := context.WithTimeout(ctx, 2*time.Second)
		err := wsjson.Read(readCtx, conn2, &msg)
		readCancel()
		if err != nil {
			break
		}
		if msg.Type == "job" && msg.Job != nil {
			receivedJob = msg.Job
			break
		}
	}

	if receivedJob == nil {
		t.Fatalf("kiosk did not receive offline recovery print job upon reconnecting")
	}
	if receivedJob.JobID != jobID {
		t.Fatalf("received job ID mismatch: got %s, want %s", receivedJob.JobID, jobID)
	}

	// Verify order status on backend was updated to DISPATCHED
	afterDispatch, err := app.db.GetOrder(ctx, orderID)
	if err != nil {
		t.Fatalf("GetOrder after dispatch failed: %v", err)
	}
	if afterDispatch.Status != contracts.JobStatusDispatched {
		t.Fatalf("expected order status DISPATCHED, got %s", afterDispatch.Status)
	}

	// 7. Kiosk executes job and sends COMPLETED progress event
	completedEvent := contracts.JobProgressEvent{
		JobID:         jobID,
		KioskID:       testKioskID,
		Status:        contracts.JobStatusCompleted,
		PagesPrinted:  ord.Command.TotalPages,
		SheetsPrinted: ord.Command.TotalSheets,
		Timestamp:     time.Now().UTC(),
	}

	progressWire := wireMessage{
		Type:     "progress",
		Progress: &completedEvent,
	}
	if err := wsjson.Write(ctx, conn2, progressWire); err != nil {
		t.Fatalf("failed to send completed progress event: %v", err)
	}

	// Wait for db update to reflect COMPLETED
	deadline = time.Now().Add(3 * time.Second)
	orderCompleted := false
	for time.Now().Before(deadline) {
		checkOrd, err := app.db.GetOrder(ctx, orderID)
		if err == nil && checkOrd.Status == contracts.JobStatusCompleted {
			orderCompleted = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !orderCompleted {
		t.Fatalf("expected order to be marked COMPLETED in database")
	}

	// 8. Kiosk disconnects and reconnects again: verify NO duplicate reprint occurs
	_ = conn2.Close(websocket.StatusNormalClosure, "disconnecting before second reconnect")

	conn3, _, err := websocket.Dial(ctx, wsURL.String(), nil)
	if err != nil {
		t.Fatalf("websocket.Dial failed for second reconnect: %v", err)
	}
	defer conn3.Close(websocket.StatusNormalClosure, "cleanup")

	// Read with a short deadline: ensure no job message is received
	duplicateReceived := false
	shortCtx, shortCancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer shortCancel()

	for {
		var msg wireMessage
		err := wsjson.Read(shortCtx, conn3, &msg)
		if err != nil {
			break // Expected: read timed out without receiving a job
		}
		if msg.Type == "job" && msg.Job != nil && msg.Job.JobID == jobID {
			duplicateReceived = true
			break
		}
	}

	if duplicateReceived {
		t.Fatalf("duplicate print job was dispatched on second reconnection!")
	}
}

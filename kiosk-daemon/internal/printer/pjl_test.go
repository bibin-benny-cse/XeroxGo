package printer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"xeroxgo/pkg/contracts"
)

func samplePrintCommand(jobID string) contracts.PrintJobCommand {
	return contracts.PrintJobCommand{
		JobID:          jobID,
		KioskID:        "kiosk-01",
		DisplayCode:    "B72Y",
		DownloadURL:    "https://example.invalid/test.pdf",
		SHA256Checksum: "0123456789abcdef",
		TotalPages:     3,
		TotalSheets:    2,
		Copies:         1,
		Duplex:         contracts.DuplexSimplex,
		StartFromPage:  1,
	}
}

func startMockPrinter(t *testing.T) (net.Listener, string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start mock printer listener: %v", err)
	}
	return ln, ln.Addr().String()
}

func TestStreamPDFWithPJL(t *testing.T) {
	ln, addr := startMockPrinter(t)
	defer ln.Close()

	pdfPayload := []byte("%PDF-FAKE-PAYLOAD-12345\n%%EOF\n")
	cmd := samplePrintCommand("job-happy-path")

	var serverErr error
	var receivedData []byte
	var wg sync.WaitGroup
	wg.Add(1)

	go func() {
		defer wg.Done()
		conn, err := ln.Accept()
		if err != nil {
			serverErr = err
			return
		}
		defer conn.Close()

		// Read incoming stream until client's CloseWrite
		data, err := io.ReadAll(conn)
		if err != nil {
			serverErr = err
			return
		}
		receivedData = data

		// Send page status responses 1, 2, 3 and job end
		for i := 1; i <= 3; i++ {
			msg := fmt.Sprintf("@PJL USTATUS PAGE\r\n%d\r\n\f", i)
			if _, err := conn.Write([]byte(msg)); err != nil {
				serverErr = err
				return
			}
		}

		endMsg := "@PJL USTATUS JOB\r\nEND\r\n\f"
		if _, err := conn.Write([]byte(endMsg)); err != nil {
			serverErr = err
			return
		}
	}()

	var pageEvents []int
	var mu sync.Mutex
	onPage := func(sheetsCompleted int) {
		mu.Lock()
		defer mu.Unlock()
		pageEvents = append(pageEvents, sheetsCompleted)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := StreamPDFWithPJL(ctx, addr, cmd, bytes.NewReader(pdfPayload), onPage)
	if err != nil {
		t.Fatalf("StreamPDFWithPJL failed: %v", err)
	}

	wg.Wait()
	if serverErr != nil {
		t.Fatalf("mock server error: %v", serverErr)
	}

	// Verify Header
	if !strings.Contains(string(receivedData), "@PJL JOB NAME=\"job-happy-path\"\r\n") {
		t.Errorf("expected job name header in received data")
	}
	if !strings.Contains(string(receivedData), "@PJL ENTER LANGUAGE=PDF\r\n") {
		t.Errorf("expected ENTER LANGUAGE=PDF in received data")
	}

	// Verify PDF payload exists unchanged after ENTER LANGUAGE=PDF
	parts := strings.Split(string(receivedData), "@PJL ENTER LANGUAGE=PDF\r\n")
	if len(parts) < 2 {
		t.Fatalf("ENTER LANGUAGE=PDF not found in received stream")
	}
	if !strings.HasPrefix(parts[1], string(pdfPayload)) {
		t.Errorf("PDF payload does not match expected bytes")
	}

	// Verify Trailer
	if !strings.Contains(string(receivedData), "@PJL EOJ NAME=\"job-happy-path\"\r\n") {
		t.Errorf("expected EOJ trailer in received data")
	}

	// Verify callbacks
	mu.Lock()
	defer mu.Unlock()
	expectedEvents := []int{1, 2, 3}
	if !reflect.DeepEqual(pageEvents, expectedEvents) {
		t.Errorf("pageEvents = %v, want %v", pageEvents, expectedEvents)
	}
}

func TestStreamPDFWithPJL_DuplexModes(t *testing.T) {
	tests := []struct {
		name          string
		duplex        contracts.DuplexMode
		copies        int
		wantHeader    string
		wantCopiesStr string
	}{
		{
			name:          "simplex",
			duplex:        contracts.DuplexSimplex,
			copies:        1,
			wantHeader:    "@PJL SET DUPLEX=OFF\r\n",
			wantCopiesStr: "@PJL SET COPIES=1\r\n",
		},
		{
			name:          "long_edge",
			duplex:        contracts.DuplexLongEdge,
			copies:        2,
			wantHeader:    "@PJL SET DUPLEX=ON\r\n@PJL SET BINDING=LONGEDGE\r\n",
			wantCopiesStr: "@PJL SET COPIES=2\r\n",
		},
		{
			name:          "short_edge",
			duplex:        contracts.DuplexShortEdge,
			copies:        3,
			wantHeader:    "@PJL SET DUPLEX=ON\r\n@PJL SET BINDING=SHORTEDGE\r\n",
			wantCopiesStr: "@PJL SET COPIES=3\r\n",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ln, addr := startMockPrinter(t)
			defer ln.Close()

			cmd := samplePrintCommand("job-duplex")
			cmd.Duplex = tc.duplex
			cmd.Copies = tc.copies

			var receivedData []byte
			var wg sync.WaitGroup
			wg.Add(1)

			go func() {
				defer wg.Done()
				conn, err := ln.Accept()
				if err != nil {
					return
				}
				defer conn.Close()

				data, _ := io.ReadAll(conn)
				receivedData = data
				_, _ = conn.Write([]byte("@PJL USTATUS JOB\r\nEND\r\n\f"))
			}()

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			err := StreamPDFWithPJL(ctx, addr, cmd, strings.NewReader("%PDF"), nil)
			if err != nil {
				t.Fatalf("StreamPDFWithPJL failed: %v", err)
			}

			wg.Wait()
			receivedStr := string(receivedData)
			if !strings.Contains(receivedStr, tc.wantHeader) {
				t.Errorf("expected header %q not found in:\n%s", tc.wantHeader, receivedStr)
			}
			if !strings.Contains(receivedStr, tc.wantCopiesStr) {
				t.Errorf("expected copies setting %q not found in:\n%s", tc.wantCopiesStr, receivedStr)
			}
		})
	}
}

func TestStreamPDFWithPJL_RejectsInvalidInputs(t *testing.T) {
	ctx := context.Background()
	canceledCtx, cancel := context.WithCancel(ctx)
	cancel()

	baseCmd := samplePrintCommand("job-valid")

	tests := []struct {
		name      string
		ctx       context.Context
		addr      string
		cmd       contracts.PrintJobCommand
		pdfReader io.Reader
	}{
		{"nil_context", nil, "127.0.0.1:9100", baseCmd, strings.NewReader("%PDF")},
		{"canceled_context", canceledCtx, "127.0.0.1:9100", baseCmd, strings.NewReader("%PDF")},
		{"empty_address", ctx, "", baseCmd, strings.NewReader("%PDF")},
		{"nil_reader", ctx, "127.0.0.1:9100", baseCmd, nil},
		{"empty_job_id", ctx, "127.0.0.1:9100", func() contracts.PrintJobCommand { c := baseCmd; c.JobID = ""; return c }(), strings.NewReader("%PDF")},
		{"zero_copies", ctx, "127.0.0.1:9100", func() contracts.PrintJobCommand { c := baseCmd; c.Copies = 0; return c }(), strings.NewReader("%PDF")},
		{"too_many_copies", ctx, "127.0.0.1:9100", func() contracts.PrintJobCommand { c := baseCmd; c.Copies = 1000; return c }(), strings.NewReader("%PDF")},
		{"zero_total_pages", ctx, "127.0.0.1:9100", func() contracts.PrintJobCommand { c := baseCmd; c.TotalPages = 0; return c }(), strings.NewReader("%PDF")},
		{"zero_total_sheets", ctx, "127.0.0.1:9100", func() contracts.PrintJobCommand { c := baseCmd; c.TotalSheets = 0; return c }(), strings.NewReader("%PDF")},
		{"invalid_duplex", ctx, "127.0.0.1:9100", func() contracts.PrintJobCommand { c := baseCmd; c.Duplex = "INVALID"; return c }(), strings.NewReader("%PDF")},
		{"start_page_zero", ctx, "127.0.0.1:9100", func() contracts.PrintJobCommand { c := baseCmd; c.StartFromPage = 0; return c }(), strings.NewReader("%PDF")},
		{"start_page_gt_total", ctx, "127.0.0.1:9100", func() contracts.PrintJobCommand { c := baseCmd; c.TotalPages = 3; c.StartFromPage = 4; return c }(), strings.NewReader("%PDF")},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := StreamPDFWithPJL(tc.ctx, tc.addr, tc.cmd, tc.pdfReader, nil)
			if err == nil {
				t.Fatalf("expected error for case %s, got nil", tc.name)
			}
		})
	}
}

type errReader struct {
	err error
}

func (r *errReader) Read(p []byte) (n int, err error) {
	return 0, r.err
}

func TestStreamPDFWithPJL_ReaderFailure(t *testing.T) {
	ln, addr := startMockPrinter(t)
	defer ln.Close()

	cmd := samplePrintCommand("job-err-reader")
	var receivedData []byte
	var wg sync.WaitGroup
	wg.Add(1)

	go func() {
		defer wg.Done()
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()

		data, _ := io.ReadAll(conn)
		receivedData = data
	}()

	controlledErr := errors.New("controlled reader error")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := StreamPDFWithPJL(ctx, addr, cmd, &errReader{err: controlledErr}, nil)
	if err == nil {
		t.Fatal("expected error from reader failure, got nil")
	}
	if !errors.Is(err, controlledErr) && !strings.Contains(err.Error(), controlledErr.Error()) {
		t.Errorf("expected error %v, got %v", controlledErr, err)
	}

	wg.Wait()
	// EOJ trailer must NOT be transmitted
	if strings.Contains(string(receivedData), "@PJL EOJ") {
		t.Errorf("EOJ trailer was transmitted despite reader error")
	}
}

func TestStreamPDFWithPJL_PageNotifications(t *testing.T) {
	ln, addr := startMockPrinter(t)
	defer ln.Close()

	cmd := samplePrintCommand("job-page-notifications")
	var wg sync.WaitGroup
	wg.Add(1)

	go func() {
		defer wg.Done()
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()

		_, _ = io.ReadAll(conn)

		// Sequence of events: 1, 2, 2, not-a-number, 0, -1, 3, then JOB END
		events := []string{
			"@PJL USTATUS PAGE\r\n1\r\n\f",
			"@PJL USTATUS PAGE\r\n2\r\n\f",
			"@PJL USTATUS PAGE\r\n2\r\n\f",
			"@PJL USTATUS PAGE\r\nnot-a-number\r\n\f",
			"@PJL USTATUS PAGE\r\n0\r\n\f",
			"@PJL USTATUS PAGE\r\n-1\r\n\f",
			"@PJL USTATUS PAGE\r\n3\r\n\f",
			"@PJL USTATUS JOB\r\nEND\r\n\f",
		}

		for _, msg := range events {
			_, _ = conn.Write([]byte(msg))
		}
	}()

	var receivedPages []int
	var mu sync.Mutex
	onPage := func(sheetsCompleted int) {
		mu.Lock()
		defer mu.Unlock()
		receivedPages = append(receivedPages, sheetsCompleted)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := StreamPDFWithPJL(ctx, addr, cmd, strings.NewReader("%PDF"), onPage)
	if err != nil {
		t.Fatalf("StreamPDFWithPJL failed: %v", err)
	}

	wg.Wait()
	mu.Lock()
	defer mu.Unlock()

	expectedPages := []int{1, 2, 3}
	if !reflect.DeepEqual(receivedPages, expectedPages) {
		t.Errorf("receivedPages = %v, want %v", receivedPages, expectedPages)
	}
}

func TestStreamPDFWithPJL_JobNameInjection(t *testing.T) {
	ln, addr := startMockPrinter(t)
	defer ln.Close()

	maliciousJobID := "job\r\n@PJL SET COPIES=999\r\n@PJL EOJ\r\n\"malicious\""
	cmd := samplePrintCommand(maliciousJobID)

	var receivedData []byte
	var wg sync.WaitGroup
	wg.Add(1)

	go func() {
		defer wg.Done()
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()

		data, _ := io.ReadAll(conn)
		receivedData = data
		_, _ = conn.Write([]byte("@PJL USTATUS JOB\r\nEND\r\n\f"))
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	pdfContent := "%PDF-INJECTION-TEST"
	err := StreamPDFWithPJL(ctx, addr, cmd, strings.NewReader(pdfContent), nil)
	if err != nil {
		t.Fatalf("StreamPDFWithPJL failed: %v", err)
	}

	wg.Wait()
	receivedStr := string(receivedData)

	// Injected copies=999 command must NOT exist
	if strings.Contains(receivedStr, "@PJL SET COPIES=999\r\n") {
		t.Errorf("job ID injection succeeded! Received data contains injected commands")
	}

	// Sanitized name should be safely escaped
	safeName := sanitizePJLJobName(maliciousJobID)
	if strings.Contains(safeName, "\r") || strings.Contains(safeName, "\n") || strings.Contains(safeName, "\"") {
		t.Errorf("sanitizePJLJobName allowed illegal characters: %q", safeName)
	}
	if !strings.Contains(receivedStr, fmt.Sprintf("@PJL JOB NAME=\"%s\"\r\n", safeName)) {
		t.Errorf("expected sanitized job name in header")
	}
	if !strings.Contains(receivedStr, fmt.Sprintf("@PJL EOJ NAME=\"%s\"\r\n", safeName)) {
		t.Errorf("expected sanitized job name in trailer")
	}
}

func TestStreamPDFWithPJL_CancelWhileWaiting(t *testing.T) {
	ln, addr := startMockPrinter(t)
	defer ln.Close()

	cmd := samplePrintCommand("job-cancel")
	fullStreamReceived := make(chan struct{})

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()

		// Read entire outgoing stream
		_, _ = io.ReadAll(conn)
		close(fullStreamReceived)

		// Never send completion event or close; hold connection open until client cancels
		select {}
	}()

	ctx, cancel := context.WithCancel(context.Background())

	errCh := make(chan error, 1)
	go func() {
		errCh <- StreamPDFWithPJL(ctx, addr, cmd, strings.NewReader("%PDF"), nil)
	}()

	// Wait until mock has confirmed entire stream was received
	select {
	case <-fullStreamReceived:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for mock to receive full stream")
	}

	// Now cancel context while client is waiting for completion
	cancel()

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("expected error on context cancellation, got nil")
		}
		if !errors.Is(err, context.Canceled) && !strings.Contains(err.Error(), "canceled") {
			t.Errorf("expected context cancellation error, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("StreamPDFWithPJL did not unblock after context cancellation")
	}
}

func TestStreamPDFWithPJL_PrinterClosesConnection(t *testing.T) {
	ln, addr := startMockPrinter(t)
	defer ln.Close()

	cmd := samplePrintCommand("job-clean-close")

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		// Read entire stream, then close cleanly without sending JOB END
		_, _ = io.ReadAll(conn)
		_ = conn.Close()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := StreamPDFWithPJL(ctx, addr, cmd, strings.NewReader("%PDF-PAYLOAD"), nil)
	if err != nil {
		t.Fatalf("expected clean EOF after write to succeed, got %v", err)
	}
}

func TestStreamPDFWithPJL_CallbackOptional(t *testing.T) {
	ln, addr := startMockPrinter(t)
	defer ln.Close()

	cmd := samplePrintCommand("job-nil-callback")

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()

		_, _ = io.ReadAll(conn)
		_, _ = conn.Write([]byte("@PJL USTATUS PAGE\r\n1\r\n\f@PJL USTATUS JOB\r\nEND\r\n\f"))
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// onPage == nil must not panic
	err := StreamPDFWithPJL(ctx, addr, cmd, strings.NewReader("%PDF"), nil)
	if err != nil {
		t.Fatalf("StreamPDFWithPJL failed with nil callback: %v", err)
	}
}

func TestStreamPDFWithPJL_WriteFailure(t *testing.T) {
	ln, addr := startMockPrinter(t)
	defer ln.Close()

	cmd := samplePrintCommand("job-write-failure")

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		// Read a few bytes and immediately close connection
		buf := make([]byte, 16)
		_, _ = conn.Read(buf)
		_ = conn.Close()
	}()

	// 2 MB reader that will fail to complete writing to closed connection
	largePayload := make([]byte, 2*1024*1024)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := StreamPDFWithPJL(ctx, addr, cmd, bytes.NewReader(largePayload), nil)
	if err == nil {
		t.Fatal("expected write failure error, got nil")
	}
}

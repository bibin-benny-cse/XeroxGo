package printer

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"xeroxgo/pkg/contracts"
)

func TestDevPrinter_PureGoPageCounting(t *testing.T) {
	singlePage := []byte("%PDF-1.4\n1 0 obj << /Type /Catalog /Pages 2 0 R >> endobj\n2 0 obj << /Type /Pages /Kids [3 0 R] /Count 1 >> endobj\n3 0 obj << /Type /Page >> endobj\nxref\ntrailer\nstartxref\n%%EOF")
	if count := CountPDFPagesPureGo(singlePage); count != 1 {
		t.Errorf("expected 1 page, got %d", count)
	}

	multiPage := []byte("%PDF-1.4\n3 0 obj << /Type /Page >> endobj\n4 0 obj << /Type /Page >> endobj\n5 0 obj << /Type /Page >> endobj\n")
	if count := CountPDFPagesPureGo(multiPage); count != 3 {
		t.Errorf("expected 3 pages, got %d", count)
	}

	empty := []byte("")
	if count := CountPDFPagesPureGo(empty); count != 1 {
		t.Errorf("expected default 1 page for empty, got %d", count)
	}
}

func TestDevPrinter_StartAndStreamPJL(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	tmpDir := t.TempDir()

	addr, cleanup, err := StartDevPrinter(ctx, tmpDir, &wg)
	if err != nil {
		t.Fatalf("StartDevPrinter failed: %v", err)
	}
	defer cleanup()

	// Connect and send PJL stream
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("failed to dial dev printer: %v", err)
	}
	defer conn.Close()

	// 2-page PDF payload with 2 copies, simplex -> 4 sheets
	pdfContent := "%PDF-1.4\n1 0 obj << /Type /Page >> endobj\n2 0 obj << /Type /Page >> endobj\n"
	pjlHeader := "\x1b%-12345X@PJL\r\n@PJL SET COPIES=2\r\n@PJL ENTER LANGUAGE=PDF\r\n"
	pjlTrailer := "\x1b%-12345X@PJL EOJ\r\n"

	stream := pjlHeader + pdfContent + pjlTrailer
	if _, err := conn.Write([]byte(stream)); err != nil {
		t.Fatalf("failed to write PJL stream: %v", err)
	}

	// Read response
	buf := make([]byte, 4096)
	var resp bytes.Buffer
	for {
		_ = conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		n, rerr := conn.Read(buf)
		if n > 0 {
			resp.Write(buf[:n])
		}
		if rerr != nil {
			break
		}
	}

	respStr := resp.String()
	if !strings.Contains(respStr, "@PJL USTATUS PAGE\r\n1\r\n") {
		t.Errorf("missing page 1 status in response: %s", respStr)
	}
	if !strings.Contains(respStr, "@PJL USTATUS PAGE\r\n4\r\n") {
		t.Errorf("missing page 4 status (2 pages * 2 copies) in response: %s", respStr)
	}
	if !strings.Contains(respStr, "@PJL USTATUS JOB\r\nEND") {
		t.Errorf("missing JOB END status in response: %s", respStr)
	}
}

func TestDevPrinter_HealthTelemetry(t *testing.T) {
	h := DevPrinterHealth(context.Background(), 250)
	if !h.Online {
		t.Error("expected dev printer to report online")
	}
	if h.State != "IDLE" {
		t.Errorf("expected IDLE state, got %s", h.State)
	}
	if h.EstimatedSheets != 250 {
		t.Errorf("expected 250 estimated sheets, got %d", h.EstimatedSheets)
	}
}

func TestDevPrinter_IntegrationWithStreamPDFWithPJL(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	tmpDir := t.TempDir()

	addr, cleanup, err := StartDevPrinter(ctx, tmpDir, &wg)
	if err != nil {
		t.Fatalf("StartDevPrinter failed: %v", err)
	}
	defer cleanup()

	cmd := contracts.PrintJobCommand{
		JobID:         "sim_job_001",
		TotalPages:    1,
		TotalSheets:   1,
		Copies:        1,
		Duplex:        contracts.DuplexSimplex,
		StartFromPage: 1,
	}

	pdfData := []byte("%PDF-1.4\n1 0 obj << /Type /Page >> endobj\n")
	var pagesReported []int
	onPage := func(sheetsCompleted int) {
		pagesReported = append(pagesReported, sheetsCompleted)
	}

	err = StreamPDFWithPJL(ctx, addr, cmd, bytes.NewReader(pdfData), onPage)
	if err != nil {
		t.Fatalf("StreamPDFWithPJL failed against DevPrinter: %v", err)
	}

	if len(pagesReported) == 0 {
		t.Fatalf("expected at least one page status callback, got none")
	}
	fmt.Printf("Dev printer successfully processed print job: reported sheets: %v\n", pagesReported)
}

package printer

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"xeroxgo/pkg/contracts"
)

// DevPrinterAdapter simulates a physical network laser printer (Brother HL-L5210DN PJL emulation)
// on a local TCP loopback port for Windows development. It processes PJL byte streams,
// records sheets, and feeds status events back to the spooler without claiming physical printing.
type DevPrinterAdapter struct {
	addr        string
	listener    net.Listener
	downloadDir string
	closeOnce   sync.Once
	totalJobs   int
	totalSheets int
	mu          sync.Mutex
}

// StartDevPrinter binds a local TCP listener to an ephemeral port (127.0.0.1:0)
// and handles incoming PJL print streams asynchronously.
func StartDevPrinter(ctx context.Context, downloadDir string, wg *sync.WaitGroup) (string, func(), error) {
	if downloadDir == "" {
		downloadDir = filepath.Join("data", "jobs")
	}
	_ = os.MkdirAll(downloadDir, 0700)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, fmt.Errorf("failed to bind dev printer on loopback: %w", err)
	}

	adapter := &DevPrinterAdapter{
		addr:        ln.Addr().String(),
		listener:    ln,
		downloadDir: downloadDir,
	}

	cleanup := func() {
		adapter.closeOnce.Do(func() {
			_ = ln.Close()
		})
	}

	if wg != nil {
		wg.Add(1)
	}

	go func() {
		if wg != nil {
			defer wg.Done()
		}
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go adapter.handleConn(ctx, conn)
		}
	}()

	log.Printf("[SIMULATED PRINTER] Dev printer listening on %s (Emulating Brother HL-L5210DN)", adapter.addr)
	return adapter.addr, cleanup, nil
}

// DevPrinterHealth returns healthy simulated telemetry for the development printer.
func DevPrinterHealth(ctx context.Context, estimatedSheets int) contracts.PrinterHealth {
	return contracts.PrinterHealth{
		Online:            true,
		State:             "IDLE",
		PaperTrayStatus:   "OK",
		EstimatedSheets:   estimatedSheets,
		TonerLevelPercent: 95,
		DrumLifePercent:   92,
		CoverOpen:         false,
		PaperJam:          false,
		OutputBinFull:     false,
		UpdatedAt:         time.Now().UTC(),
	}
}

func (a *DevPrinterAdapter) handleConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()

	var buf bytes.Buffer
	chunk := make([]byte, 32*1024)
	const maxRead = 300 * 1024 * 1024 // 300 MiB safety limit

	for buf.Len() < maxRead {
		n, rerr := conn.Read(chunk)
		if n > 0 {
			buf.Write(chunk[:n])
			if bytes.Contains(buf.Bytes(), []byte("@PJL EOJ")) {
				break
			}
		}
		if rerr != nil {
			break
		}
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

	// 1. Determine page count
	pages := a.extractPageCount(pdfBytes)

	// 2. Determine copies
	copies := 1
	reCopies := regexp.MustCompile(`@PJL\s+SET\s+COPIES\s*=\s*(\d+)`)
	if m := reCopies.FindSubmatch(streamBytes[:headerIdx]); len(m) > 1 {
		if c, cerr := strconv.Atoi(string(m[1])); cerr == nil && c > 0 {
			copies = c
		}
	}

	// 3. Determine duplex mode
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

	a.mu.Lock()
	a.totalJobs++
	a.totalSheets += totalSheets
	jobNum := a.totalJobs
	a.mu.Unlock()

	log.Printf("[SIMULATED PRINTER] Job #%d received: %d pages, %d copies, duplex=%v -> simulating %d physical sheets",
		jobNum, pages, copies, isDuplex, totalSheets)
	log.Printf("[SIMULATED PRINTER] (Windows Development Mode — simulated job execution only; no physical hardware or paper consumed)")

	// 4. Stream status updates back to spooler client
	for sheet := 1; sheet <= totalSheets; sheet++ {
		select {
		case <-ctx.Done():
			return
		case <-time.After(60 * time.Millisecond):
		}

		msg := fmt.Sprintf("@PJL USTATUS PAGE\r\n%d\r\n", sheet)
		if _, err := conn.Write([]byte(msg)); err != nil {
			return
		}
		log.Printf("[SIMULATED PRINTER] Sheet %d/%d printed (simulated)", sheet, totalSheets)
	}

	time.Sleep(40 * time.Millisecond)
	_, _ = conn.Write([]byte("@PJL USTATUS JOB\r\nEND\r\n\f"))
	log.Printf("[SIMULATED PRINTER] Job #%d finished successfully. Recorded in simulated state store.", jobNum)
}

func (a *DevPrinterAdapter) extractPageCount(pdfBytes []byte) int {
	// Attempt qpdf if available
	tmpFile, err := os.CreateTemp(a.downloadDir, "sim_page_*.pdf")
	if err == nil {
		tmpName := tmpFile.Name()
		defer func() {
			_ = tmpFile.Close()
			_ = os.Remove(tmpName)
		}()

		if _, werr := tmpFile.Write(pdfBytes); werr == nil {
			_ = tmpFile.Close()
			cmd := exec.Command("qpdf", "--show-npages", tmpName)
			out, cerr := cmd.CombinedOutput()
			if cerr == nil {
				if p, perr := strconv.Atoi(strings.TrimSpace(string(out))); perr == nil && p > 0 {
					return p
				}
			}
		}
	}

	// Pure-Go fallback: count /Type /Page (excluding /Pages) in PDF stream
	return CountPDFPagesPureGo(pdfBytes)
}

// CountPDFPagesPureGo extracts the page count directly from PDF bytes using regex,
// providing an exact fallback when qpdf is not installed in the developer's PATH.
func CountPDFPagesPureGo(pdfBytes []byte) int {
	if len(pdfBytes) == 0 {
		return 1
	}

	// Match /Type /Page not followed by 's'
	re := regexp.MustCompile(`/Type\s*/Page\b`)
	matches := re.FindAll(pdfBytes, -1)
	if len(matches) > 0 {
		return len(matches)
	}

	return 1
}

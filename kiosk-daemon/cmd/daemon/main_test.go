package main

import (
	"context"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestMockPrinterShutdownUnblocksListener(t *testing.T) {
	tempDir := t.TempDir()
	var wg sync.WaitGroup
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	addr, cleanup, err := startMockPrinter(ctx, tempDir, &wg)
	if err != nil {
		t.Fatalf("startMockPrinter failed: %v", err)
	}
	if addr == "" {
		t.Fatal("expected non-empty mock printer address")
	}

	// Verify listener accepts TCP connections (or skip dial if blocked by OS sandbox loopback security)
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		if strings.Contains(err.Error(), "access permissions") || strings.Contains(err.Error(), "forbidden") {
			t.Logf("Skipping loopback TCP dial in sandbox: %v", err)
		} else {
			t.Fatalf("failed to dial mock printer listener: %v", err)
		}
	} else {
		_ = conn.Close()
	}

	// Invoke cleanup, which must close the listener and unblock the listener goroutine
	cleanup()

	// Calling cleanup again should be safe and idempotent
	cleanup()

	// Verify listener goroutine exits promptly without hanging wg.Wait()
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		// Succeeded: listener goroutine exited promptly
	case <-time.After(3 * time.Second):
		t.Fatal("deadlock: startMockPrinter listener goroutine did not exit within 3s after cleanup")
	}
}

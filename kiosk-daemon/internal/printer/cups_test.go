package printer

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"xeroxgo/pkg/contracts"
)

func TestPrintPDFWithCUPS_Validation(t *testing.T) {
	validCmd := contracts.PrintJobCommand{
		JobID:       "job-123",
		Copies:      1,
		TotalPages:  1,
		TotalSheets: 1,
		Duplex:      contracts.DuplexSimplex,
	}

	runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		return []byte("request id is DCPT310-1 (1 file(s))"), nil
	}

	tests := []struct {
		name    string
		ctx     context.Context
		queue   string
		cmd     contracts.PrintJobCommand
		pdfPath string
		wantErr string
	}{
		{
			name:    "nil context",
			ctx:     nil,
			queue:   "DCPT310",
			cmd:     validCmd,
			pdfPath: "/tmp/test.pdf",
			wantErr: "context cannot be nil",
		},
		{
			name:    "empty queue",
			ctx:     context.Background(),
			queue:   "",
			cmd:     validCmd,
			pdfPath: "/tmp/test.pdf",
			wantErr: "queue name cannot be empty",
		},
		{
			name:    "empty pdf path",
			ctx:     context.Background(),
			queue:   "DCPT310",
			cmd:     validCmd,
			pdfPath: "",
			wantErr: "pdf path cannot be empty",
		},
		{
			name:    "empty job id",
			ctx:     context.Background(),
			queue:   "DCPT310",
			cmd:     contracts.PrintJobCommand{Copies: 1, TotalSheets: 1},
			pdfPath: "/tmp/test.pdf",
			wantErr: "job_id cannot be empty",
		},
		{
			name:    "zero copies",
			ctx:     context.Background(),
			queue:   "DCPT310",
			cmd:     contracts.PrintJobCommand{JobID: "j1", Copies: 0, TotalSheets: 1},
			pdfPath: "/tmp/test.pdf",
			wantErr: "copies must be between 1 and 999",
		},
		{
			name:    "zero sheets",
			ctx:     context.Background(),
			queue:   "DCPT310",
			cmd:     contracts.PrintJobCommand{JobID: "j1", Copies: 1, TotalSheets: 0},
			pdfPath: "/tmp/test.pdf",
			wantErr: "total_sheets must be >= 1",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := PrintPDFWithCUPSCustom(tc.ctx, runner, tc.queue, tc.cmd, tc.pdfPath, nil, 10*time.Millisecond)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("expected error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

func TestPrintPDFWithCUPS_Success(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var mu sync.Mutex
	lpstatCalls := 0
	var executedCommands []string

	runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		mu.Lock()
		defer mu.Unlock()
		cmdStr := name + " " + strings.Join(args, " ")
		executedCommands = append(executedCommands, cmdStr)

		switch name {
		case "lp":
			return []byte("request id is DCPT310-42 (1 file(s))\n"), nil
		case "lpstat":
			if len(args) > 1 && args[1] == "completed" {
				return []byte("DCPT310-42 username 1024 Tue Nov 3 20:30:00 2026\n\tAlerts: job-completed-successfully\n\tqueued for DCPT310\n"), nil
			}
			lpstatCalls++
			if lpstatCalls == 1 {
				return []byte("DCPT310-42 username 1024 Tue Nov 3 20:30:00 2026\n"), nil
			}
			// Second call: job finished, no longer active
			return []byte(""), nil
		default:
			return nil, errors.New("unexpected command: " + name)
		}
	}

	cmd := contracts.PrintJobCommand{
		JobID:       "job-xyz",
		Copies:      2,
		TotalPages:  4,
		TotalSheets: 4,
		Duplex:      contracts.DuplexLongEdge,
	}

	var completedSheets []int
	onPage := func(sheets int) {
		mu.Lock()
		completedSheets = append(completedSheets, sheets)
		mu.Unlock()
	}

	err := PrintPDFWithCUPSCustom(ctx, runner, "DCPT310", cmd, "/var/kiosk/job-xyz.pdf", onPage, 10*time.Millisecond)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	if len(executedCommands) < 2 {
		t.Fatalf("expected at least 2 commands executed, got %d", len(executedCommands))
	}

	if !strings.Contains(executedCommands[0], "lp -d DCPT310 -n 2 -t job-xyz -o sides=two-sided-long-edge /var/kiosk/job-xyz.pdf") {
		t.Errorf("unexpected lp invocation: %s", executedCommands[0])
	}

	if len(completedSheets) == 0 || completedSheets[len(completedSheets)-1] != 4 {
		t.Errorf("expected final progress to be 4 sheets, got %v", completedSheets)
	}
}

func TestPrintPDFWithCUPS_Success_IPP(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var mu sync.Mutex
	ippCalls := 0
	var completedSheets []int

	runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		mu.Lock()
		defer mu.Unlock()

		switch name {
		case "lp":
			return []byte("request id is HL-L5210DN-101 (1 file(s))\n"), nil
		case "ipptool":
			ippCalls++
			if ippCalls == 1 {
				// 1st poll: processing, 2 sheets completed
				return []byte(`
job-id (integer) = 101
job-state (enum) = processing
job-state-reasons (keyword) = none
job-media-sheets-completed (integer) = 2
`), nil
			}
			// 2nd poll: confirmed completed
			return []byte(`
job-id (integer) = 101
job-state (enum) = completed
job-state-reasons (keyword) = job-completed-successfully
job-media-sheets-completed (integer) = 4
`), nil
		default:
			return nil, errors.New("unexpected command: " + name)
		}
	}

	cmd := contracts.PrintJobCommand{
		JobID:       "job-ipp-1",
		Copies:      1,
		TotalPages:  8,
		TotalSheets: 4,
		Duplex:      contracts.DuplexLongEdge,
	}

	onPage := func(sheets int) {
		mu.Lock()
		completedSheets = append(completedSheets, sheets)
		mu.Unlock()
	}

	err := PrintPDFWithCUPSCustom(ctx, runner, "HL-L5210DN", cmd, "/var/kiosk/job-ipp.pdf", onPage, 10*time.Millisecond)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	// Verifies real sheet progress (2 then 4)
	if len(completedSheets) != 2 || completedSheets[0] != 2 || completedSheets[1] != 4 {
		t.Errorf("expected completed sheets [2, 4], got %v", completedSheets)
	}
}

func TestPrintPDFWithCUPS_LPError(t *testing.T) {
	ctx := context.Background()
	runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		return []byte("lp: error - unknown printer"), errors.New("exit status 1")
	}

	cmd := contracts.PrintJobCommand{
		JobID:       "job-err",
		Copies:      1,
		TotalSheets: 1,
		Duplex:      contracts.DuplexSimplex,
	}

	err := PrintPDFWithCUPSCustom(ctx, runner, "DCPT310", cmd, "/tmp/test.pdf", nil, 10*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "failed to submit job to CUPS queue") {
		t.Fatalf("expected lp error, got %v", err)
	}
}

func TestPrintPDFWithCUPS_QueueStoppedError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		switch name {
		case "lp":
			return []byte("request id is DCPT310-55 (1 file(s))\n"), nil
		case "lpstat":
			if len(args) > 0 && args[0] == "-p" {
				return []byte("printer DCPT310 is stopped; reason: Filter failed"), nil
			}
			return []byte("DCPT310-55 username 1024 Tue Nov 3 20:30:00 2026\n"), nil
		default:
			return []byte(""), nil
		}
	}

	cmd := contracts.PrintJobCommand{
		JobID:       "job-fail",
		Copies:      1,
		TotalSheets: 1,
		Duplex:      contracts.DuplexSimplex,
	}

	err := PrintPDFWithCUPSCustom(ctx, runner, "DCPT310", cmd, "/var/kiosk/job-fail.pdf", nil, 5*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "stopped/disabled") {
		t.Fatalf("expected stopped/disabled error, got %v", err)
	}
}

func TestPrintPDFWithCUPS_Aborted_FilterFailed(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		switch name {
		case "lp":
			return []byte("request id is HPTank1020w-202 (1 file(s))\n"), nil
		case "ipptool":
			return []byte(`
job-id (integer) = 202
job-state (enum) = aborted
job-state-reasons (keyword) = filter-failed
`), nil
		default:
			return []byte(""), nil
		}
	}

	cmd := contracts.PrintJobCommand{
		JobID:       "job-abort",
		Copies:      1,
		TotalSheets: 2,
		Duplex:      contracts.DuplexSimplex,
	}

	err := PrintPDFWithCUPSCustom(ctx, runner, "HPTank1020w", cmd, "/tmp/abort.pdf", nil, 5*time.Millisecond)
	if err == nil || (!strings.Contains(err.Error(), "aborted") && !strings.Contains(err.Error(), "stopped/disabled")) {
		t.Fatalf("expected aborted error, got %v", err)
	}
}

func TestPrintPDFWithCUPS_Canceled(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		switch name {
		case "lp":
			return []byte("request id is DCPT310-88 (1 file(s))\n"), nil
		case "ipptool":
			return []byte(`
job-id (integer) = 88
job-state (enum) = canceled
job-state-reasons (keyword) = job-canceled-by-user
`), nil
		default:
			return []byte(""), nil
		}
	}

	cmd := contracts.PrintJobCommand{
		JobID:       "job-canceled",
		Copies:      1,
		TotalSheets: 2,
		Duplex:      contracts.DuplexSimplex,
	}

	err := PrintPDFWithCUPSCustom(ctx, runner, "DCPT310", cmd, "/tmp/canceled.pdf", nil, 5*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "canceled") {
		t.Fatalf("expected canceled error, got %v", err)
	}
}

func TestPrintPDFWithCUPS_ProcessingStopped_Recovers(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var pollCount int
	var mu sync.Mutex

	runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		mu.Lock()
		defer mu.Unlock()

		switch name {
		case "lp":
			return []byte("request id is DCPT310-77 (1 file(s))\n"), nil
		case "ipptool":
			pollCount++
			if pollCount == 1 {
				// 1st poll: out of paper (recoverable pause)
				return []byte(`
job-id (integer) = 77
job-state (enum) = processing-stopped
job-state-reasons (keyword) = media-empty
`), nil
			}
			if pollCount == 2 {
				// 2nd poll: paper refilled, resumed processing
				return []byte(`
job-id (integer) = 77
job-state (enum) = processing
job-state-reasons (keyword) = none
`), nil
			}
			// 3rd poll: finished successfully
			return []byte(`
job-id (integer) = 77
job-state (enum) = completed
job-state-reasons (keyword) = job-completed-successfully
`), nil
		default:
			return []byte(""), nil
		}
	}

	cmd := contracts.PrintJobCommand{
		JobID:       "job-recover",
		Copies:      1,
		TotalSheets: 3,
		Duplex:      contracts.DuplexSimplex,
	}

	var completedSheets int
	onPage := func(sheets int) {
		completedSheets = sheets
	}

	err := PrintPDFWithCUPSCustom(ctx, runner, "DCPT310", cmd, "/tmp/recover.pdf", onPage, 5*time.Millisecond)
	if err != nil {
		t.Fatalf("expected successful recovery and print completion, got %v", err)
	}

	if completedSheets != 3 {
		t.Fatalf("expected 3 completed sheets, got %d", completedSheets)
	}
}

func TestPrintPDFWithCUPS_DisappearedJob_Unknown(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		switch name {
		case "lp":
			return []byte("request id is DCPT310-999 (1 file(s))\n"), nil
		case "lpstat":
			// Disappears from queue without appearing in completed
			return []byte(""), nil
		case "ipptool":
			// IPP returns not found
			return []byte("status-code = client-error-not-found (Job #999 does not exist.)"), errors.New("exit status 1")
		default:
			return []byte(""), nil
		}
	}

	cmd := contracts.PrintJobCommand{
		JobID:       "job-ghost",
		Copies:      1,
		TotalSheets: 2,
		Duplex:      contracts.DuplexSimplex,
	}

	var completedCalled bool
	onPage := func(sheets int) {
		completedCalled = true
	}

	err := PrintPDFWithCUPSCustom(ctx, runner, "DCPT310", cmd, "/tmp/ghost.pdf", onPage, 5*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "state unknown") {
		t.Fatalf("expected state unknown error, got %v", err)
	}

	if completedCalled {
		t.Fatal("onPage must NEVER be called when job disappears without confirmation")
	}
}

func TestPrintPDFWithCUPS_NoSyntheticHalfwayProgress(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var mu sync.Mutex
	pollCount := 0
	var sheetsReported []int

	runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		mu.Lock()
		defer mu.Unlock()

		switch name {
		case "lp":
			return []byte("request id is DCPT310-333 (1 file(s))\n"), nil
		case "ipptool":
			pollCount++
			if pollCount <= 3 {
				// While processing without hardware sheet counter, job-media-sheets-completed is 0
				return []byte(`
job-id (integer) = 333
job-state (enum) = processing
job-state-reasons (keyword) = none
job-media-sheets-completed (integer) = 0
`), nil
			}
			// Finally completes
			return []byte(`
job-id (integer) = 333
job-state (enum) = completed
job-state-reasons (keyword) = job-completed-successfully
`), nil
		default:
			return []byte(""), nil
		}
	}

	cmd := contracts.PrintJobCommand{
		JobID:       "job-nosynthetic",
		Copies:      1,
		TotalSheets: 10,
		Duplex:      contracts.DuplexSimplex,
	}

	onPage := func(sheets int) {
		mu.Lock()
		sheetsReported = append(sheetsReported, sheets)
		mu.Unlock()
	}

	err := PrintPDFWithCUPSCustom(ctx, runner, "DCPT310", cmd, "/tmp/test.pdf", onPage, 5*time.Millisecond)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	// Ensure onPage was NOT called with halfway progress (5)
	for _, s := range sheetsReported {
		if s == 5 {
			t.Fatalf("found synthetic halfway progress of 5 sheets in %v", sheetsReported)
		}
	}

	// Should only be called once with 10 on completion
	if len(sheetsReported) != 1 || sheetsReported[0] != 10 {
		t.Fatalf("expected exactly [10] sheets reported on completion, got %v", sheetsReported)
	}
}

func TestPrintPDFWithCUPS_TransientPollErrors_Recovers(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var lpCalls atomic.Int32
	var pollErrors atomic.Int32

	runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		switch name {
		case "lp":
			lpCalls.Add(1)
			return []byte("request id is DCPT310-444 (1 file(s))\n"), nil
		case "lpstat":
			// First 2 polls fail with transient error
			if pollErrors.Add(1) <= 2 {
				return nil, errors.New("lpstat: temporary socket connection error")
			}
			if len(args) > 1 && args[1] == "completed" {
				return []byte("DCPT310-444 username 1024 Tue Nov 3 20:30:00 2026\n\tAlerts: job-completed-successfully\n\tqueued for DCPT310\n"), nil
			}
			return []byte(""), nil
		default:
			return nil, errors.New("unknown command: " + name)
		}
	}

	cmd := contracts.PrintJobCommand{
		JobID:       "job-transient",
		Copies:      1,
		TotalSheets: 2,
		Duplex:      contracts.DuplexSimplex,
	}

	err := PrintPDFWithCUPSCustom(ctx, runner, "DCPT310", cmd, "/tmp/transient.pdf", nil, 5*time.Millisecond)
	if err != nil {
		t.Fatalf("expected recovery from transient errors, got %v", err)
	}

	// Crucial: lp must be submitted exactly once, preventing duplicate prints
	if count := lpCalls.Load(); count != 1 {
		t.Fatalf("expected lp to be called exactly 1 time, got %d", count)
	}
}

func TestPrintPDFWithCUPS_CancellationCancelsJob(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	var mu sync.Mutex
	canceledJobID := ""

	runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		mu.Lock()
		defer mu.Unlock()

		switch name {
		case "lp":
			return []byte("request id is DCPT310-99 (1 file(s))"), nil
		case "lpstat":
			// Trigger context cancel on first poll
			cancel()
			return []byte("DCPT310-99 still printing"), nil
		case "cancel":
			if len(args) > 0 {
				canceledJobID = args[0]
			}
			return []byte(""), nil
		default:
			return []byte(""), nil
		}
	}

	cmd := contracts.PrintJobCommand{
		JobID:       "job-cancel",
		Copies:      1,
		TotalSheets: 5,
		Duplex:      contracts.DuplexSimplex,
	}

	err := PrintPDFWithCUPSCustom(ctx, runner, "DCPT310", cmd, "/tmp/test.pdf", nil, 5*time.Millisecond)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if canceledJobID != "DCPT310-99" {
		t.Fatalf("expected cancel DCPT310-99, got %q", canceledJobID)
	}
}

func TestPollCUPSHealth(t *testing.T) {
	tests := []struct {
		name          string
		queue         string
		output        string
		cmdErr        error
		wantOnline    bool
		wantState     string
		wantPaperTray string
	}{
		{
			name:          "empty queue",
			queue:         "",
			wantOnline:    false,
			wantState:     "ERROR",
			wantPaperTray: "UNKNOWN",
		},
		{
			name:          "command failure",
			queue:         "DCPT310",
			cmdErr:        errors.New("lpstat: cups service down"),
			wantOnline:    false,
			wantState:     "OFFLINE",
			wantPaperTray: "UNKNOWN",
		},
		{
			name:          "queue idle",
			queue:         "DCPT310",
			output:        "printer DCPT310 is idle. enabled since Tue Nov  3 10:00:00 2026",
			wantOnline:    true,
			wantState:     "IDLE",
			wantPaperTray: "OK",
		},
		{
			name:          "queue idle out of paper",
			queue:         "DCPT310",
			output:        "printer DCPT310 is idle. out of paper - please refill",
			wantOnline:    true,
			wantState:     "IDLE",
			wantPaperTray: "EMPTY",
		},
		{
			name:          "queue printing",
			queue:         "DCPT310",
			output:        "printer DCPT310 now printing DCPT310-12. enabled since Tue Nov 3",
			wantOnline:    true,
			wantState:     "PRINTING",
			wantPaperTray: "OK",
		},
		{
			name:          "queue disabled",
			queue:         "DCPT310",
			output:        "printer DCPT310 disabled since Tue Nov 3 - filter failed",
			wantOnline:    false,
			wantState:     "ERROR",
			wantPaperTray: "CHECK_PRINTER",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
				return []byte(tc.output), tc.cmdErr
			}

			h := PollCUPSHealthCustom(context.Background(), runner, tc.queue, 150)
			if h.Online != tc.wantOnline {
				t.Errorf("Online: got %v, want %v", h.Online, tc.wantOnline)
			}
			if h.State != tc.wantState {
				t.Errorf("State: got %q, want %q", h.State, tc.wantState)
			}
			if h.PaperTrayStatus != tc.wantPaperTray {
				t.Errorf("PaperTrayStatus: got %q, want %q", h.PaperTrayStatus, tc.wantPaperTray)
			}
			if h.EstimatedSheets != 150 {
				t.Errorf("EstimatedSheets: got %d, want 150", h.EstimatedSheets)
			}
		})
	}
}

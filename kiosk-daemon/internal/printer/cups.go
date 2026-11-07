package printer

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"xeroxgo/pkg/contracts"
)

// CommandRunner executes a command with context and returns combined output or stdout.
type CommandRunner func(ctx context.Context, name string, args ...string) ([]byte, error)

var defaultCommandRunner CommandRunner = func(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

var (
	cupsJobIDRegex = regexp.MustCompile(`(?i)request id is ([a-zA-Z0-9_\-\.]+)`)

	ippJobStateRegex        = regexp.MustCompile(`(?m)^\s*job-state\s*\([^\)]+\)\s*=\s*([a-zA-Z0-9_\-]+)`)
	ippJobStateReasonsRegex = regexp.MustCompile(`(?m)^\s*job-state-reasons\s*\([^\)]+\)\s*=\s*([^\r\n]+)`)
	ippSheetsCompletedRegex = regexp.MustCompile(`(?m)^\s*job-media-sheets-completed\s*\([^\)]+\)\s*=\s*([0-9]+)`)
	ippStatusCodeRegex      = regexp.MustCompile(`(?m)^\s*status-code\s*=\s*([a-zA-Z0-9_\-]+)`)
)

// CUPSJobState represents the authoritative normalized state of a CUPS/IPP print job.
type CUPSJobState string

const (
	CUPSJobStatePending           CUPSJobState = "pending"
	CUPSJobStateHeld              CUPSJobState = "pending-held"
	CUPSJobStateProcessing        CUPSJobState = "processing"
	CUPSJobStateProcessingStopped CUPSJobState = "processing-stopped"
	CUPSJobStateCompleted         CUPSJobState = "completed"
	CUPSJobStateCanceled          CUPSJobState = "canceled"
	CUPSJobStateAborted           CUPSJobState = "aborted"
	CUPSJobStateUnknown           CUPSJobState = "unknown"
)

// CUPSJobInfo holds status details retrieved via IPP Get-Job-Attributes or lpstat.
type CUPSJobInfo struct {
	State           CUPSJobState
	Reasons         []string
	SheetsCompleted int
	RawStatus       string
}

// QueryCUPSJobInfo queries the authoritative state of a CUPS job.
// It prefers IPP Get-Job-Attributes via ipptool, falling back to lpstat when ipptool is unavailable.
func QueryCUPSJobInfo(ctx context.Context, runner CommandRunner, queue string, cupsJobID string) (CUPSJobInfo, error) {
	if runner == nil {
		runner = defaultCommandRunner
	}
	if ctx == nil {
		return CUPSJobInfo{State: CUPSJobStateUnknown}, errors.New("context cannot be nil")
	}

	// 1. Preferred: Query via IPP Get-Job-Attributes (ipptool) if numeric job ID is extractable
	jobNum := extractJobNumber(cupsJobID)
	if jobNum != "" {
		ippURI := fmt.Sprintf("ipp://localhost:631/jobs/%s", jobNum)
		out, err := runner(ctx, "ipptool", "-tv", ippURI, "get-job-attributes.test")
		if err == nil {
			info, ok := parseIPPJobInfo(out)
			if ok {
				return info, nil
			}
		} else if isIPPNotFoundError(out, err) {
			// Job explicitly not found or already purged in CUPS
			return CUPSJobInfo{
				State:     CUPSJobStateUnknown,
				RawStatus: strings.TrimSpace(string(out)),
			}, nil
		}
	}

	// 2. Fallback: Query via lpstat
	return queryLPStatJobInfo(ctx, runner, queue, cupsJobID)
}

func extractJobNumber(cupsJobID string) string {
	parts := strings.Split(cupsJobID, "-")
	last := parts[len(parts)-1]
	if _, err := strconv.Atoi(last); err == nil {
		return last
	}
	return ""
}

func parseIPPJobInfo(out []byte) (CUPSJobInfo, bool) {
	m := ippJobStateRegex.FindSubmatch(out)
	if len(m) < 2 {
		return CUPSJobInfo{}, false
	}

	rawState := strings.ToLower(strings.TrimSpace(string(m[1])))
	var state CUPSJobState
	switch rawState {
	case "pending":
		state = CUPSJobStatePending
	case "pending-held", "held":
		state = CUPSJobStateHeld
	case "processing":
		state = CUPSJobStateProcessing
	case "processing-stopped":
		state = CUPSJobStateProcessingStopped
	case "completed":
		state = CUPSJobStateCompleted
	case "canceled", "cancelled":
		state = CUPSJobStateCanceled
	case "aborted":
		state = CUPSJobStateAborted
	default:
		state = CUPSJobStateUnknown
	}

	var reasons []string
	if rm := ippJobStateReasonsRegex.FindSubmatch(out); len(rm) > 1 {
		for _, r := range strings.Split(string(rm[1]), ",") {
			r = strings.TrimSpace(r)
			if r != "" && r != "none" {
				reasons = append(reasons, r)
			}
		}
	}

	sheets := 0
	if sm := ippSheetsCompletedRegex.FindSubmatch(out); len(sm) > 1 {
		sheets, _ = strconv.Atoi(string(sm[1]))
	}

	return CUPSJobInfo{
		State:           state,
		Reasons:         reasons,
		SheetsCompleted: sheets,
		RawStatus:       string(out),
	}, true
}

func isIPPNotFoundError(out []byte, err error) bool {
	if len(out) > 0 {
		lower := strings.ToLower(string(out))
		if strings.Contains(lower, "not-found") || strings.Contains(lower, "does not exist") {
			return true
		}
	}
	return false
}

func queryLPStatJobInfo(ctx context.Context, runner CommandRunner, queue string, cupsJobID string) (CUPSJobInfo, error) {
	// A. Check active (not-completed) queue
	activeOut, activeErr := runner(ctx, "lpstat", "-W", "not-completed", "-o", queue)
	if activeErr == nil && strings.Contains(string(activeOut), cupsJobID) {
		// Job is active in CUPS queue. Check if printer queue is stopped or actively printing.
		pOut, pErr := runner(ctx, "lpstat", "-p", queue)
		if pErr == nil {
			pRaw := strings.ToLower(string(pOut))
			if strings.Contains(pRaw, "disabled") || strings.Contains(pRaw, "stopped") {
				if strings.Contains(pRaw, "filter failed") || strings.Contains(pRaw, "filter-failed") {
					return CUPSJobInfo{
						State:     CUPSJobStateAborted,
						Reasons:   []string{strings.TrimSpace(string(pOut))},
						RawStatus: strings.TrimSpace(string(pOut)),
					}, nil
				}
				reason := "printer-stopped"
				if strings.Contains(pRaw, "out of paper") || strings.Contains(pRaw, "media-empty") {
					reason = "media-empty"
				} else if strings.Contains(pRaw, "jam") {
					reason = "media-jam"
				} else if strings.Contains(pRaw, "cover") || strings.Contains(pRaw, "door") {
					reason = "cover-open"
				} else if strings.Contains(pRaw, "paused") {
					reason = "printer-paused"
				}
				return CUPSJobInfo{
					State:     CUPSJobStateProcessingStopped,
					Reasons:   []string{reason},
					RawStatus: strings.TrimSpace(string(pOut)),
				}, nil
			}
			if strings.Contains(pRaw, "now printing") {
				return CUPSJobInfo{
					State:     CUPSJobStateProcessing,
					RawStatus: strings.TrimSpace(string(pOut)),
				}, nil
			}
		}
		// If queue is idle or job waiting its turn
		return CUPSJobInfo{
			State:     CUPSJobStatePending,
			RawStatus: "queued in not-completed",
		}, nil
	}

	// B. Job is not in not-completed. Check completed jobs list.
	compOut, compErr := runner(ctx, "lpstat", "-W", "completed", "-l", "-o", queue)
	if compErr != nil {
		compOut, compErr = runner(ctx, "lpstat", "-W", "completed", "-o", queue)
	}

	if compErr == nil && strings.Contains(string(compOut), cupsJobID) {
		jobBlock := extractJobBlock(string(compOut), cupsJobID)
		lower := strings.ToLower(jobBlock)
		if strings.Contains(lower, "job-completed-successfully") {
			return CUPSJobInfo{
				State:     CUPSJobStateCompleted,
				Reasons:   []string{"job-completed-successfully"},
				RawStatus: strings.TrimSpace(jobBlock),
			}, nil
		}
		if strings.Contains(lower, "job-canceled") || strings.Contains(lower, "canceled") || strings.Contains(lower, "cancelled") {
			return CUPSJobInfo{
				State:     CUPSJobStateCanceled,
				Reasons:   []string{"job-canceled"},
				RawStatus: strings.TrimSpace(jobBlock),
			}, nil
		}
		if strings.Contains(lower, "aborted") || strings.Contains(lower, "filter failed") || strings.Contains(lower, "filter-failed") {
			return CUPSJobInfo{
				State:     CUPSJobStateAborted,
				Reasons:   []string{"job-aborted"},
				RawStatus: strings.TrimSpace(jobBlock),
			}, nil
		}

		// Also check lpstat -W successful for explicit scheduler confirmation
		succOut, succErr := runner(ctx, "lpstat", "-W", "successful", "-o", queue)
		if succErr == nil && strings.Contains(string(succOut), cupsJobID) {
			return CUPSJobInfo{
				State:     CUPSJobStateCompleted,
				Reasons:   []string{"job-completed-successfully"},
				RawStatus: strings.TrimSpace(jobBlock),
			}, nil
		}

		// In completed list without completion alert: job did not finish successfully
		return CUPSJobInfo{
			State:     CUPSJobStateCanceled,
			Reasons:   []string{"job ended without completion confirmation"},
			RawStatus: strings.TrimSpace(jobBlock),
		}, nil
	}

	// Check if job appears in lpstat -W successful directly
	succOut, succErr := runner(ctx, "lpstat", "-W", "successful", "-o", queue)
	if succErr == nil && strings.Contains(string(succOut), cupsJobID) {
		return CUPSJobInfo{
			State:     CUPSJobStateCompleted,
			Reasons:   []string{"job-completed-successfully"},
			RawStatus: strings.TrimSpace(string(succOut)),
		}, nil
	}

	// C. If both active and completed queries succeeded (or returned clean output),
	// but the job is absent from both lists, state is UNKNOWN.
	if activeErr == nil && compErr == nil {
		return CUPSJobInfo{
			State:     CUPSJobStateUnknown,
			RawStatus: "job not listed in not-completed or completed",
		}, nil
	}

	// If commands had errors (e.g. CUPS communication failure)
	if activeErr != nil {
		return CUPSJobInfo{State: CUPSJobStateUnknown}, activeErr
	}
	return CUPSJobInfo{State: CUPSJobStateUnknown}, compErr
}

func extractJobBlock(output, jobID string) string {
	lines := strings.Split(output, "\n")
	var block []string
	found := false
	for _, line := range lines {
		if strings.Contains(line, jobID) {
			found = true
			block = append(block, line)
			continue
		}
		if found {
			// In lpstat -l, indented lines belong to the current job
			if strings.HasPrefix(line, "\t") || strings.HasPrefix(line, "    ") {
				block = append(block, line)
			} else if strings.TrimSpace(line) != "" {
				// Next job started
				break
			}
		}
	}
	return strings.Join(block, "\n")
}

// PrintPDFWithCUPS submits a PDF file to the specified CUPS queue via the lp command
// and monitors its progress until completion.
func PrintPDFWithCUPS(
	ctx context.Context,
	queue string,
	cmd contracts.PrintJobCommand,
	pdfPath string,
	onPage func(sheetsCompleted int),
) error {
	return PrintPDFWithCUPSCustom(ctx, defaultCommandRunner, queue, cmd, pdfPath, onPage, 500*time.Millisecond)
}

// PrintPDFWithCUPSCustom allows injection of a custom command runner and polling interval for testing.
func PrintPDFWithCUPSCustom(
	ctx context.Context,
	runner CommandRunner,
	queue string,
	cmd contracts.PrintJobCommand,
	pdfPath string,
	onPage func(sheetsCompleted int),
	pollInterval time.Duration,
) error {
	if ctx == nil {
		return errors.New("context cannot be nil")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if strings.TrimSpace(queue) == "" {
		return errors.New("queue name cannot be empty")
	}
	if strings.TrimSpace(pdfPath) == "" {
		return errors.New("pdf path cannot be empty")
	}
	if cmd.JobID == "" {
		return errors.New("job_id cannot be empty")
	}
	if cmd.Copies < 1 || cmd.Copies > 999 {
		return fmt.Errorf("copies must be between 1 and 999, got %d", cmd.Copies)
	}
	if cmd.TotalSheets < 1 {
		return fmt.Errorf("total_sheets must be >= 1, got %d", cmd.TotalSheets)
	}
	if runner == nil {
		runner = defaultCommandRunner
	}

	// 1. Build lp command arguments
	lpArgs := []string{
		"-d", queue,
		"-n", strconv.Itoa(cmd.Copies),
		"-t", cmd.JobID,
	}

	switch cmd.Duplex {
	case contracts.DuplexLongEdge:
		lpArgs = append(lpArgs, "-o", "sides=two-sided-long-edge")
	case contracts.DuplexShortEdge:
		lpArgs = append(lpArgs, "-o", "sides=two-sided-short-edge")
	}

	lpArgs = append(lpArgs, pdfPath)

	log.Printf("[CUPS] Submitting print job %s to queue %q: lp %s", cmd.JobID, queue, strings.Join(lpArgs, " "))

	// 2. Submit print job via lp
	out, err := runner(ctx, "lp", lpArgs...)
	if err != nil {
		return fmt.Errorf("failed to submit job to CUPS queue %q: %w (output: %s)", queue, err, strings.TrimSpace(string(out)))
	}

	outputStr := string(out)
	match := cupsJobIDRegex.FindStringSubmatch(outputStr)
	if len(match) < 2 {
		return fmt.Errorf("unable to parse CUPS request id from lp output: %q", strings.TrimSpace(outputStr))
	}
	cupsJobID := match[1]
	log.Printf("[CUPS] Job %s assigned CUPS request ID: %s", cmd.JobID, cupsJobID)

	// 3. Monitor CUPS job until completed, failed, or context canceled
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	consecutiveUnknown := 0
	const maxUnknownRetries = 3

	consecutivePollErrors := 0
	const maxPollErrors = 10

	lastSheetsReported := 0
	var lastState CUPSJobState

	for {
		select {
		case <-ctx.Done():
			// Attempt to cancel the job in CUPS on timeout or context cancel
			cancelCtx, cancelTimeout := context.WithTimeout(context.Background(), 2*time.Second)
			_, _ = runner(cancelCtx, "cancel", cupsJobID)
			cancelTimeout()
			return ctx.Err()

		case <-ticker.C:
			info, err := QueryCUPSJobInfo(ctx, runner, queue, cupsJobID)
			if err != nil {
				consecutivePollErrors++
				log.Printf("[CUPS] Warning: transient error querying job %s state (%d/%d): %v", cupsJobID, consecutivePollErrors, maxPollErrors, err)
				if consecutivePollErrors >= maxPollErrors {
					return fmt.Errorf("CUPS polling failed repeatedly for queue %q job %s: %w", queue, cupsJobID, err)
				}
				continue
			}
			consecutivePollErrors = 0

			if info.State != lastState {
				log.Printf("[CUPS] Job %s (%s) state: %s (reasons: %v)", cmd.JobID, cupsJobID, info.State, info.Reasons)
				lastState = info.State
			}

			switch info.State {
			case CUPSJobStatePending, CUPSJobStateHeld:
				consecutiveUnknown = 0
				// Queued in CUPS; do not report synthetic progress.

			case CUPSJobStateProcessing:
				consecutiveUnknown = 0
				// Report real progress only if authoritative sheet count is available from IPP
				if info.SheetsCompleted > lastSheetsReported && info.SheetsCompleted <= cmd.TotalSheets {
					lastSheetsReported = info.SheetsCompleted
					if onPage != nil {
						onPage(info.SheetsCompleted)
					}
				}

			case CUPSJobStateProcessingStopped:
				consecutiveUnknown = 0
				// Recoverable printer pause (e.g. out of paper, paper jam, door open)
				log.Printf("[CUPS] Job %s printer paused/stopped (recoverable): %s. Waiting for printer recovery...", cupsJobID, strings.Join(info.Reasons, ", "))

			case CUPSJobStateCompleted:
				log.Printf("[CUPS] Job %s (CUPS ID %s) confirmed completed by CUPS", cmd.JobID, cupsJobID)
				if onPage != nil {
					onPage(cmd.TotalSheets)
				}
				return nil

			case CUPSJobStateCanceled:
				reason := strings.Join(info.Reasons, ", ")
				if reason == "" {
					reason = "job canceled in CUPS"
				}
				log.Printf("[CUPS] Job %s (CUPS ID %s) was canceled in CUPS: %s", cmd.JobID, cupsJobID, reason)
				return fmt.Errorf("CUPS job %s canceled: %s", cupsJobID, reason)

			case CUPSJobStateAborted:
				reason := strings.Join(info.Reasons, ", ")
				if reason == "" {
					reason = "filter failed or printer error"
				}
				log.Printf("[CUPS] Job %s (CUPS ID %s) aborted in CUPS: %s", cmd.JobID, cupsJobID, reason)
				return fmt.Errorf("CUPS printer %q is stopped/disabled: %s", queue, reason)

			case CUPSJobStateUnknown:
				consecutiveUnknown++
				log.Printf("[CUPS] Warning: Job %s state unknown (%d/%d)", cupsJobID, consecutiveUnknown, maxUnknownRetries)
				if consecutiveUnknown >= maxUnknownRetries {
					return fmt.Errorf("job %s state unknown: disappeared from CUPS without completion confirmation", cupsJobID)
				}
			}
		}
	}
}

// PollCUPSHealth checks the operational status of a CUPS queue using lpstat.
func PollCUPSHealth(ctx context.Context, queue string, estimatedSheets int) contracts.PrinterHealth {
	return PollCUPSHealthCustom(ctx, defaultCommandRunner, queue, estimatedSheets)
}

// PollCUPSHealthCustom allows injection of a custom command runner for testing CUPS health checks.
func PollCUPSHealthCustom(
	ctx context.Context,
	runner CommandRunner,
	queue string,
	estimatedSheets int,
) contracts.PrinterHealth {
	now := time.Now().UTC()
	if strings.TrimSpace(queue) == "" {
		return contracts.PrinterHealth{
			Online:          false,
			State:           "ERROR",
			PaperTrayStatus: "UNKNOWN",
			EstimatedSheets: estimatedSheets,
			UpdatedAt:       now,
		}
	}

	pollCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	out, err := runner(pollCtx, "lpstat", "-p", queue)
	if err != nil {
		return contracts.PrinterHealth{
			Online:          false,
			State:           "OFFLINE",
			PaperTrayStatus: "UNKNOWN",
			EstimatedSheets: estimatedSheets,
			UpdatedAt:       now,
		}
	}

	raw := strings.ToLower(string(out))

	if strings.Contains(raw, "disabled") {
		return contracts.PrinterHealth{
			Online:          false,
			State:           "ERROR",
			PaperTrayStatus: "CHECK_PRINTER",
			EstimatedSheets: estimatedSheets,
			UpdatedAt:       now,
		}
	}

	if strings.Contains(raw, "now printing") {
		return contracts.PrinterHealth{
			Online:          true,
			State:           "PRINTING",
			PaperTrayStatus: "OK",
			EstimatedSheets: estimatedSheets,
			UpdatedAt:       now,
		}
	}

	if strings.Contains(raw, "is idle") {
		paperStatus := "OK"
		if strings.Contains(raw, "out of paper") || strings.Contains(raw, "media-empty") {
			paperStatus = "EMPTY"
		}
		return contracts.PrinterHealth{
			Online:          true,
			State:           "IDLE",
			PaperTrayStatus: paperStatus,
			EstimatedSheets: estimatedSheets,
			UpdatedAt:       now,
		}
	}

	// Default fallback for recognized queue
	return contracts.PrinterHealth{
		Online:          true,
		State:           "READY",
		PaperTrayStatus: "OK",
		EstimatedSheets: estimatedSheets,
		UpdatedAt:       now,
	}
}

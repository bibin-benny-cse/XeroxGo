package pdf

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"xeroxgo/pkg/contracts"
)

type NormalizedResult struct {
	TotalPages     int
	TotalSheets    int
	PaddedPages    int
	SHA256Checksum string
}

// NormalizeOptions specifies the parameters for normalizing a PDF print job.
type NormalizeOptions struct {
	Copies    int
	Duplex    contracts.DuplexMode
	ColorMode string
	PageRange string
}

// ParsePageRange parses and validates custom page ranges like "1-5", "1,3,5", "1-3,7"
// against the maximum available pages in the document.
// It returns the 1-based page indices in the order specified, deduplicated.
func ParsePageRange(rangeStr string, maxPages int) ([]int, error) {
	rangeStr = strings.TrimSpace(rangeStr)
	if maxPages < 1 {
		return nil, fmt.Errorf("invalid document page count: %d", maxPages)
	}
	if rangeStr == "" || strings.EqualFold(rangeStr, "all") {
		pages := make([]int, maxPages)
		for i := 0; i < maxPages; i++ {
			pages[i] = i + 1
		}
		return pages, nil
	}

	parts := strings.Split(rangeStr, ",")
	var result []int
	seen := make(map[int]bool)

	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			return nil, errors.New("empty segment in page range")
		}

		if strings.Contains(part, "-") {
			segs := strings.Split(part, "-")
			if len(segs) != 2 {
				return nil, fmt.Errorf("invalid range syntax %q", part)
			}
			startStr := strings.TrimSpace(segs[0])
			endStr := strings.TrimSpace(segs[1])
			if startStr == "" || endStr == "" {
				return nil, fmt.Errorf("invalid range syntax %q", part)
			}

			start, err := strconv.Atoi(startStr)
			if err != nil {
				return nil, fmt.Errorf("invalid range start %q: %w", startStr, err)
			}
			end, err := strconv.Atoi(endStr)
			if err != nil {
				return nil, fmt.Errorf("invalid range end %q: %w", endStr, err)
			}

			if start < 1 {
				return nil, fmt.Errorf("page number %d is invalid (must be >= 1)", start)
			}
			if start > end {
				return nil, fmt.Errorf("invalid page range %d-%d: start exceeds end", start, end)
			}
			if end > maxPages {
				return nil, fmt.Errorf("page %d in range exceeds document page count of %d", end, maxPages)
			}

			for p := start; p <= end; p++ {
				if !seen[p] {
					seen[p] = true
					result = append(result, p)
				}
			}
		} else {
			p, err := strconv.Atoi(part)
			if err != nil {
				return nil, fmt.Errorf("invalid page number %q: %w", part, err)
			}
			if p < 1 {
				return nil, fmt.Errorf("page number %d is invalid (must be >= 1)", p)
			}
			if p > maxPages {
				return nil, fmt.Errorf("page number %d exceeds document page count of %d", p, maxPages)
			}
			if !seen[p] {
				seen[p] = true
				result = append(result, p)
			}
		}
	}

	if len(result) == 0 {
		return nil, errors.New("no pages selected")
	}

	return result, nil
}

// ExtractPageRange extracts a subset of pages into outputPath using qpdf,
// preserving the exact requested page order.
func ExtractPageRange(ctx context.Context, inputPath, outputPath string, pages []int) error {
	if ctx == nil {
		return errors.New("context must not be nil")
	}
	if len(pages) == 0 {
		return errors.New("pages slice must not be empty")
	}

	pageStrs := make([]string, len(pages))
	for i, p := range pages {
		pageStrs[i] = strconv.Itoa(p)
	}
	rangeArg := strings.Join(pageStrs, ",")

	cmd := exec.CommandContext(ctx, "qpdf", inputPath, "--pages", inputPath, rangeArg, "--", outputPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("qpdf page extraction failed: %s: %w", strings.TrimSpace(string(out)), err)
	}

	return nil
}

func NormalizeAndCount(
	ctx context.Context,
	inputPath, outputPath string,
	copies int,
	duplex contracts.DuplexMode,
) (*NormalizedResult, error) {
	return NormalizeAndCountWithOptions(ctx, inputPath, outputPath, NormalizeOptions{
		Copies:    copies,
		Duplex:    duplex,
		ColorMode: "BW",
	})
}

// NormalizeAndCountMode normalizes an input PDF and counts its pages and physical sheets,
// supporting both monochrome ("BW") and full-color ("COLOR") color spaces.
func NormalizeAndCountMode(
	ctx context.Context,
	inputPath, outputPath string,
	copies int,
	duplex contracts.DuplexMode,
	colorMode string,
) (*NormalizedResult, error) {
	return NormalizeAndCountWithOptions(ctx, inputPath, outputPath, NormalizeOptions{
		Copies:    copies,
		Duplex:    duplex,
		ColorMode: colorMode,
	})
}

// NormalizeAndCountWithOptions normalizes an input PDF according to options,
// supporting custom page ranges, duplex blank padding, and physical sheet calculations.
func NormalizeAndCountWithOptions(
	ctx context.Context,
	inputPath, outputPath string,
	opts NormalizeOptions,
) (*NormalizedResult, error) {
	// Step 1: Validate arguments and filesystem paths
	if ctx == nil {
		return nil, errors.New("context must not be nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if inputPath == "" {
		return nil, errors.New("inputPath must not be empty")
	}
	if outputPath == "" {
		return nil, errors.New("outputPath must not be empty")
	}
	if opts.Copies < 1 {
		return nil, fmt.Errorf("copies must be at least 1, got %d", opts.Copies)
	}

	switch opts.Duplex {
	case contracts.DuplexSimplex, contracts.DuplexLongEdge, contracts.DuplexShortEdge:
	default:
		return nil, fmt.Errorf("invalid duplex mode: %q", opts.Duplex)
	}

	inputInfo, err := os.Stat(inputPath)
	if err != nil {
		return nil, fmt.Errorf("input file stat failed: %w", err)
	}
	if !inputInfo.Mode().IsRegular() {
		return nil, fmt.Errorf("input path is not a regular file: %s", inputPath)
	}

	absInput, err := filepath.Abs(inputPath)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve absolute input path: %w", err)
	}
	absOutput, err := filepath.Abs(outputPath)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve absolute output path: %w", err)
	}

	if filepath.Clean(absInput) == filepath.Clean(absOutput) {
		return nil, errors.New("input and output paths identify the same file")
	}

	if outputInfo, err := os.Stat(outputPath); err == nil {
		if os.SameFile(inputInfo, outputInfo) {
			return nil, errors.New("input and output paths identify the same file")
		}
	}

	// Step 2: Validate PDF structure using qpdf
	cmdCheck := exec.CommandContext(ctx, "qpdf", "--check", inputPath)
	outCheck, errCheck := cmdCheck.CombinedOutput()
	if errCheck != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		var exitErr *exec.ExitError
		if errors.As(errCheck, &exitErr) {
			if exitErr.ExitCode() != 3 {
				return nil, fmt.Errorf("qpdf --check failed (exit code %d): %s: %w", exitErr.ExitCode(), strings.TrimSpace(string(outCheck)), errCheck)
			}
			// Exit status 3 is warning-only; accepted per specification
		} else {
			return nil, fmt.Errorf("failed to execute qpdf --check: %s: %w", strings.TrimSpace(string(outCheck)), errCheck)
		}
	}

	cmdEnc := exec.CommandContext(ctx, "qpdf", "--is-encrypted", inputPath)
	outEnc, errEnc := cmdEnc.CombinedOutput()
	if errEnc == nil {
		// Exit code 0 means the PDF is encrypted
		return nil, fmt.Errorf("pdf is encrypted: %s", inputPath)
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	var exitErrEnc *exec.ExitError
	if errors.As(errEnc, &exitErrEnc) {
		if exitErrEnc.ExitCode() != 2 {
			// Exit code 2 means not encrypted; any other code is an error
			return nil, fmt.Errorf("qpdf --is-encrypted failed (exit code %d): %s: %w", exitErrEnc.ExitCode(), strings.TrimSpace(string(outEnc)), errEnc)
		}
	} else {
		return nil, fmt.Errorf("failed to execute qpdf --is-encrypted: %s: %w", strings.TrimSpace(string(outEnc)), errEnc)
	}

	// Step 3: Count raw pages of input PDF
	cmdRawPages := exec.CommandContext(ctx, "qpdf", "--show-npages", inputPath)
	outRawPages, errRawPages := cmdRawPages.CombinedOutput()
	if errRawPages != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("failed to count pages of input PDF: %s: %w", strings.TrimSpace(string(outRawPages)), errRawPages)
	}
	rawTotalPages, err := strconv.Atoi(strings.TrimSpace(string(outRawPages)))
	if err != nil || rawTotalPages <= 0 {
		return nil, fmt.Errorf("invalid raw page count %q: %w", strings.TrimSpace(string(outRawPages)), err)
	}

	// Step 4: Handle page range selection
	workingInputPath := inputPath
	contentPages := rawTotalPages

	if trimmedRange := strings.TrimSpace(opts.PageRange); trimmedRange != "" && !strings.EqualFold(trimmedRange, "all") {
		selectedPages, err := ParsePageRange(trimmedRange, rawTotalPages)
		if err != nil {
			return nil, fmt.Errorf("invalid page range %q: %w", trimmedRange, err)
		}

		// Check if extraction is needed (subset of pages or reordered)
		needsExtraction := len(selectedPages) != rawTotalPages
		if !needsExtraction {
			for i, p := range selectedPages {
				if p != i+1 {
					needsExtraction = true
					break
				}
			}
		}

		if needsExtraction {
			tmpExtracted, err := os.CreateTemp("", "xeroxgo_range_*.pdf")
			if err != nil {
				return nil, fmt.Errorf("failed to create temp file for page extraction: %w", err)
			}
			tmpExtractedName := tmpExtracted.Name()
			_ = tmpExtracted.Close()
			defer os.Remove(tmpExtractedName)

			if err := ExtractPageRange(ctx, inputPath, tmpExtractedName, selectedPages); err != nil {
				return nil, fmt.Errorf("failed to extract selected pages: %w", err)
			}
			workingInputPath = tmpExtractedName
		}
		contentPages = len(selectedPages)
	}

	// Step 5: Determine duplex blank padding requirement
	// In duplex mode, odd-page documents must end on an even boundary so that
	// subsequent copies begin on a fresh physical sheet and the last sheet has a blank back.
	needsDuplexPadding := (opts.Duplex != contracts.DuplexSimplex) && (contentPages%2 != 0)

	// Step 6: Normalize the PDF with Ghostscript
	colorStrategy := "-sColorConversionStrategy=Gray"
	processModel := "-dProcessColorModel=/DeviceGray"
	if strings.ToUpper(strings.TrimSpace(opts.ColorMode)) == "COLOR" {
		colorStrategy = "-sColorConversionStrategy=LeaveColorUnchanged"
		processModel = "-dProcessColorModel=/DeviceRGB"
	}

	gsArgs := []string{
		"-sDEVICE=pdfwrite",
		"-dCompatibilityLevel=1.7",
		"-dPDFSETTINGS=/prepress",
		colorStrategy,
		processModel,
		"-sPAPERSIZE=a4",
		"-dFIXEDMEDIA",
		"-dPDFFitPage",
		"-dNOPAUSE",
		"-dQUIET",
		"-dBATCH",
		"-sOutputFile=" + outputPath,
		workingInputPath,
	}
	if needsDuplexPadding {
		gsArgs = append(gsArgs, "-c", "showpage")
	}

	cmdGS := exec.CommandContext(ctx, "gs", gsArgs...)
	outGS, errGS := cmdGS.CombinedOutput()
	if errGS != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("ghostscript normalization failed: %s: %w", strings.TrimSpace(string(outGS)), errGS)
	}

	// Step 7: Verify output page count
	cmdPages := exec.CommandContext(ctx, "qpdf", "--show-npages", outputPath)
	var stdoutPages, stderrPages strings.Builder
	cmdPages.Stdout = &stdoutPages
	cmdPages.Stderr = &stderrPages
	if err := cmdPages.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("qpdf --show-npages failed: %s: %w", strings.TrimSpace(stderrPages.String()), err)
	}

	actualOutputPages, err := strconv.Atoi(strings.TrimSpace(stdoutPages.String()))
	if err != nil {
		return nil, fmt.Errorf("failed to parse normalized page count %q: %w", stdoutPages.String(), err)
	}

	expectedOutputPages := contentPages
	if needsDuplexPadding {
		expectedOutputPages++
	}
	if actualOutputPages != expectedOutputPages {
		return nil, fmt.Errorf("normalized page count mismatch: expected %d, got %d", expectedOutputPages, actualOutputPages)
	}

	// Step 8: Calculate physical sheets
	var sheetsPerCopy int
	switch opts.Duplex {
	case contracts.DuplexSimplex:
		sheetsPerCopy = contentPages
	case contracts.DuplexLongEdge, contracts.DuplexShortEdge:
		sheetsPerCopy = contentPages/2 + contentPages%2
	}

	if opts.Copies > 0 && sheetsPerCopy > math.MaxInt/opts.Copies {
		return nil, errors.New("arithmetic overflow calculating total sheets")
	}
	totalSheets := sheetsPerCopy * opts.Copies

	// Step 9: Compute the SHA-256 checksum of the normalized output file
	f, err := os.Open(outputPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open normalized output file for checksum: %w", err)
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return nil, fmt.Errorf("failed to compute checksum of normalized output file: %w", err)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("failed to close normalized output file: %w", err)
	}

	checksum := hex.EncodeToString(h.Sum(nil))

	// Step 10: Return the authoritative result
	return &NormalizedResult{
		TotalPages:     contentPages,
		TotalSheets:    totalSheets,
		PaddedPages:    actualOutputPages,
		SHA256Checksum: checksum,
	}, nil
}

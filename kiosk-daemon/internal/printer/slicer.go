package printer

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// SlicePDFForResume creates a new PDF containing pages startPage through
// the last page of the original PDF, inclusive.
//
// The slicer is used after an interrupted print job to prepare only the
// remaining pages for printing.
func SlicePDFForResume(
	ctx context.Context,
	inputPath, outputPath string,
	startPage int,
) error {
	// Context validation
	if ctx == nil {
		return fmt.Errorf("validate input PDF: context is nil")
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("validate input PDF: context canceled: %w", err)
	}

	// Path validation
	if strings.TrimSpace(inputPath) == "" {
		return fmt.Errorf("validate input PDF: empty input path")
	}
	if strings.TrimSpace(outputPath) == "" {
		return fmt.Errorf("validate input PDF: empty output path")
	}

	// Early page number validation
	if startPage < 1 {
		return fmt.Errorf("validate input PDF: startPage must be >= 1, got %d", startPage)
	}

	// Input file validation
	inStat, err := os.Stat(inputPath)
	if err != nil {
		return fmt.Errorf("validate input PDF: stat input file: %w", err)
	}
	if !inStat.Mode().IsRegular() {
		return fmt.Errorf("validate input PDF: input path is not a regular file")
	}

	// Path equivalence validation
	absIn, err := filepath.Abs(inputPath)
	if err != nil {
		return fmt.Errorf("validate input PDF: resolve input path: %w", err)
	}
	absOut, err := filepath.Abs(outputPath)
	if err != nil {
		return fmt.Errorf("validate input PDF: resolve output path: %w", err)
	}
	if filepath.Clean(absIn) == filepath.Clean(absOut) {
		return fmt.Errorf("validate input PDF: input and output paths identify the same file")
	}

	// Symlink equivalence resolution
	if realIn, errIn := filepath.EvalSymlinks(absIn); errIn == nil {
		if realOut, errOut := filepath.EvalSymlinks(absOut); errOut == nil {
			if realIn == realOut {
				return fmt.Errorf("validate input PDF: input and output paths identify the same file")
			}
		} else {
			outDir := filepath.Dir(absOut)
			if realOutDir, errDir := filepath.EvalSymlinks(outDir); errDir == nil {
				realOut := filepath.Join(realOutDir, filepath.Base(absOut))
				if realIn == realOut {
					return fmt.Errorf("validate input PDF: input and output paths identify the same file")
				}
			}
		}
	}

	// Check if output path already exists
	if outStat, err := os.Stat(outputPath); err == nil {
		if outStat.IsDir() {
			return fmt.Errorf("validate input PDF: output path is a directory")
		}
		if os.SameFile(inStat, outStat) {
			return fmt.Errorf("validate input PDF: input and output paths identify the same file")
		}
	}

	// Step 1 — Check the Input PDF
	inCountOut, err := runQpdfCommand(ctx, "--show-npages", inputPath)
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("validate input PDF: %w", ctx.Err())
		}
		return fmt.Errorf("validate input PDF: qpdf --show-npages failed: %w", err)
	}

	totalPages, err := strconv.Atoi(strings.TrimSpace(inCountOut))
	if err != nil {
		return fmt.Errorf("validate input PDF: unparsable page count %q: %w", strings.TrimSpace(inCountOut), err)
	}
	if totalPages <= 0 {
		return fmt.Errorf("validate input PDF: invalid total page count %d", totalPages)
	}
	if startPage > totalPages {
		return fmt.Errorf("validate input PDF: startPage %d exceeds total pages %d", startPage, totalPages)
	}

	// Step 2 — Select the Remaining Pages
	expectedPages := totalPages - startPage + 1
	pageRange := strconv.Itoa(startPage) + "-z"

	_, err = runQpdfCommand(ctx, inputPath, "--pages", ".", pageRange, "--", outputPath)
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("slice remaining pages: %w", ctx.Err())
		}
		return fmt.Errorf("slice remaining pages: qpdf failed: %w", err)
	}

	// Step 3 — Verify the Output
	outStat, err := os.Stat(outputPath)
	if err != nil {
		return fmt.Errorf("verify sliced output: output file does not exist: %w", err)
	}
	if !outStat.Mode().IsRegular() {
		return fmt.Errorf("verify sliced output: output is not a regular file")
	}
	if outStat.Size() == 0 {
		return fmt.Errorf("verify sliced output: output file is empty")
	}

	outCountOut, err := runQpdfCommand(ctx, "--show-npages", outputPath)
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("verify sliced output: %w", ctx.Err())
		}
		return fmt.Errorf("verify sliced output: qpdf --show-npages failed: %w", err)
	}

	outPages, err := strconv.Atoi(strings.TrimSpace(outCountOut))
	if err != nil {
		return fmt.Errorf("verify sliced output: unparsable page count %q: %w", strings.TrimSpace(outCountOut), err)
	}
	if outPages != expectedPages {
		return fmt.Errorf("verify sliced output: expected %d pages, got %d", expectedPages, outPages)
	}

	// Step 4 — Return Success
	return nil
}

func runQpdfCommand(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "qpdf", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		var diags []string
		if outStr := strings.TrimSpace(stdout.String()); outStr != "" {
			diags = append(diags, outStr)
		}
		if errStr := strings.TrimSpace(stderr.String()); errStr != "" {
			diags = append(diags, errStr)
		}
		if len(diags) > 0 {
			return "", fmt.Errorf("%w: %s", err, strings.Join(diags, "; "))
		}
		return "", err
	}

	if err := ctx.Err(); err != nil {
		return "", err
	}

	return stdout.String(), nil
}

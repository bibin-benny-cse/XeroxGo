package printer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// createThreePageFixture writes a valid PostScript file with three distinct pages
// and generates a PDF using Ghostscript under dir.
func createThreePageFixture(t *testing.T, dir string) string {
	t.Helper()

	psPath := filepath.Join(dir, "fixture_3pages.ps")
	pdfPath := filepath.Join(dir, "fixture_3pages.pdf")

	psContent := `%!PS-Adobe-3.0
%%Pages: 3
%%Page: 1 1
/Helvetica findfont 24 scalefont setfont
100 700 moveto
(XeroxGo Slicer Test - Page 1) show
showpage

%%Page: 2 2
/Helvetica findfont 24 scalefont setfont
100 700 moveto
(XeroxGo Slicer Test - Page 2) show
showpage

%%Page: 3 3
/Helvetica findfont 24 scalefont setfont
100 700 moveto
(XeroxGo Slicer Test - Page 3) show
showpage
`
	if err := os.WriteFile(psPath, []byte(psContent), 0600); err != nil {
		t.Fatalf("failed to write PostScript fixture: %v", err)
	}

	cmd := exec.Command("gs",
		"-sDEVICE=pdfwrite",
		"-sPAPERSIZE=a4",
		"-dFIXEDMEDIA",
		"-dNOPAUSE",
		"-dBATCH",
		"-dQUIET",
		"-sOutputFile="+pdfPath,
		psPath,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Ghostscript failed to generate PDF fixture: %s: %v", strings.TrimSpace(string(out)), err)
	}

	return pdfPath
}

func getPageCount(t *testing.T, pdfPath string) int {
	t.Helper()
	cmd := exec.Command("qpdf", "--show-npages", pdfPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("qpdf --show-npages %s failed: %s: %v", pdfPath, strings.TrimSpace(string(out)), err)
	}
	count, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		t.Fatalf("failed to parse page count from %q: %v", string(out), err)
	}
	return count
}

func assertPDFValid(t *testing.T, pdfPath string) {
	t.Helper()
	cmd := exec.Command("qpdf", "--check", pdfPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("qpdf --check %s reported invalid PDF: %s: %v", pdfPath, strings.TrimSpace(string(out)), err)
	}
}

func computeFileSHA256(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read file for checksum %s: %v", path, err)
	}
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}

// Test A — TestSlicePDFForResume
func TestSlicePDFForResume(t *testing.T) {
	tmpDir := t.TempDir()
	inputPath := createThreePageFixture(t, tmpDir)
	outputPath := filepath.Join(tmpDir, "sliced_output.pdf")

	err := SlicePDFForResume(context.Background(), inputPath, outputPath, 2)
	if err != nil {
		t.Fatalf("SlicePDFForResume failed: %v", err)
	}

	// Assert input file still exists
	if inStat, err := os.Stat(inputPath); err != nil || !inStat.Mode().IsRegular() {
		t.Fatalf("input file missing or not regular after slice: %v", err)
	}

	// Assert output file exists and is non-empty
	outStat, err := os.Stat(outputPath)
	if err != nil {
		t.Fatalf("output file stat failed: %v", err)
	}
	if !outStat.Mode().IsRegular() {
		t.Fatalf("output file is not a regular file")
	}
	if outStat.Size() == 0 {
		t.Fatalf("output file is empty")
	}

	// Assert qpdf reports exactly two pages in the output
	pages := getPageCount(t, outputPath)
	if pages != 2 {
		t.Fatalf("expected 2 pages in output, got %d", pages)
	}

	// Assert qpdf accepts the output PDF as valid
	assertPDFValid(t, outputPath)
}

// Test B — TestSlicePDFForResume_StartAtFirstPage
func TestSlicePDFForResume_StartAtFirstPage(t *testing.T) {
	tmpDir := t.TempDir()
	inputPath := createThreePageFixture(t, tmpDir)
	outputPath := filepath.Join(tmpDir, "sliced_all.pdf")

	err := SlicePDFForResume(context.Background(), inputPath, outputPath, 1)
	if err != nil {
		t.Fatalf("SlicePDFForResume startPage=1 failed: %v", err)
	}

	pages := getPageCount(t, outputPath)
	if pages != 3 {
		t.Fatalf("expected 3 pages in output, got %d", pages)
	}
	assertPDFValid(t, outputPath)
}

// Test C — TestSlicePDFForResume_StartAtLastPage
func TestSlicePDFForResume_StartAtLastPage(t *testing.T) {
	tmpDir := t.TempDir()
	inputPath := createThreePageFixture(t, tmpDir)
	outputPath := filepath.Join(tmpDir, "sliced_last.pdf")

	err := SlicePDFForResume(context.Background(), inputPath, outputPath, 3)
	if err != nil {
		t.Fatalf("SlicePDFForResume startPage=3 failed: %v", err)
	}

	pages := getPageCount(t, outputPath)
	if pages != 1 {
		t.Fatalf("expected 1 page in output, got %d", pages)
	}
	assertPDFValid(t, outputPath)
}

// Test D — TestSlicePDFForResume_InvalidStartPage
func TestSlicePDFForResume_InvalidStartPage(t *testing.T) {
	tmpDir := t.TempDir()
	inputPath := createThreePageFixture(t, tmpDir)

	testCases := []struct {
		name      string
		startPage int
	}{
		{name: "zero", startPage: 0},
		{name: "negative", startPage: -1},
		{name: "exceeds total pages", startPage: 4},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			outputPath := filepath.Join(tmpDir, "invalid_page_"+tc.name+".pdf")
			err := SlicePDFForResume(context.Background(), inputPath, outputPath, tc.startPage)
			if err == nil {
				t.Fatalf("expected error for startPage=%d, got nil", tc.startPage)
			}
			// Verify output file was not created
			if _, statErr := os.Stat(outputPath); statErr == nil {
				t.Fatalf("output file should not have been created for invalid startPage=%d", tc.startPage)
			}
		})
	}
}

// Test E — TestSlicePDFForResume_InvalidPaths
func TestSlicePDFForResume_InvalidPaths(t *testing.T) {
	tmpDir := t.TempDir()
	inputPath := createThreePageFixture(t, tmpDir)
	initialChecksum := computeFileSHA256(t, inputPath)

	subDir := filepath.Join(tmpDir, "sub")
	if err := os.Mkdir(subDir, 0755); err != nil {
		t.Fatalf("failed to create sub directory: %v", err)
	}

	// Equivalent path using relative path navigation
	relEquivalentOutput := filepath.Join(subDir, "..", filepath.Base(inputPath))

	testCases := []struct {
		name       string
		input      string
		output     string
		targetFile string
	}{
		{
			name:       "empty input path",
			input:      "",
			output:     filepath.Join(tmpDir, "out_empty_in.pdf"),
			targetFile: filepath.Join(tmpDir, "out_empty_in.pdf"),
		},
		{
			name:       "empty output path",
			input:      inputPath,
			output:     "",
			targetFile: "",
		},
		{
			name:       "nonexistent input file",
			input:      filepath.Join(tmpDir, "nonexistent.pdf"),
			output:     filepath.Join(tmpDir, "out_nonexistent.pdf"),
			targetFile: filepath.Join(tmpDir, "out_nonexistent.pdf"),
		},
		{
			name:       "directory supplied as input",
			input:      tmpDir,
			output:     filepath.Join(tmpDir, "out_dir_in.pdf"),
			targetFile: filepath.Join(tmpDir, "out_dir_in.pdf"),
		},
		{
			name:       "input and output referring to same file",
			input:      inputPath,
			output:     inputPath,
			targetFile: inputPath,
		},
		{
			name:       "equivalent paths expressed through relative path",
			input:      inputPath,
			output:     relEquivalentOutput,
			targetFile: relEquivalentOutput,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			err := SlicePDFForResume(context.Background(), tc.input, tc.output, 1)
			if err == nil {
				t.Fatalf("expected error for case %q, got nil", tc.name)
			}
			// Verify input was not corrupted or replaced
			currentChecksum := computeFileSHA256(t, inputPath)
			if currentChecksum != initialChecksum {
				t.Fatalf("input file was corrupted or modified during case %q", tc.name)
			}
		})
	}

	// Also test symlink to input file if supported
	symlinkOutput := filepath.Join(tmpDir, "fixture_symlink.pdf")
	if err := os.Symlink(inputPath, symlinkOutput); err == nil {
		t.Run("symlink to input as output", func(t *testing.T) {
			err := SlicePDFForResume(context.Background(), inputPath, symlinkOutput, 1)
			if err == nil {
				t.Fatalf("expected error when output is symlink to input, got nil")
			}
			currentChecksum := computeFileSHA256(t, inputPath)
			if currentChecksum != initialChecksum {
				t.Fatalf("input file was modified during symlink test")
			}
		})
	}
}

// Test F — TestSlicePDFForResume_CorruptPDF
func TestSlicePDFForResume_CorruptPDF(t *testing.T) {
	tmpDir := t.TempDir()
	corruptPath := filepath.Join(tmpDir, "corrupt.pdf")
	outputPath := filepath.Join(tmpDir, "corrupt_out.pdf")

	if err := os.WriteFile(corruptPath, []byte("NOT A REAL PDF FILE HEADER OR CONTENT"), 0644); err != nil {
		t.Fatalf("failed to write corrupt fixture: %v", err)
	}

	err := SlicePDFForResume(context.Background(), corruptPath, outputPath, 1)
	if err == nil {
		t.Fatalf("expected error for corrupt PDF, got nil")
	}

	// Ensure no valid output file was produced
	if _, statErr := os.Stat(outputPath); statErr == nil {
		t.Fatalf("output file should not exist for corrupt input PDF")
	}
}

// Test G — TestSlicePDFForResume_CanceledContext
func TestSlicePDFForResume_CanceledContext(t *testing.T) {
	tmpDir := t.TempDir()
	inputPath := createThreePageFixture(t, tmpDir)
	outputPath := filepath.Join(tmpDir, "canceled_out.pdf")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := SlicePDFForResume(ctx, inputPath, outputPath, 1)
	if err == nil {
		t.Fatalf("expected error for canceled context, got nil")
	}

	if _, statErr := os.Stat(outputPath); statErr == nil {
		t.Fatalf("output file should not exist when context is canceled")
	}

	// Test nil context
	errNil := SlicePDFForResume(nil, inputPath, outputPath, 1)
	if errNil == nil {
		t.Fatalf("expected error for nil context, got nil")
	}
}

// Test H — TestSlicePDFForResume_OutputVerification
// Verifies outputPageCount = inputPageCount - startPage + 1
func TestSlicePDFForResume_OutputVerification(t *testing.T) {
	tmpDir := t.TempDir()
	inputPath := createThreePageFixture(t, tmpDir)
	inputPageCount := getPageCount(t, inputPath)

	for startPage := 1; startPage <= inputPageCount; startPage++ {
		outputPath := filepath.Join(tmpDir, "verification_page_"+strconv.Itoa(startPage)+".pdf")
		err := SlicePDFForResume(context.Background(), inputPath, outputPath, startPage)
		if err != nil {
			t.Fatalf("startPage=%d failed: %v", startPage, err)
		}

		expectedPages := inputPageCount - startPage + 1
		actualPages := getPageCount(t, outputPath)
		if actualPages != expectedPages {
			t.Fatalf("for startPage=%d: expected %d pages, qpdf reported %d", startPage, expectedPages, actualPages)
		}
		assertPDFValid(t, outputPath)
	}
}

// Test I — TestSlicePDFForResume_InputUnchanged
func TestSlicePDFForResume_InputUnchanged(t *testing.T) {
	tmpDir := t.TempDir()
	inputPath := createThreePageFixture(t, tmpDir)
	outputPath := filepath.Join(tmpDir, "slice_input_check.pdf")

	initialChecksum := computeFileSHA256(t, inputPath)

	err := SlicePDFForResume(context.Background(), inputPath, outputPath, 2)
	if err != nil {
		t.Fatalf("SlicePDFForResume failed: %v", err)
	}

	afterChecksum := computeFileSHA256(t, inputPath)
	if initialChecksum != afterChecksum {
		t.Fatalf("input PDF checksum changed: before=%s, after=%s", initialChecksum, afterChecksum)
	}
}

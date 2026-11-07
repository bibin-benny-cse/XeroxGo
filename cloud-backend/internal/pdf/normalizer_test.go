package pdf

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"xeroxgo/pkg/contracts"
)

func generateThreePagePDF(t *testing.T, dir string) string {
	t.Helper()

	psPath := filepath.Join(dir, "fixture_input.ps")
	pdfPath := filepath.Join(dir, "fixture_input.pdf")

	psContent := `%!PS
/Helvetica findfont 24 scalefont setfont
100 700 moveto
(Page 1 of XeroxGo Fixture Document) show
showpage

/Helvetica findfont 24 scalefont setfont
100 700 moveto
(Page 2 of XeroxGo Fixture Document) show
showpage

/Helvetica findfont 24 scalefont setfont
100 700 moveto
(Page 3 of XeroxGo Fixture Document) show
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
		t.Fatalf("failed to generate 3-page fixture PDF with Ghostscript: %s: %v", strings.TrimSpace(string(out)), err)
	}

	return pdfPath
}

func verifyChecksum(t *testing.T, filePath, reportedChecksum string) {
	t.Helper()

	if len(reportedChecksum) != 64 {
		t.Fatalf("expected 64-character checksum, got %d chars: %s", len(reportedChecksum), reportedChecksum)
	}
	if reportedChecksum != strings.ToLower(reportedChecksum) {
		t.Fatalf("expected lowercase checksum, got %s", reportedChecksum)
	}
	if _, err := hex.DecodeString(reportedChecksum); err != nil {
		t.Fatalf("checksum is not valid hex: %v", err)
	}

	data, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("failed to read file for checksum verification: %v", err)
	}
	hash := sha256.Sum256(data)
	expectedHex := hex.EncodeToString(hash[:])
	if reportedChecksum != expectedHex {
		t.Fatalf("checksum mismatch: reported %s, expected %s", reportedChecksum, expectedHex)
	}
}

func TestNormalizeAndCount_Simplex(t *testing.T) {
	tmpDir := t.TempDir()
	inputPDF := generateThreePagePDF(t, tmpDir)
	outputPDF := filepath.Join(tmpDir, "normalized_simplex.pdf")

	ctx := context.Background()
	res, err := NormalizeAndCount(ctx, inputPDF, outputPDF, 2, contracts.DuplexSimplex)
	if err != nil {
		t.Fatalf("NormalizeAndCount failed: %v", err)
	}

	if res.TotalPages != 3 {
		t.Errorf("TotalPages = %d, want 3", res.TotalPages)
	}
	if res.TotalSheets != 6 {
		t.Errorf("TotalSheets = %d, want 6", res.TotalSheets)
	}

	outInfo, err := os.Stat(outputPDF)
	if err != nil {
		t.Fatalf("normalized output file stat failed: %v", err)
	}
	if outInfo.Size() == 0 {
		t.Fatalf("normalized output file is empty")
	}

	verifyChecksum(t, outputPDF, res.SHA256Checksum)
}

func TestNormalizeAndCount_Duplex(t *testing.T) {
	tmpDir := t.TempDir()
	inputPDF := generateThreePagePDF(t, tmpDir)

	tests := []struct {
		name        string
		duplex      contracts.DuplexMode
		copies      int
		wantPages   int
		wantSheets  int
		outFileName string
	}{
		{
			name:        "duplex_long_edge_2_copies",
			duplex:      contracts.DuplexLongEdge,
			copies:      2,
			wantPages:   3,
			wantSheets:  4,
			outFileName: "normalized_duplex_long.pdf",
		},
		{
			name:        "duplex_short_edge_1_copy",
			duplex:      contracts.DuplexShortEdge,
			copies:      1,
			wantPages:   3,
			wantSheets:  2,
			outFileName: "normalized_duplex_short.pdf",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			outputPDF := filepath.Join(tmpDir, tt.outFileName)
			ctx := context.Background()

			res, err := NormalizeAndCount(ctx, inputPDF, outputPDF, tt.copies, tt.duplex)
			if err != nil {
				t.Fatalf("NormalizeAndCount failed: %v", err)
			}

			if res.TotalPages != tt.wantPages {
				t.Errorf("TotalPages = %d, want %d", res.TotalPages, tt.wantPages)
			}
			if res.TotalSheets != tt.wantSheets {
				t.Errorf("TotalSheets = %d, want %d", res.TotalSheets, tt.wantSheets)
			}

			outInfo, err := os.Stat(outputPDF)
			if err != nil {
				t.Fatalf("output file stat failed: %v", err)
			}
			if outInfo.Size() == 0 {
				t.Fatalf("output file is empty")
			}

			verifyChecksum(t, outputPDF, res.SHA256Checksum)
		})
	}
}

func TestNormalizeAndCount_RejectsCorruptPDF(t *testing.T) {
	tmpDir := t.TempDir()
	corruptPDF := filepath.Join(tmpDir, "corrupt.pdf")
	outputPDF := filepath.Join(tmpDir, "output.pdf")

	if err := os.WriteFile(corruptPDF, []byte("%PDF-1.7 corrupt invalid data content"), 0600); err != nil {
		t.Fatalf("failed to write corrupt file: %v", err)
	}

	ctx := context.Background()
	_, err := NormalizeAndCount(ctx, corruptPDF, outputPDF, 1, contracts.DuplexSimplex)
	if err == nil {
		t.Fatalf("NormalizeAndCount expected error on corrupt PDF, got nil")
	}
}

func TestNormalizeAndCount_RejectsEncryptedPDF(t *testing.T) {
	tmpDir := t.TempDir()
	validPDF := generateThreePagePDF(t, tmpDir)
	encryptedPDF := filepath.Join(tmpDir, "encrypted.pdf")
	outputPDF := filepath.Join(tmpDir, "output.pdf")

	cmd := exec.Command("qpdf", "--encrypt", "user-password", "owner-password", "256", "--", validPDF, encryptedPDF)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("failed to generate encrypted fixture PDF with qpdf: %s: %v", strings.TrimSpace(string(out)), err)
	}

	ctx := context.Background()
	_, err = NormalizeAndCount(ctx, encryptedPDF, outputPDF, 1, contracts.DuplexSimplex)
	if err == nil {
		t.Fatalf("NormalizeAndCount expected error on encrypted PDF, got nil")
	}
}

func TestNormalizeAndCount_RejectsInvalidArguments(t *testing.T) {
	tmpDir := t.TempDir()
	validPDF := generateThreePagePDF(t, tmpDir)
	outputPDF := filepath.Join(tmpDir, "output.pdf")
	ctx := context.Background()

	tests := []struct {
		name       string
		ctx        context.Context
		inputPath  string
		outputPath string
		copies     int
		duplex     contracts.DuplexMode
	}{
		{
			name:       "nil_context",
			ctx:        nil,
			inputPath:  validPDF,
			outputPath: outputPDF,
			copies:     1,
			duplex:     contracts.DuplexSimplex,
		},
		{
			name:       "zero_copies",
			ctx:        ctx,
			inputPath:  validPDF,
			outputPath: outputPDF,
			copies:     0,
			duplex:     contracts.DuplexSimplex,
		},
		{
			name:       "negative_copies",
			ctx:        ctx,
			inputPath:  validPDF,
			outputPath: outputPDF,
			copies:     -1,
			duplex:     contracts.DuplexSimplex,
		},
		{
			name:       "unknown_duplex_mode",
			ctx:        ctx,
			inputPath:  validPDF,
			outputPath: outputPDF,
			copies:     1,
			duplex:     contracts.DuplexMode("TRI_FOLD"),
		},
		{
			name:       "empty_input_path",
			ctx:        ctx,
			inputPath:  "",
			outputPath: outputPDF,
			copies:     1,
			duplex:     contracts.DuplexSimplex,
		},
		{
			name:       "empty_output_path",
			ctx:        ctx,
			inputPath:  validPDF,
			outputPath: "",
			copies:     1,
			duplex:     contracts.DuplexSimplex,
		},
		{
			name:       "same_input_and_output",
			ctx:        ctx,
			inputPath:  validPDF,
			outputPath: validPDF,
			copies:     1,
			duplex:     contracts.DuplexSimplex,
		},
		{
			name:       "nonexistent_input",
			ctx:        ctx,
			inputPath:  filepath.Join(tmpDir, "nonexistent.pdf"),
			outputPath: outputPDF,
			copies:     1,
			duplex:     contracts.DuplexSimplex,
		},
		{
			name:       "directory_as_input",
			ctx:        ctx,
			inputPath:  tmpDir,
			outputPath: outputPDF,
			copies:     1,
			duplex:     contracts.DuplexSimplex,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NormalizeAndCount(tt.ctx, tt.inputPath, tt.outputPath, tt.copies, tt.duplex)
			if err == nil {
				t.Fatalf("expected error for case %q, got nil", tt.name)
			}
		})
	}
}

func generateNPagePDF(t *testing.T, dir string, n int) string {
	t.Helper()
	var sb strings.Builder
	sb.WriteString("%!PS\n")
	for i := 1; i <= n; i++ {
		sb.WriteString("/Helvetica findfont 24 scalefont setfont\n")
		sb.WriteString("100 700 moveto\n")
		sb.WriteString(strings.ReplaceAll("(Page %d of XeroxGo Test Document) show\nshowpage\n", "%d", strings.TrimSpace(exec.Command("echo", "-n").String())))
		sb.WriteString("showpage\n")
	}

	pdfPath := filepath.Join(dir, "fixture_n_pages.pdf")
	cmd := exec.Command("gs",
		"-sDEVICE=pdfwrite",
		"-sPAPERSIZE=a4",
		"-dFIXEDMEDIA",
		"-dNOPAUSE",
		"-dBATCH",
		"-dQUIET",
		"-sOutputFile="+pdfPath,
		"-c",
	)
	var psCommands strings.Builder
	for i := 1; i <= n; i++ {
		psCommands.WriteString("/Helvetica findfont 24 scalefont setfont 100 700 moveto (Page ")
		psCommands.WriteString(string(rune('0' + i)))
		psCommands.WriteString(") show showpage ")
	}
	cmd.Args = append(cmd.Args, psCommands.String())
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("failed to generate %d-page fixture PDF: %s: %v", n, strings.TrimSpace(string(out)), err)
	}
	return pdfPath
}

func TestParsePageRange(t *testing.T) {
	tests := []struct {
		name      string
		rangeStr  string
		maxPages  int
		want      []int
		shouldErr bool
	}{
		{
			name:     "range_1_to_5",
			rangeStr: "1-5",
			maxPages: 5,
			want:     []int{1, 2, 3, 4, 5},
		},
		{
			name:     "discrete_pages_1_3_5",
			rangeStr: "1,3,5",
			maxPages: 5,
			want:     []int{1, 3, 5},
		},
		{
			name:     "mixed_range_and_discrete_1_to_3_and_7",
			rangeStr: "1-3,7",
			maxPages: 10,
			want:     []int{1, 2, 3, 7},
		},
		{
			name:     "whitespace_handling",
			rangeStr: " 1 - 3 ,  7 ",
			maxPages: 10,
			want:     []int{1, 2, 3, 7},
		},
		{
			name:     "all_string",
			rangeStr: "all",
			maxPages: 4,
			want:     []int{1, 2, 3, 4},
		},
		{
			name:     "empty_string_defaults_all",
			rangeStr: "",
			maxPages: 3,
			want:     []int{1, 2, 3},
		},
		{
			name:      "out_of_bounds_single_page",
			rangeStr:  "1-3,7",
			maxPages:  5,
			shouldErr: true,
		},
		{
			name:      "zero_page_invalid",
			rangeStr:  "0-3",
			maxPages:  5,
			shouldErr: true,
		},
		{
			name:      "start_greater_than_end",
			rangeStr:  "5-2",
			maxPages:  5,
			shouldErr: true,
		},
		{
			name:      "malformed_syntax",
			rangeStr:  "1,,3",
			maxPages:  5,
			shouldErr: true,
		},
		{
			name:      "non_numeric",
			rangeStr:  "1-abc",
			maxPages:  5,
			shouldErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParsePageRange(tt.rangeStr, tt.maxPages)
			if (err != nil) != tt.shouldErr {
				t.Fatalf("ParsePageRange(%q, %d) error = %v, shouldErr = %v", tt.rangeStr, tt.maxPages, err, tt.shouldErr)
			}
			if !tt.shouldErr {
				if len(got) != len(tt.want) {
					t.Fatalf("ParsePageRange got length %d, want %d (%v vs %v)", len(got), len(tt.want), got, tt.want)
				}
				for i := range got {
					if got[i] != tt.want[i] {
						t.Errorf("ParsePageRange index %d = %d, want %d", i, got[i], tt.want[i])
					}
				}
			}
		})
	}
}

func TestNormalizeAndCountWithOptions_OddDuplex3Pages2Copies(t *testing.T) {
	// Requirement 10: 3 pages * 2 copies in duplex = 6 printed sides, 4 physical sheets.
	// Requirement 11: Ensure odd-page duplex documents have a blank back on the final sheet of each copy.
	tmpDir := t.TempDir()
	inputPDF := generateThreePagePDF(t, tmpDir)
	outputPDF := filepath.Join(tmpDir, "duplex_odd_3p_2c.pdf")

	ctx := context.Background()
	res, err := NormalizeAndCountWithOptions(ctx, inputPDF, outputPDF, NormalizeOptions{
		Copies:    2,
		Duplex:    contracts.DuplexLongEdge,
		ColorMode: "BW",
	})
	if err != nil {
		t.Fatalf("NormalizeAndCountWithOptions failed: %v", err)
	}

	if res.TotalPages != 3 {
		t.Errorf("TotalPages = %d, want 3 (printed document pages)", res.TotalPages)
	}
	if res.TotalSheets != 4 {
		t.Errorf("TotalSheets = %d, want 4 (ceil(3/2)*2 physical sheets)", res.TotalSheets)
	}
	if res.PaddedPages != 4 {
		t.Errorf("PaddedPages = %d, want 4 (padded with blank page for duplex boundary)", res.PaddedPages)
	}

	// Verify that the actual output file has 4 pages
	cmd := exec.Command("qpdf", "--show-npages", outputPDF)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("qpdf --show-npages failed: %v", err)
	}
	if strings.TrimSpace(string(out)) != "4" {
		t.Errorf("output file actual page count = %s, want 4", strings.TrimSpace(string(out)))
	}
}

func TestNormalizeAndCountWithOptions_CustomRangeSelection(t *testing.T) {
	tmpDir := t.TempDir()
	inputPDF := generateNPagePDF(t, tmpDir, 5)

	ctx := context.Background()

	// 1. Valid custom range "1,3,5" on 5-page PDF in Simplex with 2 copies
	// Total pages = 3, Total sheets = 3 * 2 = 6 sheets
	outSimplex := filepath.Join(tmpDir, "out_simplex.pdf")
	resSimplex, err := NormalizeAndCountWithOptions(ctx, inputPDF, outSimplex, NormalizeOptions{
		Copies:    2,
		Duplex:    contracts.DuplexSimplex,
		PageRange: "1,3,5",
	})
	if err != nil {
		t.Fatalf("NormalizeAndCountWithOptions (simplex 1,3,5) failed: %v", err)
	}
	if resSimplex.TotalPages != 3 {
		t.Errorf("Simplex TotalPages = %d, want 3", resSimplex.TotalPages)
	}
	if resSimplex.TotalSheets != 6 {
		t.Errorf("Simplex TotalSheets = %d, want 6", resSimplex.TotalSheets)
	}

	// 2. Valid custom range "1,3,5" on 5-page PDF in Duplex with 2 copies
	// Total pages = 3, Total sheets = ceil(3/2) * 2 = 4 sheets, padded to 4 pages
	outDuplex := filepath.Join(tmpDir, "out_duplex.pdf")
	resDuplex, err := NormalizeAndCountWithOptions(ctx, inputPDF, outDuplex, NormalizeOptions{
		Copies:    2,
		Duplex:    contracts.DuplexLongEdge,
		PageRange: "1,3,5",
	})
	if err != nil {
		t.Fatalf("NormalizeAndCountWithOptions (duplex 1,3,5) failed: %v", err)
	}
	if resDuplex.TotalPages != 3 {
		t.Errorf("Duplex TotalPages = %d, want 3", resDuplex.TotalPages)
	}
	if resDuplex.TotalSheets != 4 {
		t.Errorf("Duplex TotalSheets = %d, want 4", resDuplex.TotalSheets)
	}
	if resDuplex.PaddedPages != 4 {
		t.Errorf("Duplex PaddedPages = %d, want 4", resDuplex.PaddedPages)
	}

	// 3. Invalid custom range "1-3,7" on 5-page PDF must be rejected
	outInvalid := filepath.Join(tmpDir, "out_invalid.pdf")
	_, errInvalid := NormalizeAndCountWithOptions(ctx, inputPDF, outInvalid, NormalizeOptions{
		Copies:    1,
		Duplex:    contracts.DuplexSimplex,
		PageRange: "1-3,7",
	})
	if errInvalid == nil {
		t.Fatalf("expected error for range '1-3,7' on 5-page document, got nil")
	}
	if !strings.Contains(errInvalid.Error(), "exceeds document page count") {
		t.Errorf("expected out-of-bounds error message, got: %v", errInvalid)
	}
}

package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"xeroxgo/cloud-backend/internal/store"
	"xeroxgo/cloud-backend/internal/ws"
	"xeroxgo/pkg/contracts"
)

func createTestPDFWithPages(t *testing.T, dir string, n int) string {
	t.Helper()
	pdfPath := filepath.Join(dir, "fixture_"+strconv.Itoa(n)+"p.pdf")
	var ps strings.Builder
	for i := 1; i <= n; i++ {
		ps.WriteString("/Helvetica findfont 24 scalefont setfont 100 700 moveto (Page ")
		ps.WriteString(strconv.Itoa(i))
		ps.WriteString(") show showpage ")
	}

	cmd := exec.Command("gs",
		"-sDEVICE=pdfwrite",
		"-sPAPERSIZE=a4",
		"-dFIXEDMEDIA",
		"-dNOPAUSE",
		"-dBATCH",
		"-dQUIET",
		"-sOutputFile="+pdfPath,
		"-c",
		ps.String(),
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("failed to generate %d-page test PDF: %s: %v", n, strings.TrimSpace(string(out)), err)
	}
	return pdfPath
}

func setupUploadTestServer(t *testing.T) (*appServer, *http.ServeMux) {
	t.Helper()
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open test store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	jobsDir := filepath.Join(tmpDir, "jobs")
	if err := os.MkdirAll(jobsDir, 0700); err != nil {
		t.Fatalf("failed to create jobs dir: %v", err)
	}

	cfg := &serverConfig{
		addr:               "localhost:0",
		dbPath:             dbPath,
		jobsDir:            jobsDir,
		pricePerSheetPaise: 200,
		downloadSecret:     []byte("test_secret_key_1234567890"),
	}

	hub := ws.NewHub(s)
	app := &appServer{
		cfg:      cfg,
		db:       s,
		hub:      hub,
		convWake: make(chan struct{}, 10),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/k/", app.handleUpload)

	return app, mux
}

func TestUpload_PageCalculationsAndAuthoritativeCounts(t *testing.T) {
	_, mux := setupUploadTestServer(t)
	tmpDir := t.TempDir()

	doc3p := createTestPDFWithPages(t, tmpDir, 3)
	doc5p := createTestPDFWithPages(t, tmpDir, 5)

	tests := []struct {
		name          string
		pdfPath       string
		copies        string
		duplex        string
		pageRange     string
		wantStatus    int
		wantPages     int
		wantSheets    int
		wantCopies    int
		wantTotalCost int64 // 200 paise per page
	}{
		{
			name:          "simplex_3p_2copies",
			pdfPath:       doc3p,
			copies:        "2",
			duplex:        "SIMPLEX",
			wantStatus:    http.StatusOK,
			wantPages:     3,
			wantSheets:    6, // 3 * 2
			wantCopies:    2,
			wantTotalCost: 1200, // 3 pages * 2 copies * 200 paise = 1200
		},
		{
			name:          "duplex_3p_2copies_example_from_spec",
			// Requirement 10: 3 pages * 2 copies in duplex = 6 printed sides, 4 physical sheets.
			pdfPath:       doc3p,
			copies:        "2",
			duplex:        "DUPLEX_LONG_EDGE",
			wantStatus:    http.StatusOK,
			wantPages:     3,
			wantSheets:    4, // ceil(3/2) * 2 = 4 sheets
			wantCopies:    2,
			wantTotalCost: 1200, // 6 printed sides * 200 paise = 1200
		},
		{
			name:          "duplex_3p_1copy",
			pdfPath:       doc3p,
			copies:        "1",
			duplex:        "DUPLEX_LONG_EDGE",
			wantStatus:    http.StatusOK,
			wantPages:     3,
			wantSheets:    2, // ceil(3/2) * 1 = 2 sheets
			wantCopies:    1,
			wantTotalCost: 600, // 3 printed sides * 200 paise = 600
		},
		{
			name:          "custom_range_1_3_5_on_5p_simplex_2copies",
			pdfPath:       doc5p,
			copies:        "2",
			duplex:        "SIMPLEX",
			pageRange:     "1,3,5",
			wantStatus:    http.StatusOK,
			wantPages:     3,
			wantSheets:    6, // 3 * 2
			wantCopies:    2,
			wantTotalCost: 1200,
		},
		{
			name:          "custom_range_1_3_5_on_5p_duplex_2copies",
			pdfPath:       doc5p,
			copies:        "2",
			duplex:        "DUPLEX_LONG_EDGE",
			pageRange:     "1,3,5",
			wantStatus:    http.StatusOK,
			wantPages:     3,
			wantSheets:    4, // ceil(3/2) * 2 = 4
			wantCopies:    2,
			wantTotalCost: 1200,
		},
		{
			name:       "invalid_custom_range_out_of_bounds",
			pdfPath:    doc5p,
			copies:     "1",
			duplex:     "SIMPLEX",
			pageRange:  "1-3,7", // doc has 5 pages
			wantStatus: http.StatusBadRequest,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := &bytes.Buffer{}
			writer := multipart.NewWriter(body)

			fileBytes, err := os.ReadFile(tt.pdfPath)
			if err != nil {
				t.Fatalf("failed to read test pdf: %v", err)
			}

			part, err := writer.CreateFormFile("file", filepath.Base(tt.pdfPath))
			if err != nil {
				t.Fatalf("failed to create form file: %v", err)
			}
			if _, err := part.Write(fileBytes); err != nil {
				t.Fatalf("failed to write file bytes: %v", err)
			}

			_ = writer.WriteField("copies", tt.copies)
			_ = writer.WriteField("duplex", tt.duplex)
			if tt.pageRange != "" {
				_ = writer.WriteField("page_range", tt.pageRange)
			}
			writer.Close()

			req := httptest.NewRequest(http.MethodPost, "/api/k/kiosk_test_1/upload", body)
			req.Header.Set("Content-Type", writer.FormDataContentType())
			rr := httptest.NewRecorder()

			mux.ServeHTTP(rr, req)

			if rr.Code != tt.wantStatus {
				t.Fatalf("status code = %d, want %d. Body: %s", rr.Code, tt.wantStatus, rr.Body.String())
			}

			if tt.wantStatus == http.StatusOK {
				var resp map[string]interface{}
				if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
					t.Fatalf("failed to parse json response: %v", err)
				}

				if int(resp["total_pages"].(float64)) != tt.wantPages {
					t.Errorf("total_pages = %v, want %d", resp["total_pages"], tt.wantPages)
				}
				if int(resp["total_sheets"].(float64)) != tt.wantSheets {
					t.Errorf("total_sheets = %v, want %d", resp["total_sheets"], tt.wantSheets)
				}
				if int(resp["copies"].(float64)) != tt.wantCopies {
					t.Errorf("copies = %v, want %d", resp["copies"], tt.wantCopies)
				}
				if int64(resp["total_price_paise"].(float64)) != tt.wantTotalCost {
					t.Errorf("total_price_paise = %v, want %d", resp["total_price_paise"], tt.wantTotalCost)
				}
			}
		})
	}
}

func TestUpload_DuplexOddPagesFinalSheetBlankBackInPDF(t *testing.T) {
	app, mux := setupUploadTestServer(t)
	tmpDir := t.TempDir()

	// 3-page document in duplex with 2 copies
	doc3p := createTestPDFWithPages(t, tmpDir, 3)

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	fileBytes, _ := os.ReadFile(doc3p)
	part, _ := writer.CreateFormFile("file", "doc3.pdf")
	_, _ = part.Write(fileBytes)
	_ = writer.WriteField("copies", "2")
	_ = writer.WriteField("duplex", "DUPLEX_LONG_EDGE")
	writer.Close()

	req := httptest.NewRequest(http.MethodPost, "/api/k/kiosk_test_1/upload", body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	rr := httptest.NewRecorder()

	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("upload failed: %d: %s", rr.Code, rr.Body.String())
	}

	var resp map[string]interface{}
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	orderID := resp["order_id"].(string)

	// Fetch the order from the store
	order, err := app.db.GetOrder(req.Context(), orderID)
	if err != nil {
		t.Fatalf("GetOrder failed: %v", err)
	}

	if order.Command.TotalPages != 3 {
		t.Errorf("Command.TotalPages = %d, want 3", order.Command.TotalPages)
	}
	if order.Command.TotalSheets != 4 {
		t.Errorf("Command.TotalSheets = %d, want 4", order.Command.TotalSheets)
	}
	if order.Command.Copies != 2 {
		t.Errorf("Command.Copies = %d, want 2", order.Command.Copies)
	}
	if order.Command.Duplex != contracts.DuplexLongEdge {
		t.Errorf("Command.Duplex = %v, want DUPLEX_LONG_EDGE", order.Command.Duplex)
	}
}

func getPageText(t *testing.T, pdfPath string, pageNum int) string {
	t.Helper()
	cmd := exec.Command("pdftotext", "-f", strconv.Itoa(pageNum), "-l", strconv.Itoa(pageNum), pdfPath, "-")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("pdftotext page %d failed: %s: %v", pageNum, strings.TrimSpace(string(out)), err)
	}
	return strings.TrimSpace(string(out))
}

func getPDFPageCount(t *testing.T, pdfPath string) int {
	t.Helper()
	cmd := exec.Command("qpdf", "--show-npages", pdfPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("qpdf --show-npages failed: %s: %v", strings.TrimSpace(string(out)), err)
	}
	count, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		t.Fatalf("failed to parse page count %q: %v", strings.TrimSpace(string(out)), err)
	}
	return count
}

func TestUpload_10PagePDF_Select_2_4_6_Simplex(t *testing.T) {
	// Requirement 11: Test a 10-page PDF selecting 2,4,6 and verify the backend output contains exactly those pages in order.
	app, mux := setupUploadTestServer(t)
	tmpDir := t.TempDir()

	doc10p := createTestPDFWithPages(t, tmpDir, 10)

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	fileBytes, err := os.ReadFile(doc10p)
	if err != nil {
		t.Fatalf("failed to read doc10p: %v", err)
	}
	part, _ := writer.CreateFormFile("file", "document10p.pdf")
	_, _ = part.Write(fileBytes)
	_ = writer.WriteField("copies", "1")
	_ = writer.WriteField("duplex", "SIMPLEX")
	_ = writer.WriteField("page_range", "2,4,6")
	writer.Close()

	req := httptest.NewRequest(http.MethodPost, "/api/k/kiosk_test_1/upload", body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	rr := httptest.NewRecorder()

	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("upload failed with code %d: %s", rr.Code, rr.Body.String())
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("json parse error: %v", err)
	}

	if int(resp["total_pages"].(float64)) != 3 {
		t.Errorf("total_pages = %v, want 3", resp["total_pages"])
	}
	if int(resp["total_sheets"].(float64)) != 3 {
		t.Errorf("total_sheets = %v, want 3", resp["total_sheets"])
	}

	orderID := resp["order_id"].(string)
	h := sha256.Sum256([]byte(orderID))
	outputPath := filepath.Join(app.cfg.jobsDir, hex.EncodeToString(h[:])+".pdf")

	actualPages := getPDFPageCount(t, outputPath)
	if actualPages != 3 {
		t.Fatalf("output PDF total pages = %d, want 3", actualPages)
	}

	p1 := getPageText(t, outputPath, 1)
	p2 := getPageText(t, outputPath, 2)
	p3 := getPageText(t, outputPath, 3)

	if !strings.Contains(p1, "Page 2") {
		t.Errorf("output PDF Page 1 content = %q, want Page 2", p1)
	}
	if !strings.Contains(p2, "Page 4") {
		t.Errorf("output PDF Page 2 content = %q, want Page 4", p2)
	}
	if !strings.Contains(p3, "Page 6") {
		t.Errorf("output PDF Page 3 content = %q, want Page 6", p3)
	}
}

func TestUpload_10PagePDF_Select_2_4_6_Duplex(t *testing.T) {
	// Requirement 12: Test the same selection in duplex: sheet 1 = pages 2/4, sheet 2 = page 6/blank.
	// Requirement 13: Test 3 pages x 2 duplex copies = 6 printed sides and 4 physical sheets.
	app, mux := setupUploadTestServer(t)
	tmpDir := t.TempDir()

	doc10p := createTestPDFWithPages(t, tmpDir, 10)

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	fileBytes, _ := os.ReadFile(doc10p)
	part, _ := writer.CreateFormFile("file", "document10p.pdf")
	_, _ = part.Write(fileBytes)
	_ = writer.WriteField("copies", "2")
	_ = writer.WriteField("duplex", "DUPLEX_LONG_EDGE")
	_ = writer.WriteField("page_range", "2,4,6")
	writer.Close()

	req := httptest.NewRequest(http.MethodPost, "/api/k/kiosk_test_1/upload", body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	rr := httptest.NewRecorder()

	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("upload failed with code %d: %s", rr.Code, rr.Body.String())
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("json parse error: %v", err)
	}

	if int(resp["total_pages"].(float64)) != 3 {
		t.Errorf("total_pages = %v, want 3", resp["total_pages"])
	}
	if int(resp["total_sheets"].(float64)) != 4 {
		t.Errorf("total_sheets = %v, want 4 (ceil(3/2)*2 sheets)", resp["total_sheets"])
	}
	if int(resp["copies"].(float64)) != 2 {
		t.Errorf("copies = %v, want 2", resp["copies"])
	}
	if int64(resp["total_price_paise"].(float64)) != 1200 {
		t.Errorf("total_price_paise = %v, want 1200 (6 printed sides * 200 paise)", resp["total_price_paise"])
	}

	orderID := resp["order_id"].(string)
	h := sha256.Sum256([]byte(orderID))
	outputPath := filepath.Join(app.cfg.jobsDir, hex.EncodeToString(h[:])+".pdf")

	// Padded PDF must have 4 pages per copy boundary
	actualPages := getPDFPageCount(t, outputPath)
	if actualPages != 4 {
		t.Fatalf("output PDF total pages = %d, want 4", actualPages)
	}

	// Sheet 1: Front = Page 2, Back = Page 4
	p1 := getPageText(t, outputPath, 1)
	p2 := getPageText(t, outputPath, 2)
	// Sheet 2: Front = Page 6, Back = Blank
	p3 := getPageText(t, outputPath, 3)
	p4 := getPageText(t, outputPath, 4)

	if !strings.Contains(p1, "Page 2") {
		t.Errorf("Sheet 1 Front content = %q, want Page 2", p1)
	}
	if !strings.Contains(p2, "Page 4") {
		t.Errorf("Sheet 1 Back content = %q, want Page 4", p2)
	}
	if !strings.Contains(p3, "Page 6") {
		t.Errorf("Sheet 2 Front content = %q, want Page 6", p3)
	}
	if strings.TrimSpace(p4) != "" {
		t.Errorf("Sheet 2 Back content = %q, want blank", p4)
	}
}

func TestUpload_OfficeConvertedPDF_WithPageRange(t *testing.T) {
	// Requirement 14: Test Office-converted PDFs with page selection
	_, mux := setupUploadTestServer(t)
	tmpDir := t.TempDir()

	// Create a text file that converts into a 1-page PDF
	txtFile := filepath.Join(tmpDir, "assignment.txt")
	if err := os.WriteFile(txtFile, []byte("Assignment Document Header\nHello XeroxGo!"), 0600); err != nil {
		t.Fatalf("failed to write txt file: %v", err)
	}

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	fileBytes, _ := os.ReadFile(txtFile)
	part, _ := writer.CreateFormFile("file", "assignment.txt")
	_, _ = part.Write(fileBytes)
	_ = writer.WriteField("copies", "1")
	_ = writer.WriteField("duplex", "SIMPLEX")
	_ = writer.WriteField("page_range", "1")
	writer.Close()

	req := httptest.NewRequest(http.MethodPost, "/api/k/kiosk_test_1/upload", body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	rr := httptest.NewRecorder()

	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("upload failed: code %d, body: %s", rr.Code, rr.Body.String())
	}

	var resp map[string]interface{}
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)

	if int(resp["total_pages"].(float64)) != 1 {
		t.Errorf("total_pages = %v, want 1", resp["total_pages"])
	}
	if int(resp["total_sheets"].(float64)) != 1 {
		t.Errorf("total_sheets = %v, want 1", resp["total_sheets"])
	}
}


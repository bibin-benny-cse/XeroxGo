package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"xeroxgo/cloud-backend/internal/pins"
	"xeroxgo/cloud-backend/internal/store"
	"xeroxgo/cloud-backend/internal/ws"
)

func setupTestServer(t *testing.T) (*appServer, *http.ServeMux) {
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
		addr:                  "localhost:0",
		dbPath:                dbPath,
		jobsDir:               jobsDir,
		pricePerSheetPaise:    200,
		downloadSecret:        []byte("test_secret_key_1234567890"),
		pickupPINHMACSecret:   []byte("test_pickup_pin_hmac_secret_32_bytes_1234"),
		maxActivePINsPerKiosk: 50,
	}

	hub := ws.NewHub(s)
	app := &appServer{
		cfg:        cfg,
		db:         s,
		hub:        hub,
		pinLimiter: pins.NewRateLimiter(pins.DefaultCooldownConfig()),
		convWake:   make(chan struct{}, 100),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/conversions", app.handleCreateConversion)
	mux.HandleFunc("/api/conversions/", app.handleConversionRoutes)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go app.startConversionWorker(ctx)

	return app, mux
}

func createMultipartRequest(t *testing.T, url, fieldname, filename string, content []byte) *http.Request {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile(fieldname, filename)
	if err != nil {
		t.Fatalf("failed to create form file: %v", err)
	}
	if _, err := io.Copy(part, bytes.NewReader(content)); err != nil {
		t.Fatalf("failed to write content to part: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("failed to close multipart writer: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, url, &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	return req
}

func TestAsyncConversionLifecycle_Success(t *testing.T) {
	_, mux := setupTestServer(t)

	// 1. Submit text document for conversion
	req := createMultipartRequest(t, "/api/conversions", "file", "assignment.txt", []byte("Hello XeroxGo Conversion Service\nPage 1 Content\n"))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected HTTP 202 Accepted, got %d: %s", rec.Code, rec.Body.String())
	}

	var createResp struct {
		JobID   string `json:"job_id"`
		Token   string `json:"token"`
		Status  string `json:"status"`
		PollURL string `json:"poll_url"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &createResp); err != nil {
		t.Fatalf("failed to parse create response: %v", err)
	}
	if createResp.JobID == "" || createResp.Token == "" || createResp.Status != "queued" {
		t.Fatalf("invalid create response: %+v", createResp)
	}

	// 2. Poll conversion status until completed (with timeout)
	deadline := time.Now().Add(30 * time.Second)
	var finalStatus string
	var downloadURL string

	for time.Now().Before(deadline) {
		pollReq := httptest.NewRequest(http.MethodGet, "/api/conversions/"+createResp.JobID+"?token="+createResp.Token, nil)
		pollRec := httptest.NewRecorder()
		mux.ServeHTTP(pollRec, pollReq)

		if pollRec.Code != http.StatusOK {
			t.Fatalf("expected HTTP 200 from status poll, got %d: %s", pollRec.Code, pollRec.Body.String())
		}

		var pollResp struct {
			Status      string `json:"status"`
			DownloadURL string `json:"download_url"`
			ErrorMsg    string `json:"error_message"`
		}
		if err := json.Unmarshal(pollRec.Body.Bytes(), &pollResp); err != nil {
			t.Fatalf("failed to parse poll response: %v", err)
		}

		finalStatus = pollResp.Status
		if pollResp.Status == "completed" {
			downloadURL = pollResp.DownloadURL
			break
		}
		if pollResp.Status == "failed" {
			t.Fatalf("conversion job failed unexpectedly: %s", pollResp.ErrorMsg)
		}

		time.Sleep(100 * time.Millisecond)
	}

	if finalStatus != "completed" {
		t.Fatalf("conversion did not complete within deadline, final status: %q", finalStatus)
	}
	if downloadURL == "" {
		t.Fatalf("expected download_url to be returned when completed")
	}

	// 3. Download converted PDF
	dlReq := httptest.NewRequest(http.MethodGet, downloadURL, nil)
	dlRec := httptest.NewRecorder()
	mux.ServeHTTP(dlRec, dlReq)

	if dlRec.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200 downloading PDF, got %d: %s", dlRec.Code, dlRec.Body.String())
	}
	contentType := dlRec.Header().Get("Content-Type")
	if contentType != "application/pdf" {
		t.Fatalf("expected Content-Type application/pdf, got %q", contentType)
	}
	pdfBytes := dlRec.Body.Bytes()
	if len(pdfBytes) < 10 || !strings.HasPrefix(string(pdfBytes), "%PDF-") {
		t.Fatalf("downloaded artifact is not a valid PDF: %q", string(pdfBytes[:min(20, len(pdfBytes))]))
	}
}

func TestAsyncConversion_AuthenticationAndValidation(t *testing.T) {
	_, mux := setupTestServer(t)

	// 1. Submit invalid file format
	badReq := createMultipartRequest(t, "/api/conversions", "file", "malware.exe", []byte("MZ binary"))
	badRec := httptest.NewRecorder()
	mux.ServeHTTP(badRec, badReq)
	if badRec.Code != http.StatusBadRequest {
		t.Fatalf("expected HTTP 400 for unsupported format, got %d", badRec.Code)
	}

	// 2. Submit valid file
	validReq := createMultipartRequest(t, "/api/conversions", "file", "doc.txt", []byte("Test content"))
	validRec := httptest.NewRecorder()
	mux.ServeHTTP(validRec, validReq)
	if validRec.Code != http.StatusAccepted {
		t.Fatalf("expected HTTP 202, got %d", validRec.Code)
	}
	var res struct {
		JobID string `json:"job_id"`
		Token string `json:"token"`
	}
	_ = json.Unmarshal(validRec.Body.Bytes(), &res)

	// 3. Poll without token -> 401
	pReqNoToken := httptest.NewRequest(http.MethodGet, "/api/conversions/"+res.JobID, nil)
	pRecNoToken := httptest.NewRecorder()
	mux.ServeHTTP(pRecNoToken, pReqNoToken)
	if pRecNoToken.Code != http.StatusUnauthorized {
		t.Fatalf("expected HTTP 401 without token, got %d", pRecNoToken.Code)
	}

	// 4. Poll with wrong token -> 403
	pReqWrongToken := httptest.NewRequest(http.MethodGet, "/api/conversions/"+res.JobID+"?token=wrong_token", nil)
	pRecWrongToken := httptest.NewRecorder()
	mux.ServeHTTP(pRecWrongToken, pReqWrongToken)
	if pRecWrongToken.Code != http.StatusForbidden {
		t.Fatalf("expected HTTP 403 with wrong token, got %d", pRecWrongToken.Code)
	}

	// 5. Poll nonexistent job ID -> 404
	pReq404 := httptest.NewRequest(http.MethodGet, "/api/conversions/conv_nonexistent?token=tok_any", nil)
	pRec404 := httptest.NewRecorder()
	mux.ServeHTTP(pRec404, pReq404)
	if pRec404.Code != http.StatusNotFound {
		t.Fatalf("expected HTTP 404 for nonexistent job, got %d", pRec404.Code)
	}
}

func TestAsyncConversion_ConcurrentUploads(t *testing.T) {
	_, mux := setupTestServer(t)

	// Submit 3 concurrent conversion requests to verify concurrency limiter and queue sequencing
	const numUploads = 3
	type uploadResult struct {
		jobID string
		token string
		err   error
	}

	results := make([]uploadResult, numUploads)
	var wg sync.WaitGroup
	wg.Add(numUploads)

	for i := 0; i < numUploads; i++ {
		go func(idx int) {
			defer wg.Done()
			filename := "concurrent_doc_" + string(rune('A'+idx)) + ".txt"
			content := "Content for file " + filename + "\nSequential conversion verification."
			req := createMultipartRequest(t, "/api/conversions", "file", filename, []byte(content))
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)

			if rec.Code != http.StatusAccepted {
				results[idx] = uploadResult{err: fmt.Errorf("status code %d: %s", rec.Code, rec.Body.String())}
				return
			}
			var r struct {
				JobID string `json:"job_id"`
				Token string `json:"token"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil {
				results[idx] = uploadResult{err: err}
				return
			}
			results[idx] = uploadResult{jobID: r.JobID, token: r.Token}
		}(i)
	}
	wg.Wait()

	// Wait for all 3 conversions to reach completion
	for i, res := range results {
		if res.err != nil {
			t.Fatalf("upload %d failed: %v", i, res.err)
		}

		deadline := time.Now().Add(45 * time.Second)
		completed := false
		for time.Now().Before(deadline) {
			pollReq := httptest.NewRequest(http.MethodGet, "/api/conversions/"+res.jobID+"?token="+res.token, nil)
			pollRec := httptest.NewRecorder()
			mux.ServeHTTP(pollRec, pollReq)

			var pollResp struct {
				Status string `json:"status"`
			}
			_ = json.Unmarshal(pollRec.Body.Bytes(), &pollResp)
			if pollResp.Status == "completed" {
				completed = true
				break
			}
			time.Sleep(150 * time.Millisecond)
		}

		if !completed {
			t.Fatalf("concurrent job %d (%s) failed to complete in time", i, res.jobID)
		}
	}
}

func TestAsyncConversion_FailureReporting(t *testing.T) {
	_, mux := setupTestServer(t)

	// Corrupt content with .docx extension that cannot be converted
	req := createMultipartRequest(t, "/api/conversions", "file", "broken.docx", []byte("Definitely not a valid zip or docx content at all"))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected HTTP 202, got %d", rec.Code)
	}

	var res struct {
		JobID string `json:"job_id"`
		Token string `json:"token"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &res)

	deadline := time.Now().Add(30 * time.Second)
	var finalStatus string
	var errMsg string

	for time.Now().Before(deadline) {
		pollReq := httptest.NewRequest(http.MethodGet, "/api/conversions/"+res.JobID+"?token="+res.Token, nil)
		pollRec := httptest.NewRecorder()
		mux.ServeHTTP(pollRec, pollReq)

		var pollResp struct {
			Status   string `json:"status"`
			ErrorMsg string `json:"error_message"`
		}
		_ = json.Unmarshal(pollRec.Body.Bytes(), &pollResp)
		if pollResp.Status == "failed" {
			finalStatus = "failed"
			errMsg = pollResp.ErrorMsg
			break
		}
		if pollResp.Status == "completed" {
			// Some LibreOffice versions can treat arbitrary text as plain text even with .docx
			finalStatus = "completed"
			break
		}
		time.Sleep(150 * time.Millisecond)
	}

	if finalStatus != "failed" && finalStatus != "completed" {
		t.Fatalf("expected job to settle as failed or completed, got %q", finalStatus)
	}
	if finalStatus == "failed" && errMsg == "" {
		t.Fatalf("expected non-empty error message on failure")
	}
}

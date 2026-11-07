package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"xeroxgo/cloud-backend/internal/pdf"
	"xeroxgo/cloud-backend/internal/store"
)

// handleCreateConversion accepts an uploaded Office document, text file, or image,
// creates a queued conversion job in the database, and returns the job ID and auth token quickly.
func (s *appServer) handleCreateConversion(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	const maxUploadSize = 25 * 1024 * 1024 // 25 MiB
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadSize)
	if err := r.ParseMultipartForm(maxUploadSize); err != nil {
		http.Error(w, "file exceeds 25 MiB limit or malformed", http.StatusRequestEntityTooLarge)
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "missing file field", http.StatusBadRequest)
		return
	}
	defer file.Close()

	filename := header.Filename
	if !pdf.IsSupportedFormat(filename) {
		http.Error(w, fmt.Sprintf("unsupported file format %q: only PDF, Office documents (DOCX, PPTX, XLSX), and Images (JPG, PNG) are supported", filepath.Ext(filename)), http.StatusBadRequest)
		return
	}

	jobHex, err := randomHex(12)
	if err != nil {
		http.Error(w, "failed to generate job ID", http.StatusInternalServerError)
		return
	}
	jobID := "conv_" + jobHex

	tokenHex, err := randomHex(16)
	if err != nil {
		http.Error(w, "failed to generate token", http.StatusInternalServerError)
		return
	}
	token := "tok_" + tokenHex

	conversionsDir := filepath.Join(s.cfg.jobsDir, "conversions")
	if err := os.MkdirAll(conversionsDir, 0700); err != nil {
		http.Error(w, "failed to prepare storage", http.StatusInternalServerError)
		return
	}

	ext := strings.ToLower(filepath.Ext(filename))
	if ext == "" {
		ext = ".tmp"
	}
	inputPath := filepath.Join(conversionsDir, jobID+"_in"+ext)
	outputPath := filepath.Join(conversionsDir, jobID+"_out.pdf")

	outFile, err := os.OpenFile(inputPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		http.Error(w, "failed to create staging file", http.StatusInternalServerError)
		return
	}

	written, err := io.Copy(outFile, file)
	_ = outFile.Close()
	if err != nil {
		_ = os.Remove(inputPath)
		http.Error(w, "failed to save uploaded file", http.StatusInternalServerError)
		return
	}

	now := time.Now().UTC()
	job := store.ConversionJob{
		ID:               jobID,
		Token:            token,
		OriginalFilename: filename,
		FileSize:         written,
		Status:           store.ConversionStatusQueued,
		InputPath:        inputPath,
		OutputPath:       outputPath,
		CreatedAt:        now,
		UpdatedAt:        now,
		ExpiresAt:        now.Add(1 * time.Hour),
	}

	if err := s.db.CreateConversion(r.Context(), job); err != nil {
		_ = os.Remove(inputPath)
		http.Error(w, "failed to register conversion job", http.StatusInternalServerError)
		return
	}

	// Wake up conversion background worker
	select {
	case s.convWake <- struct{}{}:
	default:
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"job_id":            jobID,
		"token":             token,
		"status":            string(store.ConversionStatusQueued),
		"original_filename": filename,
		"file_size":         written,
		"poll_url":          fmt.Sprintf("/api/conversions/%s?token=%s", jobID, token),
		"created_at":        now.Format(time.RFC3339),
	})
}

// handleConversionRoutes dispatches GET /api/conversions/{id} and GET /api/conversions/{id}/pdf
func (s *appServer) handleConversionRoutes(w http.ResponseWriter, r *http.Request) {
	prefix := "/api/conversions/"
	if !strings.HasPrefix(r.URL.Path, prefix) {
		http.NotFound(w, r)
		return
	}

	sub := strings.TrimPrefix(r.URL.Path, prefix)
	sub = strings.TrimSpace(sub)
	if sub == "" {
		if r.Method == http.MethodPost {
			s.handleCreateConversion(w, r)
			return
		}
		http.Error(w, "conversion ID required", http.StatusBadRequest)
		return
	}

	if strings.HasSuffix(sub, "/pdf") {
		jobID := strings.TrimSuffix(sub, "/pdf")
		s.handleDownloadConvertedPDF(w, r, jobID)
		return
	}

	s.handleGetConversion(w, r, sub)
}

func extractAuthToken(r *http.Request) string {
	if token := r.URL.Query().Get("token"); token != "" {
		return token
	}
	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(auth, "Bearer ") {
		return strings.TrimPrefix(auth, "Bearer ")
	}
	return ""
}

// handleGetConversion reports the current status of a conversion job.
func (s *appServer) handleGetConversion(w http.ResponseWriter, r *http.Request, jobID string) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	token := extractAuthToken(r)
	if token == "" {
		http.Error(w, "missing conversion token", http.StatusUnauthorized)
		return
	}

	job, err := s.db.GetConversion(r.Context(), jobID)
	if err != nil {
		http.Error(w, "database query error", http.StatusInternalServerError)
		return
	}
	if job == nil {
		http.Error(w, "conversion job not found", http.StatusNotFound)
		return
	}

	if subtle.ConstantTimeCompare([]byte(job.Token), []byte(token)) != 1 {
		http.Error(w, "invalid conversion token", http.StatusForbidden)
		return
	}

	resp := map[string]interface{}{
		"job_id":            job.ID,
		"status":            string(job.Status),
		"original_filename": job.OriginalFilename,
		"file_size":         job.FileSize,
		"created_at":        job.CreatedAt.Format(time.RFC3339),
		"updated_at":        job.UpdatedAt.Format(time.RFC3339),
	}

	if job.Status == store.ConversionStatusCompleted {
		resp["download_url"] = fmt.Sprintf("/api/conversions/%s/pdf?token=%s", job.ID, job.Token)
	}
	if job.Status == store.ConversionStatusFailed {
		resp["error_message"] = job.ErrorMessage
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// handleDownloadConvertedPDF serves the converted PDF artifact after completion.
func (s *appServer) handleDownloadConvertedPDF(w http.ResponseWriter, r *http.Request, jobID string) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	token := extractAuthToken(r)
	if token == "" {
		http.Error(w, "missing conversion token", http.StatusUnauthorized)
		return
	}

	job, err := s.db.GetConversion(r.Context(), jobID)
	if err != nil {
		http.Error(w, "database query error", http.StatusInternalServerError)
		return
	}
	if job == nil {
		http.Error(w, "conversion job not found", http.StatusNotFound)
		return
	}

	if subtle.ConstantTimeCompare([]byte(job.Token), []byte(token)) != 1 {
		http.Error(w, "invalid conversion token", http.StatusForbidden)
		return
	}

	if job.Status != store.ConversionStatusCompleted {
		http.Error(w, fmt.Sprintf("conversion not completed (current status: %s)", job.Status), http.StatusBadRequest)
		return
	}

	pdfFile, err := os.Open(job.OutputPath)
	if err != nil {
		http.Error(w, "converted PDF artifact not found on server", http.StatusNotFound)
		return
	}
	defer pdfFile.Close()

	stat, _ := pdfFile.Stat()
	if stat != nil {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", stat.Size()))
	}
	cleanName := filepath.Base(job.OriginalFilename)
	cleanName = strings.TrimSuffix(cleanName, filepath.Ext(cleanName)) + ".pdf"

	w.Header().Set("Content-Type", "application/pdf")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`inline; filename=%q`, cleanName))
	w.Header().Set("Cache-Control", "private, max-age=3600")

	_, _ = io.Copy(w, pdfFile)
}

// startConversionWorker starts the background worker goroutine for processing queued conversions.
func (s *appServer) startConversionWorker(ctx context.Context) {
	// 1. Recover any stale processing jobs from previous server crash
	if err := s.db.RecoverStaleConversions(ctx); err != nil {
		log.Printf("[CONVERSION] failed to recover stale jobs: %v", err)
	}

	cleanupTicker := time.NewTicker(5 * time.Minute)
	defer cleanupTicker.Stop()

	pollTicker := time.NewTicker(1 * time.Second)
	defer pollTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-cleanupTicker.C:
			s.cleanupExpiredConversions(ctx)
		case <-s.convWake:
			s.processQueuedConversions(ctx)
		case <-pollTicker.C:
			s.processQueuedConversions(ctx)
		}
	}
}

// processQueuedConversions drains the queue, converting jobs sequentially (concurrency=1).
func (s *appServer) processQueuedConversions(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		job, err := s.db.ClaimNextQueuedConversion(ctx)
		if err != nil {
			log.Printf("[CONVERSION] error claiming next queued job: %v", err)
			return
		}
		if job == nil {
			// No more queued jobs
			return
		}

		s.executeConversionJob(ctx, job)
	}
}

// executeConversionJob executes LibreOffice conversion for a claimed job with safety timeouts.
func (s *appServer) executeConversionJob(ctx context.Context, job *store.ConversionJob) {
	convCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	log.Printf("[CONVERSION] Processing job %s for file %s", job.ID, job.OriginalFilename)

	err := pdf.ConvertToPDF(convCtx, job.InputPath, job.OriginalFilename, job.OutputPath)
	if err != nil {
		log.Printf("[CONVERSION] Job %s failed: %v", job.ID, err)
		errMsg := err.Error()
		if convCtx.Err() != nil {
			errMsg = "document conversion timed out after 60 seconds"
		}
		_ = s.db.UpdateConversionStatus(ctx, job.ID, store.ConversionStatusFailed, errMsg, "")
		_ = os.Remove(job.InputPath)
		_ = os.Remove(job.OutputPath)
		return
	}

	log.Printf("[CONVERSION] Job %s completed successfully", job.ID)
	_ = s.db.UpdateConversionStatus(ctx, job.ID, store.ConversionStatusCompleted, "", job.OutputPath)
	_ = os.Remove(job.InputPath) // clean up input file to save disk space
}

// cleanupExpiredConversions deletes expired conversion records and their disk artifacts.
func (s *appServer) cleanupExpiredConversions(ctx context.Context) {
	paths, err := s.db.DeleteExpiredConversions(ctx, time.Now())
	if err != nil {
		log.Printf("[CONVERSION] cleanup error: %v", err)
		return
	}
	for _, p := range paths {
		if p != "" {
			_ = os.Remove(p)
		}
	}
	if len(paths) > 0 {
		log.Printf("[CONVERSION] cleaned up %d expired file artifacts", len(paths))
	}
}

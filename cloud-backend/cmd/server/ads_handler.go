package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"xeroxgo/pkg/contracts"
)

var (
	loginLimiterMu sync.Mutex
	loginAttempts  = make(map[string][]time.Time)
)

func isRateLimited(ip string) bool {
	loginLimiterMu.Lock()
	defer loginLimiterMu.Unlock()

	now := time.Now()
	cutoff := now.Add(-15 * time.Minute)

	// Clean up old attempts for requesting IP
	var recent []time.Time
	for _, t := range loginAttempts[ip] {
		if t.After(cutoff) {
			recent = append(recent, t)
		}
	}
	if len(recent) > 0 {
		loginAttempts[ip] = recent
	} else {
		delete(loginAttempts, ip)
	}

	// Defensive memory safeguard: prevent unbounded map growth
	if len(loginAttempts) > 2000 {
		for k, times := range loginAttempts {
			var valid []time.Time
			for _, t := range times {
				if t.After(cutoff) {
					valid = append(valid, t)
				}
			}
			if len(valid) == 0 {
				delete(loginAttempts, k)
			} else {
				loginAttempts[k] = valid
			}
		}
	}

	// Allow max 5 failed attempts per 15 minutes
	return len(recent) >= 5
}

func recordFailedLogin(ip string) {
	loginLimiterMu.Lock()
	defer loginLimiterMu.Unlock()
	loginAttempts[ip] = append(loginAttempts[ip], time.Now())
}

func clearLoginAttempts(ip string) {
	loginLimiterMu.Lock()
	defer loginLimiterMu.Unlock()
	delete(loginAttempts, ip)
}

func writeJSONError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"error": message,
	})
}

func getClientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		return strings.TrimSpace(parts[0])
	}
	if xri := r.Header.Get("X-Real-IP"); xri != "" {
		return strings.TrimSpace(xri)
	}
	return r.RemoteAddr
}

func (app *appServer) authenticateAdminRequest(r *http.Request) (string, bool) {
	// 1. Try Bearer token
	authHeader := r.Header.Get("Authorization")
	if strings.HasPrefix(authHeader, "Bearer ") {
		token := strings.TrimPrefix(authHeader, "Bearer ")
		if user, ok := app.db.ValidateAdminSession(r.Context(), token); ok {
			return user, true
		}
	}

	// 2. Try Cookie
	if cookie, err := r.Cookie("xeroxgo_admin_token"); err == nil && cookie.Value != "" {
		if user, ok := app.db.ValidateAdminSession(r.Context(), cookie.Value); ok {
			return user, true
		}
	}

	return "", false
}

// handlePublicAds handles GET /api/ads (public endpoint for mobile portal & kiosk boot)
func (app *appServer) handlePublicAds(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	ads, err := app.db.GetActiveAds(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, fmt.Sprintf("failed to fetch ads: %v", err))
		return
	}

	if ads == nil {
		ads = []contracts.AdItem{}
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=10, stale-while-revalidate=60")
	_ = json.NewEncoder(w).Encode(ads)
}

// handleAdminLogin handles POST /api/admin/login
func (app *appServer) handleAdminLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	ip := getClientIP(r)
	if isRateLimited(ip) {
		writeJSONError(w, http.StatusTooManyRequests, "too many failed login attempts, please try again in 15 minutes")
		return
	}

	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	ok, err := app.db.AuthenticateAdmin(r.Context(), req.Username, req.Password)
	if err != nil {
		log.Printf("[ERROR] AuthenticateAdmin failed for %s: %v", req.Username, err)
		writeJSONError(w, http.StatusInternalServerError, "authentication database error")
		return
	}
	if !ok {
		recordFailedLogin(ip)
		writeJSONError(w, http.StatusUnauthorized, "invalid username or password")
		return
	}

	clearLoginAttempts(ip)

	token, expiresAt, err := app.db.CreateAdminSession(r.Context(), req.Username)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, fmt.Sprintf("failed to create session: %v", err))
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     "xeroxgo_admin_token",
		Value:    token,
		Path:     "/",
		Expires:  expiresAt,
		HttpOnly: true,
		Secure:   r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https",
		SameSite: http.SameSiteStrictMode,
	})

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"ok":         true,
		"token":      token,
		"username":   req.Username,
		"expires_at": expiresAt.Format(time.RFC3339),
	})
}

// handleAdminLogout handles POST /api/admin/logout
func (app *appServer) handleAdminLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	var token string
	if cookie, err := r.Cookie("xeroxgo_admin_token"); err == nil {
		token = cookie.Value
	}
	if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
		token = strings.TrimPrefix(auth, "Bearer ")
	}

	if token != "" {
		_ = app.db.RevokeAdminSession(r.Context(), token)
	}

	http.SetCookie(w, &http.Cookie{
		Name:     "xeroxgo_admin_token",
		Value:    "",
		Path:     "/",
		Expires:  time.Unix(0, 0),
		HttpOnly: true,
		MaxAge:   -1,
	})

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
}

// handleAdminMe handles GET /api/admin/me
func (app *appServer) handleAdminMe(w http.ResponseWriter, r *http.Request) {
	user, ok := app.authenticateAdminRequest(r)
	if !ok {
		writeJSONError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	kiosksOnline := 0
	if app.hub != nil {
		kiosksOnline = app.hub.ConnectedKiosksCount()
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"username":      user,
		"kiosks_online": kiosksOnline,
	})
}

// handleAdminPassword handles POST /api/admin/password
func (app *appServer) handleAdminPassword(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	user, ok := app.authenticateAdminRequest(r)
	if !ok {
		writeJSONError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	var req struct {
		CurrentPassword string `json:"current_password"`
		NewPassword     string `json:"new_password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if err := app.db.UpdateAdminPassword(r.Context(), user, req.CurrentPassword, req.NewPassword); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
}

// handleAdminAds handles CRUD for /api/admin/ads and /api/admin/ads/reorder
func (app *appServer) handleAdminAds(w http.ResponseWriter, r *http.Request) {
	_, ok := app.authenticateAdminRequest(r)
	if !ok {
		writeJSONError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	path := strings.TrimPrefix(r.URL.Path, "/api/admin/ads")
	path = strings.TrimPrefix(path, "/")

	switch {
	case path == "" || path == "/":
		if r.Method == http.MethodGet {
			// List all ads including drafts
			ads, err := app.db.GetAllAds(r.Context())
			if err != nil {
				writeJSONError(w, http.StatusInternalServerError, fmt.Sprintf("failed to get ads: %v", err))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"ads": ads})
			return
		} else if r.Method == http.MethodPost {
			// Create new ad
			var ad contracts.AdItem
			if err := json.NewDecoder(r.Body).Decode(&ad); err != nil {
				writeJSONError(w, http.StatusBadRequest, "invalid request body")
				return
			}
			if strings.TrimSpace(ad.ID) == "" {
				b := make([]byte, 8)
				_, _ = rand.Read(b)
				ad.ID = "ad-" + hex.EncodeToString(b)
			}
			if strings.TrimSpace(ad.Title) == "" {
				writeJSONError(w, http.StatusBadRequest, "title is required")
				return
			}
			if link := strings.TrimSpace(ad.LinkURL); link != "" {
				if !strings.HasPrefix(link, "http://") && !strings.HasPrefix(link, "https://") {
					writeJSONError(w, http.StatusBadRequest, "target link URL must begin with http:// or https://")
					return
				}
			}

			if err := app.db.SaveAd(r.Context(), ad); err != nil {
				writeJSONError(w, http.StatusInternalServerError, fmt.Sprintf("failed to save ad: %v", err))
				return
			}

			// Synchronize kiosks immediately via WebSocket
			if activeAds, err := app.db.GetActiveAds(r.Context()); err == nil && app.hub != nil {
				app.hub.BroadcastSyncAds(r.Context(), activeAds)
			}

			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "ad": ad})
			return
		}
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")

	case path == "reorder":
		if r.Method != http.MethodPost {
			writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		var req struct {
			Order []string `json:"order"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid request body")
			return
		}
		if err := app.db.ReorderAds(r.Context(), req.Order); err != nil {
			writeJSONError(w, http.StatusInternalServerError, fmt.Sprintf("failed to reorder ads: %v", err))
			return
		}

		if activeAds, err := app.db.GetActiveAds(r.Context()); err == nil && app.hub != nil {
			app.hub.BroadcastSyncAds(r.Context(), activeAds)
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})

	default:
		// Target specific ad ID: /api/admin/ads/{id}
		adID := path
		if r.Method == http.MethodPut {
			var ad contracts.AdItem
			if err := json.NewDecoder(r.Body).Decode(&ad); err != nil {
				writeJSONError(w, http.StatusBadRequest, "invalid request body")
				return
			}
			ad.ID = adID
			if strings.TrimSpace(ad.Title) == "" {
				writeJSONError(w, http.StatusBadRequest, "title is required")
				return
			}
			if link := strings.TrimSpace(ad.LinkURL); link != "" {
				if !strings.HasPrefix(link, "http://") && !strings.HasPrefix(link, "https://") {
					writeJSONError(w, http.StatusBadRequest, "target link URL must begin with http:// or https://")
					return
				}
			}
			if err := app.db.SaveAd(r.Context(), ad); err != nil {
				writeJSONError(w, http.StatusInternalServerError, fmt.Sprintf("failed to update ad: %v", err))
				return
			}

			if activeAds, err := app.db.GetActiveAds(r.Context()); err == nil && app.hub != nil {
				app.hub.BroadcastSyncAds(r.Context(), activeAds)
			}

			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "ad": ad})
			return
		} else if r.Method == http.MethodDelete {
			if err := app.db.DeleteAd(r.Context(), adID); err != nil {
				writeJSONError(w, http.StatusInternalServerError, fmt.Sprintf("failed to delete ad: %v", err))
				return
			}

			if activeAds, err := app.db.GetActiveAds(r.Context()); err == nil && app.hub != nil {
				app.hub.BroadcastSyncAds(r.Context(), activeAds)
			}

			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
			return
		}
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// handleAdminUploadImage handles POST /api/admin/upload-image
func (app *appServer) handleAdminUploadImage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	_, ok := app.authenticateAdminRequest(r)
	if !ok {
		writeJSONError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	// 5 MB max
	r.Body = http.MaxBytesReader(w, r.Body, 5*1024*1024)
	if err := r.ParseMultipartForm(5 * 1024 * 1024); err != nil {
		writeJSONError(w, http.StatusBadRequest, "file too large (max 5 MB)")
		return
	}

	file, header, err := r.FormFile("image")
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "missing 'image' file in form")
		return
	}
	defer file.Close()

	// Sniff magic bytes for valid image types
	headerBytes := make([]byte, 512)
	n, _ := file.Read(headerBytes)
	contentType := http.DetectContentType(headerBytes[:n])
	var ext string
	switch contentType {
	case "image/png":
		ext = ".png"
	case "image/jpeg":
		ext = ".jpg"
	case "image/webp":
		ext = ".webp"
	default:
		writeJSONError(w, http.StatusBadRequest, "unsupported image type (allowed: PNG, JPEG, WebP)")
		return
	}

	_, _ = file.Seek(0, io.SeekStart)

	b := make([]byte, 16)
	_, _ = rand.Read(b)
	filename := fmt.Sprintf("banner-%s%s", hex.EncodeToString(b), ext)

	uploadDir := filepath.Join(app.cfg.jobsDir, "ad_images")
	if err := os.MkdirAll(uploadDir, 0755); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "failed to create upload directory")
		return
	}

	destPath := filepath.Join(uploadDir, filename)
	destFile, err := os.Create(destPath)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "failed to save uploaded file")
		return
	}
	defer destFile.Close()

	if _, err := io.Copy(destFile, file); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "failed to write file content")
		return
	}

	imageURL := fmt.Sprintf("/api/ads/images/%s", filename)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"ok":        true,
		"image_url": imageURL,
		"filename":  header.Filename,
	})
}

// handleServeAdImage serves images uploaded at /api/ads/images/{filename}
func (app *appServer) handleServeAdImage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	filename := strings.TrimPrefix(r.URL.Path, "/api/ads/images/")
	filename = filepath.Base(filename)
	if filename == "" || filename == "." {
		http.NotFound(w, r)
		return
	}

	imagePath := filepath.Join(app.cfg.jobsDir, "ad_images", filename)
	if _, err := os.Stat(imagePath); err != nil {
		http.NotFound(w, r)
		return
	}

	w.Header().Set("Cache-Control", "public, max-age=86400, immutable")
	http.ServeFile(w, r, imagePath)
}


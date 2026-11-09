package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/skip2/go-qrcode"
	"xeroxgo/cloud-backend/internal/payments"
	"xeroxgo/cloud-backend/internal/store"
	"xeroxgo/pkg/contracts"
)

// InitiateQRRequest represents the kiosk request to create or retrieve a payment QR code.
type InitiateQRRequest struct {
	SessionID     string `json:"session_id"`
	PaymentMethod string `json:"payment_method,omitempty"`
}

// InitiateQRResponse is the response sent to the kiosk with the Razorpay UPI QR code or Payment Link details.
type InitiateQRResponse struct {
	AttemptID    string `json:"attempt_id"`
	SessionID    string `json:"session_id"`
	OrderID      string `json:"order_id"`
	RazorpayQRID string `json:"razorpay_qr_id"`
	QRImageURL   string `json:"qr_image_url"`
	PaymentLink  string `json:"payment_link,omitempty"`
	AmountPaise  int64  `json:"amount_paise"`
	Currency     string `json:"currency"`
	Status       string `json:"status"`
	ExpiresAt    int64  `json:"expires_at"`
}

// PaymentStatusResponse represents the current status of a payment session and its associated order.
type PaymentStatusResponse struct {
	SessionID     string `json:"session_id"`
	OrderID       string `json:"order_id"`
	OrderStatus   string `json:"order_status"`
	SessionStatus string `json:"session_status"`
	IsPaid        bool   `json:"is_paid"`
}

// handleKioskInitiateQR creates a single-use Razorpay UPI QR code bound to an active payment session.
// If an unexpired active payment attempt already exists for this session, it returns the existing QR code
// to prevent accidental duplicate payment attempts.
func (s *appServer) handleKioskInitiateQR(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// 1. Authenticate the kiosk via independent credentials
	kioskID, err := s.authenticateKiosk(w, r)
	if err != nil {
		return
	}

	// 2. Parse request body
	const maxBodySize = 64 * 1024 // 64 KiB
	r.Body = http.MaxBytesReader(w, r.Body, maxBodySize)
	var req InitiateQRRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error": "invalid request body: session_id is required",
		})
		return
	}

	sessionID := strings.TrimSpace(req.SessionID)
	if sessionID == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error": "session_id cannot be empty",
		})
		return
	}

	// Protect QR creation for the same payment session against concurrent requests using per-session in-memory lock.
	if s.sessionLocks != nil {
		unlock := s.sessionLocks.Lock(sessionID)
		defer unlock()
	}

	now := time.Now().UTC()

	// 3. Validate active payment session
	session, err := s.db.GetActivePaymentSession(r.Context(), sessionID, now)
	if err != nil || session == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error": "active payment session not found or expired",
		})
		return
	}

	// 4. Ensure payment session belongs to this authenticated kiosk
	if session.KioskID != kioskID {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error": "forbidden: payment session does not belong to this kiosk",
		})
		return
	}

	// 5. Validate associated order state
	order, err := s.db.GetOrder(r.Context(), session.OrderID)
	if err != nil || order == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error": "associated order not found",
		})
		return
	}

	if order.Status != contracts.JobStatusPendingPayment {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error": fmt.Sprintf("order cannot be paid in current status: %s", order.Status),
		})
		return
	}

	// 6. Check for existing active, unexpired attempt for this session (prevents duplicate attempts on retry)
	existingAttempt, err := s.db.GetActivePaymentAttemptBySession(r.Context(), session.SessionID, now)
	if err == nil && existingAttempt != nil && existingAttempt.RazorpayQRID != "" && existingAttempt.QRImageURL != "" {
		paymentLink := ""
		qrImgURL := existingAttempt.QRImageURL
		if existingAttempt.PaymentMethod == store.PaymentMethodRazorpayStandard {
			paymentLink = existingAttempt.QRImageURL
			baseURL := ""
			if r.Host != "" {
				scheme := "http"
				if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
					scheme = "https"
				}
				baseURL = fmt.Sprintf("%s://%s", scheme, r.Host)
			}
			qrImgURL = fmt.Sprintf("%s/api/kiosk/payment/qr.png?attempt_id=%s", baseURL, existingAttempt.ID)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(InitiateQRResponse{
			AttemptID:    existingAttempt.ID,
			SessionID:    existingAttempt.SessionID,
			OrderID:      existingAttempt.OrderID,
			RazorpayQRID: existingAttempt.RazorpayQRID,
			QRImageURL:   qrImgURL,
			PaymentLink:  paymentLink,
			AmountPaise:  existingAttempt.AmountPaise,
			Currency:     existingAttempt.Currency,
			Status:       string(existingAttempt.Status),
			ExpiresAt:    existingAttempt.ExpiresAt.Unix(),
		})
		return
	}

	// 7. Verify Razorpay client availability
	if s.razorpayClient == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error": "payment provider is not configured",
		})
		return
	}

	// 8. Compute payment link expiry timestamp (default 20 minutes, capped at session.ExpiresAt).
	// Razorpay requires expire_by to be at least 15 minutes in the future when received.
	// We require at least 16 minutes of remaining lifetime to guarantee validity over the wire.
	const normalExpiryDuration = 20 * time.Minute
	const minRemainingLifetime = 16 * time.Minute

	targetExpiry := now.Add(normalExpiryDuration)
	if targetExpiry.After(session.ExpiresAt) {
		targetExpiry = session.ExpiresAt
	}

	closeBy := targetExpiry.Unix()
	minExpireBy := now.Unix() + int64(minRemainingLifetime/time.Second)
	if closeBy < minExpireBy {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error": "payment session has insufficient remaining lifetime; tap 'Refresh PIN' on your order screen and enter the new PIN",
		})
		return
	}

	// 9. Determine payment method and call Razorpay provider
	// Standard Payment Link is the canonical default (enabling UPI apps, netbanking, cards, and wallets).
	// UPI QR can be explicitly selected via payment_method="upi_qr" or XEROXGO_PAYMENT_METHOD="upi_qr".
	useUPI := strings.EqualFold(strings.TrimSpace(req.PaymentMethod), "upi_qr") ||
		strings.EqualFold(strings.TrimSpace(os.Getenv("XEROXGO_PAYMENT_METHOD")), "upi_qr")

	attHex, err := randomHex(12)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error": "failed to generate attempt identifier",
		})
		return
	}
	attemptID := "att_" + attHex

	baseURL := ""
	if r.Host != "" {
		scheme := "http"
		if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
			scheme = "https"
		}
		baseURL = fmt.Sprintf("%s://%s", scheme, r.Host)
	}

	var attempt store.PaymentAttempt
	var respQRImageURL string
	var respPaymentLink string

	if useUPI {
		qrResp, err := s.razorpayClient.CreateUPIQRCode(r.Context(), payments.CreateQRCodeRequest{
			AmountPaise: order.PricePaise,
			Description: fmt.Sprintf("XeroxGo Order %s at %s", order.OrderID, kioskID),
			CloseByUnix: closeBy,
			Notes: map[string]string{
				"order_id":   order.OrderID,
				"kiosk_id":   kioskID,
				"session_id": session.SessionID,
			},
		})
		if err != nil {
			status, code, desc := sanitizeErrorForLog(err)
			if status > 0 || code != "" {
				log.Printf("[ERROR] Razorpay UPI QR creation failed for order %s: HTTP %d, code=%s, desc=%s",
					order.OrderID, status, code, desc)
			} else {
				log.Printf("[ERROR] Razorpay UPI QR creation failed for order %s: %s",
					order.OrderID, desc)
			}

			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"error": "failed to generate payment QR code from provider",
			})
			return
		}

		respQRImageURL = qrResp.ImageURL
		attempt = store.PaymentAttempt{
			ID:            attemptID,
			OrderID:       order.OrderID,
			SessionID:     session.SessionID,
			KioskID:       kioskID,
			PaymentMethod: store.PaymentMethodRazorpayUPIQR,
			RazorpayQRID:  qrResp.ID,
			QRImageURL:    qrResp.ImageURL,
			AmountPaise:   order.PricePaise,
			Currency:      "INR",
			Status:        store.PaymentAttemptCreated,
			CreatedAt:     now,
			ExpiresAt:     time.Unix(qrResp.CloseBy, 0).UTC(),
			UpdatedAt:     now,
		}
	} else {
		linkResp, err := s.razorpayClient.CreatePaymentLink(r.Context(), payments.CreatePaymentLinkRequest{
			AmountPaise: order.PricePaise,
			ReferenceID: fmt.Sprintf("XG-%s", order.OrderID),
			Description: fmt.Sprintf("XeroxGo Order %s at %s", order.OrderID, kioskID),
			ExpireBy:    closeBy,
			Notes: map[string]string{
				"order_id":   order.OrderID,
				"kiosk_id":   kioskID,
				"session_id": session.SessionID,
			},
		})
		if err != nil {
			status, code, desc := sanitizeErrorForLog(err)
			if status > 0 || code != "" {
				log.Printf("[ERROR] Razorpay payment link creation failed for order %s: HTTP %d, code=%s, desc=%s",
					order.OrderID, status, code, desc)
			} else {
				log.Printf("[ERROR] Razorpay payment link creation failed for order %s: %s",
					order.OrderID, desc)
			}

			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"error": "failed to generate payment link from provider",
			})
			return
		}

		respPaymentLink = linkResp.ShortURL
		respQRImageURL = fmt.Sprintf("%s/api/kiosk/payment/qr.png?attempt_id=%s", baseURL, attemptID)
		attempt = store.PaymentAttempt{
			ID:            attemptID,
			OrderID:       order.OrderID,
			SessionID:     session.SessionID,
			KioskID:       kioskID,
			PaymentMethod: store.PaymentMethodRazorpayStandard,
			RazorpayQRID:  linkResp.ID,
			QRImageURL:    linkResp.ShortURL,
			AmountPaise:   order.PricePaise,
			Currency:      "INR",
			Status:        store.PaymentAttemptCreated,
			CreatedAt:     now,
			ExpiresAt:     time.Unix(linkResp.ExpireBy, 0).UTC(),
			UpdatedAt:     now,
		}
	}

	if err := s.db.CreatePaymentAttempt(r.Context(), attempt); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error": "failed to persist payment attempt",
		})
		return
	}

	// 10. Return payment link and QR details to authenticated kiosk
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(InitiateQRResponse{
		AttemptID:    attempt.ID,
		SessionID:    attempt.SessionID,
		OrderID:      attempt.OrderID,
		RazorpayQRID: attempt.RazorpayQRID,
		QRImageURL:   respQRImageURL,
		PaymentLink:  respPaymentLink,
		AmountPaise:  attempt.AmountPaise,
		Currency:     attempt.Currency,
		Status:       string(attempt.Status),
		ExpiresAt:    attempt.ExpiresAt.Unix(),
	})
}

// handlePaymentQRImage generates and serves a QR code image (PNG) for a payment link or session.
// It authenticates the requesting kiosk, strictly verifies that the payment attempt or session
// belongs to this authenticated kiosk (preventing cross-kiosk access using guessed identifiers),
// and sets strict Cache-Control headers to prevent caching of sensitive payment QR codes.
func (s *appServer) handlePaymentQRImage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// 1. Authenticate the kiosk via independent credentials
	kioskID, err := s.authenticateKiosk(w, r)
	if err != nil {
		return
	}

	attemptID := strings.TrimSpace(r.URL.Query().Get("attempt_id"))
	sessionID := strings.TrimSpace(r.URL.Query().Get("session_id"))
	if attemptID == "" && sessionID == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error": "attempt_id or session_id query parameter is required",
		})
		return
	}

	var qrContent string

	if attemptID != "" {
		attempt, err := s.db.GetPaymentAttemptByID(r.Context(), attemptID)
		if err != nil || attempt == nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"error": "payment attempt not found",
			})
			return
		}

		// Prevent cross-kiosk access
		if attempt.KioskID != kioskID {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"error": "forbidden: payment attempt does not belong to this kiosk",
			})
			return
		}
		qrContent = attempt.QRImageURL
	} else if sessionID != "" {
		session, err := s.db.GetPaymentSession(r.Context(), sessionID)
		if err != nil || session == nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"error": "payment session not found",
			})
			return
		}

		// Prevent cross-kiosk access
		if session.KioskID != kioskID {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"error": "forbidden: payment session does not belong to this kiosk",
			})
			return
		}

		attempt, err := s.db.GetActivePaymentAttemptBySession(r.Context(), sessionID, time.Now().UTC())
		if err != nil || attempt == nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"error": "active payment attempt not found for session",
			})
			return
		}
		qrContent = attempt.QRImageURL
	}

	if qrContent == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error": "qr content not available for payment attempt",
		})
		return
	}

	pngBytes, err := qrcode.Encode(qrContent, qrcode.Medium, 400)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error": "failed to generate qr code image",
		})
		return
	}

	// Strictly private, non-cacheable headers for sensitive single-use payment credentials
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, private")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(pngBytes)
}

// handleKioskPaymentStatus reports payment and session state to the authenticated kiosk.
func (s *appServer) handleKioskPaymentStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	kioskID, err := s.authenticateKiosk(w, r)
	if err != nil {
		return
	}

	sessionID := strings.TrimSpace(r.URL.Query().Get("session_id"))
	if sessionID == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error": "session_id query parameter is required",
		})
		return
	}

	session, err := s.db.GetPaymentSession(r.Context(), sessionID)
	if err != nil || session == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error": "payment session not found",
		})
		return
	}

	if session.KioskID != kioskID {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error": "forbidden: payment session does not belong to this kiosk",
		})
		return
	}

	order, err := s.db.GetOrder(r.Context(), session.OrderID)
	if err != nil || order == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error": "associated order not found",
		})
		return
	}

	isPaid := order.Status == contracts.JobStatusPaidWaiting ||
		order.Status == contracts.JobStatusDispatched ||
		order.Status == contracts.JobStatusPrinting ||
		order.Status == contracts.JobStatusCompleted

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(PaymentStatusResponse{
		SessionID:     session.SessionID,
		OrderID:       order.OrderID,
		OrderStatus:   string(order.Status),
		SessionStatus: string(session.Status),
		IsPaid:        isPaid,
	})
}

// sanitizeErrorForLog extracts structured upstream error fields and sanitizes descriptions.
func sanitizeErrorForLog(err error) (status int, code string, desc string) {
	if err == nil {
		return 0, "", ""
	}
	var apiErr *payments.APIError
	if errors.As(err, &apiErr) {
		status = apiErr.HTTPStatus
		code = apiErr.Code
		desc = sanitizeLogString(apiErr.Description)
		return
	}
	desc = sanitizeLogString(err.Error())
	return
}

// sanitizeLogString removes control characters and newlines to prevent log injection,
// and bounds string length without logging sensitive credentials or tokens.
func sanitizeLogString(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= 32 && r <= 126 {
			b.WriteRune(r)
		} else if r == ' ' {
			b.WriteRune(' ')
		}
	}
	clean := strings.TrimSpace(b.String())
	if len(clean) > 256 {
		clean = clean[:256] + "..."
	}
	return clean
}

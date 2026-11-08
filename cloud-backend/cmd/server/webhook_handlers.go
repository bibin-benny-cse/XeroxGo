package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"xeroxgo/cloud-backend/internal/payments"
	"xeroxgo/cloud-backend/internal/store"
	"xeroxgo/pkg/contracts"
)

// razorpayWebhookEnvelope defines the JSON payload structure sent by Razorpay webhooks.
type razorpayWebhookEnvelope struct {
	Event   string `json:"event"`
	Payload struct {
		PaymentLink struct {
			Entity struct {
				ID          string `json:"id"`
				AmountPaise int64  `json:"amount"`
				AmountPaid  int64  `json:"amount_paid"`
				Currency    string `json:"currency"`
				Status      string `json:"status"`
				ShortURL    string `json:"short_url"`
				ReferenceID string `json:"reference_id"`
				Notes       struct {
					OrderID   string `json:"order_id"`
					SessionID string `json:"session_id"`
					KioskID   string `json:"kiosk_id"`
				} `json:"notes"`
			} `json:"entity"`
		} `json:"payment_link"`
		QRCode struct {
			Entity struct {
				ID          string `json:"id"`
				AmountPaise int64  `json:"payment_amount"`
				Status      string `json:"status"`
			} `json:"entity"`
		} `json:"qr_code"`
		Payment struct {
			Entity struct {
				ID       string `json:"id"`
				OrderID  string `json:"order_id"`
				Amount   int64  `json:"amount"`
				Currency string `json:"currency"`
				Status   string `json:"status"`
				Method   string `json:"method"`
				Notes    struct {
					OrderID   string `json:"order_id"`
					SessionID string `json:"session_id"`
					KioskID   string `json:"kiosk_id"`
				} `json:"notes"`
				ErrorCode        string `json:"error_code"`
				ErrorDescription string `json:"error_description"`
			} `json:"entity"`
		} `json:"payment"`
	} `json:"payload"`
}

// handleRazorpayWebhook handles inbound Razorpay webhook events.
// It verifies the HMAC-SHA256 signature using XEROXGO_WEBHOOK_SECRET, verifies currency,
// amount, status, and order association, ensures event idempotency via payment_webhook_events,
// serializes simultaneous deliveries using per-event locks, safely allows retry on failures,
// and atomically transitions verified orders strictly to PAID_WAITING.
// It NEVER authorizes printing directly and never logs raw payloads or PII.
func (s *appServer) handleRazorpayWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	const maxWebhookSize = 1024 * 1024 // 1 MiB limit
	r.Body = http.MaxBytesReader(w, r.Body, maxWebhookSize)
	rawBody, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "failed to read webhook body", http.StatusBadRequest)
		return
	}

	// 1. Webhook signature verification over exact raw request body
	sig := strings.TrimSpace(r.Header.Get("X-Razorpay-Signature"))
	if sig == "" || !payments.VerifyWebhookSignature(rawBody, sig, string(s.cfg.webhookSecret)) {
		http.Error(w, "invalid signature", http.StatusBadRequest)
		return
	}

	// 2. Require a valid Razorpay event ID header for deduplication
	eventID := strings.TrimSpace(r.Header.Get("X-Razorpay-Event-Id"))
	if eventID == "" {
		http.Error(w, "missing required X-Razorpay-Event-Id header", http.StatusBadRequest)
		return
	}

	// 3. Serialize simultaneous deliveries of the same event using in-memory lock
	if s.webhookLocks != nil {
		unlock := s.webhookLocks.Lock(eventID)
		defer unlock()
	}

	// 4. Parse webhook JSON payload
	var envelope razorpayWebhookEnvelope
	if err := json.Unmarshal(rawBody, &envelope); err != nil {
		http.Error(w, "malformed webhook json", http.StatusBadRequest)
		return
	}

	now := time.Now().UTC()
	hasher := sha256.New()
	hasher.Write(rawBody)
	payloadHash := hex.EncodeToString(hasher.Sum(nil))

	switch envelope.Event {
	case "payment_link.paid":
		plink := envelope.Payload.PaymentLink.Entity
		plinkID := strings.TrimSpace(plink.ID)
		payment := envelope.Payload.Payment.Entity
		paymentID := strings.TrimSpace(payment.ID)

		// Map Razorpay payment link ID (or notes) to local payment attempt
		var attempt *store.PaymentAttempt
		if plinkID != "" {
			attempt, _ = s.db.GetPaymentAttemptByQRID(r.Context(), plinkID)
		}
		if attempt == nil && plink.Notes.SessionID != "" {
			attempt, _ = s.db.GetActivePaymentAttemptBySession(r.Context(), plink.Notes.SessionID, now)
		}
		if attempt == nil && plink.Notes.OrderID != "" {
			attempt, _ = s.db.GetLatestPaymentAttempt(r.Context(), plink.Notes.OrderID)
		}

		orderID := ""
		if attempt != nil {
			orderID = attempt.OrderID
		} else if plink.Notes.OrderID != "" {
			orderID = plink.Notes.OrderID
		} else if payment.Notes.OrderID != "" {
			orderID = payment.Notes.OrderID
		}

		// Check idempotency and existing status
		existing, err := s.db.GetWebhookEvent(r.Context(), eventID)
		if err == nil && existing != nil {
			if existing.Status == store.WebhookEventProcessed {
				// True duplicate: already successfully processed
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"status":"duplicate"}`))
				return
			}
			// Previously failed or received: allow safe retry
			_ = s.db.UpdateWebhookEventStatus(r.Context(), eventID, store.WebhookEventReceived, now)
		} else {
			// Record event for first time
			_, _ = s.db.RecordWebhookEvent(r.Context(), eventID, envelope.Event, payloadHash, orderID)
		}

		// Validate required identifiers
		if plinkID == "" {
			_ = s.db.UpdateWebhookEventStatus(r.Context(), eventID, store.WebhookEventFailed, now)
			http.Error(w, "payment_link entity id missing", http.StatusBadRequest)
			return
		}
		if paymentID == "" {
			_ = s.db.UpdateWebhookEventStatus(r.Context(), eventID, store.WebhookEventFailed, now)
			http.Error(w, "payment entity id missing", http.StatusBadRequest)
			return
		}

		// Validate currency
		if plink.Currency != "INR" || (payment.Currency != "" && payment.Currency != "INR") {
			_ = s.db.UpdateWebhookEventStatus(r.Context(), eventID, store.WebhookEventFailed, now)
			http.Error(w, fmt.Sprintf("unsupported payment currency: %s", plink.Currency), http.StatusBadRequest)
			return
		}

		// Validate statuses: payment_link must be "paid", payment must be "captured"
		if plink.Status != "paid" {
			_ = s.db.UpdateWebhookEventStatus(r.Context(), eventID, store.WebhookEventFailed, now)
			http.Error(w, fmt.Sprintf("payment link status is not paid: %s", plink.Status), http.StatusBadRequest)
			return
		}
		if payment.Status != "captured" {
			_ = s.db.UpdateWebhookEventStatus(r.Context(), eventID, store.WebhookEventFailed, now)
			http.Error(w, fmt.Sprintf("payment status is not captured: %s", payment.Status), http.StatusBadRequest)
			return
		}

		// Ensure attempt exists
		if attempt == nil {
			_ = s.db.UpdateWebhookEventStatus(r.Context(), eventID, store.WebhookEventFailed, now)
			http.Error(w, fmt.Sprintf("payment attempt not found for payment link id: %s", plinkID), http.StatusNotFound)
			return
		}

		// Strictly validate persisted Razorpay Payment Link ID
		if attempt.RazorpayQRID != "" && attempt.RazorpayQRID != plinkID {
			_ = s.db.UpdateWebhookEventStatus(r.Context(), eventID, store.WebhookEventFailed, now)
			http.Error(w, fmt.Sprintf("razorpay payment link id mismatch: expected %s, got %s", attempt.RazorpayQRID, plinkID), http.StatusBadRequest)
			return
		}

		// Validate association with order and session in notes
		if plink.Notes.OrderID != "" && plink.Notes.OrderID != attempt.OrderID {
			_ = s.db.UpdateWebhookEventStatus(r.Context(), eventID, store.WebhookEventFailed, now)
			http.Error(w, fmt.Sprintf("order id mismatch: attempt has %s, payment link notes have %s", attempt.OrderID, plink.Notes.OrderID), http.StatusBadRequest)
			return
		}
		if plink.Notes.SessionID != "" && plink.Notes.SessionID != attempt.SessionID {
			_ = s.db.UpdateWebhookEventStatus(r.Context(), eventID, store.WebhookEventFailed, now)
			http.Error(w, fmt.Sprintf("session id mismatch: attempt has %s, payment link notes have %s", attempt.SessionID, plink.Notes.SessionID), http.StatusBadRequest)
			return
		}

		// Verify session exists and matches order
		session, err := s.db.GetPaymentSession(r.Context(), attempt.SessionID)
		if err != nil || session == nil {
			_ = s.db.UpdateWebhookEventStatus(r.Context(), eventID, store.WebhookEventFailed, now)
			http.Error(w, "associated payment session not found", http.StatusNotFound)
			return
		}
		if session.OrderID != attempt.OrderID {
			_ = s.db.UpdateWebhookEventStatus(r.Context(), eventID, store.WebhookEventFailed, now)
			http.Error(w, "session order association mismatch", http.StatusBadRequest)
			return
		}

		// Verify payment amount in paise matches attempt amount
		paidAmount := plink.AmountPaid
		if paidAmount == 0 {
			paidAmount = plink.AmountPaise
		}
		if paidAmount == 0 {
			paidAmount = payment.Amount
		}
		if paidAmount <= 0 || paidAmount != attempt.AmountPaise {
			_ = s.db.UpdateWebhookEventStatus(r.Context(), eventID, store.WebhookEventFailed, now)
			http.Error(w, fmt.Sprintf("payment amount mismatch: expected %d paise, got %d paise", attempt.AmountPaise, paidAmount), http.StatusBadRequest)
			return
		}

		// Verify order exists and price matches
		order, err := s.db.GetOrder(r.Context(), attempt.OrderID)
		if err != nil || order == nil {
			_ = s.db.UpdateWebhookEventStatus(r.Context(), eventID, store.WebhookEventFailed, now)
			http.Error(w, "associated order not found", http.StatusNotFound)
			return
		}

		// Prevent duplicate print dispatch: if order is already past PENDING_PAYMENT, do not re-process
		if order.Status != contracts.JobStatusPendingPayment {
			_ = s.db.UpdateWebhookEventStatus(r.Context(), eventID, store.WebhookEventProcessed, now)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"already_paid"}`))
			return
		}

		if paidAmount != order.PricePaise {
			_ = s.db.UpdateWebhookEventStatus(r.Context(), eventID, store.WebhookEventFailed, now)
			http.Error(w, fmt.Sprintf("order price mismatch: expected %d paise, got %d paise", order.PricePaise, paidAmount), http.StatusBadRequest)
			return
		}

		// Atomic database transaction:
		// Updates payment_attempts -> CAPTURED
		// Updates payment_sessions -> COMPLETED
		// Updates orders -> PAID_WAITING
		// Updates payment_webhook_events -> PROCESSED
		// Safe against partial failure.
		err = s.db.CompletePaymentTx(r.Context(), attempt.ID, attempt.SessionID, order.OrderID, paymentID, eventID, now)
		if err != nil {
			_ = s.db.UpdateWebhookEventStatus(r.Context(), eventID, store.WebhookEventFailed, now)
			http.Error(w, "failed to atomically complete payment transaction", http.StatusInternalServerError)
			return
		}

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
		return

	case "payment_link.expired":
		plink := envelope.Payload.PaymentLink.Entity
		plinkID := strings.TrimSpace(plink.ID)

		var attempt *store.PaymentAttempt
		if plinkID != "" {
			attempt, _ = s.db.GetPaymentAttemptByQRID(r.Context(), plinkID)
		}
		if attempt == nil && plink.Notes.SessionID != "" {
			attempt, _ = s.db.GetActivePaymentAttemptBySession(r.Context(), plink.Notes.SessionID, now)
		}
		if attempt == nil && plink.Notes.OrderID != "" {
			attempt, _ = s.db.GetLatestPaymentAttempt(r.Context(), plink.Notes.OrderID)
		}

		orderID := ""
		if attempt != nil {
			orderID = attempt.OrderID
		} else if plink.Notes.OrderID != "" {
			orderID = plink.Notes.OrderID
		}

		existing, err := s.db.GetWebhookEvent(r.Context(), eventID)
		if err == nil && existing != nil {
			if existing.Status == store.WebhookEventProcessed {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"status":"duplicate"}`))
				return
			}
		} else {
			_, _ = s.db.RecordWebhookEvent(r.Context(), eventID, envelope.Event, payloadHash, orderID)
		}

		if attempt != nil && attempt.Status == store.PaymentAttemptCreated {
			_ = s.db.UpdatePaymentAttemptStatus(r.Context(), attempt.ID, store.PaymentAttemptExpired, "", "payment link expired", now)
		}
		_ = s.db.UpdateWebhookEventStatus(r.Context(), eventID, store.WebhookEventProcessed, now)

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
		return

	case "payment.failed":
		payment := envelope.Payload.Payment.Entity
		paymentID := strings.TrimSpace(payment.ID)
		orderID := strings.TrimSpace(payment.Notes.OrderID)

		var attempt *store.PaymentAttempt
		if payment.Notes.SessionID != "" {
			attempt, _ = s.db.GetActivePaymentAttemptBySession(r.Context(), payment.Notes.SessionID, now)
		}
		if attempt == nil && orderID != "" {
			attempt, _ = s.db.GetLatestPaymentAttempt(r.Context(), orderID)
		}

		if orderID == "" && attempt != nil {
			orderID = attempt.OrderID
		}

		existing, err := s.db.GetWebhookEvent(r.Context(), eventID)
		if err == nil && existing != nil {
			if existing.Status == store.WebhookEventProcessed {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"status":"duplicate"}`))
				return
			}
		} else {
			_, _ = s.db.RecordWebhookEvent(r.Context(), eventID, envelope.Event, payloadHash, orderID)
		}

		reason := payment.ErrorDescription
		if reason == "" {
			reason = payment.ErrorCode
		}
		if reason == "" {
			reason = "payment failed"
		}

		if attempt != nil && attempt.Status == store.PaymentAttemptCreated {
			_ = s.db.UpdatePaymentAttemptStatus(r.Context(), attempt.ID, store.PaymentAttemptFailed, paymentID, reason, now)
		}
		_ = s.db.UpdateWebhookEventStatus(r.Context(), eventID, store.WebhookEventProcessed, now)

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
		return

	case "qr_code.credited":
		qrID := strings.TrimSpace(envelope.Payload.QRCode.Entity.ID)
		payment := envelope.Payload.Payment.Entity
		paymentID := strings.TrimSpace(payment.ID)

		// Map Razorpay QR ID to local payment attempt
		attempt, _ := s.db.GetPaymentAttemptByQRID(r.Context(), qrID)
		orderID := ""
		if attempt != nil {
			orderID = attempt.OrderID
		}

		// Check idempotency and existing status
		existing, err := s.db.GetWebhookEvent(r.Context(), eventID)
		if err == nil && existing != nil {
			if existing.Status == store.WebhookEventProcessed {
				// True duplicate: already successfully processed
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"status":"duplicate"}`))
				return
			}
			// Previously failed or received: allow safe retry
			_ = s.db.UpdateWebhookEventStatus(r.Context(), eventID, store.WebhookEventReceived, now)
		} else {
			// Record event for first time
			_, _ = s.db.RecordWebhookEvent(r.Context(), eventID, envelope.Event, payloadHash, orderID)
		}

		// Validate required identifiers
		if qrID == "" {
			_ = s.db.UpdateWebhookEventStatus(r.Context(), eventID, store.WebhookEventFailed, now)
			http.Error(w, "qr_code entity id missing", http.StatusBadRequest)
			return
		}
		if paymentID == "" {
			_ = s.db.UpdateWebhookEventStatus(r.Context(), eventID, store.WebhookEventFailed, now)
			http.Error(w, "payment entity id missing", http.StatusBadRequest)
			return
		}

		// Validate currency
		if payment.Currency != "INR" {
			_ = s.db.UpdateWebhookEventStatus(r.Context(), eventID, store.WebhookEventFailed, now)
			http.Error(w, fmt.Sprintf("unsupported payment currency: %s", payment.Currency), http.StatusBadRequest)
			return
		}

		// Validate payment status
		if payment.Status != "captured" {
			_ = s.db.UpdateWebhookEventStatus(r.Context(), eventID, store.WebhookEventFailed, now)
			http.Error(w, fmt.Sprintf("payment status is not captured: %s", payment.Status), http.StatusBadRequest)
			return
		}

		// Ensure attempt exists
		if attempt == nil {
			_ = s.db.UpdateWebhookEventStatus(r.Context(), eventID, store.WebhookEventFailed, now)
			http.Error(w, fmt.Sprintf("payment attempt not found for qr id: %s", qrID), http.StatusNotFound)
			return
		}

		// Verify payment amount equals attempt amount in paise
		if payment.Amount <= 0 || payment.Amount != attempt.AmountPaise {
			_ = s.db.UpdateWebhookEventStatus(r.Context(), eventID, store.WebhookEventFailed, now)
			http.Error(w, fmt.Sprintf("payment amount mismatch: expected %d paise, got %d paise", attempt.AmountPaise, payment.Amount), http.StatusBadRequest)
			return
		}

		// Verify order exists and price matches
		order, err := s.db.GetOrder(r.Context(), attempt.OrderID)
		if err != nil || order == nil {
			_ = s.db.UpdateWebhookEventStatus(r.Context(), eventID, store.WebhookEventFailed, now)
			http.Error(w, "associated order not found", http.StatusNotFound)
			return
		}

		if payment.Amount != order.PricePaise {
			_ = s.db.UpdateWebhookEventStatus(r.Context(), eventID, store.WebhookEventFailed, now)
			http.Error(w, fmt.Sprintf("order price mismatch: expected %d paise, got %d paise", order.PricePaise, payment.Amount), http.StatusBadRequest)
			return
		}

		// Atomic database transaction:
		// Updates payment_attempts -> CAPTURED
		// Updates payment_sessions -> COMPLETED
		// Updates orders -> PAID_WAITING
		// Updates payment_webhook_events -> PROCESSED
		// Safe against partial failure.
		err = s.db.CompletePaymentTx(r.Context(), attempt.ID, attempt.SessionID, order.OrderID, paymentID, eventID, now)
		if err != nil {
			_ = s.db.UpdateWebhookEventStatus(r.Context(), eventID, store.WebhookEventFailed, now)
			http.Error(w, "failed to atomically complete payment transaction", http.StatusInternalServerError)
			return
		}

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
		return

	case "payment.captured":
		payment := envelope.Payload.Payment.Entity
		paymentID := strings.TrimSpace(payment.ID)
		orderID := strings.TrimSpace(payment.Notes.OrderID)
		if orderID == "" {
			orderID = strings.TrimSpace(payment.OrderID)
		}
		if orderID == "" {
			http.Error(w, "order id missing in payment entity", http.StatusBadRequest)
			return
		}

		existing, err := s.db.GetWebhookEvent(r.Context(), eventID)
		if err == nil && existing != nil {
			if existing.Status == store.WebhookEventProcessed {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"status":"duplicate"}`))
				return
			}
			_ = s.db.UpdateWebhookEventStatus(r.Context(), eventID, store.WebhookEventReceived, now)
		} else {
			_, _ = s.db.RecordWebhookEvent(r.Context(), eventID, envelope.Event, payloadHash, orderID)
		}

		if paymentID == "" {
			_ = s.db.UpdateWebhookEventStatus(r.Context(), eventID, store.WebhookEventFailed, now)
			http.Error(w, "payment entity id missing", http.StatusBadRequest)
			return
		}

		if payment.Currency != "INR" {
			_ = s.db.UpdateWebhookEventStatus(r.Context(), eventID, store.WebhookEventFailed, now)
			http.Error(w, fmt.Sprintf("unsupported payment currency: %s", payment.Currency), http.StatusBadRequest)
			return
		}

		if payment.Status != "captured" {
			_ = s.db.UpdateWebhookEventStatus(r.Context(), eventID, store.WebhookEventFailed, now)
			http.Error(w, fmt.Sprintf("payment status is not captured: %s", payment.Status), http.StatusBadRequest)
			return
		}

		order, err := s.db.GetOrder(r.Context(), orderID)
		if err != nil || order == nil {
			_ = s.db.UpdateWebhookEventStatus(r.Context(), eventID, store.WebhookEventFailed, now)
			http.Error(w, "order not found", http.StatusNotFound)
			return
		}

		if payment.Amount <= 0 || payment.Amount != order.PricePaise {
			_ = s.db.UpdateWebhookEventStatus(r.Context(), eventID, store.WebhookEventFailed, now)
			http.Error(w, fmt.Sprintf("payment amount mismatch: expected %d paise, got %d paise", order.PricePaise, payment.Amount), http.StatusBadRequest)
			return
		}

		// Retrieve attempt if exists
		attempt, _ := s.db.GetLatestPaymentAttempt(r.Context(), orderID)
		attemptID := ""
		sessionID := ""
		if attempt != nil {
			attemptID = attempt.ID
			sessionID = attempt.SessionID
		}

		// Atomic database transaction
		err = s.db.CompletePaymentTx(r.Context(), attemptID, sessionID, order.OrderID, paymentID, eventID, now)
		if err != nil {
			_ = s.db.UpdateWebhookEventStatus(r.Context(), eventID, store.WebhookEventFailed, now)
			http.Error(w, "failed to atomically complete payment transaction", http.StatusInternalServerError)
			return
		}

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
		return

	default:
		// Other events recorded as IGNORED
		_, _ = s.db.RecordWebhookEvent(r.Context(), eventID, envelope.Event, payloadHash, "")
		_ = s.db.UpdateWebhookEventStatus(r.Context(), eventID, store.WebhookEventIgnored, now)

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ignored"}`))
		return
	}
}

package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"xeroxgo/cloud-backend/internal/pins"
	"xeroxgo/cloud-backend/internal/store"
	"xeroxgo/pkg/contracts"
)

// handleKioskRedeemPIN handles authenticated pickup PIN redemption submitted by a registered kiosk.
// It enforces progressive rate limiting, validates the PIN HMAC and order state, marks the PIN redeemed,
// and initializes an active payment session. It NEVER authorizes printing directly.
func (s *appServer) handleKioskRedeemPIN(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// 1. Authenticate the kiosk using independent credentials
	kioskID, err := s.authenticateKiosk(w, r)
	if err != nil {
		return
	}

	// 2. Check rate-limit cooldown
	clientIP := extractClientIP(r, s.cfg.trustedProxies)
	now := time.Now()
	allowed, retryAfter := s.pinLimiter.Check(kioskID, clientIP, now)
	if !allowed {
		secs := int(retryAfter.Seconds()) + 1
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", strconv.Itoa(secs))
		w.WriteHeader(http.StatusTooManyRequests)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"error":               "too many failed attempts, progressive cooldown active",
			"retry_after_seconds": secs,
		})
		return
	}

	// 3. Parse request body
	var req pins.PINRedemptionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.pinLimiter.RecordFailure(kioskID, clientIP, now)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error": "invalid request body",
		})
		return
	}

	cleanPIN := strings.TrimSpace(req.PIN)
	if len(cleanPIN) != 4 {
		s.pinLimiter.RecordFailure(kioskID, clientIP, now)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error": "invalid pickup PIN format: must be exactly 4 numeric digits",
		})
		return
	}
	for i := 0; i < 4; i++ {
		if cleanPIN[i] < '0' || cleanPIN[i] > '9' {
			s.pinLimiter.RecordFailure(kioskID, clientIP, now)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"error": "invalid pickup PIN format: must be numeric digits",
			})
			return
		}
	}

	// 4. Compute domain-separated HMAC-SHA256 for authenticated kiosk
	pinHMAC, err := pins.ComputePINHMAC(s.cfg.pickupPINHMACSecret, kioskID, cleanPIN)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error": "internal error processing PIN",
		})
		return
	}

	// 5. Generate cryptographically random payment session ID exclusively on the backend.
	// Any client-supplied SessionID in the request body is strictly ignored to prevent collisions or spoofing.
	sessHex, err := randomHex(16)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "failed to initialize payment session"})
		return
	}
	sessionID := "sess_" + sessHex

	sessionTTL := s.cfg.paymentSessionTTL
	if sessionTTL <= 0 {
		sessionTTL = 30 * time.Minute
	}

	// 6. Atomically redeem PIN and create payment session
	result, err := s.db.RedeemPickupPIN(r.Context(), kioskID, pinHMAC, now, sessionID, sessionTTL)
	if err != nil {
		// Progressive rate-limiting penalty on failure
		s.pinLimiter.RecordFailure(kioskID, clientIP, now)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		// Generic error message without revealing whether PIN or order exists
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error": "invalid or expired pickup PIN",
		})
		return
	}

	// 6. Reset rate-limiter failure counters on success
	s.pinLimiter.RecordSuccess(kioskID, clientIP)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(pins.PINRedemptionResponse{
		Status:           "ready_for_payment",
		SessionID:        result.SessionID,
		OrderID:          result.OrderID,
		KioskID:          result.KioskID,
		PricePaise:       result.PricePaise,
		TotalPages:       result.TotalPages,
		TotalSheets:      result.TotalSheets,
		Copies:           result.Copies,
		Duplex:           string(result.Duplex),
		ColorMode:        result.ColorMode,
		SessionExpiresAt: result.ExpiresAt.Unix(),
	})
}

// handleOrderRefreshPIN authorizes a customer to refresh their pickup PIN for an unpaid order
// using cryptographic proof of order ownership.
func (s *appServer) handleOrderRefreshPIN(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	prefix := "/api/orders/"
	subPath := strings.TrimPrefix(r.URL.Path, prefix)
	orderID := ""
	if idx := strings.Index(subPath, "/"); idx != -1 {
		orderID = subPath[:idx]
	}
	orderID = strings.TrimSpace(orderID)
	if orderID == "" {
		http.Error(w, "order id required", http.StatusBadRequest)
		return
	}

	var req pins.PINRefreshRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid request body"})
		return
	}

	secret := strings.TrimSpace(req.OrderSecret)
	if secret == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "order_secret is required"})
		return
	}

	// 1. Verify proof of order ownership
	ok, err := s.db.VerifyOrderOwnership(r.Context(), orderID, secret)
	if err != nil || !ok {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid order ownership credentials"})
		return
	}

	// 2. Fetch order to verify eligibility
	order, err := s.db.GetOrder(r.Context(), orderID)
	if err != nil || order == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "order not found"})
		return
	}

	if order.Status != contracts.JobStatusPendingPayment {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error": "order is not eligible for PIN refresh; status is " + string(order.Status),
		})
		return
	}

	// 3. Generate uniform PIN with retry on active collision
	const maxRetries = 5
	var (
		newPIN       string
		newExpiresAt time.Time
	)

	for attempt := 0; attempt < maxRetries; attempt++ {
		candidatePIN, err := pins.GenerateUniformPIN()
		if err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "failed to generate pickup PIN"})
			return
		}

		pinHMAC, err := pins.ComputePINHMAC(s.cfg.pickupPINHMACSecret, order.Command.KioskID, candidatePIN)
		if err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "failed to compute PIN HMAC"})
			return
		}

		pinHex, err := randomHex(12)
		if err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "failed to generate PIN ID"})
			return
		}

		now := time.Now().UTC()
		expiresAt := now.Add(15 * time.Minute)
		pinRecord := store.PickupPIN{
			ID:        "pin_" + pinHex,
			OrderID:   orderID,
			KioskID:   order.Command.KioskID,
			PINHMAC:   pinHMAC,
			Status:    store.PickupPINActive,
			CreatedAt: now,
			ExpiresAt: expiresAt,
		}

		err = s.db.RefreshOrderPickupPIN(r.Context(), orderID, secret, pinRecord, s.cfg.maxActivePINsPerKiosk)
		if errors.Is(err, store.ErrPINCollision) {
			continue // retry with a new uniform PIN
		}
		if errors.Is(err, store.ErrActivePINLimitExceeded) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "maximum active pickup PINs exceeded for machine, try again shortly"})
			return
		}
		if err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "failed to refresh pickup PIN"})
			return
		}

		newPIN = candidatePIN
		newExpiresAt = expiresAt
		break
	}

	if newPIN == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "unable to generate unique PIN after retries, please retry"})
		return
	}

	// 4. Return new PIN to authorized customer
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(pins.PINRefreshResponse{
		OrderID:             orderID,
		PickupPIN:           newPIN,
		PINExpiresAt:        newExpiresAt.Unix(),
		PINExpiresInSeconds: int64(time.Until(newExpiresAt).Seconds()),
		Status:              string(contracts.JobStatusPendingPayment),
	})
}

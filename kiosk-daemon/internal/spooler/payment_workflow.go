package spooler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"xeroxgo/kiosk-daemon/internal/ui_ipc"
)

// PINRedeemResponse is the successful response from POST /api/kiosk/pin/redeem.
type PINRedeemResponse struct {
	Status           string `json:"status"` // "ready_for_payment"
	SessionID        string `json:"session_id"`
	OrderID          string `json:"order_id"`
	KioskID          string `json:"kiosk_id"`
	PricePaise       int64  `json:"price_paise"`
	TotalPages       int    `json:"total_pages"`
	TotalSheets      int    `json:"total_sheets"`
	Copies           int    `json:"copies"`
	Duplex           string `json:"duplex"`
	ColorMode        string `json:"color_mode"`
	SessionExpiresAt int64  `json:"session_expires_at"`
}

// PaymentQRResponse is the response from POST /api/kiosk/payment/initiate-qr.
type PaymentQRResponse struct {
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

// PaymentStatusResponse is the response from GET /api/kiosk/payment/status.
type PaymentStatusResponse struct {
	SessionID     string `json:"session_id"`
	OrderID       string `json:"order_id"`
	OrderStatus   string `json:"order_status"`
	SessionStatus string `json:"session_status"`
	IsPaid        bool   `json:"is_paid"`
}

// PaymentWorkflow coordinates authenticated kiosk requests for PIN redemption,
// single-use Razorpay UPI QR initiation, polling, and payment retries.
type PaymentWorkflow struct {
	baseURL      string
	kioskID      string
	kioskSecret  string
	httpClient   *http.Client
	pollInterval time.Duration
}

// NewPaymentWorkflow initializes a new workflow client for communicating with the backend.
// It enforces that kiosk ID and kiosk secret are non-empty and validates the base URL.
func NewPaymentWorkflow(baseURL, kioskID, kioskSecret string, httpClient *http.Client, pollInterval time.Duration) (*PaymentWorkflow, error) {
	if strings.TrimSpace(baseURL) == "" {
		return nil, errors.New("backend base URL cannot be empty")
	}
	if strings.TrimSpace(kioskID) == "" {
		return nil, errors.New("kiosk ID cannot be empty")
	}
	if strings.TrimSpace(kioskSecret) == "" {
		return nil, errors.New("kiosk secret cannot be empty")
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	if pollInterval <= 0 {
		pollInterval = 2 * time.Second
	}

	trimmedURL := strings.TrimRight(baseURL, "/")
	return &PaymentWorkflow{
		baseURL:      trimmedURL,
		kioskID:      kioskID,
		kioskSecret:  kioskSecret,
		httpClient:   httpClient,
		pollInterval: pollInterval,
	}, nil
}

// RedeemPIN submits a 4-digit pickup PIN to POST /api/kiosk/pin/redeem using authenticated
// kiosk credentials. It strictly preserves leading zeros (e.g. "0482") and returns the active payment session.
func (w *PaymentWorkflow) RedeemPIN(ctx context.Context, pin string) (*PINRedeemResponse, error) {
	cleanPIN := strings.TrimSpace(pin)
	if len(cleanPIN) != 4 {
		return nil, errors.New("invalid pickup PIN format: must be exactly 4 numeric digits")
	}
	for i := 0; i < 4; i++ {
		if cleanPIN[i] < '0' || cleanPIN[i] > '9' {
			return nil, errors.New("invalid pickup PIN format: must be numeric digits")
		}
	}

	reqBody, err := json.Marshal(map[string]string{
		"pin": cleanPIN,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal pin request: %w", err)
	}

	reqURL := w.baseURL + "/api/kiosk/pin/redeem"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("create pin redemption request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Kiosk-ID", w.kioskID)
	req.Header.Set("X-Kiosk-Secret", w.kioskSecret)

	resp, err := w.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("execute pin redemption request: %w", err)
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return nil, fmt.Errorf("read pin redemption response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		var errResp struct {
			Error             string `json:"error"`
			RetryAfterSeconds int    `json:"retry_after_seconds,omitempty"`
		}
		if json.Unmarshal(bodyBytes, &errResp) == nil && errResp.Error != "" {
			if resp.StatusCode == http.StatusTooManyRequests && errResp.RetryAfterSeconds > 0 {
				return nil, fmt.Errorf("rate limited: %s (retry in %d seconds)", errResp.Error, errResp.RetryAfterSeconds)
			}
			return nil, errors.New(errResp.Error)
		}
		return nil, fmt.Errorf("pin redemption failed with HTTP %d", resp.StatusCode)
	}

	var result PINRedeemResponse
	if err := json.Unmarshal(bodyBytes, &result); err != nil {
		return nil, fmt.Errorf("decode pin redemption response: %w", err)
	}

	return &result, nil
}

// InitiatePaymentQR requests creation or retrieval of a Razorpay Test Mode UPI QR code
// for the given active payment session via POST /api/kiosk/payment/initiate-qr.
func (w *PaymentWorkflow) InitiatePaymentQR(ctx context.Context, sessionID string) (*PaymentQRResponse, error) {
	cleanSession := strings.TrimSpace(sessionID)
	if cleanSession == "" {
		return nil, errors.New("session_id cannot be empty")
	}

	reqBody, err := json.Marshal(map[string]string{
		"session_id": cleanSession,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal initiate-qr request: %w", err)
	}

	reqURL := w.baseURL + "/api/kiosk/payment/initiate-qr"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("create initiate-qr request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Kiosk-ID", w.kioskID)
	req.Header.Set("X-Kiosk-Secret", w.kioskSecret)

	resp, err := w.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("execute initiate-qr request: %w", err)
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return nil, fmt.Errorf("read initiate-qr response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		var errResp struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(bodyBytes, &errResp) == nil && errResp.Error != "" {
			return nil, errors.New(errResp.Error)
		}
		return nil, fmt.Errorf("payment initiation failed with HTTP %d", resp.StatusCode)
	}

	var result PaymentQRResponse
	if err := json.Unmarshal(bodyBytes, &result); err != nil {
		return nil, fmt.Errorf("decode initiate-qr response: %w", err)
	}

	return &result, nil
}

// FetchQRImage downloads the actual QR image bytes from the Razorpay image URL
// and decodes them into an in-memory image.Image for framebuffer rendering.
func (w *PaymentWorkflow) FetchQRImage(ctx context.Context, qrImageURL string) (image.Image, error) {
	cleanURL := strings.TrimSpace(qrImageURL)
	if cleanURL == "" {
		return nil, errors.New("qr image url cannot be empty")
	}

	if strings.HasPrefix(cleanURL, "/") {
		cleanURL = w.baseURL + cleanURL
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cleanURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create qr image request: %w", err)
	}

	req.Header.Set("X-Kiosk-ID", w.kioskID)
	req.Header.Set("X-Kiosk-Secret", w.kioskSecret)

	resp, err := w.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch qr image: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch qr image failed with HTTP %d", resp.StatusCode)
	}

	img, _, err := image.Decode(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("decode qr image: %w", err)
	}

	return img, nil
}

// GetPaymentStatus queries the payment session state from GET /api/kiosk/payment/status.
func (w *PaymentWorkflow) GetPaymentStatus(ctx context.Context, sessionID string) (*PaymentStatusResponse, error) {
	cleanSession := strings.TrimSpace(sessionID)
	if cleanSession == "" {
		return nil, errors.New("session_id cannot be empty")
	}

	reqURL := fmt.Sprintf("%s/api/kiosk/payment/status?session_id=%s", w.baseURL, url.QueryEscape(cleanSession))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create payment status request: %w", err)
	}

	req.Header.Set("X-Kiosk-ID", w.kioskID)
	req.Header.Set("X-Kiosk-Secret", w.kioskSecret)

	resp, err := w.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("execute payment status request: %w", err)
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return nil, fmt.Errorf("read payment status response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		var errResp struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(bodyBytes, &errResp) == nil && errResp.Error != "" {
			return nil, errors.New(errResp.Error)
		}
		return nil, fmt.Errorf("payment status check failed with HTTP %d", resp.StatusCode)
	}

	var result PaymentStatusResponse
	if err := json.Unmarshal(bodyBytes, &result); err != nil {
		return nil, fmt.Errorf("decode payment status response: %w", err)
	}

	return &result, nil
}

// PollPaymentStatus polls the backend status endpoint at regular intervals until
// payment is verified (is_paid == true), the session expires/fails, or the context is canceled.
func (w *PaymentWorkflow) PollPaymentStatus(
	ctx context.Context,
	sessionID string,
	onUpdate func(status *PaymentStatusResponse),
) (*PaymentStatusResponse, error) {
	// First immediate check
	initial, err := w.GetPaymentStatus(ctx, sessionID)
	if err == nil {
		if onUpdate != nil {
			onUpdate(initial)
		}
		if initial.IsPaid || initial.OrderStatus == "PAID_WAITING" {
			return initial, nil
		}
		if initial.SessionStatus == "expired" || initial.SessionStatus == "failed" {
			return initial, fmt.Errorf("payment session %s has ended: %s", sessionID, initial.SessionStatus)
		}
	}

	ticker := time.NewTicker(w.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
			status, err := w.GetPaymentStatus(ctx, sessionID)
			if err != nil {
				// Transient network error: continue polling until context expires
				continue
			}
			if onUpdate != nil {
				onUpdate(status)
			}
			if status.IsPaid || status.OrderStatus == "PAID_WAITING" {
				return status, nil
			}
			if status.SessionStatus == "expired" || status.SessionStatus == "failed" {
				return status, fmt.Errorf("payment session %s has ended: %s", sessionID, status.SessionStatus)
			}
		}
	}
}

// KioskWorkflowSession manages the complete customer-facing PIN redemption and payment UI lifecycle on the kiosk.
type KioskWorkflowSession struct {
	client     *Client
	workflow   *PaymentWorkflow
	sessionID  string
	orderID    string
	pricePaise int64
}

// NewKioskWorkflowSession creates a session bound to a spooler client.
func NewKioskWorkflowSession(client *Client) *KioskWorkflowSession {
	return &KioskWorkflowSession{
		client:   client,
		workflow: client.workflow,
	}
}

// ExecuteWorkflow handles PIN entry, payment QR display, status polling, and verified printing initiation.
func (s *KioskWorkflowSession) ExecuteWorkflow(ctx context.Context, pin string) error {
	if s.workflow == nil {
		return errors.New("payment workflow is not configured")
	}

	// 1. PIN Entry state
	_ = s.client.renderScreen(ui_ipc.ScreenState{
		Mode:    ui_ipc.ModePINEntry,
		PIN:     pin,
		Message: "Validating pickup PIN...",
	})

	// 2. Submit PIN redemption
	redeemResp, err := s.workflow.RedeemPIN(ctx, pin)
	if err != nil {
		_ = s.client.renderScreen(ui_ipc.ScreenState{
			Mode:     ui_ipc.ModePINEntry,
			PIN:      pin,
			PINError: err.Error(),
		})
		return err
	}

	s.sessionID = redeemResp.SessionID
	s.orderID = redeemResp.OrderID
	s.pricePaise = redeemResp.PricePaise

	// 3. Initiate Payment QR
	qrResp, err := s.workflow.InitiatePaymentQR(ctx, s.sessionID)
	if err != nil {
		_ = s.client.renderScreen(ui_ipc.ScreenState{
			Mode:            ui_ipc.ModePaymentFailed,
			OrderID:         s.orderID,
			AmountPaise:     s.pricePaise,
			PaymentError:    err.Error(),
			CanRetryPayment: true,
		})
		return err
	}

	// Try fetching actual Razorpay QR image if available
	var qrImg image.Image
	if qrResp.QRImageURL != "" {
		if fetched, fetchErr := s.workflow.FetchQRImage(ctx, qrResp.QRImageURL); fetchErr == nil {
			qrImg = fetched
		}
	}

	// Fallback: if image fetch failed or was not available, locally generate QR from PaymentLink or QRImageURL
	paymentURL := qrResp.PaymentLink
	if paymentURL == "" {
		paymentURL = qrResp.QRImageURL
	}
	if qrImg == nil && paymentURL != "" {
		if localQR, err := ui_ipc.GetQRCodeImage(paymentURL); err == nil {
			qrImg = localQR
		}
	}

	// 4. Render Payment QR Screen
	expiresAt := time.Unix(qrResp.ExpiresAt, 0).UTC()
	_ = s.client.renderScreen(ui_ipc.ScreenState{
		Mode:             ui_ipc.ModePaymentQR,
		OrderID:          s.orderID,
		AmountPaise:      qrResp.AmountPaise,
		PaymentQRURL:     paymentURL,
		PaymentQRImage:   qrImg,
		PaymentExpiresAt: expiresAt,
		Message:          "Scan with any UPI app to pay and start printing",
	})

	// 5. Poll payment status
	statusResp, err := s.workflow.PollPaymentStatus(ctx, s.sessionID, func(st *PaymentStatusResponse) {
		if st != nil && !st.IsPaid && st.SessionStatus == "active" {
			// Still waiting
			_ = s.client.renderScreen(ui_ipc.ScreenState{
				Mode:             ui_ipc.ModePaymentQR,
				OrderID:          s.orderID,
				AmountPaise:      qrResp.AmountPaise,
				PaymentQRURL:     paymentURL,
				PaymentQRImage:   qrImg,
				PaymentExpiresAt: expiresAt,
			})
		}
	})

	if err != nil {
		_ = s.client.renderScreen(ui_ipc.ScreenState{
			Mode:            ui_ipc.ModePaymentFailed,
			OrderID:         s.orderID,
			AmountPaise:     s.pricePaise,
			PaymentError:    err.Error(),
			CanRetryPayment: true, // Existing payment session can be retried without re-entering PIN
		})
		return err
	}

	// 6. Verified payment confirmed!
	if !statusResp.IsPaid && statusResp.OrderStatus != "PAID_WAITING" {
		return errors.New("cannot begin printing: payment is not verified")
	}

	// Decoupled verified printing begins ONLY after backend reports verified payment
	log.Printf("[WORKFLOW] Verified payment confirmed for order %s (session %s)", s.orderID, s.sessionID)
	return nil
}

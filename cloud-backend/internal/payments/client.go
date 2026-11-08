package payments

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// RazorpayClient defines the contract for interacting with Razorpay payment APIs.
type RazorpayClient interface {
	CreateUPIQRCode(ctx context.Context, req CreateQRCodeRequest) (*QRCodeResponse, error)
	CreatePaymentLink(ctx context.Context, req CreatePaymentLinkRequest) (*PaymentLinkResponse, error)
	FetchPayment(ctx context.Context, paymentID string) (*PaymentEntity, error)
}

// Client implements RazorpayClient using Go standard library net/http.
type Client struct {
	keyID      string
	keySecret  string
	baseURL    string
	httpClient *http.Client
}

// ClientOption provides functional options to configure a Client.
type ClientOption func(*Client)

// WithBaseURL configures a custom base URL (ideal for mock servers in tests).
func WithBaseURL(baseURL string) ClientOption {
	return func(c *Client) {
		c.baseURL = strings.TrimRight(baseURL, "/")
	}
}

// WithHTTPClient configures a custom http.Client.
func WithHTTPClient(client *http.Client) ClientOption {
	return func(c *Client) {
		if client != nil {
			c.httpClient = client
		}
	}
}

// CreateQRCodeRequest specifies the parameters to generate a single-use UPI QR code.
type CreateQRCodeRequest struct {
	AmountPaise int64             `json:"amount_paise"`
	Description string            `json:"description"`
	CloseByUnix int64             `json:"close_by"`
	Notes       map[string]string `json:"notes"`
}

// razorpayQRRequestPayload matches the official Razorpay QR code creation payload schema.
type razorpayQRRequestPayload struct {
	Type          string            `json:"type"`
	Name          string            `json:"name"`
	Usage         string            `json:"usage"`
	FixedAmount   bool              `json:"fixed_amount"`
	PaymentAmount int64             `json:"payment_amount"`
	Description   string            `json:"description,omitempty"`
	CloseBy       int64             `json:"close_by,omitempty"`
	Notes         map[string]string `json:"notes,omitempty"`
}

// QRCodeResponse holds the response fields returned by Razorpay for a QR code.
type QRCodeResponse struct {
	ID          string            `json:"id"`
	Entity      string            `json:"entity"`
	CreatedAt   int64             `json:"created_at"`
	Name        string            `json:"name"`
	Usage       string            `json:"usage"`
	Type        string            `json:"type"`
	ImageURL    string            `json:"image_url"`
	AmountPaise int64             `json:"payment_amount"`
	Status      string            `json:"status"`
	Description string            `json:"description"`
	FixedAmount bool              `json:"fixed_amount"`
	CloseBy     int64             `json:"close_by"`
	Notes       map[string]string `json:"notes"`
}

// CreatePaymentLinkRequest specifies parameters to generate a Standard Payment Link.
type CreatePaymentLinkRequest struct {
	AmountPaise int64             `json:"amount_paise"`
	Description string            `json:"description,omitempty"`
	ReferenceID string            `json:"reference_id,omitempty"`
	ExpireBy    int64             `json:"expire_by,omitempty"`
	Notes       map[string]string `json:"notes,omitempty"`
}

// razorpayPaymentLinkRequestPayload matches the official Razorpay Standard Payment Link creation payload.
type razorpayPaymentLinkRequestPayload struct {
	Amount         int64             `json:"amount"`
	Currency       string            `json:"currency"`
	AcceptPartial  bool              `json:"accept_partial"`
	ReferenceID    string            `json:"reference_id,omitempty"`
	Description    string            `json:"description,omitempty"`
	ExpireBy       int64             `json:"expire_by,omitempty"`
	Notify         map[string]bool   `json:"notify"`
	ReminderEnable bool              `json:"reminder_enable"`
	Notes          map[string]string `json:"notes,omitempty"`
}

// PaymentLinkResponse holds response fields returned by Razorpay for a Standard Payment Link.
type PaymentLinkResponse struct {
	ID          string            `json:"id"`
	Entity      string            `json:"entity"`
	AmountPaise int64             `json:"amount"`
	AmountPaid  int64             `json:"amount_paid"`
	Currency    string            `json:"currency"`
	Status      string            `json:"status"`
	ShortURL    string            `json:"short_url"`
	ReferenceID string            `json:"reference_id"`
	Description string            `json:"description"`
	ExpireBy    int64             `json:"expire_by"`
	Notes       map[string]string `json:"notes"`
}

// PaymentEntity represents a Razorpay payment object.
type PaymentEntity struct {
	ID          string            `json:"id"`
	Entity      string            `json:"entity"`
	Amount      int64             `json:"amount"`
	Currency    string            `json:"currency"`
	Status      string            `json:"status"`
	OrderID     string            `json:"order_id"`
	Method      string            `json:"method"`
	Description string            `json:"description"`
	Notes       map[string]string `json:"notes"`
	CreatedAt   int64             `json:"created_at"`
}

// razorpayErrorEnvelope represents Razorpay's structured error JSON.
type razorpayErrorEnvelope struct {
	Error struct {
		Code        string `json:"code"`
		Description string `json:"description"`
		Source      string `json:"source"`
		Step        string `json:"step"`
		Reason      string `json:"reason"`
	} `json:"error"`
}

// NewClient initializes a Razorpay client strictly enforced in Test Mode.
// It mandates non-empty credentials and strictly requires key ID to start with "rzp_test_".
// Every other prefix (including live keys "rzp_live_") is rejected.
func NewClient(keyID, keySecret string, timeout time.Duration, opts ...ClientOption) (*Client, error) {
	keyID = strings.TrimSpace(keyID)
	keySecret = strings.TrimSpace(keySecret)

	if keyID == "" {
		return nil, errors.New("razorpay key ID cannot be empty")
	}
	if keySecret == "" {
		return nil, errors.New("razorpay key secret cannot be empty")
	}

	// Strictly enforce Test Mode: require rzp_test_ prefix. Reject every other prefix.
	if !strings.HasPrefix(keyID, "rzp_test_") {
		return nil, errors.New("test mode requires RAZORPAY_KEY_ID to start with rzp_test_ (live and non-test keys are strictly rejected)")
	}

	if timeout <= 0 {
		timeout = 10 * time.Second
	}

	client := &Client{
		keyID:     keyID,
		keySecret: keySecret,
		baseURL:   "https://api.razorpay.com",
		httpClient: &http.Client{
			Timeout: timeout,
		},
	}

	for _, opt := range opts {
		opt(client)
	}

	return client, nil
}

// CreateUPIQRCode creates a fixed-amount, single-use dynamic UPI QR code on Razorpay.
// It authenticates via HTTP Basic Auth and maps response fields safely without leaking secrets.
func (c *Client) CreateUPIQRCode(ctx context.Context, req CreateQRCodeRequest) (*QRCodeResponse, error) {
	if req.AmountPaise <= 0 {
		return nil, fmt.Errorf("create qr code: amount must be positive, got %d", req.AmountPaise)
	}

	payload := razorpayQRRequestPayload{
		Type:          "upi_qr",
		Name:          "XeroxGo Print",
		Usage:         "single_use",
		FixedAmount:   true,
		PaymentAmount: req.AmountPaise,
		Description:   req.Description,
		CloseBy:       req.CloseByUnix,
		Notes:         req.Notes,
	}

	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("create qr code: failed to serialize request: %w", err)
	}

	endpoint := c.baseURL + "/v1/payments/qr_codes"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("create qr code: failed to build http request: %w", err)
	}

	httpReq.SetBasicAuth(c.keyID, c.keySecret)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("create qr code: http request failed: %w", err)
	}
	defer resp.Body.Close()

	const maxRespSize = 1024 * 1024 // 1 MiB limit
	respBytes, err := io.ReadAll(io.LimitReader(resp.Body, maxRespSize))
	if err != nil {
		return nil, fmt.Errorf("create qr code: failed to read response body: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var errEnv razorpayErrorEnvelope
		if jsonErr := json.Unmarshal(respBytes, &errEnv); jsonErr == nil && errEnv.Error.Code != "" {
			return nil, fmt.Errorf("razorpay api error (status %d, code %s): %s", resp.StatusCode, errEnv.Error.Code, errEnv.Error.Description)
		}
		return nil, fmt.Errorf("razorpay api error: HTTP %d", resp.StatusCode)
	}

	var qrResp QRCodeResponse
	if err := json.Unmarshal(respBytes, &qrResp); err != nil {
		return nil, fmt.Errorf("create qr code: failed to parse json response: %w", err)
	}

	if qrResp.ID == "" || qrResp.ImageURL == "" {
		return nil, errors.New("create qr code: razorpay response missing id or image_url")
	}

	return &qrResp, nil
}

// CreatePaymentLink generates a single-use Razorpay Standard Payment Link.
// It sends the exact order amount in paise, currency INR, reference ID, and notes.
// It maps the response safely and returns the payment link ID and short URL.
func (c *Client) CreatePaymentLink(ctx context.Context, req CreatePaymentLinkRequest) (*PaymentLinkResponse, error) {
	if req.AmountPaise <= 0 {
		return nil, fmt.Errorf("create payment link: amount must be positive, got %d", req.AmountPaise)
	}

	payload := razorpayPaymentLinkRequestPayload{
		Amount:         req.AmountPaise,
		Currency:       "INR",
		AcceptPartial:  false,
		ReferenceID:    req.ReferenceID,
		Description:    req.Description,
		ExpireBy:       req.ExpireBy,
		Notify:         map[string]bool{"sms": false, "email": false},
		ReminderEnable: false,
		Notes:          req.Notes,
	}

	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("create payment link: failed to serialize request: %w", err)
	}

	endpoint := c.baseURL + "/v1/payment_links"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("create payment link: failed to build http request: %w", err)
	}

	httpReq.SetBasicAuth(c.keyID, c.keySecret)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("create payment link: http request failed: %w", err)
	}
	defer resp.Body.Close()

	const maxRespSize = 1024 * 1024 // 1 MiB limit
	respBytes, err := io.ReadAll(io.LimitReader(resp.Body, maxRespSize))
	if err != nil {
		return nil, fmt.Errorf("create payment link: failed to read response body: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var errEnv razorpayErrorEnvelope
		if jsonErr := json.Unmarshal(respBytes, &errEnv); jsonErr == nil && errEnv.Error.Code != "" {
			return nil, fmt.Errorf("razorpay api error (status %d, code %s): %s", resp.StatusCode, errEnv.Error.Code, errEnv.Error.Description)
		}
		return nil, fmt.Errorf("razorpay api error: HTTP %d", resp.StatusCode)
	}

	var linkResp PaymentLinkResponse
	if err := json.Unmarshal(respBytes, &linkResp); err != nil {
		return nil, fmt.Errorf("create payment link: failed to parse json response: %w", err)
	}

	if linkResp.ID == "" || linkResp.ShortURL == "" {
		return nil, errors.New("create payment link: razorpay response missing id or short_url")
	}

	return &linkResp, nil
}

// FetchPayment retrieves details of a payment by its Razorpay payment ID.
func (c *Client) FetchPayment(ctx context.Context, paymentID string) (*PaymentEntity, error) {
	paymentID = strings.TrimSpace(paymentID)
	if paymentID == "" {
		return nil, errors.New("fetch payment: paymentID cannot be empty")
	}

	endpoint := c.baseURL + "/v1/payments/" + paymentID
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("fetch payment: failed to build http request: %w", err)
	}

	httpReq.SetBasicAuth(c.keyID, c.keySecret)
	httpReq.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("fetch payment: http request failed: %w", err)
	}
	defer resp.Body.Close()

	const maxRespSize = 1024 * 1024
	respBytes, err := io.ReadAll(io.LimitReader(resp.Body, maxRespSize))
	if err != nil {
		return nil, fmt.Errorf("fetch payment: failed to read response body: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var errEnv razorpayErrorEnvelope
		if jsonErr := json.Unmarshal(respBytes, &errEnv); jsonErr == nil && errEnv.Error.Code != "" {
			return nil, fmt.Errorf("razorpay api error (status %d, code %s): %s", resp.StatusCode, errEnv.Error.Code, errEnv.Error.Description)
		}
		return nil, fmt.Errorf("razorpay api error: HTTP %d", resp.StatusCode)
	}

	var payment PaymentEntity
	if err := json.Unmarshal(respBytes, &payment); err != nil {
		return nil, fmt.Errorf("fetch payment: failed to parse json response: %w", err)
	}

	return &payment, nil
}

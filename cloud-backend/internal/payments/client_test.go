package payments

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func newMockHTTPClient(fn roundTripFunc) *http.Client {
	return &http.Client{
		Transport: fn,
		Timeout:   5 * time.Second,
	}
}

func TestNewClient_Validations(t *testing.T) {
	tests := []struct {
		name      string
		keyID     string
		keySecret string
		wantErr   string
	}{
		{
			name:      "empty key id",
			keyID:     "",
			keySecret: "secret123",
			wantErr:   "key ID cannot be empty",
		},
		{
			name:      "empty key secret",
			keyID:     "rzp_test_123",
			keySecret: "",
			wantErr:   "key secret cannot be empty",
		},
		{
			name:      "live key rejected",
			keyID:     "rzp_live_abc123456789",
			keySecret: "liveSecret123456",
			wantErr:   "test mode requires RAZORPAY_KEY_ID to start with rzp_test_",
		},
		{
			name:      "non test prefix rejected",
			keyID:     "rzp_beta_1234567890",
			keySecret: "someSecret123456",
			wantErr:   "test mode requires RAZORPAY_KEY_ID to start with rzp_test_",
		},
		{
			name:      "arbitrary key rejected",
			keyID:     "my_custom_key_123",
			keySecret: "someSecret123456",
			wantErr:   "test mode requires RAZORPAY_KEY_ID to start with rzp_test_",
		},
		{
			name:      "valid test key accepted",
			keyID:     "rzp_test_abc123456789",
			keySecret: "testSecret123456",
			wantErr:   "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client, err := NewClient(tt.keyID, tt.keySecret, 5*time.Second)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("expected error containing %q, got %v", tt.wantErr, err)
				}
				if client != nil {
					t.Fatalf("expected nil client on error")
				}
			} else {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if client == nil {
					t.Fatalf("expected non-nil client")
				}
			}
		})
	}
}

func TestCreateUPIQRCode_Success(t *testing.T) {
	testKeyID := "rzp_test_key123"
	testKeySecret := "secret456"

	mockClient := newMockHTTPClient(func(r *http.Request) (*http.Response, error) {
		// 1. Verify URL and Method
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if !strings.HasSuffix(r.URL.Path, "/v1/payments/qr_codes") {
			t.Errorf("expected /v1/payments/qr_codes, got %s", r.URL.Path)
		}

		// 2. Verify Basic Auth
		authHeader := r.Header.Get("Authorization")
		expectedAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte(testKeyID+":"+testKeySecret))
		if authHeader != expectedAuth {
			t.Errorf("expected auth header %q, got %q", expectedAuth, authHeader)
		}

		// 3. Verify Request Payload
		var payload razorpayQRRequestPayload
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("failed to decode request body: %v", err)
		}

		if payload.Type != "upi_qr" {
			t.Errorf("expected type upi_qr, got %s", payload.Type)
		}
		if payload.Usage != "single_use" {
			t.Errorf("expected usage single_use, got %s", payload.Usage)
		}
		if !payload.FixedAmount {
			t.Errorf("expected fixed_amount true")
		}
		if payload.PaymentAmount != 400 {
			t.Errorf("expected payment_amount 400, got %d", payload.PaymentAmount)
		}
		if payload.Notes["order_id"] != "ord_test_1" {
			t.Errorf("expected notes.order_id 'ord_test_1', got %s", payload.Notes["order_id"])
		}

		// 4. Return Razorpay QR Code Response
		respObj := QRCodeResponse{
			ID:          "qr_HMsVL8HOpbMcjU",
			Entity:      "qr_code",
			CreatedAt:   1741400000,
			Name:        "XeroxGo Print",
			Usage:       "single_use",
			Type:        "upi_qr",
			ImageURL:    "https://rzp.io/i/BWcUVrLp",
			AmountPaise: 400,
			Status:      "active",
			Description: payload.Description,
			FixedAmount: true,
			CloseBy:     1741400900,
			Notes:       payload.Notes,
		}

		respBytes, _ := json.Marshal(respObj)
		return &http.Response{
			StatusCode: http.StatusCreated,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(bytes.NewReader(respBytes)),
		}, nil
	})

	client, err := NewClient(testKeyID, testKeySecret, 5*time.Second, WithHTTPClient(mockClient))
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}

	ctx := context.Background()
	qrResp, err := client.CreateUPIQRCode(ctx, CreateQRCodeRequest{
		AmountPaise: 400,
		Description: "Order ord_test_1 at kiosk-demo",
		CloseByUnix: 1741400900,
		Notes: map[string]string{
			"order_id": "ord_test_1",
			"kiosk_id": "kiosk-demo",
		},
	})
	if err != nil {
		t.Fatalf("CreateUPIQRCode failed: %v", err)
	}

	if qrResp.ID != "qr_HMsVL8HOpbMcjU" {
		t.Errorf("expected ID 'qr_HMsVL8HOpbMcjU', got %s", qrResp.ID)
	}
	if qrResp.ImageURL != "https://rzp.io/i/BWcUVrLp" {
		t.Errorf("expected ImageURL 'https://rzp.io/i/BWcUVrLp', got %s", qrResp.ImageURL)
	}
	if qrResp.Status != "active" {
		t.Errorf("expected status 'active', got %s", qrResp.Status)
	}
}

func TestCreateUPIQRCode_APIErrors(t *testing.T) {
	testKeyID := "rzp_test_key123"
	testKeySecret := "secret456"

	mockClient := newMockHTTPClient(func(r *http.Request) (*http.Response, error) {
		errResp := map[string]interface{}{
			"error": map[string]string{
				"code":        "BAD_REQUEST_ERROR",
				"description": "The close by timestamp cannot be in the past",
			},
		}
		respBytes, _ := json.Marshal(errResp)
		return &http.Response{
			StatusCode: http.StatusBadRequest,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(bytes.NewReader(respBytes)),
		}, nil
	})

	client, err := NewClient(testKeyID, testKeySecret, 5*time.Second, WithHTTPClient(mockClient))
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}

	_, err = client.CreateUPIQRCode(context.Background(), CreateQRCodeRequest{
		AmountPaise: 400,
		Description: "Test error",
		CloseByUnix: 100,
	})
	if err == nil {
		t.Fatalf("expected error from API, got nil")
	}

	if !strings.Contains(err.Error(), "BAD_REQUEST_ERROR") || !strings.Contains(err.Error(), "The close by timestamp cannot be in the past") {
		t.Fatalf("unexpected error message: %v", err)
	}

	// Ensure secret is NEVER in the error string
	if strings.Contains(err.Error(), testKeySecret) {
		t.Fatalf("CRITICAL SECURITY: API secret leaked in error string: %v", err)
	}
}

func TestCreateUPIQRCode_InvalidAmount(t *testing.T) {
	client, err := NewClient("rzp_test_123", "secret", 5*time.Second)
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}

	_, err = client.CreateUPIQRCode(context.Background(), CreateQRCodeRequest{
		AmountPaise: 0,
	})
	if err == nil || !strings.Contains(err.Error(), "amount must be positive") {
		t.Fatalf("expected positive amount validation error, got: %v", err)
	}

	_, err = client.CreateUPIQRCode(context.Background(), CreateQRCodeRequest{
		AmountPaise: -100,
	})
	if err == nil || !strings.Contains(err.Error(), "amount must be positive") {
		t.Fatalf("expected positive amount validation error, got: %v", err)
	}
}

func TestFetchPayment_Success(t *testing.T) {
	testKeyID := "rzp_test_key123"
	testKeySecret := "secret456"

	mockClient := newMockHTTPClient(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet {
			t.Errorf("expected GET, got %s", r.Method)
		}
		if !strings.HasSuffix(r.URL.Path, "/v1/payments/pay_test_001") {
			t.Errorf("expected path ending in /v1/payments/pay_test_001, got %s", r.URL.Path)
		}

		resp := PaymentEntity{
			ID:          "pay_test_001",
			Entity:      "payment",
			Amount:      400,
			Currency:    "INR",
			Status:      "captured",
			OrderID:     "order_razor_123",
			Method:      "upi",
			Description: "Print job payment",
		}

		respBytes, _ := json.Marshal(resp)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(bytes.NewReader(respBytes)),
		}, nil
	})

	client, err := NewClient(testKeyID, testKeySecret, 5*time.Second, WithHTTPClient(mockClient))
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}

	payment, err := client.FetchPayment(context.Background(), "pay_test_001")
	if err != nil {
		t.Fatalf("FetchPayment failed: %v", err)
	}

	if payment.ID != "pay_test_001" || payment.Amount != 400 || payment.Status != "captured" || payment.Currency != "INR" {
		t.Fatalf("unexpected payment entity: %+v", payment)
	}
}

func TestCreatePaymentLink_Success(t *testing.T) {
	testKeyID := "rzp_test_key123"
	testKeySecret := "secret456"

	mockClient := newMockHTTPClient(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if !strings.HasSuffix(r.URL.Path, "/v1/payment_links") {
			t.Errorf("expected path ending in /v1/payment_links, got %s", r.URL.Path)
		}

		user, pass, ok := r.BasicAuth()
		if !ok || user != testKeyID || pass != testKeySecret {
			t.Errorf("unexpected basic auth credentials: user=%s, pass=%s", user, pass)
		}

		bodyBytes, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("failed to read body: %v", err)
		}
		var payload map[string]interface{}
		if err := json.Unmarshal(bodyBytes, &payload); err != nil {
			t.Fatalf("failed to unmarshal request json: %v", err)
		}

		if amount, ok := payload["amount"].(float64); !ok || int64(amount) != 300 {
			t.Errorf("expected amount 300, got %v", payload["amount"])
		}
		if currency, ok := payload["currency"].(string); !ok || currency != "INR" {
			t.Errorf("expected currency INR, got %v", payload["currency"])
		}
		if refID, ok := payload["reference_id"].(string); !ok || refID != "XG-TEST-001" {
			t.Errorf("expected reference_id XG-TEST-001, got %v", payload["reference_id"])
		}

		resp := PaymentLinkResponse{
			ID:          "plink_TxnrgB8J7cbi3X",
			Entity:      "payment_link",
			AmountPaise: 300,
			Currency:    "INR",
			Status:      "created",
			ShortURL:    "https://rzp.io/i/testlink001",
			ReferenceID: "XG-TEST-001",
			Description: "XeroxGo Print Order test",
			ExpireBy:    1731110000,
			Notes: map[string]string{
				"order_id": "ord_123",
				"kiosk_id": "kiosk_demo",
			},
		}

		respBytes, _ := json.Marshal(resp)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(bytes.NewReader(respBytes)),
		}, nil
	})

	client, err := NewClient(testKeyID, testKeySecret, 5*time.Second, WithHTTPClient(mockClient))
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}

	linkResp, err := client.CreatePaymentLink(context.Background(), CreatePaymentLinkRequest{
		AmountPaise: 300,
		ReferenceID: "XG-TEST-001",
		Description: "XeroxGo Print Order test",
		ExpireBy:    1731110000,
		Notes: map[string]string{
			"order_id": "ord_123",
			"kiosk_id": "kiosk_demo",
		},
	})
	if err != nil {
		t.Fatalf("CreatePaymentLink failed: %v", err)
	}

	if linkResp.ID != "plink_TxnrgB8J7cbi3X" {
		t.Errorf("expected ID plink_TxnrgB8J7cbi3X, got %s", linkResp.ID)
	}
	if linkResp.ShortURL != "https://rzp.io/i/testlink001" {
		t.Errorf("expected ShortURL https://rzp.io/i/testlink001, got %s", linkResp.ShortURL)
	}
	if linkResp.AmountPaise != 300 {
		t.Errorf("expected AmountPaise 300, got %d", linkResp.AmountPaise)
	}
	if linkResp.Currency != "INR" {
		t.Errorf("expected Currency INR, got %s", linkResp.Currency)
	}
}

func TestCreatePaymentLink_ValidationAndError(t *testing.T) {
	testKeyID := "rzp_test_key123"
	testKeySecret := "secret456"

	client, err := NewClient(testKeyID, testKeySecret, 5*time.Second)
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}

	_, err = client.CreatePaymentLink(context.Background(), CreatePaymentLinkRequest{
		AmountPaise: 0,
	})
	if err == nil || !strings.Contains(err.Error(), "amount must be positive") {
		t.Fatalf("expected positive amount validation error, got: %v", err)
	}

	_, err = client.CreatePaymentLink(context.Background(), CreatePaymentLinkRequest{
		AmountPaise: -50,
	})
	if err == nil || !strings.Contains(err.Error(), "amount must be positive") {
		t.Fatalf("expected positive amount validation error, got: %v", err)
	}

	// Provider error handling
	mockErrClient := newMockHTTPClient(func(r *http.Request) (*http.Response, error) {
		errResp := razorpayErrorEnvelope{}
		errResp.Error.Code = "BAD_REQUEST_ERROR"
		errResp.Error.Description = "Reference id already exists"
		respBytes, _ := json.Marshal(errResp)
		return &http.Response{
			StatusCode: http.StatusBadRequest,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(bytes.NewReader(respBytes)),
		}, nil
	})

	errClient, err := NewClient(testKeyID, testKeySecret, 5*time.Second, WithHTTPClient(mockErrClient))
	if err != nil {
		t.Fatalf("failed to create error client: %v", err)
	}

	_, err = errClient.CreatePaymentLink(context.Background(), CreatePaymentLinkRequest{
		AmountPaise: 300,
		ReferenceID: "XG-DUPLICATE",
	})
	if err == nil || !strings.Contains(err.Error(), "Reference id already exists") {
		t.Fatalf("expected provider error message in response, got: %v", err)
	}
}

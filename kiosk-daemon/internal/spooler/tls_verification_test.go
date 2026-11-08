package spooler

import (
	"context"
	"crypto/tls"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// TestTLSVerification_RejectsUntrustedSelfSignedCertificate verifies that the client
// enforces standard TLS verification by default and strictly rejects unverified certificates.
func TestTLSVerification_RejectsUntrustedSelfSignedCertificate(t *testing.T) {
	var requestsReceived int64
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&requestsReceived, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	// 1. Build client without custom CA (uses system trust pool)
	client, err := BuildTLSHTTPClient("", 5*time.Second)
	if err != nil {
		t.Fatalf("BuildTLSHTTPClient failed: %v", err)
	}

	// 2. HTTPS request must fail certificate verification
	_, err = client.Get(server.URL)
	if err == nil {
		t.Fatalf("expected TLS verification failure for self-signed server, got nil")
	}
	if !strings.Contains(err.Error(), "certificate") && !strings.Contains(err.Error(), "x509") {
		t.Errorf("expected x509/certificate error, got %v", err)
	}

	// 3. WebSocket dial must fail certificate verification
	u, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("url.Parse failed: %v", err)
	}
	wsURL := "wss://" + u.Host + "/ws"

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	dialOpts := &websocket.DialOptions{HTTPClient: client}
	_, _, err = websocket.Dial(ctx, wsURL, dialOpts)
	if err == nil {
		t.Fatalf("expected WebSocket TLS handshake failure for self-signed server, got nil")
	}
	if !strings.Contains(err.Error(), "certificate") && !strings.Contains(err.Error(), "x509") && !strings.Contains(err.Error(), "handshake") {
		t.Errorf("expected TLS handshake error, got %v", err)
	}

	// 4. Verify no request reached application handler
	if atomic.LoadInt64(&requestsReceived) != 0 {
		t.Errorf("expected 0 application requests received, got %d", atomic.LoadInt64(&requestsReceived))
	}
}

// TestTLSVerification_AcceptsExplicitTrustedRootCA verifies that self-signed or private
// certificates are accepted ONLY when explicitly configured via RootCAPath.
func TestTLSVerification_AcceptsExplicitTrustedRootCA(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") == "websocket" {
			c, err := websocket.Accept(w, r, nil)
			if err == nil {
				_ = c.Close(websocket.StatusNormalClosure, "done")
			}
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("tls-verified-ok"))
	}))
	defer server.Close()

	// Export test server CA certificate to PEM file
	tmpDir := t.TempDir()
	certPath := filepath.Join(tmpDir, "server_ca.pem")
	certPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: server.Certificate().Raw,
	})
	if err := os.WriteFile(certPath, certPEM, 0644); err != nil {
		t.Fatalf("failed to write CA PEM: %v", err)
	}

	// Build client with explicit trusted CA
	client, err := BuildTLSHTTPClient(certPath, 5*time.Second)
	if err != nil {
		t.Fatalf("BuildTLSHTTPClient with root CA failed: %v", err)
	}

	// 1. HTTPS request must succeed
	resp, err := client.Get(server.URL)
	if err != nil {
		t.Fatalf("HTTPS request failed with trusted root CA: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected HTTP 200, got %d", resp.StatusCode)
	}

	// 2. WebSocket dial must succeed
	u, _ := url.Parse(server.URL)
	wsURL := "wss://" + u.Host + "/ws"

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	dialOpts := &websocket.DialOptions{HTTPClient: client}
	c, _, err := websocket.Dial(ctx, wsURL, dialOpts)
	if err != nil {
		t.Fatalf("WebSocket dial failed with trusted root CA: %v", err)
	}
	_ = c.Close(websocket.StatusNormalClosure, "done")
}

// TestTLSVerification_CredentialsNeverSentOverInsecureTLS verifies that kiosk credentials
// (X-Kiosk-Secret) are never transferred over an unverified TLS connection.
func TestTLSVerification_CredentialsNeverSentOverInsecureTLS(t *testing.T) {
	var credentialsObserved atomic.Bool
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Kiosk-Secret") != "" || r.Header.Get("Authorization") != "" {
			credentialsObserved.Store(true)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	// Client without server CA in trust pool
	client, err := BuildTLSHTTPClient("", 3*time.Second)
	if err != nil {
		t.Fatalf("BuildTLSHTTPClient failed: %v", err)
	}

	wf, err := NewPaymentWorkflow(server.URL, "kiosk-001", "super-secret-kiosk-token-12345", client, 1*time.Second)
	if err != nil {
		t.Fatalf("NewPaymentWorkflow failed: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// Attempt PIN redemption
	_, err = wf.RedeemPIN(ctx, "0482")
	if err == nil {
		t.Fatalf("expected redemption to fail due to untrusted TLS cert")
	}

	// Crucial security invariant: TLS handshake aborted before any headers were received
	if credentialsObserved.Load() {
		t.Fatalf("CRITICAL SECURITY VIOLATION: X-Kiosk-Secret was sent to unverified TLS server!")
	}
}

// TestTLSVerification_StandardCipherSuitesEnforced verifies that minimum TLS 1.2 is enforced.
func TestTLSVerification_StandardCipherSuitesEnforced(t *testing.T) {
	client, err := BuildTLSHTTPClient("", 5*time.Second)
	if err != nil {
		t.Fatalf("BuildTLSHTTPClient failed: %v", err)
	}

	tr, ok := client.Transport.(*http.Transport)
	if !ok || tr.TLSClientConfig == nil {
		t.Fatalf("expected *http.Transport with non-nil TLSClientConfig")
	}

	if tr.TLSClientConfig.InsecureSkipVerify {
		t.Fatalf("CRITICAL SECURITY VIOLATION: InsecureSkipVerify must never be enabled!")
	}

	if tr.TLSClientConfig.MinVersion < tls.VersionTLS12 {
		t.Errorf("expected MinVersion >= TLS 1.2, got 0x%04x", tr.TLSClientConfig.MinVersion)
	}
}

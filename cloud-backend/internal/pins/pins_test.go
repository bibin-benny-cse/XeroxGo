package pins

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestGenerateUniformPIN_FormatAndLeadingZeros(t *testing.T) {
	seenLeadingZero := false

	// Generate 1000 PINs; verify exactly 4 numeric characters and that leading zeros occur.
	for i := 0; i < 1000; i++ {
		pin, err := GenerateUniformPIN()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(pin) != 4 {
			t.Fatalf("expected length 4, got %q (len %d)", pin, len(pin))
		}
		val, err := strconv.Atoi(pin)
		if err != nil {
			t.Fatalf("expected numeric string, got %q: %v", pin, err)
		}
		if val < 0 || val > 9999 {
			t.Fatalf("pin out of range [0000..9999]: %d", val)
		}
		if pin[0] == '0' {
			seenLeadingZero = true
		}
	}

	if !seenLeadingZero {
		t.Errorf("expected to see at least one PIN with leading zero in 1000 samples")
	}
}

func TestGenerateUniformPIN_UnbiasedDistribution(t *testing.T) {
	// Sample 20,000 PINs into 10 buckets (by first digit 0..9).
	// For uniform distribution, each bucket has expected count ~2000.
	const totalSamples = 20000
	buckets := make([]int, 10)

	for i := 0; i < totalSamples; i++ {
		pin, err := GenerateUniformPIN()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		digit := int(pin[0] - '0')
		buckets[digit]++
	}

	expected := float64(totalSamples) / 10.0
	for digit, count := range buckets {
		deviation := float64(count) - expected
		if deviation < 0 {
			deviation = -deviation
		}
		ratio := deviation / expected
		// Allow up to 15% tolerance on 20,000 samples for 10 buckets
		if ratio > 0.15 {
			t.Errorf("bucket %d count %d deviates too much from expected %.0f (ratio %.2f)", digit, count, expected, ratio)
		}
	}
}

func TestComputePINHMAC_EnforcesSecretStrength(t *testing.T) {
	shortSecret := []byte("too-short-secret-15b")
	_, err := ComputePINHMAC(shortSecret, "kiosk-demo", "1234")
	if err != ErrSecretTooShort {
		t.Fatalf("expected ErrSecretTooShort for secret < 32 bytes, got %v", err)
	}

	exactSecret := []byte("12345678901234567890123456789012") // 32 bytes
	hmacStr, err := ComputePINHMAC(exactSecret, "kiosk-demo", "1234")
	if err != nil {
		t.Fatalf("expected success with 32-byte secret, got %v", err)
	}
	if len(hmacStr) != 64 {
		t.Fatalf("expected 64-char hex string, got %q", hmacStr)
	}
}

func TestComputePINHMAC_FormatValidation(t *testing.T) {
	validSecret := []byte("a-very-long-secret-key-of-32-bytes-minimum")

	tests := []struct {
		name    string
		kioskID string
		pin     string
		wantErr error
	}{
		{"empty kiosk", "", "1234", ErrEmptyKioskID},
		{"whitespace kiosk", "   ", "1234", ErrEmptyKioskID},
		{"too short pin", "kiosk-1", "123", ErrInvalidPINFormat},
		{"too long pin", "kiosk-1", "12345", ErrInvalidPINFormat},
		{"non numeric pin", "kiosk-1", "12a4", ErrInvalidPINFormat},
		{"spaces in pin", "kiosk-1", "12 4", ErrInvalidPINFormat},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ComputePINHMAC(validSecret, tc.kioskID, tc.pin)
			if err != tc.wantErr {
				t.Fatalf("expected %v, got %v", tc.wantErr, err)
			}
		})
	}
}

func TestComputePINHMAC_DomainSeparationAndKioskIsolation(t *testing.T) {
	secret := []byte("my-super-secret-key-that-is-at-least-32-bytes-long")
	pin := "0482"

	hmacKiosk1, err := ComputePINHMAC(secret, "kiosk-alpha", pin)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	hmacKiosk2, err := ComputePINHMAC(secret, "kiosk-beta", pin)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if hmacKiosk1 == hmacKiosk2 {
		t.Fatalf("domain separation failed: different kiosks with the same PIN produced identical HMAC")
	}

	// Verify constant-time validation helper
	ok1, err := VerifyPINHMAC(secret, "kiosk-alpha", pin, hmacKiosk1)
	if err != nil || !ok1 {
		t.Fatalf("expected VerifyPINHMAC to succeed for matching kiosk, got ok=%v, err=%v", ok1, err)
	}

	ok2, err := VerifyPINHMAC(secret, "kiosk-beta", pin, hmacKiosk1)
	if err != nil || ok2 {
		t.Fatalf("expected VerifyPINHMAC to reject HMAC evaluated against wrong kiosk, got ok=%v, err=%v", ok2, err)
	}
}

func TestOrderOwnershipSecret_GenerationAndVerification(t *testing.T) {
	plaintext, hash, err := GenerateOrderOwnershipSecret()
	if err != nil {
		t.Fatalf("unexpected error generating order secret: %v", err)
	}

	if len(plaintext) != 64 {
		t.Fatalf("expected 64-char hex secret (32 random bytes), got %q", plaintext)
	}
	if len(hash) != 64 {
		t.Fatalf("expected 64-char hex SHA256 hash, got %q", hash)
	}

	// Verify hash matches
	expectedH := sha256.Sum256([]byte(plaintext))
	if hex.EncodeToString(expectedH[:]) != hash {
		t.Fatalf("hash mismatch: expected %s, got %s", hex.EncodeToString(expectedH[:]), hash)
	}

	// Test constant-time verification
	if !VerifyOrderOwnership(plaintext, hash) {
		t.Fatalf("VerifyOrderOwnership rejected valid plaintext secret")
	}

	if VerifyOrderOwnership("invalid_secret_token", hash) {
		t.Fatalf("VerifyOrderOwnership accepted invalid secret")
	}

	if VerifyOrderOwnership("", hash) {
		t.Fatalf("VerifyOrderOwnership accepted empty secret")
	}
}

func TestRateLimiter_ProgressiveCooldownAndNoPermanentLockout(t *testing.T) {
	cfg := CooldownConfig{
		GraceAttempts:  3,
		WindowDuration: 5 * time.Minute,
		MaxCooldown:    10 * time.Second,
	}
	rl := NewRateLimiter(cfg)

	kioskID := "kiosk-unit-1"
	clientIP := "192.168.1.50"
	now := time.Now()

	// 1. Initial grace attempts (failures 1..2) incur no cooldown
	for i := 1; i <= 2; i++ {
		allowed, _ := rl.Check(kioskID, clientIP, now)
		if !allowed {
			t.Fatalf("attempt %d should be allowed", i)
		}
		cd := rl.RecordFailure(kioskID, clientIP, now)
		if cd != 0 {
			t.Fatalf("attempt %d should have 0 cooldown, got %v", i, cd)
		}
	}

	// 2. Failure 3 (exhausts grace attempts): progressive cooldown begins (3s)
	cd := rl.RecordFailure(kioskID, clientIP, now)
	if cd != 3*time.Second {
		t.Fatalf("expected 3s cooldown for failure 3, got %v", cd)
	}

	allowed, rem := rl.Check(kioskID, clientIP, now.Add(1*time.Second))
	if allowed {
		t.Fatalf("expected check to be blocked during cooldown")
	}
	if rem <= 0 || rem > 3*time.Second {
		t.Fatalf("unexpected remaining cooldown: %v", rem)
	}

	// 3. After 3s passes, attempt is allowed again
	allowed, _ = rl.Check(kioskID, clientIP, now.Add(4*time.Second))
	if !allowed {
		t.Fatalf("expected attempt to be allowed after cooldown expired")
	}

	// 4. Multiple failures progress up to MaxCooldown (10s) and NEVER permanently lockout
	for i := 0; i < 20; i++ {
		cd = rl.RecordFailure(kioskID, clientIP, now.Add(time.Duration(i)*15*time.Second))
	}
	if cd != 10*time.Second {
		t.Fatalf("cooldown should be capped at MaxCooldown (10s), got %v", cd)
	}

	// Verify no permanent lockout: after 11s, kiosk is allowed to try again
	allowed, _ = rl.Check(kioskID, clientIP, now.Add(20*15*time.Second+11*time.Second))
	if !allowed {
		t.Fatalf("permanent lockout detected: kiosk should be allowed to attempt after cooldown expires")
	}

	// 5. Success resets consecutive failures
	rl.RecordSuccess(kioskID, clientIP)
	allowed, _ = rl.Check(kioskID, clientIP, now.Add(400*time.Second))
	if !allowed {
		t.Fatalf("expected allowed after success")
	}
	// Next failure should be in grace period again (cd = 0)
	cd = rl.RecordFailure(kioskID, clientIP, now.Add(401*time.Second))
	if cd != 0 {
		t.Fatalf("expected failure after reset to be in grace period, got %v", cd)
	}
}

func TestRateLimiter_ClientIPIsolation(t *testing.T) {
	rl := NewRateLimiter(DefaultCooldownConfig())
	kioskID := "kiosk-multi-source"
	attackerIP := "203.0.113.5"
	legitimateIP := "10.0.0.1"
	now := time.Now()

	// Attacker burns grace period and triggers cooldown
	for i := 0; i < 4; i++ {
		rl.RecordFailure(kioskID, attackerIP, now)
	}

	// Attacker is in cooldown
	allowed, _ := rl.Check(kioskID, attackerIP, now.Add(1*time.Second))
	if allowed {
		t.Fatalf("attacker should be under cooldown")
	}

	// Legitimate client from different IP is still permitted
	allowedLegit, _ := rl.Check(kioskID, legitimateIP, now.Add(1*time.Second))
	if !allowedLegit {
		t.Fatalf("legitimate IP was blocked by attacker's failed attempts")
	}
}

func TestPublicModels_NoSecretLeakage(t *testing.T) {
	pub := PublicOrderResponse{
		OrderID:     "ord_123",
		JobID:       "job_123",
		DisplayCode: "PIN-PROTECTED",
		Status:      "PENDING_PAYMENT",
		PricePaise:  500,
	}

	// Check fields do not contain secret hash
	if strings.Contains(pub.DisplayCode, "hash") || strings.Contains(pub.DisplayCode, "secret") {
		t.Fatalf("display code contains sensitive substring: %s", pub.DisplayCode)
	}
}

func TestRateLimiter_MapBoundsAndEviction(t *testing.T) {
	cfg := CooldownConfig{
		GraceAttempts:  3,
		WindowDuration: 5 * time.Minute,
		MaxCooldown:    10 * time.Second,
		MaxEntries:     5, // Strict bound for testing
	}
	rl := NewRateLimiter(cfg)
	now := time.Now()

	// Insert 10 different client IPs
	for i := 1; i <= 10; i++ {
		ip := fmt.Sprintf("192.168.1.%d", i)
		rl.RecordFailure("kiosk-test", ip, now.Add(time.Duration(i)*time.Millisecond))
	}

	count := rl.EntryCount()
	if count > 5 {
		t.Fatalf("expected map bounds to cap at MaxEntries (5), got %d", count)
	}
}

func TestRateLimiter_CleanupExpired(t *testing.T) {
	cfg := CooldownConfig{
		GraceAttempts:  3,
		WindowDuration: 2 * time.Minute,
		MaxCooldown:    10 * time.Second,
		MaxEntries:     100,
	}
	rl := NewRateLimiter(cfg)
	start := time.Now()

	rl.RecordFailure("kiosk-test", "10.0.0.1", start)
	rl.RecordFailure("kiosk-test", "10.0.0.2", start)

	if rl.EntryCount() != 2 {
		t.Fatalf("expected 2 entries, got %d", rl.EntryCount())
	}

	// Clean up at t+1 minute (not expired yet)
	cleaned := rl.Cleanup(start.Add(1 * time.Minute))
	if cleaned != 0 || rl.EntryCount() != 2 {
		t.Fatalf("expected 0 entries cleaned at 1 minute, got cleaned=%d, remaining=%d", cleaned, rl.EntryCount())
	}

	// Clean up at t+3 minutes (past WindowDuration of 2m)
	// Cleans 2 client entries and 1 kiosk aggregate entry = 3 total entries
	cleaned = rl.Cleanup(start.Add(3 * time.Minute))
	if cleaned != 3 || rl.EntryCount() != 0 {
		t.Fatalf("expected 3 entries cleaned at 3 minutes, got cleaned=%d, remaining=%d", cleaned, rl.EntryCount())
	}
}

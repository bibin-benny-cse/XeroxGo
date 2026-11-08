package pins

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"strings"
)

const (
	// DomainPrefix enforces versioned domain separation for pickup PIN HMAC calculation.
	DomainPrefix = "v1:pickup_pin"

	// MinSecretLength is the required minimum byte length for XEROXGO_PICKUP_PIN_HMAC_SECRET.
	MinSecretLength = 32
)

var (
	ErrSecretTooShort    = errors.New("pickup pin HMAC secret must be at least 32 bytes")
	ErrEmptyKioskID      = errors.New("kiosk ID cannot be empty")
	ErrInvalidPINFormat  = errors.New("pin must be exactly 4 numeric digits")
	ErrInvalidSecretHash = errors.New("order secret hash is invalid or empty")
)

// GenerateUniformPIN generates an unbiased 4-digit numeric string in the range [0000..9999]
// using crypto/rand.Int. It uses rejection sampling internally to eliminate modulo bias completely,
// while preserving leading zeros.
func GenerateUniformPIN() (string, error) {
	maxVal := big.NewInt(10000)
	n, err := rand.Int(rand.Reader, maxVal)
	if err != nil {
		return "", fmt.Errorf("generate uniform pin: %w", err)
	}
	return fmt.Sprintf("%04d", n.Int64()), nil
}

// GenerateOrderOwnershipSecret generates a cryptographically secure 32-byte secret (returned as a 64-character
// lowercase hexadecimal string) and its SHA-256 hexadecimal hash. The raw secret is returned once to the customer;
// only the hash is persisted.
func GenerateOrderOwnershipSecret() (plaintext string, hash string, err error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", "", fmt.Errorf("generate order secret: %w", err)
	}
	plaintext = hex.EncodeToString(buf)
	h := sha256.Sum256([]byte(plaintext))
	hash = hex.EncodeToString(h[:])
	return plaintext, hash, nil
}

// VerifyOrderOwnership checks a candidate plaintext order ownership secret against a stored SHA-256 hash
// using constant-time comparison to prevent timing side channels.
func VerifyOrderOwnership(candidateSecret string, storedHash string) bool {
	candidateSecret = strings.TrimSpace(candidateSecret)
	storedHash = strings.TrimSpace(storedHash)
	if candidateSecret == "" || storedHash == "" {
		return false
	}
	h := sha256.Sum256([]byte(candidateSecret))
	candidateHash := hex.EncodeToString(h[:])
	return subtle.ConstantTimeCompare([]byte(candidateHash), []byte(storedHash)) == 1
}

// ComputePINHMAC computes the HMAC-SHA256 of the 4-digit PIN using the versioned domain separator:
// v1:pickup_pin:<kiosk_id>:<pin>
// It enforces that the secret is at least 32 bytes and the PIN is exactly 4 numeric digits.
func ComputePINHMAC(hmacSecret []byte, kioskID string, pin string) (string, error) {
	if len(hmacSecret) < MinSecretLength {
		return "", ErrSecretTooShort
	}
	kioskID = strings.TrimSpace(kioskID)
	if kioskID == "" {
		return "", ErrEmptyKioskID
	}
	if len(pin) != 4 {
		return "", ErrInvalidPINFormat
	}
	for i := 0; i < 4; i++ {
		if pin[i] < '0' || pin[i] > '9' {
			return "", ErrInvalidPINFormat
		}
	}

	msg := fmt.Sprintf("%s:%s:%s", DomainPrefix, kioskID, pin)
	mac := hmac.New(sha256.New, hmacSecret)
	mac.Write([]byte(msg))
	return hex.EncodeToString(mac.Sum(nil)), nil
}

// VerifyPINHMAC validates a candidate 4-digit PIN against an expected HMAC-SHA256 string
// using constant-time comparison.
func VerifyPINHMAC(hmacSecret []byte, kioskID string, pin string, expectedHMAC string) (bool, error) {
	actualHMAC, err := ComputePINHMAC(hmacSecret, kioskID, pin)
	if err != nil {
		return false, err
	}
	return subtle.ConstantTimeCompare([]byte(actualHMAC), []byte(expectedHMAC)) == 1, nil
}

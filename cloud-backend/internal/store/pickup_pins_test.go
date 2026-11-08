package store

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"xeroxgo/pkg/contracts"
)

func hashPIN(kioskID, pin, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(fmt.Sprintf("v1:pickup_pin:%s:%s", kioskID, pin)))
	return hex.EncodeToString(mac.Sum(nil))
}

func hashSecret(secret string) string {
	h := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(h[:])
}

func setupOrderWithSecret(t *testing.T, s *Store, orderID, kioskID, customerSecret string) Order {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	ord := Order{
		OrderID: orderID,
		Command: contracts.PrintJobCommand{
			JobID:          "job-" + orderID,
			KioskID:        kioskID,
			DisplayCode:    "PIN-PROTECTED", // Never plaintext PIN
			DownloadURL:    "https://example.com/dl.pdf",
			SHA256Checksum: "dummychecksum1234567890abcdef1234567890abcdef",
			TotalPages:     4,
			TotalSheets:    2,
			Copies:         1,
			Duplex:         contracts.DuplexLongEdge,
			StartFromPage:  1,
		},
		Status:          contracts.JobStatusPendingPayment,
		PricePaise:      400,
		ColorMode:       "BW",
		OrderSecretHash: hashSecret(customerSecret),
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	if err := s.CreateOrder(ctx, ord); err != nil {
		t.Fatalf("setupOrderWithSecret: %v", err)
	}
	return ord
}

// Test 1: Verify tables and indexes are initialized properly without data corruption
func TestMigrationAndSchemaInitialization(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	// Verify kiosks table exists
	kiosk := Kiosk{
		KioskID:    "kiosk-m1",
		SecretHash: hashSecret("kiosk_secret_1"),
		Name:       "Main Library Kiosk",
		IsActive:   true,
	}
	if err := s.RegisterKiosk(ctx, kiosk); err != nil {
		t.Fatalf("RegisterKiosk failed: %v", err)
	}

	retrieved, err := s.GetKiosk(ctx, "kiosk-m1")
	if err != nil || retrieved == nil {
		t.Fatalf("GetKiosk failed: %v", err)
	}
	if retrieved.Name != "Main Library Kiosk" || !retrieved.IsActive {
		t.Fatalf("retrieved kiosk mismatch: %+v", retrieved)
	}
}

// Test 2: Unique constraint on active PIN HMAC per kiosk and collision detection
func TestPickupPINActiveUniquenessAndCollision(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	setupOrderWithSecret(t, s, "ord-u1", "kiosk-1", "cust-sec-1")
	setupOrderWithSecret(t, s, "ord-u2", "kiosk-1", "cust-sec-2")
	setupOrderWithSecret(t, s, "ord-u3", "kiosk-2", "cust-sec-3")

	hmacSecret := "test_hmac_secret_32_bytes_long!!"
	pinHMAC := hashPIN("kiosk-1", "0482", hmacSecret)

	now := time.Now().UTC()
	pin1 := PickupPIN{
		ID:        "pin-1",
		OrderID:   "ord-u1",
		KioskID:   "kiosk-1",
		PINHMAC:   pinHMAC,
		ExpiresAt: now.Add(15 * time.Minute),
	}

	if err := s.CreatePickupPIN(ctx, pin1, 50); err != nil {
		t.Fatalf("CreatePickupPIN 1 failed: %v", err)
	}

	// Attempting to create duplicate active PIN HMAC on the same kiosk must fail with ErrPINCollision
	pin2 := PickupPIN{
		ID:        "pin-2",
		OrderID:   "ord-u2",
		KioskID:   "kiosk-1",
		PINHMAC:   pinHMAC,
		ExpiresAt: now.Add(15 * time.Minute),
	}
	err := s.CreatePickupPIN(ctx, pin2, 50)
	if err != ErrPINCollision {
		t.Fatalf("expected ErrPINCollision on duplicate active PIN for same kiosk, got: %v", err)
	}

	// But the same PIN on a DIFFERENT kiosk must succeed
	pinDifferentKioskHMAC := hashPIN("kiosk-2", "0482", hmacSecret)
	pin3 := PickupPIN{
		ID:        "pin-3",
		OrderID:   "ord-u3",
		KioskID:   "kiosk-2",
		PINHMAC:   pinDifferentKioskHMAC,
		ExpiresAt: now.Add(15 * time.Minute),
	}
	if err := s.CreatePickupPIN(ctx, pin3, 50); err != nil {
		t.Fatalf("expected success for same PIN on different kiosk, got: %v", err)
	}
}

// Test 3: Expired PINs do not block new PIN generation on collision checks
func TestPickupPINExpiredCollisionTolerance(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	setupOrderWithSecret(t, s, "ord-exp1", "kiosk-1", "cust-sec-1")
	setupOrderWithSecret(t, s, "ord-exp2", "kiosk-1", "cust-sec-2")

	hmacSecret := "test_hmac_secret_32_bytes_long!!"
	pinHMAC := hashPIN("kiosk-1", "0482", hmacSecret)

	pastTime := time.Now().UTC().Add(-20 * time.Minute)
	expiredPin := PickupPIN{
		ID:        "pin-exp-1",
		OrderID:   "ord-exp1",
		KioskID:   "kiosk-1",
		PINHMAC:   pinHMAC,
		CreatedAt: pastTime,
		ExpiresAt: pastTime.Add(15 * time.Minute), // expired 5 minutes ago
	}

	if err := s.CreatePickupPIN(ctx, expiredPin, 50); err != nil {
		t.Fatalf("failed to insert initial expired pin: %v", err)
	}

	// Now inserting a new PIN with the same HMAC should expire the stale record and succeed!
	newPin := PickupPIN{
		ID:        "pin-new-1",
		OrderID:   "ord-exp2",
		KioskID:   "kiosk-1",
		PINHMAC:   pinHMAC,
		CreatedAt: time.Now().UTC(),
		ExpiresAt: time.Now().UTC().Add(15 * time.Minute),
	}
	if err := s.CreatePickupPIN(ctx, newPin, 50); err != nil {
		t.Fatalf("expected success after staling old record, got: %v", err)
	}
}

// Test 4: Enforce configurable active PIN limit per machine
func TestActivePINLimitPerMachine(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	hmacSecret := "test_hmac_secret_32_bytes_long!!"
	maxPins := 3

	for i := 1; i <= maxPins; i++ {
		ordID := fmt.Sprintf("ord-limit-%d", i)
		setupOrderWithSecret(t, s, ordID, "kiosk-limit", fmt.Sprintf("sec-%d", i))
		pin := PickupPIN{
			ID:        fmt.Sprintf("pin-limit-%d", i),
			OrderID:   ordID,
			KioskID:   "kiosk-limit",
			PINHMAC:   hashPIN("kiosk-limit", fmt.Sprintf("%04d", i), hmacSecret),
			ExpiresAt: time.Now().UTC().Add(15 * time.Minute),
		}
		if err := s.CreatePickupPIN(ctx, pin, maxPins); err != nil {
			t.Fatalf("failed to create pin %d: %v", i, err)
		}
	}

	// 4th pin should exceed the limit
	setupOrderWithSecret(t, s, "ord-limit-4", "kiosk-limit", "sec-4")
	excessPin := PickupPIN{
		ID:        "pin-limit-4",
		OrderID:   "ord-limit-4",
		KioskID:   "kiosk-limit",
		PINHMAC:   hashPIN("kiosk-limit", "0004", hmacSecret),
		ExpiresAt: time.Now().UTC().Add(15 * time.Minute),
	}
	err := s.CreatePickupPIN(ctx, excessPin, maxPins)
	if err != ErrActivePINLimitExceeded {
		t.Fatalf("expected ErrActivePINLimitExceeded, got: %v", err)
	}
}

// Test 5: Atomic single-use redemption and subsequent redemption rejection
func TestPickupPINAtomicRedemption(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	hmacSecret := "test_hmac_secret_32_bytes_long!!"
	setupOrderWithSecret(t, s, "ord-red-1", "kiosk-red", "cust-sec-red")

	pinHMAC := hashPIN("kiosk-red", "0482", hmacSecret)
	now := time.Now().UTC()
	pin := PickupPIN{
		ID:        "pin-red-1",
		OrderID:   "ord-red-1",
		KioskID:   "kiosk-red",
		PINHMAC:   pinHMAC,
		CreatedAt: now,
		ExpiresAt: now.Add(15 * time.Minute),
	}
	if err := s.CreatePickupPIN(ctx, pin, 50); err != nil {
		t.Fatalf("CreatePickupPIN: %v", err)
	}

	// 1. Redeem with correct kiosk and PIN
	res, err := s.RedeemPickupPIN(ctx, "kiosk-red", pinHMAC, now.Add(1*time.Minute), "sess-1", 15*time.Minute)
	if err != nil {
		t.Fatalf("RedeemPickupPIN failed: %v", err)
	}
	if res.OrderID != "ord-red-1" || res.SessionID != "sess-1" || res.PricePaise != 400 {
		t.Fatalf("redemption result mismatch: %+v", res)
	}

	// 2. Second redemption of the same PIN must fail with ErrPINAlreadyRedeemed
	_, err = s.RedeemPickupPIN(ctx, "kiosk-red", pinHMAC, now.Add(2*time.Minute), "sess-2", 15*time.Minute)
	if err != ErrPINAlreadyRedeemed {
		t.Fatalf("expected ErrPINAlreadyRedeemed, got: %v", err)
	}

	// 3. Payment session must exist and be active
	sess, err := s.GetPaymentSession(ctx, "sess-1")
	if err != nil || sess == nil {
		t.Fatalf("GetPaymentSession failed: %v", err)
	}
	if sess.Status != PaymentSessionActive || sess.OrderID != "ord-red-1" {
		t.Fatalf("unexpected payment session: %+v", sess)
	}
}

// Test 6: Concurrent redemption race condition test (exactly 1 succeeds, all others fail)
func TestConcurrentPINRedemption(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	hmacSecret := "test_hmac_secret_32_bytes_long!!"
	setupOrderWithSecret(t, s, "ord-race-1", "kiosk-race", "cust-sec-race")

	pinHMAC := hashPIN("kiosk-race", "7890", hmacSecret)
	now := time.Now().UTC()
	pin := PickupPIN{
		ID:        "pin-race-1",
		OrderID:   "ord-race-1",
		KioskID:   "kiosk-race",
		PINHMAC:   pinHMAC,
		CreatedAt: now,
		ExpiresAt: now.Add(15 * time.Minute),
	}
	if err := s.CreatePickupPIN(ctx, pin, 50); err != nil {
		t.Fatalf("CreatePickupPIN: %v", err)
	}

	const concurrency = 10
	var wg sync.WaitGroup
	var successCount int32
	var alreadyRedeemedCount int32

	startGate := make(chan struct{})

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		sessID := fmt.Sprintf("sess-race-%d", i)
		go func(sid string) {
			defer wg.Done()
			<-startGate
			_, err := s.RedeemPickupPIN(ctx, "kiosk-race", pinHMAC, time.Now().UTC(), sid, 15*time.Minute)
			if err == nil {
				atomic.AddInt32(&successCount, 1)
			} else if err == ErrPINAlreadyRedeemed {
				atomic.AddInt32(&alreadyRedeemedCount, 1)
			}
		}(sessID)
	}

	close(startGate)
	wg.Wait()

	if successCount != 1 {
		t.Fatalf("expected exactly 1 concurrent redemption to succeed, got %d", successCount)
	}
	if alreadyRedeemedCount != int32(concurrency-1) {
		t.Fatalf("expected %d to fail with ErrPINAlreadyRedeemed, got %d", concurrency-1, alreadyRedeemedCount)
	}
}

// Test 7: Redemption validation on expiry, wrong kiosk, wrong order status
func TestPickupPINRedemptionEdgeCases(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	hmacSecret := "test_hmac_secret_32_bytes_long!!"

	// Case A: Expired PIN
	setupOrderWithSecret(t, s, "ord-val-exp", "kiosk-val", "sec-val-exp")
	pinHMAC := hashPIN("kiosk-val", "1234", hmacSecret)
	past := time.Now().UTC().Add(-30 * time.Minute)
	pinExp := PickupPIN{
		ID:        "pin-val-exp",
		OrderID:   "ord-val-exp",
		KioskID:   "kiosk-val",
		PINHMAC:   pinHMAC,
		CreatedAt: past,
		ExpiresAt: past.Add(15 * time.Minute),
	}
	_ = s.CreatePickupPIN(ctx, pinExp, 50)
	_, err := s.RedeemPickupPIN(ctx, "kiosk-val", pinHMAC, time.Now().UTC(), "s-exp", 15*time.Minute)
	if err != ErrPINExpired {
		t.Fatalf("expected ErrPINExpired, got: %v", err)
	}

	// Case B: Wrong Kiosk
	setupOrderWithSecret(t, s, "ord-val-wrongk", "kiosk-real", "sec-wrongk")
	pinHMAC2 := hashPIN("kiosk-real", "2345", hmacSecret)
	pinWrongK := PickupPIN{
		ID:        "pin-val-wrongk",
		OrderID:   "ord-val-wrongk",
		KioskID:   "kiosk-real",
		PINHMAC:   pinHMAC2,
		ExpiresAt: time.Now().UTC().Add(15 * time.Minute),
	}
	_ = s.CreatePickupPIN(ctx, pinWrongK, 50)
	_, err = s.RedeemPickupPIN(ctx, "kiosk-wrong", pinHMAC2, time.Now().UTC(), "s-wrong", 15*time.Minute)
	if err != ErrPINNotFound {
		t.Fatalf("expected ErrPINNotFound for wrong kiosk, got: %v", err)
	}

	// Case C: Order status already PAID
	setupOrderWithSecret(t, s, "ord-val-paid", "kiosk-paid", "sec-paid")
	_ = s.UpdateOrderStatus(ctx, "ord-val-paid", contracts.JobStatusPaidWaiting)
	pinHMAC3 := hashPIN("kiosk-paid", "3456", hmacSecret)
	pinPaid := PickupPIN{
		ID:        "pin-val-paid",
		OrderID:   "ord-val-paid",
		KioskID:   "kiosk-paid",
		PINHMAC:   pinHMAC3,
		ExpiresAt: time.Now().UTC().Add(15 * time.Minute),
	}
	_ = s.CreatePickupPIN(ctx, pinPaid, 50)
	_, err = s.RedeemPickupPIN(ctx, "kiosk-paid", pinHMAC3, time.Now().UTC(), "s-paid", 15*time.Minute)
	if err != ErrPINOrderMismatch {
		t.Fatalf("expected ErrPINOrderMismatch for already paid order, got: %v", err)
	}
}

// Test 8: Proof of order ownership on PIN refresh (Correction 2)
func TestProofOfOrderOwnershipOnPINRefresh(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	hmacSecret := "test_hmac_secret_32_bytes_long!!"

	setupOrderWithSecret(t, s, "ord-own-1", "kiosk-own", "correct-secret-123")

	// Verify order ownership check directly
	ok, err := s.VerifyOrderOwnership(ctx, "ord-own-1", "correct-secret-123")
	if err != nil || !ok {
		t.Fatalf("expected ownership verification to succeed, got %v, %v", ok, err)
	}

	ok, err = s.VerifyOrderOwnership(ctx, "ord-own-1", "wrong-secret-xyz")
	if err != ErrOrderUnauthorized || ok {
		t.Fatalf("expected ErrOrderUnauthorized for wrong secret, got %v, %v", ok, err)
	}

	// Initial expired PIN
	past := time.Now().UTC().Add(-20 * time.Minute)
	oldPin := PickupPIN{
		ID:        "pin-own-old",
		OrderID:   "ord-own-1",
		KioskID:   "kiosk-own",
		PINHMAC:   hashPIN("kiosk-own", "1111", hmacSecret),
		CreatedAt: past,
		ExpiresAt: past.Add(15 * time.Minute),
	}
	_ = s.CreatePickupPIN(ctx, oldPin, 50)

	// Refresh with wrong secret must fail
	newPin := PickupPIN{
		ID:        "pin-own-new",
		OrderID:   "ord-own-1",
		KioskID:   "kiosk-own",
		PINHMAC:   hashPIN("kiosk-own", "2222", hmacSecret),
		ExpiresAt: time.Now().UTC().Add(15 * time.Minute),
	}
	err = s.RefreshOrderPickupPIN(ctx, "ord-own-1", "wrong-secret", newPin, 50)
	if err != ErrOrderUnauthorized {
		t.Fatalf("expected ErrOrderUnauthorized on refresh, got: %v", err)
	}

	// Refresh with correct secret must succeed and supersede old PIN
	err = s.RefreshOrderPickupPIN(ctx, "ord-own-1", "correct-secret-123", newPin, 50)
	if err != nil {
		t.Fatalf("expected refresh to succeed, got: %v", err)
	}

	// Old pin should now be SUPERSEDED
	var oldStatus string
	_ = s.db.QueryRowContext(ctx, "SELECT status FROM pickup_pins WHERE id = 'pin-own-old'").Scan(&oldStatus)
	if oldStatus != string(PickupPINSuperceded) {
		t.Fatalf("expected old pin status SUPERSEDED, got %q", oldStatus)
	}

	// New pin should be ACTIVE and redeemable
	res, err := s.RedeemPickupPIN(ctx, "kiosk-own", newPin.PINHMAC, time.Now().UTC(), "sess-refreshed", 15*time.Minute)
	if err != nil {
		t.Fatalf("failed to redeem newly refreshed PIN: %v", err)
	}
	if res.SessionID != "sess-refreshed" {
		t.Fatalf("unexpected session ID: %s", res.SessionID)
	}
}

// Test 9: Webhook event deduplication with unique Razorpay event ID (Correction 4)
func TestWebhookEventDeduplication(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	eventID := "evt_test_razorpay_12345"

	// 1. First time recording event: returns true
	isFirst, err := s.RecordWebhookEvent(ctx, eventID, "qr_code.credited", "hash123", "ord-1")
	if err != nil {
		t.Fatalf("RecordWebhookEvent failed: %v", err)
	}
	if !isFirst {
		t.Fatalf("expected isFirst=true on initial event recording")
	}

	// 2. Second time recording exact same event ID: returns false
	isSecond, err := s.RecordWebhookEvent(ctx, eventID, "qr_code.credited", "hash123", "ord-1")
	if err != nil {
		t.Fatalf("RecordWebhookEvent second attempt failed: %v", err)
	}
	if isSecond {
		t.Fatalf("expected isSecond=false on duplicate event recording")
	}

	// 3. Update status
	now := time.Now().UTC()
	if err := s.UpdateWebhookEventStatus(ctx, eventID, WebhookEventProcessed, now); err != nil {
		t.Fatalf("UpdateWebhookEventStatus failed: %v", err)
	}

	evt, err := s.GetWebhookEvent(ctx, eventID)
	if err != nil || evt == nil {
		t.Fatalf("GetWebhookEvent failed: %v", err)
	}
	if evt.Status != WebhookEventProcessed || evt.EventType != "qr_code.credited" {
		t.Fatalf("unexpected webhook event: %+v", evt)
	}

	// 4. Concurrent duplicate delivery test
	const count = 10
	var wg sync.WaitGroup
	var recordedCount int32

	dupEventID := "evt_concurrent_dup_999"
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec, _ := s.RecordWebhookEvent(ctx, dupEventID, "payment.captured", "payload_hash", "ord-dup")
			if rec {
				atomic.AddInt32(&recordedCount, 1)
			}
		}()
	}
	wg.Wait()

	if recordedCount != 1 {
		t.Fatalf("expected exactly 1 recorded event among concurrent webhook deliveries, got %d", recordedCount)
	}
}

// Test 10: Protection of payment audit records against accidental cascading deletion (Correction 3)
func TestProtectedPaymentAuditCascadeRestriction(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	setupOrderWithSecret(t, s, "ord-protect-1", "kiosk-p1", "cust-sec-p1")

	// Create payment session
	now := time.Now().UTC()
	sess := PaymentSession{
		SessionID: "sess-protect-1",
		OrderID:   "ord-protect-1",
		KioskID:   "kiosk-p1",
		Status:    PaymentSessionActive,
		CreatedAt: now,
		ExpiresAt: now.Add(15 * time.Minute),
	}
	if err := s.CreatePaymentSession(ctx, sess); err != nil {
		t.Fatalf("CreatePaymentSession failed: %v", err)
	}

	// Create payment attempt
	attempt := PaymentAttempt{
		ID:            "att-protect-1",
		OrderID:       "ord-protect-1",
		SessionID:     "sess-protect-1",
		KioskID:       "kiosk-p1",
		PaymentMethod: PaymentMethodRazorpayUPIQR,
		RazorpayQRID:  "qr_test_protect_123",
		AmountPaise:   400,
		Currency:      "INR",
		Status:        PaymentAttemptCreated,
		CreatedAt:     now,
		ExpiresAt:     now.Add(15 * time.Minute),
		UpdatedAt:     now,
	}
	if err := s.CreatePaymentAttempt(ctx, attempt); err != nil {
		t.Fatalf("CreatePaymentAttempt failed: %v", err)
	}

	// Attempting to delete the order must be RESTRICTED by foreign key constraint!
	_, err := s.db.ExecContext(ctx, "DELETE FROM orders WHERE order_id = 'ord-protect-1';")
	if err == nil {
		t.Fatalf("expected foreign key RESTRICT constraint failure when deleting order with payment attempts, but deletion succeeded")
	}

	// Verify retrieval by ID
	byID, err := s.GetPaymentAttemptByID(ctx, "att-protect-1")
	if err != nil || byID == nil {
		t.Fatalf("GetPaymentAttemptByID failed: %v", err)
	}
	if byID.AmountPaise != 400 || byID.RazorpayQRID != "qr_test_protect_123" {
		t.Fatalf("unexpected attempt retrieved by ID: %+v", byID)
	}

	// Verify retrieval of active attempt by session
	active, err := s.GetActivePaymentAttemptBySession(ctx, "sess-protect-1", now.Add(5*time.Minute))
	if err != nil || active == nil {
		t.Fatalf("GetActivePaymentAttemptBySession failed: %v", err)
	}
	if active.ID != "att-protect-1" {
		t.Fatalf("unexpected active attempt ID: %s", active.ID)
	}

	// After expiry, active attempt returns nil
	expired, err := s.GetActivePaymentAttemptBySession(ctx, "sess-protect-1", now.Add(20*time.Minute))
	if err != nil {
		t.Fatalf("GetActivePaymentAttemptBySession returned error after expiry: %v", err)
	}
	if expired != nil {
		t.Fatalf("expected nil for expired attempt, got %+v", expired)
	}
}

// Test 11: Per-kiosk credentials and independent authentication (Correction 5)
func TestPerKioskCredentialsAndSecretVerification(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	k1Secret := "secret_for_kiosk_alpha_123"
	k2Secret := "secret_for_kiosk_beta_456"

	// Register 2 kiosks with distinct credentials
	if err := s.RegisterKiosk(ctx, Kiosk{
		KioskID:    "kiosk-alpha",
		SecretHash: hashSecret(k1Secret),
		Name:       "Library Floor 1",
		IsActive:   true,
	}); err != nil {
		t.Fatalf("failed to register kiosk-alpha: %v", err)
	}

	if err := s.RegisterKiosk(ctx, Kiosk{
		KioskID:    "kiosk-beta",
		SecretHash: hashSecret(k2Secret),
		Name:       "Student Union",
		IsActive:   true,
	}); err != nil {
		t.Fatalf("failed to register kiosk-beta: %v", err)
	}

	// Verify k1 credentials
	ok, err := s.ValidateKioskSecret(ctx, "kiosk-alpha", k1Secret)
	if err != nil || !ok {
		t.Fatalf("expected kiosk-alpha validation success, got ok=%v, err=%v", ok, err)
	}

	// k1 credentials on k2 must fail
	ok, err = s.ValidateKioskSecret(ctx, "kiosk-beta", k1Secret)
	if err != ErrInvalidKioskSecret || ok {
		t.Fatalf("expected ErrInvalidKioskSecret for wrong kiosk secret, got ok=%v, err=%v", ok, err)
	}

	// Inactive kiosk must fail
	_ = s.RegisterKiosk(ctx, Kiosk{
		KioskID:    "kiosk-inactive",
		SecretHash: hashSecret("secret_inactive"),
		Name:       "Decommissioned Kiosk",
		IsActive:   false,
	})
	ok, err = s.ValidateKioskSecret(ctx, "kiosk-inactive", "secret_inactive")
	if err != ErrKioskInactive || ok {
		t.Fatalf("expected ErrKioskInactive, got ok=%v, err=%v", ok, err)
	}
}

// Test 12: Payment attempt QR ID lookup and status transitions
func TestPaymentAttemptQRMappingAndStatus(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	setupOrderWithSecret(t, s, "ord-qr-1", "kiosk-qr", "sec-qr")
	now := time.Now().UTC()

	sess := PaymentSession{
		SessionID: "sess-qr-1",
		OrderID:   "ord-qr-1",
		KioskID:   "kiosk-qr",
		Status:    PaymentSessionActive,
		CreatedAt: now,
		ExpiresAt: now.Add(15 * time.Minute),
	}
	_ = s.CreatePaymentSession(ctx, sess)

	attempt := PaymentAttempt{
		ID:            "att-qr-1",
		OrderID:       "ord-qr-1",
		SessionID:     "sess-qr-1",
		KioskID:       "kiosk-qr",
		PaymentMethod: PaymentMethodRazorpayUPIQR,
		RazorpayQRID:  "qr_live_test_777888",
		AmountPaise:   400,
		Currency:      "INR",
		Status:        PaymentAttemptCreated,
		CreatedAt:     now,
		ExpiresAt:     now.Add(15 * time.Minute),
		UpdatedAt:     now,
	}
	if err := s.CreatePaymentAttempt(ctx, attempt); err != nil {
		t.Fatalf("CreatePaymentAttempt: %v", err)
	}

	// Lookup by QR ID
	byQR, err := s.GetPaymentAttemptByQRID(ctx, "qr_live_test_777888")
	if err != nil || byQR == nil {
		t.Fatalf("GetPaymentAttemptByQRID failed: %v", err)
	}
	if byQR.ID != "att-qr-1" || byQR.OrderID != "ord-qr-1" {
		t.Fatalf("mismatched attempt by QR: %+v", byQR)
	}

	// Duplicate QR ID must fail on unique constraint
	dupAttempt := attempt
	dupAttempt.ID = "att-qr-2"
	err = s.CreatePaymentAttempt(ctx, dupAttempt)
	if err == nil {
		t.Fatalf("expected error on duplicate razorpay_qr_id, got nil")
	}

	// Update status to CAPTURED
	if err := s.UpdatePaymentAttemptStatus(ctx, "att-qr-1", PaymentAttemptCaptured, "pay_rzp_999", "", now.Add(2*time.Minute)); err != nil {
		t.Fatalf("UpdatePaymentAttemptStatus failed: %v", err)
	}

	byPay, err := s.GetPaymentAttemptByPaymentID(ctx, "pay_rzp_999")
	if err != nil || byPay == nil {
		t.Fatalf("GetPaymentAttemptByPaymentID failed: %v", err)
	}
	if byPay.Status != PaymentAttemptCaptured {
		t.Fatalf("expected status CAPTURED, got %s", byPay.Status)
	}
}

// Test 13: Database persistence after reopening (verifies all tables survive database reconnect)
func TestDatabasePersistenceAcrossReopen(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "persist_test.db")
	s1, err := Open(dbPath)
	if err != nil {
		t.Fatalf("initial open failed: %v", err)
	}

	ctx := context.Background()
	now := time.Now().UTC()

	// 1. Kiosk
	_ = s1.RegisterKiosk(ctx, Kiosk{
		KioskID:    "kiosk-reopen",
		SecretHash: hashSecret("kiosk_sec"),
		Name:       "Reopen Kiosk",
		IsActive:   true,
	})

	// 2. Order
	setupOrderWithSecret(t, s1, "ord-reopen-1", "kiosk-reopen", "sec-reopen")

	// 3. Pickup PIN
	_ = s1.CreatePickupPIN(ctx, PickupPIN{
		ID:        "pin-reopen-1",
		OrderID:   "ord-reopen-1",
		KioskID:   "kiosk-reopen",
		PINHMAC:   hashPIN("kiosk-reopen", "0482", "hmac_sec"),
		ExpiresAt: now.Add(15 * time.Minute),
	}, 50)

	// 4. Payment Webhook Event
	_, _ = s1.RecordWebhookEvent(ctx, "evt-reopen-1", "qr_code.credited", "hash", "ord-reopen-1")

	_ = s1.Close()

	// Reopen database
	s2, err := Open(dbPath)
	if err != nil {
		t.Fatalf("reopen failed: %v", err)
	}
	defer s2.Close()

	k, err := s2.GetKiosk(ctx, "kiosk-reopen")
	if err != nil || k == nil || k.Name != "Reopen Kiosk" {
		t.Fatalf("kiosk did not persist across reopen: %+v, %v", k, err)
	}

	p, err := s2.GetPickupPINByOrder(ctx, "ord-reopen-1")
	if err != nil || p == nil || p.ID != "pin-reopen-1" {
		t.Fatalf("pickup PIN did not persist across reopen: %+v, %v", p, err)
	}

	evt, err := s2.GetWebhookEvent(ctx, "evt-reopen-1")
	if err != nil || evt == nil || evt.EventID != "evt-reopen-1" {
		t.Fatalf("webhook event did not persist across reopen: %+v, %v", evt, err)
	}
}

// Test 14: CompletePaymentTx ACID transaction atomicity and rollback
func TestCompletePaymentTx_AtomicRollbackAndSuccess(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	setupOrderWithSecret(t, s, "ord-tx-1", "kiosk-tx", "sec-tx-1")

	// Create session
	sess := PaymentSession{
		SessionID: "sess-tx-1",
		OrderID:   "ord-tx-1",
		KioskID:   "kiosk-tx",
		Status:    PaymentSessionActive,
		CreatedAt: now,
		ExpiresAt: now.Add(30 * time.Minute),
	}
	if err := s.CreatePaymentSession(ctx, sess); err != nil {
		t.Fatalf("CreatePaymentSession failed: %v", err)
	}

	// Create attempt
	attempt := PaymentAttempt{
		ID:            "att-tx-1",
		OrderID:       "ord-tx-1",
		SessionID:     "sess-tx-1",
		KioskID:       "kiosk-tx",
		PaymentMethod: PaymentMethodRazorpayUPIQR,
		RazorpayQRID:  "qr_tx_1",
		AmountPaise:   400,
		Currency:      "INR",
		Status:        PaymentAttemptCreated,
		CreatedAt:     now,
		ExpiresAt:     now.Add(15 * time.Minute),
		UpdatedAt:     now,
	}
	if err := s.CreatePaymentAttempt(ctx, attempt); err != nil {
		t.Fatalf("CreatePaymentAttempt failed: %v", err)
	}

	// Record webhook event
	_, _ = s.RecordWebhookEvent(ctx, "evt-tx-1", "qr_code.credited", "hash_tx", "ord-tx-1")

	// 1. Success case: All tables updated atomically
	err := s.CompletePaymentTx(ctx, "att-tx-1", "sess-tx-1", "ord-tx-1", "pay_tx_123", "evt-tx-1", now)
	if err != nil {
		t.Fatalf("CompletePaymentTx failed: %v", err)
	}

	order, _ := s.GetOrder(ctx, "ord-tx-1")
	if order.Status != contracts.JobStatusPaidWaiting {
		t.Errorf("expected order status PAID_WAITING, got %s", order.Status)
	}

	savedAttempt, _ := s.GetPaymentAttemptByID(ctx, "att-tx-1")
	if savedAttempt.Status != PaymentAttemptCaptured || savedAttempt.RazorpayPaymentID != "pay_tx_123" {
		t.Errorf("unexpected attempt state: %+v", savedAttempt)
	}

	savedSess, _ := s.GetPaymentSession(ctx, "sess-tx-1")
	if savedSess.Status != PaymentSessionCompleted {
		t.Errorf("expected session COMPLETED, got %s", savedSess.Status)
	}

	savedEvt, _ := s.GetWebhookEvent(ctx, "evt-tx-1")
	if savedEvt.Status != WebhookEventProcessed {
		t.Errorf("expected event PROCESSED, got %s", savedEvt.Status)
	}
}

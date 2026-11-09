package store

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"xeroxgo/pkg/contracts"
)

type PickupPINStatus string

const (
	PickupPINActive     PickupPINStatus = "ACTIVE"
	PickupPINRedeemed   PickupPINStatus = "REDEEMED"
	PickupPINExpired    PickupPINStatus = "EXPIRED"
	PickupPINSuperceded PickupPINStatus = "SUPERSEDED"
)

type PickupPIN struct {
	ID         string
	OrderID    string
	KioskID    string
	PINHMAC    string // HMAC-SHA256 only, never plaintext
	Status     PickupPINStatus
	CreatedAt  time.Time
	ExpiresAt  time.Time
	RedeemedAt time.Time
}

type PickupPINRedemptionResult struct {
	PINID       string
	OrderID     string
	KioskID     string
	SessionID   string
	PricePaise  int64
	TotalPages  int
	TotalSheets int
	Copies      int
	Duplex      contracts.DuplexMode
	ColorMode   string
	ExpiresAt   time.Time
}

type PaymentSessionStatus string

const (
	PaymentSessionActive    PaymentSessionStatus = "ACTIVE"
	PaymentSessionCompleted PaymentSessionStatus = "COMPLETED"
	PaymentSessionExpired   PaymentSessionStatus = "EXPIRED"
)

type PaymentSession struct {
	SessionID string
	OrderID   string
	KioskID   string
	Status    PaymentSessionStatus
	CreatedAt time.Time
	ExpiresAt time.Time
}

type PaymentMethod string

const (
	PaymentMethodRazorpayUPIQR    PaymentMethod = "RAZORPAY_UPI_QR"
	PaymentMethodRazorpayStandard PaymentMethod = "RAZORPAY_STANDARD"
)

type PaymentAttemptStatus string

const (
	PaymentAttemptCreated  PaymentAttemptStatus = "CREATED"
	PaymentAttemptCaptured PaymentAttemptStatus = "CAPTURED"
	PaymentAttemptFailed   PaymentAttemptStatus = "FAILED"
	PaymentAttemptExpired  PaymentAttemptStatus = "EXPIRED"
)

type PaymentAttempt struct {
	ID                string
	OrderID           string
	SessionID         string
	KioskID           string
	PaymentMethod     PaymentMethod
	RazorpayQRID      string
	RazorpayOrderID   string
	RazorpayPaymentID string
	QRImageURL        string
	AmountPaise       int64
	Currency          string
	Status            PaymentAttemptStatus
	ErrorReason       string
	CreatedAt         time.Time
	ExpiresAt         time.Time
	UpdatedAt         time.Time
}

type WebhookEventStatus string

const (
	WebhookEventReceived  WebhookEventStatus = "RECEIVED"
	WebhookEventProcessed WebhookEventStatus = "PROCESSED"
	WebhookEventFailed    WebhookEventStatus = "FAILED"
	WebhookEventIgnored   WebhookEventStatus = "IGNORED"
)

type WebhookEvent struct {
	EventID     string
	EventType   string
	PayloadHash string
	OrderID     string
	Status      WebhookEventStatus
	ReceivedAt  time.Time
	ProcessedAt time.Time
}

type Kiosk struct {
	KioskID    string
	SecretHash string
	Name       string
	IsActive   bool
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

var (
	ErrPINNotFound            = errors.New("pickup PIN not found")
	ErrPINExpired             = errors.New("pickup PIN is expired")
	ErrPINAlreadyRedeemed     = errors.New("pickup PIN has already been redeemed")
	ErrPINInvalidStatus       = errors.New("pickup PIN is not active")
	ErrPINOrderMismatch       = errors.New("pickup PIN order does not match kiosk or status")
	ErrPINCollision           = errors.New("pickup PIN collision on active PINs")
	ErrActivePINLimitExceeded = errors.New("maximum active pickup PINs exceeded for kiosk")
	ErrKioskNotFound          = errors.New("kiosk not found")
	ErrKioskInactive          = errors.New("kiosk is inactive")
	ErrInvalidKioskSecret     = errors.New("invalid kiosk secret")
	ErrOrderUnauthorized      = errors.New("order ownership verification failed")
	ErrSessionExpired         = errors.New("payment session is expired")
	ErrSessionNotFound        = errors.New("payment session not found")
)

// CreatePickupPIN expires stale active PINs for the kiosk, enforces the per-kiosk active limit,
// and persists a new pickup PIN record storing only the HMAC-SHA256 hash.
func (s *Store) CreatePickupPIN(ctx context.Context, pin PickupPIN, maxActivePerKiosk int) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("create pickup pin: store is closed or nil")
	}
	if ctx == nil {
		return fmt.Errorf("create pickup pin: context is nil")
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("create pickup pin: %w", err)
	}

	if strings.TrimSpace(pin.ID) == "" {
		return fmt.Errorf("create pickup pin: ID cannot be empty")
	}
	if strings.TrimSpace(pin.OrderID) == "" {
		return fmt.Errorf("create pickup pin: OrderID cannot be empty")
	}
	if strings.TrimSpace(pin.KioskID) == "" {
		return fmt.Errorf("create pickup pin: KioskID cannot be empty")
	}
	if strings.TrimSpace(pin.PINHMAC) == "" {
		return fmt.Errorf("create pickup pin: PINHMAC cannot be empty")
	}
	if pin.ExpiresAt.IsZero() {
		return fmt.Errorf("create pickup pin: ExpiresAt cannot be zero")
	}

	createdAt := pin.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	} else {
		createdAt = createdAt.UTC()
	}
	expiresAt := pin.ExpiresAt.UTC()
	createdNS := createdAt.UnixNano()
	expiresNS := expiresAt.UnixNano()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("create pickup pin: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// 1. Expire stale active records before collision checks
	_, err = tx.ExecContext(ctx, s.rebind(`
UPDATE pickup_pins
SET status = 'EXPIRED'
WHERE kiosk_id = ? AND status = 'ACTIVE' AND expires_at_ns <= ?;
`), pin.KioskID, createdNS)
	if err != nil {
		return fmt.Errorf("create pickup pin: expire stale pins: %w", err)
	}

	// 2. Enforce configurable active PIN limit per machine
	if maxActivePerKiosk > 0 {
		var activeCount int
		err = tx.QueryRowContext(ctx, s.rebind(`
SELECT COUNT(*) FROM pickup_pins
WHERE kiosk_id = ? AND status = 'ACTIVE';
`), pin.KioskID).Scan(&activeCount)
		if err != nil {
			return fmt.Errorf("create pickup pin: count active pins: %w", err)
		}
		if activeCount >= maxActivePerKiosk {
			return ErrActivePINLimitExceeded
		}
	}

	// 3. Insert PIN record. Active collision triggers ErrPINCollision via partial unique index.
	status := string(pin.Status)
	if status == "" {
		status = string(PickupPINActive)
	}

	_, err = tx.ExecContext(ctx, s.rebind(`
INSERT INTO pickup_pins (id, order_id, kiosk_id, pin_hmac, status, created_at_ns, expires_at_ns, redeemed_at_ns)
VALUES (?, ?, ?, ?, ?, ?, ?, 0);
`), pin.ID, pin.OrderID, pin.KioskID, pin.PINHMAC, status, createdNS, expiresNS)
	if err != nil {
		errMsg := strings.ToLower(err.Error())
		if strings.Contains(errMsg, "unique") || strings.Contains(errMsg, "constraint") {
			return ErrPINCollision
		}
		return fmt.Errorf("create pickup pin: insert: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("create pickup pin: commit tx: %w", err)
	}
	return nil
}

// RedeemPickupPIN atomically validates the active PIN, confirms expiry, kiosk association,
// order status, marks the PIN redeemed, and creates a machine payment session.
func (s *Store) RedeemPickupPIN(ctx context.Context, kioskID string, pinHMAC string, now time.Time, sessionID string, sessionTTL time.Duration) (*PickupPINRedemptionResult, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("redeem pickup pin: store is closed or nil")
	}
	if ctx == nil {
		return nil, fmt.Errorf("redeem pickup pin: context is nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("redeem pickup pin: %w", err)
	}
	if strings.TrimSpace(kioskID) == "" {
		return nil, fmt.Errorf("redeem pickup pin: kioskID cannot be empty")
	}
	if strings.TrimSpace(pinHMAC) == "" {
		return nil, fmt.Errorf("redeem pickup pin: pinHMAC cannot be empty")
	}
	if strings.TrimSpace(sessionID) == "" {
		return nil, fmt.Errorf("redeem pickup pin: sessionID cannot be empty")
	}
	if sessionTTL <= 0 {
		sessionTTL = 30 * time.Minute
	}

	nowUTC := now.UTC()
	nowNS := nowUTC.UnixNano()
	sessionExpiresNS := nowUTC.Add(sessionTTL).UnixNano()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("redeem pickup pin: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// 1. Fetch PIN matching kiosk_id and pin_hmac
	var (
		pinID        string
		orderID      string
		pinStatus    string
		expiresAtNS  int64
		redeemedAtNS int64
	)

	queryPIN := `
SELECT id, order_id, status, expires_at_ns, redeemed_at_ns
FROM pickup_pins
WHERE kiosk_id = ? AND pin_hmac = ?
ORDER BY created_at_ns DESC
LIMIT 1;
`
	err = tx.QueryRowContext(ctx, s.rebind(queryPIN), kioskID, pinHMAC).Scan(
		&pinID, &orderID, &pinStatus, &expiresAtNS, &redeemedAtNS,
	)
	if err == sql.ErrNoRows {
		return nil, ErrPINNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("redeem pickup pin: query pin: %w", err)
	}

	if pinStatus == string(PickupPINRedeemed) || redeemedAtNS > 0 {
		return nil, ErrPINAlreadyRedeemed
	}

	if pinStatus != string(PickupPINActive) {
		return nil, ErrPINInvalidStatus
	}

	if expiresAtNS <= nowNS {
		_, _ = tx.ExecContext(ctx, s.rebind(`UPDATE pickup_pins SET status = 'EXPIRED' WHERE id = ?;`), pinID)
		_ = tx.Commit()
		return nil, ErrPINExpired
	}

	// 2. Validate associated order: matches kiosk_id and status is PENDING_PAYMENT
	var (
		orderKioskID string
		orderStatus  string
		pricePaise   int64
		totalPages   int
		totalSheets  int
		copies       int
		duplex       string
		colorMode    string
	)

	queryOrder := `
SELECT o.kiosk_id, o.status, o.price_paise,
       p.total_pages, p.total_sheets, p.copies, p.duplex, COALESCE(p.color_mode, 'BW')
FROM orders o
JOIN print_configs p ON o.order_id = p.order_id
WHERE o.order_id = ?;
`
	err = tx.QueryRowContext(ctx, s.rebind(queryOrder), orderID).Scan(
		&orderKioskID, &orderStatus, &pricePaise,
		&totalPages, &totalSheets, &copies, &duplex, &colorMode,
	)
	if err == sql.ErrNoRows {
		return nil, ErrPINOrderMismatch
	}
	if err != nil {
		return nil, fmt.Errorf("redeem pickup pin: query order: %w", err)
	}

	if orderKioskID != kioskID || orderStatus != string(contracts.JobStatusPendingPayment) {
		return nil, ErrPINOrderMismatch
	}

	// 3. Atomically mark PIN as REDEEMED
	res, err := tx.ExecContext(ctx, s.rebind(`
UPDATE pickup_pins
SET status = 'REDEEMED', redeemed_at_ns = ?
WHERE id = ? AND status = 'ACTIVE';
`), nowNS, pinID)
	if err != nil {
		return nil, fmt.Errorf("redeem pickup pin: update status: %w", err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("redeem pickup pin: check rows affected: %w", err)
	}
	if rows != 1 {
		return nil, ErrPINAlreadyRedeemed
	}

	// 4. Invalidate any existing active sessions for this order to ensure only one active session exists
	_, err = tx.ExecContext(ctx, s.rebind(`
UPDATE payment_sessions
SET status = 'EXPIRED'
WHERE order_id = ? AND status = 'ACTIVE';
`), orderID)
	if err != nil {
		return nil, fmt.Errorf("redeem pickup pin: expire previous payment sessions: %w", err)
	}

	// 5. Create separate payment session bound to machine & order with its own expiry
	_, err = tx.ExecContext(ctx, s.rebind(`
INSERT INTO payment_sessions (session_id, order_id, kiosk_id, status, created_at_ns, expires_at_ns)
VALUES (?, ?, ?, 'ACTIVE', ?, ?);
`), sessionID, orderID, kioskID, nowNS, sessionExpiresNS)
	if err != nil {
		return nil, fmt.Errorf("redeem pickup pin: create payment session: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("redeem pickup pin: commit tx: %w", err)
	}

	return &PickupPINRedemptionResult{
		PINID:       pinID,
		OrderID:     orderID,
		KioskID:     kioskID,
		SessionID:   sessionID,
		PricePaise:  pricePaise,
		TotalPages:  totalPages,
		TotalSheets: totalSheets,
		Copies:      copies,
		Duplex:      contracts.DuplexMode(duplex),
		ColorMode:   colorMode,
		ExpiresAt:   time.Unix(0, sessionExpiresNS).UTC(),
	}, nil
}

// GetPickupPINByOrder returns the most recent pickup PIN associated with an order.
func (s *Store) GetPickupPINByOrder(ctx context.Context, orderID string) (*PickupPIN, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("get pickup pin by order: store is closed or nil")
	}
	if ctx == nil {
		return nil, fmt.Errorf("get pickup pin by order: context is nil")
	}
	if strings.TrimSpace(orderID) == "" {
		return nil, fmt.Errorf("get pickup pin by order: orderID cannot be empty")
	}

	query := `
SELECT id, order_id, kiosk_id, pin_hmac, status, created_at_ns, expires_at_ns, redeemed_at_ns
FROM pickup_pins
WHERE order_id = ?
ORDER BY created_at_ns DESC
LIMIT 1;
`
	var (
		id         string
		resOrderID string
		kioskID    string
		pinHMAC    string
		status     string
		createdNS  int64
		expiresNS  int64
		redeemedNS int64
	)
	err := s.db.QueryRowContext(ctx, s.rebind(query), orderID).Scan(
		&id, &resOrderID, &kioskID, &pinHMAC, &status, &createdNS, &expiresNS, &redeemedNS,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get pickup pin by order: scan: %w", err)
	}

	var redeemedAt time.Time
	if redeemedNS > 0 {
		redeemedAt = time.Unix(0, redeemedNS).UTC()
	}

	return &PickupPIN{
		ID:         id,
		OrderID:    resOrderID,
		KioskID:    kioskID,
		PINHMAC:    pinHMAC,
		Status:     PickupPINStatus(status),
		CreatedAt:  time.Unix(0, createdNS).UTC(),
		ExpiresAt:  time.Unix(0, expiresNS).UTC(),
		RedeemedAt: redeemedAt,
	}, nil
}

// VerifyOrderOwnership checks whether the provided plaintext secret matches the order's order_secret_hash.
func (s *Store) VerifyOrderOwnership(ctx context.Context, orderID string, secret string) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("verify order ownership: store is closed or nil")
	}
	if ctx == nil {
		return false, fmt.Errorf("verify order ownership: context is nil")
	}
	if strings.TrimSpace(orderID) == "" {
		return false, fmt.Errorf("verify order ownership: orderID cannot be empty")
	}
	if strings.TrimSpace(secret) == "" {
		return false, ErrOrderUnauthorized
	}

	var storedHash string
	err := s.db.QueryRowContext(ctx, s.rebind(`SELECT COALESCE(order_secret_hash, '') FROM orders WHERE order_id = ?;`), orderID).Scan(&storedHash)
	if err == sql.ErrNoRows {
		return false, ErrPINNotFound
	}
	if err != nil {
		return false, fmt.Errorf("verify order ownership: scan: %w", err)
	}

	if storedHash == "" {
		return false, ErrOrderUnauthorized
	}

	h := sha256.Sum256([]byte(secret))
	computedHash := hex.EncodeToString(h[:])

	if subtle.ConstantTimeCompare([]byte(storedHash), []byte(computedHash)) == 1 {
		return true, nil
	}
	return false, ErrOrderUnauthorized
}

// RefreshOrderPickupPIN validates proof of order ownership, ensures previous PINs are superseded,
// and issues a new pickup PIN for an eligible unpaid order.
func (s *Store) RefreshOrderPickupPIN(ctx context.Context, orderID string, secret string, newPIN PickupPIN, maxActivePerKiosk int) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("refresh order pickup pin: store is closed or nil")
	}
	if ctx == nil {
		return fmt.Errorf("refresh order pickup pin: context is nil")
	}

	// 1. Verify proof of order ownership
	ok, err := s.VerifyOrderOwnership(ctx, orderID, secret)
	if err != nil || !ok {
		return ErrOrderUnauthorized
	}

	// 2. Fetch order to verify status is PENDING_PAYMENT
	order, err := s.GetOrder(ctx, orderID)
	if err != nil || order == nil {
		return ErrPINOrderMismatch
	}
	if order.Status != contracts.JobStatusPendingPayment {
		return ErrPINOrderMismatch
	}

	// 3. Mark previous unredeemed PINs for this order as SUPERSEDED
	_, err = s.db.ExecContext(ctx, s.rebind(`
UPDATE pickup_pins
SET status = 'SUPERSEDED'
WHERE order_id = ? AND status IN ('ACTIVE', 'EXPIRED');
`), orderID)
	if err != nil {
		return fmt.Errorf("refresh order pickup pin: supersede old pins: %w", err)
	}

	// 4. Invalidate any existing active payment sessions for this order
	_, err = s.db.ExecContext(ctx, s.rebind(`
UPDATE payment_sessions
SET status = 'EXPIRED'
WHERE order_id = ? AND status = 'ACTIVE';
`), orderID)
	if err != nil {
		return fmt.Errorf("refresh order pickup pin: expire previous payment sessions: %w", err)
	}

	// 5. Create new pickup PIN
	return s.CreatePickupPIN(ctx, newPIN, maxActivePerKiosk)
}

// RegisterKiosk persists or updates a registered kiosk with its unique secret hash.
func (s *Store) RegisterKiosk(ctx context.Context, kiosk Kiosk) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("register kiosk: store is closed or nil")
	}
	if ctx == nil {
		return fmt.Errorf("register kiosk: context is nil")
	}
	if strings.TrimSpace(kiosk.KioskID) == "" {
		return fmt.Errorf("register kiosk: kioskID cannot be empty")
	}
	if strings.TrimSpace(kiosk.SecretHash) == "" {
		return fmt.Errorf("register kiosk: secretHash cannot be empty")
	}

	now := time.Now().UTC()
	createdNS := kiosk.CreatedAt.UTC().UnixNano()
	if kiosk.CreatedAt.IsZero() {
		createdNS = now.UnixNano()
	}
	updatedNS := kiosk.UpdatedAt.UTC().UnixNano()
	if kiosk.UpdatedAt.IsZero() {
		updatedNS = createdNS
	}
	isActive := 1
	if !kiosk.IsActive {
		isActive = 0
	}

	query := `
INSERT INTO kiosks (kiosk_id, secret_hash, name, is_active, created_at_ns, updated_at_ns)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT(kiosk_id) DO UPDATE SET
    secret_hash = excluded.secret_hash,
    name = excluded.name,
    is_active = excluded.is_active,
    updated_at_ns = excluded.updated_at_ns;
`
	_, err := s.db.ExecContext(ctx, s.rebind(query),
		kiosk.KioskID,
		kiosk.SecretHash,
		kiosk.Name,
		isActive,
		createdNS,
		updatedNS,
	)
	if err != nil {
		return fmt.Errorf("register kiosk: %w", err)
	}
	return nil
}

// GetKiosk fetches the registered kiosk details by kiosk ID.
func (s *Store) GetKiosk(ctx context.Context, kioskID string) (*Kiosk, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("get kiosk: store is closed or nil")
	}
	if ctx == nil {
		return nil, fmt.Errorf("get kiosk: context is nil")
	}
	if strings.TrimSpace(kioskID) == "" {
		return nil, fmt.Errorf("get kiosk: kioskID cannot be empty")
	}

	query := `
SELECT kiosk_id, secret_hash, name, is_active, created_at_ns, updated_at_ns
FROM kiosks
WHERE kiosk_id = ?;
`
	var (
		resID      string
		secretHash string
		name       string
		isActive   int
		createdNS  int64
		updatedNS  int64
	)
	err := s.db.QueryRowContext(ctx, s.rebind(query), kioskID).Scan(
		&resID, &secretHash, &name, &isActive, &createdNS, &updatedNS,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get kiosk: scan: %w", err)
	}

	return &Kiosk{
		KioskID:    resID,
		SecretHash: secretHash,
		Name:       name,
		IsActive:   isActive == 1,
		CreatedAt:  time.Unix(0, createdNS).UTC(),
		UpdatedAt:  time.Unix(0, updatedNS).UTC(),
	}, nil
}

// ValidateKioskSecret verifies the secret against the per-kiosk secret hash using constant-time comparison.
func (s *Store) ValidateKioskSecret(ctx context.Context, kioskID string, rawSecret string) (bool, error) {
	kiosk, err := s.GetKiosk(ctx, kioskID)
	if err != nil {
		return false, err
	}
	if kiosk == nil {
		return false, ErrKioskNotFound
	}
	if !kiosk.IsActive {
		return false, ErrKioskInactive
	}

	h := sha256.Sum256([]byte(rawSecret))
	computedHash := hex.EncodeToString(h[:])

	if subtle.ConstantTimeCompare([]byte(kiosk.SecretHash), []byte(computedHash)) == 1 {
		return true, nil
	}
	if subtle.ConstantTimeCompare([]byte(kiosk.SecretHash), []byte(rawSecret)) == 1 {
		return true, nil
	}
	return false, ErrInvalidKioskSecret
}

// CreatePaymentSession registers a new payment session.
func (s *Store) CreatePaymentSession(ctx context.Context, sess PaymentSession) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("create payment session: store is closed or nil")
	}
	if ctx == nil {
		return fmt.Errorf("create payment session: context is nil")
	}
	if strings.TrimSpace(sess.SessionID) == "" {
		return fmt.Errorf("create payment session: SessionID cannot be empty")
	}
	if strings.TrimSpace(sess.OrderID) == "" {
		return fmt.Errorf("create payment session: OrderID cannot be empty")
	}
	if strings.TrimSpace(sess.KioskID) == "" {
		return fmt.Errorf("create payment session: KioskID cannot be empty")
	}

	now := time.Now().UTC()
	createdNS := sess.CreatedAt.UTC().UnixNano()
	if sess.CreatedAt.IsZero() {
		createdNS = now.UnixNano()
	}
	expiresNS := sess.ExpiresAt.UTC().UnixNano()

	status := string(sess.Status)
	if status == "" {
		status = string(PaymentSessionActive)
	}

	query := `
INSERT INTO payment_sessions (session_id, order_id, kiosk_id, status, created_at_ns, expires_at_ns)
VALUES (?, ?, ?, ?, ?, ?);
`
	_, err := s.db.ExecContext(ctx, s.rebind(query), sess.SessionID, sess.OrderID, sess.KioskID, status, createdNS, expiresNS)
	if err != nil {
		return fmt.Errorf("create payment session: %w", err)
	}
	return nil
}

// GetPaymentSession retrieves a payment session by its ID.
func (s *Store) GetPaymentSession(ctx context.Context, sessionID string) (*PaymentSession, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("get payment session: store is closed or nil")
	}
	if ctx == nil {
		return nil, fmt.Errorf("get payment session: context is nil")
	}
	if strings.TrimSpace(sessionID) == "" {
		return nil, fmt.Errorf("get payment session: sessionID cannot be empty")
	}

	query := `
SELECT session_id, order_id, kiosk_id, status, created_at_ns, expires_at_ns
FROM payment_sessions
WHERE session_id = ?;
`
	var (
		resID     string
		orderID   string
		kioskID   string
		status    string
		createdNS int64
		expiresNS int64
	)
	err := s.db.QueryRowContext(ctx, s.rebind(query), sessionID).Scan(&resID, &orderID, &kioskID, &status, &createdNS, &expiresNS)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get payment session: scan: %w", err)
	}

	return &PaymentSession{
		SessionID: resID,
		OrderID:   orderID,
		KioskID:   kioskID,
		Status:    PaymentSessionStatus(status),
		CreatedAt: time.Unix(0, createdNS).UTC(),
		ExpiresAt: time.Unix(0, expiresNS).UTC(),
	}, nil
}

// UpdatePaymentSessionStatus updates a payment session status.
func (s *Store) UpdatePaymentSessionStatus(ctx context.Context, sessionID string, status PaymentSessionStatus) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("update payment session status: store is closed or nil")
	}
	if ctx == nil {
		return fmt.Errorf("update payment session status: context is nil")
	}

	res, err := s.db.ExecContext(ctx, s.rebind(`UPDATE payment_sessions SET status = ? WHERE session_id = ?;`), string(status), sessionID)
	if err != nil {
		return fmt.Errorf("update payment session status: %w", err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("update payment session status: rows affected: %w", err)
	}
	if rows == 0 {
		return ErrSessionNotFound
	}
	return nil
}

// UpdatePaymentSessionExpiry updates a payment session expiry timestamp.
func (s *Store) UpdatePaymentSessionExpiry(ctx context.Context, sessionID string, expiresAt time.Time) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("update payment session expiry: store is closed or nil")
	}
	if ctx == nil {
		return fmt.Errorf("update payment session expiry: context is nil")
	}

	res, err := s.db.ExecContext(ctx, s.rebind(`UPDATE payment_sessions SET expires_at_ns = ? WHERE session_id = ?;`), expiresAt.UTC().UnixNano(), sessionID)
	if err != nil {
		return fmt.Errorf("update payment session expiry: %w", err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("update payment session expiry: rows affected: %w", err)
	}
	if rows == 0 {
		return ErrSessionNotFound
	}
	return nil
}

// GetActivePaymentSession retrieves an active, unexpired payment session by ID.
// It verifies that status is 'ACTIVE' and expires_at_ns > now.
func (s *Store) GetActivePaymentSession(ctx context.Context, sessionID string, now time.Time) (*PaymentSession, error) {
	sess, err := s.GetPaymentSession(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	if sess == nil {
		return nil, ErrSessionNotFound
	}
	if sess.Status != PaymentSessionActive {
		return nil, ErrSessionExpired
	}
	if !sess.ExpiresAt.After(now.UTC()) {
		return nil, ErrSessionExpired
	}
	return sess, nil
}

// GetActivePaymentSessionByOrder retrieves the active, unexpired payment session bound to an order and kiosk.
func (s *Store) GetActivePaymentSessionByOrder(ctx context.Context, orderID string, kioskID string, now time.Time) (*PaymentSession, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("get active payment session by order: store is closed or nil")
	}
	if ctx == nil {
		return nil, fmt.Errorf("get active payment session by order: context is nil")
	}
	if strings.TrimSpace(orderID) == "" {
		return nil, fmt.Errorf("get active payment session by order: orderID cannot be empty")
	}
	if strings.TrimSpace(kioskID) == "" {
		return nil, fmt.Errorf("get active payment session by order: kioskID cannot be empty")
	}

	nowNS := now.UTC().UnixNano()
	query := `
SELECT session_id, order_id, kiosk_id, status, created_at_ns, expires_at_ns
FROM payment_sessions
WHERE order_id = ? AND kiosk_id = ? AND status = 'ACTIVE' AND expires_at_ns > ?
ORDER BY created_at_ns DESC
LIMIT 1;
`
	var (
		resID     string
		resOrder  string
		resKiosk  string
		status    string
		createdNS int64
		expiresNS int64
	)
	err := s.db.QueryRowContext(ctx, s.rebind(query), orderID, kioskID, nowNS).Scan(
		&resID, &resOrder, &resKiosk, &status, &createdNS, &expiresNS,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get active payment session by order: scan: %w", err)
	}

	return &PaymentSession{
		SessionID: resID,
		OrderID:   resOrder,
		KioskID:   resKiosk,
		Status:    PaymentSessionStatus(status),
		CreatedAt: time.Unix(0, createdNS).UTC(),
		ExpiresAt: time.Unix(0, expiresNS).UTC(),
	}, nil
}

// CreatePaymentAttempt inserts an auditable payment attempt record.
func (s *Store) CreatePaymentAttempt(ctx context.Context, attempt PaymentAttempt) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("create payment attempt: store is closed or nil")
	}
	if ctx == nil {
		return fmt.Errorf("create payment attempt: context is nil")
	}
	if strings.TrimSpace(attempt.ID) == "" {
		return fmt.Errorf("create payment attempt: ID cannot be empty")
	}
	if strings.TrimSpace(attempt.OrderID) == "" {
		return fmt.Errorf("create payment attempt: OrderID cannot be empty")
	}
	if strings.TrimSpace(attempt.SessionID) == "" {
		return fmt.Errorf("create payment attempt: SessionID cannot be empty")
	}
	if strings.TrimSpace(attempt.KioskID) == "" {
		return fmt.Errorf("create payment attempt: KioskID cannot be empty")
	}
	if attempt.AmountPaise <= 0 {
		return fmt.Errorf("create payment attempt: AmountPaise must be > 0, got %d", attempt.AmountPaise)
	}

	now := time.Now().UTC()
	createdNS := attempt.CreatedAt.UTC().UnixNano()
	if attempt.CreatedAt.IsZero() {
		createdNS = now.UnixNano()
	}
	expiresNS := attempt.ExpiresAt.UTC().UnixNano()
	updatedNS := attempt.UpdatedAt.UTC().UnixNano()
	if attempt.UpdatedAt.IsZero() {
		updatedNS = createdNS
	}

	status := string(attempt.Status)
	if status == "" {
		status = string(PaymentAttemptCreated)
	}
	currency := attempt.Currency
	if currency == "" {
		currency = "INR"
	}

	query := `
INSERT INTO payment_attempts (
    id, order_id, session_id, kiosk_id, payment_method,
    razorpay_qr_id, razorpay_order_id, razorpay_payment_id, qr_image_url,
    amount_paise, currency, status, error_reason,
    created_at_ns, expires_at_ns, updated_at_ns
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);
`
	_, err := s.db.ExecContext(ctx, s.rebind(query),
		attempt.ID,
		attempt.OrderID,
		attempt.SessionID,
		attempt.KioskID,
		string(attempt.PaymentMethod),
		attempt.RazorpayQRID,
		attempt.RazorpayOrderID,
		attempt.RazorpayPaymentID,
		attempt.QRImageURL,
		attempt.AmountPaise,
		currency,
		status,
		attempt.ErrorReason,
		createdNS,
		expiresNS,
		updatedNS,
	)
	if err != nil {
		return fmt.Errorf("create payment attempt: %w", err)
	}
	return nil
}

// GetPaymentAttemptByQRID fetches a payment attempt by its Razorpay QR code ID.
func (s *Store) GetPaymentAttemptByQRID(ctx context.Context, qrID string) (*PaymentAttempt, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("get payment attempt by qr id: store is closed or nil")
	}
	if ctx == nil {
		return nil, fmt.Errorf("get payment attempt by qr id: context is nil")
	}
	if strings.TrimSpace(qrID) == "" {
		return nil, fmt.Errorf("get payment attempt by qr id: qrID cannot be empty")
	}

	query := `
SELECT id, order_id, session_id, kiosk_id, payment_method,
       razorpay_qr_id, razorpay_order_id, razorpay_payment_id, qr_image_url,
       amount_paise, currency, status, error_reason,
       created_at_ns, expires_at_ns, updated_at_ns
FROM payment_attempts
WHERE razorpay_qr_id = ?;
`
	return s.scanPaymentAttempt(s.db.QueryRowContext(ctx, s.rebind(query), qrID))
}

// GetPaymentAttemptByPaymentID fetches a payment attempt by its verified Razorpay payment ID.
func (s *Store) GetPaymentAttemptByPaymentID(ctx context.Context, paymentID string) (*PaymentAttempt, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("get payment attempt by payment id: store is closed or nil")
	}
	if ctx == nil {
		return nil, fmt.Errorf("get payment attempt by payment id: context is nil")
	}
	if strings.TrimSpace(paymentID) == "" {
		return nil, fmt.Errorf("get payment attempt by payment id: paymentID cannot be empty")
	}

	query := `
SELECT id, order_id, session_id, kiosk_id, payment_method,
       razorpay_qr_id, razorpay_order_id, razorpay_payment_id, qr_image_url,
       amount_paise, currency, status, error_reason,
       created_at_ns, expires_at_ns, updated_at_ns
FROM payment_attempts
WHERE razorpay_payment_id = ?;
`
	return s.scanPaymentAttempt(s.db.QueryRowContext(ctx, s.rebind(query), paymentID))
}

// GetLatestPaymentAttempt retrieves the most recent payment attempt for an order.
func (s *Store) GetLatestPaymentAttempt(ctx context.Context, orderID string) (*PaymentAttempt, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("get latest payment attempt: store is closed or nil")
	}
	if ctx == nil {
		return nil, fmt.Errorf("get latest payment attempt: context is nil")
	}
	if strings.TrimSpace(orderID) == "" {
		return nil, fmt.Errorf("get latest payment attempt: orderID cannot be empty")
	}

	query := `
SELECT id, order_id, session_id, kiosk_id, payment_method,
       razorpay_qr_id, razorpay_order_id, razorpay_payment_id, qr_image_url,
       amount_paise, currency, status, error_reason,
       created_at_ns, expires_at_ns, updated_at_ns
FROM payment_attempts
WHERE order_id = ?
ORDER BY created_at_ns DESC
LIMIT 1;
`
	return s.scanPaymentAttempt(s.db.QueryRowContext(ctx, s.rebind(query), orderID))
}

// GetActivePaymentAttemptBySession returns the most recent unexpired CREATED payment attempt for a session.
func (s *Store) GetActivePaymentAttemptBySession(ctx context.Context, sessionID string, now time.Time) (*PaymentAttempt, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("get active payment attempt: store is closed or nil")
	}
	if ctx == nil {
		return nil, fmt.Errorf("get active payment attempt: context is nil")
	}
	if strings.TrimSpace(sessionID) == "" {
		return nil, fmt.Errorf("get active payment attempt: sessionID cannot be empty")
	}

	nowNS := now.UTC().UnixNano()
	query := `
SELECT id, order_id, session_id, kiosk_id, payment_method,
       razorpay_qr_id, razorpay_order_id, razorpay_payment_id, qr_image_url,
       amount_paise, currency, status, error_reason,
       created_at_ns, expires_at_ns, updated_at_ns
FROM payment_attempts
WHERE session_id = ? AND status = 'CREATED' AND expires_at_ns > ?
ORDER BY created_at_ns DESC
LIMIT 1;
`
	return s.scanPaymentAttempt(s.db.QueryRowContext(ctx, s.rebind(query), sessionID, nowNS))
}

// GetPaymentAttemptByID fetches a payment attempt by its ID.
func (s *Store) GetPaymentAttemptByID(ctx context.Context, id string) (*PaymentAttempt, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("get payment attempt by id: store is closed or nil")
	}
	if ctx == nil {
		return nil, fmt.Errorf("get payment attempt by id: context is nil")
	}
	if strings.TrimSpace(id) == "" {
		return nil, fmt.Errorf("get payment attempt by id: id cannot be empty")
	}

	query := `
SELECT id, order_id, session_id, kiosk_id, payment_method,
       razorpay_qr_id, razorpay_order_id, razorpay_payment_id, qr_image_url,
       amount_paise, currency, status, error_reason,
       created_at_ns, expires_at_ns, updated_at_ns
FROM payment_attempts
WHERE id = ?;
`
	return s.scanPaymentAttempt(s.db.QueryRowContext(ctx, s.rebind(query), id))
}

func (s *Store) scanPaymentAttempt(row *sql.Row) (*PaymentAttempt, error) {
	var (
		id                string
		orderID           string
		sessionID         string
		kioskID           string
		paymentMethod     string
		razorpayQRID      string
		razorpayOrderID   string
		razorpayPaymentID string
		qrImageURL        string
		amountPaise       int64
		currency          string
		status            string
		errorReason       string
		createdNS         int64
		expiresNS         int64
		updatedNS         int64
	)
	err := row.Scan(
		&id, &orderID, &sessionID, &kioskID, &paymentMethod,
		&razorpayQRID, &razorpayOrderID, &razorpayPaymentID, &qrImageURL,
		&amountPaise, &currency, &status, &errorReason,
		&createdNS, &expiresNS, &updatedNS,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("scan payment attempt: %w", err)
	}

	return &PaymentAttempt{
		ID:                id,
		OrderID:           orderID,
		SessionID:         sessionID,
		KioskID:           kioskID,
		PaymentMethod:     PaymentMethod(paymentMethod),
		RazorpayQRID:      razorpayQRID,
		RazorpayOrderID:   razorpayOrderID,
		RazorpayPaymentID: razorpayPaymentID,
		QRImageURL:        qrImageURL,
		AmountPaise:       amountPaise,
		Currency:          currency,
		Status:            PaymentAttemptStatus(status),
		ErrorReason:       errorReason,
		CreatedAt:         time.Unix(0, createdNS).UTC(),
		ExpiresAt:         time.Unix(0, expiresNS).UTC(),
		UpdatedAt:         time.Unix(0, updatedNS).UTC(),
	}, nil
}

// UpdatePaymentAttemptStatus updates the attempt's status and payment ID.
func (s *Store) UpdatePaymentAttemptStatus(ctx context.Context, id string, status PaymentAttemptStatus, razorpayPaymentID string, errorReason string, now time.Time) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("update payment attempt status: store is closed or nil")
	}
	if ctx == nil {
		return fmt.Errorf("update payment attempt status: context is nil")
	}

	updatedNS := now.UTC().UnixNano()
	query := `
UPDATE payment_attempts
SET status = ?,
    razorpay_payment_id = CASE WHEN ? != '' THEN ? ELSE razorpay_payment_id END,
    error_reason = ?,
    updated_at_ns = ?
WHERE id = ?;
`
	_, err := s.db.ExecContext(ctx, s.rebind(query), string(status), razorpayPaymentID, razorpayPaymentID, errorReason, updatedNS, id)
	if err != nil {
		return fmt.Errorf("update payment attempt status: %w", err)
	}
	return nil
}

// CompletePaymentTx atomically transitions a payment attempt to CAPTURED,
// completes the payment session, updates the order status to PAID_WAITING,
// and marks the webhook event as PROCESSED in a single ACID transaction.
// If any step fails, the entire transaction is rolled back, preventing partial failure.
func (s *Store) CompletePaymentTx(ctx context.Context, attemptID, sessionID, orderID, razorpayPaymentID, eventID string, now time.Time) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("complete payment tx: store is closed or nil")
	}
	if ctx == nil {
		return fmt.Errorf("complete payment tx: context is nil")
	}
	if strings.TrimSpace(orderID) == "" {
		return fmt.Errorf("complete payment tx: orderID cannot be empty")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("complete payment tx: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	nowNS := now.UTC().UnixNano()

	// 1. Update Payment Attempt if present
	if strings.TrimSpace(attemptID) != "" {
		queryAttempt := `
UPDATE payment_attempts
SET status = 'CAPTURED',
    razorpay_payment_id = CASE WHEN ? != '' THEN ? ELSE razorpay_payment_id END,
    updated_at_ns = ?
WHERE id = ?;
`
		if _, err := tx.ExecContext(ctx, s.rebind(queryAttempt), razorpayPaymentID, razorpayPaymentID, nowNS, attemptID); err != nil {
			return fmt.Errorf("complete payment tx: update attempt: %w", err)
		}
	}

	// 2. Complete Payment Session if present
	if strings.TrimSpace(sessionID) != "" {
		querySession := `
UPDATE payment_sessions
SET status = 'COMPLETED'
WHERE session_id = ?;
`
		if _, err := tx.ExecContext(ctx, s.rebind(querySession), sessionID); err != nil {
			return fmt.Errorf("complete payment tx: update session: %w", err)
		}
	}

	// 3. Update Order status strictly to PAID_WAITING
	queryOrder := `
UPDATE orders
SET status = ?, updated_at_ns = ?
WHERE order_id = ?;
`
	if _, err := tx.ExecContext(ctx, s.rebind(queryOrder), string(contracts.JobStatusPaidWaiting), nowNS, orderID); err != nil {
		return fmt.Errorf("complete payment tx: update order: %w", err)
	}

	// 4. Mark Webhook Event as PROCESSED if present
	if strings.TrimSpace(eventID) != "" {
		queryEvent := `
UPDATE payment_webhook_events
SET status = 'PROCESSED', processed_at_ns = ?
WHERE event_id = ?;
`
		if _, err := tx.ExecContext(ctx, s.rebind(queryEvent), nowNS, eventID); err != nil {
			return fmt.Errorf("complete payment tx: update webhook event: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("complete payment tx: commit: %w", err)
	}
	return nil
}

// RecordWebhookEvent records an incoming webhook event by its unique Razorpay event ID.
// Returns (true, nil) if recorded for the first time, (false, nil) if duplicate.
func (s *Store) RecordWebhookEvent(ctx context.Context, eventID string, eventType string, payloadHash string, orderID string) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("record webhook event: store is closed or nil")
	}
	if ctx == nil {
		return false, fmt.Errorf("record webhook event: context is nil")
	}
	if strings.TrimSpace(eventID) == "" {
		return false, fmt.Errorf("record webhook event: eventID cannot be empty")
	}

	receivedNS := time.Now().UTC().UnixNano()
	query := `
INSERT INTO payment_webhook_events (event_id, event_type, payload_hash, order_id, status, received_at_ns, processed_at_ns)
VALUES (?, ?, ?, ?, 'RECEIVED', ?, 0)
ON CONFLICT(event_id) DO NOTHING;
`
	res, err := s.db.ExecContext(ctx, s.rebind(query), eventID, eventType, payloadHash, orderID, receivedNS)
	if err != nil {
		return false, fmt.Errorf("record webhook event: %w", err)
	}

	rows, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("record webhook event: check rows affected: %w", err)
	}
	return rows == 1, nil
}

// UpdateWebhookEventStatus updates the processing status and timestamp for a webhook event.
func (s *Store) UpdateWebhookEventStatus(ctx context.Context, eventID string, status WebhookEventStatus, now time.Time) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("update webhook event status: store is closed or nil")
	}
	if ctx == nil {
		return fmt.Errorf("update webhook event status: context is nil")
	}

	processedNS := now.UTC().UnixNano()
	query := `
UPDATE payment_webhook_events
SET status = ?, processed_at_ns = ?
WHERE event_id = ?;
`
	_, err := s.db.ExecContext(ctx, s.rebind(query), string(status), processedNS, eventID)
	if err != nil {
		return fmt.Errorf("update webhook event status: %w", err)
	}
	return nil
}

// GetWebhookEvent retrieves a webhook event by its event ID.
func (s *Store) GetWebhookEvent(ctx context.Context, eventID string) (*WebhookEvent, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("get webhook event: store is closed or nil")
	}
	if ctx == nil {
		return nil, fmt.Errorf("get webhook event: context is nil")
	}

	query := `
SELECT event_id, event_type, payload_hash, order_id, status, received_at_ns, processed_at_ns
FROM payment_webhook_events
WHERE event_id = ?;
`
	var (
		resID       string
		eventType   string
		payloadHash string
		orderID     string
		status      string
		receivedNS  int64
		processedNS int64
	)
	err := s.db.QueryRowContext(ctx, s.rebind(query), eventID).Scan(
		&resID, &eventType, &payloadHash, &orderID, &status, &receivedNS, &processedNS,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get webhook event: %w", err)
	}

	var processedAt time.Time
	if processedNS > 0 {
		processedAt = time.Unix(0, processedNS).UTC()
	}

	return &WebhookEvent{
		EventID:     resID,
		EventType:   eventType,
		PayloadHash: payloadHash,
		OrderID:     orderID,
		Status:      WebhookEventStatus(status),
		ReceivedAt:  time.Unix(0, receivedNS).UTC(),
		ProcessedAt: processedAt,
	}, nil
}

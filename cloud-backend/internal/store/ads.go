package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"xeroxgo/pkg/contracts"
)

// hashPassword produces a salted SHA-256 hash using 100,000 iterations: $sha256$iterations$salt$hash
func hashPassword(password string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generate salt: %w", err)
	}
	saltHex := hex.EncodeToString(salt)
	hash := computeIteratedHash(password, saltHex, 100000)
	return fmt.Sprintf("$sha256$100000$%s$%s", saltHex, hash), nil
}

func computeIteratedHash(password, salt string, iterations int) string {
	h := sha256.New()
	h.Write([]byte(salt + ":" + password))
	digest := h.Sum(nil)
	for i := 1; i < iterations; i++ {
		h.Reset()
		h.Write(digest)
		digest = h.Sum(nil)
	}
	return hex.EncodeToString(digest)
}

func verifyPassword(password, storedHash string) bool {
	parts := strings.Split(storedHash, "$")
	if len(parts) != 5 || parts[1] != "sha256" {
		return false
	}
	var iterations int
	if _, err := fmt.Sscanf(parts[2], "%d", &iterations); err != nil || iterations < 1 {
		return false
	}
	salt := parts[3]
	expectedHash := parts[4]
	actualHash := computeIteratedHash(password, salt, iterations)
	return subtle.ConstantTimeCompare([]byte(actualHash), []byte(expectedHash)) == 1
}

func hashToken(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

// systemDefaultAds defines the immutable 3 standard XeroxGO promo cards.
var systemDefaultAds = []contracts.AdItem{
	{
		ID:          "ad-quick-easy",
		Title:       "Quick & Easy",
		Description: "Direct UPI payment. No coins, no cash needed.",
		Type:        contracts.AdTypeGradient,
		GradientC0:  "#1E3A8A",
		GradientC1:  "#2563EB",
		GradientC2:  "#4F46E5",
		SortOrder:   1,
		IsActive:    true,
	},
	{
		ID:          "ad-high-quality",
		Title:       "High Quality Prints",
		Description: "Crisp laser documents ready in seconds.",
		Type:        contracts.AdTypeGradient,
		GradientC0:  "#065F46",
		GradientC1:  "#10B981",
		GradientC2:  "#059669",
		SortOrder:   2,
		IsActive:    true,
	},
	{
		ID:          "ad-skip-queue",
		Title:       "Skip The Queue",
		Description: "Pick up your print job with your 4-digit code.",
		Type:        contracts.AdTypeGradient,
		GradientC0:  "#581C87",
		GradientC1:  "#7C3AED",
		GradientC2:  "#D97706",
		SortOrder:   3,
		IsActive:    true,
	},
}

// GetSystemDefaultAds returns a copy of the 3 immutable system default promo cards.
func GetSystemDefaultAds() []contracts.AdItem {
	now := time.Now().UTC()
	res := make([]contracts.AdItem, len(systemDefaultAds))
	for i, ad := range systemDefaultAds {
		ad.UpdatedAt = now
		res[i] = ad
	}
	return res
}

func isSystemDefaultAdID(id string) bool {
	return id == "ad-quick-easy" || id == "ad-high-quality" || id == "ad-skip-queue"
}

// SeedDefaultAds cleans up legacy seeded default ad IDs from the custom ads table.
// Default ads are now permanent system fallbacks and not stored as custom ads.
func (s *Store) SeedDefaultAds(ctx context.Context) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("seed ads: store is closed or nil")
	}

	q := s.rebind("DELETE FROM ads WHERE id IN ('ad-quick-easy', 'ad-high-quality', 'ad-skip-queue')")
	_, err := s.db.ExecContext(ctx, q)
	return err
}

// BootstrapAdmin creates the initial admin user if none exists.
func (s *Store) BootstrapAdmin(ctx context.Context, username, initialPassword string) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("bootstrap admin: store is closed or nil")
	}
	if strings.TrimSpace(username) == "" || len(initialPassword) < 6 {
		return fmt.Errorf("bootstrap admin: username and password (min 6 chars) required")
	}

	var count int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM admin_users").Scan(&count); err != nil {
		return fmt.Errorf("bootstrap admin: count users: %w", err)
	}
	if count > 0 {
		return nil
	}

	pwdHash, err := hashPassword(initialPassword)
	if err != nil {
		return fmt.Errorf("bootstrap admin: hash password: %w", err)
	}

	nowNs := time.Now().UTC().UnixNano()
	q := s.rebind("INSERT INTO admin_users (username, password_hash, created_at_ns, updated_at_ns) VALUES (?, ?, ?, ?) ON CONFLICT(username) DO NOTHING")
	if _, err := s.db.ExecContext(ctx, q, strings.TrimSpace(username), pwdHash, nowNs, nowNs); err != nil {
		return fmt.Errorf("bootstrap admin: insert: %w", err)
	}
	return nil
}

// AuthenticateAdmin checks the given credentials against admin_users.
func (s *Store) AuthenticateAdmin(ctx context.Context, username, password string) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("authenticate admin: store is closed or nil")
	}

	var storedHash string
	q := s.rebind("SELECT password_hash FROM admin_users WHERE username = ?")
	err := s.db.QueryRowContext(ctx, q, strings.TrimSpace(username)).Scan(&storedHash)
	if err != nil {
		if err == sql.ErrNoRows {
			return false, nil
		}
		return false, fmt.Errorf("authenticate admin: query: %w", err)
	}

	return verifyPassword(password, storedHash), nil
}

// UpdateAdminPassword updates password for an existing admin after verifying the old password.
func (s *Store) UpdateAdminPassword(ctx context.Context, username, oldPassword, newPassword string) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("update password: store is closed or nil")
	}
	if len(newPassword) < 8 {
		return fmt.Errorf("update password: new password must be at least 8 characters")
	}

	ok, err := s.AuthenticateAdmin(ctx, username, oldPassword)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("invalid current password")
	}

	newHash, err := hashPassword(newPassword)
	if err != nil {
		return fmt.Errorf("update password: hash: %w", err)
	}

	nowNs := time.Now().UTC().UnixNano()
	q := s.rebind("UPDATE admin_users SET password_hash = ?, updated_at_ns = ? WHERE username = ?")
	if _, err := s.db.ExecContext(ctx, q, newHash, nowNs, username); err != nil {
		return fmt.Errorf("update password: %w", err)
	}

	// Invalidate all existing sessions for this user upon password change
	revokeQ := s.rebind("DELETE FROM admin_sessions WHERE username = ?")
	_, err = s.db.ExecContext(ctx, revokeQ, username)
	return err
}

// CreateAdminSession creates a cryptographically secure 24-hour session token for an authenticated admin.
func (s *Store) CreateAdminSession(ctx context.Context, username string) (string, time.Time, error) {
	if s == nil || s.db == nil {
		return "", time.Time{}, fmt.Errorf("create session: store is closed or nil")
	}

	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		return "", time.Time{}, fmt.Errorf("create session: rand: %w", err)
	}
	rawToken := hex.EncodeToString(tokenBytes)
	tokenH := hashToken(rawToken)

	now := time.Now().UTC()
	expiresAt := now.Add(24 * time.Hour)
	nowNs := now.UnixNano()
	expiresNs := expiresAt.UnixNano()

	q := s.rebind("INSERT INTO admin_sessions (token_hash, username, expires_at_ns, created_at_ns) VALUES (?, ?, ?, ?)")
	if _, err := s.db.ExecContext(ctx, q, tokenH, username, expiresNs, nowNs); err != nil {
		return "", time.Time{}, fmt.Errorf("create session: insert: %w", err)
	}

	return rawToken, expiresAt, nil
}

// ValidateAdminSession verifies a raw session token and returns the username if valid.
func (s *Store) ValidateAdminSession(ctx context.Context, rawToken string) (string, bool) {
	if s == nil || s.db == nil || strings.TrimSpace(rawToken) == "" {
		return "", false
	}

	tokenH := hashToken(rawToken)
	nowNs := time.Now().UTC().UnixNano()

	var username string
	q := s.rebind("SELECT username FROM admin_sessions WHERE token_hash = ? AND expires_at_ns > ?")
	err := s.db.QueryRowContext(ctx, q, tokenH, nowNs).Scan(&username)
	if err != nil {
		return "", false
	}
	return username, true
}

// RevokeAdminSession deletes a session token upon logout.
func (s *Store) RevokeAdminSession(ctx context.Context, rawToken string) error {
	if s == nil || s.db == nil || strings.TrimSpace(rawToken) == "" {
		return nil
	}
	tokenH := hashToken(rawToken)
	q := s.rebind("DELETE FROM admin_sessions WHERE token_hash = ?")
	_, err := s.db.ExecContext(ctx, q, tokenH)
	return err
}

// GetActiveAds returns all currently active custom ads.
// If no custom ads are active, it falls back to the 3 immutable system default cards.
func (s *Store) GetActiveAds(ctx context.Context) ([]contracts.AdItem, error) {
	customAds, err := s.fetchAdsQuery(ctx, "SELECT id, title, description, ad_type, image_url, gradient_c0, gradient_c1, gradient_c2, link_url, sort_order, is_active, updated_at_ns FROM ads WHERE is_active = 1 AND id NOT IN ('ad-quick-easy', 'ad-high-quality', 'ad-skip-queue') ORDER BY sort_order ASC, created_at_ns ASC")
	if err != nil {
		return nil, err
	}
	if len(customAds) == 0 {
		return GetSystemDefaultAds(), nil
	}
	return customAds, nil
}

// GetAllAds returns all user-configured custom ads, excluding system default cards.
func (s *Store) GetAllAds(ctx context.Context) ([]contracts.AdItem, error) {
	return s.fetchAdsQuery(ctx, "SELECT id, title, description, ad_type, image_url, gradient_c0, gradient_c1, gradient_c2, link_url, sort_order, is_active, updated_at_ns FROM ads WHERE id NOT IN ('ad-quick-easy', 'ad-high-quality', 'ad-skip-queue') ORDER BY sort_order ASC, created_at_ns ASC")
}

func (s *Store) fetchAdsQuery(ctx context.Context, q string) ([]contracts.AdItem, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("fetch ads: store is closed or nil")
	}

	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("fetch ads: %w", err)
	}
	defer rows.Close()

	items := []contracts.AdItem{}
	for rows.Next() {
		var (
			it        contracts.AdItem
			adTypeStr string
			isActive  int
			updatedNs int64
		)
		err := rows.Scan(
			&it.ID,
			&it.Title,
			&it.Description,
			&adTypeStr,
			&it.ImageURL,
			&it.GradientC0,
			&it.GradientC1,
			&it.GradientC2,
			&it.LinkURL,
			&it.SortOrder,
			&isActive,
			&updatedNs,
		)
		if err != nil {
			return nil, fmt.Errorf("fetch ads scan: %w", err)
		}
		it.Type = contracts.AdType(adTypeStr)
		it.IsActive = (isActive == 1)
		it.UpdatedAt = time.Unix(0, updatedNs).UTC()
		items = append(items, it)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("fetch ads rows: %w", err)
	}

	return items, nil
}

// SaveAd creates or updates an ad item.
func (s *Store) SaveAd(ctx context.Context, ad contracts.AdItem) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("save ad: store is closed or nil")
	}
	if strings.TrimSpace(ad.ID) == "" {
		return fmt.Errorf("save ad: id cannot be empty")
	}
	if isSystemDefaultAdID(ad.ID) {
		return fmt.Errorf("system default card IDs are reserved and cannot be modified")
	}
	if strings.TrimSpace(ad.Title) == "" {
		return fmt.Errorf("save ad: title cannot be empty")
	}

	if ad.Type == "" {
		ad.Type = contracts.AdTypeGradient
	}
	if ad.GradientC0 == "" {
		ad.GradientC0 = "#1E3A8A"
	}
	if ad.GradientC1 == "" {
		ad.GradientC1 = "#2563EB"
	}
	if ad.GradientC2 == "" {
		ad.GradientC2 = "#4F46E5"
	}

	nowNs := time.Now().UTC().UnixNano()
	activeInt := 0
	if ad.IsActive {
		activeInt = 1
	}

	// Use standard SQLite/Postgres UPSERT syntax
	q := s.rebind(`INSERT INTO ads (
        id, title, description, ad_type, image_url,
        gradient_c0, gradient_c1, gradient_c2, link_url,
        sort_order, is_active, created_at_ns, updated_at_ns
    ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
    ON CONFLICT(id) DO UPDATE SET
        title = excluded.title,
        description = excluded.description,
        ad_type = excluded.ad_type,
        image_url = excluded.image_url,
        gradient_c0 = excluded.gradient_c0,
        gradient_c1 = excluded.gradient_c1,
        gradient_c2 = excluded.gradient_c2,
        link_url = excluded.link_url,
        sort_order = excluded.sort_order,
        is_active = excluded.is_active,
        updated_at_ns = excluded.updated_at_ns;`)

	_, err := s.db.ExecContext(
		ctx,
		q,
		ad.ID,
		ad.Title,
		ad.Description,
		string(ad.Type),
		ad.ImageURL,
		ad.GradientC0,
		ad.GradientC1,
		ad.GradientC2,
		ad.LinkURL,
		ad.SortOrder,
		activeInt,
		nowNs,
		nowNs,
	)
	if err != nil {
		return fmt.Errorf("save ad: %w", err)
	}
	return nil
}

// DeleteAd deletes an ad by ID.
func (s *Store) DeleteAd(ctx context.Context, id string) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("delete ad: store is closed or nil")
	}
	if isSystemDefaultAdID(id) {
		return fmt.Errorf("system default cards are permanent and cannot be deleted")
	}
	q := s.rebind("DELETE FROM ads WHERE id = ?")
	_, err := s.db.ExecContext(ctx, q, id)
	return err
}

// ReorderAds updates sort_order for multiple ads sequentially in a single transaction.
func (s *Store) ReorderAds(ctx context.Context, orderedIDs []string) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("reorder ads: store is closed or nil")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("reorder ads: begin tx: %w", err)
	}
	defer tx.Rollback()

	nowNs := time.Now().UTC().UnixNano()
	q := s.rebind("UPDATE ads SET sort_order = ?, updated_at_ns = ? WHERE id = ?")
	for i, id := range orderedIDs {
		if _, err := tx.ExecContext(ctx, q, i+1, nowNs, id); err != nil {
			return fmt.Errorf("reorder ads: exec %s: %w", id, err)
		}
	}

	return tx.Commit()
}

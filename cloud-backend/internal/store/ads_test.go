package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"xeroxgo/pkg/contracts"
)

func TestAdsAndAdminAuth(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test_ads.db")

	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer s.Close()

	// 1. Test SeedDefaultAds
	if err := s.SeedDefaultAds(ctx); err != nil {
		t.Fatalf("SeedDefaultAds failed: %v", err)
	}
	ads, err := s.GetActiveAds(ctx)
	if err != nil {
		t.Fatalf("GetActiveAds failed: %v", err)
	}
	if len(ads) != 3 {
		t.Fatalf("expected 3 default ads, got %d", len(ads))
	}
	if ads[0].Title != "Quick & Easy" {
		t.Errorf("expected first ad to be 'Quick & Easy', got %q", ads[0].Title)
	}

	// 2. Test BootstrapAdmin & AuthenticateAdmin
	if err := s.BootstrapAdmin(ctx, "admin", "secret123"); err != nil {
		t.Fatalf("BootstrapAdmin failed: %v", err)
	}
	ok, err := s.AuthenticateAdmin(ctx, "admin", "secret123")
	if err != nil || !ok {
		t.Fatalf("AuthenticateAdmin with correct password failed: ok=%v, err=%v", ok, err)
	}
	badOk, err := s.AuthenticateAdmin(ctx, "admin", "wrongpassword")
	if err != nil || badOk {
		t.Fatalf("AuthenticateAdmin with wrong password should fail: ok=%v, err=%v", badOk, err)
	}

	// 3. Test Sessions
	token, expiresAt, err := s.CreateAdminSession(ctx, "admin")
	if err != nil || token == "" {
		t.Fatalf("CreateAdminSession failed: %v", err)
	}
	if expiresAt.Before(time.Now()) {
		t.Fatalf("session already expired")
	}

	user, valid := s.ValidateAdminSession(ctx, token)
	if !valid || user != "admin" {
		t.Fatalf("ValidateAdminSession failed: valid=%v, user=%s", valid, user)
	}

	if err := s.RevokeAdminSession(ctx, token); err != nil {
		t.Fatalf("RevokeAdminSession failed: %v", err)
	}
	_, afterRevoke := s.ValidateAdminSession(ctx, token)
	if afterRevoke {
		t.Fatalf("token should be invalid after revoke")
	}

	// 4. Test SaveAd & Reorder & Fallback Behavior
	// Before any custom ad is added, GetAllAds must be 0 and GetActiveAds must be the 3 defaults.
	allAdsBefore, err := s.GetAllAds(ctx)
	if err != nil {
		t.Fatalf("GetAllAds failed: %v", err)
	}
	if len(allAdsBefore) != 0 {
		t.Fatalf("expected 0 custom ads before save, got %d", len(allAdsBefore))
	}

	newAd1 := contracts.AdItem{
		ID:          "ad-custom-1",
		Title:       "Special Event",
		Description: "Free printing day!",
		Type:        contracts.AdTypeGradient,
		GradientC0:  "#FF0000",
		GradientC1:  "#00FF00",
		GradientC2:  "#0000FF",
		SortOrder:   1,
		IsActive:    true,
	}
	if err := s.SaveAd(ctx, newAd1); err != nil {
		t.Fatalf("SaveAd failed: %v", err)
	}

	newAd2 := contracts.AdItem{
		ID:          "ad-custom-2",
		Title:       "Campus Fest",
		Description: "Discounts all week!",
		Type:        contracts.AdTypeGradient,
		GradientC0:  "#111111",
		GradientC1:  "#222222",
		GradientC2:  "#333333",
		SortOrder:   2,
		IsActive:    true,
	}
	if err := s.SaveAd(ctx, newAd2); err != nil {
		t.Fatalf("SaveAd 2 failed: %v", err)
	}

	allAds, err := s.GetAllAds(ctx)
	if err != nil {
		t.Fatalf("GetAllAds failed: %v", err)
	}
	if len(allAds) != 2 {
		t.Fatalf("expected 2 custom ads, got %d", len(allAds))
	}

	// Active ads must now be strictly the custom ads; default cards have disappeared.
	activeWithCustom, err := s.GetActiveAds(ctx)
	if err != nil {
		t.Fatalf("GetActiveAds failed: %v", err)
	}
	if len(activeWithCustom) != 2 {
		t.Fatalf("expected 2 active custom ads (defaults disappeared), got %d", len(activeWithCustom))
	}

	// Reorder
	if err := s.ReorderAds(ctx, []string{"ad-custom-2", "ad-custom-1"}); err != nil {
		t.Fatalf("ReorderAds failed: %v", err)
	}
	reordered, err := s.GetActiveAds(ctx)
	if err != nil {
		t.Fatalf("GetActiveAds failed: %v", err)
	}
	if reordered[0].ID != "ad-custom-2" {
		t.Errorf("expected first ad after reorder to be 'ad-custom-2', got %s", reordered[0].ID)
	}

	// Attempting to delete default card must fail
	if err := s.DeleteAd(ctx, "ad-quick-easy"); err == nil {
		t.Fatalf("DeleteAd on default card should fail")
	}

	// Delete both custom ads
	if err := s.DeleteAd(ctx, "ad-custom-1"); err != nil {
		t.Fatalf("DeleteAd custom 1 failed: %v", err)
	}
	if err := s.DeleteAd(ctx, "ad-custom-2"); err != nil {
		t.Fatalf("DeleteAd custom 2 failed: %v", err)
	}

	// When all custom ads are deleted, GetActiveAds must immediately fall back to the 3 defaults!
	fallbackAds, err := s.GetActiveAds(ctx)
	if err != nil {
		t.Fatalf("GetActiveAds after delete failed: %v", err)
	}
	if len(fallbackAds) != 3 {
		t.Fatalf("expected fallback to 3 default ads, got %d", len(fallbackAds))
	}
	if fallbackAds[0].Title != "Quick & Easy" {
		t.Errorf("expected fallback first ad to be 'Quick & Easy', got %q", fallbackAds[0].Title)
	}

	// Delete
	if err := s.DeleteAd(ctx, "ad-custom-1"); err != nil {
		t.Fatalf("DeleteAd failed: %v", err)
	}
	afterDelete, err := s.GetActiveAds(ctx)
	if err != nil {
		t.Fatalf("GetActiveAds failed: %v", err)
	}
	if len(afterDelete) != 3 {
		t.Fatalf("expected 3 ads after delete, got %d", len(afterDelete))
	}
}

func TestUpdateAdminPasswordRevokesSessions(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test_auth.db")

	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer s.Close()

	if err := s.BootstrapAdmin(ctx, "admin", "initialpwd1"); err != nil {
		t.Fatalf("BootstrapAdmin failed: %v", err)
	}

	// Create session
	token, _, err := s.CreateAdminSession(ctx, "admin")
	if err != nil {
		t.Fatalf("CreateAdminSession failed: %v", err)
	}
	if user, ok := s.ValidateAdminSession(ctx, token); !ok || user != "admin" {
		t.Fatalf("session should be valid initially")
	}

	// Password < 8 characters should fail
	if err := s.UpdateAdminPassword(ctx, "admin", "initialpwd1", "short"); err == nil {
		t.Fatalf("expected error for password < 8 characters")
	}

	// Update password with valid length
	if err := s.UpdateAdminPassword(ctx, "admin", "initialpwd1", "newstrongpwd123"); err != nil {
		t.Fatalf("UpdateAdminPassword failed: %v", err)
	}

	// Session must now be revoked
	if _, ok := s.ValidateAdminSession(ctx, token); ok {
		t.Fatalf("session should be revoked after password update")
	}

	// Old password must fail
	oldOk, err := s.AuthenticateAdmin(ctx, "admin", "initialpwd1")
	if err != nil || oldOk {
		t.Fatalf("old password should fail")
	}

	// New password must succeed
	newOk, err := s.AuthenticateAdmin(ctx, "admin", "newstrongpwd123")
	if err != nil || !newOk {
		t.Fatalf("new password should succeed")
	}
}


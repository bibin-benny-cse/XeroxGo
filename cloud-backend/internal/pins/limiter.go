package pins

import (
	"sync"
	"time"
)

// CooldownConfig defines progressive cooldown parameters.
type CooldownConfig struct {
	GraceAttempts  int           // Number of failed attempts before progressive cooldown begins (default: 3)
	WindowDuration time.Duration // Time after which inactive attempt counters are cleared (default: 10m)
	MaxCooldown    time.Duration // Maximum cooldown duration (default: 60s, avoids permanent lockout)
	MaxEntries     int           // Maximum tracked client records before forced eviction (default: 10000)
}

// DefaultCooldownConfig returns safe production defaults.
func DefaultCooldownConfig() CooldownConfig {
	return CooldownConfig{
		GraceAttempts:  3,
		WindowDuration: 10 * time.Minute,
		MaxCooldown:    60 * time.Second,
		MaxEntries:     10000,
	}
}

type attemptRecord struct {
	consecutiveFailures int
	cooldownUntil       time.Time
	lastAttemptAt       time.Time
}

// RateLimiter manages thread-safe per-client and per-kiosk progressive cooldowns for PIN redemption attempts.
//
// NOTE: RateLimiter is an instance-local, in-memory rate limiter designed for single-process deployments.
// Attempt counters and cooldown states are held entirely in application memory; they are not synchronized
// across multi-instance clusters and will reset upon process restart. For multi-replica cloud deployments,
// backing rate limits with a shared Redis or transactional store is recommended.
type RateLimiter struct {
	mu     sync.Mutex
	cfg    CooldownConfig
	client map[string]*attemptRecord
	kiosk  map[string]*attemptRecord
}

// NewRateLimiter instantiates a progressive cooldown rate limiter.
func NewRateLimiter(cfg CooldownConfig) *RateLimiter {
	if cfg.GraceAttempts <= 0 {
		cfg.GraceAttempts = 3
	}
	if cfg.WindowDuration <= 0 {
		cfg.WindowDuration = 10 * time.Minute
	}
	if cfg.MaxCooldown <= 0 {
		cfg.MaxCooldown = 60 * time.Second
	}
	if cfg.MaxEntries <= 0 {
		cfg.MaxEntries = 10000
	}
	return &RateLimiter{
		cfg:    cfg,
		client: make(map[string]*attemptRecord),
		kiosk:  make(map[string]*attemptRecord),
	}
}

// Check evaluates whether an attempt is permitted right now.
// If currently under cooldown, it returns (false, remainingDuration).
// Otherwise, it returns (true, 0).
func (rl *RateLimiter) Check(kioskID, clientIP string, now time.Time) (bool, time.Duration) {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	clientKey := kioskID + "|" + clientIP

	// 1. Check client-specific record first
	if rec, ok := rl.client[clientKey]; ok {
		if now.Before(rec.cooldownUntil) {
			return false, rec.cooldownUntil.Sub(now)
		}
	}

	// 2. Check kiosk-aggregate record
	if rec, ok := rl.kiosk[kioskID]; ok {
		if now.Before(rec.cooldownUntil) {
			return false, rec.cooldownUntil.Sub(now)
		}
	}

	return true, 0
}

// RecordFailure increments failed attempt counters and applies progressive cooldowns.
// Returns the cooldown duration enforced for this failure.
func (rl *RateLimiter) RecordFailure(kioskID, clientIP string, now time.Time) time.Duration {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	clientKey := kioskID + "|" + clientIP

	// Evict expired or oldest records if capacity limit is reached
	if len(rl.client) >= rl.cfg.MaxEntries {
		rl.evictExpiredOrOldest(now)
	}

	// Update client-level record
	rec, ok := rl.client[clientKey]
	if !ok || now.Sub(rec.lastAttemptAt) > rl.cfg.WindowDuration {
		rec = &attemptRecord{}
		rl.client[clientKey] = rec
	}
	rec.consecutiveFailures++
	rec.lastAttemptAt = now

	cooldown := rl.calculateCooldown(rec.consecutiveFailures)
	if cooldown > 0 {
		rec.cooldownUntil = now.Add(cooldown)
	}

	// Update kiosk-level aggregate record with a higher threshold (grace * 4) to protect
	// against physical brute force while preventing a single source from easily blocking other sources.
	kRec, kOk := rl.kiosk[kioskID]
	if !kOk || now.Sub(kRec.lastAttemptAt) > rl.cfg.WindowDuration {
		kRec = &attemptRecord{}
		rl.kiosk[kioskID] = kRec
	}
	kRec.consecutiveFailures++
	kRec.lastAttemptAt = now
	if kRec.consecutiveFailures > rl.cfg.GraceAttempts*4 {
		kCooldown := rl.calculateCooldown(kRec.consecutiveFailures - rl.cfg.GraceAttempts*3)
		if kCooldown > 0 {
			kRec.cooldownUntil = now.Add(kCooldown)
		}
	}

	return cooldown
}

// RecordSuccess resets the consecutive failures and cooldowns for the successful client.
func (rl *RateLimiter) RecordSuccess(kioskID, clientIP string) {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	clientKey := kioskID + "|" + clientIP
	delete(rl.client, clientKey)

	// Soften kiosk aggregate on successful redemption
	if kRec, ok := rl.kiosk[kioskID]; ok {
		kRec.consecutiveFailures = 0
		kRec.cooldownUntil = time.Time{}
	}
}

// Cleanup removes expired records that are past both WindowDuration and cooldown.
func (rl *RateLimiter) Cleanup(now time.Time) int {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	return rl.cleanupInternal(now)
}

func (rl *RateLimiter) cleanupInternal(now time.Time) int {
	removed := 0
	for key, rec := range rl.client {
		if now.Sub(rec.lastAttemptAt) > rl.cfg.WindowDuration && !now.Before(rec.cooldownUntil) {
			delete(rl.client, key)
			removed++
		}
	}
	for key, rec := range rl.kiosk {
		if now.Sub(rec.lastAttemptAt) > rl.cfg.WindowDuration && !now.Before(rec.cooldownUntil) {
			delete(rl.kiosk, key)
			removed++
		}
	}
	return removed
}

func (rl *RateLimiter) evictExpiredOrOldest(now time.Time) {
	// First pass: evict expired
	if cleaned := rl.cleanupInternal(now); cleaned > 0 {
		return
	}

	// Second pass: if still full, evict oldest entry
	var oldestKey string
	var oldestTime time.Time
	first := true
	for key, rec := range rl.client {
		if first || rec.lastAttemptAt.Before(oldestTime) {
			oldestKey = key
			oldestTime = rec.lastAttemptAt
			first = false
		}
	}
	if oldestKey != "" {
		delete(rl.client, oldestKey)
	}
}

// EntryCount returns the current count of active client tracking entries (for testing).
func (rl *RateLimiter) EntryCount() int {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	return len(rl.client)
}

// Reset clears state for testing or administrative actions.
func (rl *RateLimiter) Reset() {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	rl.client = make(map[string]*attemptRecord)
	rl.kiosk = make(map[string]*attemptRecord)
}

// calculateCooldown returns progressive duration based on consecutive failures:
// 1..(grace-1): 0s
// grace: 3s
// grace+1: 5s
// grace+2: 15s
// grace+3: 30s
// grace+4+: 60s (capped at MaxCooldown)
func (rl *RateLimiter) calculateCooldown(failures int) time.Duration {
	excess := failures - rl.cfg.GraceAttempts + 1
	if excess <= 0 {
		return 0
	}
	var cd time.Duration
	switch excess {
	case 1:
		cd = 3 * time.Second
	case 2:
		cd = 5 * time.Second
	case 3:
		cd = 15 * time.Second
	case 4:
		cd = 30 * time.Second
	default:
		cd = rl.cfg.MaxCooldown
	}
	if cd > rl.cfg.MaxCooldown {
		cd = rl.cfg.MaxCooldown
	}
	return cd
}

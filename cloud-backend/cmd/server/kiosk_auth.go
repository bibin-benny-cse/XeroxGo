package main

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"

	"xeroxgo/cloud-backend/internal/store"
)

// authenticateKiosk validates independent kiosk credentials from HTTP headers against the database.
// It strictly rejects unauthenticated, unknown, or inactive kiosks, and NEVER trusts any kiosk_id
// supplied in request bodies or query parameters.
func (s *appServer) authenticateKiosk(w http.ResponseWriter, r *http.Request) (string, error) {
	kioskID := strings.TrimSpace(r.Header.Get("X-Kiosk-ID"))
	if kioskID == "" {
		kioskID = strings.TrimSpace(r.Header.Get("X-Machine-ID"))
	}

	secret := strings.TrimSpace(r.Header.Get("X-Kiosk-Secret"))
	if secret == "" {
		authHeader := strings.TrimSpace(r.Header.Get("Authorization"))
		if strings.HasPrefix(strings.ToLower(authHeader), "bearer ") {
			secret = strings.TrimSpace(authHeader[7:])
		}
	}

	if kioskID == "" || secret == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error": "unauthorized: missing kiosk credentials (X-Kiosk-ID and X-Kiosk-Secret required)",
		})
		return "", errors.New("missing kiosk credentials")
	}

	ok, err := s.db.ValidateKioskSecret(r.Context(), kioskID, secret)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		if errors.Is(err, store.ErrKioskNotFound) {
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"error": "unauthorized: unknown kiosk",
			})
			return "", err
		}
		if errors.Is(err, store.ErrKioskInactive) {
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"error": "forbidden: kiosk is inactive",
			})
			return "", err
		}
		if errors.Is(err, store.ErrInvalidKioskSecret) {
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"error": "unauthorized: invalid kiosk secret",
			})
			return "", err
		}

		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error": "internal authentication error",
		})
		return "", err
	}

	if !ok {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error": "unauthorized: invalid kiosk credentials",
		})
		return "", errors.New("invalid kiosk credentials")
	}

	return kioskID, nil
}

// extractClientIP retrieves the client IP for rate-limiting purposes.
// Forwarded headers (X-Forwarded-For, X-Real-IP) are ONLY trusted if the direct peer
// (RemoteAddr) matches one of the configured trustedProxies (CIDRs or IPs).
// If the direct peer is not a trusted proxy, RemoteAddr is returned to prevent header spoofing.
func extractClientIP(r *http.Request, trustedProxies []string) string {
	peerHost, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		peerHost = r.RemoteAddr
	}
	peerHost = strings.TrimSpace(peerHost)
	peerIP := net.ParseIP(peerHost)

	// If peer IP cannot be parsed or no trusted proxies configured, never trust forwarded headers
	if peerIP == nil || len(trustedProxies) == 0 {
		return peerHost
	}

	if !isIPInTrustedList(peerIP, trustedProxies) {
		// Untrusted peer: ignore X-Forwarded-For and X-Real-IP completely
		return peerHost
	}

	// Peer is a trusted proxy: parse X-Forwarded-For right-to-left
	xff := r.Header.Get("X-Forwarded-For")
	if xff != "" {
		parts := strings.Split(xff, ",")
		// Walk backwards to find first untrusted upstream IP
		for i := len(parts) - 1; i >= 0; i-- {
			candidateStr := strings.TrimSpace(parts[i])
			candidateIP := net.ParseIP(candidateStr)
			if candidateIP == nil {
				continue
			}
			if !isIPInTrustedList(candidateIP, trustedProxies) {
				return candidateStr
			}
		}
		// If all IPs were in trusted proxy list, fall back to leftmost valid IP
		if len(parts) > 0 {
			leftmost := strings.TrimSpace(parts[0])
			if net.ParseIP(leftmost) != nil {
				return leftmost
			}
		}
	}

	xri := strings.TrimSpace(r.Header.Get("X-Real-IP"))
	if xri != "" && net.ParseIP(xri) != nil {
		return xri
	}

	return peerHost
}

func isIPInTrustedList(ip net.IP, trusted []string) bool {
	if ip == nil {
		return false
	}
	for _, entry := range trusted {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if strings.Contains(entry, "/") {
			if _, ipNet, err := net.ParseCIDR(entry); err == nil && ipNet.Contains(ip) {
				return true
			}
		} else {
			if parsed := net.ParseIP(entry); parsed != nil && parsed.Equal(ip) {
				return true
			}
		}
	}
	return false
}

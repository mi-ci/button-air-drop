package api

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"video-detector-clone/internal/auth"
)

type rateLimitDimension string

const (
	rateLimitByIP   rateLimitDimension = "ip"
	rateLimitByUser rateLimitDimension = "user"
)

type rateLimitScope struct {
	Dimension rateLimitDimension
	Limit     int
	Window    time.Duration
}

type rateLimitPolicy struct {
	Scopes []rateLimitScope
}

type rateLimitEntry struct {
	Count      int
	WindowEnds time.Time
}

var (
	publicReadRatePolicy = rateLimitPolicy{
		Scopes: []rateLimitScope{
			{Dimension: rateLimitByIP, Limit: 120, Window: time.Minute},
		},
	}
	kakaoAuthRatePolicy = rateLimitPolicy{
		Scopes: []rateLimitScope{
			{Dimension: rateLimitByIP, Limit: 20, Window: time.Minute},
		},
	}
	authenticatedReadRatePolicy = rateLimitPolicy{
		Scopes: []rateLimitScope{
			{Dimension: rateLimitByUser, Limit: 120, Window: time.Minute},
			{Dimension: rateLimitByIP, Limit: 180, Window: time.Minute},
		},
	}
	profileUpdateRatePolicy = rateLimitPolicy{
		Scopes: []rateLimitScope{
			{Dimension: rateLimitByUser, Limit: 10, Window: time.Minute},
			{Dimension: rateLimitByIP, Limit: 30, Window: time.Minute},
		},
	}
	gameClickRatePolicy = rateLimitPolicy{
		Scopes: []rateLimitScope{
			{Dimension: rateLimitByUser, Limit: 20, Window: 10 * time.Second},
			{Dimension: rateLimitByIP, Limit: 40, Window: 10 * time.Second},
		},
	}
	errMissingBearerToken = errors.New("missing bearer token")
	errInvalidBearerToken = errors.New("invalid bearer token")
)

func (s *Server) withPublicRateLimit(policy rateLimitPolicy, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.enforceRateLimit(w, r, "", policy) {
			return
		}
		next(w, r)
	}
}

func (s *Server) withAuthRateLimit(policy rateLimitPolicy, next func(http.ResponseWriter, *http.Request, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, err := s.userIDFromRequest(r)
		if err != nil {
			if errors.Is(err, errMissingBearerToken) {
				http.Error(w, "missing bearer token", http.StatusUnauthorized)
				return
			}
			if errors.Is(err, errInvalidBearerToken) {
				http.Error(w, "invalid token", http.StatusUnauthorized)
				return
			}
			http.Error(w, "failed to authenticate request", http.StatusUnauthorized)
			return
		}

		if !s.enforceRateLimit(w, r, userID, policy) {
			return
		}

		next(w, r, userID)
	}
}

func (s *Server) userIDFromRequest(r *http.Request) (string, error) {
	authHeader := strings.TrimSpace(r.Header.Get("Authorization"))
	if !strings.HasPrefix(authHeader, "Bearer ") {
		return "", errMissingBearerToken
	}

	claims, err := auth.ParseToken(s.jwtSecret, strings.TrimSpace(strings.TrimPrefix(authHeader, "Bearer ")))
	if err != nil {
		return "", errInvalidBearerToken
	}
	return claims.UserID, nil
}

func (s *Server) enforceRateLimit(w http.ResponseWriter, r *http.Request, userID string, policy rateLimitPolicy) bool {
	if len(policy.Scopes) == 0 {
		return true
	}

	clientIP := clientIPFromRequest(r)
	if clientIP == "" {
		clientIP = "unknown"
	}

	now := time.Now()
	retryAfter := time.Duration(0)
	for _, scope := range policy.Scopes {
		identifier := ""
		switch scope.Dimension {
		case rateLimitByUser:
			if strings.TrimSpace(userID) == "" {
				continue
			}
			identifier = userID
		case rateLimitByIP:
			identifier = clientIP
		default:
			continue
		}

		allowed, wait := s.takeRateLimitSlot(r.Method, r.URL.Path, scope, identifier, now)
		if !allowed && wait > retryAfter {
			retryAfter = wait
		}
	}

	if retryAfter <= 0 {
		return true
	}

	writeJSON(w, http.StatusTooManyRequests, map[string]any{
		"error":      "too many requests",
		"retryAfter": int((retryAfter + time.Second - 1) / time.Second),
	})
	return false
}

func (s *Server) takeRateLimitSlot(method, path string, scope rateLimitScope, identifier string, now time.Time) (bool, time.Duration) {
	s.rateMu.Lock()
	defer s.rateMu.Unlock()

	s.cleanupExpiredRateLimitsLocked(now)

	key := fmt.Sprintf("%s:%s:%s:%s", method, path, scope.Dimension, identifier)
	entry, exists := s.rateLimits[key]
	if !exists || !now.Before(entry.WindowEnds) {
		s.rateLimits[key] = rateLimitEntry{
			Count:      1,
			WindowEnds: now.Add(scope.Window),
		}
		return true, 0
	}

	if entry.Count >= scope.Limit {
		retryAfter := time.Until(entry.WindowEnds)
		if retryAfter < 0 {
			retryAfter = 0
		}
		return false, retryAfter
	}

	entry.Count++
	s.rateLimits[key] = entry
	return true, 0
}

func (s *Server) cleanupExpiredRateLimitsLocked(now time.Time) {
	for key, entry := range s.rateLimits {
		if !now.Before(entry.WindowEnds) {
			delete(s.rateLimits, key)
		}
	}
}

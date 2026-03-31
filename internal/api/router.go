package api

import (
	"bufio"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"video-detector-clone/internal/auth"
	"video-detector-clone/internal/config"
	"video-detector-clone/internal/game"
	"video-detector-clone/internal/storage"
)

type Server struct {
	cfg         *config.Config
	db          *sql.DB
	game        *game.Manager
	jwtSecret   []byte
	location    *time.Location
	wsHub       *wsHub
	tokenTTL    time.Duration
	httpServer  http.Handler
	clickMu     sync.Mutex
	lastClickAt map[string]time.Time
	httpClient  *http.Client
	rateMu      sync.Mutex
	rateLimits  map[string]rateLimitEntry
}

type ClickUsage struct {
	Used      int  `json:"used"`
	Limit     int  `json:"limit"`
	Remaining int  `json:"remaining"`
	IsAdmin   bool `json:"isAdmin"`
}

const (
	defaultDailyClickLimit = 3
	adminDailyClickLimit   = 1000
	adminEmail             = "atm7999@naver.com"
	clickCooldown          = time.Second
	authIPWindow           = time.Hour
	authIPLimit            = 3
	profileChangeWindow    = 7 * 24 * time.Hour
)

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
)

type StatusResponse struct {
	Status   string `json:"status"`
	Service  string `json:"service"`
	Language string `json:"language"`
}

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

func SetupRoutes(cfg *config.Config) (http.Handler, func(context.Context) error, error) {
	loc, err := time.LoadLocation("Asia/Seoul")
	if err != nil {
		return nil, nil, err
	}

	db, err := storage.Open(cfg.DB.Path)
	if err != nil {
		return nil, nil, err
	}

	manager := game.NewManager(db, time.Duration(cfg.Game.InitialSeconds)*time.Second, loc)
	ctx, cancel := context.WithCancel(context.Background())
	manager.Start(ctx)

	srv := &Server{
		cfg:         cfg,
		db:          db,
		game:        manager,
		jwtSecret:   []byte(cfg.Auth.JWTSecret),
		location:    loc,
		wsHub:       newWSHub(),
		tokenTTL:    time.Duration(cfg.Auth.AccessTokenHours) * time.Hour,
		lastClickAt: map[string]time.Time{},
		httpClient:  &http.Client{Timeout: 10 * time.Second},
		rateLimits:  map[string]rateLimitEntry{},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/status", srv.handleStatus)
	mux.HandleFunc("/api/auth/kakao/start", srv.withPublicRateLimit(kakaoAuthRatePolicy, srv.handleKakaoStart))
	mux.HandleFunc("/api/auth/kakao/callback", srv.withPublicRateLimit(kakaoAuthRatePolicy, srv.handleKakaoCallback))
	mux.HandleFunc("/api/me", srv.withAuthRateLimit(authenticatedReadRatePolicy, srv.handleMe))
	mux.HandleFunc("/api/me/profile", srv.withAuthRateLimit(profileUpdateRatePolicy, srv.handleProfileUpdate))
	mux.HandleFunc("/api/game/state", srv.withPublicRateLimit(publicReadRatePolicy, srv.handleGameState))
	mux.HandleFunc("/api/game/click", srv.withAuthRateLimit(gameClickRatePolicy, srv.handleGameClick))
	mux.HandleFunc("/api/rankings/today", srv.withPublicRateLimit(publicReadRatePolicy, srv.handleGameState))
	mux.HandleFunc("/api/rankings/me", srv.withAuthRateLimit(authenticatedReadRatePolicy, srv.handleMyRanking))
	mux.HandleFunc("/ws", srv.handleWS)
	mux.HandleFunc("/", handleSPA)
	srv.httpServer = mux

	go srv.broadcastLoop(ctx)

	cleanup := func(_ context.Context) error {
		cancel()
		srv.wsHub.closeAll()
		return db.Close()
	}

	return mux, cleanup, nil
}

func (s *Server) handleStatus(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, StatusResponse{
		Status:   "ok",
		Service:  "button-air-drop",
		Language: runtime.Version(),
	})
}

func (s *Server) handleKakaoStart(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Kakao.RestAPIKey == "" || s.cfg.Kakao.RedirectURI == "" {
		http.Error(w, "kakao login is not configured", http.StatusInternalServerError)
		return
	}

	state := s.newOAuthState()
	http.SetCookie(w, &http.Cookie{
		Name:     "button_air_drop_oauth_state",
		Value:    state,
		Path:     "/",
		MaxAge:   600,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})

	values := url.Values{}
	values.Set("response_type", "code")
	values.Set("client_id", s.cfg.Kakao.RestAPIKey)
	values.Set("redirect_uri", s.cfg.Kakao.RedirectURI)
	values.Set("state", state)
	if strings.TrimSpace(s.cfg.Kakao.Scope) != "" {
		values.Set("scope", s.cfg.Kakao.Scope)
	}

	http.Redirect(w, r, "https://kauth.kakao.com/oauth/authorize?"+values.Encode(), http.StatusFound)
}

func (s *Server) handleKakaoCallback(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if errorCode := strings.TrimSpace(r.URL.Query().Get("error")); errorCode != "" {
		s.redirectLoginResult(w, r, "", "", "", "kakao-login-cancelled")
		return
	}

	if !s.validOAuthState(r) {
		s.redirectLoginResult(w, r, "", "", "", "invalid-oauth-state")
		return
	}

	code := strings.TrimSpace(r.URL.Query().Get("code"))
	if code == "" {
		s.redirectLoginResult(w, r, "", "", "", "missing-kakao-code")
		return
	}

	kakaoAccessToken, err := s.exchangeKakaoCode(code)
	if err != nil {
		log.Printf("kakao token exchange failed: %v", err)
		s.redirectLoginResult(w, r, "", "", "", "kakao-token-exchange-failed")
		return
	}

	kakaoUser, err := s.fetchKakaoUser(kakaoAccessToken)
	if err != nil {
		log.Printf("kakao user fetch failed: %v", err)
		s.redirectLoginResult(w, r, "", "", "", "kakao-user-fetch-failed")
		return
	}

	user, err := s.ensureKakaoUser(kakaoUser)
	if err != nil {
		log.Printf("kakao user ensure failed: %v", err)
		s.redirectLoginResult(w, r, "", "", "", "kakao-user-save-failed")
		return
	}

	token, err := auth.SignToken(s.jwtSecret, user.UserID, user.ContactEmail, user.Nickname, time.Now().Add(s.tokenTTL))
	if err != nil {
		s.redirectLoginResult(w, r, "", "", "", "token-sign-failed")
		return
	}

	s.redirectLoginResult(w, r, token, user.Nickname, user.ContactEmail, "")
}

func (s *Server) handleMe(w http.ResponseWriter, _ *http.Request, userID string) {
	user, err := s.lookupUser(userID)
	if err != nil {
		http.Error(w, "failed to load user", http.StatusInternalServerError)
		return
	}

	usage, err := s.getClickUsage(userID)
	if err != nil {
		http.Error(w, "failed to load click usage", http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"userId":              user.UserID,
		"email":               user.ContactEmail,
		"nickname":            user.Nickname,
		"contactEmail":        user.ContactEmail,
		"contactEmailConsent": user.ContactEmailConsent,
		"clickUsage":          usage,
	})
}

func (s *Server) handleProfileUpdate(w http.ResponseWriter, r *http.Request, userID string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Nickname            string `json:"nickname"`
		ContactEmail        string `json:"contactEmail"`
		ContactEmailConsent bool   `json:"contactEmailConsent"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}

	nickname := normalizeNickname(req.Nickname)
	if !isValidNickname(nickname) {
		http.Error(w, "invalid nickname", http.StatusBadRequest)
		return
	}

	contactEmail := strings.TrimSpace(strings.ToLower(req.ContactEmail))
	if contactEmail != "" && (!strings.Contains(contactEmail, "@") || len(contactEmail) < 5) {
		http.Error(w, "invalid contact email", http.StatusBadRequest)
		return
	}
	if contactEmail != "" && !req.ContactEmailConsent {
		http.Error(w, "contact email consent required", http.StatusBadRequest)
		return
	}

	user, err := s.lookupUser(userID)
	if err != nil {
		http.Error(w, "failed to load user", http.StatusInternalServerError)
		return
	}

	nowTime := time.Now().In(s.location)
	canChangeNickname, err := canChangeProfileField(user.Nickname, nickname, user.NicknameChangedAt, nowTime)
	if err != nil {
		http.Error(w, "failed to validate nickname change", http.StatusInternalServerError)
		return
	}
	if !canChangeNickname {
		http.Error(w, "nickname can only be changed once every 7 days", http.StatusTooManyRequests)
		return
	}

	canChangeContactEmail, err := canChangeProfileField(user.ContactEmail, contactEmail, user.ContactEmailChangedAt, nowTime)
	if err != nil {
		http.Error(w, "failed to validate contact email change", http.StatusInternalServerError)
		return
	}
	if !canChangeContactEmail {
		http.Error(w, "contact email can only be changed once every 7 days", http.StatusTooManyRequests)
		return
	}

	now := nowTime.Format(time.RFC3339Nano)
	nicknameChangedAt := user.NicknameChangedAt
	if user.Nickname != nickname {
		nicknameChangedAt = now
	}
	contactEmailChangedAt := user.ContactEmailChangedAt
	if user.ContactEmail != contactEmail {
		contactEmailChangedAt = now
	}

	_, err = s.db.Exec(`
		UPDATE users
		SET nickname = ?, nickname_changed_at = ?, contact_email = ?, contact_email_changed_at = ?, contact_email_consent = ?, contact_email_consent_at = ?, updated_at = ?
		WHERE email = ?
	`, nickname, nicknameChangedAt, contactEmail, contactEmailChangedAt, boolToInt(req.ContactEmailConsent), consentTimestamp(contactEmail, req.ContactEmailConsent, now), now, userID)
	if err != nil {
		if isUniqueConstraintError(err) {
			http.Error(w, "nickname already taken", http.StatusConflict)
			return
		}
		http.Error(w, "failed to update profile", http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"userId":              userID,
		"email":               contactEmail,
		"nickname":            nickname,
		"contactEmail":        contactEmail,
		"contactEmailConsent": req.ContactEmailConsent,
	})
}

func (s *Server) handleGameState(w http.ResponseWriter, _ *http.Request) {
	state, err := s.game.Snapshot()
	if err != nil {
		http.Error(w, "failed to load state", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, state)
}

func (s *Server) handleGameClick(w http.ResponseWriter, r *http.Request, userID string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	allowed, usage, err := s.consumeClickIfAllowed(userID)
	if err != nil {
		http.Error(w, "failed to validate click limit", http.StatusInternalServerError)
		return
	}
	if !allowed {
		writeJSON(w, http.StatusTooManyRequests, map[string]any{
			"error":      "daily click limit reached",
			"clickUsage": usage,
		})
		return
	}

	if ok, retryAfter := s.allowClickNow(userID); !ok {
		writeJSON(w, http.StatusTooManyRequests, map[string]any{
			"error":      "click cooldown active",
			"retryAfter": retryAfter.Milliseconds(),
			"clickUsage": usage,
		})
		return
	}

	changed, err := s.game.Click(userID)
	if err != nil {
		http.Error(w, "failed to click", http.StatusInternalServerError)
		return
	}
	if !changed {
		writeJSON(w, http.StatusOK, map[string]any{
			"ignored":    true,
			"clickUsage": usage,
		})
		return
	}

	usage, err = s.incrementClickUsage(userID)
	if err != nil {
		http.Error(w, "failed to save click usage", http.StatusInternalServerError)
		return
	}

	state, err := s.game.Snapshot()
	if err != nil {
		http.Error(w, "failed to click", http.StatusInternalServerError)
		return
	}

	s.wsHub.broadcastState(state)
	writeJSON(w, http.StatusOK, map[string]any{
		"state":      state,
		"clickUsage": usage,
	})
}

func (s *Server) handleMyRanking(w http.ResponseWriter, _ *http.Request, userID string) {
	now := time.Now().In(s.location)
	rows, err := s.db.Query(`
		SELECT duration_ms, created_at
		FROM ranking_entries
		WHERE ranking_date = ? AND email = ?
		ORDER BY duration_ms DESC, created_at ASC
		LIMIT 10
	`, currentRankingDate(now, s.location), userID)
	if err != nil {
		http.Error(w, "failed to load personal rankings", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	entries := []map[string]any{}
	var bestMS int64
	for rows.Next() {
		var durationMS int64
		var createdAt string
		if err := rows.Scan(&durationMS, &createdAt); err != nil {
			http.Error(w, "failed to scan personal rankings", http.StatusInternalServerError)
			return
		}
		if durationMS > bestMS {
			bestMS = durationMS
		}
		entries = append(entries, map[string]any{
			"durationMs": durationMS,
			"createdAt":  createdAt,
		})
	}
	if err := rows.Err(); err != nil {
		http.Error(w, "failed to read personal rankings", http.StatusInternalServerError)
		return
	}

	user, err := s.lookupUser(userID)
	if err != nil {
		http.Error(w, "failed to load user", http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"userId":       userID,
		"email":        user.ContactEmail,
		"nickname":     user.Nickname,
		"contactEmail": user.ContactEmail,
		"rankingDate":  currentRankingDate(now, s.location),
		"attemptCount": len(entries),
		"bestMs":       bestMS,
		"entries":      entries,
	})
}

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

var (
	errMissingBearerToken = errors.New("missing bearer token")
	errInvalidBearerToken = errors.New("invalid bearer token")
)

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
		if !allowed {
			if wait > retryAfter {
				retryAfter = wait
			}
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

func (s *Server) broadcastLoop(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			state, err := s.game.Snapshot()
			if err != nil {
				log.Printf("broadcast snapshot failed: %v", err)
				continue
			}
			s.wsHub.broadcastState(state)
		}
	}
}

func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := upgradeWebSocket(w, r)
	if err != nil {
		http.Error(w, "websocket upgrade failed", http.StatusBadRequest)
		return
	}

	s.wsHub.add(conn)

	state, err := s.game.Snapshot()
	if err == nil {
		_ = conn.WriteJSON(map[string]any{
			"type":  "state",
			"state": state,
		})
	}
}

func handleSPA(w http.ResponseWriter, r *http.Request) {
	distPath := resolveDistPath()
	requestPath := filepath.Join(distPath, r.URL.Path)

	info, err := os.Stat(requestPath)
	if err == nil && !info.IsDir() {
		http.ServeFile(w, r, requestPath)
		return
	}

	http.ServeFile(w, r, filepath.Join(distPath, "index.html"))
}

func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(data)
}

type wsHub struct {
	mu      sync.Mutex
	clients map[*wsConn]struct{}
}

func newWSHub() *wsHub {
	return &wsHub{clients: map[*wsConn]struct{}{}}
}

func (h *wsHub) add(conn *wsConn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.clients[conn] = struct{}{}
}

func (h *wsHub) broadcastState(state any) {
	payload, err := json.Marshal(map[string]any{
		"type":  "state",
		"state": state,
	})
	if err != nil {
		return
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	for conn := range h.clients {
		if err := conn.WriteText(payload); err != nil {
			_ = conn.Close()
			delete(h.clients, conn)
		}
	}
}

func (h *wsHub) closeAll() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for conn := range h.clients {
		_ = conn.Close()
		delete(h.clients, conn)
	}
}

type wsConn struct {
	net.Conn
	br *bufio.ReadWriter
	mu sync.Mutex
}

func (c *wsConn) WriteJSON(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return c.WriteText(data)
}

func (c *wsConn) WriteText(payload []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	header := []byte{0x81}
	payloadLen := len(payload)
	switch {
	case payloadLen <= 125:
		header = append(header, byte(payloadLen))
	case payloadLen <= 65535:
		header = append(header, 126, byte(payloadLen>>8), byte(payloadLen))
	default:
		return errors.New("payload too large")
	}

	if _, err := c.br.Write(header); err != nil {
		return err
	}
	if _, err := c.br.Write(payload); err != nil {
		return err
	}
	return c.br.Flush()
}

func upgradeWebSocket(w http.ResponseWriter, r *http.Request) (*wsConn, error) {
	if !headerContainsToken(r.Header, "Connection", "Upgrade") || !headerContainsToken(r.Header, "Upgrade", "websocket") {
		return nil, errors.New("missing websocket upgrade headers")
	}

	key := strings.TrimSpace(r.Header.Get("Sec-WebSocket-Key"))
	if key == "" {
		return nil, errors.New("missing websocket key")
	}

	hijacker, ok := w.(http.Hijacker)
	if !ok {
		return nil, errors.New("hijacking not supported")
	}

	conn, br, err := hijacker.Hijack()
	if err != nil {
		return nil, err
	}

	accept := websocketAccept(key)
	response := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + accept + "\r\n" +
		"\r\n"
	if _, err := br.WriteString(response); err != nil {
		_ = conn.Close()
		return nil, err
	}
	if err := br.Flush(); err != nil {
		_ = conn.Close()
		return nil, err
	}

	return &wsConn{Conn: conn, br: br}, nil
}

func websocketAccept(key string) string {
	hash := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	return base64.StdEncoding.EncodeToString(hash[:])
}

func headerContainsToken(header http.Header, key, token string) bool {
	for _, value := range header.Values(key) {
		for _, part := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(part), token) {
				return true
			}
		}
	}
	return false
}

func currentRankingDate(now time.Time, loc *time.Location) string {
	return now.In(loc).Format("2006-01-02")
}

func (s *Server) getClickUsage(email string) (ClickUsage, error) {
	limit := clickLimitForEmail(email)
	used := 0
	err := s.db.QueryRow(`
		SELECT click_count
		FROM daily_click_usage
		WHERE ranking_date = ? AND email = ?
	`, currentRankingDate(time.Now().In(s.location), s.location), email).Scan(&used)
	if err != nil && err != sql.ErrNoRows {
		return ClickUsage{}, err
	}
	if err == sql.ErrNoRows {
		used = 0
	}

	remaining := limit - used
	if remaining < 0 {
		remaining = 0
	}

	return ClickUsage{
		Used:      used,
		Limit:     limit,
		Remaining: remaining,
		IsAdmin:   strings.EqualFold(email, adminEmail),
	}, nil
}

func (s *Server) consumeClickIfAllowed(email string) (bool, ClickUsage, error) {
	usage, err := s.getClickUsage(email)
	if err != nil {
		return false, ClickUsage{}, err
	}
	return usage.Used < usage.Limit, usage, nil
}

func (s *Server) incrementClickUsage(email string) (ClickUsage, error) {
	now := time.Now().In(s.location)
	_, err := s.db.Exec(`
		INSERT INTO daily_click_usage (ranking_date, email, click_count, updated_at)
		VALUES (?, ?, 1, ?)
		ON CONFLICT(ranking_date, email) DO UPDATE SET
			click_count = daily_click_usage.click_count + 1,
			updated_at = excluded.updated_at
	`, currentRankingDate(now, s.location), email, now.Format(time.RFC3339Nano))
	if err != nil {
		return ClickUsage{}, err
	}
	return s.getClickUsage(email)
}

func clickLimitForEmail(email string) int {
	if strings.EqualFold(email, adminEmail) {
		return adminDailyClickLimit
	}
	return defaultDailyClickLimit
}

func (s *Server) allowClickNow(email string) (bool, time.Duration) {
	s.clickMu.Lock()
	defer s.clickMu.Unlock()

	now := time.Now()
	last := s.lastClickAt[email]
	if !last.IsZero() {
		elapsed := now.Sub(last)
		if elapsed < clickCooldown {
			return false, clickCooldown - elapsed
		}
	}

	s.lastClickAt[email] = now
	return true, 0
}

func (s *Server) allowAuthRequest(ip string) (bool, time.Duration, error) {
	windowStart := time.Now().Add(-authIPWindow).Format(time.RFC3339Nano)
	rows, err := s.db.Query(`
		SELECT created_at
		FROM auth_request_log
		WHERE ip_address = ? AND created_at >= ?
		ORDER BY created_at ASC
	`, ip, windowStart)
	if err != nil {
		return false, 0, err
	}
	defer rows.Close()

	count := 0
	var oldest string
	for rows.Next() {
		var createdAt string
		if err := rows.Scan(&createdAt); err != nil {
			return false, 0, err
		}
		if count == 0 {
			oldest = createdAt
		}
		count++
	}
	if err := rows.Err(); err != nil {
		return false, 0, err
	}
	if count < authIPLimit {
		return true, 0, nil
	}

	oldestTime, err := time.Parse(time.RFC3339Nano, oldest)
	if err != nil {
		return false, 0, err
	}
	retryAfter := authIPWindow - time.Since(oldestTime)
	if retryAfter < 0 {
		retryAfter = 0
	}
	return false, retryAfter, nil
}

func (s *Server) logAuthRequest(ip string, now time.Time) error {
	_, err := s.db.Exec(`
		INSERT INTO auth_request_log (ip_address, created_at)
		VALUES (?, ?)
	`, ip, now.Format(time.RFC3339Nano))
	return err
}

type userProfile struct {
	UserID                string
	Nickname              string
	ContactEmail          string
	ContactEmailConsent   bool
	NicknameChangedAt     string
	ContactEmailChangedAt string
}

type kakaoTokenResponse struct {
	AccessToken string `json:"access_token"`
}

type kakaoUserInfo struct {
	ID           int64 `json:"id"`
	KakaoAccount struct {
		Profile struct {
			Nickname string `json:"nickname"`
		} `json:"profile"`
	} `json:"kakao_account"`
	Properties struct {
		Nickname string `json:"nickname"`
	} `json:"properties"`
}

var nicknamePattern = regexp.MustCompile(`^[A-Za-z0-9가-힣]{2,12}$`)

var nicknameAdjectives = []string{
	"Mint", "Sunny", "Rapid", "Lucky", "Bold", "Calm", "Swift", "Bright",
}

var nicknameNouns = []string{
	"Rocket", "Tiger", "Button", "Cloud", "Falcon", "Nova", "Pixel", "River",
}

func (s *Server) lookupUser(userID string) (userProfile, error) {
	var user userProfile
	var consentInt int
	err := s.db.QueryRow(`
		SELECT email, nickname, contact_email, contact_email_consent, nickname_changed_at, contact_email_changed_at
		FROM users
		WHERE email = ?
	`, userID).Scan(
		&user.UserID,
		&user.Nickname,
		&user.ContactEmail,
		&consentInt,
		&user.NicknameChangedAt,
		&user.ContactEmailChangedAt,
	)
	if err != nil {
		return user, err
	}
	user.ContactEmailConsent = consentInt == 1
	return user, nil
}

func canChangeProfileField(currentValue, nextValue, changedAt string, now time.Time) (bool, error) {
	if currentValue == nextValue {
		return true, nil
	}
	if strings.TrimSpace(changedAt) == "" {
		return true, nil
	}

	lastChangedAt, err := time.Parse(time.RFC3339Nano, changedAt)
	if err != nil {
		return false, err
	}
	return now.Sub(lastChangedAt) >= profileChangeWindow, nil
}

func (s *Server) ensureKakaoUser(kakaoUser kakaoUserInfo) (userProfile, error) {
	kakaoID := strconv.FormatInt(kakaoUser.ID, 10)

	var existingUserID string
	err := s.db.QueryRow(`SELECT email FROM users WHERE kakao_id = ?`, kakaoID).Scan(&existingUserID)
	if err == nil {
		return s.lookupUser(existingUserID)
	}
	if err != sql.ErrNoRows {
		return userProfile{}, err
	}

	userID := "kakao:" + kakaoID
	now := time.Now().In(s.location).Format(time.RFC3339Nano)
	for range 64 {
		nickname := randomNickname()
		_, insertErr := s.db.Exec(`
			INSERT INTO users (email, nickname, created_at, updated_at, auth_provider, kakao_id)
			VALUES (?, ?, ?, ?, 'kakao', ?)
		`, userID, nickname, now, now, kakaoID)
		if insertErr == nil {
			return userProfile{
				UserID:   userID,
				Nickname: nickname,
			}, nil
		}
		if !isUniqueConstraintError(insertErr) {
			return userProfile{}, insertErr
		}
	}

	return userProfile{}, errors.New("failed to generate unique nickname")
}

func (s *Server) newOAuthState() string {
	raw := fmt.Sprintf("%d-%d", time.Now().UnixNano(), rand.IntN(1_000_000))
	sum := sha256.Sum256([]byte(raw))
	return base64.RawURLEncoding.EncodeToString(sum[:16])
}

func (s *Server) validOAuthState(r *http.Request) bool {
	queryState := strings.TrimSpace(r.URL.Query().Get("state"))
	cookie, err := r.Cookie("button_air_drop_oauth_state")
	if err != nil || queryState == "" {
		return false
	}
	return subtleConstantEqual(queryState, cookie.Value)
}

func (s *Server) exchangeKakaoCode(code string) (string, error) {
	values := url.Values{}
	values.Set("grant_type", "authorization_code")
	values.Set("client_id", s.cfg.Kakao.RestAPIKey)
	values.Set("redirect_uri", s.cfg.Kakao.RedirectURI)
	values.Set("code", code)
	if strings.TrimSpace(s.cfg.Kakao.ClientSecret) != "" {
		values.Set("client_secret", s.cfg.Kakao.ClientSecret)
	}

	req, err := http.NewRequest(http.MethodPost, "https://kauth.kakao.com/oauth/token", strings.NewReader(values.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded;charset=utf-8")

	res, err := s.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()

	body, err := io.ReadAll(res.Body)
	if err != nil {
		return "", err
	}
	if res.StatusCode >= 400 {
		return "", fmt.Errorf("kakao token http %d: %s", res.StatusCode, strings.TrimSpace(string(body)))
	}

	var tokenResponse kakaoTokenResponse
	if err := json.Unmarshal(body, &tokenResponse); err != nil {
		return "", err
	}
	if tokenResponse.AccessToken == "" {
		return "", errors.New("missing kakao access token")
	}
	return tokenResponse.AccessToken, nil
}

func (s *Server) fetchKakaoUser(accessToken string) (kakaoUserInfo, error) {
	req, err := http.NewRequest(http.MethodGet, "https://kapi.kakao.com/v2/user/me", nil)
	if err != nil {
		return kakaoUserInfo{}, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)

	res, err := s.httpClient.Do(req)
	if err != nil {
		return kakaoUserInfo{}, err
	}
	defer res.Body.Close()

	body, err := io.ReadAll(res.Body)
	if err != nil {
		return kakaoUserInfo{}, err
	}
	if res.StatusCode >= 400 {
		return kakaoUserInfo{}, fmt.Errorf("kakao user http %d: %s", res.StatusCode, strings.TrimSpace(string(body)))
	}

	var user kakaoUserInfo
	if err := json.Unmarshal(body, &user); err != nil {
		return kakaoUserInfo{}, err
	}
	if user.ID == 0 {
		return kakaoUserInfo{}, errors.New("missing kakao user id")
	}
	return user, nil
}

func (s *Server) redirectLoginResult(w http.ResponseWriter, r *http.Request, token, nickname, contactEmail, loginError string) {
	target := "/"
	values := url.Values{}
	if token != "" {
		values.Set("accessToken", token)
	}
	if nickname != "" {
		values.Set("nickname", nickname)
	}
	if contactEmail != "" {
		values.Set("email", contactEmail)
	}
	if loginError != "" {
		values.Set("loginError", loginError)
	}
	if encoded := values.Encode(); encoded != "" {
		target += "?" + encoded
	}

	http.SetCookie(w, &http.Cookie{
		Name:     "button_air_drop_oauth_state",
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, target, http.StatusFound)
}

func randomNickname() string {
	adjective := nicknameAdjectives[rand.IntN(len(nicknameAdjectives))]
	noun := nicknameNouns[rand.IntN(len(nicknameNouns))]
	number := rand.IntN(9000) + 1000
	return fmt.Sprintf("%s%s%d", adjective, noun, number)
}

func normalizeNickname(value string) string {
	return strings.TrimSpace(value)
}

func isValidNickname(value string) bool {
	return nicknamePattern.MatchString(value)
}

func isUniqueConstraintError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func consentTimestamp(contactEmail string, consent bool, now string) string {
	if contactEmail == "" || !consent {
		return ""
	}
	return now
}

func subtleConstantEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	left := []byte(a)
	right := []byte(b)
	var diff byte
	for i := range left {
		diff |= left[i] ^ right[i]
	}
	return diff == 0
}

func clientIPFromRequest(r *http.Request) string {
	if forwarded := strings.TrimSpace(r.Header.Get("X-Forwarded-For")); forwarded != "" {
		parts := strings.Split(forwarded, ",")
		if len(parts) > 0 {
			return strings.TrimSpace(parts[0])
		}
	}

	host, _, err := net.SplitHostPort(strings.TrimSpace(r.RemoteAddr))
	if err == nil {
		return host
	}
	return strings.TrimSpace(r.RemoteAddr)
}

func resolveDistPath() string {
	candidates := []string{
		filepath.Join("frontend", "dist"),
	}

	if executablePath, err := os.Executable(); err == nil {
		executableDir := filepath.Dir(executablePath)
		candidates = append(candidates,
			filepath.Join(executableDir, "frontend", "dist"),
			filepath.Join(executableDir, "..", "frontend", "dist"),
		)
	}

	for _, candidate := range candidates {
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			return candidate
		}
	}

	return filepath.Join("frontend", "dist")
}

package api

import (
	"bufio"
	"context"
	"crypto/sha1"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"video-detector-clone/internal/auth"
	"video-detector-clone/internal/config"
	"video-detector-clone/internal/game"
	"video-detector-clone/internal/storage"
)

type Server struct {
	cfg        *config.Config
	db         *sql.DB
	game       *game.Manager
	jwtSecret  []byte
	location   *time.Location
	wsHub      *wsHub
	codeTTL    time.Duration
	tokenTTL   time.Duration
	httpServer http.Handler
}

type StatusResponse struct {
	Status   string `json:"status"`
	Service  string `json:"service"`
	Language string `json:"language"`
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
		cfg:       cfg,
		db:        db,
		game:      manager,
		jwtSecret: []byte(cfg.Auth.JWTSecret),
		location:  loc,
		wsHub:     newWSHub(),
		codeTTL:   time.Duration(cfg.Auth.CodeTTLMinutes) * time.Minute,
		tokenTTL:  time.Duration(cfg.Auth.AccessTokenHours) * time.Hour,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/status", srv.handleStatus)
	mux.HandleFunc("/api/auth/request", srv.handleAuthRequest)
	mux.HandleFunc("/api/auth/verify", srv.handleAuthVerify)
	mux.HandleFunc("/api/me", srv.withAuth(srv.handleMe))
	mux.HandleFunc("/api/game/state", srv.handleGameState)
	mux.HandleFunc("/api/game/click", srv.withAuth(srv.handleGameClick))
	mux.HandleFunc("/api/rankings/today", srv.handleGameState)
	mux.HandleFunc("/api/rankings/me", srv.withAuth(srv.handleMyRanking))
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

func (s *Server) handleAuthRequest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Email string `json:"email"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}

	email := strings.TrimSpace(strings.ToLower(req.Email))
	if !strings.Contains(email, "@") || len(email) < 5 {
		http.Error(w, "invalid email", http.StatusBadRequest)
		return
	}

	code := fmt.Sprintf("%06d", rand.IntN(1000000))
	now := time.Now().In(s.location)
	expiresAt := now.Add(s.codeTTL)

	if _, err := s.db.Exec(`DELETE FROM email_codes WHERE email = ?`, email); err != nil {
		http.Error(w, "failed to save code", http.StatusInternalServerError)
		return
	}
	if _, err := s.db.Exec(`
		INSERT INTO email_codes (email, code, expires_at, created_at)
		VALUES (?, ?, ?, ?)
	`, email, code, expiresAt.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano)); err != nil {
		http.Error(w, "failed to save code", http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"email":      email,
		"expiresAt":  expiresAt.Format(time.RFC3339),
		"devCode":    code,
		"message":    "Email sending is not connected yet. Use the returned devCode for now.",
		"maskedEmail": game.MaskEmail(email),
	})
}

func (s *Server) handleAuthVerify(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Email string `json:"email"`
		Code  string `json:"code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}

	email := strings.TrimSpace(strings.ToLower(req.Email))
	code := strings.TrimSpace(req.Code)

	var savedCode string
	var expiresRaw string
	err := s.db.QueryRow(`
		SELECT code, expires_at
		FROM email_codes
		WHERE email = ?
		ORDER BY created_at DESC
		LIMIT 1
	`, email).Scan(&savedCode, &expiresRaw)
	if err != nil {
		http.Error(w, "code not found", http.StatusUnauthorized)
		return
	}

	expiresAt, err := time.Parse(time.RFC3339Nano, expiresRaw)
	if err != nil {
		http.Error(w, "invalid code expiry", http.StatusInternalServerError)
		return
	}
	if time.Now().After(expiresAt) || code != savedCode {
		http.Error(w, "invalid or expired code", http.StatusUnauthorized)
		return
	}

	token, err := auth.SignToken(s.jwtSecret, email, time.Now().Add(s.tokenTTL))
	if err != nil {
		http.Error(w, "failed to sign token", http.StatusInternalServerError)
		return
	}

	_, _ = s.db.Exec(`DELETE FROM email_codes WHERE email = ?`, email)
	writeJSON(w, http.StatusOK, map[string]any{
		"accessToken": token,
		"expiresInHours": int(s.tokenTTL.Hours()),
		"email": email,
		"maskedEmail": game.MaskEmail(email),
	})
}

func (s *Server) handleMe(w http.ResponseWriter, _ *http.Request, email string) {
	writeJSON(w, http.StatusOK, map[string]string{
		"email":       email,
		"maskedEmail": game.MaskEmail(email),
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

func (s *Server) handleGameClick(w http.ResponseWriter, r *http.Request, email string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if err := s.game.Click(email); err != nil {
		http.Error(w, "failed to click", http.StatusInternalServerError)
		return
	}

	state, err := s.game.Snapshot()
	if err != nil {
		http.Error(w, "failed to load state", http.StatusInternalServerError)
		return
	}

	s.wsHub.broadcastState(state)
	writeJSON(w, http.StatusOK, state)
}

func (s *Server) handleMyRanking(w http.ResponseWriter, _ *http.Request, email string) {
	now := time.Now().In(s.location)
	rows, err := s.db.Query(`
		SELECT duration_ms, created_at
		FROM ranking_entries
		WHERE ranking_date = ? AND email = ?
		ORDER BY duration_ms DESC, created_at ASC
	`, currentRankingDate(now, s.location), email)
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

	writeJSON(w, http.StatusOK, map[string]any{
		"email":        email,
		"maskedEmail":  game.MaskEmail(email),
		"rankingDate":  currentRankingDate(now, s.location),
		"attemptCount": len(entries),
		"bestMs":       bestMS,
		"entries":      entries,
	})
}

func (s *Server) withAuth(next func(http.ResponseWriter, *http.Request, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		authHeader := strings.TrimSpace(r.Header.Get("Authorization"))
		if !strings.HasPrefix(authHeader, "Bearer ") {
			http.Error(w, "missing bearer token", http.StatusUnauthorized)
			return
		}

		claims, err := auth.ParseToken(s.jwtSecret, strings.TrimSpace(strings.TrimPrefix(authHeader, "Bearer ")))
		if err != nil {
			http.Error(w, "invalid token", http.StatusUnauthorized)
			return
		}

		next(w, r, claims.Email)
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

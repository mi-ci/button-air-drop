package api

import (
	"context"
	"database/sql"
	"net/http"
	"runtime"
	"sync"
	"time"

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
	Used      int `json:"used"`
	Limit     int `json:"limit"`
	Remaining int `json:"remaining"`
}

type StatusResponse struct {
	Status   string `json:"status"`
	Service  string `json:"service"`
	Language string `json:"language"`
}

const (
	defaultDailyClickLimit = 3
	clickCooldown          = time.Second
	authIPWindow           = time.Hour
	authIPLimit            = 3
	profileChangeWindow    = 7 * 24 * time.Hour
)

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

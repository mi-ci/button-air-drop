package api

import (
	"database/sql"
	"net/http"
	"time"
)

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
	rankingDate := currentRankingDate(now, s.location)

	rankByEntryID, err := s.loadCurrentRanks(rankingDate)
	if err != nil {
		http.Error(w, "failed to load current ranks", http.StatusInternalServerError)
		return
	}

	rows, err := s.db.Query(`
		SELECT id, duration_ms, created_at
		FROM ranking_entries
		WHERE ranking_date = ? AND user_id = ?
		ORDER BY duration_ms DESC, created_at ASC
		LIMIT 10
	`, rankingDate, userID)
	if err != nil {
		http.Error(w, "failed to load personal rankings", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	entries := []map[string]any{}
	var bestMS int64
	for rows.Next() {
		var entryID int64
		var durationMS int64
		var createdAt string
		if err := rows.Scan(&entryID, &durationMS, &createdAt); err != nil {
			http.Error(w, "failed to scan personal rankings", http.StatusInternalServerError)
			return
		}
		if durationMS > bestMS {
			bestMS = durationMS
		}
		entries = append(entries, map[string]any{
			"durationMs":  durationMS,
			"createdAt":   createdAt,
			"currentRank": rankByEntryID[entryID],
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
		"nickname":     user.Nickname,
		"contactEmail": user.ContactEmail,
		"rankingDate":  rankingDate,
		"attemptCount": len(entries),
		"bestMs":       bestMS,
		"entries":      entries,
	})
}

func (s *Server) loadCurrentRanks(rankingDate string) (map[int64]int, error) {
	rows, err := s.db.Query(`
		SELECT id
		FROM ranking_entries
		WHERE ranking_date = ?
		ORDER BY duration_ms DESC, created_at ASC
	`, rankingDate)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	ranks := map[int64]int{}
	rank := 1
	for rows.Next() {
		var entryID int64
		if err := rows.Scan(&entryID); err != nil {
			return nil, err
		}
		ranks[entryID] = rank
		rank++
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return ranks, nil
}

func currentRankingDate(now time.Time, loc *time.Location) string {
	return now.In(loc).Format("2006-01-02")
}

func (s *Server) getClickUsage(userID string) (ClickUsage, error) {
	limit := defaultDailyClickLimit
	used := 0
	err := s.db.QueryRow(`
		SELECT click_count
		FROM daily_click_usage
		WHERE ranking_date = ? AND user_id = ?
	`, currentRankingDate(time.Now().In(s.location), s.location), userID).Scan(&used)
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
	}, nil
}

func (s *Server) consumeClickIfAllowed(userID string) (bool, ClickUsage, error) {
	usage, err := s.getClickUsage(userID)
	if err != nil {
		return false, ClickUsage{}, err
	}
	return usage.Used < usage.Limit, usage, nil
}

func (s *Server) incrementClickUsage(userID string) (ClickUsage, error) {
	now := time.Now().In(s.location)
	_, err := s.db.Exec(`
		INSERT INTO daily_click_usage (ranking_date, user_id, click_count, updated_at)
		VALUES (?, ?, 1, ?)
		ON CONFLICT(ranking_date, user_id) DO UPDATE SET
			click_count = daily_click_usage.click_count + 1,
			updated_at = excluded.updated_at
	`, currentRankingDate(now, s.location), userID, now.Format(time.RFC3339Nano))
	if err != nil {
		return ClickUsage{}, err
	}
	return s.getClickUsage(userID)
}

func (s *Server) allowClickNow(userID string) (bool, time.Duration) {
	s.clickMu.Lock()
	defer s.clickMu.Unlock()

	now := time.Now()
	last := s.lastClickAt[userID]
	if !last.IsZero() {
		elapsed := now.Sub(last)
		if elapsed < clickCooldown {
			return false, clickCooldown - elapsed
		}
	}

	s.lastClickAt[userID] = now
	return true, 0
}

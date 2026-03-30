package game

import (
	"context"
	"database/sql"
	"sync"
	"time"
)

type RankingEntry struct {
	Rank        int    `json:"rank"`
	MaskedEmail string `json:"maskedEmail"`
	DurationMS  int64  `json:"durationMs"`
}

type State struct {
	ServerTimeMS int64          `json:"serverTimeMs"`
	RankingDate  string         `json:"rankingDate"`
	RemainingMS  int64          `json:"remainingMs"`
	InitialMS    int64          `json:"initialMs"`
	LeaderEmail  string         `json:"leaderEmail"`
	LeaderMasked string         `json:"leaderMasked"`
	HeldMS       int64          `json:"heldMs"`
	Leaderboard  []RankingEntry `json:"leaderboard"`
}

type Manager struct {
	mu          sync.Mutex
	db          *sql.DB
	initial     time.Duration
	location    *time.Location
	leaderEmail string
	leaderSince time.Time
	expiresAt   time.Time
}

func NewManager(db *sql.DB, initial time.Duration, location *time.Location) *Manager {
	manager := &Manager{
		db:       db,
		initial:  initial,
		location: location,
	}
	manager.restoreCurrentRound()
	return manager
}

func (m *Manager) Start(ctx context.Context) {
	ticker := time.NewTicker(250 * time.Millisecond)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				m.finalizeExpired(now)
			}
		}
	}()
}

func (m *Manager) Click(email string) error {
	now := time.Now().In(m.location)

	m.mu.Lock()
	defer m.mu.Unlock()

	if !m.expiresAt.IsZero() && now.After(m.expiresAt) {
		m.persistLeaderLocked(now)
	}

	if m.leaderEmail != "" && m.leaderEmail != email {
		m.persistLeaderLocked(now)
	}

	if m.leaderEmail == email && now.Before(m.expiresAt) {
		return nil
	}

	m.leaderEmail = email
	m.leaderSince = now
	m.expiresAt = now.Add(m.initial)
	return m.saveCurrentRoundLocked(now)
}

func (m *Manager) Snapshot() (State, error) {
	now := time.Now().In(m.location)
	m.finalizeExpired(now)

	m.mu.Lock()
	defer m.mu.Unlock()

	state := State{
		ServerTimeMS: now.UnixMilli(),
		RankingDate:  rankingDate(now, m.location),
		InitialMS:    m.initial.Milliseconds(),
		RemainingMS:  m.initial.Milliseconds(),
		Leaderboard:  []RankingEntry{},
	}

	if m.leaderEmail != "" {
		remaining := m.expiresAt.Sub(now)
		if remaining < 0 {
			remaining = 0
		}
		state.RemainingMS = remaining.Milliseconds()
		state.LeaderEmail = m.leaderEmail
		state.LeaderMasked = MaskEmail(m.leaderEmail)
		state.HeldMS = now.Sub(m.leaderSince).Milliseconds()
	}

	rows, err := m.db.Query(`
		SELECT masked_email, duration_ms
		FROM ranking_entries
		WHERE ranking_date = ?
		ORDER BY duration_ms DESC, created_at ASC
		LIMIT 20
	`, rankingDate(now, m.location))
	if err != nil {
		return state, err
	}
	defer rows.Close()

	rank := 1
	for rows.Next() {
		var entry RankingEntry
		entry.Rank = rank
		if err := rows.Scan(&entry.MaskedEmail, &entry.DurationMS); err != nil {
			return state, err
		}
		state.Leaderboard = append(state.Leaderboard, entry)
		rank++
	}

	return state, rows.Err()
}

func (m *Manager) finalizeExpired(now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.leaderEmail == "" || m.expiresAt.IsZero() || now.Before(m.expiresAt) {
		return
	}

	m.persistLeaderLocked(now)
}

func (m *Manager) persistLeaderLocked(now time.Time) {
	if m.leaderEmail == "" {
		_, _ = m.db.Exec(`DELETE FROM current_rounds WHERE id = 1`)
		return
	}

	duration := now.Sub(m.leaderSince)
	if m.expiresAt.Before(now) {
		duration = m.expiresAt.Sub(m.leaderSince)
	}
	if duration < 0 {
		duration = 0
	}

	_, _ = m.db.Exec(`
		INSERT INTO ranking_entries (ranking_date, email, masked_email, duration_ms, created_at)
		VALUES (?, ?, ?, ?, ?)
	`, rankingDate(now, m.location), m.leaderEmail, MaskEmail(m.leaderEmail), duration.Milliseconds(), now.Format(time.RFC3339Nano))

	m.leaderEmail = ""
	m.leaderSince = time.Time{}
	m.expiresAt = time.Time{}
	_, _ = m.db.Exec(`DELETE FROM current_rounds WHERE id = 1`)
}

func (m *Manager) saveCurrentRoundLocked(now time.Time) error {
	_, err := m.db.Exec(`
		INSERT INTO current_rounds (id, ranking_date, leader_email, leader_since, expires_at, updated_at)
		VALUES (1, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			ranking_date = excluded.ranking_date,
			leader_email = excluded.leader_email,
			leader_since = excluded.leader_since,
			expires_at = excluded.expires_at,
			updated_at = excluded.updated_at
	`, rankingDate(now, m.location), m.leaderEmail, m.leaderSince.Format(time.RFC3339Nano), m.expiresAt.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano))
	return err
}

func (m *Manager) restoreCurrentRound() {
	var rankingDay string
	var leaderEmail string
	var leaderSinceRaw string
	var expiresAtRaw string

	err := m.db.QueryRow(`
		SELECT ranking_date, leader_email, leader_since, expires_at
		FROM current_rounds
		WHERE id = 1
	`).Scan(&rankingDay, &leaderEmail, &leaderSinceRaw, &expiresAtRaw)
	if err != nil {
		return
	}

	now := time.Now().In(m.location)
	if rankingDay != rankingDate(now, m.location) {
		_, _ = m.db.Exec(`DELETE FROM current_rounds WHERE id = 1`)
		return
	}

	leaderSince, err := time.Parse(time.RFC3339Nano, leaderSinceRaw)
	if err != nil {
		_, _ = m.db.Exec(`DELETE FROM current_rounds WHERE id = 1`)
		return
	}
	expiresAt, err := time.Parse(time.RFC3339Nano, expiresAtRaw)
	if err != nil {
		_, _ = m.db.Exec(`DELETE FROM current_rounds WHERE id = 1`)
		return
	}

	m.mu.Lock()
	m.leaderEmail = leaderEmail
	m.leaderSince = leaderSince.In(m.location)
	m.expiresAt = expiresAt.In(m.location)
	m.mu.Unlock()

	m.finalizeExpired(now)
}

func rankingDate(now time.Time, loc *time.Location) string {
	return now.In(loc).Format("2006-01-02")
}

func MaskEmail(email string) string {
	at := -1
	for i, ch := range email {
		if ch == '@' {
			at = i
			break
		}
	}
	if at <= 1 {
		return "***"
	}
	return email[:1] + "***" + email[at:]
}

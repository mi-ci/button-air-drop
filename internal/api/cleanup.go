package api

import (
	"context"
	"database/sql"
	"log"
	"time"
)

func (s *Server) startCleanupLoop(ctx context.Context) {
	s.runDailyCleanup(time.Now().In(s.location))

	for {
		wait := time.Until(nextMidnight(time.Now().In(s.location), s.location))
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			s.runDailyCleanup(time.Now().In(s.location))
		}
	}
}

func (s *Server) runDailyCleanup(now time.Time) {
	if err := s.archiveAndDeleteExpiredRankings(now); err != nil {
		log.Printf("daily cleanup failed: %v", err)
	}
}

func (s *Server) archiveAndDeleteExpiredRankings(now time.Time) error {
	cutoffDate := currentRankingDate(now.AddDate(0, 0, -1), s.location)
	archivedAt := now.Format(time.RFC3339Nano)

	tx, err := s.db.BeginTx(context.Background(), &sql.TxOptions{})
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`
		INSERT OR REPLACE INTO ranking_entries_archive (id, ranking_date, user_id, duration_ms, created_at, display_name, archived_at)
		SELECT id, ranking_date, user_id, duration_ms, created_at, display_name, ?
		FROM ranking_entries
		WHERE ranking_date < ?
	`, archivedAt, cutoffDate); err != nil {
		return err
	}

	if _, err := tx.Exec(`
		DELETE FROM ranking_entries
		WHERE ranking_date < ?
	`, cutoffDate); err != nil {
		return err
	}

	if _, err := tx.Exec(`
		INSERT OR REPLACE INTO daily_click_usage_archive (ranking_date, user_id, click_count, updated_at, archived_at)
		SELECT ranking_date, user_id, click_count, updated_at, ?
		FROM daily_click_usage
		WHERE ranking_date < ?
	`, archivedAt, cutoffDate); err != nil {
		return err
	}

	if _, err := tx.Exec(`
		DELETE FROM daily_click_usage
		WHERE ranking_date < ?
	`, cutoffDate); err != nil {
		return err
	}

	return tx.Commit()
}

func nextMidnight(now time.Time, loc *time.Location) time.Time {
	nextDay := now.In(loc).AddDate(0, 0, 1)
	return time.Date(nextDay.Year(), nextDay.Month(), nextDay.Day(), 0, 0, 0, 0, loc)
}

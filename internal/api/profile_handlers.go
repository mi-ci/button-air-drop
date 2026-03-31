package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

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
		SET nickname = ?, nickname_changed_at = ?, contact_email = ?, contact_email_changed_at = ?, contact_email_consent = ?, updated_at = ?
		WHERE kakao_id = ? OR user_id = ?
	`, nickname, nicknameChangedAt, contactEmail, contactEmailChangedAt, boolToInt(req.ContactEmailConsent), now, userID, userID)
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
		"nickname":            nickname,
		"contactEmail":        contactEmail,
		"contactEmailConsent": req.ContactEmailConsent,
	})
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

func normalizeNickname(value string) string {
	return strings.TrimSpace(value)
}

func isValidNickname(value string) bool {
	return nicknamePattern.MatchString(value)
}

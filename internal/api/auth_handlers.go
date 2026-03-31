package api

import (
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"video-detector-clone/internal/auth"
)

type userProfile struct {
	UserID                string
	KakaoID               string
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

var nicknamePattern = regexp.MustCompile(`^[A-Za-z0-9가-힣]{2,6}$`)

var nicknameAdjectives = []string{
	"초록", "맑은", "푸른", "고운", "환한", "검은", "하얀", "붉은", "노란",
	"은빛", "금빛", "밝은", "조용", "반짝", "산뜻", "따뜻", "차분", "선명",
	"포근", "싱그", "달큰", "화창", "청량", "단정", "온화", "영롱", "찬란",
	"짙은", "부드", "반듯", "고요", "낭만", "예쁜", "힘찬", "기쁜", "상냥",
}

var nicknameNouns = []string{
	"호랑", "구름", "바다", "별빛", "산책", "노을", "바람", "여름", "달빛",
	"새벽", "하늘", "파도", "은하", "서리", "단비", "이슬", "숲길", "겨울",
	"가을", "봄날", "벚꽃", "강물", "들꽃", "소나", "새싹", "꽃길", "하모",
	"시냇", "물결", "별님", "해님", "솔빛", "달님", "눈꽃", "석양", "풀잎",
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

	token, err := auth.SignToken(s.jwtSecret, user.UserID, user.Nickname, time.Now().Add(s.tokenTTL))
	if err != nil {
		s.redirectLoginResult(w, r, "", "", "", "token-sign-failed")
		return
	}

	s.redirectLoginResult(w, r, token, user.Nickname, user.ContactEmail, "")
}

func (s *Server) lookupUser(userID string) (userProfile, error) {
	var user userProfile
	var consentInt int
	err := s.db.QueryRow(`
		SELECT user_id, kakao_id, nickname, contact_email, contact_email_consent, nickname_changed_at, contact_email_changed_at
		FROM users
		WHERE kakao_id = ? OR user_id = ?
		LIMIT 1
	`, userID, userID).Scan(
		&user.UserID,
		&user.KakaoID,
		&user.Nickname,
		&user.ContactEmail,
		&consentInt,
		&user.NicknameChangedAt,
		&user.ContactEmailChangedAt,
	)
	if err != nil {
		return user, err
	}
	if user.KakaoID != "" {
		user.UserID = user.KakaoID
	}
	user.ContactEmailConsent = consentInt == 1
	return user, nil
}

func (s *Server) ensureKakaoUser(kakaoUser kakaoUserInfo) (userProfile, error) {
	kakaoID := strconv.FormatInt(kakaoUser.ID, 10)

	var existingUserID string
	err := s.db.QueryRow(`SELECT user_id FROM users WHERE kakao_id = ?`, kakaoID).Scan(&existingUserID)
	if err == nil {
		return s.lookupUser(existingUserID)
	}
	if err != sql.ErrNoRows {
		return userProfile{}, err
	}

	userID := kakaoID
	now := time.Now().In(s.location).Format(time.RFC3339Nano)
	for range 64 {
		nickname := randomNickname()
		_, insertErr := s.db.Exec(`
			INSERT INTO users (user_id, nickname, created_at, updated_at, kakao_id)
			VALUES (?, ?, ?, ?, ?)
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
		values.Set("contactEmail", contactEmail)
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
	number := rand.IntN(90) + 10
	return fmt.Sprintf("%s%s%02d", adjective, noun, number)
}

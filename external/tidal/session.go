// Package tidal integrates Tidal music streaming into cliamp via the same
// private API that the listen.tidal.com web player uses. See docs/tidal.md
// for the user-facing setup, quality tiers, and DRM caveat.
package tidal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"cliamp/applog"
	"cliamp/internal/httpclient"
)

// Base URLs and endpoints for Tidal's private web API.
// These match the ones used by the listen.tidal.com SPA (and Vermilion).
const (
	tidalAPIURL       = "https://listen.tidal.com/"
	tidalAuthURL      = "https://auth.tidal.com/"
	tidalAuthTokenEP  = "v1/oauth2/token"
	tidalSessionEP    = "v1/sessions"
	tidalSearchEP     = "v2/search/"
	tidalTrackEP      = "v1/tracks/"
	tidalPlaylistsEP  = "v1/playlists/"
	tidalUsersEP      = "v1/users/"
	tidalFavoritesEP  = "v1/users/%s/favorites/"
	tidalPlaybackInfo = "/playbackinfo"
)

// Default token grant lifetime if the server doesn't report one (Tidal
// access tokens are ~24h in practice).
const defaultTokenLifetime = 24 * time.Hour

// Refresh the access token this long before its advertised expiry so a
// long-running session never reaches for a dead token.
const refreshLeeway = 5 * time.Minute

// Max JSON body we'll read back from Tidal (guards against runaway responses).
const maxResponseBody = 10 << 20 // 10 MB

// ErrAuth is returned when refresh fails or no credentials are configured.
var ErrAuth = errors.New("tidal: authentication failed — check client_id and refresh_token")

// Session owns a Tidal access token, the sessionId + countryCode needed on
// every API call, and refreshes the token in the background before it
// expires.
type Session struct {
	clientID     string
	refreshToken string

	mu          sync.Mutex
	accessToken string
	expiresAt   time.Time
	sessionID   string
	countryCode string
	userID      string
	http        *http.Client
}

// NewSession builds an unauthenticated session. Call Login before use.
// Passes the shared streaming http.Client so we reuse connection pools.
func NewSession(clientID, refreshToken string) *Session {
	return &Session{
		clientID:     clientID,
		refreshToken: refreshToken,
		http:         httpclient.Streaming,
	}
}

// Login refreshes the access token and fetches the sessionId + countryCode.
// Must be called once before any API method. Safe to call repeatedly; it
// is a no-op if a valid token is already present.
func (s *Session) Login(ctx context.Context) error {
	if err := s.refresh(ctx); err != nil {
		return err
	}
	return s.bootstrapSession(ctx)
}

// refresh exchanges the long-lived refresh_token for a fresh access_token.
// Thread-safe: multiple goroutines racing to refresh still only fire once.
func (s *Session) refresh(ctx context.Context) error {
	s.mu.Lock()
	if s.accessToken != "" && time.Now().Before(s.expiresAt.Add(-refreshLeeway)) {
		s.mu.Unlock()
		return nil
	}
	clientID := s.clientID
	refreshTok := s.refreshToken
	s.mu.Unlock()

	if clientID == "" || refreshTok == "" {
		return ErrAuth
	}

	form := url.Values{
		"client_id":     {clientID},
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshTok},
		"scope":         {"r_usr+w_usr"},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		tidalAuthURL+tidalAuthTokenEP, strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("tidal: build refresh: %w", err)
	}
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=UTF-8")

	resp, err := s.http.Do(req)
	if err != nil {
		return fmt.Errorf("tidal: refresh: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%w (status %d: %s)", ErrAuth, resp.StatusCode, snippet(body))
	}

	var tok struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
		UserID      any    `json:"user_id"`
	}
	if err := json.Unmarshal(body, &tok); err != nil {
		return fmt.Errorf("tidal: parse refresh: %w", err)
	}
	if tok.AccessToken == "" {
		return fmt.Errorf("%w (no access_token in response)", ErrAuth)
	}

	lifetime := defaultTokenLifetime
	if tok.ExpiresIn > 0 {
		lifetime = time.Duration(tok.ExpiresIn) * time.Second
	}

	s.mu.Lock()
	s.accessToken = tok.AccessToken
	s.expiresAt = time.Now().Add(lifetime)
	if userID := coerceID(tok.UserID); userID != "" {
		s.userID = userID
	}
	s.mu.Unlock()
	applog.Printf("tidal: token refreshed, valid for %v", lifetime.Round(time.Minute))
	return nil
}

// bootstrapSession fetches the Tidal session (sessionId + countryCode)
// using the access token. Required before any catalog or playback call.
func (s *Session) bootstrapSession(ctx context.Context) error {
	resp, err := s.api(ctx, http.MethodGet, tidalSessionEP, nil, nil)
	if err != nil {
		return fmt.Errorf("tidal: session: %w", err)
	}
	defer resp.Body.Close()

	var sess struct {
		SessionID   string `json:"sessionId"`
		CountryCode string `json:"countryCode"`
		UserID      any    `json:"userId"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBody)).Decode(&sess); err != nil {
		return fmt.Errorf("tidal: parse session: %w", err)
	}
	if sess.SessionID == "" || sess.CountryCode == "" {
		return fmt.Errorf("tidal: session: missing sessionId or countryCode")
	}

	s.mu.Lock()
	s.sessionID = sess.SessionID
	s.countryCode = sess.CountryCode
	if uid := coerceID(sess.UserID); uid != "" {
		s.userID = uid
	}
	s.mu.Unlock()
	return nil
}

// CountryCode returns the authenticated user's country code (e.g. "US", "NO").
func (s *Session) CountryCode() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.countryCode == "" {
		return "US"
	}
	return s.countryCode
}

// SessionID returns the current sessionId (needed by an external bridge to
// perform its own license request).
func (s *Session) SessionID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessionID
}

// AccessToken returns the current bearer token, refreshing if near expiry.
// Primarily exposed for the external LOSSLESS bridge helper.
func (s *Session) AccessToken(ctx context.Context) (string, error) {
	if err := s.refresh(ctx); err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.accessToken, nil
}

// UserID returns the authenticated Tidal numeric user id.
func (s *Session) UserID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.userID
}

// api performs an authenticated call to listen.tidal.com. Refreshes the
// token first if needed, wires in countryCode automatically when query
// is non-nil, and transparently retries once on 401 (expired token) and
// up to 4x on 429 with exponential backoff.
func (s *Session) api(ctx context.Context, method, endpoint string, query url.Values, body io.Reader) (*http.Response, error) {
	return s.apiWithBody(ctx, method, endpoint, query, body, "")
}

// apiWithBody is like api but accepts a content-type for POST/PUT calls.
func (s *Session) apiWithBody(ctx context.Context, method, endpoint string, query url.Values, body io.Reader, contentType string) (*http.Response, error) {
	if err := s.refresh(ctx); err != nil {
		return nil, err
	}

	// Buffer the body once so we can replay on retry.
	var bodyBytes []byte
	if body != nil {
		var err error
		bodyBytes, err = io.ReadAll(body)
		if err != nil {
			return nil, fmt.Errorf("tidal: read body: %w", err)
		}
	}

	const maxRetries = 4
	var lastStatus int
	var lastBody []byte

	for attempt := 0; attempt < maxRetries; attempt++ {
		if query != nil && query.Get("countryCode") == "" {
			query.Set("countryCode", s.CountryCode())
		}

		fullURL := tidalAPIURL + endpoint
		if len(query) > 0 {
			fullURL += "?" + query.Encode()
		}

		var reqBody io.Reader
		if bodyBytes != nil {
			reqBody = bytes.NewReader(bodyBytes)
		}
		req, err := http.NewRequestWithContext(ctx, method, fullURL, reqBody)
		if err != nil {
			return nil, fmt.Errorf("tidal: build request: %w", err)
		}

		s.mu.Lock()
		tok := s.accessToken
		s.mu.Unlock()

		req.Header.Set("Accept", "application/json")
		req.Header.Set("Authorization", "Bearer "+tok)
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}

		resp, err := s.http.Do(req)
		if err != nil {
			return nil, fmt.Errorf("tidal: %s %s: %w", method, endpoint, err)
		}

		// 401 once: force a token refresh and try again. Happens after a
		// long idle when the cached expiry time lied.
		if resp.StatusCode == http.StatusUnauthorized && attempt == 0 {
			resp.Body.Close()
			applog.Printf("tidal: 401 on %s, forcing refresh", endpoint)
			s.mu.Lock()
			s.accessToken = ""
			s.expiresAt = time.Time{}
			s.mu.Unlock()
			if err := s.refresh(ctx); err != nil {
				return nil, err
			}
			continue
		}

		// 429: respect Retry-After if present, otherwise exponential.
		if resp.StatusCode == http.StatusTooManyRequests {
			wait := time.Duration(1<<attempt) * time.Second
			if ra := resp.Header.Get("Retry-After"); ra != "" {
				if secs, err := strconv.Atoi(ra); err == nil && secs > 0 {
					wait = time.Duration(secs) * time.Second
				}
			}
			resp.Body.Close()
			applog.Printf("tidal: 429 on %s, waiting %v", endpoint, wait)
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(wait):
				continue
			}
		}

		if resp.StatusCode >= 400 {
			lastStatus = resp.StatusCode
			lastBody, _ = io.ReadAll(io.LimitReader(resp.Body, 512))
			resp.Body.Close()
			return nil, fmt.Errorf("tidal: %s %s: http %d: %s", method, endpoint, lastStatus, snippet(lastBody))
		}

		return resp, nil
	}

	return nil, fmt.Errorf("tidal: %s %s: exhausted retries (last status %d: %s)", method, endpoint, lastStatus, snippet(lastBody))
}

// Close releases any persistent resources. Currently a no-op — we use the
// shared httpclient.Streaming pool which lives for the whole process.
func (s *Session) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.accessToken = ""
	s.expiresAt = time.Time{}
	s.sessionID = ""
}

// snippet trims long HTTP error bodies so they don't blow out log lines.
func snippet(b []byte) string {
	const limit = 200
	if len(b) <= limit {
		return string(b)
	}
	return string(b[:limit]) + "…"
}

// coerceID converts Tidal's loosely-typed JSON user id (sometimes a number,
// sometimes a string) to a string.
func coerceID(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return strconv.FormatInt(int64(x), 10)
	case int64:
		return strconv.FormatInt(x, 10)
	case json.Number:
		return x.String()
	}
	return ""
}

package tidal

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gopxl/beep/v2"

	"cliamp/playlist"
	"cliamp/provider"
)

var (
	_ playlist.Provider       = (*Provider)(nil)
	_ playlist.Authenticator  = (*Provider)(nil)
	_ provider.Searcher       = (*Provider)(nil)
	_ provider.CustomStreamer = (*Provider)(nil)
	_ provider.Closer         = (*Provider)(nil)
)

// Pagination limits for Tidal endpoints. 100 is the hard server cap.
const (
	pageSize      = 100
	maxSearchHits = 25
)

// favoritesID is the synthetic playlist id used for "Your Music" / Tidal
// favorites — the one list that doesn't have a real UUID.
const favoritesID = "TIDAL_FAVORITES"

// Quality tiers. Tidal also supports HI_RES_LOSSLESS / DOLBY_ATMOS but those
// are always DRM-protected so the native streamer never uses them.
const (
	QualityLow      = "LOW"      // 96 kbps AAC, unencrypted
	QualityHigh     = "HIGH"     // 320 kbps AAC, unencrypted
	QualityLossless = "LOSSLESS" // 16-bit FLAC, DRM (needs external bridge)
)

// Provider implements playlist.Provider for Tidal.
type Provider struct {
	session         *Session
	quality         string // preferred quality: LOW, HIGH, LOSSLESS
	externalCommand string // optional shell command template for LOSSLESS playback

	mu         sync.Mutex
	listCache  []playlist.PlaylistInfo
	listCached time.Time
	tracksByID map[string][]playlist.Track // playlistID -> tracks (per-session cache)
}

// Config holds user-facing options for the Tidal provider.
type Config struct {
	ClientID        string
	RefreshToken    string
	Quality         string // LOW, HIGH, LOSSLESS — default HIGH
	ExternalCommand string // shell template, placeholders {id} {quality}; stdout = playable URL or path
}

// IsSet reports whether the Tidal provider should be instantiated.
func (c Config) IsSet() bool {
	return c.ClientID != "" && c.RefreshToken != ""
}

// cacheTTL is how long we trust the cached playlist list before re-fetching.
const cacheTTL = 5 * time.Minute

// New creates a Tidal Provider from the parsed config.
func New(cfg Config) *Provider {
	quality := strings.ToUpper(strings.TrimSpace(cfg.Quality))
	switch quality {
	case QualityLow, QualityHigh, QualityLossless:
	default:
		quality = QualityHigh
	}
	return &Provider{
		session:         NewSession(cfg.ClientID, cfg.RefreshToken),
		quality:         quality,
		externalCommand: strings.TrimSpace(cfg.ExternalCommand),
		tracksByID:      make(map[string][]playlist.Track),
	}
}

func (p *Provider) Name() string { return "Tidal" }

// Authenticate validates the configured refresh_token by forcing a token
// refresh + session bootstrap.
func (p *Provider) Authenticate() error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return p.session.Login(ctx)
}

func (p *Provider) Close() {
	p.session.Close()
}

// ensureSession is a cheap check that a valid session exists. Lazy, so a
// misconfigured [tidal] section surfaces errors at first use, not startup.
func (p *Provider) ensureSession() error {
	if p.session.SessionID() != "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return p.session.Login(ctx)
}

// Playlists returns the user's favorite tracks ("Your Music") plus all
// owned/created playlists.
func (p *Provider) Playlists() ([]playlist.PlaylistInfo, error) {
	if err := p.ensureSession(); err != nil {
		return nil, err
	}

	p.mu.Lock()
	if p.listCache != nil && time.Since(p.listCached) < cacheTTL {
		cached := p.listCache
		p.mu.Unlock()
		return cached, nil
	}
	p.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	uid := p.session.UserID()
	if uid == "" {
		return nil, fmt.Errorf("tidal: no user id available")
	}

	var out []playlist.PlaylistInfo

	// Surface favorites first so they appear at the top of the picker.
	favCount, _ := p.fetchFavoriteCount(ctx, uid)
	out = append(out, playlist.PlaylistInfo{
		ID:         favoritesID,
		Name:       "Your Music",
		TrackCount: favCount,
	})

	// User-created playlists.
	offset := 0
	for {
		q := url.Values{
			"limit":  {strconv.Itoa(pageSize)},
			"offset": {strconv.Itoa(offset)},
		}
		resp, err := p.session.api(ctx, http.MethodGet,
			fmt.Sprintf("%s%s/playlists", tidalUsersEP, uid), q, nil)
		if err != nil {
			return nil, fmt.Errorf("tidal: list playlists: %w", err)
		}

		var result struct {
			Items []struct {
				UUID            string `json:"uuid"`
				Title           string `json:"title"`
				NumberOfTracks  int    `json:"numberOfTracks"`
				NumberOfVideos  int    `json:"numberOfVideos"`
				LastItemAddedAt string `json:"lastItemAddedAt"`
			} `json:"items"`
			TotalNumberOfItems int `json:"totalNumberOfItems"`
		}
		if err := decodeBody(resp, &result); err != nil {
			return nil, fmt.Errorf("tidal: parse playlists: %w", err)
		}

		for _, it := range result.Items {
			out = append(out, playlist.PlaylistInfo{
				ID:         it.UUID,
				Name:       it.Title,
				TrackCount: it.NumberOfTracks + it.NumberOfVideos,
			})
		}

		offset += len(result.Items)
		if len(result.Items) == 0 || offset >= result.TotalNumberOfItems {
			break
		}
	}

	p.mu.Lock()
	p.listCache = out
	p.listCached = time.Now()
	p.mu.Unlock()

	return out, nil
}

// fetchFavoriteCount gets just the total count of favorite tracks without
// pulling all of them. Cheap because Tidal returns totalNumberOfItems on
// the first page.
func (p *Provider) fetchFavoriteCount(ctx context.Context, uid string) (int, error) {
	q := url.Values{"limit": {"1"}, "offset": {"0"}}
	resp, err := p.session.api(ctx, http.MethodGet,
		fmt.Sprintf(tidalFavoritesEP, uid)+"tracks", q, nil)
	if err != nil {
		return 0, err
	}
	var result struct {
		TotalNumberOfItems int `json:"totalNumberOfItems"`
	}
	if err := decodeBody(resp, &result); err != nil {
		return 0, err
	}
	return result.TotalNumberOfItems, nil
}

// Tracks handles both favorites and regular playlist track lookup.
func (p *Provider) Tracks(playlistID string) ([]playlist.Track, error) {
	if err := p.ensureSession(); err != nil {
		return nil, err
	}

	p.mu.Lock()
	if cached, ok := p.tracksByID[playlistID]; ok {
		p.mu.Unlock()
		return cached, nil
	}
	p.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	var tracks []playlist.Track
	var err error
	if playlistID == favoritesID {
		tracks, err = p.fetchFavoriteTracks(ctx)
	} else {
		tracks, err = p.fetchPlaylistTracks(ctx, playlistID)
	}
	if err != nil {
		return nil, err
	}

	p.mu.Lock()
	p.tracksByID[playlistID] = tracks
	p.mu.Unlock()
	return tracks, nil
}

// fetchFavoriteTracks walks /users/{uid}/favorites/tracks.
func (p *Provider) fetchFavoriteTracks(ctx context.Context) ([]playlist.Track, error) {
	uid := p.session.UserID()
	if uid == "" {
		return nil, fmt.Errorf("tidal: no user id available")
	}

	var out []playlist.Track
	offset := 0
	for {
		q := url.Values{
			"limit":  {strconv.Itoa(pageSize)},
			"offset": {strconv.Itoa(offset)},
			"order":  {"DATE"},
			"orderDirection": {"DESC"},
		}
		resp, err := p.session.api(ctx, http.MethodGet,
			fmt.Sprintf(tidalFavoritesEP, uid)+"tracks", q, nil)
		if err != nil {
			return nil, fmt.Errorf("tidal: favorites: %w", err)
		}

		var result struct {
			Items []struct {
				Item apiTrack `json:"item"`
			} `json:"items"`
			TotalNumberOfItems int `json:"totalNumberOfItems"`
		}
		if err := decodeBody(resp, &result); err != nil {
			return nil, fmt.Errorf("tidal: parse favorites: %w", err)
		}

		for _, it := range result.Items {
			if t, ok := it.Item.toTrack(); ok {
				t.Favorite = true
				out = append(out, t)
			}
		}

		offset += len(result.Items)
		if len(result.Items) == 0 || offset >= result.TotalNumberOfItems {
			break
		}
	}
	return out, nil
}

// fetchPlaylistTracks walks /playlists/{uuid}/items.
func (p *Provider) fetchPlaylistTracks(ctx context.Context, uuid string) ([]playlist.Track, error) {
	var out []playlist.Track
	offset := 0
	for {
		q := url.Values{
			"limit":  {strconv.Itoa(pageSize)},
			"offset": {strconv.Itoa(offset)},
		}
		resp, err := p.session.api(ctx, http.MethodGet,
			tidalPlaylistsEP+uuid+"/items", q, nil)
		if err != nil {
			return nil, fmt.Errorf("tidal: playlist items: %w", err)
		}

		var result struct {
			Items []struct {
				Type string   `json:"type"` // "track" or "video"
				Item apiTrack `json:"item"`
			} `json:"items"`
			TotalNumberOfItems int `json:"totalNumberOfItems"`
		}
		if err := decodeBody(resp, &result); err != nil {
			return nil, fmt.Errorf("tidal: parse playlist items: %w", err)
		}

		for _, it := range result.Items {
			if it.Type != "" && it.Type != "track" {
				continue // skip videos — not playable as audio
			}
			if t, ok := it.Item.toTrack(); ok {
				out = append(out, t)
			}
		}

		offset += len(result.Items)
		if len(result.Items) == 0 || offset >= result.TotalNumberOfItems {
			break
		}
	}
	return out, nil
}

func (p *Provider) SearchTracks(ctx context.Context, query string, limit int) ([]playlist.Track, error) {
	if err := p.ensureSession(); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = maxSearchHits
	}
	if limit > maxSearchHits {
		limit = maxSearchHits
	}

	// Tidal's search rejects a lot of punctuation — strip defensively, same
	// as Vermilion's TS impl does.
	cleaned := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == ' ', r == '-':
			return r
		}
		return -1
	}, query)
	cleaned = strings.TrimSpace(cleaned)
	if cleaned == "" {
		return nil, nil
	}

	q := url.Values{
		"query":                  {cleaned},
		"includeContributors":    {"true"},
		"includeDidYouMean":      {"true"},
		"includeUserPlaylists":   {"false"},
		"limit":                  {strconv.Itoa(limit)},
		"types":                  {"TRACKS"},
		"locale":                 {"en_US"},
	}
	resp, err := p.session.api(ctx, http.MethodGet, tidalSearchEP, q, nil)
	if err != nil {
		return nil, fmt.Errorf("tidal: search: %w", err)
	}

	var result struct {
		Tracks struct {
			Items []apiTrack `json:"items"`
		} `json:"tracks"`
	}
	if err := decodeBody(resp, &result); err != nil {
		return nil, fmt.Errorf("tidal: parse search: %w", err)
	}

	out := make([]playlist.Track, 0, len(result.Tracks.Items))
	for _, it := range result.Tracks.Items {
		if t, ok := it.toTrack(); ok {
			out = append(out, t)
		}
	}
	return out, nil
}

// apiTrack is the common Tidal JSON shape used across search, playlist
// items, and favorites — unified so toTrack() has one conversion path.
type apiTrack struct {
	ID          any    `json:"id"` // sometimes number, sometimes string
	Title       string `json:"title"`
	Duration    int    `json:"duration"`
	TrackNumber int    `json:"trackNumber"`
	AudioQual   string `json:"audioQuality"` // e.g. "LOSSLESS"
	StreamReady *bool  `json:"streamReady"`
	Artist      struct {
		Name string `json:"name"`
	} `json:"artist"`
	Artists []struct {
		Name string `json:"name"`
	} `json:"artists"`
	Album struct {
		ID          any    `json:"id"`
		Title       string `json:"title"`
		ReleaseDate string `json:"releaseDate"`
	} `json:"album"`
}

// toTrack converts a Tidal API track to a cliamp playlist.Track. Returns
// (_, false) for tracks that are structurally unplayable (no id).
func (a apiTrack) toTrack() (playlist.Track, bool) {
	id := coerceID(a.ID)
	if id == "" {
		return playlist.Track{}, false
	}

	artistString := a.Artist.Name
	if len(a.Artists) > 0 {
		names := make([]string, 0, len(a.Artists))
		for _, ar := range a.Artists {
			if ar.Name != "" {
				names = append(names, ar.Name)
			}
		}
		if len(names) > 0 {
			artistString = strings.Join(names, ", ")
		}
	}

	year := 0
	if len(a.Album.ReleaseDate) >= 4 {
		if y, err := strconv.Atoi(a.Album.ReleaseDate[:4]); err == nil {
			year = y
		}
	}

	unplayable := a.StreamReady != nil && !*a.StreamReady

	return playlist.Track{
		Path:         "tidal:track:" + id,
		Title:        a.Title,
		Artist:       artistString,
		Album:        a.Album.Title,
		Year:         year,
		TrackNumber:  a.TrackNumber,
		DurationSecs: a.Duration,
		Unplayable:   unplayable,
	}, true
}

func (p *Provider) URISchemes() []string { return []string{"tidal:"} }

// NewStreamer resolves a tidal:* URI to a concrete playback source.
func (p *Provider) NewStreamer(uri string) (beep.StreamSeekCloser, beep.Format, time.Duration, error) {
	id, err := parseTrackURI(uri)
	if err != nil {
		return nil, beep.Format{}, 0, err
	}
	if err := p.ensureSession(); err != nil {
		return nil, beep.Format{}, 0, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Quality ladder: try user-preferred quality first. If LOSSLESS is
	// requested but no external bridge is configured, fall back to HIGH
	// with a clear warning — better than erroring out.
	quality := p.quality
	if quality == QualityLossless && p.externalCommand == "" {
		return nil, beep.Format{}, 0,
			fmt.Errorf("tidal: LOSSLESS requires [tidal].external_command — Widevine DRM cannot be decrypted by cliamp directly (see docs/tidal.md)")
	}

	if quality == QualityLossless {
		return p.newExternalStreamer(ctx, id)
	}
	return p.newAACStreamer(ctx, id, quality)
}

// newAACStreamer fetches the Tidal BTS manifest (unencrypted AAC/MP4 URL)
// and wraps it in an ffmpeg-backed StreamSeekCloser.
func (p *Provider) newAACStreamer(ctx context.Context, id, quality string) (beep.StreamSeekCloser, beep.Format, time.Duration, error) {
	info, err := p.playbackInfo(ctx, id, quality)
	if err != nil {
		return nil, beep.Format{}, 0, err
	}

	streamURL, err := info.resolveAACURL()
	if err != nil {
		return nil, beep.Format{}, 0,
			fmt.Errorf("tidal: resolve stream url: %w", err)
	}

	dur := p.cachedDuration(id)
	if dur == 0 {
		dur = p.lookupDuration(ctx, id)
	}

	return newFFmpegURLStreamer(streamURL, dur)
}

// cachedDuration looks up a track's duration from the per-playlist track
// cache — when the user hits play from the playlist view (the common path),
// we already have DurationSecs and can skip a round-trip.
func (p *Provider) cachedDuration(id string) time.Duration {
	uri := "tidal:track:" + id
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, tracks := range p.tracksByID {
		for i := range tracks {
			if tracks[i].Path == uri && tracks[i].DurationSecs > 0 {
				return time.Duration(tracks[i].DurationSecs) * time.Second
			}
		}
	}
	return 0
}

// newExternalStreamer invokes the configured external_command, expecting
// it to print a playable URL (HTTP) or local file path on stdout. cliamp
// then decodes that through ffmpeg as it does for AAC.
func (p *Provider) newExternalStreamer(ctx context.Context, id string) (beep.StreamSeekCloser, beep.Format, time.Duration, error) {
	tok, err := p.session.AccessToken(ctx)
	if err != nil {
		return nil, beep.Format{}, 0, err
	}
	src, dur, err := runExternalCommand(ctx, p.externalCommand, externalArgs{
		TrackID:     id,
		Quality:     QualityLossless,
		AccessToken: tok,
		SessionID:   p.session.SessionID(),
		CountryCode: p.session.CountryCode(),
	})
	if err != nil {
		return nil, beep.Format{}, 0, err
	}
	if dur == 0 {
		dur = p.cachedDuration(id)
	}
	if dur == 0 {
		dur = p.lookupDuration(ctx, id)
	}
	return newFFmpegURLStreamer(src, dur)
}

// lookupDuration fetches just the duration field from /tracks/{id}. Used
// when playbackinfo doesn't already tell us.
func (p *Provider) lookupDuration(ctx context.Context, id string) time.Duration {
	q := url.Values{"locale": {"en_US"}}
	resp, err := p.session.api(ctx, http.MethodGet, tidalTrackEP+id, q, nil)
	if err != nil {
		return 0
	}
	var t struct {
		Duration int `json:"duration"`
	}
	if err := decodeBody(resp, &t); err != nil {
		return 0
	}
	return time.Duration(t.Duration) * time.Second
}

// playbackInfo calls /tracks/{id}/playbackinfo and parses the returned
// manifest. The caller picks a sensible URL from it.
func (p *Provider) playbackInfo(ctx context.Context, id, quality string) (*playbackInfoResp, error) {
	q := url.Values{
		"audioquality":      {quality},
		"playbackmode":      {"STREAM"},
		"assetpresentation": {"FULL"},
	}
	resp, err := p.session.api(ctx, http.MethodGet,
		tidalTrackEP+id+tidalPlaybackInfo, q, nil)
	if err != nil {
		return nil, fmt.Errorf("tidal: playbackinfo: %w", err)
	}
	var info playbackInfoResp
	if err := decodeBody(resp, &info); err != nil {
		return nil, fmt.Errorf("tidal: parse playbackinfo: %w", err)
	}
	return &info, nil
}

// parseTrackURI extracts the track id from "tidal:track:<id>".
func parseTrackURI(uri string) (string, error) {
	const prefix = "tidal:track:"
	if !strings.HasPrefix(uri, prefix) {
		return "", fmt.Errorf("tidal: unsupported uri %q", uri)
	}
	id := strings.TrimPrefix(uri, prefix)
	if id == "" {
		return "", fmt.Errorf("tidal: empty track id in %q", uri)
	}
	return id, nil
}

// decodeBody reads a JSON response body then closes it. Size-limited to
// avoid a malicious/malformed response eating memory.
func decodeBody(resp *http.Response, v any) error {
	defer resp.Body.Close()
	return json.NewDecoder(io.LimitReader(resp.Body, maxResponseBody)).Decode(v)
}

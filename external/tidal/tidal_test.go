package tidal

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestParseTrackURI(t *testing.T) {
	tests := []struct {
		in      string
		wantID  string
		wantErr bool
	}{
		{"tidal:track:12345", "12345", false},
		{"tidal:track:abc-def", "abc-def", false},
		{"tidal:track:", "", true},
		{"spotify:track:xyz", "", true},
		{"", "", true},
		{"tidal:album:999", "", true},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			got, err := parseTrackURI(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got id=%q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.wantID {
				t.Fatalf("id = %q, want %q", got, tc.wantID)
			}
		})
	}
}

func TestResolveAACURL_BTS(t *testing.T) {
	manifest := `{"mimeType":"audio/mp4","codecs":"mp4a.40.5","encryptionType":"NONE","urls":["https://cdn.tidal.example/track.m4a?token=abc"]}`
	r := &playbackInfoResp{
		ManifestMimeType: "application/vnd.tidal.bts",
		Manifest:         base64.StdEncoding.EncodeToString([]byte(manifest)),
	}
	got, err := r.resolveAACURL()
	if err != nil {
		t.Fatalf("resolveAACURL: %v", err)
	}
	want := "https://cdn.tidal.example/track.m4a?token=abc"
	if got != want {
		t.Fatalf("url = %q, want %q", got, want)
	}
}

func TestResolveAACURL_EncryptedBTS(t *testing.T) {
	// Simulates Tidal unexpectedly returning an encrypted stream at HIGH.
	manifest := `{"mimeType":"audio/mp4","encryptionType":"OLD","urls":["https://cdn/track.m4a"]}`
	r := &playbackInfoResp{
		ManifestMimeType: "application/vnd.tidal.bts",
		Manifest:         base64.StdEncoding.EncodeToString([]byte(manifest)),
	}
	if _, err := r.resolveAACURL(); err == nil {
		t.Fatal("expected error for encrypted BTS manifest")
	}
}

func TestResolveAACURL_DRMDash(t *testing.T) {
	// A LOSSLESS playbackinfo would return DASH with a Widevine UUID.
	dash := `<?xml version="1.0"?><MPD><Period><AdaptationSet><ContentProtection schemeIdUri="urn:uuid:edef8ba9-79d6-4ace-a3c8-27dcd51d21ed"/></AdaptationSet></Period></MPD>`
	r := &playbackInfoResp{
		ManifestMimeType: "application/dash+xml",
		Manifest:         base64.StdEncoding.EncodeToString([]byte(dash)),
	}
	_, err := r.resolveAACURL()
	if err == nil {
		t.Fatal("expected error for DRM-protected DASH manifest")
	}
	if !strings.Contains(err.Error(), "DRM") {
		t.Fatalf("expected DRM error, got: %v", err)
	}
}

func TestResolveAACURL_PlainDash(t *testing.T) {
	// Unencrypted DASH with a BaseURL — rare but supported.
	dash := `<?xml version="1.0"?><MPD><BaseURL>https://cdn/track.m4s</BaseURL></MPD>`
	r := &playbackInfoResp{
		ManifestMimeType: "application/dash+xml",
		Manifest:         base64.StdEncoding.EncodeToString([]byte(dash)),
	}
	got, err := r.resolveAACURL()
	if err != nil {
		t.Fatalf("resolveAACURL: %v", err)
	}
	if got != "https://cdn/track.m4s" {
		t.Fatalf("got %q", got)
	}
}

func TestSplitArgv(t *testing.T) {
	tests := []struct {
		in   string
		want []string
	}{
		{`foo bar baz`, []string{"foo", "bar", "baz"}},
		{`cmd --token "abc def"`, []string{"cmd", "--token", "abc def"}},
		{`cmd 'single quoted' plain`, []string{"cmd", "single quoted", "plain"}},
		{`cmd --flag=value`, []string{"cmd", "--flag=value"}},
		{`cmd\ with\ spaces`, []string{"cmd with spaces"}},
		{`   spaced   `, []string{"spaced"}},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			got, err := splitArgv(tc.in)
			if err != nil {
				t.Fatalf("splitArgv: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("len = %d, want %d; got=%#v", len(got), len(tc.want), got)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("arg %d = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestSplitArgv_UnterminatedQuote(t *testing.T) {
	if _, err := splitArgv(`cmd "unterminated`); err == nil {
		t.Fatal("expected error for unterminated quote")
	}
}

func TestRenderTemplate(t *testing.T) {
	got := renderTemplate(`helper --id {id} --tok {token} q={quality}`, externalArgs{
		TrackID:     "42",
		Quality:     "LOSSLESS",
		AccessToken: "eyJxxx",
	})
	want := "helper --id 42 --tok eyJxxx q=LOSSLESS"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestApiTrackToTrack(t *testing.T) {
	// Minimal track with a string-typed id (Tidal sometimes does this).
	in := apiTrack{
		ID:          "99",
		Title:       "Hello",
		Duration:    185,
		TrackNumber: 3,
	}
	in.Artist.Name = "World"
	in.Album.Title = "Greetings"
	in.Album.ReleaseDate = "2024-01-15"

	got, ok := in.toTrack()
	if !ok {
		t.Fatal("toTrack returned !ok for valid input")
	}
	if got.Path != "tidal:track:99" {
		t.Fatalf("Path = %q", got.Path)
	}
	if got.Year != 2024 {
		t.Fatalf("Year = %d", got.Year)
	}
	if got.Artist != "World" {
		t.Fatalf("Artist = %q", got.Artist)
	}
	if got.DurationSecs != 185 {
		t.Fatalf("DurationSecs = %d", got.DurationSecs)
	}
}

func TestApiTrackToTrack_MultiArtist(t *testing.T) {
	in := apiTrack{ID: float64(7), Title: "X"}
	in.Artists = []struct {
		Name string `json:"name"`
	}{{Name: "A"}, {Name: "B"}, {Name: ""}, {Name: "C"}}
	got, ok := in.toTrack()
	if !ok {
		t.Fatal("!ok")
	}
	if got.Artist != "A, B, C" {
		t.Fatalf("Artist = %q", got.Artist)
	}
	if got.Path != "tidal:track:7" {
		t.Fatalf("Path = %q", got.Path)
	}
}

func TestApiTrackToTrack_EmptyID(t *testing.T) {
	if _, ok := (apiTrack{}).toTrack(); ok {
		t.Fatal("expected !ok for empty id")
	}
}

func TestConfigIsSet(t *testing.T) {
	if (Config{}).IsSet() {
		t.Fatal("empty config should not be IsSet")
	}
	if (Config{ClientID: "x"}).IsSet() {
		t.Fatal("missing refresh_token should not be IsSet")
	}
	if !(Config{ClientID: "x", RefreshToken: "y"}).IsSet() {
		t.Fatal("both fields set should be IsSet")
	}
}

func TestNewNormalizesQuality(t *testing.T) {
	cases := map[string]string{
		"":         QualityHigh, // default
		"hi_res":   QualityHigh, // unknown → default
		"high":     QualityHigh,
		" LOW ":    QualityLow,
		"lossless": QualityLossless,
	}
	for in, want := range cases {
		got := New(Config{ClientID: "x", RefreshToken: "y", Quality: in}).quality
		if got != want {
			t.Fatalf("Quality(%q) = %q, want %q", in, got, want)
		}
	}
}

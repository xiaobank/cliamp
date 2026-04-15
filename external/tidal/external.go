package tidal

// External bridge for LOSSLESS / HI_RES Tidal playback.
//
// cliamp cannot decrypt Widevine-protected streams itself (see the package
// docstring in session.go). As an escape hatch, users who already have a
// Widevine-capable helper — whether a licensed desktop app that exposes a
// URL hook, or an extracted-CDM tool installed at their own risk — can
// point [tidal].external_command at it.
//
// The CONTRACT with the external command:
//
//   - cliamp runs it once per play, passing the track id via the command
//     line template.
//   - The command must print a SINGLE line to stdout: either an HTTP(S)
//     URL to an unencrypted audio file or a local filesystem path.
//   - cliamp then decodes that URL/path through ffmpeg like any other
//     stream. The external helper is responsible for obtaining the DRM
//     license, decrypting the stream, and exposing the plaintext output.
//   - Exit 0 = success; non-zero = surface stderr to the user.
//   - Must complete within 30 seconds.
//
// Template placeholders:
//
//   {id}            — Tidal track id (e.g. "12345678")
//   {quality}       — requested quality tier ("LOSSLESS")
//   {token}         — current Tidal bearer token (avoids re-auth in the bridge)
//   {session_id}    — Tidal sessionId (needed for Tidal's license endpoint)
//   {country_code}  — two-letter country code associated with the account
//
// Example config:
//
//   [tidal]
//   client_id      = "..."
//   refresh_token  = "..."
//   quality        = "LOSSLESS"
//   external_command = "tidal-url-helper --id {id} --token {token}"
//
// The helper binary is NOT provided by cliamp. Users integrate at their own
// risk and responsibility under Tidal's Terms of Service.

import (
	"bufio"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// externalArgs carries the substitution values the template may reference.
type externalArgs struct {
	TrackID     string
	Quality     string
	AccessToken string
	SessionID   string
	CountryCode string
}

// externalTimeout caps how long we wait for a bridge helper before giving
// up. 30s matches the Spotify NewStream timeout pattern.
const externalTimeout = 30 * time.Second

// runExternalCommand renders the template, spawns the helper, and returns
// whatever URL or path it prints on stdout (plus optionally a duration
// hint if the helper chose to print it as a second line).
//
// Duration hint protocol (optional, backward-compatible): if the helper
// prints a second stdout line of the form "duration=SECONDS", we use
// that for the progress bar. First line is always the playable source.
func runExternalCommand(ctx context.Context, template string, args externalArgs) (string, time.Duration, error) {
	if strings.TrimSpace(template) == "" {
		return "", 0, fmt.Errorf("tidal: external_command is empty")
	}

	rendered := renderTemplate(template, args)
	argv, err := splitArgv(rendered)
	if err != nil {
		return "", 0, fmt.Errorf("tidal: parse external_command: %w", err)
	}
	if len(argv) == 0 {
		return "", 0, fmt.Errorf("tidal: external_command rendered to empty command")
	}

	ctx, cancel := context.WithTimeout(ctx, externalTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	// Do NOT expose the environment by default — sensitive tokens are
	// passed explicitly via placeholders only.
	cmd.Env = nil

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", 0, fmt.Errorf("tidal: external stdout pipe: %w", err)
	}
	var stderr strings.Builder
	cmd.Stderr = lineCap{w: &stderr, limit: 4096}

	if err := cmd.Start(); err != nil {
		return "", 0, fmt.Errorf("tidal: external start: %w", err)
	}

	// Read first line = playable source; optional second line = duration hint.
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 4096), 64*1024)

	var source string
	var dur time.Duration
	if scanner.Scan() {
		source = strings.TrimSpace(scanner.Text())
	}
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "duration=") {
			if secs, perr := parseFloatSeconds(strings.TrimPrefix(line, "duration=")); perr == nil {
				dur = secs
			}
		}
	}

	waitErr := cmd.Wait()
	if waitErr != nil {
		return "", 0, fmt.Errorf("tidal: external_command failed: %w: %s",
			waitErr, truncate(stderr.String(), 500))
	}
	if source == "" {
		return "", 0, fmt.Errorf("tidal: external_command produced no output")
	}
	return source, dur, nil
}

// renderTemplate performs literal placeholder substitution. Deliberately
// not using text/template so a user's tokens (which may contain Go template
// metachars) can't trigger parse errors.
func renderTemplate(tmpl string, args externalArgs) string {
	repl := strings.NewReplacer(
		"{id}", args.TrackID,
		"{quality}", args.Quality,
		"{token}", args.AccessToken,
		"{session_id}", args.SessionID,
		"{country_code}", args.CountryCode,
	)
	return repl.Replace(tmpl)
}

// splitArgv does shell-like argv splitting, supporting quoted strings so
// a template like `mycli --token "{token}"` survives a token with spaces.
// Returns an error only on unterminated quotes.
func splitArgv(s string) ([]string, error) {
	var argv []string
	var cur strings.Builder
	inSingle, inDouble := false, false
	escape := false
	flush := func() {
		if cur.Len() > 0 || inSingle || inDouble {
			argv = append(argv, cur.String())
			cur.Reset()
		}
	}
	for _, r := range s {
		if escape {
			cur.WriteRune(r)
			escape = false
			continue
		}
		switch {
		case r == '\\' && !inSingle:
			escape = true
		case r == '\'' && !inDouble:
			inSingle = !inSingle
		case r == '"' && !inSingle:
			inDouble = !inDouble
		case (r == ' ' || r == '\t') && !inSingle && !inDouble:
			flush()
		default:
			cur.WriteRune(r)
		}
	}
	if inSingle || inDouble {
		return nil, fmt.Errorf("unterminated quoted string")
	}
	flush()
	return argv, nil
}

// parseFloatSeconds parses "N" or "N.M" as a duration in seconds.
func parseFloatSeconds(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty")
	}
	// Use time.ParseDuration first so "3m20s" also works.
	if d, err := time.ParseDuration(s); err == nil {
		return d, nil
	}
	// Fallback: treat as seconds.
	var secs float64
	if _, err := fmt.Sscanf(s, "%f", &secs); err != nil {
		return 0, err
	}
	return time.Duration(secs * float64(time.Second)), nil
}

// lineCap is an io.Writer that caps total bytes written. Used to cap
// stderr captured from the helper so a chatty tool can't balloon memory.
type lineCap struct {
	w     *strings.Builder
	limit int
}

func (l lineCap) Write(p []byte) (int, error) {
	remaining := l.limit - l.w.Len()
	if remaining <= 0 {
		return len(p), nil
	}
	if len(p) > remaining {
		l.w.Write(p[:remaining])
		return len(p), nil
	}
	l.w.Write(p)
	return len(p), nil
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

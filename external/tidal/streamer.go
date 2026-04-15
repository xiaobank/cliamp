package tidal

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"math"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/gopxl/beep/v2"
)

// Fixed output format we decode into. Picking a single rate keeps the
// streamer simple; cliamp's player resamples to the device rate if needed.
const (
	outSampleRate = 44100
	outChannels   = 2
)

// pipeBufSize matches player/ytdl.go for consistency with the rest of the
// ffmpeg-pipe machinery.
const pipeBufSize = 64 * 1024

// playbackInfoResp is the JSON shape Tidal returns from
// /v1/tracks/{id}/playbackinfo. Only the fields we actually use are
// modeled; the rest is ignored.
type playbackInfoResp struct {
	TrackID             int    `json:"trackId"`
	AudioMode           string `json:"audioMode"`
	AudioQuality        string `json:"audioQuality"`
	ManifestMimeType    string `json:"manifestMimeType"`
	Manifest            string `json:"manifest"` // base64-encoded body
	SampleRate          int    `json:"sampleRate"`
	BitDepth            int    `json:"bitDepth"`
	LicenseSecurityToken string `json:"licenseSecurityToken"`
}

// resolveAACURL inspects the (usually base64-JSON) BTS manifest returned
// for HIGH / LOW quality and extracts a direct, plain-HTTP streaming URL.
//
// Tidal's two relevant manifest flavors for unencrypted AAC:
//   - "application/vnd.tidal.bts"  → base64-JSON with {"urls":[...]}
//   - "application/dash+xml"       → base64 DASH. Normally means Widevine,
//     but we try to read out the <BaseURL> anyway for forward compat.
func (r *playbackInfoResp) resolveAACURL() (string, error) {
	raw, err := base64.StdEncoding.DecodeString(r.Manifest)
	if err != nil {
		return "", fmt.Errorf("decode manifest: %w", err)
	}

	switch r.ManifestMimeType {
	case "application/vnd.tidal.bts", "application/vnd.tidal.bts+json":
		var bts struct {
			MimeType       string   `json:"mimeType"`
			Codecs         string   `json:"codecs"`
			EncryptionType string   `json:"encryptionType"`
			KeyID          string   `json:"keyId"`
			URLs           []string `json:"urls"`
		}
		if err := json.Unmarshal(raw, &bts); err != nil {
			return "", fmt.Errorf("parse bts manifest: %w", err)
		}
		// EncryptionType "NONE" is what we want; anything else means Tidal
		// stuck encryption on a nominally-plain tier and we can't decode.
		if bts.EncryptionType != "" && bts.EncryptionType != "NONE" {
			return "", fmt.Errorf("tidal returned encrypted stream (%s) at non-lossless quality — try LOW quality or use external_command", bts.EncryptionType)
		}
		if len(bts.URLs) == 0 {
			return "", fmt.Errorf("bts manifest has no urls")
		}
		return bts.URLs[0], nil

	case "application/dash+xml":
		// This is a DRM-protected DASH manifest — refuse loudly so the
		// caller knows to either switch quality or configure the bridge.
		if hasWidevine(raw) {
			return "", fmt.Errorf("tidal returned DRM-protected DASH manifest — switch to HIGH/LOW or configure [tidal].external_command")
		}
		// Rare: unencrypted DASH. Try to pull a <BaseURL> out.
		if u := firstBaseURL(raw); u != "" {
			return u, nil
		}
		return "", fmt.Errorf("dash manifest has no usable BaseURL")

	default:
		return "", fmt.Errorf("unknown manifest type %q", r.ManifestMimeType)
	}
}

// drmMarkers are substrings whose presence in a DASH manifest indicates
// the stream is encrypted and cannot be decoded without a CDM.
var drmMarkers = [][]byte{
	[]byte("edef8ba9-79d6-4ace-a3c8-27dcd51d21ed"), // Widevine
	[]byte("9a04f079-9840-4286-ab92-e65be0885f95"), // PlayReady
	[]byte("urn:mpeg:dash:mp4protection"),
}

func hasWidevine(dash []byte) bool {
	for _, m := range drmMarkers {
		if bytes.Contains(dash, m) {
			return true
		}
	}
	return false
}

// firstBaseURL pulls the first <BaseURL>...</BaseURL> out of a DASH
// manifest without loading a full MPD parser.
func firstBaseURL(dash []byte) string {
	type BaseURLTag struct {
		Value string `xml:",chardata"`
	}
	dec := xml.NewDecoder(bytes.NewReader(dash))
	for {
		tok, err := dec.Token()
		if err != nil {
			return ""
		}
		if se, ok := tok.(xml.StartElement); ok && se.Name.Local == "BaseURL" {
			var b BaseURLTag
			if err := dec.DecodeElement(&b, &se); err == nil {
				return strings.TrimSpace(b.Value)
			}
		}
	}
}

// ffmpegURLStreamer decodes a plain-HTTP AAC/MP4 URL (or local path)
// through ffmpeg, producing a beep.StreamSeekCloser that streams PCM
// as it arrives instead of buffering the whole track.
//
// Seek = kill + restart ffmpeg with -ss (demuxer-level fast seek).
type ffmpegURLStreamer struct {
	url    string
	cmd    *exec.Cmd
	pipe   io.ReadCloser
	reader *bufio.Reader

	pos   int // sample frames emitted
	total int // total sample frames (0 if unknown)
	err   error
	buf   [outChannels * 4]byte // one f32le stereo frame = 8 bytes
}

// newFFmpegURLStreamer returns a streamer that pulls PCM from `ffmpeg -i url`.
// duration is the track length used to compute total sample frames for the
// progress bar; pass 0 if unknown.
func newFFmpegURLStreamer(streamURL string, duration time.Duration) (beep.StreamSeekCloser, beep.Format, time.Duration, error) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return nil, beep.Format{}, 0,
			fmt.Errorf("tidal: ffmpeg is required to decode AAC — install it with your package manager")
	}
	s := &ffmpegURLStreamer{url: streamURL}
	if duration > 0 {
		s.total = int(float64(duration.Seconds()) * float64(outSampleRate))
	}
	if err := s.start(0); err != nil {
		return nil, beep.Format{}, 0, err
	}
	format := beep.Format{
		SampleRate:  beep.SampleRate(outSampleRate),
		NumChannels: outChannels,
		Precision:   4, // f32le
	}
	return s, format, duration, nil
}

// start spawns ffmpeg, seeking to `seekPos` sample frames if non-zero.
// Output is stereo f32le at outSampleRate Hz on stdout.
func (s *ffmpegURLStreamer) start(seekPos int) error {
	args := make([]string, 0, 14)
	if seekPos > 0 {
		secs := float64(seekPos) / float64(outSampleRate)
		args = append(args, "-ss", strconv.FormatFloat(secs, 'f', 3, 64))
	}
	args = append(args,
		"-i", s.url,
		"-f", "f32le",
		"-acodec", "pcm_f32le",
		"-ar", strconv.Itoa(outSampleRate),
		"-ac", strconv.Itoa(outChannels),
		"-loglevel", "error",
		"pipe:1",
	)
	cmd := exec.Command("ffmpeg", args...)
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("ffmpeg pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("ffmpeg start: %w", err)
	}
	s.cmd = cmd
	s.pipe = pipe
	s.reader = bufio.NewReaderSize(pipe, pipeBufSize)
	s.pos = seekPos
	s.err = nil
	return nil
}

// stop kills the running ffmpeg process and cleans up its pipes. Safe to
// call multiple times.
func (s *ffmpegURLStreamer) stop() {
	if s.pipe != nil {
		s.pipe.Close()
	}
	if s.cmd != nil && s.cmd.Process != nil {
		s.cmd.Process.Kill()
		_ = s.cmd.Wait()
	}
}

// Stream reads f32le frames from ffmpeg and decodes them into beep samples.
func (s *ffmpegURLStreamer) Stream(samples [][2]float64) (int, bool) {
	n := 0
	for i := range samples {
		if _, err := io.ReadFull(s.reader, s.buf[:]); err != nil {
			if err != io.EOF && err != io.ErrUnexpectedEOF {
				s.err = err
			}
			break
		}
		samples[i] = [2]float64{
			float64(math.Float32frombits(binary.LittleEndian.Uint32(s.buf[0:4]))),
			float64(math.Float32frombits(binary.LittleEndian.Uint32(s.buf[4:8]))),
		}
		n++
	}
	s.pos += n
	return n, n > 0
}

func (s *ffmpegURLStreamer) Err() error    { return s.err }
func (s *ffmpegURLStreamer) Len() int      { return s.total }
func (s *ffmpegURLStreamer) Position() int { return s.pos }

// Seek repositions playback by killing ffmpeg and restarting at -ss N.
// pos is in sample frames at outSampleRate.
func (s *ffmpegURLStreamer) Seek(pos int) error {
	if pos < 0 {
		pos = 0
	}
	if s.total > 0 && pos > s.total {
		pos = s.total
	}
	s.stop()
	return s.start(pos)
}

func (s *ffmpegURLStreamer) Close() error {
	s.stop()
	return nil
}

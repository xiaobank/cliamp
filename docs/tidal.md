# Tidal Integration

Cliamp can browse and play [Tidal](https://tidal.com/) through its audio pipeline — EQ, visualizer, and all other effects apply. Requires an active Tidal subscription.

> [!IMPORTANT]
> The Tidal integration is **unofficial**. It talks to the same private API the listen.tidal.com web player uses. Tidal does not officially support third-party clients — you are using it at your own risk and under their Terms of Service.

## What works

| Quality | Format | Plays in cliamp? |
|---|---|---|
| **LOW** | 96 kbps AAC | Yes — native ffmpeg pipeline |
| **HIGH** | 320 kbps AAC | Yes — native ffmpeg pipeline |
| **LOSSLESS** | 16-bit FLAC (MPEG-DASH) | Only via an external helper — see [Lossless playback](#lossless-playback) |
| **HI_RES / HI_RES_LOSSLESS** | 24-bit FLAC / MQA | Only via an external helper |

### Why no native lossless?

Tidal's lossless and hi-res streams are protected by **Widevine DRM**. Widevine is Google's closed-source DRM system — the decryption module (CDM) is only shipped to licensed platforms (Chromium, Android, Electron). A Go terminal application cannot ship or embed the CDM, and a third-party-extracted CDM would violate both Tidal's ToS and the DMCA.

The `external_command` hook (below) is an escape hatch: if you already run a Widevine-capable helper (a desktop Tidal client, or a tool you installed at your own risk that extracts and caches decrypted audio), cliamp will happily play whatever URL or file it hands back.

## Setup

### Getting your credentials

Tidal does not issue public API credentials. You copy them from the web player's OAuth response:

1. Log in to [listen.tidal.com](https://listen.tidal.com).
2. Open DevTools (`F12`) and switch to the **Network** tab.
3. Type `token` into the filter bar.
4. Refresh the page. You should see a request to `auth.tidal.com/v1/oauth2/token`.
5. Click the request, open the **Response** tab.
6. Copy the `client_id` and `refresh_token` values.

> [!WARNING]
> Treat your `refresh_token` like a password. Anyone with it has full access to your account. Do not commit it to git.

### Configuring cliamp

Add the `[tidal]` section to `~/.config/cliamp/config.toml`:

```toml
[tidal]
client_id     = "your-client-id-from-devtools"
refresh_token = "your-refresh-token-from-devtools"
quality       = "HIGH"   # LOW | HIGH | LOSSLESS
```

The provider appears in the picker on next launch.

## Usage

Open the provider browser with `Esc`/`b` and select **Tidal**. You'll see:

- **Your Music** — everything you've hearted (favorites).
- Every playlist you've created.

Navigate with arrow keys, press Enter to load a playlist. Tracks stream through cliamp's pipeline, so EQ, visualizer, mono, shuffle, repeat all work.

### Controls

| Key | Action |
|---|---|
| `Up` `Down` / `j` `k` | Navigate playlists |
| `Enter` | Load the selected playlist |
| `Tab` | Switch between provider and playlist focus |
| `Esc` / `b` | Open provider browser |

## Lossless playback

Setting `quality = "LOSSLESS"` without configuring `external_command` will cause playback to fail with a descriptive error — cliamp cannot decrypt Widevine streams itself.

If you have a helper binary that knows how to produce a plaintext URL or local file for a given Tidal track id, point `external_command` at it:

```toml
[tidal]
client_id        = "..."
refresh_token    = "..."
quality          = "LOSSLESS"
external_command = "my-tidal-helper --id {id} --token {token}"
```

### External command contract

When a LOSSLESS track is requested, cliamp invokes the command with these placeholders substituted:

| Placeholder | Value |
|---|---|
| `{id}` | Tidal track id (numeric string) |
| `{quality}` | `"LOSSLESS"` |
| `{token}` | Current Tidal bearer access token |
| `{session_id}` | Tidal sessionId (for the helper's license request) |
| `{country_code}` | Two-letter ISO country code for the account |

Expected behavior:

- The command must run to completion within **30 seconds**.
- On success, it **MUST print one line to stdout**: either an `http://` / `https://` URL, or a local filesystem path, pointing at plaintext audio (anything ffmpeg can decode — FLAC, WAV, M4A, …).
- Optionally, it may print a **second line** in the form `duration=SECONDS` (float or `time.ParseDuration` format) to hint the track length for the seek bar. Without this, cliamp falls back to `/v1/tracks/{id}` for the duration.
- Exit code 0 means success. Non-zero exit codes surface the helper's stderr (capped at 500 bytes) back to the user.
- Quotes work as you'd expect: `external_command = 'helper --arg "value with spaces"'`.

Cliamp does **not** ship or recommend any specific helper. If you write or use one, that's on you under Tidal's ToS.

## Troubleshooting

- **"tidal: authentication failed"** — your `refresh_token` is stale. Grab a fresh one from listen.tidal.com devtools (Tidal rotates them periodically).
- **"tidal returned DRM-protected DASH manifest"** — you asked for a quality tier Tidal decided to encrypt. Downgrade to `quality = "HIGH"` or set up `external_command`.
- **Tracks are missing / skip silently** — Tidal flagged them with `streamReady = false` for your country or tier. Nothing cliamp can do.
- **"ffmpeg is required"** — install `ffmpeg` with your package manager. Tidal streams AAC in MP4, which ffmpeg handles natively.
- **Playlist doesn't appear** — only playlists you own or created show up. Save someone else's shared playlist into your library first via the web player.

## Requirements

- Active Tidal subscription (any tier works for LOW/HIGH; the lossless bridge is on you).
- `ffmpeg` installed on your `PATH`.
- `client_id` + `refresh_token` copied from listen.tidal.com.

## Security note

Credentials live in `~/.config/cliamp/config.toml` in plaintext. This file should have mode `0600`:

```bash
chmod 600 ~/.config/cliamp/config.toml
```

Cliamp never sends your credentials anywhere except `auth.tidal.com` and `listen.tidal.com`.

# Twitch Watcher

This Go service monitors the Twitch usernames in `config.json`. When any configured streamer goes live, it starts an authenticated Steel browser, waits for the stream video to load, and keeps that page open for six minutes. Multiple live streamers are watched concurrently. Completed stream IDs are saved separately for each username in `state.json`; a failed page load is retried on a later poll.

## Private configuration

On the Windows machine that has the logged-in Firefox profile and the existing Twitch and DiscoveryProject configuration, run:

```powershell
.\setup-windows.ps1 -Channels streamer_one,streamer_two
go run . -check
go run . -probe
```

Set `channels` to one or more Twitch usernames (up to 100); the names above are placeholders. After the first setup, running the script without `-Channels` preserves the existing list. Existing single-`channel` configs still work and are migrated by the setup script. The script reads active Twitch cookies directly from Firefox, including cookies hidden from DevTools JavaScript. It also reads the Twitch API token, Steel key, and the existing recorder's developer Telegram bot and chat settings. It writes `config.json` locally; Git ignores that file. Run the script again when the Firefox login or tokens change. `-check` validates the Twitch API token and reports live status for each configured channel without opening Steel. `-probe` starts and releases one short Steel session to verify the cloud browser can open Twitch with the Firefox login.

Steel cannot reach the Windows `127.0.0.1` 3proxy listener. Setup reads warmer's existing 3proxy credentials from `C:\3proxy\3proxy.cfg` and configures Steel to use warmer's direct authenticated HTTP 3proxy at `104.219.236.83:7743`. No stunnel is used by the watcher. If that server, port, or credentials change, update setup or pass `-ProxyUrl 'http://user:password@public-host:port'`. Steel's session API rejected a SOCKS5 proxy URL, so the watcher requires HTTP(S). DataImpulse endpoints are rejected.

Do not commit or paste `config.json`. It contains a transferable Twitch login cookie and API credentials. The example file documents its shape.

## Linux deployment

Install Go 1.24 or newer, Node.js/npm, and PM2 on the server. Clone the repository, copy the private `config.json` to the repository directory using a secure transfer, then run:

```bash
npm run deploy
```

The deploy script builds the Go binary and starts or restarts `twitch-watcher` under PM2, matching the deploy command in the existing Twitch recorder. It does not start a watcher on this Windows machine. Check `pm2 logs twitch-watcher` for the live check, video-loaded message, and completed stream ID. Keep `config.json` readable only by the account running PM2.

The watcher checks that Twitch's visible video element has loaded, then leaves that same Steel browser page open for `watch_seconds` (six minutes by default). It does not sample or total playback time after the initial load check. Steel's inactivity timeout is extended past the hold period so an idle control connection does not close the page. If the video cannot load, the watcher tries up to three fresh sessions; it never combines time from different pages. Every watch session has a nine-minute hard timeout, including login and video loading, so it cannot run for ten minutes and Steel closes it even if the watcher process crashes before releasing it. `watch_seconds` can be 1–360 seconds. A stream is marked complete after the hold finishes. Adding more live channels starts more simultaneous Steel sessions and can increase usage. Old single-stream `state.json` files migrate to the first configured channel (or the legacy `channel` field if present).

The service validates the saved Firefox login token at startup and hourly. It also checks the browser UI before starting the six-minute hold. If Twitch rejects the token or shows the signed-out login button, it stops watching and sends one Telegram alert to the configured developer chat. The alert flag is saved in `state.json` so restarts do not repeat it. Failed Telegram sends are retried on later polls. If Telegram succeeds but saving state fails, a restart may repeat the alert. After the login is restored, the service rechecks the browser each minute before resuming.

# Twitch Watcher

This Go service monitors the Twitch usernames in `config.json`. When any configured streamer goes live, it starts an authenticated Steel browser for that stream and watches five minutes of advancing video. Multiple live streamers are watched concurrently. Completed stream IDs are saved separately for each username in `state.json`; a failed watch is retried on a later poll.

## Private configuration

On the Windows machine that has the logged-in Firefox profile and the existing Twitch and DiscoveryProject configuration, run:

```powershell
.\setup-windows.ps1 -Channels streamer_one,streamer_two
go run . -check
go run . -probe
```

Set `channels` to one or more Twitch usernames (up to 100); the names above are placeholders. After the first setup, running the script without `-Channels` preserves the existing list. Existing single-`channel` configs still work and are migrated by the setup script. The script reads active Twitch cookies directly from Firefox, including cookies hidden from DevTools JavaScript. It also reads the Twitch API token, Steel key, and the existing recorder's developer Telegram bot and chat settings. It writes `config.json` locally; Git ignores that file. Run the script again when the Firefox login or tokens change. `-check` validates the Twitch API token and reports live status for each configured channel without opening Steel. `-probe` starts and releases one short Steel session to verify the cloud browser can open Twitch with the Firefox login.

The Windows `3proxy` listener is local to that machine and forwards into a TLS-wrapped SOCKS proxy via stunnel. Steel's cloud browser cannot connect to `127.0.0.1` on this machine, and Steel's proxy URL does not describe that TLS-wrapped SOCKS chain. By default, setup uses the reachable DataImpulse proxy already configured for Steel in DiscoveryProject. To use an authenticated public HTTP(S) or SOCKS5 endpoint for the 3proxy chain instead, pass `-ProxyUrl 'http://user:password@public-host:port'` to the setup script. Test that endpoint from outside your local network before deploying.

Do not commit or paste `config.json`. It contains a transferable Twitch login cookie and API credentials. The example file documents its shape.

## Linux deployment

Install Go 1.24 or newer, Node.js/npm, and PM2 on the server. Clone the repository, copy the private `config.json` to the repository directory using a secure transfer, then run:

```bash
npm run deploy
```

The deploy script builds the Go binary and starts or restarts `twitch-watcher` under PM2, matching the deploy command in the existing Twitch recorder. It does not start a watcher on this Windows machine. Check `pm2 logs twitch-watcher` for the live check, playback confirmation, and completed stream ID. Keep `config.json` readable only by the account running PM2.

Steel sessions have a hard timeout with extra time for navigation and release. The watcher checks video time every five seconds, tolerates brief buffering, and reports an error if playback cannot start or stalls for 30 seconds. A successful watch is recorded only after five minutes of advancing playback. Each stream is watched once, even across service restarts. Adding more live channels starts more simultaneous Steel sessions and can increase usage. Old single-stream `state.json` files migrate to the first configured channel (or the legacy `channel` field if present).

The service validates the saved Firefox login token at startup and hourly. It also checks the browser UI before and during each watch. If Twitch rejects the token or shows the signed-out login button, it stops watching and sends one Telegram alert to the configured developer chat. The alert flag is saved in `state.json` so restarts do not repeat it. Failed Telegram sends are retried on later polls. If Telegram succeeds but saving state fails, a restart may repeat the alert. After the login is restored, the service rechecks the browser each minute before resuming.

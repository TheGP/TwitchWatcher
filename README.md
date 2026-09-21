# Twitch Watcher

This Go service checks whether `shaneboehm` is live. For each new Twitch stream ID, it starts one authenticated Steel browser, confirms that video playback advances for five minutes, releases the Steel session, and stores the completed stream ID in `state.json`. It retries a failed watch on the next poll.

## Private configuration

On the Windows machine that has the logged-in Firefox profile and the existing Twitch and DiscoveryProject configuration, run:

```powershell
.\setup-windows.ps1
go run . -check
go run . -probe
```

The setup script reads active Twitch cookies directly from Firefox, including cookies hidden from DevTools JavaScript. It also reads the Twitch API token, Steel key, and the existing recorder's developer Telegram bot and chat settings. It writes `config.json` locally; Git ignores that file. Run the script again when the Firefox login or tokens change. `-check` validates the Twitch API token and reports whether the channel is currently live without opening a Steel session. `-probe` starts and releases one short Steel session to verify the cloud browser can open Twitch with the Firefox login.

The Windows `3proxy` listener is local to that machine and forwards into a TLS-wrapped SOCKS proxy via stunnel. Steel's cloud browser cannot connect to `127.0.0.1` on this machine, and Steel's proxy URL does not describe that TLS-wrapped SOCKS chain. By default, setup uses the reachable DataImpulse proxy already configured for Steel in DiscoveryProject. To use an authenticated public HTTP(S) or SOCKS5 endpoint for the 3proxy chain instead, pass `-ProxyUrl 'http://user:password@public-host:port'` to the setup script. Test that endpoint from outside your local network before deploying.

Do not commit or paste `config.json`. It contains a transferable Twitch login cookie and API credentials. The example file documents its shape.

## Linux deployment

Install Go 1.24 or newer, Node.js/npm, and PM2 on the server. Clone the repository, copy the private `config.json` to the repository directory using a secure transfer, then run:

```bash
npm run deploy
```

The deploy script builds the Go binary and starts or restarts `twitch-watcher` under PM2, matching the deploy command in the existing Twitch recorder. It does not start a watcher on this Windows machine. Check `pm2 logs twitch-watcher` for the live check, playback confirmation, and completed stream ID. Keep `config.json` readable only by the account running PM2.

Steel sessions have a hard timeout with extra time for navigation and release. The watcher checks video time every five seconds, tolerates brief buffering, and reports an error if playback cannot start or stalls for 30 seconds. A successful watch is recorded only after five minutes of advancing playback. One stream is watched once, even across service restarts.

The service validates the saved Firefox login token at startup and hourly. It also checks the browser UI before and during each watch. If Twitch rejects the token or shows the signed-out login button, it stops watching and sends one Telegram alert to the configured developer chat. The alert flag is saved in `state.json` so restarts do not repeat it. Failed Telegram sends are retried on later polls. If Telegram succeeds but saving state fails, a restart may repeat the alert. After the login is restored, the service rechecks the browser each minute before resuming.

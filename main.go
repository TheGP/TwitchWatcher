package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"syscall"
	"time"
)

const steelAPI = "https://api.steel.dev/v1/sessions"

var twitchLogin = regexp.MustCompile(`^[A-Za-z0-9_]{3,25}$`)

type cookie struct {
	Name     string `json:"name"`
	Value    string `json:"value"`
	Domain   string `json:"domain"`
	Path     string `json:"path"`
	Secure   bool   `json:"secure"`
	HTTPOnly bool   `json:"http_only"`
	Expires  int64  `json:"expires"`
}

type config struct {
	Channel      string   `json:"channel,omitempty"` // Legacy single-channel config.
	Channels     []string `json:"channels"`
	PollSeconds  int      `json:"poll_seconds"`
	WatchSeconds int      `json:"watch_seconds"`
	TwitchToken  string   `json:"twitch_token"`
	SteelAPIKey  string   `json:"steel_api_key"`
	TelegramBot  string   `json:"telegram_bot"`
	TelegramChat string   `json:"telegram_chat"`
	ProxyURL     string   `json:"proxy_url"`
	Cookies      []cookie `json:"cookies"`
}

type state struct {
	CompletedStreamID string            `json:"completed_stream_id,omitempty"` // Legacy state.
	CompletedStreams  map[string]string `json:"completed_streams,omitempty"`
	AuthLost          bool              `json:"auth_lost"`
	AuthAlerted       bool              `json:"auth_alerted"`
}

type steelSession struct {
	ID           string `json:"id"`
	WebsocketURL string `json:"websocketUrl"`
}

func main() {
	configPath := flag.String("config", "config.json", "private config file")
	statePath := flag.String("state", "state.json", "completed stream state file")
	check := flag.Bool("check", false, "validate config and query live status once without opening Steel")
	probe := flag.Bool("probe", false, "open and release one Steel browser to verify the Twitch login")
	flag.Parse()

	cfg, err := loadConfig(*configPath)
	if err != nil {
		log.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	client := &http.Client{Timeout: 30 * time.Second}
	clientID, err := validateTwitchToken(ctx, client, cfg.TwitchToken)
	if err != nil {
		log.Fatal(err)
	}
	if *check {
		if _, err := validateTwitchToken(ctx, client, viewerToken(cfg)); err != nil {
			log.Fatalf("Firefox Twitch login cookie is invalid: %v", err)
		}
		streams, err := liveStreamIDs(ctx, client, cfg.Channels, cfg.TwitchToken, clientID)
		if err != nil {
			log.Fatal(err)
		}
		for _, channel := range cfg.Channels {
			log.Printf("channel=%s live=%t", channel, streams[channel] != "")
		}
		return
	}
	if *probe {
		if _, err := validateTwitchToken(ctx, client, viewerToken(cfg)); err != nil {
			log.Fatalf("Firefox Twitch login cookie is invalid: %v", err)
		}
		if err := probeSteel(ctx, client, cfg); err != nil {
			log.Fatal(redactError(err, cfg))
		}
		log.Print("Steel browser loaded Twitch with the authenticated Firefox session")
		return
	}
	if err := run(ctx, client, cfg, clientID, *statePath); err != nil && !errors.Is(err, context.Canceled) {
		log.Fatal(err)
	}
}

func loadConfig(path string) (config, error) {
	var cfg config
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("read config: %w", err)
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("parse config: %w", err)
	}
	if len(cfg.Channels) == 0 && cfg.Channel != "" {
		cfg.Channels = []string{cfg.Channel}
	}
	if len(cfg.Channels) == 0 || len(cfg.Channels) > 100 || cfg.TwitchToken == "" || cfg.SteelAPIKey == "" || !validTelegramToken.MatchString(cfg.TelegramBot) || cfg.TelegramChat == "" {
		return cfg, errors.New("config requires 1..100 channels, twitch_token, steel_api_key, telegram_bot, and telegram_chat")
	}
	seen := make(map[string]bool, len(cfg.Channels))
	for i, channel := range cfg.Channels {
		channel = strings.ToLower(channel)
		if !twitchLogin.MatchString(channel) || seen[channel] {
			return cfg, fmt.Errorf("invalid or duplicate Twitch channel %q", channel)
		}
		cfg.Channels[i] = channel
		seen[channel] = true
	}
	hasAuth := false
	for _, item := range cfg.Cookies {
		if item.Name == "auth-token" && item.Value != "" && (item.Domain == "twitch.tv" || item.Domain == ".twitch.tv") {
			hasAuth = true
		}
	}
	if !hasAuth {
		return cfg, errors.New("config requires a Twitch auth-token cookie")
	}
	proxy, err := url.Parse(cfg.ProxyURL)
	if err != nil || proxy.Host == "" || (proxy.Scheme != "http" && proxy.Scheme != "https" && proxy.Scheme != "socks5") {
		return cfg, errors.New("proxy_url must be a reachable http, https, or socks5 proxy URL")
	}
	if host := proxy.Hostname(); host == "localhost" {
		return cfg, errors.New("Steel cannot reach a local proxy; configure a public proxy endpoint")
	} else if address := net.ParseIP(host); address != nil && (!address.IsGlobalUnicast() || address.IsPrivate()) {
		return cfg, errors.New("Steel cannot reach a local proxy; configure a public proxy endpoint")
	}
	if cfg.PollSeconds == 0 {
		cfg.PollSeconds = 30
	}
	if cfg.WatchSeconds == 0 {
		cfg.WatchSeconds = 300
	}
	if cfg.PollSeconds < 10 || cfg.WatchSeconds < 1 || cfg.WatchSeconds > 3600 {
		return cfg, errors.New("poll_seconds must be at least 10 and watch_seconds must be 1..3600")
	}
	return cfg, nil
}

func validateTwitchToken(ctx context.Context, client *http.Client, token string) (string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://id.twitch.tv/oauth2/validate", nil)
	if err != nil {
		return "", err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	var response struct {
		ClientID string `json:"client_id"`
	}
	if err := getJSON(client, request, &response); err != nil {
		return "", fmt.Errorf("validate Twitch token: %w", err)
	}
	if response.ClientID == "" {
		return "", errors.New("Twitch token validation returned no client ID")
	}
	return response.ClientID, nil
}

func liveStreamIDs(ctx context.Context, client *http.Client, channels []string, token, clientID string) (map[string]string, error) {
	query := url.Values{"first": {"100"}}
	for _, channel := range channels {
		query.Add("user_login", channel)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.twitch.tv/helix/streams?"+query.Encode(), nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Client-Id", clientID)
	var response struct {
		Data []struct {
			ID        string `json:"id"`
			UserLogin string `json:"user_login"`
		} `json:"data"`
	}
	if err := getJSON(client, request, &response); err != nil {
		return nil, fmt.Errorf("get Twitch streams: %w", err)
	}
	streams := make(map[string]string, len(response.Data))
	for _, stream := range response.Data {
		streams[strings.ToLower(stream.UserLogin)] = stream.ID
	}
	return streams, nil
}

func getJSON(client *http.Client, request *http.Request, target any) error {
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return httpStatusError{code: response.StatusCode}
	}
	return json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(target)
}

func loadState(statePath string, cfg config) (state, error) {
	var previous state
	if data, err := os.ReadFile(statePath); err == nil {
		if err := json.Unmarshal(data, &previous); err != nil {
			return previous, fmt.Errorf("parse state: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return previous, fmt.Errorf("read state: %w", err)
	}
	if previous.CompletedStreams == nil {
		previous.CompletedStreams = make(map[string]string)
	}
	if previous.CompletedStreamID != "" {
		channel := cfg.Channel
		if channel == "" {
			channel = cfg.Channels[0]
		}
		channel = strings.ToLower(channel)
		if _, exists := previous.CompletedStreams[channel]; !exists {
			previous.CompletedStreams[channel] = previous.CompletedStreamID
		}
		previous.CompletedStreamID = ""
	}
	return previous, nil
}

type watchResult struct {
	channel    string
	streamID   string
	generation uint64
	err        error
}

type activeWatch struct {
	cancel     context.CancelFunc
	generation uint64
}

func run(ctx context.Context, client *http.Client, cfg config, clientID, statePath string) error {
	return runWithWatch(ctx, client, cfg, clientID, statePath, watchStream)
}

func runWithWatch(ctx context.Context, client *http.Client, cfg config, clientID, statePath string, watch func(context.Context, *http.Client, config, string) error) error {
	previous, err := loadState(statePath, cfg)
	if err != nil {
		return err
	}
	log.Printf("monitoring %d channels every %d seconds", len(cfg.Channels), cfg.PollSeconds)
	auth := authMonitor{client: client, cfg: cfg, state: &previous, statePath: statePath}
	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	results := make(chan watchResult, len(cfg.Channels))
	active := make(map[string]activeWatch)
	var generation uint64
	cancelActive := func() {
		for channel, watch := range active {
			watch.cancel()
			delete(active, channel)
		}
	}
	defer cancelActive()
	poll := func() error {
		canWatch, err := auth.check(ctx)
		if err != nil {
			return err
		}
		if !canWatch {
			cancelActive()
			return nil
		}
		streams, err := liveStreamIDs(ctx, client, cfg.Channels, cfg.TwitchToken, clientID)
		if err != nil {
			log.Printf("live check failed: %v", err)
			return nil
		}
		for _, channel := range cfg.Channels {
			id := streams[channel]
			_, running := active[channel]
			if id == "" || id == previous.CompletedStreams[channel] || running {
				continue
			}
			log.Printf("channel=%s stream=%s is live; starting Steel watch", channel, id)
			watchCtx, cancel := context.WithCancel(runCtx)
			generation++
			active[channel] = activeWatch{cancel: cancel, generation: generation}
			go func(channel, id string, generation uint64) {
				result := watchResult{channel: channel, streamID: id, generation: generation, err: watch(watchCtx, client, cfg, channel)}
				select {
				case results <- result:
				case <-runCtx.Done():
				}
			}(channel, id, generation)
		}
		return nil
	}
	if err := poll(); err != nil {
		return err
	}
	ticker := time.NewTicker(time.Duration(cfg.PollSeconds) * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := poll(); err != nil {
				return err
			}
		case result := <-results:
			current, exists := active[result.channel]
			if !exists || current.generation != result.generation {
				continue // A canceled watch has already been stopped.
			}
			current.cancel()
			delete(active, result.channel)
			if errors.Is(result.err, errTwitchAuthLost) {
				auth.markLost()
				auth.notify(ctx)
				cancelActive()
			} else if result.err != nil {
				log.Printf("channel=%s watch failed: %s", result.channel, redactError(result.err, cfg))
			} else {
				previous.CompletedStreams[result.channel] = result.streamID
				if err := saveState(statePath, previous); err != nil {
					return err
				}
				log.Printf("channel=%s completed %d seconds for stream %s", result.channel, cfg.WatchSeconds, result.streamID)
			}
		}
	}
}

func redactError(err error, cfg config) string {
	message := err.Error()
	secrets := []string{cfg.SteelAPIKey, cfg.TwitchToken, cfg.TelegramBot, cfg.ProxyURL}
	for _, item := range cfg.Cookies {
		secrets = append(secrets, item.Value)
	}
	for _, secret := range secrets {
		if secret != "" {
			message = strings.ReplaceAll(message, secret, "[redacted]")
		}
	}
	return message
}

func saveState(path string, value state) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path+".tmp", append(data, '\n'), 0600); err != nil {
		return err
	}
	if err := os.Rename(path+".tmp", path); err != nil {
		return err
	}
	return nil
}

func watchStream(ctx context.Context, client *http.Client, cfg config, channel string) error {
	// Reserve time for auth confirmation, playback startup, and cleanup.
	timeout := time.Duration(cfg.WatchSeconds+130) * time.Second
	return openSteelPage(ctx, client, cfg, "https://www.twitch.tv/"+channel, timeout, func(pageCtx context.Context, browser *cdpClient) error {
		if err := confirmBrowserAuth(pageCtx, browser); err != nil {
			return err
		}
		return waitForPlayback(pageCtx, browser, time.Duration(cfg.WatchSeconds)*time.Second)
	})
}

func probeSteel(ctx context.Context, client *http.Client, cfg config) error {
	return openSteelPage(ctx, client, cfg, "https://www.twitch.tv/", 90*time.Second, func(pageCtx context.Context, browser *cdpClient) error {
		return confirmBrowserAuth(pageCtx, browser)
	})
}

func openSteelPage(ctx context.Context, client *http.Client, cfg config, pageURL string, timeout time.Duration, action func(context.Context, *cdpClient) error) error {
	session, err := createSteelSession(ctx, client, cfg, timeout)
	if err != nil {
		return err
	}
	defer func() {
		if err := releaseSteelSession(client, cfg.SteelAPIKey, session.ID); err != nil {
			log.Printf("release Steel session failed: %v", err)
		}
	}()
	ws, err := url.Parse(session.WebsocketURL)
	if err != nil {
		return fmt.Errorf("parse Steel websocket URL: %w", err)
	}
	query := ws.Query()
	query.Set("apiKey", cfg.SteelAPIKey)
	ws.RawQuery = query.Encode()
	pageCtx, cancelTimeout := context.WithTimeout(ctx, timeout-10*time.Second)
	defer cancelTimeout()
	browser, err := connectCDP(pageCtx, ws.String())
	if err != nil {
		return fmt.Errorf("connect to Steel browser: %w", err)
	}
	defer browser.Close()
	if err := browser.SetCookies(pageCtx, cfg.Cookies); err != nil {
		return fmt.Errorf("set Twitch cookies: %w", err)
	}
	if err := browser.Navigate(pageCtx, pageURL); err != nil {
		return fmt.Errorf("open Twitch in Steel: %w", err)
	}
	return action(pageCtx, browser)
}

func waitForPlayback(ctx context.Context, browser *cdpClient, duration time.Duration) error {
	startDeadline := time.Now().Add(60 * time.Second)
	var lastProgress time.Time
	var lastSample time.Time
	var watched time.Duration
	var previous float64
	for {
		var playback struct {
			Present bool    `json:"present"`
			Paused  bool    `json:"paused"`
			Time    float64 `json:"time"`
			Login   bool    `json:"login"`
		}
		err := browser.Evaluate(ctx, `(() => {
			const login = document.querySelector('button[data-a-target="login-button"]');
			const video = document.querySelector('video');
			if (!video) return {present:false,paused:true,time:0,login:!!login && !!login.getClientRects().length};
			video.muted = true;
			if (video.paused) video.play().catch(() => {});
			return {present:true,paused:video.paused,time:video.currentTime,login:!!login && !!login.getClientRects().length};
		})()`, &playback)
		if err != nil {
			return fmt.Errorf("check Twitch playback: %w", err)
		}
		now := time.Now()
		if playback.Login {
			return errTwitchAuthLost
		}
		if playback.Present && !playback.Paused && playback.Time > previous+0.5 {
			if lastProgress.IsZero() {
				log.Print("Twitch video playback confirmed")
			} else {
				playbackDelta := time.Duration(math.Min(playback.Time-previous, now.Sub(lastSample).Seconds()) * float64(time.Second))
				watched += playbackDelta
			}
			lastProgress = now
		}
		if watched >= duration {
			return nil
		}
		if now.After(startDeadline) && lastProgress.IsZero() {
			return errors.New("Twitch video did not start within 60 seconds")
		}
		if !lastProgress.IsZero() && now.Sub(lastProgress) > 30*time.Second {
			return errors.New("Twitch playback stalled for 30 seconds")
		}
		previous = playback.Time
		lastSample = now
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

func createSteelSession(ctx context.Context, client *http.Client, cfg config, timeout time.Duration) (steelSession, error) {
	var session steelSession
	options := map[string]any{
		"timeout":           timeout.Milliseconds(),
		"inactivityTimeout": 120000,
	}
	if cfg.ProxyURL != "" {
		options["proxyUrl"] = cfg.ProxyURL
	}
	body, err := json.Marshal(options)
	if err != nil {
		return session, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, steelAPI, bytes.NewReader(body))
	if err != nil {
		return session, err
	}
	request.Header.Set("steel-api-key", cfg.SteelAPIKey)
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return session, fmt.Errorf("create Steel session: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusCreated {
		return session, fmt.Errorf("create Steel session: HTTP %d", response.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&session); err != nil {
		return session, err
	}
	if session.ID == "" || session.WebsocketURL == "" {
		if session.ID != "" {
			_ = releaseSteelSession(client, cfg.SteelAPIKey, session.ID)
		}
		return session, errors.New("Steel session response lacks ID or websocket URL")
	}
	return session, nil
}

func releaseSteelSession(client *http.Client, apiKey, id string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, steelAPI+"/"+url.PathEscape(id)+"/release", nil)
	if err != nil {
		return err
	}
	request.Header.Set("steel-api-key", apiKey)
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", response.StatusCode)
	}
	return nil
}

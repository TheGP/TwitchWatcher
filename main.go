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
const maxWatchSession = 9 * time.Minute
const maxLoadAttempts = 3

var twitchLogin = regexp.MustCompile(`^[A-Za-z0-9_]{3,25}$`)
var errVideoNotLoaded = errors.New("Twitch video could not be loaded")
var errPageHoldInterrupted = errors.New("Steel page hold was interrupted")

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
	if err != nil || proxy.Host == "" || (proxy.Scheme != "http" && proxy.Scheme != "https") {
		return cfg, errors.New("proxy_url must be a reachable http or https proxy URL")
	}
	if host := strings.ToLower(proxy.Hostname()); host == "dataimpulse.com" || strings.HasSuffix(host, ".dataimpulse.com") {
		return cfg, errors.New("DataImpulse proxies are disabled for this project")
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
		cfg.WatchSeconds = 360
	}
	if cfg.PollSeconds < 10 || cfg.WatchSeconds < 1 || cfg.WatchSeconds > 360 {
		return cfg, errors.New("poll_seconds must be at least 10 and watch_seconds must be 1..360")
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

type watchCooldown struct {
	streamID string
	until    time.Time
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
	retryAfter := make(map[string]watchCooldown)
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
			cooldown := retryAfter[channel]
			if id == "" || id == previous.CompletedStreams[channel] || running || (cooldown.streamID == id && time.Now().Before(cooldown.until)) {
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
				if errors.Is(result.err, errVideoNotLoaded) {
					retryAfter[result.channel] = watchCooldown{streamID: result.streamID, until: time.Now().Add(5 * time.Minute)}
				}
				log.Printf("channel=%s watch failed: %s", result.channel, redactError(result.err, cfg))
			} else {
				delete(retryAfter, result.channel)
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
	hold := time.Duration(cfg.WatchSeconds) * time.Second
	return watchLoadedPage(ctx, cfg, channel, 5*time.Second, func(watchCtx context.Context) error {
		timeout := min(hold+3*time.Minute, maxWatchSession)
		return openSteelPage(watchCtx, client, cfg, "https://www.twitch.tv/"+channel, timeout, func(pageCtx context.Context, browser *cdpClient) error {
			if err := confirmBrowserAuth(pageCtx, browser); err != nil {
				return err
			}
			if err := waitForVideoLoaded(pageCtx, browser); err != nil {
				return err
			}
			log.Printf("channel=%s Twitch video loaded; keeping page open for %s", channel, hold)
			select {
			case <-pageCtx.Done():
				return fmt.Errorf("%w: %v", errPageHoldInterrupted, pageCtx.Err())
			case <-time.After(hold):
				return nil
			}
		})
	})
}

func watchLoadedPage(ctx context.Context, cfg config, channel string, retryBase time.Duration, attempt func(context.Context) error) error {
	for attemptNumber := 1; attemptNumber <= maxLoadAttempts; attemptNumber++ {
		err := attempt(ctx)
		if err == nil {
			return nil
		}
		if errors.Is(err, errTwitchAuthLost) {
			return err
		}
		if errors.Is(err, errPageHoldInterrupted) {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if attemptNumber == maxLoadAttempts {
			return fmt.Errorf("%w after %d attempts: %v", errVideoNotLoaded, attemptNumber, err)
		}
		log.Printf("channel=%s could not load Twitch video; retrying: %s", channel, redactError(err, cfg))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(attemptNumber) * retryBase):
		}
	}
	panic("unreachable")
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

type videoState struct {
	Present bool `json:"present"`
	Ready   bool `json:"ready"`
	Ended   bool `json:"ended"`
	Login   bool `json:"login"`
}

func waitForVideoLoaded(ctx context.Context, browser *cdpClient) error {
	deadline := time.NewTimer(time.Minute)
	defer deadline.Stop()
	for {
		var video videoState
		err := browser.Evaluate(ctx, `(() => {
			const login = `+twitchLoginButtonJS+`;
			const video = document.querySelector('[data-a-target="video-ref"] video') ||
				document.querySelector('[data-a-target="video-player"] video[aria-label="Twitch video player"]') ||
				document.querySelector('video[aria-label="Twitch video player"]');
			if (!video) return {present:false,ready:false,ended:false,login};
			video.muted = true;
			if (video.paused) video.play().catch(() => {});
			return {present:!!video.getClientRects().length,ready:video.readyState >= 2,ended:video.ended,login};
		})()`, &video)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("check Twitch video: %w", err)
		}
		if video.Login {
			return errTwitchAuthLost
		}
		if video.Present && video.Ready && !video.Ended {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return errors.New("Twitch video did not load within 60 seconds")
		case <-time.After(2 * time.Second):
		}
	}
}

func createSteelSession(ctx context.Context, client *http.Client, cfg config, timeout time.Duration) (steelSession, error) {
	var session steelSession
	inactivityTimeout := timeout - 10*time.Second
	if inactivityTimeout < 15*time.Second {
		inactivityTimeout = timeout / 2
	}
	options := map[string]any{
		"timeout":           timeout.Milliseconds(),
		"inactivityTimeout": inactivityTimeout.Milliseconds(),
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

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
	Channel      string   `json:"channel"`
	PollSeconds  int      `json:"poll_seconds"`
	WatchSeconds int      `json:"watch_seconds"`
	TwitchToken  string   `json:"twitch_token"`
	SteelAPIKey  string   `json:"steel_api_key"`
	ProxyURL     string   `json:"proxy_url"`
	Cookies      []cookie `json:"cookies"`
}

type state struct {
	CompletedStreamID string `json:"completed_stream_id"`
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
	for _, item := range cfg.Cookies {
		if item.Name == "auth-token" && (item.Domain == ".twitch.tv" || item.Domain == "twitch.tv") {
			if _, err := validateTwitchToken(ctx, client, item.Value); err != nil {
				log.Fatalf("Firefox Twitch login cookie is invalid: %v", err)
			}
			break
		}
	}
	if *check {
		id, err := liveStreamID(ctx, client, cfg.Channel, cfg.TwitchToken, clientID)
		if err != nil {
			log.Fatal(err)
		}
		log.Printf("channel=%s live=%t", cfg.Channel, id != "")
		return
	}
	if *probe {
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
	if !twitchLogin.MatchString(cfg.Channel) || cfg.TwitchToken == "" || cfg.SteelAPIKey == "" {
		return cfg, errors.New("config requires channel, twitch_token, and steel_api_key")
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

func liveStreamID(ctx context.Context, client *http.Client, channel, token, clientID string) (string, error) {
	endpoint := "https://api.twitch.tv/helix/streams?user_login=" + url.QueryEscape(channel)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Client-Id", clientID)
	var response struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := getJSON(client, request, &response); err != nil {
		return "", fmt.Errorf("get Twitch stream: %w", err)
	}
	if len(response.Data) == 0 {
		return "", nil
	}
	return response.Data[0].ID, nil
}

func getJSON(client *http.Client, request *http.Request, target any) error {
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", response.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(target)
}

func run(ctx context.Context, client *http.Client, cfg config, clientID, statePath string) error {
	var previous state
	if data, err := os.ReadFile(statePath); err == nil {
		if err := json.Unmarshal(data, &previous); err != nil {
			return fmt.Errorf("parse state: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("read state: %w", err)
	}
	log.Printf("monitoring %s every %d seconds", cfg.Channel, cfg.PollSeconds)
	for {
		id, err := liveStreamID(ctx, client, cfg.Channel, cfg.TwitchToken, clientID)
		if err != nil {
			log.Printf("live check failed: %v", err)
		} else if id != "" && id != previous.CompletedStreamID {
			log.Printf("stream %s is live; starting Steel watch", id)
			if err := watchStream(ctx, client, cfg); err != nil {
				log.Printf("watch failed: %s", redactError(err, cfg))
			} else {
				previous.CompletedStreamID = id
				if err := saveState(statePath, previous); err != nil {
					return err
				}
				log.Printf("completed %d seconds for stream %s", cfg.WatchSeconds, id)
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(cfg.PollSeconds) * time.Second):
		}
	}
}

func redactError(err error, cfg config) string {
	message := err.Error()
	secrets := []string{cfg.SteelAPIKey, cfg.TwitchToken, cfg.ProxyURL}
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

func watchStream(ctx context.Context, client *http.Client, cfg config) error {
	// Reserve time for navigation and cleanup beyond the requested viewing period.
	timeout := time.Duration(cfg.WatchSeconds+90) * time.Second
	return openSteelPage(ctx, client, cfg, "https://www.twitch.tv/"+cfg.Channel, timeout, func(pageCtx context.Context, browser *cdpClient) error {
		return waitForPlayback(pageCtx, browser, time.Duration(cfg.WatchSeconds)*time.Second)
	})
}

func probeSteel(ctx context.Context, client *http.Client, cfg config) error {
	return openSteelPage(ctx, client, cfg, "https://www.twitch.tv/", 90*time.Second, func(pageCtx context.Context, browser *cdpClient) error {
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			var loggedIn bool
			if err := browser.Evaluate(pageCtx, `Boolean(document.querySelector('[data-a-target="user-menu-toggle"]'))`, &loggedIn); err != nil {
				return err
			}
			if loggedIn {
				return nil
			}
			select {
			case <-pageCtx.Done():
				return pageCtx.Err()
			case <-time.After(time.Second):
			}
		}
		return errors.New("Twitch account menu did not appear in Steel within 30 seconds")
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
		}
		err := browser.Evaluate(ctx, `(() => {
			const video = document.querySelector('video');
			if (!video) return {present:false,paused:true,time:0};
			video.muted = true;
			if (video.paused) video.play().catch(() => {});
			return {present:true,paused:video.paused,time:video.currentTime};
		})()`, &playback)
		if err != nil {
			return fmt.Errorf("check Twitch playback: %w", err)
		}
		now := time.Now()
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

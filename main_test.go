package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestLoadConfigRejectsLocalProxyAndMissingAuth(t *testing.T) {
	base := config{
		Channels: []string{"streamer_one", "streamer_two"}, TwitchToken: "token", SteelAPIKey: "key", TelegramBot: "123:abcdef", TelegramChat: "42",
		ProxyURL: "http://user:password@proxy.example.com:8080",
		Cookies:  []cookie{{Name: "auth-token", Value: "cookie", Domain: ".twitch.tv"}},
	}
	for _, test := range []struct {
		name      string
		edit      func(*config)
		wantError bool
	}{
		{"valid", func(*config) {}, false},
		{"local proxy", func(cfg *config) { cfg.ProxyURL = "http://127.0.0.1:3128" }, true},
		{"private proxy", func(cfg *config) { cfg.ProxyURL = "http://192.168.0.53:3128" }, true},
		{"disallowed proxy", func(cfg *config) { cfg.ProxyURL = "http://user:pass@gw.dataimpulse.com:10000" }, true},
		{"socks unsupported by Steel", func(cfg *config) { cfg.ProxyURL = "socks5://user:pass@104.219.236.83:1080" }, true},
		{"missing auth", func(cfg *config) { cfg.Cookies = nil }, true},
		{"missing Telegram", func(cfg *config) { cfg.TelegramBot = "" }, true},
		{"missing channels", func(cfg *config) { cfg.Channels = nil }, true},
		{"bad channel", func(cfg *config) { cfg.Channels = []string{"bad/channel"} }, true},
		{"duplicate channel", func(cfg *config) { cfg.Channels = []string{"One", "one"} }, true},
		{"too many channels", func(cfg *config) { cfg.Channels = make([]string, 101) }, true},
		{"legacy channel", func(cfg *config) { cfg.Channels = nil; cfg.Channel = "Streamer_One" }, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := base
			test.edit(&cfg)
			data, err := json.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			loaded, err := loadConfig(path)
			if (err != nil) != test.wantError {
				t.Fatalf("loadConfig error = %v, wantError = %t", err, test.wantError)
			}
			if err == nil && test.name == "legacy channel" && (len(loaded.Channels) != 1 || loaded.Channels[0] != "streamer_one") {
				t.Fatalf("legacy channel not normalized: %+v", loaded.Channels)
			}
		})
	}
}

func TestLiveStreamIDsChecksConfiguredChannelsTogether(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path != "/helix/streams" || request.URL.Query().Get("first") != "100" {
			t.Fatalf("unexpected stream request: %s", request.URL)
		}
		logins := request.URL.Query()["user_login"]
		if len(logins) != 2 || logins[0] != "one" || logins[1] != "two" {
			t.Fatalf("unexpected usernames: %v", logins)
		}
		return response(http.StatusOK, `{"data":[{"id":"stream-1","user_login":"One"}]}`), nil
	})}
	streams, err := liveStreamIDs(context.Background(), client, []string{"one", "two"}, "token", "client")
	if err != nil || len(streams) != 1 || streams["one"] != "stream-1" || streams["two"] != "" {
		t.Fatalf("live streams=%v err=%v", streams, err)
	}
}

func TestLoadStateMigratesLegacyCompletion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte(`{"completed_stream_id":"old-stream","auth_alerted":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	previous, err := loadState(path, config{Channel: "Old_Channel", Channels: []string{"new_channel", "old_channel"}})
	if err != nil || previous.CompletedStreams["old_channel"] != "old-stream" || previous.CompletedStreams["new_channel"] != "" || previous.CompletedStreamID != "" || !previous.AuthAlerted {
		t.Fatalf("legacy state migration: %+v err=%v", previous, err)
	}
}

func TestRunWatchesTwoLiveChannelsConcurrentlyAndPersistsSeparately(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch request.URL.Host {
		case "id.twitch.tv":
			return response(http.StatusOK, `{"client_id":"test-client"}`), nil
		case "api.twitch.tv":
			return response(http.StatusOK, `{"data":[{"id":"stream-1","user_login":"one"},{"id":"stream-2","user_login":"two"}]}`), nil
		default:
			return nil, fmt.Errorf("unexpected request host %s", request.URL.Host)
		}
	})}
	started := make(chan string, 2)
	release := make(chan struct{})
	watch := func(ctx context.Context, _ *http.Client, _ config, channel string) error {
		started <- channel
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	path := filepath.Join(t.TempDir(), "state.json")
	finished := make(chan error, 1)
	go func() {
		finished <- runWithWatch(ctx, client, config{Channels: []string{"one", "two"}, Cookies: []cookie{{Name: "auth-token", Value: "valid", Domain: ".twitch.tv"}}, PollSeconds: 10, WatchSeconds: 300}, "test-client", path, watch)
	}()
	seen := make(map[string]bool)
	for range 2 {
		select {
		case channel := <-started:
			seen[channel] = true
		case <-time.After(time.Second):
			t.Fatal("both channels did not start concurrently")
		}
	}
	if !seen["one"] || !seen["two"] {
		t.Fatalf("did not start both distinct channels: %v", seen)
	}
	close(release)
	deadline := time.Now().Add(time.Second)
	for {
		data, err := os.ReadFile(path)
		if err == nil {
			var saved state
			if json.Unmarshal(data, &saved) == nil && saved.CompletedStreams["one"] == "stream-1" && saved.CompletedStreams["two"] == "stream-2" {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("separate completed streams were not saved: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-finished; !errors.Is(err, context.Canceled) {
		t.Fatalf("run stopped with %v", err)
	}
}

func TestRunStopsOtherWatchesWhenTwitchAuthIsLost(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch request.URL.Host {
		case "id.twitch.tv":
			return response(http.StatusOK, `{"client_id":"test-client"}`), nil
		case "api.twitch.tv":
			return response(http.StatusOK, `{"data":[{"id":"stream-1","user_login":"one"},{"id":"stream-2","user_login":"two"}]}`), nil
		case "api.telegram.org":
			return response(http.StatusOK, `{"ok":true}`), nil
		default:
			return nil, fmt.Errorf("unexpected request host %s", request.URL.Host)
		}
	})}
	otherCanceled := make(chan struct{})
	watch := func(ctx context.Context, _ *http.Client, _ config, channel string) error {
		if channel == "one" {
			return errTwitchAuthLost
		}
		<-ctx.Done()
		close(otherCanceled)
		return ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	path := filepath.Join(t.TempDir(), "state.json")
	finished := make(chan error, 1)
	go func() {
		finished <- runWithWatch(ctx, client, config{Channels: []string{"one", "two"}, Cookies: []cookie{{Name: "auth-token", Value: "valid", Domain: ".twitch.tv"}}, TelegramBot: "123:abcdef", TelegramChat: "42", PollSeconds: 10}, "test-client", path, watch)
	}()
	select {
	case <-otherCanceled:
	case <-time.After(time.Second):
		t.Fatal("auth loss did not cancel the other watch")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var saved state
	if err := json.Unmarshal(data, &saved); err != nil || !saved.AuthLost || !saved.AuthAlerted || len(saved.CompletedStreams) != 0 {
		t.Fatalf("auth loss state: %+v err=%v", saved, err)
	}
	cancel()
	if err := <-finished; !errors.Is(err, context.Canceled) {
		t.Fatalf("run stopped with %v", err)
	}
}

func TestRedactError(t *testing.T) {
	cfg := config{SteelAPIKey: "steel-secret", TwitchToken: "twitch-secret", ProxyURL: "http://proxy-secret@host:80", Cookies: []cookie{{Value: "cookie-secret"}}}
	message := redactError(errors.New("steel-secret twitch-secret http://proxy-secret@host:80 cookie-secret"), cfg)
	if strings.Contains(message, "secret") {
		t.Fatalf("secret leaked in log message: %s", message)
	}
}

func TestPlaybackProgressCountsOnlyVerifiedReadyVideo(t *testing.T) {
	var progress playbackProgress
	playing := playbackSample{Present: true, Ready: true, Time: 10}
	next := playbackSample{Present: true, Ready: true, Time: 15}
	if delta := progress.add(playing, next, 5*time.Second); delta != 5*time.Second {
		t.Fatalf("playing video credited %s, want 5s", delta)
	}
	paused := next
	paused.Paused = true
	if delta := progress.add(next, paused, 5*time.Second); delta != 0 {
		t.Fatalf("paused video credited %s", delta)
	}
	unloaded := next
	unloaded.Ready = false
	if delta := progress.add(unloaded, playbackSample{Present: true, Ready: true, Time: 30}, 5*time.Second); delta != 0 {
		t.Fatalf("unloaded video credited %s", delta)
	}
	missing := next
	missing.Present = false
	if delta := progress.add(missing, playbackSample{Present: true, Ready: true, Time: 35}, 5*time.Second); delta != 0 {
		t.Fatalf("missing video credited %s", delta)
	}
	jump := playbackSample{Present: true, Ready: true, Time: 45}
	if delta := progress.add(next, jump, 30*time.Second); delta != 0 {
		t.Fatalf("long unobserved gap credited %s", delta)
	}
	reset := playing
	if delta := progress.add(jump, reset, 5*time.Second); delta != 0 {
		t.Fatalf("reset video timeline credited %s", delta)
	}
	if progress.watched != 5*time.Second {
		t.Fatalf("verified progress=%s, want 5s", progress.watched)
	}
}

func TestWatchUntilPlaybackCarriesProgressAcrossSessions(t *testing.T) {
	attempts := 0
	err := watchUntilPlayback(context.Background(), config{}, "example", 5*time.Second, 0, func(_ context.Context, timeout time.Duration, progress *playbackProgress) error {
		attempts++
		if timeout > maxSteelSession {
			t.Fatalf("session timeout exceeds 15 minutes: %s", timeout)
		}
		if attempts == 1 {
			progress.watched += 2 * time.Second
			return errors.New("control connection dropped")
		}
		progress.watched += 3 * time.Second
		return nil
	})
	if err != nil || attempts != 2 {
		t.Fatalf("progress did not carry between sessions: attempts=%d err=%v", attempts, err)
	}
}

func TestWatchUntilPlaybackBoundsEmptySessions(t *testing.T) {
	attempts := 0
	err := watchUntilPlayback(context.Background(), config{}, "example", 5*time.Second, 0, func(_ context.Context, _ time.Duration, _ *playbackProgress) error {
		attempts++
		return errors.New("proxy tunnel failed")
	})
	if !errors.Is(err, errNoPlayback) || attempts != maxNoProgressSessions {
		t.Fatalf("no-progress retry limit: attempts=%d err=%v", attempts, err)
	}
}

func TestRunCoolsDownSameStreamAfterNoPlayback(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch request.URL.Host {
		case "id.twitch.tv":
			return response(http.StatusOK, `{"client_id":"test-client"}`), nil
		case "api.twitch.tv":
			return response(http.StatusOK, `{"data":[{"id":"stream-1","user_login":"one"}]}`), nil
		default:
			return nil, fmt.Errorf("unexpected request host %s", request.URL.Host)
		}
	})}
	var attempts atomic.Int32
	watch := func(context.Context, *http.Client, config, string) error {
		attempts.Add(1)
		return errNoPlayback
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2300*time.Millisecond)
	defer cancel()
	err := runWithWatch(ctx, client, config{Channels: []string{"one"}, Cookies: []cookie{{Name: "auth-token", Value: "valid", Domain: ".twitch.tv"}}, PollSeconds: 1}, "test-client", filepath.Join(t.TempDir(), "state.json"), watch)
	if !errors.Is(err, context.DeadlineExceeded) || attempts.Load() != 1 {
		t.Fatalf("same stream restarted during cooldown: attempts=%d err=%v", attempts.Load(), err)
	}
}

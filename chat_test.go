package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func TestProductionChatConfig(t *testing.T) {
	cfg, err := loadChatConfig("chat.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Targets) != 1 || cfg.Targets[0].Channel != "platinumbabydoll" || len(cfg.Targets[0].Messages) != 7 {
		t.Fatalf("unexpected production chat config: %+v", cfg)
	}
	seen := make(map[string]bool)
	for _, message := range cfg.Targets[0].Messages {
		if seen[message.ID] {
			t.Fatalf("duplicate message ID %q", message.ID)
		}
		seen[message.ID] = true
		if message.ID == "dance" {
			want := []string{
				"djnyx8Dancegirl djnyx8Dancegirl djnyx8Dancegirl djnyx8Dancegirl djnyx8Dancegirl",
				"djnyx8HeMan djnyx8HeMan djnyx8HeMan djnyx8HeMan djnyx8HeMan",
				"djnyx8Cardance djnyx8Cardance djnyx8Cardance djnyx8Cardance djnyx8Cardance",
				"djnyx8Pedro djnyx8Pedro djnyx8Pedro djnyx8Pedro djnyx8Pedro",
			}
			if !slices.Equal(message.Texts, want) || message.IntervalMinSeconds != 600 || message.IntervalMaxSeconds != 1800 {
				t.Fatalf("unexpected randomized dance config: %+v", message)
			}
		}
	}
}

func TestChatScheduleSendsOnceAndRepeatsPerStream(t *testing.T) {
	target := chatTarget{Channel: "channel", Messages: []chatMessage{
		{ID: "first", Text: "one", Once: true},
		{ID: "second", Text: "two", Once: true, DelaySeconds: 180},
		{ID: "repeat", Text: "again", DelaySeconds: 1200, IntervalSeconds: 1200},
	}}
	status := state{ChatStreams: map[string]chatStreamState{}, ChatMessages: map[string]chatMessageState{}}
	started := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	streams := map[string]string{"channel": "stream-1"}
	var sent []string
	send := func(_ context.Context, _ string, text string) error {
		sent = append(sent, text)
		return nil
	}
	check := func(at time.Time, wantCount int) {
		t.Helper()
		if _, err := processChatSchedules(context.Background(), []chatTarget{target}, streams, &status, at, send); err != nil {
			t.Fatal(err)
		}
		if len(sent) != wantCount {
			t.Fatalf("at %s sent=%v, want count %d", at.Sub(started), sent, wantCount)
		}
	}
	check(started, 1)
	check(started.Add(179*time.Second), 1)
	check(started.Add(180*time.Second), 2)
	check(started.Add(20*time.Minute), 3)
	check(started.Add(40*time.Minute), 4)

	streams["channel"] = "stream-2"
	newStream := started.Add(24 * time.Hour)
	check(newStream, 4) // One-time messages stay sent; repeat waits 20 minutes.
	check(newStream.Add(20*time.Minute), 5)
	if sent[0] != "one" || sent[1] != "two" || sent[2] != "again" || sent[3] != "again" || sent[4] != "again" {
		t.Fatalf("unexpected message order: %v", sent)
	}
}

func TestChatScheduleDoesNotRecordFailedSend(t *testing.T) {
	status := state{ChatStreams: map[string]chatStreamState{}, ChatMessages: map[string]chatMessageState{}}
	target := chatTarget{Channel: "channel", Messages: []chatMessage{{ID: "first", Text: "one", Once: true}}}
	_, err := processChatSchedules(context.Background(), []chatTarget{target}, map[string]string{"channel": "stream"}, &status, time.Now(), func(context.Context, string, string) error {
		return errors.New("rejected")
	})
	if err == nil || !status.ChatMessages["channel/first"].SentAt.IsZero() {
		t.Fatalf("failed send was recorded: state=%+v err=%v", status.ChatMessages, err)
	}
}

func TestChatSchedulePersistsRandomRangeAndChoosesConfiguredText(t *testing.T) {
	target := chatTarget{Channel: "channel", Messages: []chatMessage{{
		ID: "repeat", Texts: []string{"alpha", "beta"}, IntervalMinSeconds: 600, IntervalMaxSeconds: 1800,
	}}}
	started := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	status := state{ChatStreams: map[string]chatStreamState{}, ChatMessages: map[string]chatMessageState{}}
	streams := map[string]string{"channel": "stream-1"}
	var sent string
	send := func(_ context.Context, _ string, text string) error {
		sent = text
		return nil
	}
	changed, err := processChatSchedules(context.Background(), []chatTarget{target}, streams, &status, started, send)
	if err != nil || !changed || sent != "" {
		t.Fatalf("initial scheduling: changed=%v sent=%q err=%v", changed, sent, err)
	}
	delivery := status.ChatMessages["channel/repeat"]
	if delay := delivery.NextSentAt.Sub(started); delay < 10*time.Minute || delay > 30*time.Minute {
		t.Fatalf("initial random delay = %v, want 10..30 minutes", delay)
	}
	next := delivery.NextSentAt
	changed, err = processChatSchedules(context.Background(), []chatTarget{target}, streams, &status, started.Add(time.Minute), send)
	if err != nil || changed || status.ChatMessages["channel/repeat"].NextSentAt != next {
		t.Fatalf("persisted schedule was rerolled: changed=%v state=%+v err=%v", changed, status.ChatMessages["channel/repeat"], err)
	}
	changed, err = processChatSchedules(context.Background(), []chatTarget{target}, streams, &status, next, send)
	if err != nil || !changed || (sent != "alpha" && sent != "beta") {
		t.Fatalf("random send: changed=%v sent=%q err=%v", changed, sent, err)
	}
	delivery = status.ChatMessages["channel/repeat"]
	if delay := delivery.NextSentAt.Sub(next); delay < 10*time.Minute || delay > 30*time.Minute {
		t.Fatalf("next random delay = %v, want 10..30 minutes", delay)
	}
}

func TestReadChatTokenFromPrivateEnv(t *testing.T) {
	t.Setenv("TWITCH_CHAT_TOKEN", "")
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte("TWITCH_CHAT_TOKEN=oauth:test-token\n"), 0600); err != nil {
		t.Fatal(err)
	}
	token, err := readChatToken(path)
	if err != nil || token != "test-token" {
		t.Fatalf("token=%q err=%v", token, err)
	}
}

func TestChatOnlyTargetNeverStartsSteelWatch(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch request.URL.Host {
		case "api.twitch.tv":
			return response(http.StatusOK, `{"data":[{"id":"chat-stream","user_login":"chat_only"}]}`), nil
		case "id.twitch.tv":
			return response(http.StatusOK, `{"client_id":"test-client"}`), nil
		default:
			return nil, fmt.Errorf("unexpected request host %s", request.URL.Host)
		}
	})}
	watchCalls := 0
	sendCalls := 0
	watch := func(context.Context, *http.Client, config, string) error {
		watchCalls++
		return nil
	}
	send := func(_ context.Context, channel, text string) error {
		if channel != "chat_only" || text != "hello" {
			t.Fatalf("unexpected chat send: channel=%s text=%q", channel, text)
		}
		sendCalls++
		return nil
	}
	cfg := config{
		Channels: []string{"watch_only"}, TwitchToken: "token", PollSeconds: 10,
		Cookies: []cookie{{Name: "auth-token", Value: "viewer", Domain: ".twitch.tv"}},
	}
	chatCfg := chatConfig{Targets: []chatTarget{{Channel: "chat_only", Messages: []chatMessage{{ID: "hello", Text: "hello", Once: true}}}}}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	statePath := filepath.Join(t.TempDir(), "state.json")
	err := runServices(ctx, client, cfg, chatCfg, "test-client", statePath, watch, send)
	if !errors.Is(err, context.DeadlineExceeded) || watchCalls != 0 || sendCalls != 1 {
		t.Fatalf("chat-only behavior: watchCalls=%d sendCalls=%d err=%v", watchCalls, sendCalls, err)
	}
	loaded, err := loadState(statePath, cfg)
	if err != nil || loaded.ChatMessages["chat_only/hello"].SentAt.IsZero() {
		t.Fatalf("chat delivery was not persisted: state=%+v err=%v", loaded, err)
	}
}

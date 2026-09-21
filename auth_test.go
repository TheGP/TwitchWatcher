package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBrowserAuthEvidenceRequiresStableSamples(t *testing.T) {
	var evidence browserAuthEvidence
	for _, sample := range []struct {
		login, menu, confirmed bool
	}{
		{false, true, false}, // Shared menu can appear before the login button.
		{true, true, false},
		{false, true, false},
		{false, true, false},
		{false, true, true},
	} {
		confirmed, err := evidence.observe(sample.login, sample.menu)
		if err != nil || confirmed != sample.confirmed {
			t.Fatalf("sample=%+v confirmed=%t err=%v", sample, confirmed, err)
		}
	}
	if confirmed, err := evidence.observe(true, true); confirmed || err != nil {
		t.Fatalf("first login sample: confirmed=%t err=%v", confirmed, err)
	}
	if confirmed, err := evidence.observe(true, true); confirmed || !errors.Is(err, errTwitchAuthLost) {
		t.Fatalf("second login sample: confirmed=%t err=%v", confirmed, err)
	}
}

func TestAuthAlertContinuesWhenStateCannotBeSaved(t *testing.T) {
	sends := 0
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Host == "id.twitch.tv" {
			return response(http.StatusUnauthorized, `{"status":401}`), nil
		}
		if request.URL.Host == "api.telegram.org" {
			sends++
			return response(http.StatusOK, `{"ok":true}`), nil
		}
		t.Fatalf("unexpected request host %s", request.URL.Host)
		return nil, nil
	})}
	status := &state{}
	monitor := authMonitor{client: client, cfg: config{TelegramBot: "123:abcdef", TelegramChat: "42", Cookies: []cookie{{Name: "auth-token", Value: "invalid", Domain: ".twitch.tv"}}}, state: status, statePath: filepath.Join(t.TempDir(), "missing", "state.json")}
	canWatch, err := monitor.check(context.Background())
	if err != nil || canWatch || sends != 1 || !status.AuthLost || !status.AuthAlerted {
		t.Fatalf("alert not sent despite failed save: canWatch=%t sends=%d state=%+v err=%v", canWatch, sends, status, err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return fn(request) }

func response(code int, body string) *http.Response {
	return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader(body))}
}

func TestAuthAlertIsSentOnceAndPersisted(t *testing.T) {
	sends := 0
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch request.URL.Host {
		case "id.twitch.tv":
			return response(http.StatusUnauthorized, `{"status":401}`), nil
		case "api.telegram.org":
			sends++
			if request.Method != http.MethodPost || request.URL.Path != "/bot123:abcdef/sendMessage" {
				t.Fatalf("unexpected Telegram request: %s %s", request.Method, request.URL.Path)
			}
			body, err := io.ReadAll(request.Body)
			if err != nil {
				t.Fatal(err)
			}
			form, err := url.ParseQuery(string(body))
			if err != nil || form.Get("chat_id") != "42" || !strings.Contains(form.Get("text"), "Twitch Watcher") {
				t.Fatalf("unexpected Telegram form: %v", form)
			}
			return response(http.StatusOK, `{"ok":true}`), nil
		default:
			t.Fatalf("unexpected request host: %s", request.URL.Host)
			return nil, nil
		}
	})}
	cfg := config{TelegramBot: "123:abcdef", TelegramChat: "42", Cookies: []cookie{{Name: "auth-token", Value: "invalid", Domain: ".twitch.tv"}}}
	status := &state{}
	statePath := filepath.Join(t.TempDir(), "state.json")
	monitor := authMonitor{client: client, cfg: cfg, state: status, statePath: statePath}
	for range 2 {
		monitor.nextCheck = time.Time{}
		canWatch, err := monitor.check(context.Background())
		if err != nil || canWatch {
			t.Fatalf("auth check: canWatch=%t error=%v", canWatch, err)
		}
	}
	if sends != 1 || !status.AuthLost || !status.AuthAlerted {
		t.Fatalf("alert sends=%d state=%+v", sends, status)
	}
	data, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	var saved state
	if err := json.Unmarshal(data, &saved); err != nil || !saved.AuthLost || !saved.AuthAlerted {
		t.Fatalf("persisted auth state: %+v error=%v", saved, err)
	}
	restarted := authMonitor{client: client, cfg: cfg, state: &saved, statePath: statePath}
	if _, err := restarted.check(context.Background()); err != nil {
		t.Fatal(err)
	}
	if sends != 1 {
		t.Fatalf("restart repeated Telegram alert; sends=%d", sends)
	}
}

func TestAuthAlertRetriesAfterTelegramFailure(t *testing.T) {
	sends := 0
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Host == "id.twitch.tv" {
			return response(http.StatusUnauthorized, `{"status":401}`), nil
		}
		sends++
		if sends == 1 {
			return response(http.StatusBadGateway, `{"ok":false}`), nil
		}
		return response(http.StatusOK, `{"ok":true}`), nil
	})}
	status := &state{}
	monitor := authMonitor{client: client, cfg: config{TelegramBot: "123:abcdef", TelegramChat: "42", Cookies: []cookie{{Name: "auth-token", Value: "invalid", Domain: ".twitch.tv"}}}, state: status, statePath: filepath.Join(t.TempDir(), "state.json")}
	for range 2 {
		if _, err := monitor.check(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if sends != 2 || !status.AuthAlerted {
		t.Fatalf("expected retry then one latched alert, sends=%d state=%+v", sends, status)
	}
}

func TestTransientTwitchFailureDoesNotAlert(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Host != "id.twitch.tv" {
			t.Fatalf("unexpected request to %s", request.URL.Host)
		}
		return response(http.StatusServiceUnavailable, `{"status":503}`), nil
	})}
	status := &state{}
	monitor := authMonitor{client: client, cfg: config{Cookies: []cookie{{Name: "auth-token", Value: "token", Domain: ".twitch.tv"}}}, state: status, statePath: filepath.Join(t.TempDir(), "state.json")}
	canWatch, err := monitor.check(context.Background())
	if err != nil || !canWatch || status.AuthLost || status.AuthAlerted {
		t.Fatalf("transient outage treated as auth loss: canWatch=%t state=%+v err=%v", canWatch, status, err)
	}
}

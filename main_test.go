package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadConfigRejectsLocalProxyAndMissingAuth(t *testing.T) {
	base := config{
		Channel: "shaneboehm", TwitchToken: "token", SteelAPIKey: "key", TelegramBot: "123:abcdef", TelegramChat: "42",
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
		{"missing auth", func(cfg *config) { cfg.Cookies = nil }, true},
		{"missing Telegram", func(cfg *config) { cfg.TelegramBot = "" }, true},
		{"bad channel", func(cfg *config) { cfg.Channel = "bad/channel" }, true},
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
			_, err = loadConfig(path)
			if (err != nil) != test.wantError {
				t.Fatalf("loadConfig error = %v, wantError = %t", err, test.wantError)
			}
		})
	}
}

func TestRedactError(t *testing.T) {
	cfg := config{SteelAPIKey: "steel-secret", TwitchToken: "twitch-secret", ProxyURL: "http://proxy-secret@host:80", Cookies: []cookie{{Value: "cookie-secret"}}}
	message := redactError(errors.New("steel-secret twitch-secret http://proxy-secret@host:80 cookie-secret"), cfg)
	if strings.Contains(message, "secret") {
		t.Fatalf("secret leaked in log message: %s", message)
	}
}

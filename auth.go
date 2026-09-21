package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

var (
	errTwitchAuthLost  = errors.New("Twitch opened signed out")
	validTelegramToken = regexp.MustCompile(`^[0-9]+:[A-Za-z0-9_-]+$`)
)

// Twitch's test hook is preferred; scoped header button text is a fallback.
const twitchLoginButtonJS = `(() => {
	const login = document.querySelector('button[data-a-target="login-button"]');
	if (login && login.getClientRects().length) return true;
	return [...document.querySelectorAll('[data-a-target="user-card"] button')].some(button =>
		button.getClientRects().length && /^log in$/i.test((button.getAttribute('aria-label') || button.textContent || '').trim()));
})()`

type httpStatusError struct{ code int }

func (err httpStatusError) Error() string { return fmt.Sprintf("HTTP %d", err.code) }

func viewerToken(cfg config) string {
	for _, item := range cfg.Cookies {
		if item.Name == "auth-token" && (item.Domain == ".twitch.tv" || item.Domain == "twitch.tv") {
			return item.Value
		}
	}
	return ""
}

type browserAuthEvidence struct {
	loginSamples    int
	signedInSamples int
}

func (evidence *browserAuthEvidence) observe(login, menu bool) (bool, error) {
	if login {
		evidence.loginSamples++
		evidence.signedInSamples = 0
		if evidence.loginSamples >= 2 {
			return false, errTwitchAuthLost
		}
		return false, nil
	}
	evidence.loginSamples = 0
	if menu {
		evidence.signedInSamples++
		return evidence.signedInSamples >= 3, nil
	}
	evidence.signedInSamples = 0
	return false, nil
}

// confirmBrowserAuth waits for Twitch's hydrated UI. The user menu also exists
// when signed out, so three stable samples without a login button are needed.
func confirmBrowserAuth(ctx context.Context, browser *cdpClient) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(8 * time.Second):
	}
	var states []struct {
		Login bool `json:"login"`
		Menu  bool `json:"menu"`
	}
	err := browser.Evaluate(ctx, `(async () => {
		const samples = [];
		for (let i = 0; i < 3; i++) {
			const menu = document.querySelector('[data-a-target="user-menu-toggle"],button[aria-label="User Menu"]');
			samples.push({login:`+twitchLoginButtonJS+`,menu:!!menu && !!menu.getClientRects().length});
			if (i < 2) await new Promise(resolve => setTimeout(resolve, 2000));
		}
		return samples;
	})()`, &states)
	if err != nil {
		return fmt.Errorf("read Twitch login state: %w", err)
	}
	var evidence browserAuthEvidence
	for _, state := range states {
		confirmed, err := evidence.observe(state.Login, state.Menu)
		if err != nil {
			return err
		}
		if confirmed {
			return nil
		}
	}
	return errors.New("Twitch login state could not be confirmed")
}

type authMonitor struct {
	client    *http.Client
	cfg       config
	state     *state
	statePath string
	nextCheck time.Time
}

func (monitor *authMonitor) check(ctx context.Context) (bool, error) {
	if !time.Now().Before(monitor.nextCheck) {
		_, err := validateTwitchToken(ctx, monitor.client, viewerToken(monitor.cfg))
		var status httpStatusError
		switch {
		case err == nil:
			monitor.nextCheck = time.Now().Add(time.Hour)
			if monitor.state.AuthLost {
				if err := probeSteel(ctx, monitor.client, monitor.cfg); err == nil {
					monitor.state.AuthLost = false
					monitor.state.AuthAlerted = false
					if err := saveState(monitor.statePath, *monitor.state); err != nil {
						return false, err
					}
					log.Print("Twitch browser login recovered")
				} else if !errors.Is(err, errTwitchAuthLost) {
					log.Printf("Twitch login recovery check failed: %s", redactError(err, monitor.cfg))
				}
				if monitor.state.AuthLost {
					monitor.nextCheck = time.Now().Add(time.Minute)
				}
			}
		case errors.As(err, &status) && status.code == http.StatusUnauthorized:
			monitor.nextCheck = time.Now().Add(time.Minute)
			monitor.markLost()
		default:
			monitor.nextCheck = time.Now().Add(time.Minute)
			log.Printf("Twitch login token check failed: %s", redactError(err, monitor.cfg))
		}
	}
	if monitor.state.AuthLost && !monitor.state.AuthAlerted {
		monitor.notify(ctx)
	}
	return !monitor.state.AuthLost, nil
}

func (monitor *authMonitor) markLost() {
	monitor.nextCheck = time.Now().Add(time.Minute)
	if !monitor.state.AuthLost {
		monitor.state.AuthLost = true
		if err := saveState(monitor.statePath, *monitor.state); err != nil {
			log.Printf("Could not persist Twitch auth loss state: %v", err)
		}
		log.Print("Twitch Firefox browser authentication lost")
	}
}

func (monitor *authMonitor) notify(ctx context.Context) {
	if monitor.state.AuthAlerted {
		return
	}
	message := "⚠️ Twitch Watcher: saved Firefox login is no longer authenticated on Twitch. Refresh config.json from the logged-in Firefox profile, copy it to the Linux server, and run npm run deploy."
	if err := sendTelegram(ctx, monitor.client, monitor.cfg.TelegramBot, monitor.cfg.TelegramChat, message); err != nil {
		log.Printf("Telegram auth alert failed: %v", err)
		return
	}
	monitor.state.AuthAlerted = true
	if err := saveState(monitor.statePath, *monitor.state); err != nil {
		log.Printf("Could not persist Telegram auth alert state: %v", err)
	} else {
		log.Print("Telegram Twitch auth alert sent")
	}
}

func sendTelegram(ctx context.Context, client *http.Client, token, chatID, message string) error {
	if !validTelegramToken.MatchString(token) {
		return errors.New("invalid Telegram bot token format")
	}
	form := url.Values{"chat_id": {chatID}, "text": {message}}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.telegram.org/bot"+token+"/sendMessage", strings.NewReader(form.Encode()))
	if err != nil {
		return errors.New("cannot prepare Telegram request")
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := client.Do(request)
	if err != nil {
		return errors.New("Telegram request failed; check network access")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("Telegram returned HTTP %d", response.StatusCode)
	}
	var result struct {
		OK bool `json:"ok"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&result); err != nil || !result.OK {
		return errors.New("Telegram rejected the message")
	}
	return nil
}

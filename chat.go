package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

const twitchIRC = "wss://irc-ws.chat.twitch.tv:443"

var chatMessageID = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

type chatConfig struct {
	Targets []chatTarget `json:"targets"`
}

type chatTarget struct {
	Channel  string        `json:"channel"`
	Messages []chatMessage `json:"messages"`
}

type chatMessage struct {
	ID              string `json:"id"`
	Text            string `json:"text"`
	DelaySeconds    int    `json:"delay_seconds"`
	Once            bool   `json:"once,omitempty"`
	IntervalSeconds int    `json:"interval_seconds,omitempty"`
}

type chatIdentity struct {
	Login string
	Token string
}

type chatStreamState struct {
	StreamID   string    `json:"stream_id"`
	DetectedAt time.Time `json:"detected_at"`
}

type chatMessageState struct {
	SentAt     time.Time `json:"sent_at,omitempty"`
	StreamID   string    `json:"stream_id,omitempty"`
	LastSentAt time.Time `json:"last_sent_at,omitempty"`
}

type chatSendFunc func(context.Context, string, string) error

func loadChatConfig(path string) (chatConfig, error) {
	var cfg chatConfig
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return cfg, nil
	}
	if err != nil {
		return cfg, fmt.Errorf("read chat config: %w", err)
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("parse chat config: %w", err)
	}
	seenChannels := make(map[string]bool, len(cfg.Targets))
	for targetIndex := range cfg.Targets {
		target := &cfg.Targets[targetIndex]
		target.Channel = strings.ToLower(strings.TrimSpace(target.Channel))
		if !twitchLogin.MatchString(target.Channel) || seenChannels[target.Channel] || len(target.Messages) == 0 {
			return cfg, fmt.Errorf("invalid, duplicate, or empty chat target %q", target.Channel)
		}
		seenChannels[target.Channel] = true
		seenIDs := make(map[string]bool, len(target.Messages))
		for _, message := range target.Messages {
			if !chatMessageID.MatchString(message.ID) || seenIDs[message.ID] {
				return cfg, fmt.Errorf("invalid or duplicate chat message ID %q for %s", message.ID, target.Channel)
			}
			seenIDs[message.ID] = true
			if message.Text == "" || strings.TrimSpace(message.Text) != message.Text || strings.ContainsAny(message.Text, "\r\n") || len(message.Text) > 400 {
				return cfg, fmt.Errorf("chat message %s/%s must be 1..400 bytes without surrounding whitespace or newlines", target.Channel, message.ID)
			}
			if message.DelaySeconds < 0 || message.DelaySeconds > 86400 {
				return cfg, fmt.Errorf("invalid delay for chat message %s/%s", target.Channel, message.ID)
			}
			if message.Once == (message.IntervalSeconds > 0) || (!message.Once && message.IntervalSeconds < 60) || message.IntervalSeconds > 86400 {
				return cfg, fmt.Errorf("chat message %s/%s must be once or have a 60..86400 second interval", target.Channel, message.ID)
			}
		}
	}
	return cfg, nil
}

func chatChannels(cfg chatConfig) []string {
	channels := make([]string, 0, len(cfg.Targets))
	for _, target := range cfg.Targets {
		channels = append(channels, target.Channel)
	}
	return channels
}

func monitoredChannels(cfg config, chatCfg chatConfig) []string {
	channels := slices.Clone(cfg.Channels)
	seen := make(map[string]bool, len(channels)+len(chatCfg.Targets))
	for _, channel := range channels {
		seen[channel] = true
	}
	for _, target := range chatCfg.Targets {
		if !seen[target.Channel] {
			channels = append(channels, target.Channel)
			seen[target.Channel] = true
		}
	}
	return channels
}

func readChatToken(path string) (string, error) {
	if token := strings.TrimPrefix(strings.TrimSpace(os.Getenv("TWITCH_CHAT_TOKEN")), "oauth:"); token != "" {
		return token, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read chat token: %w", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		key, value, found := strings.Cut(strings.TrimSpace(line), "=")
		if found && key == "TWITCH_CHAT_TOKEN" {
			value = strings.Trim(strings.TrimSpace(value), `"'`)
			value = strings.TrimPrefix(value, "oauth:")
			if value != "" {
				return value, nil
			}
		}
	}
	return "", errors.New("TWITCH_CHAT_TOKEN is missing")
}

func validateChatToken(ctx context.Context, client *http.Client, token string) (chatIdentity, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://id.twitch.tv/oauth2/validate", nil)
	if err != nil {
		return chatIdentity{}, err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	var response struct {
		Login  string   `json:"login"`
		Scopes []string `json:"scopes"`
	}
	if err := getJSON(client, request, &response); err != nil {
		return chatIdentity{}, fmt.Errorf("validate Twitch chat token: %w", err)
	}
	if response.Login == "" || !slices.Contains(response.Scopes, "chat:read") || !slices.Contains(response.Scopes, "chat:edit") {
		return chatIdentity{}, errors.New("Twitch chat token requires a login plus chat:read and chat:edit scopes")
	}
	return chatIdentity{Login: strings.ToLower(response.Login), Token: token}, nil
}

func processChatSchedules(ctx context.Context, targets []chatTarget, streams map[string]string, status *state, now time.Time, send chatSendFunc) (bool, error) {
	changed := false
	for _, target := range targets {
		streamID := streams[target.Channel]
		if streamID == "" {
			continue
		}
		stream := status.ChatStreams[target.Channel]
		if stream.StreamID != streamID {
			stream = chatStreamState{StreamID: streamID, DetectedAt: now}
			status.ChatStreams[target.Channel] = stream
			changed = true
		}
		for _, message := range target.Messages {
			key := target.Channel + "/" + message.ID
			delivery := status.ChatMessages[key]
			var due time.Time
			if message.Once {
				if !delivery.SentAt.IsZero() {
					continue
				}
				due = stream.DetectedAt.Add(time.Duration(message.DelaySeconds) * time.Second)
			} else if delivery.StreamID != streamID {
				due = stream.DetectedAt.Add(time.Duration(message.DelaySeconds) * time.Second)
			} else {
				due = delivery.LastSentAt.Add(time.Duration(message.IntervalSeconds) * time.Second)
			}
			if now.Before(due) {
				continue
			}
			if err := send(ctx, target.Channel, message.Text); err != nil {
				return changed, fmt.Errorf("send chat message %s/%s: %w", target.Channel, message.ID, err)
			}
			if message.Once {
				delivery.SentAt = now
			} else {
				delivery.StreamID = streamID
				delivery.LastSentAt = now
			}
			status.ChatMessages[key] = delivery
			log.Printf("chat channel=%s message=%s sent", target.Channel, message.ID)
			return true, nil // At most one message per poll prevents catch-up bursts.
		}
	}
	return changed, nil
}

func sendTwitchChat(ctx context.Context, identity chatIdentity, channel, message string) error {
	if !twitchLogin.MatchString(channel) || message == "" || len(message) > 400 || strings.ContainsAny(message, "\r\n") {
		return errors.New("invalid Twitch chat channel or message")
	}
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, twitchIRC, nil)
	if err != nil {
		return fmt.Errorf("connect Twitch IRC: %w", err)
	}
	defer conn.Close()
	deadline := time.Now().Add(20 * time.Second)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	if err := conn.SetReadDeadline(deadline); err != nil {
		return err
	}
	if err := conn.SetWriteDeadline(deadline); err != nil {
		return err
	}
	for _, command := range []string{
		"PASS oauth:" + identity.Token,
		"NICK " + identity.Login,
		"CAP REQ :twitch.tv/tags twitch.tv/commands",
		"JOIN #" + channel,
	} {
		if err := conn.WriteMessage(websocket.TextMessage, []byte(command+"\r\n")); err != nil {
			return fmt.Errorf("authenticate Twitch IRC: %w", err)
		}
	}
	if err := waitIRC(conn, channel, "ROOMSTATE", "USERSTATE"); err != nil {
		return err
	}
	if err := conn.WriteMessage(websocket.TextMessage, []byte("PRIVMSG #"+channel+" :"+message+"\r\n")); err != nil {
		return fmt.Errorf("send Twitch PRIVMSG: %w", err)
	}
	if err := waitIRC(conn, channel, "USERSTATE"); err != nil {
		return fmt.Errorf("confirm Twitch chat message: %w", err)
	}
	return nil
}

func waitIRC(conn *websocket.Conn, channel string, expected ...string) error {
	seen := make(map[string]bool, len(expected))
	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			return fmt.Errorf("read Twitch IRC: %w", err)
		}
		for _, line := range strings.Split(string(raw), "\r\n") {
			switch {
			case strings.HasPrefix(line, "PING "):
				if err := conn.WriteMessage(websocket.TextMessage, []byte("PONG "+strings.TrimPrefix(line, "PING ")+"\r\n")); err != nil {
					return err
				}
			case strings.Contains(line, " NOTICE "):
				return errors.New("Twitch IRC rejected the request")
			default:
				for _, command := range expected {
					if strings.Contains(line, " "+command+" #"+channel) {
						seen[command] = true
					}
				}
				if len(seen) == len(expected) {
					return nil
				}
			}
		}
	}
}

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

// cdpClient sends only the browser commands this watcher needs. Steel's browser
// websocket uses the Chrome DevTools Protocol with flat target sessions.
type cdpClient struct {
	conn      *websocket.Conn
	sessionID string
	nextID    int
}

func connectCDP(ctx context.Context, websocketURL string) (*cdpClient, error) {
	parsed, err := url.Parse(websocketURL)
	if err != nil || parsed.Scheme != "wss" || parsed.Hostname() != "connect.steel.dev" {
		return nil, errors.New("invalid Steel browser websocket URL")
	}
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, websocketURL, nil)
	if err != nil {
		return nil, err
	}
	browser := &cdpClient{conn: conn}
	var targets struct {
		TargetInfos []struct {
			ID   string `json:"targetId"`
			Type string `json:"type"`
		} `json:"targetInfos"`
	}
	if err := browser.call(ctx, false, "Target.getTargets", nil, &targets); err != nil {
		browser.Close()
		return nil, err
	}
	var targetID string
	for _, target := range targets.TargetInfos {
		if target.Type == "page" {
			targetID = target.ID
			break
		}
	}
	if targetID == "" {
		var created struct {
			TargetID string `json:"targetId"`
		}
		if err := browser.call(ctx, false, "Target.createTarget", map[string]any{"url": "about:blank"}, &created); err != nil {
			browser.Close()
			return nil, err
		}
		targetID = created.TargetID
	}
	if targetID == "" {
		browser.Close()
		return nil, errors.New("Steel browser has no page target")
	}
	var attached struct {
		SessionID string `json:"sessionId"`
	}
	if err := browser.call(ctx, false, "Target.attachToTarget", map[string]any{"targetId": targetID, "flatten": true}, &attached); err != nil {
		browser.Close()
		return nil, err
	}
	if attached.SessionID == "" {
		browser.Close()
		return nil, errors.New("Steel browser did not attach to the page")
	}
	browser.sessionID = attached.SessionID
	return browser, nil
}

func (browser *cdpClient) Close() error { return browser.conn.Close() }

func (browser *cdpClient) call(ctx context.Context, page bool, method string, params any, result any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	browser.nextID++
	id := browser.nextID
	request := map[string]any{"id": id, "method": method}
	if params != nil {
		request["params"] = params
	}
	if page {
		request["sessionId"] = browser.sessionID
	}
	deadline := time.Now().Add(20 * time.Second)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	if err := browser.conn.SetWriteDeadline(deadline); err != nil {
		return err
	}
	if err := browser.conn.WriteJSON(request); err != nil {
		return fmt.Errorf("send %s: %w", method, err)
	}
	if err := browser.conn.SetReadDeadline(deadline); err != nil {
		return err
	}
	for {
		_, raw, err := browser.conn.ReadMessage()
		if err != nil {
			return fmt.Errorf("read %s: %w", method, err)
		}
		var reply struct {
			ID     int             `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(raw, &reply); err != nil {
			return err
		}
		if reply.ID != id {
			continue // Unsolicited page event or an earlier response.
		}
		if reply.Error != nil {
			return fmt.Errorf("%s: %s", method, reply.Error.Message)
		}
		if result != nil {
			return json.Unmarshal(reply.Result, result)
		}
		return nil
	}
}

func (browser *cdpClient) SetCookies(ctx context.Context, cookies []cookie) error {
	params := make([]map[string]any, 0, len(cookies))
	for _, item := range cookies {
		if !(item.Domain == "twitch.tv" || strings.HasSuffix(item.Domain, ".twitch.tv")) || item.Name == "" || item.Value == "" {
			continue
		}
		value := map[string]any{
			"name": item.Name, "value": item.Value, "domain": item.Domain,
			"path": item.Path, "secure": item.Secure, "httpOnly": item.HTTPOnly,
		}
		if item.Expires > 0 {
			value["expires"] = float64(item.Expires) / 1000
		}
		params = append(params, value)
	}
	return browser.call(ctx, true, "Network.setCookies", map[string]any{"cookies": params}, nil)
}

func (browser *cdpClient) Navigate(ctx context.Context, pageURL string) error {
	var reply struct {
		ErrorText string `json:"errorText"`
	}
	if err := browser.call(ctx, true, "Page.navigate", map[string]any{"url": pageURL}, &reply); err != nil {
		return err
	}
	if reply.ErrorText != "" {
		return errors.New(reply.ErrorText)
	}
	return nil
}

func (browser *cdpClient) Evaluate(ctx context.Context, expression string, value any) error {
	var reply struct {
		Result struct {
			Value json.RawMessage `json:"value"`
		} `json:"result"`
		ExceptionDetails json.RawMessage `json:"exceptionDetails"`
	}
	if err := browser.call(ctx, true, "Runtime.evaluate", map[string]any{"expression": expression, "returnByValue": true}, &reply); err != nil {
		return err
	}
	if len(reply.ExceptionDetails) > 0 {
		return errors.New("JavaScript evaluation failed")
	}
	if len(reply.Result.Value) == 0 {
		return errors.New("JavaScript evaluation returned no value")
	}
	return json.Unmarshal(reply.Result.Value, value)
}

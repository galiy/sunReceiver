// maxbot
// Copyright (C) 2026  Aleksandr Galinskii
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with this program.  If not, see <https://www.gnu.org/licenses/>.

package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"
)

// dshClient — HTTP-клиент unary API DeepSeek Harness (dsh).
//
// Авторизация: launch token печатается процессом dsh в journald. Клиент
// извлекает его, обменивает на подписанную cookie через GET /connect и дальше
// шлёт POST /api/<endpoint> с envelope {type,rpcId,method,payload:{args}}.
type dshClient struct {
	baseURL string
	unit    string // systemd-юнит harness (для journalctl)
	hc      *http.Client

	mu     sync.Mutex
	token  string
	cookie string

	// tokenFetcher — подмена извлечения токена в тестах.
	tokenFetcher func(ctx context.Context) (string, error)
}

func newDSHClient(baseURL, unit string) *dshClient {
	if unit == "" {
		unit = "deepseek-harness"
	}
	return &dshClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		unit:    unit,
		hc:      &http.Client{Timeout: 90 * time.Second},
	}
}

var launchTokenRe = regexp.MustCompile(`token=([A-Za-z0-9_-]{16,})`)

// discoverToken достаёт последний launch token из журнала systemd.
func (c *dshClient) discoverToken(ctx context.Context) (string, error) {
	if c.tokenFetcher != nil {
		return c.tokenFetcher(ctx)
	}
	// Читаем весь текущий boot: launch token печатается при старте harness, а
	// хвоста журнала может не хватить, если harness работает давно.
	jctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	// -g фильтрует на стороне journalctl, чтобы не тянуть весь boot в память.
	cmd := exec.CommandContext(jctx, "journalctl", "-u", c.unit, "-b", "-o", "cat", "--no-pager", "-g", "token=")
	out, err := cmd.Output()
	if err != nil {
		// Старый journalctl без -g: читаем boot целиком (редкий fallback).
		cmd = exec.CommandContext(jctx, "journalctl", "-u", c.unit, "-b", "-o", "cat", "--no-pager")
		out, err = cmd.Output()
		if err != nil {
			return "", fmt.Errorf("journalctl -u %s: %w (нужен доступ к journald, например группа systemd-journal)", c.unit, err)
		}
	}
	m := launchTokenRe.FindAllStringSubmatch(string(out), -1)
	if len(m) == 0 {
		return "", fmt.Errorf("launch token harness не найден в журнале %s", c.unit)
	}
	return m[len(m)-1][1], nil
}

// resetAuth сбрасывает закэшированные токен и cookie (например, после 401 или
// перезапуска harness).
func (c *dshClient) resetAuth() {
	c.mu.Lock()
	c.token = ""
	c.cookie = ""
	c.mu.Unlock()
}

// ensureAuth гарантирует наличие cookie: при необходимости получает токен и
// выполняет GET /connect.
func (c *dshClient) ensureAuth(ctx context.Context) (string, error) {
	c.mu.Lock()
	if c.cookie != "" {
		cookie := c.cookie
		c.mu.Unlock()
		return cookie, nil
	}
	c.mu.Unlock()

	token, err := c.discoverToken(ctx)
	if err != nil {
		return "", err
	}
	u := c.baseURL + "/connect?interactions=none&token=" + url.QueryEscape(token)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	// Редирект на /api/remote.mux не нужен: cookie уже в ответе.
	client := *c.hc
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		// url.Error печатает полный URL с launch token — не даём ему утечь в
		// сообщения оператору/логи.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return "", fmt.Errorf("dsh connect: %w", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusFound && resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return "", fmt.Errorf("dsh connect: http %d", resp.StatusCode)
	}
	var cookies []string
	for _, sc := range resp.Header.Values("Set-Cookie") {
		if i := strings.IndexByte(sc, ';'); i >= 0 {
			sc = sc[:i]
		}
		if strings.Contains(sc, "=") {
			cookies = append(cookies, sc)
		}
	}
	if len(cookies) == 0 {
		return "", fmt.Errorf("dsh connect: сервер не выдал cookie")
	}
	cookie := strings.Join(cookies, "; ")
	c.mu.Lock()
	c.token = token
	c.cookie = cookie
	c.mu.Unlock()
	return cookie, nil
}

// dshAPIError — ошибка remote-вызова harness.
type dshAPIError struct {
	Code    string          `json:"code"`
	Message string          `json:"message"`
	Details json.RawMessage `json:"details"`
}

func (e *dshAPIError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("%s: %s", e.Code, e.Message)
	}
	return e.Message
}

// call выполняет unary-вызов /api/<method> и декодирует значение в out.
func (c *dshClient) call(ctx context.Context, method string, args any, out any) error {
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		cookie, err := c.ensureAuth(ctx)
		if err != nil {
			return err
		}
		err = c.callOnce(ctx, method, args, out, cookie)
		if err == nil {
			return nil
		}
		var httpErr *dshHTTPStatus
		if !errors.As(err, &httpErr) || httpErr.Status != http.StatusUnauthorized {
			return err
		}
		// Cookie/токен могли устареть (перезапуск harness) — повторяем один раз.
		c.resetAuth()
		lastErr = err
	}
	return lastErr
}

// dshHTTPStatus — HTTP-ошибка с кодом статуса (для обработки 401).
type dshHTTPStatus struct {
	Status int
	Body   string
}

func (e *dshHTTPStatus) Error() string {
	return fmt.Sprintf("dsh: http %d: %s", e.Status, e.Body)
}

func (c *dshClient) callOnce(ctx context.Context, method string, args any, out any, cookie string) error {
	if args == nil {
		args = map[string]any{}
	}
	rid := newUUID()
	envelope := map[string]any{
		"type":    "client-request",
		"rpcId":   rid,
		"method":  method,
		"payload": map[string]any{"args": args},
	}
	body, err := json.Marshal(envelope)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/"+method, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Cookie", cookie)
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("dsh %s: %w", method, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return fmt.Errorf("dsh %s: чтение ответа: %w", method, err)
	}
	if resp.StatusCode != http.StatusOK {
		return &dshHTTPStatus{Status: resp.StatusCode, Body: truncate(string(raw), 300)}
	}
	var envelopeResp struct {
		Type   string          `json:"type"`
		RPCID  string          `json:"rpcId"`
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(raw, &envelopeResp); err != nil {
		return fmt.Errorf("dsh %s: разбор ответа: %w", method, err)
	}
	if envelopeResp.Type != "server-response" {
		return fmt.Errorf("dsh %s: неожиданный тип ответа %q", method, envelopeResp.Type)
	}
	if envelopeResp.RPCID != rid {
		return fmt.Errorf("dsh %s: rpcId ответа не совпадает с запросом", method)
	}
	if len(envelopeResp.Result) == 0 || string(envelopeResp.Result) == "null" {
		return fmt.Errorf("dsh %s: пустой result (rpcId %s)", method, rid)
	}
	var result struct {
		OK    bool            `json:"ok"`
		Value json.RawMessage `json:"value"`
		Error *dshAPIError    `json:"error"`
	}
	if err := json.Unmarshal(envelopeResp.Result, &result); err != nil {
		return fmt.Errorf("dsh %s: разбор result: %w", method, err)
	}
	if !result.OK {
		if result.Error != nil {
			return result.Error
		}
		return fmt.Errorf("dsh %s: remote-вызов не удался", method)
	}
	if out != nil && len(result.Value) > 0 {
		if err := json.Unmarshal(result.Value, out); err != nil {
			return fmt.Errorf("dsh %s: разбор value: %w", method, err)
		}
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// newUUID — UUID v4 для rpcId.
func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "00000000-0000-4000-8000-000000000000"
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

// ---- типы API ----

type modelSelection struct {
	Provider        string `json:"provider"`
	Model           string `json:"model"`
	ReasoningEffort string `json:"reasoningEffort,omitempty"`
}

type modelCatalog struct {
	Default           modelSelection `json:"default"`
	RoutableProviders []string       `json:"routableProviders"`
	Groups            []struct {
		ID     string `json:"id"`
		Name   string `json:"name"`
		Models []struct {
			ID        string `json:"id"`
			Name      string `json:"name"`
			Reasoning *struct {
				Efforts []struct {
					ID string `json:"id"`
				} `json:"efforts"`
			} `json:"reasoning"`
		} `json:"models"`
	} `json:"groups"`
	Failures []struct {
		ID      string `json:"id"`
		Name    string `json:"name"`
		Message string `json:"message"`
	} `json:"failures"`
}

// supportsEffort — поддерживает ли модель данный reasoning effort.
func (c *modelCatalog) supportsEffort(provider, model, effort string) bool {
	for _, g := range c.Groups {
		if g.ID != provider {
			continue
		}
		for _, m := range g.Models {
			if m.ID != model {
				continue
			}
			if m.Reasoning == nil {
				return false
			}
			for _, e := range m.Reasoning.Efforts {
				if e.ID == effort {
					return true
				}
			}
			return false
		}
	}
	return false
}

// hasModel — есть ли модель в каталоге.
func (c *modelCatalog) hasModel(provider, model string) bool {
	for _, g := range c.Groups {
		if g.ID != provider {
			continue
		}
		for _, m := range g.Models {
			if m.ID == model {
				return true
			}
		}
	}
	return false
}

type promptPart struct {
	Type      string `json:"type"`
	Text      string `json:"text,omitempty"`
	MediaType string `json:"mediaType,omitempty"`
	Data      string `json:"data,omitempty"`
	Name      string `json:"name,omitempty"`
}

type sessionRecord struct {
	Type  string       `json:"type"`
	Event sessionEvent `json:"event"`
}

type sessionEvent struct {
	Type string          `json:"type"`
	Seq  int64           `json:"seq"`
	Time int64           `json:"time"`
	Data json.RawMessage `json:"data"`
}

type sessionEventsPage struct {
	Records []sessionRecord `json:"records"`
	HasMore bool            `json:"hasMore"`
	Cursor  int64           `json:"cursor"`
}

type sessionStateValue struct {
	Cursor       int64    `json:"cursor"`
	Active       bool     `json:"active"`
	Running      bool     `json:"running"`
	Queue        []any    `json:"queue"`
	PendingInput []string `json:"pendingInput"`
}

type sessionWaitValue struct {
	Reason string `json:"reason"`
	State  struct {
		Cursor       int64    `json:"cursor"`
		Active       bool     `json:"active"`
		Running      bool     `json:"running"`
		PendingInput []string `json:"pendingInput"`
	} `json:"state"`
	Output struct {
		Records   []sessionRecord `json:"records"`
		Truncated bool            `json:"truncated"`
	} `json:"output"`
}

func (c *dshClient) modelCatalog(ctx context.Context) (*modelCatalog, error) {
	var out modelCatalog
	if err := c.call(ctx, "session/modelCatalog", map[string]any{}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *dshClient) createSession(ctx context.Context, cwd string) (string, error) {
	var out struct {
		SessionID string `json:"sessionId"`
	}
	err := c.call(ctx, "session/create", map[string]any{
		"request": map[string]any{"cwd": cwd},
	}, &out)
	if err != nil {
		return "", err
	}
	if out.SessionID == "" {
		return "", fmt.Errorf("dsh: пустой sessionId")
	}
	return out.SessionID, nil
}

func (c *dshClient) selectModel(ctx context.Context, sessionID, provider, model, effort string) error {
	req := map[string]any{"sessionId": sessionID, "provider": provider, "model": model}
	if effort != "" {
		req["reasoningEffort"] = effort
	}
	return c.call(ctx, "session/selectModel", map[string]any{"request": req}, nil)
}

func (c *dshClient) prompt(ctx context.Context, sessionID, requestID string, content []promptPart) error {
	return c.call(ctx, "session/prompt", map[string]any{
		"request": map[string]any{
			"requestId": requestID,
			"sessionId": sessionID,
			"mode":      "queue",
			"content":   content,
		},
	}, nil)
}

func (c *dshClient) state(ctx context.Context, sessionID string) (*sessionStateValue, error) {
	var out sessionStateValue
	if err := c.call(ctx, "session/state", map[string]any{
		"request": map[string]any{"sessionId": sessionID},
	}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *dshClient) wait(ctx context.Context, sessionID string, afterSeq int64, timeoutMs int) (*sessionWaitValue, error) {
	req := map[string]any{"sessionId": sessionID, "timeoutMs": timeoutMs}
	if afterSeq > 0 {
		req["afterSeq"] = afterSeq
	}
	var out sessionWaitValue
	if err := c.call(ctx, "session/wait", map[string]any{"request": req}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *dshClient) events(ctx context.Context, sessionID string, throughSeq int64, maxMessages int) (*sessionEventsPage, error) {
	req := map[string]any{
		"address":     map[string]any{"kind": "session", "sessionId": sessionID},
		"maxMessages": maxMessages,
	}
	if throughSeq > 0 {
		req["throughSeq"] = throughSeq
	}
	var out sessionEventsPage
	if err := c.call(ctx, "session/events", map[string]any{"request": req}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *dshClient) cancel(ctx context.Context, sessionID string) error {
	return c.call(ctx, "session/cancel", map[string]any{
		"request": map[string]any{"sessionId": sessionID},
	}, nil)
}

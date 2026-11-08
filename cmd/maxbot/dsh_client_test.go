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
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// newTestDSHClient поднимает httptest-сервер harness и клиент с подменённым
// токеном. api — обработчик /api/<method>.
func newTestDSHClient(t *testing.T, api http.HandlerFunc, connects *atomic.Int32) (*dshClient, *httptest.Server) {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/connect":
			if connects != nil {
				connects.Add(1)
			}
			if r.URL.Query().Get("token") != "test-token" {
				http.Error(w, "bad token", http.StatusUnauthorized)
				return
			}
			http.SetCookie(w, &http.Cookie{Name: "dsh-test", Value: "c", Path: "/"})
			w.Header().Set("Location", "./api/remote.mux")
			w.WriteHeader(http.StatusFound)
		case strings.HasPrefix(r.URL.Path, "/api/"):
			if c, err := r.Cookie("dsh-test"); err != nil || c.Value != "c" {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			api(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	c := newDSHClient(srv.URL, "unit")
	c.tokenFetcher = func(context.Context) (string, error) { return "test-token", nil }
	return c, srv
}

func writeRemoteResult(t *testing.T, w http.ResponseWriter, rpcID string, value any) {
	t.Helper()
	enc, err := json.Marshal(map[string]any{
		"type":  "server-response",
		"rpcId": rpcID,
		"result": map[string]any{
			"ok":    true,
			"value": value,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(enc)
}

func TestDSHModelCatalog(t *testing.T) {
	var gotMethod string
	c, srv := newTestDSHClient(t, func(w http.ResponseWriter, r *http.Request) {
		var env struct {
			Type    string         `json:"type"`
			RPCID   string         `json:"rpcId"`
			Method  string         `json:"method"`
			Payload map[string]any `json:"payload"`
		}
		if err := json.NewDecoder(r.Body).Decode(&env); err != nil {
			t.Errorf("envelope: %v", err)
		}
		gotMethod = env.Method
		if env.Type != "client-request" {
			t.Errorf("type = %q", env.Type)
		}
		writeRemoteResult(t, w, env.RPCID, map[string]any{
			"default": map[string]any{"provider": "wbllm", "model": "coder-medium", "reasoningEffort": "low"},
			"groups": []any{map[string]any{
				"id": "wbllm", "name": "WB LLM",
				"models": []any{map[string]any{"id": "coder-medium", "name": "coder-medium"}},
			}},
		})
	}, nil)
	defer srv.Close()

	cat, err := c.modelCatalog(context.Background())
	if err != nil {
		t.Fatalf("modelCatalog: %v", err)
	}
	if gotMethod != "session/modelCatalog" {
		t.Fatalf("method = %q", gotMethod)
	}
	if cat.Default.Model != "coder-medium" || !cat.hasModel("wbllm", "coder-medium") {
		t.Fatalf("catalog = %+v", cat)
	}
}

func TestDSHCallRetriesOn401(t *testing.T) {
	var connects atomic.Int32
	var calls atomic.Int32
	c, srv := newTestDSHClient(t, func(w http.ResponseWriter, r *http.Request) {
		var env struct {
			RPCID string `json:"rpcId"`
		}
		_ = json.NewDecoder(r.Body).Decode(&env)
		if calls.Add(1) == 1 {
			http.Error(w, "stale cookie", http.StatusUnauthorized)
			return
		}
		writeRemoteResult(t, w, env.RPCID, map[string]any{})
	}, &connects)
	defer srv.Close()

	if _, err := c.modelCatalog(context.Background()); err != nil {
		t.Fatalf("повтор после 401: %v", err)
	}
	if connects.Load() != 2 {
		t.Fatalf("connect вызван %d раз, want 2", connects.Load())
	}
}

func TestDSHCallError(t *testing.T) {
	c, srv := newTestDSHClient(t, func(w http.ResponseWriter, r *http.Request) {
		var env struct {
			RPCID string `json:"rpcId"`
		}
		_ = json.NewDecoder(r.Body).Decode(&env)
		enc, _ := json.Marshal(map[string]any{
			"type": "server-response", "rpcId": env.RPCID,
			"result": map[string]any{
				"ok":    false,
				"error": map[string]any{"code": "session/model-unavailable", "message": "нет модели"},
			},
		})
		w.Write(enc)
	}, nil)
	defer srv.Close()

	_, err := c.modelCatalog(context.Background())
	if err == nil || !strings.Contains(err.Error(), "model-unavailable") {
		t.Fatalf("ожидали ошибку remote, got %v", err)
	}
}

func TestDSHArgsShapes(t *testing.T) {
	type captured struct {
		method string
		args   map[string]any
	}
	var got []captured
	c, srv := newTestDSHClient(t, func(w http.ResponseWriter, r *http.Request) {
		var env struct {
			RPCID  string `json:"rpcId"`
			Method string `json:"method"`
			P      struct {
				Args map[string]any `json:"args"`
			} `json:"payload"`
		}
		if err := json.NewDecoder(r.Body).Decode(&env); err != nil {
			t.Errorf("envelope: %v", err)
		}
		got = append(got, captured{env.Method, env.P.Args})
		switch env.Method {
		case "session/create":
			writeRemoteResult(t, w, env.RPCID, map[string]any{"sessionId": "s1"})
		case "session/wait":
			writeRemoteResult(t, w, env.RPCID, map[string]any{"reason": "idle", "state": map[string]any{"cursor": 7}})
		case "session/events":
			writeRemoteResult(t, w, env.RPCID, map[string]any{"records": []any{}, "cursor": 7})
		default:
			writeRemoteResult(t, w, env.RPCID, map[string]any{})
		}
	}, nil)
	defer srv.Close()

	ctx := context.Background()
	if _, err := c.createSession(ctx, "/opt/dsh-bot"); err != nil {
		t.Fatal(err)
	}
	if err := c.selectModel(ctx, "s1", "wbllm", "coder-medium", "low"); err != nil {
		t.Fatal(err)
	}
	if err := c.prompt(ctx, "s1", "req-1", []promptPart{{Type: "text", Text: "привет"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.wait(ctx, "s1", 5, 30000); err != nil {
		t.Fatal(err)
	}
	if _, err := c.events(ctx, "s1", 7, 300); err != nil {
		t.Fatal(err)
	}
	if err := c.cancel(ctx, "s1"); err != nil {
		t.Fatal(err)
	}

	if len(got) != 6 {
		t.Fatalf("вызовов %d, want 6", len(got))
	}
	req := func(i int) map[string]any {
		r, _ := got[i].args["request"].(map[string]any)
		return r
	}
	if req(0)["cwd"] != "/opt/dsh-bot" {
		t.Fatalf("create args = %v", got[0].args)
	}
	if r := req(1); r["sessionId"] != "s1" || r["provider"] != "wbllm" || r["model"] != "coder-medium" || r["reasoningEffort"] != "low" {
		t.Fatalf("selectModel args = %v", r)
	}
	if r := req(2); r["requestId"] != "req-1" || r["sessionId"] != "s1" || r["mode"] != "queue" {
		t.Fatalf("prompt args = %v", r)
	} else if content, _ := r["content"].([]any); len(content) != 1 {
		t.Fatalf("prompt content = %v", r["content"])
	}
	if r := req(3); r["sessionId"] != "s1" || r["afterSeq"] != float64(5) || r["timeoutMs"] != float64(30000) {
		t.Fatalf("wait args = %v", r)
	}
	if r := req(4); r["throughSeq"] != float64(7) {
		t.Fatalf("events args = %v", r)
	} else if addr, _ := r["address"].(map[string]any); addr["kind"] != "session" || addr["sessionId"] != "s1" {
		t.Fatalf("events address = %v", r["address"])
	}
	if req(5)["sessionId"] != "s1" {
		t.Fatalf("cancel args = %v", got[5].args)
	}
}

func TestDSHState(t *testing.T) {
	var gotArgs map[string]any
	c, srv := newTestDSHClient(t, func(w http.ResponseWriter, r *http.Request) {
		var env struct {
			RPCID  string `json:"rpcId"`
			Method string `json:"method"`
			P      struct {
				Args map[string]any `json:"args"`
			} `json:"payload"`
		}
		_ = json.NewDecoder(r.Body).Decode(&env)
		gotArgs = env.P.Args
		writeRemoteResult(t, w, env.RPCID, map[string]any{"cursor": 9, "running": false, "active": false})
	}, nil)
	defer srv.Close()

	st, err := c.state(context.Background(), "s1")
	if err != nil {
		t.Fatal(err)
	}
	if st.Cursor != 9 {
		t.Fatalf("cursor = %d", st.Cursor)
	}
	req, _ := gotArgs["request"].(map[string]any)
	if req["sessionId"] != "s1" {
		t.Fatalf("state args = %v", gotArgs)
	}
}

func TestInputRequiredDedupAndUnblock(t *testing.T) {
	var cancels atomic.Int32
	c, srv := newTestDSHClient(t, func(w http.ResponseWriter, r *http.Request) {
		var env struct {
			RPCID string `json:"rpcId"`
		}
		_ = json.NewDecoder(r.Body).Decode(&env)
		if strings.HasSuffix(r.URL.Path, "/session/cancel") {
			cancels.Add(1)
		}
		writeRemoteResult(t, w, env.RPCID, map[string]any{})
	}, nil)
	defer srv.Close()

	b := &bot{dsh: c, dshCfg: &dshConfig{}, dshState: dshState{SessionID: "s1", PendingSchedText: "старый хвост"}}
	ctx := context.Background()
	text := b.handleInputRequired(ctx, "s1", "Что делать?", 42, 0)
	if !strings.Contains(text, "Что делать?") || cancels.Load() != 1 {
		t.Fatalf("первый вопрос: text=%q cancels=%d", text, cancels.Load())
	}
	if st := b.dshSnapshot(); !st.AwaitingInput || st.InputSeq != 42 || st.PendingSchedText != "" {
		t.Fatalf("состояние после вопроса: %+v", st)
	}
	if again := b.handleInputRequired(ctx, "s1", "Что делать?", 42, 0); again != "" || cancels.Load() != 1 {
		t.Fatalf("повтор того же вопроса не должен слать/отменять: %q cancels=%d", again, cancels.Load())
	}
	if text2 := b.handleInputRequired(ctx, "s1", "Новый вопрос?", 43, 0); !strings.Contains(text2, "Новый вопрос?") || cancels.Load() != 2 {
		t.Fatalf("новый вопрос должен обрабатываться: %q cancels=%d", text2, cancels.Load())
	}
	if err := b.unblockAwaitingInput(ctx); err != nil {
		t.Fatal(err)
	}
	if st := b.dshSnapshot(); st.AwaitingInput || st.InputSeq != 0 {
		t.Fatalf("unblock не сбросил состояние: %+v", st)
	}
	if cancels.Load() != 3 {
		t.Fatalf("unblock должен отменить ход: cancels=%d", cancels.Load())
	}
}

func TestWaitForAnswerPrefixesBrowserPrompt(t *testing.T) {
	c, srv := newTestDSHClient(t, func(w http.ResponseWriter, r *http.Request) {
		var env struct {
			RPCID  string `json:"rpcId"`
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&env)
		switch env.Method {
		case "session/wait":
			writeRemoteResult(t, w, env.RPCID, map[string]any{
				"reason": "idle",
				"state":  map[string]any{"cursor": 10},
			})
		case "session/events":
			rec := func(typ string, seq int64, data any) map[string]any {
				return map[string]any{"type": "event", "event": map[string]any{
					"type": typ, "seq": seq, "data": data,
				}}
			}
			user := rec("user/message", 1, map[string]any{
				"content": []any{map[string]any{"type": "text", "text": "Вопрос из браузера?"}},
				"source":  map[string]any{"kind": "user", "rpcId": "browser-req"},
			})
			start := rec("turn/start", 2, map[string]any{"turn": 1})
			answer := rec("assistant/message", 3, map[string]any{
				"message": map[string]any{"content": []any{
					map[string]any{"type": "text", "text": "ответ"},
				}},
			})
			end := rec("turn/end", 4, map[string]any{"reason": map[string]any{"kind": "completed"}})
			writeRemoteResult(t, w, env.RPCID, map[string]any{
				"records": []any{user, start, answer, end}, "cursor": 10, "hasMore": false,
			})
		default:
			writeRemoteResult(t, w, env.RPCID, map[string]any{})
		}
	}, nil)
	defer srv.Close()

	b := &bot{dsh: c, dshCfg: &dshConfig{}, dshState: dshState{SessionID: "s1"}}
	res, err := b.waitForAnswer(context.Background(), "s1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Turns) != 1 || res.Turns[0].User != "Вопрос из браузера?" || res.Turns[0].Text != "ответ" {
		t.Fatalf("res.Turns = %+v", res.Turns)
	}
	if res.Text != "ответ" {
		t.Fatalf("res.Text = %q", res.Text)
	}
}

func TestWaitForAnswerKeepsCompletedTextOnQuestion(t *testing.T) {
	c, srv := newTestDSHClient(t, func(w http.ResponseWriter, r *http.Request) {
		var env struct {
			RPCID  string `json:"rpcId"`
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&env)
		switch env.Method {
		case "session/wait":
			writeRemoteResult(t, w, env.RPCID, map[string]any{
				"reason": "input-required",
				"state":  map[string]any{"cursor": 10, "pendingInput": []any{"question"}},
			})
		case "session/events":
			msg := map[string]any{"type": "event", "event": map[string]any{
				"type": "assistant/message", "seq": 5,
				"data": map[string]any{"message": map[string]any{"content": []any{
					map[string]any{"type": "text", "text": "готовый ответ"},
				}}},
			}}
			end := map[string]any{"type": "event", "event": map[string]any{"type": "turn/end", "seq": 6}}
			writeRemoteResult(t, w, env.RPCID, map[string]any{
				"records": []any{msg, end}, "cursor": 10, "hasMore": false,
			})
		default: // cancel и прочее
			writeRemoteResult(t, w, env.RPCID, map[string]any{})
		}
	}, nil)
	defer srv.Close()

	b := &bot{dsh: c, dshCfg: &dshConfig{}, dshState: dshState{SessionID: "s1"}}
	res, err := b.waitForAnswer(context.Background(), "s1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Text, "готовый ответ") {
		t.Fatalf("завершённый текст потерян при вопросе: text=%q", res.Text)
	}
	if res.Question == "" {
		t.Fatal("ожидали вопрос")
	}
}

func TestDSHConnectErrorHidesToken(t *testing.T) {
	c := newDSHClient("http://127.0.0.1:1", "unit")
	c.tokenFetcher = func(context.Context) (string, error) { return "secret-token-value", nil }
	_, err := c.ensureAuth(context.Background())
	if err == nil {
		t.Fatal("ожидали ошибку connect")
	}
	if strings.Contains(err.Error(), "secret-token-value") {
		t.Fatalf("токен утёк в ошибку: %v", err)
	}
}

func TestLoadConfigDSH(t *testing.T) {
	dir := t.TempDir()
	plain := filepath.Join(dir, "plain.json")
	if err := os.WriteFile(plain, []byte(`{"token":"t","redis":"127.0.0.1:6379"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(plain)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DSH != nil {
		t.Fatal("без раздела dsh чат должен быть выключен")
	}

	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte(`{"token":"t","redis":"r","dsh":{"base_url":"ftp://x"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(bad); err == nil {
		t.Fatal("ftp:// base_url должен быть отклонён")
	}

	good := filepath.Join(dir, "good.json")
	if err := os.WriteFile(good, []byte(`{"token":"t","redis":"r","dsh":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err = loadConfig(good)
	if err != nil || cfg.DSH == nil || cfg.DSH.DefaultModel != "coder-medium" || cfg.DSH.HeartbeatSec != 180 || cfg.DSH.BaseURL == "" {
		t.Fatalf("дефолты dsh: %+v err=%v", cfg, err)
	}
}

func TestMaxUploadFile(t *testing.T) {
	var uploadSeen atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/uploads":
			if r.URL.Query().Get("type") != "file" {
				t.Errorf("type = %q", r.URL.Query().Get("type"))
			}
			writeJSON(w, map[string]any{"url": "http://" + r.Host + "/upload", "token": "up1"})
		case "/upload":
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				t.Errorf("multipart: %v", err)
			}
			if _, _, err := r.FormFile("data"); err != nil {
				t.Errorf("нет поля data: %v", err)
			}
			uploadSeen.Store(true)
			writeJSON(w, map[string]any{"token": "tok2"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	orig := maxAPIBase
	maxAPIBase = srv.URL
	defer func() { maxAPIBase = orig }()

	c := newMaxClient("tok", "1", "")
	token, err := c.uploadFile(context.Background(), "a.txt", []byte("hello"))
	if err != nil {
		t.Fatalf("uploadFile: %v", err)
	}
	if !uploadSeen.Load() || token != "tok2" {
		t.Fatalf("uploadSeen=%v token=%q", uploadSeen.Load(), token)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

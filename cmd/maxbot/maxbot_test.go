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
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMaxClientSend(t *testing.T) {
	var gotAuth, gotTarget string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotTarget = r.URL.Query().Get("user_id")
		if r.URL.Query().Get("chat_id") != "" {
			t.Errorf("не должен быть chat_id при заданном user_id")
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"message":{"body":{"mid":"mid.123"}}}`))
	}))
	defer srv.Close()

	// Переопределяем базовый URL API на тестовый сервер.
	orig := maxAPIBase
	maxAPIBase = srv.URL
	defer func() { maxAPIBase = orig }()

	c := newMaxClient("tok", "42", "")
	if err := c.send(context.Background(), "привет"); err != nil {
		t.Fatalf("send: %v", err)
	}
	if gotAuth != "tok" {
		t.Fatalf("Authorization = %q, want токен", gotAuth)
	}
	if gotTarget != "42" {
		t.Fatalf("user_id = %q, want 42", gotTarget)
	}
}

func TestMaxClientSendError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer srv.Close()
	orig := maxAPIBase
	maxAPIBase = srv.URL
	defer func() { maxAPIBase = orig }()

	c := newMaxClient("bad", "", "7")
	if err := c.send(context.Background(), "x"); err == nil {
		t.Fatal("send: хотим ошибку при 401, получили nil")
	}
}

func TestMaxClientSendUsesChatID(t *testing.T) {
	var gotTarget string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotTarget = r.URL.Query().Get("chat_id")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	orig := maxAPIBase
	maxAPIBase = srv.URL
	defer func() { maxAPIBase = orig }()

	c := newMaxClient("tok", "", "99")
	if err := c.send(context.Background(), "x"); err != nil {
		t.Fatalf("send: %v", err)
	}
	if gotTarget != "99" {
		t.Fatalf("chat_id = %q, want 99", gotTarget)
	}
}

// TestSubscriberLifecycle — полный цикл подписки: регистрация первого (запись в
// конфиг + приветствие), отказ второму, отписка текущего (очистка конфига).
func TestSubscriberLifecycle(t *testing.T) {
	var responses []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// /messages — отправка; извлекаем текст для проверки приветствия/отказа.
		if strings.HasSuffix(r.URL.Path, "/messages") {
			body := make([]byte, 4096)
			n, _ := r.Body.Read(body)
			responses = append(responses, string(body[:n]))
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"message":{"body":{"mid":"mid.1"}}}`))
	}))
	defer srv.Close()
	orig := maxAPIBase
	maxAPIBase = srv.URL
	defer func() { maxAPIBase = orig }()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "maxbot.json")
	if err := os.WriteFile(cfgPath, []byte(`{"token":"tok","redis":"x"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	b := &bot{
		cfg:     &config{Token: "tok", Redis: "x"},
		cfgPath: cfgPath,
		client:  newMaxClient("tok", "", ""),
	}

	// 1) Первый подписчик (bot_started) — регистрируется, приветствие.
	firstUser, firstChat := "111222333", "444555666"
	b.handleSubscriberEvent(maxUpdate{UpdateType: "bot_started", ChatID: 444555666, User: struct {
		UserID    int64  `json:"user_id"`
		FirstName string `json:"first_name"`
		LastName  string `json:"last_name"`
	}{UserID: 111222333}})
	if u, c := b.client.recipient(); u != firstUser || c != firstChat {
		t.Fatalf("первый подписчик: recipient=%q/%q, want %q/%q", u, c, firstUser, firstChat)
	}
	if len(responses) != 1 || !strings.Contains(responses[0], "зарегистрированы") {
		t.Fatalf("первому должно быть отправлено приветствие, got %d: %v", len(responses), responses)
	}
	// Адресат записан в конфиг.
	bb, _ := os.ReadFile(cfgPath)
	if !strings.Contains(string(bb), firstUser) || !strings.Contains(string(bb), firstChat) {
		t.Fatalf("адресат должен быть записан в конфиг:\n%s", bb)
	}

	// 2) Второй подписчик — отказ, адресат не перезаписывается.
	b.handleSubscriberEvent(maxUpdate{UpdateType: "bot_started", ChatID: 777888999, User: struct {
		UserID    int64  `json:"user_id"`
		FirstName string `json:"first_name"`
		LastName  string `json:"last_name"`
	}{UserID: 123456789}})
	if u, c := b.client.recipient(); u != firstUser || c != firstChat {
		t.Fatalf("второй подписчик не должен перезаписать адресат: %q/%q", u, c)
	}
	if len(responses) != 2 || !strings.Contains(responses[1], "Регистрация невозможна") {
		t.Fatalf("второму должен быть отправлен отказ, got %d: %v", len(responses), responses)
	}

	// 3) Отписка текущего подписчика — адресат очищается в конфиге, режим ожидания.
	b.handleSubscriberEvent(maxUpdate{UpdateType: "bot_stopped", ChatID: 444555666, User: struct {
		UserID    int64  `json:"user_id"`
		FirstName string `json:"first_name"`
		LastName  string `json:"last_name"`
	}{UserID: 111222333}})
	if b.client.hasRecipient() {
		t.Fatalf("после отписки адресат должен быть очищен")
	}
	bb, _ = os.ReadFile(cfgPath)
	if strings.Contains(string(bb), firstUser) || strings.Contains(string(bb), firstChat) {
		t.Fatalf("после отписки адресат должен быть удалён из конфига:\n%s", bb)
	}
}

// manualDeadlineCtx — контекст, который переводится в DeadlineExceeded по
// команде теста (детерминированно, без гонки с таймером).
type manualDeadlineCtx struct {
	context.Context
	done chan struct{}
}

func (c *manualDeadlineCtx) Done() <-chan struct{} { return c.done }
func (c *manualDeadlineCtx) Err() error {
	select {
	case <-c.done:
		return context.DeadlineExceeded
	default:
		return nil
	}
}
func (c *manualDeadlineCtx) Deadline() (time.Time, bool) { return time.Time{}, false }

// TestPollSubscriberRoundDeadlineIsEmpty — сервер держит long-poll, клиентский
// ctx завершается дедлайном: это штатный пустой опрос, а не ошибка. Marker не
// теряется (next=0, вызывающий сохраняет прежний).
func TestPollSubscriberRoundDeadlineIsEmpty(t *testing.T) {
	gotMarker := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMarker <- r.URL.Query().Get("marker")
		<-r.Context().Done() // держим запрос до отмены клиентом
	}))
	defer srv.Close()
	orig := maxAPIBase
	maxAPIBase = srv.URL
	defer func() { maxAPIBase = orig }()

	ctx := &manualDeadlineCtx{Context: context.Background(), done: make(chan struct{})}
	c := newMaxClient("tok", "", "")
	type result struct {
		updates  []maxUpdate
		next     int64
		timedOut bool
		err      error
	}
	resCh := make(chan result, 1)
	go func() {
		updates, next, timedOut, err := c.pollSubscriberRound(ctx, 42, subscriberEvents)
		resCh <- result{updates, next, timedOut, err}
	}()

	select {
	case mk := <-gotMarker:
		if mk != "42" {
			t.Fatalf("marker в запросе = %q, want 42", mk)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("запрос не дошёл до хендлера")
	}
	close(ctx.done)

	select {
	case res := <-resCh:
		if res.err != nil {
			t.Fatalf("дедлайн не должен быть ошибкой: %v", res.err)
		}
		if !res.timedOut {
			t.Fatal("ожидали timedOut=true")
		}
		if len(res.updates) != 0 || res.next != 0 {
			t.Fatalf("пустой опрос: updates=%v next=%d", res.updates, res.next)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("опрос не завершился после дедлайна")
	}
}

// TestPollSubscriberRoundEvents — события и marker обрабатываются как раньше.
func TestPollSubscriberRoundEvents(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"updates":[{"update_type":"bot_started","chat_id":1,"user":{"user_id":2}}],"marker":43}`))
	}))
	defer srv.Close()
	orig := maxAPIBase
	maxAPIBase = srv.URL
	defer func() { maxAPIBase = orig }()

	c := newMaxClient("tok", "", "")
	updates, next, timedOut, err := c.pollSubscriberRound(context.Background(), 42, subscriberEvents)
	if err != nil || timedOut {
		t.Fatalf("events: err=%v timedOut=%v", err, timedOut)
	}
	if len(updates) != 1 || updates[0].UpdateType != "bot_started" || next != 43 {
		t.Fatalf("events=%v next=%d", updates, next)
	}
}

type timeoutNetErr struct{}

func (timeoutNetErr) Error() string   { return "i/o timeout" }
func (timeoutNetErr) Timeout() bool   { return true }
func (timeoutNetErr) Temporary() bool { return true }

func TestIsLongPollTimeout(t *testing.T) {
	if !isLongPollTimeout(context.DeadlineExceeded) {
		t.Fatal("context.DeadlineExceeded должен считаться long-poll таймаутом")
	}
	// http.Client оборачивает дедлайн контекста в *url.Error — errors.Is работает.
	if !isLongPollTimeout(&url.Error{Op: "Get", URL: "http://x", Err: context.DeadlineExceeded}) {
		t.Fatal("обёрнутый context deadline должен считаться long-poll таймаутом")
	}
	// Транспортный таймаут (dial/DNS/TLS) — реальная ошибка, не пустой опрос.
	if isLongPollTimeout(timeoutNetErr{}) {
		t.Fatal("транспортный timeout не должен считаться пустым long-poll")
	}
	var _ net.Error = timeoutNetErr{}
	if isLongPollTimeout(errors.New("http 500")) {
		t.Fatal("обычная ошибка не должна считаться таймаутом")
	}
}

func TestFetchUpdatesHTTPErrorNotTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	orig := maxAPIBase
	maxAPIBase = srv.URL
	defer func() { maxAPIBase = orig }()

	c := newMaxClient("tok", "", "")
	_, _, err := c.fetchUpdates(context.Background(), 1, subscriberEvents)
	if err == nil || isLongPollTimeout(err) {
		t.Fatalf("HTTP 500: err=%v, isTimeout=%v", err, isLongPollTimeout(err))
	}
}
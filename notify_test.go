// sunReceiver
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
	"net/http"
	"net/http/httptest"
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

	c := newMaxClient(&notifySection{Token: "tok", UserID: "42"})
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

	c := newMaxClient(&notifySection{Token: "bad", ChatID: "7"})
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

	c := newMaxClient(&notifySection{Token: "tok", ChatID: "99"})
	if err := c.send(context.Background(), "x"); err != nil {
		t.Fatalf("send: %v", err)
	}
	if gotTarget != "99" {
		t.Fatalf("chat_id = %q, want 99", gotTarget)
	}
}

func TestMapTrackStatusDown(t *testing.T) {
	var tr mapTrack
	now := time.Now()
	// Без данных — недоступен.
	if st := tr.status(now, 20*time.Second); st != "down" {
		t.Fatalf("пустой трекер: status=%q, want down", st)
	}
	// Успешный опрос — доступен.
	tr.trackOK("modbus", now, true, 230)
	if st := tr.status(now.Add(5*time.Second), 20*time.Second); st != "" {
		t.Fatalf("после OK: status=%q, want \"\"", st)
	}
	// Дольше окна без валидных данных — снова down.
	if st := tr.status(now.Add(30*time.Second), 20*time.Second); st != "down" {
		t.Fatalf("при устаревании: status=%q, want down", st)
	}
}

func TestMapTrackStatusStale(t *testing.T) {
	var tr mapTrack
	now := time.Now()
	tr.source = "api"
	// Timestamp из ответа сильно отстаёт и не меняется — stale.
	old := time.Now().Add(-time.Hour)
	tr.trackOK("api", now, true, 230)
	tr.trackAPITS(old.Add(10*time.Minute).Unix(), now)
	tr.trackAPITS(old.Add(10*time.Minute).Unix(), now.Add(time.Second)) // не меняется
	if st := tr.status(now.Add(30*time.Second), 20*time.Second); st != "stale" {
		t.Fatalf("устаревшее неизменное время: status=%q, want stale", st)
	}
	// Когда время снова начинает изменяться/свежее — не stale, и данные ещё валидны.
	tr.trackOK("api", now.Add(31*time.Second), true, 230)
	tr.trackAPITS(time.Now().Unix(), now.Add(31*time.Second))
	if st := tr.status(now.Add(35*time.Second), 20*time.Second); st != "" {
		t.Fatalf("свежее время: status=%q, want \"\"", st)
	}
}

func TestMapTrackStatusFreshAPI(t *testing.T) {
	var tr mapTrack
	now := time.Now()
	tr.trackOK("api", now, true, 230)
	tr.trackAPITS(now.Add(-time.Minute).Unix(), now)
	if st := tr.status(now.Add(5*time.Second), 20*time.Second); st == "stale" {
		t.Fatalf("свежий timestamp не должен давать stale: %q", st)
	}
}

func TestAlertDetectorHysteresis(t *testing.T) {
	d := &alertDetector{stable: 30 * time.Second}
	now := time.Now()

	// Первая итерация аварии — сообщение ещё не шлём (не прошло стабильности).
	if msg, ok := d.evaluate(now, true, func(alarm bool) (string, bool) {
		return "ALARM", true
	}); ok {
		t.Fatalf("до стабильности не должно быть сообщения, получили %q", msg)
	}
	// После 30 с стабильной аварии — шлём один раз.
	if msg, ok := d.evaluate(now.Add(35*time.Second), true, func(alarm bool) (string, bool) {
		return "ALARM", true
	}); !ok || msg != "ALARM" {
		t.Fatalf("после стабильности хотим ALARM, got ok=%v msg=%q", ok, msg)
	}
	// Повторные итерации аварии — не дублируем.
	if _, ok := d.evaluate(now.Add(40*time.Second), true, func(alarm bool) (string, bool) {
		return "ALARM", true
	}); ok {
		t.Fatalf("дедупликация: повторная авария не должна слать")
	}
	// Восстановление: переход в норму на now+80с, стабильность 30с — после неё
	// и не раньше шлём одно сообщение. Промежуточная итерация на +90с ещё не
	// прошла стабильность (с момента перехода прошло меньше 30с).
	if _, ok := d.evaluate(now.Add(80*time.Second), false, func(alarm bool) (string, bool) {
		return "RECOVER", true
	}); ok {
		t.Fatalf("до стабильности восстановления не должно слать")
	}
	if _, ok := d.evaluate(now.Add(90*time.Second), false, func(alarm bool) (string, bool) {
		return "RECOVER", true
	}); ok {
		t.Fatalf("через 10с после перехода восстановление ещё не стабильно")
	}
	if msg, ok := d.evaluate(now.Add(115*time.Second), false, func(alarm bool) (string, bool) {
		return "RECOVER", true
	}); !ok || msg != "RECOVER" {
		t.Fatalf("после стабильности хотим RECOVER, got ok=%v msg=%q", ok, msg)
	}
	// Повторное восстановление — не дублируем.
	if _, ok := d.evaluate(now.Add(125*time.Second), false, func(alarm bool) (string, bool) {
		return "RECOVER", true
	}); ok {
		t.Fatalf("дедупликация восстановления")
	}
}

func TestAlertDetectorReset(t *testing.T) {
	d := &alertDetector{stable: 30 * time.Second}
	now := time.Now()
	// отправленное восстановление должно аннулироваться только reset(),
	// после которого следующая авария анализируется заново.
	if _, ok := d.evaluate(now, true, func(alarm bool) (string, bool) { return "A1", true }); ok {
		t.Fatal("не должно слать до стабильности")
	}
	if msg, ok := d.evaluate(now.Add(31*time.Second), true, func(alarm bool) (string, bool) { return "A2", true }); !ok || msg != "A2" {
		t.Fatalf("хотим A2, got ok=%v msg=%q", ok, msg)
	}
	d.reset()
	// после reset новый считай аварии снова пройдёт стабильность.
	if msg, ok := d.evaluate(now.Add(32*time.Second), true, func(alarm bool) (string, bool) { return "A3", true }); ok || msg != "" {
		t.Fatalf("после reset: до стабильности не слать, got ok=%v msg=%q", ok, msg)
	}
	if msg, ok := d.evaluate(now.Add(65*time.Second), true, func(alarm bool) (string, bool) { return "A4", true }); !ok || msg != "A4" {
		t.Fatalf("после reset+стабильность хотим A4, got ok=%v msg=%q", ok, msg)
	}
}
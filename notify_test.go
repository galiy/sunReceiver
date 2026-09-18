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
	if msg, isDown, ok := d.evaluate(now, true, func(alarm bool) (string, bool) {
		return "ALARM", true
	}); ok {
		t.Fatalf("до стабильности не должно быть сообщения, got %q/%v", msg, isDown)
	}
	// После 30 с стабильной аварии — шлём один раз.
	if msg, isDown, ok := d.evaluate(now.Add(35*time.Second), true, func(alarm bool) (string, bool) {
		return "ALARM", true
	}); !ok || msg != "ALARM" || !isDown {
		t.Fatalf("после стабильности хотим ALARM/down, got ok=%v msg=%q isDown=%v", ok, msg, isDown)
	}
	// Подтверждаем доставку.
	d.confirmDispatched(true)
	// Пока авария держится — не дублируем.
	if _, _, ok := d.evaluate(now.Add(40*time.Second), true, func(alarm bool) (string, bool) {
		return "ALARM", true
	}); ok {
		t.Fatalf("дедупликация: повторная авария не должна слать")
	}
	// Восстановление: переход в норму, после стабильности — одно сообщение.
	if _, _, ok := d.evaluate(now.Add(80*time.Second), false, func(alarm bool) (string, bool) {
		return "RECOVER", true
	}); ok {
		t.Fatalf("до стабильности восстановления не должно слать")
	}
	if msg, isDown, ok := d.evaluate(now.Add(115*time.Second), false, func(alarm bool) (string, bool) {
		return "RECOVER", true
	}); !ok || msg != "RECOVER" || isDown {
		t.Fatalf("после стабильности хотим RECOVER/up, got ok=%v msg=%q isDown=%v", ok, msg, isDown)
	}
	d.confirmDispatched(false)
	// Повторное восстановление — не дублируем.
	if _, _, ok := d.evaluate(now.Add(125*time.Second), false, func(alarm bool) (string, bool) {
		return "RECOVER", true
	}); ok {
		t.Fatalf("дедупликация восстановления")
	}
}

func TestAlertDetectorReset(t *testing.T) {
	d := &alertDetector{stable: 30 * time.Second}
	now := time.Now()
	if _, _, ok := d.evaluate(now, true, func(alarm bool) (string, bool) { return "A1", true }); ok {
		t.Fatal("не должно слать до стабильности")
	}
	if msg, _, ok := d.evaluate(now.Add(31*time.Second), true, func(alarm bool) (string, bool) { return "A2", true }); !ok || msg != "A2" {
		t.Fatalf("хотим A2, got ok=%v msg=%q", ok, msg)
	}
	d.confirmDispatched(true)
	d.reset()
	// после reset новый считай аварии снова пройдёт стабильность.
	if msg, _, ok := d.evaluate(now.Add(32*time.Second), true, func(alarm bool) (string, bool) { return "A3", true }); ok || msg != "" {
		t.Fatalf("после reset: до стабильности не слать, got ok=%v msg=%q", ok, msg)
	}
	if msg, _, ok := d.evaluate(now.Add(65*time.Second), true, func(alarm bool) (string, bool) { return "A4", true }); !ok || msg != "A4" {
		t.Fatalf("после reset+стабильность хотим A4, got ok=%v msg=%q", ok, msg)
	}
}

// TestAlertDetectorRetryUntilDispatched — проверка «outbox»: сформированное
// пограничное сообщение повторяется каждый такт, пока не будет подтверждена
// успешная отправка (confirmDispatched), и не теряется при сбое сети/MAX.
func TestAlertDetectorRetryUntilDispatched(t *testing.T) {
	d := &alertDetector{stable: 30 * time.Second}
	now := time.Now()

	// Момент падения: фиксируем downSince.
	if _, _, ok := d.evaluate(now, true, func(alarm bool) (string, bool) {
		return "ALARM", true
	}); ok {
		t.Fatal("в момент падения не должно слать")
	}
	// Созрела авария, но отправка не удалась (confirm не вызывается).
	if msg, _, ok := d.evaluate(now.Add(35*time.Second), true, func(alarm bool) (string, bool) {
		return "ALARM", true
	}); !ok || msg != "ALARM" {
		t.Fatalf("авария должна сформироваться, got ok=%v msg=%q", ok, msg)
	}
	// Следующие такты — то же сообщение повторяется (пробуем дослать).
	for _, tm := range []time.Time{now.Add(40 * time.Second), now.Add(50 * time.Second), now.Add(60 * time.Second)} {
		if msg, isDown, ok := d.evaluate(tm, true, func(alarm bool) (string, bool) {
			return "ALARM", true
		}); !ok || msg != "ALARM" || !isDown {
			t.Fatalf("ретрай: хотим ALARM, got ok=%v msg=%q isDown=%v", ok, msg, isDown)
		}
	}
	// Успешно доставлено — после confirm повторная отправка не идёт.
	d.confirmDispatched(true)
	if _, _, ok := d.evaluate(now.Add(65*time.Second), true, func(alarm bool) (string, bool) {
		return "ALARM", true
	}); ok {
		t.Fatal("после подтверждения не должно дублировать")
	}
}

// TestAlertDetectorRecoverOrder — при сбое отправки авария приоритетно
// ретраится, и только после её доставки уходит восстановление (порядок важен).
func TestAlertDetectorRecoverOrder(t *testing.T) {
	d := &alertDetector{stable: 30 * time.Second}
	now := time.Now()
	// Момент падения: фиксируем downSince.
	if _, _, ok := d.evaluate(now, true, func(alarm bool) (string, bool) {
		return "ALARM", true
	}); ok {
		t.Fatal("в момент падения не должно слать")
	}
	// Авария сформирована, но отправка сорвалась.
	if msg, isDown, ok := d.evaluate(now.Add(35*time.Second), true, func(alarm bool) (string, bool) {
		return "ALARM", true
	}); !ok || msg != "ALARM" || !isDown {
		t.Fatalf("авария должна сформироваться, got ok=%v msg=%q", ok, msg)
	}
	// Состояние вернулось в норму, но авария не доставлена — ретраим её приоритетно.
	if msg, isDown, ok := d.evaluate(now.Add(80*time.Second), false, func(alarm bool) (string, bool) {
		return "RECOVER", true
	}); !ok || msg != "ALARM" || !isDown {
		t.Fatalf("приоритет: хотим ALARM, got ok=%v msg=%q isDown=%v", ok, msg, isDown)
	}
	// Доставляем аварию.
	d.confirmDispatched(true)
	// Восстановление уходит после стабильности нормы, не раньше.
	if msg, _, ok := d.evaluate(now.Add(85*time.Second), false, func(alarm bool) (string, bool) {
		return "RECOVER", true
	}); ok || msg != "" {
		t.Fatalf("восстановление должно дождаться стабильности, got ok=%v msg=%q", ok, msg)
	}
	if msg, isDown, ok := d.evaluate(now.Add(120*time.Second), false, func(alarm bool) (string, bool) {
		return "RECOVER", true
	}); !ok || msg != "RECOVER" || isDown {
		t.Fatalf("хотим RECOVER/up, got ok=%v msg=%q isDown=%v", ok, msg, isDown)
	}
	d.confirmDispatched(false)
	// После доставки восстановления авария закрыта — повторного сообщения нет.
	if _, _, ok := d.evaluate(now.Add(130*time.Second), false, func(alarm bool) (string, bool) {
		return "RECOVER", true
	}); ok {
		t.Fatal("после подтверждения восстановления не должно дублировать")
	}
}

func TestMeterVoltageFromSnapFresh(t *testing.T) {
	now := time.Now()
	snap := deviceSnapshot{
		Timestamp: now.Format(time.RFC3339),
		Values:    valuesContract{"meter_voltage": 237.9},
	}
	if v, ok := meterVoltageFromSnap(snap, now); !ok || v != 237.9 {
		t.Fatalf("свежий снимок: want 237.9/true, got %v/%v", v, ok)
	}
}

func TestMeterVoltageFromSnapStale(t *testing.T) {
	now := time.Now()
	// Снимок остался от последнего удачного опроса, но счётчик давно молчит
	// (timestamp старше окна свежести) — напряжение НЕ показываем.
	snap := deviceSnapshot{
		Timestamp: now.Add(-time.Hour).Format(time.RFC3339),
		Values:    valuesContract{"meter_voltage": 237.9},
	}
	if v, ok := meterVoltageFromSnap(snap, now); ok || v != 0 {
		t.Fatalf("устаревший снимок: want 0/false, got %v/%v (нельзя показывать напряжение как живое)", v, ok)
	}
}

func TestMeterVoltageFromSnapInvalidTimestamp(t *testing.T) {
	now := time.Now()
	snap := deviceSnapshot{
		Timestamp: "не-дата",
		Values:    valuesContract{"meter_voltage": 237.9},
	}
	if _, ok := meterVoltageFromSnap(snap, now); ok {
		t.Fatal("битый timestamp: хотим ok=false")
	}
}

func TestMeterVoltageFromSnapNoField(t *testing.T) {
	now := time.Now()
	snap := deviceSnapshot{Timestamp: now.Format(time.RFC3339), Values: valuesContract{}}
	if _, ok := meterVoltageFromSnap(snap, now); ok {
		t.Fatal("нет meter_voltage: хотим ok=false")
	}
}
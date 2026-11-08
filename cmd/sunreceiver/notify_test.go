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
	"testing"
	"time"
)

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
	tr.source = "malina26"
	// Timestamp из ответа сильно отстаёт и не меняется — stale.
	old := time.Now().Add(-time.Hour)
	tr.trackOK("malina26", now, true, 230)
	tr.trackAPITS(old.Add(10*time.Minute).Unix(), now)
	tr.trackAPITS(old.Add(10*time.Minute).Unix(), now.Add(time.Second)) // не меняется
	if st := tr.status(now.Add(30*time.Second), 20*time.Second); st != "stale" {
		t.Fatalf("устаревшее неизменное время: status=%q, want stale", st)
	}
	// Когда время снова начинает изменяться/свежее — не stale, и данные ещё валидны.
	tr.trackOK("malina26", now.Add(31*time.Second), true, 230)
	tr.trackAPITS(time.Now().Unix(), now.Add(31*time.Second))
	if st := tr.status(now.Add(35*time.Second), 20*time.Second); st != "" {
		t.Fatalf("свежее время: status=%q, want \"\"", st)
	}
}

func TestMapTrackStatusFreshAPI(t *testing.T) {
	var tr mapTrack
	now := time.Now()
	tr.trackOK("malina26", now, true, 230)
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
// успешная запись (confirmDispatched), и не теряется при сбое Redis.
func TestAlertDetectorRetryUntilDispatched(t *testing.T) {
	d := &alertDetector{stable: 30 * time.Second}
	now := time.Now()

	// Момент падения: фиксируем downSince.
	if _, _, ok := d.evaluate(now, true, func(alarm bool) (string, bool) {
		return "ALARM", true
	}); ok {
		t.Fatal("в момент падения не должно слать")
	}
	// Созрела авария, но запись не удалась (confirm не вызывается).
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
	// Успешно записано — после confirm повторная запись не идёт.
	d.confirmDispatched(true)
	if _, _, ok := d.evaluate(now.Add(65*time.Second), true, func(alarm bool) (string, bool) {
		return "ALARM", true
	}); ok {
		t.Fatal("после подтверждения не должно дублировать")
	}
}

// TestAlertDetectorRecoverOrder — при сбое записи авария приоритетно
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
	// Авария сформирована, но запись сорвалась.
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

func TestMeterInfoFromSnapFresh(t *testing.T) {
	now := time.Now()
	snap := deviceSnapshot{
		Timestamp: now.Format(time.RFC3339),
		Values:    valuesContract{"meter_voltage": 237.9},
	}
	val, _, state := meterInfoFromSnap(snap, now)
	if state != meterStateFresh || val != 237.9 {
		t.Fatalf("свежий снимок: want 237.9/fresh, got %v/%v", val, state)
	}
}

func TestMeterInfoFromSnapStale(t *testing.T) {
	now := time.Now()
	// Снимок остался от последнего удачного опроса, но счётчик давно молчит
	// (timestamp старше окна свежести) — счётчик недоступен.
	snap := deviceSnapshot{
		Timestamp: now.Add(-time.Hour).Format(time.RFC3339),
		Values:    valuesContract{"meter_voltage": 237.9},
	}
	val, ts, state := meterInfoFromSnap(snap, now)
	if state != meterStateStale || val != 0 {
		t.Fatalf("устаревший снимок: want 0/stale, got %v/%v (нельзя показывать напряжение как живое)", val, state)
	}
	if !ts.Before(now) {
		t.Fatalf("stale: хотим timestamp последнего снэпшота в прошлом, got %v", ts)
	}
}

func TestMeterInfoFromSnapInvalidTimestamp(t *testing.T) {
	now := time.Now()
	snap := deviceSnapshot{
		Timestamp: "не-дата",
		Values:    valuesContract{"meter_voltage": 237.9},
	}
	if _, _, state := meterInfoFromSnap(snap, now); state != meterStateNone {
		t.Fatalf("битый timestamp: want none, got %v", state)
	}
}

func TestMeterInfoFromSnapNoField(t *testing.T) {
	now := time.Now()
	snap := deviceSnapshot{Timestamp: now.Format(time.RFC3339), Values: valuesContract{}}
	if _, _, state := meterInfoFromSnap(snap, now); state != meterStateStale {
		t.Fatalf("нет meter_voltage: want stale, got %v", state)
	}
}

// Короткая авария (< stable), о которой ALARM получателю не отправлялся, не должна
// давать ложное «восстановление» после возврата в норму (регрессия).
func TestAlertDetectorShortAlarmNoFalseRecover(t *testing.T) {
	d := &alertDetector{stable: 30 * time.Second}
	now := time.Now()
	build := func(bool) (string, bool) { return "MSG", true }

	if _, _, ok := d.evaluate(now, true, build); ok {
		t.Fatal("ALARM до стабильности не должен слать")
	}
	if _, _, ok := d.evaluate(now.Add(10*time.Second), true, build); ok {
		t.Fatal("ALARM 10с < stable не должен слать")
	}
	// Норма после короткой аварии: даже по истечении stable RECOVER не шлём.
	if msg, _, ok := d.evaluate(now.Add(20*time.Second), false, build); ok {
		t.Fatalf("ложное восстановление без предшествующей аварии: msg=%q", msg)
	}
	if msg, _, ok := d.evaluate(now.Add(60*time.Second), false, build); ok {
		t.Fatalf("ложное восстановление после удержания: msg=%q", msg)
	}
}
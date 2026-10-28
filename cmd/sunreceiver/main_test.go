package main

import (
	"testing"
	"time"
)

func TestMapErrStateNoDataDebounce(t *testing.T) {
	s := newMapErrState()
	t0 := time.Now()
	if s.failure("map", "no_data", t0, mapNoDataLogAfter) {
		t.Fatal("сразу после сбоя писать нельзя")
	}
	if s.failure("map", "no_data", t0.Add(mapNoDataLogAfter-time.Second), mapNoDataLogAfter) {
		t.Fatal("до порога писать нельзя")
	}
	if !s.failure("map", "no_data", t0.Add(mapNoDataLogAfter), mapNoDataLogAfter) {
		t.Fatal("после порога нужно записать один раз")
	}
	// Без подтверждения (сбой PG) попытка должна повторяться.
	if !s.failure("map", "no_data", t0.Add(mapNoDataLogAfter+time.Second), mapNoDataLogAfter) {
		t.Fatal("после сбоя INSERT нужен повтор")
	}
	s.markLogged("map", "no_data")
	if s.failure("map", "no_data", t0.Add(10*time.Minute), mapNoDataLogAfter) {
		t.Fatal("повтор в том же эпизоде писать нельзя")
	}
	s.clear("map")
	if s.failure("map", "no_data", t0.Add(11*time.Minute), mapNoDataLogAfter) {
		t.Fatal("после восстановления отсчёт начинается заново")
	}
	if !s.failure("map", "no_data", t0.Add(16*time.Minute), mapNoDataLogAfter) {
		t.Fatal("новый эпизод после порога должен записаться")
	}
}

func TestMapErrStateRealCodeOnce(t *testing.T) {
	s := newMapErrState()
	now := time.Now()
	if !s.failure("mppt-1", "0x05", now, mapNoDataLogAfter) {
		t.Fatal("реальная ошибка пишется сразу")
	}
	s.markLogged("mppt-1", "0x05")
	if s.failure("mppt-1", "0x05", now.Add(time.Second), mapNoDataLogAfter) {
		t.Fatal("та же реальная ошибка повторно не пишется")
	}
	if !s.failure("mppt-1", "0x06", now.Add(2*time.Second), mapNoDataLogAfter) {
		t.Fatal("смена кода реальной ошибки должна записаться")
	}
	s.markLogged("mppt-1", "0x06")
	// no_data → реальный код → no_data: таймер отсутствия данных не сбрасывается,
	// а один и тот же реальный код не пишется повторно.
	if s.failure("mppt-1", "no_data", now.Add(3*time.Second), mapNoDataLogAfter) {
		t.Fatal("no_data пишется только после 5 минут")
	}
	if s.failure("mppt-1", "0x06", now.Add(4*time.Second), mapNoDataLogAfter) {
		t.Fatal("тот же реальный код после no_data не пишется повторно")
	}
	if !s.failure("mppt-1", "no_data", now.Add(3*time.Second+mapNoDataLogAfter), mapNoDataLogAfter) {
		t.Fatal("таймер no_data должен продолжаться между сменами кода")
	}
}

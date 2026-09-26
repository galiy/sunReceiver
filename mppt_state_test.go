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

import "testing"

func TestMPPTEmptyTolerance(t *testing.T) {
	s := newMPPTPollState()
	// 1–2 подряд идущих пустых ответа: шлём НЕ вызывать PruneMPPT, чтобы одиночная
	// пустота не снесла существующие #mppt-ключи current (старые ключи сохраняются).
	if s.shouldPrune(true) {
		t.Fatal("1-я пустота: shouldPrune=true — PruneMPPT был бы вызван и снёс бы #mppt-ключи")
	}
	if s.shouldPrune(true) {
		t.Fatal("2-я пустота подряд: shouldPrune=true — короткая пустота чистит ключи")
	}
	if !s.shouldPrune(true) {
		t.Fatal("3-я пустота подряд: shouldPrune=false — устойчивая пустота должна чистить")
	}
	// Непустой ответ сбрасывает счётчик и разрешает прунинг.
	if !s.shouldPrune(false) {
		t.Fatal("непустой ответ: shouldPrune=false — должны разрешить прунинг")
	}
	if s.shouldPrune(true) {
		t.Fatal("после непустого ответа счётчик не сброшен: должны терпеть 1-ю пустоту заново")
	}
	if s.empty != 1 {
		t.Fatalf("empty = %d, want 1 (счётчик сброшен непустым ответом и инкремент новой пустотой)", s.empty)
	}
}

func TestMPPTStableKeyEmptyUID(t *testing.T) {
	s := newMPPTPollState()
	// Первый опрос: слот 0 отдаёт UID — запоминаем и используем его как ключ.
	if uid := s.resolveUID(0, "1097"); uid != "1097" {
		t.Fatalf("resolveUID(0, \"1097\") = %q, want 1097", uid)
	}
	// Последующий опрос: ответ пришёл без UID — ключ должен остаться стабильным
	// (по последнему известному UID слота), а не «дрейфовать» по индексу.
	if uid := s.resolveUID(0, ""); uid != "1097" {
		t.Fatalf("resolveUID(0, \"\") = %q, want 1097 (стабильный ключ)", uid)
	}
	t0 := invTarget{IP: "10.0.0.1", Kind: kindMPPT, Slot: 0, UID: s.resolveUID(0, "")}
	if k := devKey(t0); k != "10.0.0.1#mppt-1097" {
		t.Fatalf("devKey = %q, want 10.0.0.1#mppt-1097", k)
	}
	// Слот, для которого UID ни разу не встречался — фолбэк на индексный ключ.
	if uid := s.resolveUID(2, ""); uid != "" {
		t.Fatalf("resolveUID(2, \"\") = %q, want \"\"", uid)
	}
	if k := devKey(invTarget{IP: "10.0.0.1", Kind: kindMPPT, Slot: 2, UID: ""}); k != "10.0.0.1#mppt2" {
		t.Fatalf("devKey без UID = %q, want 10.0.0.1#mppt2", k)
	}
}

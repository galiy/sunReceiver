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

func TestMPPTKeyByUIDOnly(t *testing.T) {
	// Ключ MPPT — только по серийнику (UID).
	if k := devKey(invTarget{IP: "10.0.0.1", Kind: kindMPPT, Slot: 0, UID: "1097"}); k != "mppt-1097" {
		t.Fatalf("devKey = %q, want mppt-1097", k)
	}
	// Пустой UID — пустой ключ (пулер такие контроллеры пропускает).
	if k := devKey(invTarget{IP: "10.0.0.1", Kind: kindMPPT, Slot: 2, UID: ""}); k != "" {
		t.Fatalf("devKey без UID = %q, want \"\"", k)
	}
}

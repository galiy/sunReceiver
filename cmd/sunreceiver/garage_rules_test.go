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

func testGarageCfg() *garageConfig {
	return &garageConfig{
		InverterStale:     30 * time.Second,
		PVOnVoltage:       100,
		Relay1Hold:        5 * time.Minute,
		Relay1OffHold:     5 * time.Minute,
		MeterStale:        5 * time.Second,
		PhaseFaultVoltage: 100,
		PhaseRecover:      30 * time.Second,
		DirectVoltageMin:  215,
		DirectVoltageMax:  242,
		DirectReturnMin:   215,
		DirectReturnMax:   242,
		DirectReturnHold:  5 * time.Minute,

		OvervoltagePrimaryHold:   5 * time.Minute,
		OvervoltageSecondaryVolt: 245,
		OvervoltageSecondaryHold: 30 * time.Second,
		OvervoltageInstant:       248,

		UndervoltagePrimaryHold:   60 * time.Second,
		UndervoltageSecondaryVolt: 210,
		UndervoltageSecondaryHold: 30 * time.Second,
		UndervoltageInstant:       205,

		StateGrace: 60 * time.Second,
	}
}

func relaysOn(ns ...int) [4]bool {
	var r [4]bool
	for _, n := range ns {
		r[n-1] = true
	}
	return r
}

func changeMap(ch []garageRelayChange) map[int]bool {
	m := map[int]bool{}
	for _, c := range ch {
		m[c.Relay] = c.On
	}
	return m
}

func freshMeter(a, b, c float64) garageMeterInput {
	return garageMeterInput{Fresh: true, Voltage: [3]float64{a, b, c}}
}

// Реле 1: PV > 100 В на свежем инверторе — инверторы подключены (реле off).
func TestGarageRelay1PV(t *testing.T) {
	d := newGarageDecider(testGarageCfg())
	now := time.Now()
	mem := relaysOn(1, 2, 4) // реле 1 off? нет: здесь on — отключены
	meter := freshMeter(230, 230, 230)
	inv := []garageInverterInput{{Name: "D1", Fresh: true, PV1: 120, PV2: 0}}

	target, ch := d.decide(now, mem, meter, inv)
	if target[0] {
		t.Fatalf("реле 1 должно быть off (инверторы подключены), target=%v", target)
	}
	if on, changed := changeMap(ch)[1]; !changed || on {
		t.Fatalf("ожидали переключение реле 1 на off, changes=%v", ch)
	}
}

// Реле 1: гистерезис 5 минут в обе стороны.
func TestGarageRelay1Hysteresis(t *testing.T) {
	d := newGarageDecider(testGarageCfg())
	now := time.Now()
	mem := relaysOn(1, 2, 4) // инверторы отключены
	meter := freshMeter(230, 230, 230)
	inv := []garageInverterInput{{Name: "D1", Fresh: true, PV1: 150}}

	// Переключение произошло только что — повторно не переключаем.
	d.noteSwitch(1, now)
	_, ch := d.decide(now.Add(time.Minute), mem, meter, inv)
	if _, changed := changeMap(ch)[1]; changed {
		t.Fatalf("реле 1 не должно переключаться внутри гистерезиса: %v", ch)
	}
	// Через 5 минут — переключаем.
	target, ch2 := d.decide(now.Add(5*time.Minute+time.Second), mem, meter, inv)
	if target[0] {
		t.Fatalf("через 5 минут реле 1 должно стать off, target=%v", target)
	}
	if on, changed := changeMap(ch2)[1]; !changed || on {
		t.Fatalf("ожидали переключение реле 1 на off: %v", ch2)
	}
}

// Без свежих данных DTS017M: реле 2 и 3 не трогаем, реле 4 — off, реле 1 работает.
func TestGarageNoMeter(t *testing.T) {
	d := newGarageDecider(testGarageCfg())
	now := time.Now()
	d.startedAt = now.Add(-2 * time.Minute) // прогрев давно прошёл
	mem := relaysOn(2, 3, 4)
	inv := []garageInverterInput{{Name: "D1", Fresh: true, PV1: 150}}
	target, ch := d.decide(now, mem, garageMeterInput{Fresh: false}, inv)
	if !target[1] || !target[2] {
		t.Fatalf("реле 2/3 не должны меняться: %v", target)
	}
	if target[3] {
		t.Fatalf("реле 4 должно быть off (ручной режим)")
	}
	m := changeMap(ch)
	if on, changed := m[4]; !changed || on {
		t.Fatalf("ожидали переключение реле 4 на off: %v", ch)
	}
}

// На старте модуля реле 4 не уводится в ручной режим, пока счётчик ни разу не
// был свежим: иначе рестарт сервиса на 1–2 секунды меняет режим подачи.
func TestGarageStartupMeterStaleGrace(t *testing.T) {
	now := time.Now()
	d := newGarageDecider(testGarageCfg())
	d.startedAt = now
	target, ch := d.decide(now, relaysOn(4), garageMeterInput{Fresh: false}, nil)
	if !target[3] {
		t.Fatalf("в первые state_grace_sec реле 4 должно оставаться on: %v", target)
	}
	if _, changed := changeMap(ch)[4]; changed {
		t.Fatalf("в первые state_grace_sec реле 4 не трогаем: %v", ch)
	}
	// Выдержка истекла, счётчик так и не появился — ручной режим.
	target, ch = d.decide(now.Add(61*time.Second), relaysOn(4), garageMeterInput{Fresh: false}, nil)
	if target[3] {
		t.Fatalf("после state_grace_sec без данных реле 4 должно быть off: %v", target)
	}
	if on, changed := changeMap(ch)[4]; !changed || on {
		t.Fatalf("ожидали переключение реле 4 на off: %v", ch)
	}

	// Если свежие данные уже были, пропадание счётчика уводит реле 4 в ручной
	// режим сразу — стартовая пауза не применяется.
	d2 := newGarageDecider(testGarageCfg())
	target, _ = d2.decide(now, relaysOn(4), freshMeter(230, 230, 230), nil)
	if !target[3] {
		t.Fatalf("при свежем счётчике реле 4 on: %v", target)
	}
	target, _ = d2.decide(now.Add(time.Second), relaysOn(4), garageMeterInput{Fresh: false}, nil)
	if target[3] {
		t.Fatalf("после свежих данных пропадание счётчика — сразу ручной режим: %v", target)
	}
}

// Авария фаз: любая фаза < 100 В — 2 on, 3 off, 4 on немедленно.
func TestGaragePhaseFault(t *testing.T) {
	d := newGarageDecider(testGarageCfg())
	now := time.Now()
	mem := relaysOn(4)
	target, _ := d.decide(now, mem, freshMeter(230, 90, 230), nil)
	if !target[1] || target[2] || !target[3] {
		t.Fatalf("авария фаз: ожидали 2 on, 3 off, 4 on, got %v", target)
	}
	if !d.faultActive {
		t.Fatal("faultActive должен быть установлен")
	}
}

// Восстановление после аварии фаз — не ранее 30 с нормального напряжения.
func TestGaragePhaseRecover(t *testing.T) {
	d := newGarageDecider(testGarageCfg())
	now := time.Now()
	mem := relaysOn(2, 4)
	d.decide(now, mem, freshMeter(90, 230, 230), nil) // авария
	inFault := relaysOn(2, 4)

	// Через 10 с нормального напряжения — ещё держим аварию.
	target, _ := d.decide(now.Add(10*time.Second), inFault, freshMeter(230, 230, 230), nil)
	if !target[1] || target[2] {
		t.Fatalf("в выдержке восстановления конфигурация аварии должна сохраняться: %v", target)
	}
	// Через 30+ с — обычные правила.
	target, _ = d.decide(now.Add(41*time.Second), relaysOn(2, 4), freshMeter(230, 230, 230), nil)
	if target[1] || !target[3] {
		t.Fatalf("после выдержки: реле 2 off, реле 4 on, got %v", target)
	}
}

// Прямая подача/стабилизатор и 10-минутный возврат.
func TestGarageRelay3Voltage(t *testing.T) {
	d := newGarageDecider(testGarageCfg())
	now := time.Now()
	// Уже в прямой подаче при 230 В — держим.
	target, _ := d.decide(now, relaysOn(3, 4), freshMeter(230, 229, 231), nil)
	if !target[2] {
		t.Fatalf("при 215–242 В ожидали прямую подачу: %v", target)
	}
	// Превышение 242 В, но менее 5 минут — остаёмся на прямой подаче.
	target, _ = d.decide(now.Add(time.Second), relaysOn(3, 4), freshMeter(230, 243, 225), nil)
	if !target[2] {
		t.Fatalf("при 243 В раньше 5 минут стабилизатор не нужен: %v", target)
	}
	// Более 5 минут превышения 242 В — стабилизатор.
	t2 := now.Add(time.Second + 5*time.Minute + time.Second)
	target, _ = d.decide(t2, relaysOn(3, 4), freshMeter(230, 243, 225), nil)
	if target[2] {
		t.Fatalf("после 5 минут превышения 242 В ожидали стабилизатор: %v", target)
	}
	// 218–230 В, но менее 5 минут — ещё стабилизатор.
	t3 := t2.Add(2 * time.Minute)
	target, _ = d.decide(t3, relaysOn(4), freshMeter(230, 229, 225), nil)
	if target[2] {
		t.Fatalf("возврат раньше 5 минут недопустим: %v", target)
	}
	// Более 5 минут удержания — прямая подача.
	target, _ = d.decide(t3.Add(5*time.Minute+time.Second), relaysOn(4), freshMeter(230, 229, 225), nil)
	if !target[2] {
		t.Fatalf("после 5 минут удержания ожидали прямую подачу: %v", target)
	}
}

// Мгновенный переход на стабилизатор при перенапряжении > 248 В.
func TestGarageInstantOvervoltage(t *testing.T) {
	d := newGarageDecider(testGarageCfg())
	now := time.Now()
	target, _ := d.decide(now, relaysOn(3, 4), freshMeter(230, 249, 225), nil)
	if target[2] {
		t.Fatalf(">248 В — стабилизатор мгновенно: %v", target)
	}
}

// Выдержки понижения: <215 В — 1 мин, <210 В — 30 с, <205 В — мгновенно.
func TestGarageUndervoltageTiers(t *testing.T) {
	now := time.Now()

	d := newGarageDecider(testGarageCfg())
	target, _ := d.decide(now, relaysOn(3, 4), freshMeter(214, 225, 225), nil)
	if !target[2] {
		t.Fatalf("214 В раньше 1 минуты — остаёмся на прямой подаче: %v", target)
	}
	target, _ = d.decide(now.Add(61*time.Second), relaysOn(3, 4), freshMeter(214, 225, 225), nil)
	if target[2] {
		t.Fatalf("214 В дольше 1 минуты — стабилизатор: %v", target)
	}

	// 205 В — ещё не «мгновенно», работает выдержка <210 (30 с).
	d = newGarageDecider(testGarageCfg())
	target, _ = d.decide(now, relaysOn(3, 4), freshMeter(205, 225, 225), nil)
	if !target[2] {
		t.Fatalf("205 В без выдержки — прямая подача: %v", target)
	}
	target, _ = d.decide(now.Add(31*time.Second), relaysOn(3, 4), freshMeter(205, 225, 225), nil)
	if target[2] {
		t.Fatalf("205 В дольше 30 с — стабилизатор: %v", target)
	}

	// Ниже 205 В — мгновенно.
	d = newGarageDecider(testGarageCfg())
	target, _ = d.decide(now, relaysOn(3, 4), freshMeter(204, 225, 225), nil)
	if target[2] {
		t.Fatalf("204 В — стабилизатор сразу: %v", target)
	}
}

// Зона 230–235 В отдельной выдержки не имеет: при 232 В прямая подача
// сохраняется сколь угодно долго (таймер 235 В не запускается).
func TestGarageNoSwitchAt232(t *testing.T) {
	d := newGarageDecider(testGarageCfg())
	now := time.Now()
	target, _ := d.decide(now, relaysOn(3, 4), freshMeter(225, 232, 225), nil)
	if !target[2] {
		t.Fatal("при 232 В должно оставаться на прямой подаче")
	}
	target, _ = d.decide(now.Add(60*time.Minute), relaysOn(3, 4), freshMeter(225, 232, 225), nil)
	if !target[2] {
		t.Fatal("232 В даже через час не должны переключать на стабилизатор")
	}
	target, _ = d.decide(now.Add(61*time.Minute), relaysOn(3, 4), freshMeter(225, 243, 225), nil)
	if !target[2] {
		t.Fatal("243 В раньше 5 минут — ещё прямая подача")
	}
	target, _ = d.decide(now.Add(67*time.Minute), relaysOn(3, 4), freshMeter(225, 243, 225), nil)
	if target[2] {
		t.Fatal("243 В дольше 5 минут — стабилизатор")
	}
}

// Выдержка 30 с при превышении второй ступени (245 В).
func TestGarageOvervoltageSecondaryHold(t *testing.T) {
	d := newGarageDecider(testGarageCfg())
	now := time.Now()
	target, _ := d.decide(now, relaysOn(3, 4), freshMeter(225, 246, 225), nil)
	if !target[2] {
		t.Fatal("при 246 В раньше 30 с прямая подача сохраняется")
	}
	target, _ = d.decide(now.Add(29*time.Second), relaysOn(3, 4), freshMeter(225, 246, 225), nil)
	if !target[2] {
		t.Fatal("при 246 В через 29 с прямая подача ещё сохраняется")
	}
	target, _ = d.decide(now.Add(31*time.Second), relaysOn(3, 4), freshMeter(225, 246, 225), nil)
	if target[2] {
		t.Fatal("при 246 В дольше 30 с — стабилизатор")
	}
}

// Направление реле 1: отключение инверторов — только после 5 минут без PV>100.
func TestGarageRelay1OnDirection(t *testing.T) {
	d := newGarageDecider(testGarageCfg())
	now := time.Now()
	inv := []garageInverterInput{{Name: "D1", Fresh: true, PV1: 10, PV2: 5}}
	target, ch := d.decide(now, relaysOn(4), freshMeter(230, 230, 230), inv)
	if target[0] {
		t.Fatalf("без PV раньше 5 минут инверторы не отключаем: %v", target)
	}
	// Короткий возврат PV сбрасывает отсчёт.
	d.decide(now.Add(4*time.Minute), relaysOn(4), freshMeter(230, 230, 230),
		[]garageInverterInput{{Name: "D1", Fresh: true, PV1: 200, PV2: 0}})
	target, _ = d.decide(now.Add(6*time.Minute), relaysOn(4), freshMeter(230, 230, 230), inv)
	if target[0] {
		t.Fatalf("после возврата PV отсчёт должен начаться заново: %v", target)
	}
	// Непрерывные 5 минут без PV — отключаем.
	target, ch = d.decide(now.Add(11*time.Minute+time.Second), relaysOn(4), freshMeter(230, 230, 230), inv)
	if !target[0] {
		t.Fatalf("после 5 минут без PV реле 1 должно включиться: %v", target)
	}
	if on, changed := changeMap(ch)[1]; !changed || !on {
		t.Fatalf("ожидали переключение реле 1 на on: %v", ch)
	}
}

// Возобновление данных счётчика во время аварии фаз: выдержка 30 с набирается заново.
func TestGaragePhaseRecoverAfterDataPause(t *testing.T) {
	d := newGarageDecider(testGarageCfg())
	now := time.Now()
	d.decide(now, relaysOn(4), freshMeter(90, 230, 230), nil) // авария
	// Данные пропали на 2 минуты — реле 2/3 не трогаются, флаг аварии сохраняется.
	target, _ := d.decide(now.Add(time.Minute), relaysOn(2, 4), garageMeterInput{Fresh: false}, nil)
	if !target[1] || target[2] {
		t.Fatalf("без данных реле 2/3 не трогаются: %v", target)
	}
	// Данные вернулись с нормой: 10 с мало — авария держится.
	target, _ = d.decide(now.Add(2*time.Minute), relaysOn(2, 4), freshMeter(230, 230, 230), nil)
	if !target[1] || target[2] {
		t.Fatalf("выдержка должна набираться заново: %v", target)
	}
	// Через 30+ с нормы — обычные правила.
	target, _ = d.decide(now.Add(2*time.Minute+31*time.Second), relaysOn(2, 4), freshMeter(230, 230, 230), nil)
	if target[1] || !target[3] {
		t.Fatalf("после выдержки реле 2 off, реле 4 on: %v", target)
	}
}

// Свежесть: strict — до stale, usable — до stale+grace (без дребезга реле).
func TestGarageMeterFreshness(t *testing.T) {
	stale, grace := 5*time.Second, 10*time.Second
	cases := []struct {
		age           time.Duration
		usable, stric bool
	}{
		{0, true, true},
		{5 * time.Second, true, true},
		{6 * time.Second, true, false},
		{15 * time.Second, true, false},
		{16 * time.Second, false, false},
		{-time.Second, false, false},
	}
	for _, c := range cases {
		usable, strict := meterFreshness(c.age, stale, grace)
		if usable != c.usable || strict != c.stric {
			t.Fatalf("age=%v: usable=%v strict=%v, want %v/%v", c.age, usable, strict, c.usable, c.stric)
		}
	}
}

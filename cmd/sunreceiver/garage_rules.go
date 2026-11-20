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
	"time"
)

// Логика принятия решений модуля «Управление гаражом». Чистая часть, отделённая
// от сети/хранилища: состояние реле (mem), входные данные мониторинга и часы.
//
// Назначение реле (relay4, каналы 1..4; on = 1):
//   - реле 1: on — инверторы Deye Right/Left отключены от сети; off — подключены;
//   - реле 2: on — подача напряжения в гараж полностью отключена;
//   - реле 4: on — режим задаётся реле 3, off — ручное управление;
//   - реле 3: on — прямая подача, off — через стабилизатор.
//
// Правила и гистерезисы — из ТЗ docs/modules/garagemgr.md.

// garageInverterInput — один инвертор группы «Гараж» на текущем такте.
type garageInverterInput struct {
	Name  string
	Fresh bool    // данные не старше garage.inverter_stale_sec
	PV1   float64 // В
	PV2   float64 // В
}

// garageMeterInput — DTS017M на текущем такте.
type garageMeterInput struct {
	// Fresh — данные пригодны для управления (не старше meter_stale + grace):
	// кратковременные пропуски не дёргают реле.
	Fresh bool
	// FreshStrict — данные не старше garage.meter_stale_sec (состояние 3).
	FreshStrict bool
	Voltage     [3]float64
}

// meterFreshness разделяет «пригодно для управления» и «строго актуально»:
// strict — возраст не больше stale; usable — не больше stale+grace. Так
// кратковременные задержки Redis/счётчика не вызывают дребезг реле 4.
func meterFreshness(age, stale, grace time.Duration) (usable, strict bool) {
	if age < 0 {
		return false, false
	}
	strict = age <= stale
	usable = age <= stale+grace
	return usable, strict
}

// garageRelayChange — требуемое переключение канала.
type garageRelayChange struct {
	Relay  int // 1..4
	On     bool
	Reason string // pv / phase_fault / phase_recover / voltage / manual / meter_ok / meter_stale
}

// needStabilizer решает, пора ли уйти с прямой подачи на стабилизатор.
// Переход мгновенный при maxV > overvoltage_instant_voltage или
// minV < undervoltage_instant_voltage; иначе — по минимальной выполненной
// выдержке: >235 В — 5 мин, >240 В — 30 с; <215 В — 1 мин, <210 В — 30 с.
// Ниже порога мгновенного перехода (<205 В) — сразу. Таймеры сбрасываются, когда условие перестало
// выполняться. Вызывается только в состоянии прямой подачи.
func (d *garageDecider) needStabilizer(now time.Time, minV, maxV float64) bool {
	instant := maxV > d.cfg.OvervoltageInstant || minV < d.cfg.UndervoltageInstant
	if instant {
		d.resetVoltageTiers()
		return true
	}
	type tier struct {
		over  bool
		value float64
		hold  time.Duration
		since *time.Time
	}
	tiers := []tier{
		{true, d.cfg.DirectVoltageMax, d.cfg.Overvoltage235Hold, &d.ovSince[0]},
		{true, garageOvervoltage240Voltage, d.cfg.Overvoltage240Hold, &d.ovSince[1]},
		{false, d.cfg.DirectVoltageMin, d.cfg.Undervoltage215Hold, &d.uvSince[0]},
		{false, garageUndervoltage210Voltage, d.cfg.Undervoltage210Hold, &d.uvSince[1]},
	}
	fire := false
	for _, t := range tiers {
		active := maxV > t.value
		if !t.over {
			active = minV < t.value
		}
		if !active {
			*t.since = time.Time{}
			continue
		}
		if t.since.IsZero() {
			*t.since = now
		}
		if now.Sub(*t.since) >= t.hold {
			fire = true
		}
	}
	if fire {
		d.resetVoltageTiers()
	}
	return fire
}

// resetVoltageTiers сбрасывает все таймеры выдержек перехода на стабилизатор.
func (d *garageDecider) resetVoltageTiers() {
	for i := range d.ovSince {
		d.ovSince[i] = time.Time{}
	}
	for i := range d.uvSince {
		d.uvSince[i] = time.Time{}
	}
}

// garageDecider хранит таймеры гистерезисов между тактами. Не потокобезопасен:
// используется только горутиной модуля.
type garageDecider struct {
	cfg *garageConfig

	// pvOffSince — начало непрерывного отсутствия PV>100 на свежих
	// инверторах: отключение инверторов — только после relay1_off_hold.
	pvOffSince time.Time

	// relay1BlockUntil — до какого момента запрещены переключения реле 1
	// (5 минут после фактического переключения). Таймер в памяти: при рестарте
	// сервиса сбрасывается (решение оператора).
	relay1BlockUntil time.Time

	// faultActive — авария фаз активна (в т.ч. идёт выдержка восстановления).
	faultActive bool
	// recoverSince — начало непрерывного «> phase_fault_voltage на всех фазах».
	recoverSince time.Time
	// directReturnSince — начало удержания напряжения в direct_return_* при
	// работе от стабилизатора.
	directReturnSince time.Time

	// Таймеры выдержек перехода на стабилизатор по превышению/понижению
	// (пороги 235/240 и 215/210 В). Индексы — по возрастанию порога.
	ovSince [2]time.Time
	uvSince [2]time.Time
}

func newGarageDecider(cfg *garageConfig) *garageDecider {
	return &garageDecider{cfg: cfg}
}

// noteSwitch фиксирует фактическое переключение реле (после успешной установки
// или приведения к реальному состоянию) — запускает гистерезис реле 1.
func (d *garageDecider) noteSwitch(relay int, now time.Time) {
	if relay == 1 && d.cfg != nil {
		d.relay1BlockUntil = now.Add(d.cfg.Relay1Hold)
	}
}

// phaseBounds — минимум и максимум по трём фазам: пороги применяются к каждой
// фазе отдельно, поэтому «в рамках» = все фазы в рамках (min не ниже нижней
// границы и max не выше верхней).
func phaseBounds(v [3]float64) (low, high float64) {
	low, high = v[0], v[0]
	for i := 1; i < 3; i++ {
		if v[i] < low {
			low = v[i]
		}
		if v[i] > high {
			high = v[i]
		}
	}
	return low, high
}

// pvOn — есть ли активный инвертор с PV-напряжением выше порога.
func (d *garageDecider) pvOn(inv []garageInverterInput) bool {
	for _, it := range inv {
		if !it.Fresh {
			continue
		}
		if it.PV1 > d.cfg.PVOnVoltage || it.PV2 > d.cfg.PVOnVoltage {
			return true
		}
	}
	return false
}

// decide вычисляет целевое состояние всех 4 реле и список требуемых переключений
// относительно текущего запомненного состояния mem (индексы 0..3 = реле 1..4).
// Реле, которые менять не нужно, в changes не попадают.
func (d *garageDecider) decide(now time.Time, mem [4]bool, meter garageMeterInput, inv []garageInverterInput) (target [4]bool, changes []garageRelayChange) {
	target = mem

	add := func(relay int, on bool, reason string) {
		if target[relay-1] != on {
			target[relay-1] = on
			changes = append(changes, garageRelayChange{Relay: relay, On: on, Reason: reason})
		}
	}

	// --- Реле 1: инверторы «Гараж» по PV (работает и без DTS017M) ---
	// Отключение (on) — только если «нет свежего инвертора с PV>100»
	// держится непрерывно дольше relay1_off_hold. Появление PV>100 сразу
	// сбрасывает отсчёт. Таймер живёт в памяти процесса.
	pv := d.pvOn(inv)
	wantDisconnect := false
	if pv {
		d.pvOffSince = time.Time{}
	} else {
		if d.pvOffSince.IsZero() {
			d.pvOffSince = now
		}
		wantDisconnect = now.Sub(d.pvOffSince) > d.cfg.Relay1OffHold
	}
	if wantDisconnect != mem[0] {
		// Гистерезис 5 минут в обе стороны: не переключаем, пока блокировка.
		if d.relay1BlockUntil.IsZero() || !now.Before(d.relay1BlockUntil) {
			add(1, wantDisconnect, "pv")
		}
	}

	// --- Реле 2/3/4: только при свежих данных DTS017M ---
	if !meter.Fresh {
		// Без актуальных данных счётчика: реле 2 и 3 не трогаем, реле 4 —
		// ручной режим (off). Флаг аварии фаз сохраняем: возобновление данных с
		// нормальным напряжением не должно мгновенно снимать аварию — 30-секундная
		// выдержка должна набраться заново. Таймеры выдержек сбрасываются.
		d.recoverSince = time.Time{}
		d.directReturnSince = time.Time{}
		d.resetVoltageTiers()
		add(4, false, "meter_stale")
		return target, changes
	}

	minV, maxV := phaseBounds(meter.Voltage)
	fault := minV < d.cfg.PhaseFaultVoltage
	normal := minV > d.cfg.PhaseFaultVoltage

	if fault {
		d.faultActive = true
		d.recoverSince = time.Time{}
		d.directReturnSince = time.Time{}
		d.resetVoltageTiers()
		add(2, true, "phase_fault")
		add(3, false, "phase_fault")
		add(4, true, "phase_fault")
		return target, changes
	}

	if d.faultActive {
		// Выдержка восстановления: не ранее phase_recover_sec нормального
		// напряжения на всех фазах; всё это время — конфигурация аварии.
		if normal {
			if d.recoverSince.IsZero() {
				d.recoverSince = now
			}
			if now.Sub(d.recoverSince) >= d.cfg.PhaseRecover {
				d.faultActive = false
			}
		} else {
			d.recoverSince = time.Time{}
		}
		if d.faultActive {
			add(2, true, "phase_fault")
			add(3, false, "phase_fault")
			add(4, true, "phase_fault")
			return target, changes
		}
		// Выдержка вышла — применяем обычные правила ниже.
		add(2, false, "phase_recover")
	}

	// Обычный режим состояния 3: подача разрешена, управление взято.
	add(2, false, "phase_ok")
	add(4, true, "meter_ok")

	// Реле 3: прямая подача / стабилизатор.
	if mem[2] {
		// Сейчас прямая: держим, пока все фазы в direct_voltage_* (с выдержками
		// по уровню превышения/понижения — см. needStabilizer).
		if d.needStabilizer(now, minV, maxV) {
			add(3, false, "voltage")
			d.directReturnSince = time.Time{}
		}
	} else {
		// Сейчас стабилизатор: возврат на прямую — после удержания худшей фазы
		// в direct_return_* не менее direct_return_hold.
		inReturn := minV >= d.cfg.DirectReturnMin && maxV <= d.cfg.DirectReturnMax
		if !inReturn {
			d.directReturnSince = time.Time{}
		} else {
			if d.directReturnSince.IsZero() {
				d.directReturnSince = now
			}
			if now.Sub(d.directReturnSince) >= d.cfg.DirectReturnHold {
				add(3, true, "voltage")
				d.directReturnSince = time.Time{}
			}
		}
	}
	return target, changes
}

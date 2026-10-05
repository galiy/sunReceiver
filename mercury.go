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
	"fmt"
	"math"
	"sort"
	"time"
)

// Счётчик энергосбыта «Меркурий» — без интерфейса мониторинга, установлен
// последовательно с DDS238. Показания снимаются вручную и хранятся в разделе
// конфига `mercury`. Прогноз показаний на текущий момент = последняя ручная точка
// + прирост DDS238 (та же тарифная раскладка «день/ночь» × «импорт/экспорт») с
// момента этой точки до now. Подробнее — docs/meter_mercury.md.

// mercurySection — одно ручное снятие показаний из конфига (раздел mercury).
// Время — RFC3339 с часовым поясом; энергии — kWh.
type mercurySection struct {
	TakenAt     string  `json:"taken_at"`
	ImportDay   float64 `json:"import_day"`
	ImportNight float64 `json:"import_night"`
	ExportDay   float64 `json:"export_day"`
	ExportNight float64 `json:"export_night"`
}

// mercuryReading — разобранная точка ручного снятия (для расчёта прогноза).
type mercuryReading struct {
	TakenAt     time.Time
	ImportDay   float64
	ImportNight float64
	ExportDay   float64
	ExportNight float64
}

// mercuryReadings — активные ручные точки (заполняется loadConfig, отсортированы по
// времени возрастанию). Пустой срез — «Меркурий» не настроен.
var mercuryReadings []mercuryReading

// mercuryForecast — прогноз показаний «Меркурия» на текущий момент (kWh).
type mercuryForecast struct {
	TakenAt     string  `json:"taken_at"` // время опорной ручной точки
	ImportDay   float64 `json:"import_day"`
	ImportNight float64 `json:"import_night"`
	ExportDay   float64 `json:"export_day"`
	ExportNight float64 `json:"export_night"`
}

// loadMercuryConfig разбирает раздел mercury: проверяет taken_at (RFC3339) и
// сортирует точки по времени. Пустой раздел допустим.
func loadMercuryConfig(secs []mercurySection) error {
	if len(secs) == 0 {
		mercuryReadings = nil
		return nil
	}
	out := make([]mercuryReading, 0, len(secs))
	for i, s := range secs {
		ts, err := time.Parse(time.RFC3339, s.TakenAt)
		if err != nil {
			return fmt.Errorf("в разделе mercury элемент %d: неверный taken_at %q (нужен RFC3339, напр. 2026-10-05T23:00:00+03:00)", i, s.TakenAt)
		}
		out = append(out, mercuryReading{
			TakenAt:     ts,
			ImportDay:   s.ImportDay,
			ImportNight: s.ImportNight,
			ExportDay:   s.ExportDay,
			ExportNight: s.ExportNight,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TakenAt.Before(out[j].TakenAt) })
	mercuryReadings = out
	return nil
}

// startOfLocalDay — начало календарных суток (локальная зона) для t.
func startOfLocalDay(t time.Time) time.Time {
	y, m, d := t.In(time.Local).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.Local)
}

// subPtr возвращает *a − *b (0, если любой указатель nil — граница не захвачена).
func subPtr(a, b *float64) float64 {
	if a == nil || b == nil {
		return 0
	}
	return *a - *b
}

// ddsTariffPartialUpTo — тарифные величины DDS238 текущего дня, накопленные к
// моменту at (в пределах дня). Точен, когда at совпадает с тарифной границей
// (00:00/07:00/23:00); иначе используется ближайшая предшествующая граница
// (погрешность — прирост после неё, поэтому ручные точки лучше снимать на границе).
func ddsTariffPartialUpTo(at time.Time, b *meterBoundaryRow) (impDay, impNight, expDay, expNight float64) {
	if b == nil {
		return 0, 0, 0, 0
	}
	h := at.In(time.Local).Hour()
	// Ближайшая предшествующая тарифная граница дня:
	//  - до 07:00 — это 00:00, прироста с начала суток нет;
	//  - 07:00..22:59 — 07:00, ночь [00:00,07:00] уже накоплена, день ещё нет;
	//  - 23:00.. — 23:00, день [07:00,23:00] полон, прирост ночи [23:00,at] ≈ 0
	//    (приближение: прирост после границы не атрибуцируется).
	if h < meterDayStartH {
		return 0, 0, 0, 0
	}
	impNight = subPtr(b.Import0700, b.Import0000)
	expNight = subPtr(b.Export0700, b.Export0000)
	if h >= meterDayEndH {
		impDay = subPtr(b.Import2300, b.Import0700)
		expDay = subPtr(b.Export2300, b.Export0700)
	}
	return max0f(impDay), max0f(impNight), max0f(expDay), max0f(expNight)
}

// mercuryCacheTTL — срок жизни кеша статической части прогноза «Меркурия»
// (суммы финализированных дней и прироста дня T0). Данные меняются редко, 10 мин
// с запасом; при ошибке/смене дня кеш пересчитывается.
const mercuryCacheTTL = 10 * time.Minute

// mercuryStatic вычисляет статическую часть прогноза: сумму финализированных
// посуточных тарифов DDS238 за дни [date(T0), date(now)) и тарифный прирост дня T0
// к моменту T0. Результат кешируется (TTL mercuryCacheTTL): финализированные дни и границы
// меняются редко. ok=false при отсутствии PG/ошибке.
func (h *dashboardHandler) mercuryStatic(t0, now time.Time) (sum [4]float64, p0 [4]float64, ok bool) {
	dateT0 := startOfLocalDay(t0)
	dateNow := startOfLocalDay(now)
	h.mercuryMu.Lock()
	if h.mercuryCacheOK && h.mercuryCacheT0.Equal(t0) && h.mercuryCacheDay.Equal(dateNow) &&
		time.Since(h.mercuryCacheAt) < mercuryCacheTTL {
		sum, p0 = h.mercuryCacheSum, h.mercuryCacheP0
		h.mercuryMu.Unlock()
		return sum, p0, true
	}
	h.mercuryMu.Unlock()

	if h.pg == nil {
		return sum, p0, false
	}
	// Финализированные дни [date(T0), date(now)): их суммарные тарифные величины.
	days, err := h.pg.DailyTariffsRange(dateT0, dateNow)
	if err != nil {
		return sum, p0, false
	}
	for _, d := range days {
		sum[0] += d.ImportDay
		sum[1] += d.ImportNight
		sum[2] += d.ExportDay
		sum[3] += d.ExportNight
	}
	// Прирост дня T0 к T0 (из захваченных границ 00:00/07:00/23:00).
	b0, err := h.pg.MeterBoundaryValues(dateT0)
	if err != nil {
		return sum, p0, false
	}
	p0[0], p0[1], p0[2], p0[3] = ddsTariffPartialUpTo(t0, b0)

	h.mercuryMu.Lock()
	h.mercuryCacheAt = time.Now()
	h.mercuryCacheT0 = t0
	h.mercuryCacheDay = dateNow
	h.mercuryCacheSum = sum
	h.mercuryCacheP0 = p0
	h.mercuryCacheOK = true
	h.mercuryMu.Unlock()
	return sum, p0, true
}

// mercuryForecastNow строит прогноз показаний «Меркурия» на момент now:
//
//	forecast[field] = manual[last][field]
//	                + (∑ финализированных дней [date(T0), date(now))
//	                   + прирост сегодняшнего дня к now
//	                   − прирост дня T0 к T0)
//
// Если последняя ручная точка ещё в будущем (now ≤ T0) — прогноз равен ручным
// значениям (коррекция невозможна). ok=false, если точек нет или нет данных PG.
func (h *dashboardHandler) mercuryForecastNow(now time.Time, todayImpDay, todayImpNight, todayExpDay, todayExpNight float64) (mercuryForecast, bool) {
	if len(mercuryReadings) == 0 {
		return mercuryForecast{}, false
	}
	last := mercuryReadings[len(mercuryReadings)-1]
	f := mercuryForecast{
		TakenAt:     last.TakenAt.Format(time.RFC3339),
		ImportDay:   last.ImportDay,
		ImportNight: last.ImportNight,
		ExportDay:   last.ExportDay,
		ExportNight: last.ExportNight,
	}
	if !last.TakenAt.Before(now) {
		return f, true // точка в будущем — коррекции нет
	}
	sum, p0, ok := h.mercuryStatic(last.TakenAt, now)
	if !ok {
		return mercuryForecast{}, false
	}
	f.ImportDay = round2(last.ImportDay + max0f(sum[0]+todayImpDay-p0[0]))
	f.ImportNight = round2(last.ImportNight + max0f(sum[1]+todayImpNight-p0[1]))
	f.ExportDay = round2(last.ExportDay + max0f(sum[2]+todayExpDay-p0[2]))
	f.ExportNight = round2(last.ExportNight + max0f(sum[3]+todayExpNight-p0[3]))
	return f, true
}

// round2 округляет до 2 знаков (счётчики — 2 знака после запятой).
func round2(v float64) float64 { return math.Round(v*100) / 100 }

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
	"log"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"
)

// logCE308 — единая точка логирования модуля CE308.
func logCE308(f string, a ...any) {
	log.Printf("ce308: "+f, a...)
}

// ce308Section — раздел "ce308" sunReceiver.json: настройка электросчётчика
// Энергомера СЕ308 (опрос по BLE). Disabled — ОБЯЗАТЕЛЬНОЕ поле: false —
// счётчик опрашивается; true — опрос CE308 отключён.
type ce308Section struct {
	// Name — имя Bluetooth-устройства (BT-имя, напр. "CE308 #000000000"),
	// используется для отображения и как ключ идентификатора в Redis/PG.
	Name string `json:"name"`
	// MAC — BD_ADDR счётчика (адрес для BLE-подключения).
	MAC string `json:"mac"`
	// PIN — BLE-PIN (код доступа радиоинтерфейса, 6 цифр) для спаривания.
	PIN      string `json:"pin"`
	Disabled *bool  `json:"disabled"`
}

// ce308Config — проверенный конфиг CE308 (только активная ветка).
type ce308Config struct {
	Name string
	MAC  string
	PIN  string
}

// Теги мгновенных значений CE308 в valuesContract. Единица зашита в суффикс:
// *_voltage — V, *_current — A, *_active_power — W, *_reactive_power — var.
// Мощность счётчик отдаёт в кВт/квар, поэтому переводится в Вт/вар (×1000).
const (
	ce308L1Voltage = "ce308_l1_voltage"
	ce308L2Voltage = "ce308_l2_voltage"
	ce308L3Voltage = "ce308_l3_voltage"
	ce308L1Current = "ce308_l1_current"
	ce308L2Current = "ce308_l2_current"
	ce308L3Current = "ce308_l3_current"
	ce308L1ActiveP = "ce308_l1_active_power"
	ce308L2ActiveP = "ce308_l2_active_power"
	ce308L3ActiveP = "ce308_l3_active_power"
	ce308ActiveP   = "ce308_active_power" // сумма по фазам
	ce308L1ReactP  = "ce308_l1_reactive_power"
	ce308L2ReactP  = "ce308_l2_reactive_power"
	ce308L3ReactP  = "ce308_l3_reactive_power"
	ce308ReactP    = "ce308_reactive_power" // сумма по фазам
)

// ce308CurrentTags — все теги мгновенных значений, попадающих в values.
var ce308CurrentTags = []string{
	ce308L1Voltage, ce308L2Voltage, ce308L3Voltage,
	ce308L1Current, ce308L2Current, ce308L3Current,
	ce308L1ActiveP, ce308L2ActiveP, ce308L3ActiveP, ce308ActiveP,
	ce308L1ReactP, ce308L2ReactP, ce308L3ReactP, ce308ReactP,
}

// ce308Reads — расшифрованные показания за один опрос. Массивы по фазам (3),
// суммы мощности — в последнем элементе (индекс 3) ответа POWEP/POWEQ.
type ce308Reads struct {
	Volta     []float64 // фазные напряжения, В
	Curre     []float64 // фазные токи, А
	ActiveP   []float64 // активная мощность, Вт (ф1,ф2,ф3,Σ)
	ReactiveP []float64 // реактивная мощность, вар (ф1,ф2,ф3,Σ)
}

// buildCE308Reads последовательно читает команды мгновенных значений.
// Каждое чтение при таймауте ретраится (нестабильный BLE-канал), но постоянное
// соединение при этом НЕ разрывается: решение о реконнекте принимает пулер по
// числу подряд неудачных опросов.
func buildCE308Reads(m *ce308Meter) (ce308Reads, error) {
	var r ce308Reads
	v, err := readCE308WithRetries(m, "VOLTA()")
	if err != nil {
		return r, fmt.Errorf("VOLTA: %w", err)
	}
	r.Volta = ce308Floats(ce308Groups(v))
	c, err := readCE308WithRetries(m, "CURRE()")
	if err != nil {
		return r, fmt.Errorf("CURRE: %w", err)
	}
	r.Curre = ce308Floats(ce308Groups(c))
	p, err := readCE308WithRetries(m, "POWEP()")
	if err != nil {
		return r, fmt.Errorf("POWEP: %w", err)
	}
	r.ActiveP = toWatts(ce308Floats(ce308Groups(p)))
	q, err := readCE308WithRetries(m, "POWEQ()")
	if err != nil {
		return r, fmt.Errorf("POWEQ: %w", err)
	}
	r.ReactiveP = toWatts(ce308Floats(ce308Groups(q)))
	return r, nil
}

// readCE308WithRetries выполняет одно чтение, ретрая только таймаут ответа
// (транзиентный сбой радио, лечится повтором). Ошибка записи/транспорта (соединение,
// вероятно, оборвано) возвращается сразу — ретраить её бессмысленно, пулер решит
// про реконнект. Возвращает ошибку последней попытки, если все попытки не удались.
func readCE308WithRetries(m *ce308Meter, cmd string) (string, error) {
	var lastErr error
	for attempt := 0; attempt <= ce308ReadRetries; attempt++ {
		s, err := m.Read(cmd)
		if err == nil {
			return s, nil
		}
		lastErr = err
		if isCE308ReadTimeout(err) && attempt < ce308ReadRetries {
			logCE308("чтение %s: таймаут (попытка %d/%d) — повтор", cmd, attempt+1, ce308ReadRetries+1)
			continue
		}
		return "", err
	}
	return "", lastErr
}

// toWatts переводит кВт/квар → Вт/вар и округляет до 1 знака.
func toWatts(kw []float64) []float64 {
	out := make([]float64, 0, len(kw))
	for _, v := range kw {
		out = append(out, ce308Round1(v*1000))
	}
	return out
}

// ce308Round1 округляет до 1 знака после запятой.
func ce308Round1(v float64) float64 {
	return math.Round(v*10) / 10
}

// Допустимые диапазоны показаний CE308 (грубая проверка валидности после чтения
// по нестабильному BLE-каналу). Границы щедрые — отбрасывается только очевидный
// мусор (NaN/Inf, бессмысленно большие значения), но не реальная дельта сети.
const (
	ce308VoltageMax  = 500.0    // фазные напряжения, В
	ce308CurrentMax  = 300.0    // фазные токи, А
	ce308PowerAbsMax = 200000.0 // активная/реактивная мощность, Вт/вар (по фазам и Σ)
)

// ce308ReadsValid проверяет целостность принятых показаний: ожидаемое число
// элементов (3 фазы; у мощности — ещё Σ), все значения конечны (не NaN/Inf) и
// укладываются в грубые физические диапазоны. Нестабильный BLE-канал может дать
// обрыв/искажение кадра, поэтому кривой снимок отбрасывается, а не пишется в
// Redis/PG. Суммы мощности (Σ) проверяются только на диапазон, не на равенство
// Σ = ф1+ф2+ф3 (счётчик может давать незначительную погрешность).
func ce308ReadsValid(r ce308Reads) bool {
	if len(r.Volta) < 3 || len(r.Curre) < 3 || len(r.ActiveP) < 4 || len(r.ReactiveP) < 4 {
		return false
	}
	for _, v := range r.Volta[:3] {
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > ce308VoltageMax {
			return false
		}
	}
	for _, v := range r.Curre[:3] {
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > ce308CurrentMax {
			return false
		}
	}
	for _, v := range r.ActiveP {
		if math.IsNaN(v) || math.IsInf(v, 0) || math.Abs(v) > ce308PowerAbsMax {
			return false
		}
	}
	for _, v := range r.ReactiveP {
		if math.IsNaN(v) || math.IsInf(v, 0) || math.Abs(v) > ce308PowerAbsMax {
			return false
		}
	}
	return true
}

// valuesCE308 строит map значений из показаний (напряжения/токи/мощности в
// исходных единицах, округление до 1 знака). Используется обычная
// map[string]float64, а НЕ valuesContract: у последней MarshalJSON выводит только
// общие теги контракта (commonContractTags), в которые теги ce308_* не входят —
// иначе значения терялись бы при сериализации снимка.
//
// Σ мощности (ce308_active_power / ce308_reactive_power) — алгебраическая сумма
// трёх фаз по знаку, а НЕ значение Σ из ответа счётчика (POWEP/POWEQ). Фазы могут
// быть взаимоисключающими (одна отдаёт в сеть, другая потребляет), и знаковая
// сумма фаз — физически корректный итог; это же значение выводит таблица дашборда,
// поэтому графики и анимация считают по этому тегу, а не по полю счётчика.
func valuesCE308(r ce308Reads) map[string]float64 {
	out := map[string]float64{}
	// putPhases кладёт округлённые значения фаз и возвращает их знаковую сумму.
	putPhases := func(keys []string, vals []float64) float64 {
		sum := 0.0
		for i, key := range keys {
			if i < len(vals) {
				v := ce308Round1(vals[i])
				out[key] = v
				sum += v
			}
		}
		return ce308Round1(sum)
	}
	putPhases([]string{ce308L1Voltage, ce308L2Voltage, ce308L3Voltage}, r.Volta)
	putPhases([]string{ce308L1Current, ce308L2Current, ce308L3Current}, r.Curre)
	out[ce308ActiveP] = putPhases([]string{ce308L1ActiveP, ce308L2ActiveP, ce308L3ActiveP}, r.ActiveP)
	out[ce308ReactP] = putPhases([]string{ce308L1ReactP, ce308L2ReactP, ce308L3ReactP}, r.ReactiveP)
	return out
}

// ce308Snapshot — снимок CE308, хранимый в Redis (current/series) и PG.
// Values — обычная map (без фильтрации тегов), т.к. у valuesContract при
// сериализации остаются только общие теги.
type ce308Snapshot struct {
	Name      string             `json:"name"`
	IP        string             `json:"ip"`
	Timestamp string             `json:"timestamp"`
	Values    map[string]float64 `json:"values"`
}

// ce308EnergySnapshot — разовый снимок накопленной электроэнергии по разрезам
// Актив/Реактив × День/Ночь × Потребление/Отдача. Хранится отдельным ключом
// Redis, история по энергии НЕ ведётся. Единицы — как отдаёт счётчик: кВт·ч
// (активная) и квар·ч (реактивная). Timestamp — время актуальности снимка.
type ce308EnergySnapshot struct {
	Name      string `json:"name"`
	Timestamp string `json:"timestamp"`
	// Активная энергия: потребление (A+), отдача (A−).
	ActiveConsumptionDay   float64 `json:"active_consumption_day"`   // A+ T1 (день)
	ActiveConsumptionNight float64 `json:"active_consumption_night"` // A+ T2 (ночь)
	ActiveConsumptionTotal float64 `json:"active_consumption_total"` // A+ сумма
	ActiveDeliveryDay      float64 `json:"active_delivery_day"`      // A− T1 (день)
	ActiveDeliveryNight    float64 `json:"active_delivery_night"`    // A− T2 (ночь)
	ActiveDeliveryTotal    float64 `json:"active_delivery_total"`    // A− сумма
	// Реактивная энергия: потребление (R+), отдача (R−).
	ReactiveConsumptionDay   float64 `json:"reactive_consumption_day"`
	ReactiveConsumptionNight float64 `json:"reactive_consumption_night"`
	ReactiveConsumptionTotal float64 `json:"reactive_consumption_total"`
	ReactiveDeliveryDay      float64 `json:"reactive_delivery_day"`
	ReactiveDeliveryNight    float64 `json:"reactive_delivery_night"`
	ReactiveDeliveryTotal    float64 `json:"reactive_delivery_total"`
}

// readCE308Energy читает накопления энергии. Команды ENDzz() возвращают
// ENDzz(дата,сумма)(T1)(T2)(T3…); сумма = T1+T2. Виды энергии (zz):
// 01 — активная потребление A+, 02 — активная отдача A−, 03 — реактивная
// потребление R+, 04 — реактивная отдача R−. Тарифы: T1 = день, T2 = ночь.
func readCE308Energy(m *ce308Meter) (*ce308EnergySnapshot, error) {
	snap := &ce308EnergySnapshot{}
	type item struct {
		cmd               string
		day, night, total *float64
	}
	items := []item{
		{"END01()", &snap.ActiveConsumptionDay, &snap.ActiveConsumptionNight, &snap.ActiveConsumptionTotal},
		{"END02()", &snap.ActiveDeliveryDay, &snap.ActiveDeliveryNight, &snap.ActiveDeliveryTotal},
		{"END03()", &snap.ReactiveConsumptionDay, &snap.ReactiveConsumptionNight, &snap.ReactiveConsumptionTotal},
		{"END04()", &snap.ReactiveDeliveryDay, &snap.ReactiveDeliveryNight, &snap.ReactiveDeliveryTotal},
	}
	for _, it := range items {
		s, err := m.Read(it.cmd)
		if err != nil {
			return nil, err
		}
		day, night, total, err := parseCE308End(it.cmd, s)
		if err != nil {
			return nil, err
		}
		*it.day, *it.night, *it.total = day, night, total
	}
	return snap, nil
}

// parseCE308End разбирает ответ ENDzz(): ENDzz(дата,сумма)(T1)(T2)(T3…).
// Возвращает день (T1), ночь (T2) и сумму по ВСЕМ тарифным группам (T1+T2+T3…).
// Тарифы: T1 — день, T2 — ночь. Нечисловая тарифная группа (искажённый кадр на
// нестабильном BLE) — ошибка, а не молчаливый 0.
func parseCE308End(cmd, resp string) (day, night, total float64, err error) {
	g := ce308Groups(resp)
	if len(g) < 3 {
		return 0, 0, 0, fmt.Errorf("%s: неожиданный ответ %q", cmd, resp)
	}
	tariffs := make([]float64, 0, len(g)-1)
	for _, x := range g[1:] {
		v, perr := strconv.ParseFloat(strings.TrimSpace(x), 64)
		if perr != nil {
			return 0, 0, 0, fmt.Errorf("%s: нечисловой тариф %q в ответе %q", cmd, x, resp)
		}
		tariffs = append(tariffs, v)
	}
	for _, v := range tariffs {
		total += v
	}
	return tariffs[0], tariffs[1], total, nil
}

// triggerCE308 — межгорутинный канал запроса снимка энергии. Устанавливается
// пулером при старте; недоступен (nil) — запрос игнорируется.
var (
	ce308TrigMu   sync.Mutex
	ce308TrigChan chan struct{}
)

// triggerCE308EnergySnapshot возвращает true, если запрос на снимок энергии
// передан пулеру (неблокирующе). Пулер обработает его в ближайшем цикле.
func triggerCE308EnergySnapshot() bool {
	ce308TrigMu.Lock()
	ch := ce308TrigChan
	ce308TrigMu.Unlock()
	if ch == nil {
		return false
	}
	select {
	case ch <- struct{}{}:
		return true
	default:
		return false // один снимок уже в очереди
	}
}

// setCE308TriggerChan привязывает канал пулера (при старте) и снимает (при стопе).
func setCE308TriggerChan(ch chan struct{}) {
	ce308TrigMu.Lock()
	ce308TrigChan = ch
	ce308TrigMu.Unlock()
}

// ce308ConfigFromSection строит *ce308Config из раздела ce308. nil — если раздела
// нет или он отключён/неполон. Ошибка — только при некорректных обязательных полях.
func ce308ConfigFromSection(s *ce308Section) (*ce308Config, error) {
	if s == nil {
		return nil, nil
	}
	if s.Disabled != nil && *s.Disabled {
		logCE308("disabled=true — опрос CE308 отключён")
		return nil, nil
	}
	if s.MAC == "" {
		return nil, fmt.Errorf("в разделе ce308 не задано обязательное поле mac")
	}
	if s.PIN == "" {
		return nil, fmt.Errorf("в разделе ce308 не задано обязательное поле pin")
	}
	name := s.Name
	if name == "" {
		name = "CE308 " + s.MAC
	}
	return &ce308Config{Name: name, MAC: s.MAC, PIN: s.PIN}, nil
}

// describeCE308Config — строка-описание конфига для лога.
func describeCE308Config(c *ce308Config) string {
	if c == nil {
		return ""
	}
	return fmt.Sprintf("%s (MAC %s)", c.Name, c.MAC)
}

// ce308Timestamp возвращает время актуальности (момент снятия) снимка в RFC3339.
func ce308Timestamp(t time.Time) string {
	return t.Format(time.RFC3339)
}

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
	"strconv"
	"strings"
	"sync"
	"time"
)

func logCE308(f string, a ...any) {
	log.Printf("ce308: "+f, a...)
}

// ce308ReadRetries — число ПОВТОРОВ после первой неудачной попытки чтения при
// таймауте (итого до ce308ReadRetries+1 попыток на одну команду).
const ce308ReadRetries = 2

// ce308Section — раздел "ce308" sunReceiver.json: счётчик Энергомера СЕ308 (BLE).
// Работает единственным режимом — только итоговые показания (раз в 30 мин / по
// кнопке), без постоянного соединения и телеметрии.
type ce308Section struct {
	// Name — имя Bluetooth-устройства (для отображения).
	Name string `json:"name"`
	// MAC — BD_ADDR счётчика (адрес для BLE-подключения).
	MAC string `json:"mac"`
	// PIN — BLE-PIN (код доступа радиоинтерфейса, 6 цифр) для спаривания.
	PIN string `json:"pin"`
	// Disabled — ОБЯЗАТЕЛЬНОЕ поле: false — счётчик опрашивается; true — отключён.
	Disabled *bool `json:"disabled"`
}

// ce308Config — проверенный конфиг CE308 (только активная ветка).
type ce308Config struct {
	Name string
	MAC  string
	PIN  string
}

// ce308EnergySnapshot — разовый снимок накопленной электроэнергии по разрезам
// Актив/Реактив × День/Ночь × Потребление/Отдача. Хранится отдельным ключом
// Redis, история по энергии НЕ ведётся. Единицы — кВт·ч (активная) и квар·ч
// (реактивная). Timestamp — время актуальности снимка.
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

// readCE308WithRetries выполняет одно чтение, ретрая только таймаут ответа
// (транзиентный сбой радио, лечится повтором). Ошибка записи/транспорта
// возвращается сразу — ретраить её бессмысленно.
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

// readCE308Energy читает текущие накопления энергии. Используем EMDzz(0.0,FF) —
// суточный архив по индексу i=0 (текущая фиксация НА): ответ содержит ТОЛЬКО
// текущий блок, он малый и приходит быстро.
// Виды энергии (zz): 01 — активная потребление A+, 02 — активная отдача A−,
// 03 — реактивная потребление R+, 04 — реактивная отдача R−.
// Тарифы: T1 = день, T2 = ночь.
func readCE308Energy(m *ce308Meter) (*ce308EnergySnapshot, error) {
	snap := &ce308EnergySnapshot{}
	type item struct {
		cmd               string
		day, night, total *float64
	}
	items := []item{
		{"EMD01(0.0,FF)", &snap.ActiveConsumptionDay, &snap.ActiveConsumptionNight, &snap.ActiveConsumptionTotal},
		{"EMD02(0.0,FF)", &snap.ActiveDeliveryDay, &snap.ActiveDeliveryNight, &snap.ActiveDeliveryTotal},
		{"EMD03(0.0,FF)", &snap.ReactiveConsumptionDay, &snap.ReactiveConsumptionNight, &snap.ReactiveConsumptionTotal},
		{"EMD04(0.0,FF)", &snap.ReactiveDeliveryDay, &snap.ReactiveDeliveryNight, &snap.ReactiveDeliveryTotal},
	}
	for _, it := range items {
		s, err := readCE308WithRetries(m, it.cmd)
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
// Нечисловая тарифная группа — ошибка, а не молчаливый 0.
func parseCE308End(cmd, resp string) (day, night, total float64, err error) {
	// Берём только ПЕРВЫЙ блок (актуальный снимок): обрезаем ответ по началу
	// следующего ENDzz(.
	name := cmd
	if i := strings.IndexByte(name, '('); i >= 0 {
		name = name[:i]
	}
	prefix := name + "("
	if i := strings.Index(resp, prefix); i >= 0 {
		tail := resp[i+len(prefix):]
		if j := strings.Index(tail, prefix); j >= 0 {
			resp = resp[:i+len(prefix)+j]
		}
	}
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
// нет или он отключён. Ошибка — только при некорректных обязательных полях.
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
	return fmt.Sprintf("%s (MAC %s) — режим: только итоговые показания (раз в 30 мин / по кнопке)", c.Name, c.MAC)
}

// ce308Timestamp возвращает время актуальности (момент снятия) снимка в RFC3339.
func ce308Timestamp(t time.Time) string {
	return t.Format(time.RFC3339)
}

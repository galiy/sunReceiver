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
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"time"
)

// rangeCacheTTL — срок жизни кешированного набора снимков в loadRange. Страница
// графиков делает fetch /api/series с общими from/to за цикл (60 с), и данные
// Redis обновляются каждые ~10 с, поэтому 15 с — свежее окно кеша: повторные
// (в т.ч. параллельные вкладки) запросы за тот же период сходятся в один
// реальный read из Redis/PG.
const rangeCacheTTL = 15 * time.Second

// tariffCacheTTL — срок жизни кеша тарифных исходников (границ текущего дня и
// сумм финализированных дней месяца/года). Главная страница обновляется каждую
// секунду, но эти данные в PG меняются редко: границы сегодня — 3 раза в сутки,
// набор финализированных дней — раз в сутки. Поэтому 60-секундный кеш снимает
// 3 запроса к PG/сек (7200/мин) до ~1 раза в минуту без заметной задержки.
const tariffCacheTTL = 60 * time.Second

// ce308EnergyMinInterval — минимальный период между ручными снимками энергии CE308:
// кнопка «Обновить» на дашборде не чаще раза в 5 минут. Защита от частых нажатий:
// чтение END01..END04 занимает ~1.5-2 мин и на это время блокирует опрос мгновенных
// значений (единственный последовательный BLE-канал). Повторный запрос раньше срока
// отклоняется (429 + retry_after), без повторного снятия снимка.
const ce308EnergyMinInterval = 5 * time.Minute

// dashFlags — флаги видимости блоков на дашборде, вычисленные из sunReceiver.json.
// Nonzero-поля управляют рендерингом рамок/плашек и кнопки «Электроэнергия»:
//   - ShowMap — показывать блок «Данные МАП» (map.disabled != true);
//   - ShowMeter — показывать блок счётчика и тарифов, кнопку «Электроэнергия»
//     (счётчик опрашивается, meter.disabled != true);
//   - ShowBMS — показывать блок BMS-батареек (пулер ANT BMS запущен);
//   - ShowCE308 — показывать рамки «Электросчётчик CE308» (раздел ce308 настроен).
type dashFlags struct {
	ShowMap   bool
	ShowMeter bool
	ShowBMS   bool
	ShowRelay bool
	ShowCE308 bool
}

// dashboardHandler — веб-дашборд: отдаёт три HTML-страницы и JSON API.
//   - Главная страница (/) — текущие параметры: плашки, электросчётчик, сводная
//     таблица; обновляются каждую секунду из Redis.
//   - Страница графиков (/charts) — временные ряды инверторов, МАП и счётчика за
//     выбранный период (Redis полное разрешение за 2 суток + PG 5-минутные средние).
//   - Страница электроэнергии (/energy) — посуточные и помесячные тарифы счётчика
//     (потребление/отдача «День»/«Ночь») из daily_tariffs с независимыми диапазонами.
type dashboardHandler struct {
	store *redisStore
	pg    *pgStore
	flags dashFlags
	relay *relayController

	// placements — упорядоченный список размещений сетевых инверторов (Deye/Sofar)
	// из sunReceiver.json (см. placementOrder). Определяет число и порядок пар плашек
	// «Суммарная активная / PV» в рамке «Мощности инверторов»: одна пара на каждое
	// размещение. MPPT-контроллеры (КЭС) в этот список не входят.
	placements []string

	// placeByIP — размещение инвертора по IP из конфига (надёжный источник).
	// У устаревшего снимка поле placement может отсутствовать (записано старой
	// версией до его появления), поэтому группировку анимации строим по конфигу.
	placeByIP map[string]string

	// Кэш loadRange: 4 одинаковых запроса /api/series за цикл сойдутся в один
	// read из Redis/PG. Ключ — от (start, end).
	cacheMu sync.Mutex
	cache   map[string]cachedRange

	// Кэш тарифных исходников /api/current (TTL tariffCacheTTL): границы текущего
	// дня и суммы финализированных прошедших дней месяца/года. Главная страница
	// опрашивает /api/current каждую секунду, но эти данные в PG меняются редко
	// (границы — 3 раза/сутки, набор финализированных дней — раз/сутки), поэтому
	// кеш снижает фоновые запросы к PG с 3/сек до ~1/мин. Текущий день и живые
	// показания счётчика пересчитываются в каждом запросе отдельно.
	tariffMu sync.Mutex
	tariffAt time.Time
	tariffB  *meterBoundaryRow // границы текущего дня (00:00/07:00/23:00)
	tariffM  [4]float64        // месяц: [importDay, importNight, exportDay, exportNight]
	tariffY  [4]float64        // год:  [importDay, importNight, exportDay, exportNight]

	// Ограничение частоты ручного снимка энергии CE308 (кнопка «Обновить», POST
	// /api/ce308/energy): не чаще раза в ce308EnergyMinInterval. Хранится время
	// последнего принятого (не отклонённого) сигнала. Шифруется мьютексом, т.к.
	// /api/ce308/energy может вызываться конкурентно с разных вкладок.
	ce308EnergyMu sync.Mutex
	ce308EnergyAt time.Time
}

// cachedRange — кешированный результат loadRange.
type cachedRange struct {
	at    time.Time
	snaps []deviceSnapshot
}

// currentResponse отвечает на GET /api/current.
type currentResponse struct {
	GeneratedAt string  `json:"generated_at"`
	TotalPower  float64 `json:"total_power"`
	TotalPV     float64 `json:"total_pv"`
	// Суммарные показатели по размещениям инверторов (группа «Мощности инверторов»):
	// по одной паре плашек «активная + PV» на каждое размещение из sunReceiver.json
	// (поле placement). MPPT-контроллеры (КЭС) в эту сумму не входят.
	Placements []placementPower `json:"placements"`
	MapGridV   float64          `json:"map_grid_voltage"`
	MapGridP   float64          `json:"map_grid_power"`
	MapBatV    float64          `json:"map_battery_voltage"`
	MapBatP    float64          `json:"map_battery_power"`
	MapCons    float64          `json:"map_consumption"`
	// Расчётные тарифные величины счётчика за текущие календарные сутки (kWh):
	// потребление/отдача «День»/«Ночь», посчитанные из актуальных показаний
	// счётчика (Redis) и фиксированных граничных показаний (PG daily_tariffs).
	MeterImportDay   float64 `json:"meter_import_day"`
	MeterImportNight float64 `json:"meter_import_night"`
	MeterExportDay   float64 `json:"meter_export_day"`
	MeterExportNight float64 `json:"meter_export_night"`
	// То же за текущий месяц (MM.YYYY) и текущий год (YYYY): сумма финализированных
	// дней периода + незавершённый сегодняшний день.
	MeterImportDayMonth   float64          `json:"meter_import_day_month"`
	MeterImportNightMonth float64          `json:"meter_import_night_month"`
	MeterExportDayMonth   float64          `json:"meter_export_day_month"`
	MeterExportNightMonth float64          `json:"meter_export_night_month"`
	MeterImportDayYear    float64          `json:"meter_import_day_year"`
	MeterImportNightYear  float64          `json:"meter_import_night_year"`
	MeterExportDayYear    float64          `json:"meter_export_day_year"`
	MeterExportNightYear  float64          `json:"meter_export_night_year"`
	Devices               []deviceSnapshot `json:"devices"`
}

// seriesPoint — одна точка временного ряда: время + значение.
type seriesPoint struct {
	T string  `json:"t"`
	V float64 `json:"v"`
}

// animationResponse — данные страницы анимации (/api/animation): две схемы
// (Дом и Гараж) с устройствами и мощностями на связях. Значения передаются
// со знаком: клиент по знаку определяет направление потока и цвет огоньков
// (подробнее о семантике знаков — docs/universal-contract.md и schemes/*.puml).
type animationResponse struct {
	GeneratedAt string     `json:"generated_at"`
	House       animScheme `json:"house"`
	Garage      animScheme `json:"garage"`
}

// animScheme — одна схема (Дом или Гараж).
type animScheme struct {
	// Inverters — сетевые инверторы (Deye/Sofar) размещения, отсортированные по
	// порядку на дашборде. PV — мощность солнечных панелей (P_PV: у Deye
	// dc_total_power, у Sofar pv1_power+pv2_power), AC — активная мощность.
	Inverters []animInverter `json:"inverters"`
	// KES — MPPT-контроллеры (КЭС) — только в схеме Дома (все КЭС под батареей).
	KES []animInverter `json:"kes,omitempty"`
	// Значения МАП (общие для схемы Дома): мощность сети и батареи.
	MapGridPower    float64 `json:"map_grid_power"`
	MapBatteryPower float64 `json:"map_battery_power"`
	// Активная мощность электросчётчика (только в схеме Дома).
	MeterActivePower float64 `json:"meter_active_power"`
	// Накопленные показания счётчика за всё время (kWh): потребление из сети
	// (meter_import) и отдача в сеть (meter_export) — как на ЖК счётчика. Только
	// в схеме Дома.
	MeterImportTotal float64 `json:"meter_import_total"`
	MeterExportTotal float64 `json:"meter_export_total"`
	// HousePower — мощность Дома (формула-разница), только в схеме Дома.
	HousePower float64 `json:"house_power"`
	// GaragePower — мощность на отрезке «Сеть гаража — Гараж»:
	// ce308Power + Σac(инверторы гаража) — нагрузка гаража (внешняя сеть + инверторы).
	// Положительная — потребление гаража, отрицательная — отдача. Только в схеме Гаража.
	GaragePower float64 `json:"garage_power"`
	// Ce308Power — активная мощность электросчётчика CE308 (гараж): знак как у
	// счётчика (потребление +, отдача в сеть −). Только в схеме Гаража.
	Ce308Power float64 `json:"ce308_power"`
	// MapTemps — температуры МАП для панели над изображением МАП: Тор и Транзисторы
	// (map_temp_tor / map_temp_transistor). Только в схеме Дома.
	MapTemps []animTemp `json:"map_temps,omitempty"`
	// BatteryTemp — температура батареи по данным МАП (map_temp_battery), °C.
	// Показывается справа от спрайта батареи. Только в схеме Дома.
	BatteryTemp *float64 `json:"battery_temp,omitempty"`
}

// animTemp — одна температура на схеме анимации. Label — русская подпись («Тор»,
// «Транзисторы», «Корпус», «Батарея»): на панели над МАП подпись видна, а справа
// от инверторов/батареи она показывается только как подсказка (title) при наведении.
// Value — температура в °C.
type animTemp struct {
	Label string  `json:"label"`
	Value float64 `json:"value"`
}

// animInverter — одно устройство схемы (инвертор или КЭС).
type animInverter struct {
	Name  string  `json:"name"`
	Kind  string  `json:"kind"`  // "deye"|"sofar" (инвертор) или "kes" (КЭС) — выбор спрайта
	PV    float64 `json:"pv"`    // активная мощность PV (P_PV)
	AC    float64 `json:"ac"`    // активная мощность на выходе (ac_active_power)
	Stale bool    `json:"stale"` // снимок старше окна (устройство молчит, напр. ночью)
	// Temps — температуры инвертора (°C): Корпус и Транзисторы. Подписи не
	// отображаются, только подсказка при наведении (см. animTemp.Label).
	Temps []animTemp `json:"temps,omitempty"`
}

// deviceSeries — временной ряд ac_active_power одного инвертора.
type deviceSeries struct {
	Name   string        `json:"name"`
	IP     string        `json:"ip"`
	Color  string        `json:"color"`
	Points []seriesPoint `json:"points"`
}

// seriesResponse отвечает на GET /api/series.
type seriesResponse struct {
	GeneratedAt        string         `json:"generated_at"`
	From               string         `json:"from"`
	To                 string         `json:"to"`
	Series             []deviceSeries `json:"series"`
	Total              []seriesPoint  `json:"total,omitempty"`
	MapGridVoltage     []seriesPoint  `json:"map_grid_voltage,omitempty"`
	MapGridPower       []seriesPoint  `json:"map_grid_power,omitempty"`
	MapBatVoltage      []seriesPoint  `json:"map_battery_voltage,omitempty"`
	MapBatPower        []seriesPoint  `json:"map_battery_power,omitempty"`
	MapCons            []seriesPoint  `json:"map_consumption,omitempty"`
	HousePower         []seriesPoint  `json:"house_power,omitempty"`
	HouseInverterPower []seriesPoint  `json:"house_inverter_power,omitempty"`
	MeterVoltage       []seriesPoint  `json:"meter_voltage,omitempty"`
	MeterActivePower   []seriesPoint  `json:"meter_active_power,omitempty"`
	// Ряды электросчётчика CE308 (опрос по BLE, отдельный пулер/хранилище):
	// фазные напряжения и суммарная активная мощность для наложения на графики
	// напряжений и мощностей (аналог белых линий счётчика DDS238).
	CE308L1Voltage   []seriesPoint `json:"ce308_l1_voltage,omitempty"`
	CE308L2Voltage   []seriesPoint `json:"ce308_l2_voltage,omitempty"`
	CE308L3Voltage   []seriesPoint `json:"ce308_l3_voltage,omitempty"`
	CE308ActivePower []seriesPoint `json:"ce308_active_power,omitempty"`
	// Temps — временные ряды температур всех устройств, отдающих температурные
	// теги универсального контракта (инверторы Deye/Sofar и МАП): по одной линии
	// на каждый датчик («Имя — датчик»). Только Redis (в PG температуры не
	// усредняются, см. accumulatorSkipTags), поэтому за период старше окна
	// удержания Redis ряды пусты.
	Temps []deviceSeries `json:"temps,omitempty"`
}

// meterDailyResponse отвечает на GET /api/tariffs: посуточные тарифные величины
// счётчика (потребление/отдача «День»/«Ночь»), отсортированные по дню возрастанию.
type meterDailyResponse struct {
	GeneratedAt string         `json:"generated_at"`
	From        string         `json:"from"`
	To          string         `json:"to"`
	Days        []meterDayStat `json:"days"`
}

// meterDayStat — тарифные величины одного календарного дня (kWh).
type meterDayStat struct {
	Day         string  `json:"day"` // YYYY-MM-DD
	ImportDay   float64 `json:"import_day"`
	ImportNight float64 `json:"import_night"`
	ExportDay   float64 `json:"export_day"`
	ExportNight float64 `json:"export_night"`
}

// seriesPalette — цвета линий инверторов (по индексу после сортировки по имени).
var seriesPalette = []string{
	"#428bca", "#5cb85c", "#f0ad4e", "#d9534f",
	"#5bc0de", "#9463b8", "#7f8fa6", "#17a2b8",
	"#a6c9e2", "#9ec79b", "#f6c28b", "#c9a3a8",
}

// tempPalette — цвета линий графика температур (по индексу линии после
// группировки по устройству и сортировки датчиков). Больше seriesPalette:
// линий на графике много (каждый инвертор — 2 датчика, МАП — 3).
var tempPalette = []string{
	"#428bca", "#d9534f", "#5cb85c", "#f0ad4e",
	"#9463b8", "#17a2b8", "#e8590c", "#20c997",
	"#e64980", "#7048e8", "#f08c00", "#1098ad",
	"#6b7785", "#c2255c", "#2b8a3e", "#862e9c",
}

// snapFloat извлекает числовое значение из универсального контракта по ключу
// (обёртка над toFloat — единая реализация в accumulator.go).
func snapFloat(v valuesContract, key string) (float64, bool) {
	raw, ok := v[key]
	if !ok {
		return 0, false
	}
	return toFloat(raw)
}

// placementPower — суммарная мощность инверторов одного размещения («Мощности
// инверторов»): активная мощность (W) и суммарная мощность PV (W). MPPT-контроллеры
// (КЭС) и МАП в эти суммы не входят.
type placementPower struct {
	Name  string  `json:"name"`
	Power float64 `json:"power"`
	PV    float64 `json:"pv"`
}

// isMAPDevice возвращает true, если снимок принадлежит устройству МАП (kindMAP,
// батарея/сеть). Маркер — наличие тега battery_voltage, которого нет у инверторов
// (Deye/Sofar) и MPPT-контроллеров. Мощность МАП учитывается только на своих
// плашках и графиках, а не в сумме по инверторам.
func isMAPDevice(v valuesContract) bool {
	_, ok := v["battery_voltage"]
	return ok
}

// isMeterDevice возвращает true, если снимок принадлежит электросчётчику DDS238
// (маркер — наличие тега meter_voltage, которого нет у инверторов и МАП). Аналог
// JS-функции isMeterDevice; используется в apiCurrent для поиска актуальных
// показаний счётчика (Import/Export) при расчёте тарифов текущего дня.
func isMeterDevice(v valuesContract) bool {
	_, ok := v["meter_voltage"]
	return ok
}

// isMPPTKey возвращает true для ключа/IP устройства MPPT-контроллера (devKey вида
// host#mppt<slot>). Используется, чтобы MPPT-контроллеры сортировались и
// выводились после сетевых инверторов.
func isMPPTKey(ip string) bool {
	return strings.Contains(ip, "#mppt")
}

// placementTotals группирует свежие снимки сетевых инверторов (Deye/Sofar) по
// размещениям (field placement) и считает суммарные активную и PV-мощности.
// Устройства МАП, MPPT-контроллеры (КЭС) и электросчётчик исключаются — они не
// участвуют в рамке «Мощности инверторов». Возвращает список размещений В ПОРЯДКЕ,
// заданном конфигом (order из placementOrder) — каждое размещение присутствует в
// результате всегда (ноль, если ни один его инвертор не дал свежих данных, напр.
// ночью), плюс размещения из старых снимков, найденные в данных, но отсутствующие
// в конфиге (дописываются в конец, дубли исключаются). Плюс общая активная и
// PV-мощность (сумма по всем размещениям).
func placementTotals(devices []deviceSnapshot, order []string, staleCutoff time.Time) ([]placementPower, float64, float64) {
	inOrder := make(map[string]bool, len(order))
	placeOrder := make([]string, 0, len(order))
	for _, p := range order {
		placeOrder = append(placeOrder, p)
		inOrder[p] = true
	}
	// configured — размещения из конфига: они отображаются всегда (с норлём при
	// отсутствии свежих данных), тогда как «чужие» размещения из старых снимков —
	// только при наличии данных.
	configured := make(map[string]bool, len(order))
	for _, p := range order {
		configured[p] = true
	}
	placePower := map[string]*placementPower{}
	getPlace := func(name string) *placementPower {
		if name == "" {
			name = "Дом"
		}
		pp, ok := placePower[name]
		if !ok {
			pp = &placementPower{Name: name}
			placePower[name] = pp
			if !inOrder[name] {
				inOrder[name] = true
				placeOrder = append(placeOrder, name)
			}
		}
		return pp
	}
	var total, totalPV float64
	for _, d := range devices {
		// Мощности МАП (батарея/сеть), MPPT-контроллеров (КЭС) и электросчётчика в
		// сумме по инверторам не участвуют: они отображаются на своих плашках/графиках.
		if isMAPDevice(d.Values) || isMPPTKey(d.IP) || isMeterDevice(d.Values) {
			continue
		}
		// Молчащий (оффлайн) инвертор в текущую сумму не входит.
		if ts, err := time.Parse(time.RFC3339, d.Timestamp); err != nil || !ts.After(staleCutoff) {
			continue
		}
		var p, pv float64
		if v, ok := snapFloat(d.Values, "ac_active_power"); ok {
			p += v
		}
		if v, ok := snapFloat(d.Values, "pv1_power"); ok {
			pv += v
		}
		if v, ok := snapFloat(d.Values, "pv2_power"); ok {
			pv += v
		}
		pp := getPlace(d.Placement)
		pp.Power += p
		pp.PV += pv
		total += p
		totalPV += pv
	}
	placements := make([]placementPower, 0, len(placeOrder))
	for _, name := range placeOrder {
		pp, ok := placePower[name]
		if !ok {
			// Размещение из конфига без свежих данных (все инверторы оффлайн) — плашка
			// выводится с нулями («—»). «Чужие» размещения без данных пропускаем.
			if !configured[name] {
				continue
			}
			placements = append(placements, placementPower{Name: name})
			continue
		}
		placements = append(placements, placementPower{
			Name:  pp.Name,
			Power: math.Round(pp.Power*10) / 10,
			PV:    math.Round(pp.PV*10) / 10,
		})
	}
	return placements,
		math.Round(total*10) / 10,
		math.Round(totalPV*10) / 10
}

func (h *dashboardHandler) charts(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := webTemplates.ExecuteTemplate(w, "charts.html", map[string]any{"active": "charts", "flags": h.flags, "CacheBust": webCacheBust}); err != nil {
		log.Printf("dashboard: render /charts: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

func (h *dashboardHandler) energy(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := webTemplates.ExecuteTemplate(w, "energy.html", map[string]any{"active": "energy", "flags": h.flags, "CacheBust": webCacheBust}); err != nil {
		log.Printf("dashboard: render /energy: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

// apiBMS отдаёт актуальное состояние всех ANT BMS (HASH sunreceiver:bms,
// пулер bms_poller.go) для батареек на главной странице (обновление раз в минуту).
func (h *dashboardHandler) apiBMS(w http.ResponseWriter, r *http.Request) {
	m, err := h.store.BMSCurrent()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	devs := make([]bmsDevice, 0, len(m))
	for _, raw := range m {
		var d bmsDevice
		if err := json.Unmarshal([]byte(raw), &d); err != nil {
			continue
		}
		devs = append(devs, d)
	}
	sort.Slice(devs, func(i, j int) bool { return bmsKey(devs[i]) < bmsKey(devs[j]) })
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"generated_at": time.Now().Format(time.RFC3339),
		"bms":          devs,
	})
}

// apiBMSOne отдаёт актуальное состояние одной ANT BMS по ключу bmsKey
// (/api/bms/<name>) для страницы деталей (обновление раз в секунду). name в URL —
// ключ устройства (deviceName или "deviceName@Port", см. bmsKey); отображаемое имя
// берётся из поля deviceName самого JSON.
// /api/bms/<name>/series — временной ряд 5-минутных усреднённых точек
// (см. apiBMSSeries).
func (h *dashboardHandler) apiBMSOne(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/api/bms/")
	if name == "" {
		http.Error(w, "не указано имя BMS", http.StatusBadRequest)
		return
	}
	if strings.HasSuffix(name, "/series") {
		h.apiBMSSeries(w, r, strings.TrimSuffix(name, "/series"))
		return
	}
	raw, err := h.store.BMSOne(name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if raw == "" {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "BMS не найдена"})
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(raw))
}

// apiBMSSeries отдаёт 5-минутные усреднённые точки BMS (/api/bms/<name>/series)
// за период [from, to] (RFC3339; по умолч. — последние 24 часа). Часть периода
// вне окна удержания Redis (старше 2 календарных суток) — из PostgreSQL
// (sunreceiver.bms_averages, вся история), рецентная часть — из Redis-ряда;
// сшивка по recentCutoff (как loadRange для инверторов). Точки — bmsSeriesPoint:
// ts + усреднённые параметры (см. bms_accumulator.go).
func (h *dashboardHandler) apiBMSSeries(w http.ResponseWriter, r *http.Request, name string) {
	now := time.Now()
	to := now
	if s := r.URL.Query().Get("to"); s != "" {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			to = t
		}
	}
	from := to.Add(-24 * time.Hour)
	if s := r.URL.Query().Get("from"); s != "" {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			from = t
		}
	}
	// Кламп диапазона (как в apiSeries): to <= now, from >= нижний предел,
	// from >= to → 400.
	if to.After(now) {
		to = now
	}
	if minFrom := recentCutoff(now).AddDate(0, 0, -400); from.Before(minFrom) {
		from = minFrom
	}
	if !from.Before(to) {
		http.Error(w, "from >= to", http.StatusBadRequest)
		return
	}
	cutoff := recentCutoff(now)
	var pts []bmsSeriesPoint
	// Старая часть периода (до cutoff) — из PostgreSQL (вся история).
	// pgEnd = cutoff-1с: точка с ts == cutoff — начало 5-минутного промежутка,
	// который уже в окне Redis, и дубль от обоих источников исключается.
	if h.pg != nil && from.Before(cutoff) {
		pgEnd := cutoff.Add(-time.Second)
		if to.Before(pgEnd) {
			pgEnd = to
		}
		old, err := h.pg.BMSAverages(name, from, pgEnd)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		pts = append(pts, old...)
	}
	// Рецентная часть (в пределах окна удержания) — из Redis-ряда.
	redisStart := from
	if redisStart.Before(cutoff) {
		redisStart = cutoff
	}
	if to.After(redisStart) {
		recent, err := h.store.QueryBMSSeries(name, redisStart, to)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		pts = append(pts, recent...)
	}
	if pts == nil {
		pts = []bmsSeriesPoint{}
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"name":   name,
		"from":   from.Format(time.RFC3339),
		"to":     to.Format(time.RFC3339),
		"points": pts,
	})
}

// bmsDetail — страница деталей ANT BMS (/bms/<name>): все текущие параметры
// выбранной батареи, обновление раз в секунду. JS берёт имя из URL и
// опрашивает /api/bms/<name>.
func (h *dashboardHandler) bmsDetail(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := webTemplates.ExecuteTemplate(w, "bms.html", map[string]any{"active": "home", "flags": h.flags, "CacheBust": webCacheBust}); err != nil {
		log.Printf("dashboard: render /bms: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

func (h *dashboardHandler) index(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := webTemplates.ExecuteTemplate(w, "index.html", map[string]any{"active": "home", "flags": h.flags, "CacheBust": webCacheBust}); err != nil {
		log.Printf("dashboard: render /: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

// tariffLogMu/tariffLogAt — ограничение частоты диагностики неполных границ тарифа:
// /api/current опрашивается раз в секунду, поэтому при длительной нехватке границ
// простая лог-строка спамила бы журнал. Диагностика печатается не чаще раза в минуту.
var (
	tariffLogMu sync.Mutex
	tariffLogAt time.Time
)

func logTariffMissing(format string, args ...any) {
	tariffLogMu.Lock()
	defer tariffLogMu.Unlock()
	if !tariffLogAt.IsZero() && time.Since(tariffLogAt) < time.Minute {
		return
	}
	tariffLogAt = time.Now()
	log.Printf(format, args...)
}

// meterTariffToday вычисляет тарифные величины счётчика за текущие календарные
// сутки [00:00, now): потребление/отдачу «День» и «Ночь» (kWh). День = 07:00–23:00,
// ночь = 00:00–07:00 и 23:00–24:00. Используются актуальные показания (impNow/expNow,
// из Redis) и фиксированные границы дня (b, из pg.daily_tariffs). Границы, которые
// ещё не наступили или не захвачены, отсутствуют (nil) — расчёт строится только из
// доступных показаний; величины приводятся к ≥0 (сброс счётчика игнорируется).
// Отсутствие нужной в текущей ветке границы логируется (см. logTariffMissing),
// чтобы «день/ночь» не атрибуцировались молча от неполных данных.
func meterTariffToday(now time.Time, impNow, expNow float64, b *meterBoundaryRow) (impDay, impNight, expDay, expNight float64) {
	hour := now.In(time.Local).Hour()
	// Ночная зона [00:00, 07:00): весь прирост с начала суток — ночь.
	if hour < meterDayStartH {
		if b.Import0000 != nil {
			impNight = impNow - *b.Import0000
		} else {
			logTariffMissing("tariff: ночь с начала суток: нет показания на 00:00 (import) — ночной импорт не атрибуцирован")
		}
		if b.Export0000 != nil {
			expNight = expNow - *b.Export0000
		} else {
			logTariffMissing("tariff: ночь с начала суток: нет показания на 00:00 (export) — ночная отдача не атрибуцирована")
		}
		return 0, max0f(impNight), 0, max0f(expNight)
	}
	// Дневная зона [07:00, 23:00): ночь уже сформирована [00:00,07:00], день растёт от 07:00.
	if hour < meterDayEndH {
		if b.Import0000 != nil && b.Import0700 != nil {
			impNight = *b.Import0700 - *b.Import0000
		} else if b.Import0000 != nil || b.Import0700 != nil {
			logTariffMissing("tariff: день: ночь [00:00,07:00] не собрана — неполные границы import 00:00/07:00")
		}
		if b.Export0000 != nil && b.Export0700 != nil {
			expNight = *b.Export0700 - *b.Export0000
		} else if b.Export0000 != nil || b.Export0700 != nil {
			logTariffMissing("tariff: день: ночь [00:00,07:00] не собрана — неполные границы export 00:00/07:00")
		}
		if b.Import0700 != nil {
			impDay = impNow - *b.Import0700
		} else if b.Import0000 != nil {
			impDay = impNow - *b.Import0000
			logTariffMissing("tariff: день: нет показания на 07:00 (import) — дневной импорт считается от 00:00")
		} else {
			logTariffMissing("tariff: день: нет показаний import 07:00/00:00 — дневной импорт не атрибуцирован")
		}
		if b.Export0700 != nil {
			expDay = expNow - *b.Export0700
		} else if b.Export0000 != nil {
			expDay = expNow - *b.Export0000
			logTariffMissing("tariff: день: нет показания на 07:00 (export) — дневная отдача считается от 00:00")
		} else {
			logTariffMissing("tariff: день: нет показаний export 07:00/00:00 — дневная отдача не атрибуцирована")
		}
		return max0f(impDay), max0f(impNight), max0f(expDay), max0f(expNight)
	}
	// Ночная зона [23:00, 24:00): день полон [07:00,23:00], ночь = [00:00,07:00] + [23:00,now].
	if b.Import0700 != nil && b.Import2300 != nil {
		impDay = *b.Import2300 - *b.Import0700
	} else if b.Import0700 != nil || b.Import2300 != nil {
		logTariffMissing("tariff: ночь: день [07:00,23:00] не собран — неполные границы import 07:00/23:00")
	}
	if b.Export0700 != nil && b.Export2300 != nil {
		expDay = *b.Export2300 - *b.Export0700
	} else if b.Export0700 != nil || b.Export2300 != nil {
		logTariffMissing("tariff: ночь: день [07:00,23:00] не собран — неполные границы export 07:00/23:00")
	}
	if b.Import0000 != nil && b.Import0700 != nil {
		impNight = *b.Import0700 - *b.Import0000
	} else if b.Import0000 != nil || b.Import0700 != nil {
		logTariffMissing("tariff: ночь: ночь [00:00,07:00] не собрана — неполные границы import 00:00/07:00")
	}
	if b.Export0000 != nil && b.Export0700 != nil {
		expNight = *b.Export0700 - *b.Export0000
	} else if b.Export0000 != nil || b.Export0700 != nil {
		logTariffMissing("tariff: ночь: ночь [00:00,07:00] не собрана — неполные границы export 00:00/07:00")
	}
	// В ночной ветке [23:00,24:00) показатель на 23:00 обязателен для закрытия дня.
	if b.Import2300 == nil {
		logTariffMissing("tariff: ночь: нет показания на 23:00 (import) — ночной импорт [23:00,now] не атрибуцирован")
	}
	if b.Export2300 == nil {
		logTariffMissing("tariff: ночь: нет показания на 23:00 (export) — ночная отдача [23:00,now] не атрибуцирована")
	}
	if b.Import2300 != nil {
		impNight += impNow - *b.Import2300
	}
	if b.Export2300 != nil {
		expNight += expNow - *b.Export2300
	}
	return max0f(impDay), max0f(impNight), max0f(expDay), max0f(expNight)
}

// max0f возвращает v, если v ≥ 0, иначе 0 (отрицательные разности при сбросе счётчика).
func max0f(v float64) float64 {
	if v < 0 {
		return 0
	}
	return v
}

// addTariffs складывает тарифные величины посуточных финализированных дней периода
// (days) с тарифами текущего незавершённого дня (tDay/tNight/eDay/eNight). Возвращает
// итоговые значения периода (kWh): потребление/отдачу «День»/«Ночь».
func addTariffs(days []meterDayStat, tDay, tNight, eDay, eNight float64) (float64, float64, float64, float64) {
	var d, n, ed, en float64
	for _, st := range days {
		d += st.ImportDay
		n += st.ImportNight
		ed += st.ExportDay
		en += st.ExportNight
	}
	return d + tDay, n + tNight, ed + eDay, en + eNight
}

// loadTariffData возвращает кешированные исходники тарифов для /api/current:
// границы текущего дня (b), сумму финализированных прошедших дней текущего
// месяца (m: [importDay, importNight, exportDay, exportNight]) и года (y). Текущий
// день сюда НЕ входит (он считается отдельно на каждом запросе из живых
// показаний счётчика). Результат кешируется на tariffCacheTTL — данные в PG
// меняются редко, а /api/current опрашивается каждую секунду.
func (h *dashboardHandler) loadTariffData(now time.Time) (*meterBoundaryRow, [4]float64, [4]float64) {
	h.tariffMu.Lock()
	defer h.tariffMu.Unlock()
	if !h.tariffAt.IsZero() && now.Sub(h.tariffAt) < tariffCacheTTL {
		return h.tariffB, h.tariffM, h.tariffY
	}
	var b *meterBoundaryRow
	var m, y [4]float64
	if h.pg != nil {
		loc := time.Local
		start, _ := dayBounds(now, loc)
		if bb, err := h.pg.MeterBoundaryValues(start); err == nil {
			b = bb
		} else {
			log.Printf("dashboard: meter tariff today: %v", err)
		}
		if b != nil {
			yy, mo, _ := now.In(loc).Date()
			monthStart := time.Date(yy, mo, 1, 0, 0, 0, 0, loc)
			monthEnd := monthStart.AddDate(0, 1, 0)
			yearStart := time.Date(yy, 1, 1, 0, 0, 0, 0, loc)
			yearEnd := yearStart.AddDate(1, 0, 0)
			if days, err := h.pg.DailyTariffsRange(monthStart, monthEnd); err == nil {
				m[0], m[1], m[2], m[3] = addTariffs(days, 0, 0, 0, 0)
			} else {
				log.Printf("dashboard: meter tariff month: %v", err)
			}
			if days, err := h.pg.DailyTariffsRange(yearStart, yearEnd); err == nil {
				y[0], y[1], y[2], y[3] = addTariffs(days, 0, 0, 0, 0)
			} else {
				log.Printf("dashboard: meter tariff year: %v", err)
			}
		}
	}
	h.tariffAt = now
	h.tariffB = b
	h.tariffM = m
	h.tariffY = y
	return h.tariffB, h.tariffM, h.tariffY
}

func (h *dashboardHandler) apiCurrent(w http.ResponseWriter, r *http.Request) {
	devices, err := h.store.Current()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	sort.SliceStable(devices, func(i, j int) bool {
		// MPPT-контроллеры — всегда последними; остальные — в порядке конфигурации
		// (поле order снимка = позиция устройства в sunReceiver.json: инверторы в
		// порядке invertors, затем МАП). При равных order (напр. старые снимки без order)
		// — стабильно по имени.
		mi, mj := isMPPTKey(devices[i].IP), isMPPTKey(devices[j].IP)
		if mi != mj {
			return !mi
		}
		oi, oj := devices[i].Order, devices[j].Order
		if oi != oj {
			return oi < oj
		}
		return devices[i].Name < devices[j].Name
	})
	var total float64
	var totalPV float64
	var placements []placementPower
	// Напряжение/мощность сети и батареи МАП — из снимков устройства МАП (батарея/сеть).
	var gridV, gridP, batV, batP float64
	// Устройство считаем «живым», если снимок не старше 20 минут (то же окно
	// STALE_MS, что на дашборде): инвертор, выключенный ночью, в текущую сумму
	// не входит, а его устаревшее значение (напр. 9 Вт заката) не выставляется
	// как текущая мощность.
	staleCutoff := time.Now().Add(-20 * time.Minute)
	// Суммарные показатели по размещениям инверторов (группа «Мощности инверторов»):
	// одна пара плашек «активная + PV» на каждое размещение, имя/порядок — из
	// placementOrder (sunReceiver.json). MPPT-контроллеры (КЭС), счётчик и МАП в
	// суммы не входят (см. placementTotals).
	placements, total, totalPV = placementTotals(devices, h.placements, staleCutoff)
	for _, d := range devices {
		if !isMAPDevice(d.Values) {
			continue
		}
		if ts, err := time.Parse(time.RFC3339, d.Timestamp); err != nil || !ts.After(staleCutoff) {
			continue
		}
		if v, ok := snapFloat(d.Values, "grid_voltage"); ok {
			gridV = v
		}
		if v, ok := snapFloat(d.Values, "grid_power"); ok {
			gridP = v
		}
		if v, ok := snapFloat(d.Values, "battery_voltage"); ok {
			batV = v
		}
		if v, ok := snapFloat(d.Values, "battery_power"); ok {
			batP = v
		}
	}
	gridV = math.Round(gridV*10) / 10
	gridP = math.Round(gridP*10) / 10
	batV = math.Round(batV*10) / 10
	batP = math.Round(batP*10) / 10

	// Расчёт тарифных величин сегодняшнего дня: актуальные показания счётчика
	// (Redis, device с тегами meter_*) и фиксированные граничные (PG daily_tariffs).
	now := time.Now()
	var impNow, expNow float64
	hasImp, hasExp := false, false
	for _, d := range devices {
		if isMeterDevice(d.Values) {
			// «Молчащий» счётчик (снимок старше staleCutoff) не отдаёт актуальные
			// показания — тарифные плашки не строятся из замолчавших значений.
			if ts, err := time.Parse(time.RFC3339, d.Timestamp); err != nil || !ts.After(staleCutoff) {
				continue
			}
			if v, ok := snapFloat(d.Values, "meter_import"); ok {
				impNow, hasImp = v, true
			}
			if v, ok := snapFloat(d.Values, "meter_export"); ok {
				expNow, hasExp = v, true
			}
			break
		}
	}
	impDay, impNight, expDay, expNight := 0.0, 0.0, 0.0, 0.0
	impDayM, impNightM, expDayM, expNightM := 0.0, 0.0, 0.0, 0.0
	impDayY, impNightY, expDayY, expNightY := 0.0, 0.0, 0.0, 0.0
	if hasImp && hasExp && h.pg != nil {
		// Исходники (границы дня и суммы финализированных прошедших дней месяца/года)
		// берём из TTL-кеша; сегодняшний день и живые показания счётчика (impNow/expNow)
		// пересчитываются на каждом запросе — так плашки остаются актуальными без лишних
		// запросов к PG (данные прошедших дней в PG неизменны).
		b, mSum, ySum := h.loadTariffData(now)
		if b != nil {
			impDay, impNight, expDay, expNight = meterTariffToday(now, impNow, expNow, b)
		} else {
			logTariffMissing("tariff: границы текущих суток отсутствуют в PG — тарифные плашки дня не рассчитываются")
		}
		impDayM, impNightM, expDayM, expNightM = mSum[0]+impDay, mSum[1]+impNight, mSum[2]+expDay, mSum[3]+expNight
		impDayY, impNightY, expDayY, expNightY = ySum[0]+impDay, ySum[1]+impNight, ySum[2]+expDay, ySum[3]+expNight
	}
	impDay, impNight, expDay, expNight = math.Round(impDay*100)/100, math.Round(impNight*100)/100,
		math.Round(expDay*100)/100, math.Round(expNight*100)/100
	impDayM, impNightM, expDayM, expNightM = math.Round(impDayM*100)/100, math.Round(impNightM*100)/100,
		math.Round(expDayM*100)/100, math.Round(expNightM*100)/100
	impDayY, impNightY, expDayY, expNightY = math.Round(impDayY*100)/100, math.Round(impNightY*100)/100,
		math.Round(expDayY*100)/100, math.Round(expNightY*100)/100

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(currentResponse{
		GeneratedAt:           time.Now().Format(time.RFC3339),
		TotalPower:            total,
		TotalPV:               totalPV,
		Placements:            placements,
		MapGridV:              gridV,
		MapGridP:              gridP,
		MapBatV:               batV,
		MapBatP:               batP,
		MapCons:               gridP + batP,
		MeterImportDay:        impDay,
		MeterImportNight:      impNight,
		MeterExportDay:        expDay,
		MeterExportNight:      expNight,
		MeterImportDayMonth:   impDayM,
		MeterImportNightMonth: impNightM,
		MeterExportDayMonth:   expDayM,
		MeterExportNightMonth: expNightM,
		MeterImportDayYear:    impDayY,
		MeterImportNightYear:  impNightY,
		MeterExportDayYear:    expDayY,
		MeterExportNightYear:  expNightY,
		Devices:               devices,
	})
}

// animPV возвращает P_PV устройства: у Deye — dc_total_power, у Sofar —
// pv1_power+pv2_power; у КЭС (MPPT) — pv1_power (P_PV). Порядок проверки:
// dc_total_power (Deye), затем сумма pv1+pv2 (Sofar/КЭС). Если ни одного тега
// нет — 0.
func animPV(v valuesContract) float64 {
	if x, ok := snapFloat(v, "dc_total_power"); ok {
		return x
	}
	var p float64
	if x, ok := snapFloat(v, "pv1_power"); ok {
		p += x
	}
	if x, ok := snapFloat(v, "pv2_power"); ok {
		p += x
	}
	return p
}

// apiAnimation отдаёт данные страницы анимации (/api/animation): две схемы
// (Дом и Гараж) с инверторами/КЭС и мощностями на связях. Устройства группируются
// по размещению (placement; пустое → «Дом»), MPPT-контроллеры (КЭС) — только в
// схеме Дома, МАП и счётчик — общие для Дома. Значения передаются со знаком;
// устройства со снимком старше STALE (20 мин) помечаются Stale — клиент рисует
// их мощность нулём и приглушает спрайт (ночью инверторы/КЭС не отдают данные).
func (h *dashboardHandler) apiAnimation(w http.ResponseWriter, r *http.Request) {
	devices, err := h.store.Current()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	now := time.Now()
	ce308, err := h.store.CE308Current()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	res := buildAnimationResponse(devices, ce308, h.placeByIP, now)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(res)
}

// buildAnimationResponse собирает данные страницы анимации из снимков устройств:
// сортирует их как apiCurrent (MPPT — последними), группирует инверторы по
// размещению (пустое → «Дом»), MPPT-контроллеры (КЭС) — в схему Дома, из МАП и
// счётчика берёт мощности сети/батареи/счётчика. Считает мощность Дома по
// формуле-разнице. Для Гаража берёт активную мощность электросчётчика CE308
// (ce308) и считает мощность Гаража на развилке (см. GaragePower). Устройства со
// снимком старше окна (20 мин) помечаются Stale — клиент показывает их мощность
// нулём (ночью инверторы/КЭС не отдают данные).
func buildAnimationResponse(devices []deviceSnapshot, ce308 map[string]ce308Snapshot, placeByIP map[string]string, now time.Time) animationResponse {
	// Сортируем как apiCurrent: MPPT — последними, остальные по Order.
	sort.SliceStable(devices, func(i, j int) bool {
		mi, mj := isMPPTKey(devices[i].IP), isMPPTKey(devices[j].IP)
		if mi != mj {
			return !mi
		}
		oi, oj := devices[i].Order, devices[j].Order
		if oi != oj {
			return oi < oj
		}
		return devices[i].Name < devices[j].Name
	})
	staleCutoff := now.Add(-20 * time.Minute)
	stale := func(d deviceSnapshot) bool {
		ts, err := time.Parse(time.RFC3339, d.Timestamp)
		return err != nil || !ts.After(staleCutoff)
	}

	// Схема Дома и Гаража: инверторы по размещению (пустое → «Дом»).
	house := animScheme{}
	garage := animScheme{}
	// Мощности МАП (сеть/батарея) и счётчика — из устройства МАП (батарея/сеть)
	// и электросчётчика; берём свежий снимок (аналог apiCurrent).
	for _, d := range devices {
		if isMAPDevice(d.Values) {
			if stale(d) {
				continue
			}
			if v, ok := snapFloat(d.Values, "grid_power"); ok {
				house.MapGridPower = v
			}
			if v, ok := snapFloat(d.Values, "battery_power"); ok {
				house.MapBatteryPower = v
			}
			// Температуры МАП: панель над изображением (Тор/Транзисторы) и
			// температура батареи (справа от спрайта батареи).
			house.MapTemps = mapSchemeTemps(d.Values)
			if v, ok := snapFloat(d.Values, "map_temp_battery"); ok {
				bt := v
				house.BatteryTemp = &bt
			}
			continue
		}
		// Счётчик DDS238 — не инвертор: пропускаем его ВСЕГДА (в т.ч. устаревший
		// снимок), иначе при «молчании» он попадает в ветвь инверторов Дома.
		if isMeterDevice(d.Values) {
			if !stale(d) {
				if v, ok := snapFloat(d.Values, "meter_active_power"); ok {
					house.MeterActivePower = v
				}
				if v, ok := snapFloat(d.Values, "meter_import"); ok {
					house.MeterImportTotal = math.Round(v*100) / 100
				}
				if v, ok := snapFloat(d.Values, "meter_export"); ok {
					house.MeterExportTotal = math.Round(v*100) / 100
				}
			}
			continue
		}
		if isMPPTKey(d.IP) {
			kes := animInverter{
				Name:  d.Name,
				Kind:  "kes",
				PV:    animPV(d.Values),
				AC:    snapOrZero(d.Values, "ac_active_power"),
				Stale: stale(d),
			}
			if kes.Stale { // молчащее устройство: мощности считаем нулевыми
				kes.PV, kes.AC = 0, 0
			}
			house.KES = append(house.KES, kes)
			continue
		}
		inv := animInverter{
			Name:  d.Name,
			Kind:  inverterKind(d.Values, d.Kind),
			PV:    animPV(d.Values),
			AC:    snapOrZero(d.Values, "ac_active_power"),
			Stale: stale(d),
		}
		inv.Temps = inverterTemps(inv.Kind, d.Values)
		if inv.Stale { // молчащее устройство: мощности считаем нулевыми
			inv.PV, inv.AC = 0, 0
		}
		// Размещение: приоритет у конфига (у устаревшего снимка placement может
		// отсутствовать); пустое → «Дом».
		pl := d.Placement
		if m, ok := placeByIP[d.IP]; ok {
			pl = m
		}
		if pl == "" {
			pl = "Дом"
		}
		switch pl {
		case "Гараж":
			garage.Inverters = append(garage.Inverters, inv)
		default: // «Дом» (в т.ч. пустое)
			house.Inverters = append(house.Inverters, inv)
		}
	}

	// Мощность Дома = сумма всех источников, приходящих в МАП:
	//   P_дом = Σac(инверторы дома) + P(сеть) + P(батарея)
	// где ак.мощность сети grid_power положительная при потреблении из сети и
	// отрицательная при отдаче в сеть; battery_power положительная при отдаче
	// батареи (разряде) и отрицательная при заряде. Инверторы дома отдают
	// положительную (выработка). Следовательно при отдаче в сеть она вычитается:
	// напр. 392 + (−4551) + 7038 = 2879 Вт в дом.
	houseAC := 0.0
	for _, inv := range house.Inverters {
		houseAC += inv.AC
	}
	house.HousePower = houseAC + house.MapGridPower + house.MapBatteryPower
	// Мощность на отрезке «Сеть гаража — Гараж» (новая формула владельца):
	//   P = (0 − P(внешняя сеть ↔ CE308)) + P(шина инверторов ↔ Сеть гаража)
	// где P(внешняя сеть ↔ CE308) — поток из CE308 во внешнюю сеть (отдача;
	// в знаках CE308 отдача отрицательна, поэтому величина = −ce308Power), а
	// P(шина инверторов ↔ Сеть гаража) — вклад инверторов гаража в узел = Σac.
	// Итог: P_гараж = ce308Power + Σac — нагрузка гаража (внешняя сеть + инверторы).
	// Пример: сеть +120 Вт (потребление), инверторы +1 Вт → в гараж +121 Вт;
	// при отдаче в сеть ce308Power отрицателен и вычитается из выработки.
	// Если свежего снимка CE308 нет — мощности гаража не считаем (узел статичен).
	garageAC := 0.0
	for _, inv := range garage.Inverters {
		garageAC += inv.AC
	}
	if ce308Power, ok := freshCE308Power(ce308, now); ok {
		garage.Ce308Power = ce308Power
		garage.GaragePower = ce308Power + garageAC
	} else {
		garage.Ce308Power = 0
		garage.GaragePower = 0
	}

	res := animationResponse{
		GeneratedAt: now.Format(time.RFC3339),
		House:       house,
		Garage:      garage,
	}
	// Округляем до 1 знака.
	res.House.MapGridPower = animRound1(res.House.MapGridPower)
	res.House.MapBatteryPower = animRound1(res.House.MapBatteryPower)
	res.House.MeterActivePower = animRound1(res.House.MeterActivePower)
	res.House.HousePower = animRound1(res.House.HousePower)
	res.Garage.Ce308Power = animRound1(res.Garage.Ce308Power)
	res.Garage.GaragePower = animRound1(res.Garage.GaragePower)
	for i := range res.House.Inverters {
		res.House.Inverters[i].PV = animRound1(res.House.Inverters[i].PV)
		res.House.Inverters[i].AC = animRound1(res.House.Inverters[i].AC)
	}
	for i := range res.House.KES {
		res.House.KES[i].PV = animRound1(res.House.KES[i].PV)
		res.House.KES[i].AC = animRound1(res.House.KES[i].AC)
	}
	for i := range res.Garage.Inverters {
		res.Garage.Inverters[i].PV = animRound1(res.Garage.Inverters[i].PV)
		res.Garage.Inverters[i].AC = animRound1(res.Garage.Inverters[i].AC)
	}
	return res
}

// freshCE308Power возвращает суммарную активную мощность электросчётчика CE308
// из свежего снимка (не старше окна staleCutoff, аналог stale в
// buildAnimationResponse). ok=false, если CE308 не настроен/не опрошен или снимок
// устарел — тогда мощность гаража на схеме не считаем.
func freshCE308Power(ce308 map[string]ce308Snapshot, now time.Time) (float64, bool) {
	staleCutoff := now.Add(-20 * time.Minute)
	for _, sn := range ce308 {
		ts, err := time.Parse(time.RFC3339, sn.Timestamp)
		if err != nil || !ts.After(staleCutoff) {
			continue
		}
		if v, ok := sn.Values[ce308ActiveP]; ok {
			return v, true
		}
	}
	return 0, false
}

// ce308Name возвращает имя устройства CE308 детерминированно (лексикографически
// минимальный ключ): map обходится в случайном порядке, а имя нужно как ключ
// выборки PG/Redis. "" — если текущих снимков CE308 нет.
func ce308Name(cur map[string]ce308Snapshot) string {
	name := ""
	for k := range cur {
		if name == "" || k < name {
			name = k
		}
	}
	return name
}

// inverterKind определяет марку сетевого инвертора по снимку: поле Kind задаётся
// пулером из конфига (invTarget.Kind) и содержит "deye"/"sofar"/"kes". Для
// старых снимков без Kind — эвристика по тегам: наличие dc_total_power (тег только
// у Deye) → deye, иначе sofar. Используется для выбора спрайта на странице анимации.
func inverterKind(v valuesContract, kind string) string {
	switch kind {
	case "deye":
		return "deye"
	case "sofar":
		return "sofar"
	case "kes":
		return "kes"
	}
	if _, ok := v["dc_total_power"]; ok {
		return "deye"
	}
	return "sofar"
}

// snapOrZero извлекает числовое значение из универсального контракта по ключу,
// возвращая 0 при отсутствии/нечисловом значении (для анимации: отсутствие тега
// означает нулевую мощность).
func snapOrZero(v valuesContract, key string) float64 {
	if x, ok := snapFloat(v, key); ok {
		return x
	}
	return 0
}

// animRound1 округляет float до 1 знака (значения схем анимации). Это отдельная
// функция, т.к. round1 из main.go — для тегов универсального контракта (другая
// сигнатура), а здесь значения уже извлечены в float.
func animRound1(v float64) float64 {
	return math.Round(v*10) / 10
}

// inverterTemps возвращает температуры инвертора по его марке: Корпус и Транзисторы.
// У Deye — temperature_radiator (Корпус) и temperature_igbt (Транзисторы); у Sofar —
// temperature_inner (Корпус) и temperature_module (Транзисторы). Отсутствующий
// датчик не добавляется: у Deye отсутствие маппится сентелом offset −100
// (raw 0 → −100), поэтому значения ≤ −100 пропускаются. Для КЭС (MPPT, kind "kes")
// температуры не выводятся.
func inverterTemps(kind string, v valuesContract) []animTemp {
	var temps []animTemp
	add := func(tag, label string) {
		if x, ok := snapFloat(v, tag); ok && x > -100 {
			temps = append(temps, animTemp{Label: label, Value: animRound1(x)})
		}
	}
	switch kind {
	case "deye":
		add("temperature_radiator", "Корпус")
		add("temperature_igbt", "Транзисторы")
	case "sofar":
		add("temperature_inner", "Корпус")
		add("temperature_module", "Транзисторы")
	}
	return temps
}

// mapSchemeTemps собирает температуры МАП для панели над изображением МАП:
// Тор (map_temp_tor) и Транзисторы (map_temp_transistor). Отсутствующий датчик
// (гейтится по Temp_off на стороне пулера) не добавляется.
func mapSchemeTemps(v valuesContract) []animTemp {
	var temps []animTemp
	add := func(tag, label string) {
		if x, ok := snapFloat(v, tag); ok {
			temps = append(temps, animTemp{Label: label, Value: animRound1(x)})
		}
	}
	add("map_temp_tor", "Тор")
	add("map_temp_transistor", "Транзисторы")
	return temps
}

// temperatureSeries собирает временные ряды температур всех устройств, отдающих
// температурные теги универсального контракта: сетевые инверторы (Deye —
// temperature_radiator/temperature_igbt, Sofar — temperature_inner/
// temperature_module) и МАП (map_temp_battery/map_temp_tor/map_temp_transistor).
// Каждая линия — отдельный датчик отдельного устройства («Имя — датчик»).
// Отсутствующий датчик не добавляется: у Deye отсутствие маппится offset −100
// (raw 0 → −100), такие значения (≤ −100) пропускаются; у МАП тег отсутствует,
// если датчика нет (гейт по Temp_off на стороне пулера). Порядок устройств —
// как на графике мощностей (MPPT в конец, затем по имени), МАП — в конце.
// Температуры пишутся только в Redis (в PG не усредняются, см.
// accumulatorSkipTags), поэтому за период старше окна удержания Redis ряды пусты.
func temperatureSeries(snaps []deviceSnapshot) []deviceSeries {
	type tempKey struct{ ip, label string }
	byKey := map[tempKey]*deviceSeries{}
	devName := map[string]string{}
	devIsMap := map[string]bool{}
	seenDev := map[string]bool{}
	var devOrder []string
	add := func(sn deviceSnapshot, label, tag string) {
		if _, present := sn.Values[tag]; !present {
			return
		}
		x, ok := snapFloat(sn.Values, tag)
		if !ok || x <= -100 {
			return
		}
		k := tempKey{sn.IP, label}
		ds, ok := byKey[k]
		if !ok {
			ds = &deviceSeries{IP: sn.IP}
			byKey[k] = ds
		}
		ds.Points = append(ds.Points, seriesPoint{T: sn.Timestamp, V: x})
		if !seenDev[sn.IP] {
			seenDev[sn.IP] = true
			devOrder = append(devOrder, sn.IP)
			devIsMap[sn.IP] = isMAPDevice(sn.Values)
			devName[sn.IP] = sn.Name
		}
	}
	for _, sn := range snaps {
		if isMeterDevice(sn.Values) {
			continue
		}
		switch {
		case isMAPDevice(sn.Values):
			add(sn, "Батарея", "map_temp_battery")
			add(sn, "Тор", "map_temp_tor")
			add(sn, "Транзисторы", "map_temp_transistor")
		case sn.Kind == "deye":
			add(sn, "Корпус", "temperature_radiator")
			add(sn, "Транзисторы", "temperature_igbt")
		case sn.Kind == "sofar":
			add(sn, "Корпус", "temperature_inner")
			add(sn, "Транзисторы", "temperature_module")
		}
	}
	sort.SliceStable(devOrder, func(i, j int) bool {
		a, b := devOrder[i], devOrder[j]
		if devIsMap[a] != devIsMap[b] {
			return !devIsMap[a]
		}
		mi, mj := isMPPTKey(a), isMPPTKey(b)
		if mi != mj {
			return !mi
		}
		return devName[a] < devName[b]
	})
	labelRank := map[string]int{"Батарея": 0, "Тор": 1, "Корпус": 2, "Транзисторы": 3}
	out := make([]deviceSeries, 0, len(byKey))
	for _, ip := range devOrder {
		labels := make([]tempKey, 0, 3)
		for k := range byKey {
			if k.ip == ip {
				labels = append(labels, k)
			}
		}
		sort.SliceStable(labels, func(i, j int) bool { return labelRank[labels[i].label] < labelRank[labels[j].label] })
		prefix := devName[ip]
		if devIsMap[ip] {
			prefix = "МАП"
		}
		for _, k := range labels {
			ds := byKey[k]
			ds.Name = prefix + " — " + k.label
			out = append(out, *ds)
		}
	}
	for i := range out {
		out[i].Color = tempPalette[i%len(tempPalette)]
	}
	return out
}

// apiSeries отдаёт временные ряды ac_active_power по инверторам за период [from, to].
// По умолчанию (без параметров или при ошибке парсинга) — текущие календарные сутки.
// Часть периода, попадающая в последние 2 календарных суток, читается из Redis
// (полное разрешение), более старая часть — из PostgreSQL (5-минутные средние).
func (h *dashboardHandler) apiSeries(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	loc := time.Local

	from, to := dayBounds(now, loc)
	if v := r.URL.Query().Get("from"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			from = t
		}
	}
	if v := r.URL.Query().Get("to"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			to = t
		}
	}
	// Кламп диапазона: to не дальше now (иначе eachMonth(goto, to) идёт помесячно
	// по пустым месяцам за годы — медленный ответ на «глый» LAN-эндпоинт); from —
	// с нижним пределом (recentCutoff − 400 дней), чтобы огромный from не тянул весь
	// averages + sumActive. «Перевёрнутый» диапазон (from >= to) — 400.
	if to.After(now) {
		to = now
	}
	if minFrom := recentCutoff(now).AddDate(0, 0, -400); from.Before(minFrom) {
		from = minFrom
	}
	if !from.Before(to) {
		http.Error(w, "from >= to", http.StatusBadRequest)
		return
	}

	snaps, err := h.loadRange(from, to, now)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Группируем по имени инвертора, цвет — по индексу в отсортированном списке.
	// Снимки устройства МАП (батарея/сеть) исключаем — их мощность отображается
	// только на своих графиках МАП, а не на графике активной мощности инверторов.
	byIP := map[string]*deviceSeries{}
	nameToIP := map[string]string{}
	for _, sn := range snaps {
		// Снимки устройства МАП (батарея/сеть) и счётчика DDS238 исключаем — их
		// мощность отображается на своих графиках/плашках, а не на графике активной
		// мощности инверторов. Смотрим дальше: устройство попадает в ряд только если
		// хоть в одном снимке есть ac_active_power (у счётчика его нет).
		if isMAPDevice(sn.Values) {
			continue
		}
		v, hasPower := snapFloat(sn.Values, "ac_active_power")
		if !hasPower {
			continue
		}
		if _, ok := byIP[sn.IP]; !ok {
			byIP[sn.IP] = &deviceSeries{IP: sn.IP, Name: sn.Name}
			nameToIP[sn.Name] = sn.IP
		}
		byIP[sn.IP].Points = append(byIP[sn.IP].Points, seriesPoint{T: sn.Timestamp, V: v})
	}

	names := make([]string, 0, len(byIP))
	for _, ds := range byIP {
		names = append(names, string(ds.Name))
	}
	sort.SliceStable(names, func(i, j int) bool {
		mi, mj := isMPPTKey(nameToIP[names[i]]), isMPPTKey(nameToIP[names[j]])
		if mi != mj {
			return !mi
		}
		return names[i] < names[j]
	})

	res := seriesResponse{
		GeneratedAt: now.Format(time.RFC3339),
		From:        from.Format(time.RFC3339),
		To:          to.Format(time.RFC3339),
		Series:      make([]deviceSeries, 0, len(names)),
	}
	// name → *deviceSeries, O(1) на имя вместо O(N) вложенного обхода.
	nameToSeries := make(map[string]*deviceSeries, len(byIP))
	for _, ds := range byIP {
		nameToSeries[ds.Name] = ds
	}
	for i, n := range names {
		ds := nameToSeries[n]
		ds.Color = seriesPalette[i%len(seriesPalette)]
		res.Series = append(res.Series, *ds)
	}

	// Суммарный ряд: складываем ac_active_power всех инверторов по минутным бакетам.
	// Бакетирование нужно, т.к. инверторы опрашиваются параллельно и времена точек
	// не совпадают точно; окно в 60 с сглаживает расхождение и даёт чистый итог.
	res.Total = sumActive(snaps)

	// Ряды МАП (батарея/сеть) — метрики устройства KindMAP (единственное, отдающее
	// эти теги): напряжение/мощность сети и батареи для графиков дашборда.
	res.MapGridVoltage = singleMetricSeries(snaps, "grid_voltage")
	res.MapGridPower = singleMetricSeries(snaps, "grid_power")
	res.MapBatVoltage = singleMetricSeries(snaps, "battery_voltage")
	res.MapBatPower = singleMetricSeries(snaps, "battery_power")
	res.MapCons = sumSeries(res.MapGridPower, res.MapBatPower)

	// Ряд «Мощность дома» — по формуле анимации Дома: Σac инверторов Дома (по
	// размещению из конфига, пустое → «Дом») + grid_power + battery_power. Знаки
	// как в контракте (как на странице анимации Дома). Второй возвращаемый ряд —
	// только Σac инверторов Дома (вклад Дома в формулу, отдельная линия).
	res.HousePower, res.HouseInverterPower = housePowerSeries(snaps, h.placeByIP)

	// Ряды электросчётчика DDS238: напряжение и активная мощность для наложения
	// на графики напряжений и мощностей (белые линии счётчика). Маркер устройства —
	// наличие meter_voltage, которого нет ни у инверторов, ни у МАП/MPPT.
	res.MeterVoltage = meterSeries(snaps, "meter_voltage")
	res.MeterActivePower = meterSeries(snaps, "meter_active_power")

	// Ряды температур всех устройств (инверторы Deye/Sofar + МАП) для отдельного
	// графика температур. Только Redis (в PG температуры не усредняются).
	res.Temps = temperatureSeries(snaps)

	// Ряды электросчётчика CE308 (опрос по BLE): фазные напряжения и суммарная
	// активная мощность. CE308 хранится в собственных ключах Redis/PG (нет
	// универсального контракта значений), поэтому читается отдельно от loadRange.
	ce308, err := h.ce308SeriesRange(from, to, now)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	res.CE308L1Voltage = ce308[ce308L1Voltage]
	res.CE308L2Voltage = ce308[ce308L2Voltage]
	res.CE308L3Voltage = ce308[ce308L3Voltage]
	res.CE308ActivePower = ce308[ce308ActiveP]

	// Усреднение длинных серий: если в ряду больше maxSeriesPoints точек — диапазон
	// [from, to] делится на равные периоды и точки в пределах периода схлопываются
	// в одну усреднённую (см. downsampleSeries). Применяется ко всем рядам ответа.
	res.Total = downsampleSeries(res.Total, from, to)
	for i := range res.Series {
		res.Series[i].Points = downsampleSeries(res.Series[i].Points, from, to)
	}
	res.MapGridVoltage = downsampleSeries(res.MapGridVoltage, from, to)
	res.MapGridPower = downsampleSeries(res.MapGridPower, from, to)
	res.MapBatVoltage = downsampleSeries(res.MapBatVoltage, from, to)
	res.MapBatPower = downsampleSeries(res.MapBatPower, from, to)
	res.MapCons = downsampleSeries(res.MapCons, from, to)
	res.HousePower = downsampleSeries(res.HousePower, from, to)
	res.HouseInverterPower = downsampleSeries(res.HouseInverterPower, from, to)
	res.MeterVoltage = downsampleSeries(res.MeterVoltage, from, to)
	res.MeterActivePower = downsampleSeries(res.MeterActivePower, from, to)
	res.CE308L1Voltage = downsampleSeries(res.CE308L1Voltage, from, to)
	res.CE308L2Voltage = downsampleSeries(res.CE308L2Voltage, from, to)
	res.CE308L3Voltage = downsampleSeries(res.CE308L3Voltage, from, to)
	res.CE308ActivePower = downsampleSeries(res.CE308ActivePower, from, to)
	for i := range res.Temps {
		res.Temps[i].Points = downsampleSeries(res.Temps[i].Points, from, to)
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(res)
}

// sumActive строит временной ряд суммарной активной мощности всех инверторов.
// Инверторы опрашиваются параллельно и присылают снимки с разным фазовым сдвигом
// относительно границ секундных интервалов, поэтому НЕЛЬЗЯ суммировать только те
// снимки, что попали в один бакет — иначе в каждом бакете часть инверторов молчит,
// и сумма проседает к нулю. Вместо этого на каждую точку берётся последнее известное
// (carry-forward) значение каждого инвертора на этот момент времени и суммируется.
// Получается кусочно-постоянный непрерывный ряд без глубоких провалов; правая точка
// совпадает с суммой последних значений (/api/current total_power).
//
// Окно актуальности carry-forward ограничено: если устройство не присылало снимков
// дольше inverterStaleWindow (напр. ушло офлайн/исчезло), его устаревшая мощность
// перестаёт учитываться в сумме — иначе отключившийся инвертор вечно «выдавал бы»
// последнее значение, и на графике суммарной мощности появлялась бы линия, будто он
// продолжает работать. Окно много больше асинхронного джиттера опроса (~8 с), поэтому
// глубокие провалы из-за фазовых сдвигов не возвращаются.
func sumActive(snaps []deviceSnapshot) []seriesPoint {
	const staleWindow = 20 * time.Minute
	type rec struct {
		ts time.Time
		v  float64
		ip string
	}
	var recs []rec
	for _, sn := range snaps {
		if isMAPDevice(sn.Values) {
			continue
		}
		v, ok := snapFloat(sn.Values, "ac_active_power")
		if !ok {
			continue
		}
		ts, err := time.Parse(time.RFC3339, sn.Timestamp)
		if err != nil {
			continue
		}
		recs = append(recs, rec{ts: ts, v: v, ip: sn.IP})
	}
	sort.Slice(recs, func(i, j int) bool { return recs[i].ts.Before(recs[j].ts) })
	current := map[string]float64{}
	lastSeen := map[string]time.Time{}
	out := make([]seriesPoint, 0, len(recs))
	for _, r := range recs {
		current[r.ip] = r.v
		lastSeen[r.ip] = r.ts
		// Исключаем устройства, замолчавшие дольше окна актуальности: их устаревшая
		// мощность больше не суммируется, пока не появится новый реальный снимок.
		var total float64
		for ip, v := range current {
			if r.ts.Sub(lastSeen[ip]) > staleWindow {
				delete(current, ip)
				delete(lastSeen, ip)
				continue
			}
			total += v
		}
		total = math.Round(total*10) / 10
		if n := len(out); n > 0 && out[n-1].T == r.ts.Format(time.RFC3339) {
			out[n-1].V = total
		} else {
			out = append(out, seriesPoint{T: r.ts.Format(time.RFC3339), V: total})
		}
	}
	return out
}

// sumSeries складывает два временных ряда поэлементно по совпадающей временной
// метке T и возвращает результирующий ряд (потребление = мощность сети + батареи).
// Ряды a и b отсортированы по времени и имеют общие точки (с одного снимка МАП);
// если в одном из рядов нет точки на временную метку другого — она пропускается.
func sumSeries(a, b []seriesPoint) []seriesPoint {
	var out []seriesPoint
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i].T == b[j].T:
			out = append(out, seriesPoint{T: a[i].T, V: math.Round((a[i].V+b[j].V)*10) / 10})
			i++
			j++
		case a[i].T < b[j].T:
			i++
		default:
			j++
		}
	}
	return out
}

// housePowerSeries собирает временной ряд мощности Дома по формуле анимации Дома:
//
//	P_дом(t) = Σac(инверторы Дома, carry-forward) + grid_power(t) + battery_power(t)
//
// Сетка времени — снимки МАП (как у map_grid_power/map_battery_power): на каждую
// точку МАП берётся суммарная активная мощность инверторов Дома (breaking описан
// в sumActive: инверторы опрашиваются параллельно и присылают снимки с разным
// сдвигом, поэтому складывается последнее известное значение каждого инвертора
// Дома на этот момент, с окном актуальности inverterStaleWindow). Размещение
// берётся из конфига (placeByIP), пустое → «Дом», MPPT-контроллеры (КЭС) и
// счётчик в сумму не входят (как в buildAnimationResponse).
//
// Возвращает два ряда на общей сетке МАП: house (полная мощность Дома) и ac
// (суммарная активная мощность инверторов Дома — отдельная линия на графике
// «Мощности», чтобы был виден вклад Дома в формулу).
func housePowerSeries(snaps []deviceSnapshot, placeByIP map[string]string) (house, ac []seriesPoint) {
	const staleWindow = 20 * time.Minute
	type invRec struct {
		ts time.Time
		v  float64
		ip string
	}
	// Инверторы Дома: отбираем по размещению, исключая МАП/счётчик/КЭС (КЭС — это
	// MPPT-контроллеры, их мощность уходит к батарее, а не в формулу Дома).
	invRecs := make([]invRec, 0, len(snaps))
	for _, sn := range snaps {
		if isMAPDevice(sn.Values) || isMeterDevice(sn.Values) || isMPPTKey(sn.IP) {
			continue
		}
		v, ok := snapFloat(sn.Values, "ac_active_power")
		if !ok {
			continue
		}
		pl := sn.Placement
		if m, ok := placeByIP[sn.IP]; ok {
			pl = m
		}
		if pl == "" {
			pl = "Дом"
		}
		if pl != "Дом" {
			continue
		}
		ts, err := time.Parse(time.RFC3339, sn.Timestamp)
		if err != nil {
			continue
		}
		invRecs = append(invRecs, invRec{ts: ts, v: v, ip: sn.IP})
	}
	sort.Slice(invRecs, func(i, j int) bool { return invRecs[i].ts.Before(invRecs[j].ts) })

	// Сетка времени — точки МАП (места, где есть grid_power и battery_power).
	type mapRec struct {
		ts   time.Time
		grid float64
		bat  float64
	}
	var mapRecs []mapRec
	for _, sn := range snaps {
		if !isMAPDevice(sn.Values) {
			continue
		}
		g, okG := snapFloat(sn.Values, "grid_power")
		b, okB := snapFloat(sn.Values, "battery_power")
		if !okG || !okB {
			continue
		}
		ts, err := time.Parse(time.RFC3339, sn.Timestamp)
		if err != nil {
			continue
		}
		mapRecs = append(mapRecs, mapRec{ts: ts, grid: g, bat: b})
	}
	sort.Slice(mapRecs, func(i, j int) bool { return mapRecs[i].ts.Before(mapRecs[j].ts) })

	// Carry-forward суммы инверторов Дома по точкам МАП.
	current := map[string]float64{}
	lastSeen := map[string]time.Time{}
	out := make([]seriesPoint, 0, len(mapRecs))
	acOut := make([]seriesPoint, 0, len(mapRecs))
	ii := 0
	for _, mr := range mapRecs {
		for ii < len(invRecs) && !invRecs[ii].ts.After(mr.ts) {
			current[invRecs[ii].ip] = invRecs[ii].v
			lastSeen[invRecs[ii].ip] = invRecs[ii].ts
			ii++
		}
		var ac float64
		for ip, v := range current {
			if mr.ts.Sub(lastSeen[ip]) > staleWindow {
				delete(current, ip)
				delete(lastSeen, ip)
				continue
			}
			ac += v
		}
		acR := math.Round(ac*10) / 10
		house := math.Round((ac+mr.grid+mr.bat)*10) / 10
		if n := len(out); n > 0 && out[n-1].T == mr.ts.Format(time.RFC3339) {
			out[n-1].V = house
			acOut[n-1].V = acR
		} else {
			ts := mr.ts.Format(time.RFC3339)
			out = append(out, seriesPoint{T: ts, V: house})
			acOut = append(acOut, seriesPoint{T: ts, V: acR})
		}
	}
	return out, acOut
}

// singleMetricSeries собирает временной ряд одного тега по снимкам устройства
// МАП (метрики МАП — grid_voltage, grid_power, battery_voltage, battery_power — есть
// только у kindMAP-устройства). Снимки других устройств (Deye/Sofar/MPPT) отбрасываются:
// фильтр по наличию battery_voltage исключает случайные теги с тем же именем
// (например, grid_power). Точки сортируются по времени; дубли с одинаковым временем
// схлопываются (в пределах окна SaveSnapshotWindow остаётся одна точка).
func singleMetricSeries(snaps []deviceSnapshot, key string) []seriesPoint {
	var pts []seriesPoint
	for _, sn := range snaps {
		if _, isMAP := sn.Values["battery_voltage"]; !isMAP {
			continue
		}
		v, ok := snapFloat(sn.Values, key)
		if !ok {
			continue
		}
		pts = append(pts, seriesPoint{T: sn.Timestamp, V: v})
	}
	sort.Slice(pts, func(i, j int) bool { return pts[i].T < pts[j].T })
	// Схлопываем дубли с одинаковым временем (если есть) — оставляем последний.
	if len(pts) > 1 {
		out := pts[:1]
		for i := 1; i < len(pts); i++ {
			if pts[i].T == out[len(out)-1].T {
				out[len(out)-1] = pts[i]
			} else {
				out = append(out, pts[i])
			}
		}
		pts = out
	}
	return pts
}

// meterSeries собирает временной ряд одного тега счётчика DDS238 по его снимкам
// (снимки инверторов/МАП/MPPT отбрасываются). Маркер устройства — meter_voltage.
// Точки сортируются по времени; дубли с одним временем схлопываются.
func meterSeries(snaps []deviceSnapshot, key string) []seriesPoint {
	var pts []seriesPoint
	for _, sn := range snaps {
		if _, isMeter := sn.Values["meter_voltage"]; !isMeter {
			continue
		}
		v, ok := snapFloat(sn.Values, key)
		if !ok {
			continue
		}
		pts = append(pts, seriesPoint{T: sn.Timestamp, V: v})
	}
	sort.Slice(pts, func(i, j int) bool { return pts[i].T < pts[j].T })
	if len(pts) > 1 {
		out := pts[:1]
		for i := 1; i < len(pts); i++ {
			if pts[i].T == out[len(out)-1].T {
				out[len(out)-1] = pts[i]
			} else {
				out = append(out, pts[i])
			}
		}
		pts = out
	}
	return pts
}

// maxSeriesPoints — порог размера серии /api/series: если точек больше, ряд
// усредняется (downsampleSeries), чтобы фронт не тянул и не рендерил десятки
// тысяч точек на длинных периодах.
const maxSeriesPoints = 1000

// downsampleSeries усредняет серию, если в ней больше maxSeriesPoints точек.
// Запрошенный диапазон [from, to] делится на n равных периодов: n = min(1000,
// кол-во секунд диапазона) — для диапазона короче 1000 с по одному периоду на
// секунду. Все точки, попавшие в один период, усредняются: среднее значение,
// временная метка первой точки периода; пустые периоды пропускаются. Ряд
// короче порога возвращается как есть. Точки ожидаются отсортированными по T
// (RFC3339), как их собирают помощники apiSeries.
func downsampleSeries(pts []seriesPoint, from, to time.Time) []seriesPoint {
	if len(pts) <= maxSeriesPoints {
		return pts
	}
	span := to.Sub(from).Seconds()
	if span < 1 {
		return pts
	}
	n := int(span)
	if n > maxSeriesPoints {
		n = maxSeriesPoints
	}
	binSec := span / float64(n)
	type acc struct {
		t     string
		sum   float64
		count int
	}
	bins := make([]acc, n)
	for _, p := range pts {
		t, err := time.Parse(time.RFC3339, p.T)
		if err != nil {
			continue
		}
		i := int(t.Sub(from).Seconds() / binSec)
		if i < 0 {
			i = 0
		} else if i >= n {
			i = n - 1
		}
		if bins[i].count == 0 {
			bins[i].t = p.T
		}
		bins[i].sum += p.V
		bins[i].count++
	}
	out := make([]seriesPoint, 0, n)
	for _, b := range bins {
		if b.count == 0 {
			continue
		}
		out = append(out, seriesPoint{T: b.t, V: math.Round(b.sum/float64(b.count)*10) / 10})
	}
	return out
}

// loadRange возвращает снимки за период [start, end]. Точки старше окна
// последних 2 календарных суток берутся из PostgreSQL (5-минутные средние),
// точки внутри окна — из Redis (полное разрешение). Если PG отключено,
// возвращаются только данные из Redis в пределах окна удержания.
//
// Результат кешируется на rangeCacheTTL: повторные запросы /api/series за тот
// же период за цикл (несколько вкладок, зум) сходятся в один read.
func (h *dashboardHandler) loadRange(start, end time.Time, now time.Time) ([]deviceSnapshot, error) {
	key := fmt.Sprintf("%d|%d", start.UnixNano(), end.UnixNano())
	h.cacheMu.Lock()
	if c, ok := h.cache[key]; ok && now.Sub(c.at) < rangeCacheTTL {
		h.cacheMu.Unlock()
		return c.snaps, nil
	}
	// Чистим устаревшие записи, пока держим блокировку.
	for k, c := range h.cache {
		if now.Sub(c.at) >= rangeCacheTTL {
			delete(h.cache, k)
		}
	}
	h.cacheMu.Unlock()

	snaps, err := h.loadRangeUncached(start, end, now)
	if err != nil {
		return nil, err
	}
	h.cacheMu.Lock()
	if h.cache == nil {
		h.cache = map[string]cachedRange{}
	}
	h.cache[key] = cachedRange{at: now, snaps: snaps}
	h.cacheMu.Unlock()
	return snaps, nil
}

// loadRangeUncached — реальное чтение из PG (старая часть) и Redis (рецентная часть).
func (h *dashboardHandler) loadRangeUncached(start, end time.Time, now time.Time) ([]deviceSnapshot, error) {
	cutoff := recentCutoff(now)
	var all []deviceSnapshot
	// Старая часть периода (до cutoff) — из PostgreSQL.
	// oldEnd = cutoff−1с (аналог BMS-ветки apiBMSSeries): cutoff = 00:00 вчера —
	// граница 5-минутного промежутка; если бы PG читал [start, cutoff] включительно,
	// 5-мин точка ровно на cutoff и сырой снимок Redis на cutoff дали бы дубль точки
	// (одинаковый T) в поинверторном ряду byIP (нет dedup) — «шторка» на графике.
	// Сырые снимки Redis на [cutoff, …] закрывают шов — лап не будет.
	if h.pg != nil && start.Before(cutoff) {
		oldEnd := cutoff.Add(-time.Second)
		if end.Before(oldEnd) {
			oldEnd = end
		}
		pgSnaps, err := h.pg.Averages(start, oldEnd)
		if err != nil {
			return nil, err
		}
		all = append(all, pgSnaps...)
	}
	// Рецентная часть периода (от cutoff) — из Redis (полное разрешение ~10 с).
	// Если в Redis снимков нет (их потеряли, например при сбое Redis или пока был
	// выключен пулер, а реставрация из PG срабатывает только при полностью пустом
	// Redis) — дополняем окно 5-минутными средними из PG, чтобы график не остался
	// пустым, а показал хотя бы усреднённую историю за период.
	if end.After(cutoff) {
		rStart := start
		if rStart.Before(cutoff) {
			rStart = cutoff
		}
		redisSnaps, err := h.store.QuerySeries(rStart, end)
		if err != nil {
			return nil, err
		}
		if len(redisSnaps) == 0 && h.pg != nil {
			pgSnaps, perr := h.pg.Averages(rStart, end)
			if perr != nil {
				return nil, perr
			}
			all = append(all, pgSnaps...)
		} else {
			all = append(all, redisSnaps...)
		}
	}
	return all, nil
}

// dayBounds возвращает границы текущих календарных суток в зоне loc.
func dayBounds(now time.Time, loc *time.Location) (time.Time, time.Time) {
	y, m, d := now.In(loc).Date()
	start := time.Date(y, m, d, 0, 0, 0, 0, loc)
	end := start.AddDate(0, 0, 1).Add(-time.Nanosecond)
	return start, end
}

// recoverMiddleware — защитная обёртка над mux: паника в любом обработчике
// логируется (со стеком) и превращается в http.Error 500 вместо падения
// всего сервера. Один упавший хендлер (напр. из-за краевых данных в Redis/PG)
// не должен уносить остальные эндпоинты.
func recoverMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("dashboard: panic в %s %s: %v\n%s", r.Method, r.URL.Path, rec, debug.Stack())
				http.Error(w, "internal error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// apiRelay отвечает на /api/relay.
//   - GET — снимок состояния ламп (имя, канал, текущее состояние).
//   - POST (JSON {"name": <имя лампы>, "state": "off|on|blink"}) — переключает
//     лампу в заданное состояние, сохраняя текущее состояние второй лампы.
func (h *dashboardHandler) apiRelay(w http.ResponseWriter, r *http.Request) {
	if h.relay == nil {
		http.Error(w, "relay controller is not configured", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if r.Method == http.MethodPost {
		var req struct {
			Name  string    `json:"name"`
			State lampState `json:"state"`
		}
		// Разбираем state как строку ("on"/"off"/"blink").
		var raw struct {
			Name  string `json:"name"`
			State string `json:"state"`
		}
		if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
			http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
			return
		}
		st, ok := parseLampState(raw.State)
		if !ok {
			http.Error(w, `unknown state (use "off","on","blink"): `+raw.State, http.StatusBadRequest)
			return
		}
		req.Name = raw.Name
		req.State = st
		if req.Name == "" {
			http.Error(w, "missing lamp name", http.StatusBadRequest)
			return
		}
		if err := h.relay.SetLampByName(req.Name, req.State); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	_ = json.NewEncoder(w).Encode(relayStatusResponse{
		GeneratedAt: time.Now().Format(time.RFC3339),
		Lamps:       h.relay.Snapshot(),
	})
}

// relayStatusResponse — ответ /api/relay.
type relayStatusResponse struct {
	GeneratedAt string            `json:"generated_at"`
	Lamps       []relayLampStatus `json:"lamps"`
}

// apiBasicAuth — HTTP Basic-аутентификация для `/api/*` дашборда. При пустых
// user/pass — проход без проверки (доступ снаружи закрывает обратный прокси).
// Используется constant-time сравнение, защищающее от timing-атак по паролю.
func apiBasicAuth(user, pass string) func(http.Handler) http.Handler {
	if user == "" && pass == "" {
		return func(next http.Handler) http.Handler { return next }
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			u, p, ok := r.BasicAuth()
			valid := ok &&
				subtle.ConstantTimeCompare([]byte(u), []byte(user)) == 1 &&
				subtle.ConstantTimeCompare([]byte(p), []byte(pass)) == 1
			if !valid {
				w.Header().Set("WWW-Authenticate", `Basic realm="sunreceiver"`)
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// buildDashboardMux собирает всё HTTP-дерево дашборда: статику, страницы и `/api/*`
// под HTTP Basic (если заданы учётные данные), оборачивая recoverMiddleware.
// Вынесено из serveDashboard, чтобы роутинг был покрыт тестом на регрессию.
// API-хендлеры монтируются по ПОЛНОМУ пути `/api/<pattern>` на главном mux, а не
// через вложенный подмьютекс с http.StripPrefix: последний в связке с ServeMux
// отдаёт 307-редирект на искажённый путь (остаточный RawPath) и ломает API.
func buildDashboardMux(pages map[string]http.HandlerFunc, static http.Handler, api map[string]http.HandlerFunc, authUser, authPass string) http.Handler {
	auth := apiBasicAuth(authUser, authPass)
	mux := http.NewServeMux()
	for pat, fn := range api {
		// Ключи начинаются с '/': "/api/"+"/current" дало бы "/api//current"
		// (двойной слэш — маршрут не совпадал и всё падало в catch-all "/").
		mux.Handle("/api/"+strings.TrimPrefix(pat, "/"), auth(http.HandlerFunc(fn)))
	}
	mux.Handle("/static/", static)
	for pat, fn := range pages {
		mux.HandleFunc(pat, fn)
	}
	return recoverMiddleware(mux)
}

// apiCE308Current отвечает на GET /api/ce308/current: текущий снимок CE308
// (мгновенные значения + время актуальности). 404, если счётчик ещё не опрошен.
func (h *dashboardHandler) apiCE308Current(w http.ResponseWriter, r *http.Request) {
	cur, err := h.store.CE308Current()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if len(cur) == 0 {
		http.Error(w, "нет данных CE308", http.StatusNotFound)
		return
	}
	writeJSONResponse(w, cur)
}

// apiCE308Energy обрабатывает GET и POST /api/ce308/energy. GET отдаёт последний
// разовый снимок накопленной энергии CE308; POST — сигнал пулеру снять свежий
// снимок (не чаще раза в ce308EnergyMinInterval, иначе 429).
func (h *dashboardHandler) apiCE308Energy(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		// Ручной снимок энергии ограничен по частоте (не чаще ce308EnergyMinInterval):
		// иначе частые клики «Обновить» заставляют пулер подолгу читать END01..END04 и
		// блокируют опрос мгновенных значений. Повторный запрос — 429 + retry_after.
		h.ce308EnergyMu.Lock()
		if !h.ce308EnergyAt.IsZero() {
			if d := time.Since(h.ce308EnergyAt); d < ce308EnergyMinInterval {
				delay := ce308EnergyMinInterval - d
				h.ce308EnergyMu.Unlock()
				w.Header().Set("Content-Type", "application/json; charset=utf-8")
				w.WriteHeader(http.StatusTooManyRequests)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"accepted":    false,
					"retry_after": delay.Seconds(),
				})
				return
			}
		}
		h.ce308EnergyAt = time.Now()
		h.ce308EnergyMu.Unlock()
		ok := triggerCE308EnergySnapshot()
		writeJSONResponse(w, map[string]bool{"accepted": ok})
		return
	}
	snap, err := h.store.CE308Energy()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if snap == nil {
		http.Error(w, "нет снимка энергии CE308", http.StatusNotFound)
		return
	}
	writeJSONResponse(w, snap)
}

// apiCE308Series отвечает на GET /api/ce308/series?from&to: история мгновенных
// значений CE308 за период [from, to] (RFC3339; по умолчанию — последние 24 ч).
// Рецентная часть (окно удержания Redis) — из Redis-ряда (~1 точка за 2 с),
// более старая — из PostgreSQL (10-сек усреднённые точки). Точки объединяются
// и сортируются по времени.
func (h *dashboardHandler) apiCE308Series(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	to := now
	if s := r.URL.Query().Get("to"); s != "" {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			to = t
		}
	}
	from := to.Add(-24 * time.Hour)
	if s := r.URL.Query().Get("from"); s != "" {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			from = t
		}
	}
	if to.After(now) {
		to = now
	}
	if minFrom := recentCutoff(now).AddDate(0, 0, -400); from.Before(minFrom) {
		from = minFrom
	}
	if !from.Before(to) {
		http.Error(w, "from >= to", http.StatusBadRequest)
		return
	}
	cur, _ := h.store.CE308Current()
	name := ce308Name(cur)
	if name == "" {
		http.Error(w, "нет данных CE308", http.StatusNotFound)
		return
	}
	cutoff := recentCutoff(now)
	var pts []seriesPoint
	// Старая часть периода (до cutoff) — из PostgreSQL (вся история 10-сек точек).
	if h.pg != nil && from.Before(cutoff) {
		pgEnd := cutoff.Add(-time.Second)
		if to.Before(pgEnd) {
			pgEnd = to
		}
		old, err := h.pg.QueryCE308Averages(name, from, pgEnd)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		for _, p := range old {
			pts = append(pts, ce308SeriesPoint(p))
		}
	}
	// Рецентная часть (в пределах окна удержания) — из Redis-ряда.
	redisStart := from
	if redisStart.Before(cutoff) {
		redisStart = cutoff
	}
	if to.After(redisStart) {
		recent, err := h.store.QueryCE308Series(redisStart, to)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		for _, sn := range recent {
			if fn, ferr := toFloat(sn.Values[ce308ActiveP]); ferr {
				pts = append(pts, seriesPoint{T: sn.Timestamp, V: fn})
			}
		}
	}
	sort.Slice(pts, func(i, j int) bool { return pts[i].T < pts[j].T })
	pts = downsampleSeries(pts, from, to)
	if pts == nil {
		pts = []seriesPoint{}
	}
	writeJSONResponse(w, map[string]any{
		"name":   name,
		"from":   from.Format(time.RFC3339),
		"to":     to.Format(time.RFC3339),
		"points": pts,
	})
}

// ce308SeriesPoint преобразует усреднённую 10-сек точку PG в seriesPoint
// (одна линия суммарной активной мощности CE308).
func ce308SeriesPoint(p ce308PGPoint) seriesPoint {
	f, _ := toFloat(p.Values[ce308ActiveP])
	return seriesPoint{T: p.TS.Format(time.RFC3339), V: f}
}

// ce308SeriesRange собирает по электросчётчику CE308 за период [from, to]
// временные ряды фазных напряжений (ce308_l1/l2/l3_voltage) и суммарной активной
// мощности (ce308_active_power) для наложения на графики напряжений и мощностей.
// Рецентная часть (окно удержания Redis) — из Redis-ряда (~1 точка за 2 с),
// более старая — из PostgreSQL (10-сек усреднённые точки). Возвращает map
// «тег → ряд». Если счётчик ещё не опрошен (нет текущего имени) — nil.
func (h *dashboardHandler) ce308SeriesRange(from, to time.Time, now time.Time) (map[string][]seriesPoint, error) {
	cur, err := h.store.CE308Current()
	if err != nil {
		return nil, err
	}
	name := ce308Name(cur)
	if name == "" {
		return nil, nil
	}
	type rec struct {
		ts   time.Time
		vals map[string]float64
	}
	var recs []rec
	cutoff := recentCutoff(now)
	// Старая часть периода (до cutoff) — из PostgreSQL (10-сек точки).
	if h.pg != nil && from.Before(cutoff) {
		pgEnd := cutoff.Add(-time.Second)
		if to.Before(pgEnd) {
			pgEnd = to
		}
		old, err := h.pg.QueryCE308Averages(name, from, pgEnd)
		if err != nil {
			return nil, err
		}
		for _, p := range old {
			recs = append(recs, rec{ts: p.TS, vals: p.Values})
		}
	}
	// Рецентная часть (в пределах окна удержания) — из Redis-ряда.
	redisStart := from
	if redisStart.Before(cutoff) {
		redisStart = cutoff
	}
	if to.After(redisStart) {
		recent, err := h.store.QueryCE308Series(redisStart, to)
		if err != nil {
			return nil, err
		}
		for _, sn := range recent {
			ts, terr := time.Parse(time.RFC3339, sn.Timestamp)
			if terr != nil {
				continue
			}
			recs = append(recs, rec{ts: ts, vals: sn.Values})
		}
	}
	sort.Slice(recs, func(i, j int) bool { return recs[i].ts.Before(recs[j].ts) })
	out := map[string][]seriesPoint{}
	for _, metric := range []string{ce308L1Voltage, ce308L2Voltage, ce308L3Voltage, ce308ActiveP} {
		var pts []seriesPoint
		lastT := ""
		for _, r := range recs {
			v, ok := r.vals[metric]
			if !ok {
				continue
			}
			t := r.ts.Format(time.RFC3339)
			if t == lastT {
				if n := len(pts); n > 0 {
					pts[n-1].V = v
				}
				continue
			}
			lastT = t
			pts = append(pts, seriesPoint{T: t, V: v})
		}
		out[metric] = pts
	}
	return out, nil
}

// writeJSONResponse — вспомогательный вывод JSON-ответа (no-store).
func writeJSONResponse(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}

// serveDashboard — HTTP-сервер веб-дашборда. При закрытии stop аккуратно
// завершает сервер (http.Server.Shutdown, бюджет 5 с), чтобы main мог закрыть
// пулы Redis/PG после завершения всех фоновых горутин (bgWg).
func serveDashboard(addr string, store *redisStore, pg *pgStore, relay *relayController, stop context.Context, flags dashFlags, placements []string, placeByIP map[string]string, authUser, authPass string) {
	h := &dashboardHandler{store: store, pg: pg, flags: flags, relay: relay, placements: placements, placeByIP: placeByIP}
	// Внутренние страницы (индекс/графики) открыты; данные и управление —
	// в `/api/*`, защищаются HTTP Basic (если заданы учётные данные).
	pages := map[string]http.HandlerFunc{
		"/":       h.index,
		"/charts": h.charts,
		"/energy": h.energy,
		"/bms/":   h.bmsDetail,
	}
	api := map[string]http.HandlerFunc{
		"/current":       h.apiCurrent,
		"/series":        h.apiSeries,
		"/tariffs":       h.apiTariffs,
		"/animation":     h.apiAnimation,
		"/bms":           h.apiBMS,
		"/bms/":          h.apiBMSOne,
		"/ce308/current": h.apiCE308Current,
		"/ce308/energy":  h.apiCE308Energy,
		"/ce308/series":  h.apiCE308Series,
	}
	if relay != nil {
		api["/relay"] = h.apiRelay
	}
	handler := buildDashboardMux(pages, staticFiles(), api, authUser, authPass)
	srv := &http.Server{
		Addr: addr, Handler: handler,
		// Таймауты защищают от slowloris и «висящих» соединений, не ограничивая
		// длинные ответы (/api/series за большой период идёт из PG — WriteTimeout=0):
		// ReadHeaderTimeout отсекает медленные/зависшие клиенты при приёме заголовка,
		// IdleTimeout сбрасывает неактивные keep-alive, MaxHeaderBytes — лимит шапки.
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
		WriteTimeout:      0,
		MaxHeaderBytes:    1 << 20,
	}
	go func() {
		<-stop.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()
	log.Printf("dashboard: http://%s/ (графики — http://%s/charts)", addr, addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Printf("dashboard: %v", err)
	}
}

// apiTariffs отдаёт посуточную тарифную статистику счётчика (день/ночь ×
// потребление/отдача) за запрошенный период [from, to] (RFC3339, дата+время),
// отсортированную по дате. Выборка — по календарным дням в локальной зоне:
// первый день по дате from, последний по дате to (время внутри суток на
// выборку не влияет). Параметры from/to необязательны; если не заданы —
// берётся текущий календарный месяц. Только финализированные дни (полные
// показания на 00:00/07:00/23:00).
func (h *dashboardHandler) apiTariffs(w http.ResponseWriter, r *http.Request) {
	from, to := parseTariffRange(r.URL.Query().Get("from"), r.URL.Query().Get("to"))
	days := []meterDayStat{}
	if h.pg != nil {
		var err error
		days, err = h.pg.DailyTariffsRange(from, to)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(meterDailyResponse{
		GeneratedAt: time.Now().Format(time.RFC3339),
		From:        from.Format(time.RFC3339),
		To:          to.Add(-time.Second).Format(time.RFC3339),
		Days:        days,
	})
}

// parseTariffRange разбирает необязательные параметры from/to (RFC3339,
// дата+время) в диапазон дней [start, end) в локальной зоне: start — начало
// суток первого дня (по дате from), end — начало суток после последнего
// (по дате to). Время внутри суток на выборку не влияет — только календарный
// день. Пустые значения или ошибка парсинга дают текущий календарный месяц.
func parseTariffRange(fromStr, toStr string) (time.Time, time.Time) {
	now := time.Now()
	loc := now.Location()
	defStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, loc)
	defEnd := defStart.AddDate(0, 1, 0)
	if t, err := time.Parse(time.RFC3339, fromStr); err == nil {
		t = t.In(loc)
		defStart = time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, loc)
	}
	if t, err := time.Parse(time.RFC3339, toStr); err == nil {
		t = t.In(loc)
		defEnd = time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, loc).AddDate(0, 0, 1)
	}
	if defEnd.Before(defStart) {
		defEnd = defStart.AddDate(0, 1, 0)
	}
	return defStart, defEnd
}

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
// графиков делает 4 fetch /api/series с одинаковыми from/to за цикл (60 с), и
// данные Redis обновляются каждые ~10 с, поэтому 15 с — свежее и даёт схождение
// всех 4 запросов в один реальный read из Redis/PG.
const rangeCacheTTL = 15 * time.Second

// tariffCacheTTL — срок жизни кеша тарифных исходников (границ текущего дня и
// сумм финализированных дней месяца/года). Главная страница обновляется каждую
// секунду, но эти данные в PG меняются редко: границы сегодня — 3 раза в сутки,
// набор финализированных дней — раз в сутки. Поэтому 60-секундный кеш снимает
// 3 запроса к PG/сек (7200/мин) до ~1 раза в минуту без заметной задержки.
const tariffCacheTTL = 60 * time.Second

// dashFlags — флаги видимости блоков на дашборде, вычисленные из sunReceiver.json.
// Nonzero-поля управляют рендерингом рамок/плашек и кнопки «Электроэнергия»:
//   - ShowMap — показывать блок «Данные МАП» (map.disabled != true);
//   - ShowMeter — показывать блок счётчика и тарифов, кнопку «Электроэнергия»
//     (счётчик опрашивается, meter.disabled != true);
//   - ShowBMS — показывать блок BMS-батареек (пулер ANT BMS запущен).
type dashFlags struct {
	ShowMap   bool
	ShowMeter bool
	ShowBMS   bool
	ShowRelay bool
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
	MapGridPower   float64 `json:"map_grid_power"`
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
	// GaragePower — мощность Гаража («остаток»; пока нет нагрузки — 0).
	GaragePower float64 `json:"garage_power"`
}

// animInverter — одно устройство схемы (инвертор или КЭС).
type animInverter struct {
	Name  string  `json:"name"`
	Kind  string  `json:"kind"`  // "deye"|"sofar" (инвертор) или "kes" (КЭС) — выбор спрайта
	PV    float64 `json:"pv"`    // активная мощность PV (P_PV)
	AC    float64 `json:"ac"`    // активная мощность на выходе (ac_active_power)
	Stale bool    `json:"stale"` // снимок старше окна (устройство молчит, напр. ночью)
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
	GeneratedAt      string         `json:"generated_at"`
	From             string         `json:"from"`
	To               string         `json:"to"`
	Series           []deviceSeries `json:"series"`
	Total            []seriesPoint  `json:"total,omitempty"`
	MapGridVoltage   []seriesPoint  `json:"map_grid_voltage,omitempty"`
	MapGridPower     []seriesPoint  `json:"map_grid_power,omitempty"`
	MapBatVoltage    []seriesPoint  `json:"map_battery_voltage,omitempty"`
	MapBatPower      []seriesPoint  `json:"map_battery_power,omitempty"`
	MapCons          []seriesPoint  `json:"map_consumption,omitempty"`
	MeterVoltage     []seriesPoint  `json:"meter_voltage,omitempty"`
	MeterActivePower []seriesPoint  `json:"meter_active_power,omitempty"`
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
		math.Round(total*10)/10,
		math.Round(totalPV*10)/10
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
	res := buildAnimationResponse(devices, h.placeByIP, now)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(res)
}

// buildAnimationResponse собирает данные страницы анимации из снимков устройств:
// сортирует их как apiCurrent (MPPT — последними), группирует инверторы по
// размещению (пустое → «Дом»), MPPT-контроллеры (КЭС) — в схему Дома, из МАП и
// счётчика берёт мощности сети/батареи/счётчика. Считает мощность Дома по
// формуле-разнице. Устройства со снимком старше окна (20 мин) помечаются Stale —
// клиент показывает их мощность нулём (ночью инверторы/КЭС не отдают данные).
func buildAnimationResponse(devices []deviceSnapshot, placeByIP map[string]string, now time.Time) animationResponse {
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
			continue
		}
		if isMeterDevice(d.Values) && !stale(d) {
			if v, ok := snapFloat(d.Values, "meter_active_power"); ok {
				house.MeterActivePower = v
			}
			if v, ok := snapFloat(d.Values, "meter_import"); ok {
				house.MeterImportTotal = math.Round(v*100) / 100
			}
			if v, ok := snapFloat(d.Values, "meter_export"); ok {
				house.MeterExportTotal = math.Round(v*100) / 100
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
	// В гараже пока нет нагрузки — «остаток» нулевой.
	garage.GaragePower = 0

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

	// Ряды электросчётчика DDS238: напряжение и активная мощность для наложения
	// на графики напряжений и мощностей (белые линии счётчика). Маркер устройства —
	// наличие meter_voltage, которого нет ни у инверторов, ни у МАП/MPPT.
	res.MeterVoltage = meterSeries(snaps, "meter_voltage")
	res.MeterActivePower = meterSeries(snaps, "meter_active_power")

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
	res.MeterVoltage = downsampleSeries(res.MeterVoltage, from, to)
	res.MeterActivePower = downsampleSeries(res.MeterActivePower, from, to)

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
// Результат кешируется на rangeCacheTTL: страница графиков делает 4 fetch
// /api/series с одинаковыми from/to за цикл, и все 4 сходятся в один read.
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

// serveDashboard — HTTP-сервер веб-дашборда. При закрытии stop аккуратно
// завершает сервер (http.Server.Shutdown, бюджет 5 с), чтобы main мог закрыть
// пулы Redis/PG после завершения всех фоновых горутин (bgWg).
func serveDashboard(addr string, store *redisStore, pg *pgStore, relay *relayController, stop context.Context, flags dashFlags, placements []string, placeByIP map[string]string, authUser, authPass string) {
	h := &dashboardHandler{store: store, pg: pg, flags: flags, relay: relay, placements: placements, placeByIP: placeByIP}
	// Внутренние страницы (индекс/графики) открыты; данные и управление —
	// в `/api/*`, защищаются HTTP Basic (если заданы учётные данные).
	pages := map[string]http.HandlerFunc{
		"/":         h.index,
		"/charts":   h.charts,
		"/energy":   h.energy,
		"/bms/":     h.bmsDetail,
	}
	api := map[string]http.HandlerFunc{
		"/current": h.apiCurrent,
		"/series":  h.apiSeries,
		"/tariffs": h.apiTariffs,
		"/animation": h.apiAnimation,
		"/bms":     h.apiBMS,
		"/bms/":    h.apiBMSOne,
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

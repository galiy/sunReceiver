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
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"
)

// bmsDevice — одна ANT BMS из коллекции bmslistener (read_bms.php, System V
// shm 2018 на ПАК «Малина»). Формат публикации — bmslistener/bmslistener.c
// (publish_all); все поля присутствуют в ответе.
type bmsDevice struct {
	// Kind — тип BMS: "antbms" (read_bms.php) или "enbms" (BLE, Enjie). Пусто —
	// читать как ANT (обратная совместимость записей без поля). Дашборд по нему
	// скрывает поля, которых у EnBMS нет (MOS/балансировка, счётчик кадров).
	Kind          string    `json:"kind,omitempty"`
	DeviceName    string    `json:"deviceName"`     // "AntBms <ёмкость> A/h"
	Port          string    `json:"port"`           // USB-порт адаптера (позиционный)
	Key           string    `json:"key"`            // ключ устройства (bmsKey): deviceName или "deviceName@Port"; для запросов/URL
	Timestamp     int64     `json:"timestamp"`      // Unix-время последнего валидного кадра
	Time          string    `json:"time"`           // локальное время HH:MM:SS
	CellCount     int       `json:"cell_count"`     // число ячеек (S)
	CellsV        []float64 `json:"cells_v"`        // напряжения ячеек, V
	CurrentA      float64   `json:"current_a"`      // ток, A (знаковый)
	Soc           int       `json:"soc"`            // State of Charge, %
	CapacityAh    float64   `json:"capacity_ah"`    // ёмкость, А·ч
	RemainingAh   float64   `json:"remaining_ah"`   // остаточная ёмкость, А·ч
	TemperaturesC []float64 `json:"temperatures_c"` // температуры, °C (до 6 датчиков)
	ChargeMos     int       `json:"charge_mos"`     // MOS зарядки: 1 = включён
	DischargeMos  int       `json:"discharge_mos"`  // MOS разряда: 1 = включён
	Balancer      int       `json:"balancer"`       // балансировка: 1 = активна
	PowerW        float64   `json:"power_w"`        // мощность, W (знаковая)
	MaxCellIdx    int       `json:"max_cell_idx"`   // индекс ячейки с максимальным напряжением
	MaxCellV      float64   `json:"max_cell_v"`     // напряжение максимальной ячейки, V
	MinCellIdx    int       `json:"min_cell_idx"`   // индекс ячейки с минимальным напряжением
	MinCellV      float64   `json:"min_cell_v"`     // напряжение минимальной ячейки, V
	AvgCellV      float64   `json:"avg_cell_v"`     // среднее напряжение ячейки, V
	Frames        uint32    `json:"frames"`         // счётчик валидных кадров с запуска слушателя
	// Дополнительно (по типам BMS):
	Soh         float64  `json:"soh,omitempty"`          // SOH, % (EnBMS)
	Cycles      int      `json:"cycles,omitempty"`       // число циклов (EnBMS)
	BalanceMask uint32   `json:"balance_mask,omitempty"` // маска балансируемых ячеек (ANT, addr 132)
	CycleAh     float64  `json:"cycle_ah,omitempty"`     // суммарная цикловая ёмкость, А·ч (ANT, addr 83)
	Model       string   `json:"model,omitempty"`        // модель/протокол устройства (EnBMS BasicInfo)
	Alarms      []string `json:"alarms,omitempty"`       // активные алармы/защиты (человекочитаемо)
}

// bmsKey — ключ BMS-устройства для HASH sunreceiver:bms, Redis-ряда, PG (name)
// и дашборда (/api/bms/<name>). Приоритет — уже вычисленный d.Key (если задан
// при резолве коллекции); иначе — deviceName, либо "deviceName@Port" при
// непустом Port (фолбэк для одиночных вызовов, где контекст коллизий неизвестен).
func bmsKey(d bmsDevice) string {
	if d.Key != "" {
		return d.Key
	}
	if d.Port != "" {
		return d.DeviceName + "@" + d.Port
	}
	return d.DeviceName
}

// resolveBMSKey вычисляет ключ устройства в контексте всей коллекции:
// если среди активных устройств есть ДВА с одинаковым DeviceName (коллизия
// имён) — ключ различается по USB-порту ("deviceName@Port"); иначе ключ = сам
// DeviceName. Так обычные (уникальные) имена сохраняют прежний ключ и
// непрерывность исторических рядов Redis/PG, а реальная коллизия имён
// разводится по порту.
func resolveBMSKey(d bmsDevice, nameCount map[string]int) string {
	if nameCount[d.DeviceName] > 1 && d.Port != "" {
		return d.DeviceName + "@" + d.Port
	}
	return d.DeviceName
}

// bmsCollection — ответ read_bms.php (публикация bmslistener).
type bmsCollection struct {
	Updated int64       `json:"updated"` // Unix-время публикации коллекции
	Devices []bmsDevice `json:"devices"`
}

// bmsApiClient — доступ к read_bms.php (веб-API ПАК «Малина»). Тот же хост и
// Basic-auth, что и у read_json.php (раздел "mppt" sunReceiver.json); путь
// эндпоинта задаётся полем mppt.bms_path.
type bmsApiClient struct {
	url     string
	client  *http.Client
	authHdr string
}

// bmsSite — глобальный доступ к read_bms.php; заполняется в main() из раздела
// "map" sunReceiver.json (поле bms_path). nil — опрос BMS отключён.
var bmsSite *bmsApiClient

// loadBmsSite собирает bmsApiClient из раздела "map", если в нём задано bms_path.
// Если поле отсутствует или раздел неполный — nil (BMS не опрашивается).
func loadBmsSite(sec *mapSection) *bmsApiClient {
	if sec == nil || sec.BMSPath == "" {
		return nil
	}
	if sec.Disabled != nil && *sec.Disabled {
		log.Printf("bms: раздел map disabled=true — опрос BMS отключён")
		return nil
	}
	if sec.BMSDisabled != nil && *sec.BMSDisabled {
		log.Printf("bms: bms_disabled=true — опрос BMS отключён")
		return nil
	}
	if sec.BaseURL == "" || sec.Login == "" || sec.Password == "" {
		log.Printf("bms: раздел map неполный (нужны base_url, login, password) — опрос BMS отключён")
		return nil
	}
	tok := base64.StdEncoding.EncodeToString([]byte(sec.Login + ":" + sec.Password))
	return &bmsApiClient{
		url:     sec.BaseURL + sec.BMSPath,
		client:  &http.Client{Timeout: 5 * time.Second},
		authHdr: "Basic " + tok,
	}
}

// fetch читает коллекцию BMS с read_bms.php.
func (s *bmsApiClient) fetch(ctx context.Context) (*bmsCollection, error) {
	req, err := http.NewRequest(http.MethodGet, s.url, nil)
	if err != nil {
		return nil, err
	}
	rctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	req = req.WithContext(rctx)
	req.Header.Set("Authorization", s.authHdr)
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("bms api get %s: %w", s.url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("bms api %s: status %d", s.url, resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("bms api %s: read: %w", s.url, err)
	}
	var col bmsCollection
	if err := json.Unmarshal(body, &col); err != nil {
		return nil, fmt.Errorf("bms api: parse json: %w", err)
	}
	return &col, nil
}

// runBmsPoll — отдельный 1-секундный цикл опроса ANT BMS (read_bms.php на
// ПАК «Малина»; данные — C-демон bmslistener, shm 2018).
//
//   - актуальное состояние пишется в Redis-ключ (HASH sunreceiver:bms);
//   - КАЖДОЕ снятое показание пишется в Redis-ряд (месячный ZSET
//     sunreceiver:bms:series, окно 2 календарных суток; см. saveBMSReading);
//   - параллельно 1-секундные снимки накапливаются в памяти (bmsAccumulator) в
//     5-минутные усреднённые точки и пишутся в PostgreSQL
//     (sunreceiver.bms_averages) — гранулярность PG = 1 запись / 5 минут.
//
// В общем снимке sunreceiver:current BMS не участвует (нет универсального
// контракта values).
func runBmsPoll(store *redisStore, pg *pgStore, ctx context.Context) {
	ticker := time.NewTicker(bmsPollInterval)
	defer ticker.Stop()
	acc := newBmsAccumulator()
	lastAlarms := map[string]map[string]bool{} // device key -> активные алармы (для истории)
	log.Printf("bms avg: накопление 5-минутных усреднённых точек (в памяти процесса; PG %v)", pg != nil)
	for {
		select {
		case <-ticker.C:
			col := pollAndSaveBMS(ctx, store)
			if col != nil {
				now := time.Now()
				for i := range col.Devices {
					d := col.Devices[i]
					acc.add(d, now)
					saveBMSReading(store, d, now)
					// История ошибок: пишем только появление нового аларма.
					if pg != nil {
						key := bmsKey(d)
						cur := map[string]bool{}
						for _, a := range d.Alarms {
							cur[a] = true
						}
						prev := lastAlarms[key]
						for a := range cur {
							if prev == nil || !prev[a] {
								if err := pg.InsertDeviceError(key, "antbms", a, a, now); err != nil {
									log.Printf("bms error pg %s: %v", key, err)
								}
							}
						}
						lastAlarms[key] = cur
					}
				}
			}
			saveBMSClosedBuckets(pg, acc.closed(time.Now()))
		case <-ctx.Done():
			// Неполный 5-минутный промежуток в PG не пишем (там только полные
			// бакеты); в Redis он не нужен — каждое показание уже записано живым
			// опросом (saveBMSReading). main() ждёт завершение этой горутины (bgWg)
			// ДО закрытия пулов Redis/PG.
			return
		}
	}
}

// saveBMSReading пишет МГНОВЕННОЕ (одно снятое) показание ANT BMS в Redis-ряд
// (month ZSET, score = секунда, samples=1). 5-минутные средние для PG считает
// bms_accumulator.go; в Redis-ряду средних больше нет — только сырые показания.
func saveBMSReading(store *redisStore, d bmsDevice, ts time.Time) {
	// Устройства без DeviceName (пустая запись) в ряд не пишем — их же
	// пропускает и аккумулятор (bmsAccumulator.add).
	if d.DeviceName == "" {
		return
	}
	sp := bmsSeriesPoint{Name: bmsKey(d), Display: d.DeviceName, Ts: ts.Format(time.RFC3339)}
	sp.bmsAveraged = bmsAveraged{
		CurrentA:     d.CurrentA,
		PowerW:       d.PowerW,
		Soc:          float64(d.Soc),
		CapacityAh:   d.CapacityAh,
		RemainingAh:  d.RemainingAh,
		MaxCellV:     d.MaxCellV,
		MinCellV:     d.MinCellV,
		AvgCellV:     d.AvgCellV,
		CellsV:       d.CellsV,
		Temperatures: d.TemperaturesC,
		CellCount:    d.CellCount,
		ChargeMos:    d.ChargeMos,
		DischargeMos: d.DischargeMos,
		Balancer:     d.Balancer,
		Frames:       d.Frames,
		Samples:      1,
	}
	if err := store.SaveBMSSeries(sp, ts); err != nil {
		log.Printf("bms redis series %s: %v", bmsKey(d), err)
	}
}

// bmsEmptyTolerance — сколько подряд валидно-пустых ответов (updated>0,
// devices=[]) требуется, чтобы почистить дашборд BMS (см. pollAndSaveBMS).
var bmsEmptyTolerance = 3

// bmsEmptyStreak — счётчик подряд идущих валидно-пустых ответов. Гвард
// устойчивости (K1): одиночная/короткая пустота (перезапуск bmslistener,
// временный сбой shm) не должна стирать весь дашборд батарей.
var bmsEmptyStreak int

// saveBMSClosedBuckets пишет готовые 5-минутные усреднённые точки BMS в PG
// (sunreceiver.bms_averages, вечно) — гранулярность PG = 1 запись / 5 минут.
// В Redis-ряд 5-минутные средние НЕ пишутся: там лежат сырые показания, которые
// пулер пишет сам (saveBMSReading).
func saveBMSClosedBuckets(pg *pgStore, pts []bmsAvgPoint) {
	if pg == nil {
		return
	}
	type row struct {
		name  string
		start time.Time
		avg   bmsAveraged
	}
	pgRows := make([]row, 0, len(pts))
	for _, p := range pts {
		if p.avg.Samples == 0 {
			continue
		}
		pgRows = append(pgRows, row{name: p.name, start: p.start, avg: p.avg})
	}
	if len(pgRows) == 0 {
		return
	}
	// Набор точек одного вызова пишем ОДНОЙ транзакцией с ограниченным retry:
	// кратковременный сбой PG не должен оставлять «дыру» в 5-минутном ряде BMS
	// (для BMS история восстанавливается только живым опросом — потеря необратима).
	if err := retryPg(func() error {
		return pg.withTx(func(q pgExecer) error {
			for _, r := range pgRows {
				if err := insertBMSAveragedExec(q, pg.ctx, r.name, r.start, r.avg); err != nil {
					return err
				}
			}
			return nil
		})
	}, 3); err != nil {
		log.Printf("bms avg pg: %v", err)
	}
}

// recomputeMinMaxCells пересчитывает индексы и напряжения max/min ячеек из
// фактического массива ячеек (cells_v), а не из меток, заявленных BMS в кадре
// (f[115]/f[118]): встроенная метка может не совпадать с реальными напряжениями
// (в т.ч. при нескольких ячейках с одинаковым напряжением BMS «прыгает» между
// ними), из-за чего красная/синяя ячейка на дашборде не соответствует фактически
// минимальной/максимальной. При равенстве берётся первая (наименьший индекс) —
// подсветка остаётся стабильной. Индексы 1-based, как в кадре и на дашборде.
func recomputeMinMaxCells(d *bmsDevice) {
	if len(d.CellsV) == 0 {
		return
	}
	maxIdx, minIdx := 0, 0
	for i := 1; i < len(d.CellsV); i++ {
		if d.CellsV[i] > d.CellsV[maxIdx] {
			maxIdx = i
		}
		if d.CellsV[i] < d.CellsV[minIdx] {
			minIdx = i
		}
	}
	d.MaxCellIdx = maxIdx + 1
	d.MinCellIdx = minIdx + 1
	d.MaxCellV = d.CellsV[maxIdx]
	d.MinCellV = d.CellsV[minIdx]
}

// pollAndSaveBMS делает один запрос read_bms.php и обновляет коллекцию в
// Redis: устройство появляется/исчезает с дашборда по факту наличия в ответе
// (как MPPT). При ошибке запроса предыдущее состояние в Redis сохраняется
// (возвращается nil). Валидно-пустой ответ (updated>0, devices=[]) чистит
// дашборд только при устойчивой пустоте (bmsEmptyTolerance подряд) — одиночный
// всплеск пустоты (перезапуск bmslistener, сбой shm) коллекцию не трогает.
// decodeAntAlarms расшифровывает коды состояния MOSFET и балансировки ANT BMS
// (протокол 0x55) в список активных алармов/защит. Коды 0/1 — норма (выкл/вкл).
func decodeAntAlarms(d bmsDevice) []string {
	charge := map[int]string{
		2: "Overvoltage protection", 3: "Over current protection", 5: "Total overpressure",
		6: "Battery overtemperature", 7: "Power overtemperature", 8: "Abnormal current",
		9: "Balanced line dropped", 10: "Motherboard overtemperature", 13: "Discharge tube abnormality",
	}
	disch := map[int]string{
		2: "Over-discharge protection", 3: "Over current protection", 5: "Total undervoltage",
		6: "Battery overtemperature", 7: "Power overtemperature", 8: "Abnormal current",
		9: "Balanced line dropped", 10: "Motherboard overtemperature", 12: "Short circuit protection",
		13: "Discharge tube abnormality", 14: "Start exception",
	}
	bal := map[int]string{3: "Balance overtemperature", 10: "Motherboard overtemperature"}
	var out []string
	if s, ok := charge[d.ChargeMos]; ok {
		out = append(out, "Charge: "+s)
	}
	if s, ok := disch[d.DischargeMos]; ok {
		out = append(out, "Discharge: "+s)
	}
	if s, ok := bal[d.Balancer]; ok {
		out = append(out, "Balance: "+s)
	}
	return out
}

func pollAndSaveBMS(ctx context.Context, store *redisStore) *bmsCollection {
	col, err := bmsSite.fetch(ctx)
	if err != nil {
		log.Printf("bms api: %v", err)
		return nil
	}
	// read_bms.php при сбое чтения shm отдаёт {"updated":0,"devices":[]} (HTTP 200).
	// bmslistener никогда не публикует updated=0 — это маркер сбоя: коллекцию в
	// Redis НЕ трогаем (иначе одиночная shm-гонка вычистит весь дашборд BMS).
	// Возврат nil — аккумулятор (acc.add) по nil пропустит, в него уходят только
	// валидные устройства.
	if col.Updated == 0 {
		// Сбойный ответ разрывает серию валидно-пустых: «пусто, пусто, сбой, пусто»
		// не должно считаться тремя подряд пустыми и чистить дашборд.
		bmsEmptyStreak = 0
		log.Printf("bms api: сбойный ответ (updated=0) — коллекция не трогается")
		return nil
	}
	// Валидно-ПУСТАЯ коллекция (updated>0, devices=[]) — не обязательно «0 батарей»:
	// bmslistener может временно публиковать пустоту (перезапуск, сбой shm). Чтобы
	// одиночная/короткая пустота не стирала весь дашборд, требуем bmsEmptyTolerance
	// подряд идущих пустых ответов, прежде чем чистить. Счётчик сбрасывается при
	// любом ответе с хотя бы одним устройством.
	if len(col.Devices) == 0 {
		bmsEmptyStreak++
		if bmsEmptyStreak < bmsEmptyTolerance {
			log.Printf("bms api: пустая коллекция (%d/%d) — коллекция не трогается",
				bmsEmptyStreak, bmsEmptyTolerance)
			return col
		}
	} else {
		bmsEmptyStreak = 0
	}
	// Индексы/напряжения max/min ячеек пересчитываем из фактических cells_v —
	// метка из кадра BMS (f[115]/f[118]) может не соответствовать реальным
	// напряжениям (см. recomputeMinMaxCells).
	for i := range col.Devices {
		recomputeMinMaxCells(&col.Devices[i])
		col.Devices[i].Alarms = decodeAntAlarms(col.Devices[i])
	}
	// Проверяем коллизию имён в пределах коллекции: если два устройства имеют
	// одинаковый DeviceName, их ключи разводятся по USB-порту (resolveBMSKey).
	// Это сохраняет непрерывность исторических рядов для уникальных имён.
	nameCount := make(map[string]int, len(col.Devices))
	for i := range col.Devices {
		nameCount[col.Devices[i].DeviceName]++
	}
	active := make(map[string]string, len(col.Devices))
	for i := range col.Devices {
		d := col.Devices[i]
		if d.DeviceName == "" {
			continue
		}
		col.Devices[i].Key = resolveBMSKey(d, nameCount)
		b, err := json.Marshal(col.Devices[i])
		if err != nil {
			log.Printf("bms: marshal %s: %v", d.DeviceName, err)
			continue
		}
		active[col.Devices[i].Key] = string(b)
	}
	if err := store.SetBMS(active); err != nil {
		log.Printf("bms redis: %v", err)
	}
	return col
}

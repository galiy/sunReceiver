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
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"time"

	"github.com/galiy/sunReceiver/internal/bmscontract"
)

// Пулер BMS через новый HTTP-API ПАК «Малина26» (BMS-Receiver). ANT BMS и EnBMS
// переехали в единый тип: контракт телеметрии один (бинарный, 107 Б, big-endian),
// а тип BMS (`bms_type` из списка) нужен только для раскладки тревог/ошибок.
//
// Ручки (см. cleaning.malina docs/bms-reader.md, docs/bms-reader-arch.md §12):
//   - GET /api/bms/devices → [{id, gen, serial, base, bms_type}];
//   - GET /api/bms/raw?id=&gen= → бинарный контракт 107 Б (per slot);
//     несовпадение id+gen или пустой слот → 404.
//
// Хранение обособлено: Redis current/series (sunreceiver:bms26:*) и PG
// sunreceiver.bms26_averages.

// bms26Source — источник BMS: HTTP-клиент «Малина26» (token/Bearer/401). Клиент
// переиспользует тот же источник, что и method=malina26 раздела map (общий
// host/port/password и токен) — отдельного раздела в конфиге у BMS нет.
type bms26Source struct {
	http         *malina26Source
	pollInterval time.Duration
}

// bms26 — глобальный источник BMS; nil — опрос отключён (нет method=malina26).
var bms26 *bms26Source

// newBms26Source создаёт источник BMS на общем HTTP-клиенте «Малина26».
// Вызывается только когда method=malina26 активен (источник malina26 != nil).
func newBms26Source(httpSrc *malina26Source) *bms26Source {
	if httpSrc == nil {
		return nil
	}
	return &bms26Source{http: httpSrc, pollInterval: time.Second}
}

// bms26Device — запись списка /api/bms/devices.
type bms26Device struct {
	ID      int    `json:"id"`
	Gen     uint64 `json:"gen"`
	Serial  string `json:"serial"`
	Base    string `json:"base"`
	BmsType string `json:"bms_type"`
}

// bms26Snapshot — текущий снимок одной BMS (JSON в Redis current и PG values).
// Имена полей совместимы с прежним ANT-снимком (cells_v, current_a, soc, …),
// чтобы страница BMS/графики переиспользовались на новом ключе.
type bms26Snapshot struct {
	Serial        string    `json:"serial"`
	Base          string    `json:"base"`
	BmsType       string    `json:"bms_type"`
	Timestamp     string    `json:"timestamp"`
	DeviceName    string    `json:"deviceName"`
	CellCount     int       `json:"cell_count"`
	TempCount     int       `json:"temp_count"`
	Flags         int       `json:"flags"`
	CellsV        []float64 `json:"cells_v"`
	TemperaturesC []float64 `json:"temperatures_c"`
	CurrentA      float64   `json:"current_a"`
	PowerW        float64   `json:"power_w"`
	Soc           float64   `json:"soc"`
	Soh           float64   `json:"soh"`
	CapacityAh    float64   `json:"capacity_ah"`
	RemainingAh   float64   `json:"remaining_ah"`
	RatedAh       float64   `json:"rated_ah"`
	Cycles        int       `json:"cycles"`
	TotalV        float64   `json:"total_voltage_v"`
	PortV         float64   `json:"port_voltage_v"`
	BalanceMask   uint32    `json:"balance_mask"`
	CellWarnMask  uint32    `json:"cell_warn_mask,omitempty"` // бит i: предупреждение ячейки i+1 (EnBMS)
	CellProtMask  uint32    `json:"cell_prot_mask,omitempty"` // бит i: защита ячейки i+1 (EnBMS)
	TempWarnMask  uint32    `json:"temp_warn_mask,omitempty"` // бит i: предупреждение датчика i+1 (EnBMS)
	TempProtMask  uint32    `json:"temp_prot_mask,omitempty"` // бит i: защита датчика i+1 (EnBMS)
	Mode          int       `json:"mode"`                     // канонический код (0..5)
	AlarmBytes    []int     `json:"alarm_bytes"`              // 19 Б битфилда (для трактовки по типу)
	Alarms        []string  `json:"alarms,omitempty"`         // человекочитаемые тревоги (по bms_type)
	Protections   []string  `json:"protections,omitempty"`    // защиты (по подтипу; EnBMS)
	// Производные по ячейкам (вычисляются, в контракте не хранятся).
	MaxCellIdx int     `json:"max_cell_idx"`
	MaxCellV   float64 `json:"max_cell_v"`
	MinCellIdx int     `json:"min_cell_idx"`
	MinCellV   float64 `json:"min_cell_v"`
	AvgCellV   float64 `json:"avg_cell_v"`
}

// decodeBms26Contract разбирает бинарный контракт BMS (107 Б, big-endian) через
// локальную копию пакета bmscontract (internal/bmscontract; оригинал —
// github.com/galiy/malina-26/daemons/lib/bmscontract, см. cleaning.malina/docs/bms-reader.md §1).
// Знак тока: разряд «+», заряд «−». Значения, которых нет у типа, — 0. Оффсеты живут
// в пакете контракта; при V2/V3 обновляется КОПИЯ пакета (оффсеты здесь не правим).
func decodeBms26Contract(b []byte) (bms26Snapshot, bool) {
	t, err := bmscontract.Decode(b)
	if err != nil {
		return bms26Snapshot{}, false
	}
	var s bms26Snapshot
	cc := int(t.CellCount)
	if cc > bmscontract.MaxCells {
		cc = bmscontract.MaxCells
	}
	tc := int(t.TempCount)
	if tc > bmscontract.MaxTemps {
		tc = bmscontract.MaxTemps
	}
	s.CellCount, s.TempCount = cc, tc
	s.Flags = int(t.Flags)

	s.CellsV = make([]float64, 0, cc)
	for i := 0; i < cc; i++ {
		s.CellsV = append(s.CellsV, float64(t.CellsMV[i])/1000.0)
	}
	s.TemperaturesC = make([]float64, 0, tc)
	for i := 0; i < tc; i++ {
		s.TemperaturesC = append(s.TemperaturesC, float64(t.TempsDC[i])/10.0)
	}
	s.CurrentA = float64(t.CurrentDA) / 100.0
	s.PowerW = float64(t.PowerW)
	s.Soc = float64(t.SocDP) / 10.0
	s.Soh = float64(t.SohDP) / 10.0
	s.CapacityAh = float64(t.CapMAh) / 1000.0
	s.RemainingAh = float64(t.RemainingMAh) / 1000.0
	s.RatedAh = float64(t.RatedMAh) / 1000.0
	s.Cycles = int(t.Cycles)
	s.TotalV = float64(t.TotalCV) / 100.0
	s.PortV = float64(t.PortCV) / 100.0
	s.BalanceMask = t.BalanceMask
	s.Mode = int(t.Mode & 0x00FF) // младший байт — канонический код
	s.AlarmBytes = make([]int, bmscontract.AlarmLen)
	for i := 0; i < bmscontract.AlarmLen; i++ {
		s.AlarmBytes[i] = int(t.Alarm[i])
	}
	// Производные по ячейкам (как ANT-пулер: пересчёт из фактических напряжений).
	if cc > 0 {
		maxIdx, minIdx := 0, 0
		sum := 0.0
		for i := 0; i < cc; i++ {
			sum += s.CellsV[i]
			if s.CellsV[i] > s.CellsV[maxIdx] {
				maxIdx = i
			}
			if s.CellsV[i] < s.CellsV[minIdx] {
				minIdx = i
			}
		}
		s.MaxCellIdx, s.MinCellIdx = maxIdx+1, minIdx+1
		s.MaxCellV, s.MinCellV = s.CellsV[maxIdx], s.CellsV[minIdx]
		s.AvgCellV = sum / float64(cc)
	}
	return s, true
}

// fetchBmsDevices читает список BMS через /api/bms/devices.
func (s *bms26Source) fetchBmsDevices(ctx context.Context) ([]bms26Device, error) {
	code, b, err := s.http.get(ctx, "/api/bms/devices")
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, fmt.Errorf("bms26 devices: status %d", code)
	}
	var out []bms26Device
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("bms26 devices: json: %w", err)
	}
	return out, nil
}

// fetchBmsRaw читает контракт слота; nil,nil — слот потерян (404).
func (s *bms26Source) fetchBmsRaw(ctx context.Context, id int, gen uint64) ([]byte, error) {
	code, b, err := s.http.get(ctx, fmt.Sprintf("/api/bms/raw?id=%d&gen=%d", id, gen))
	if err != nil {
		return nil, err
	}
	if code == http.StatusNotFound {
		return nil, nil
	}
	if code != http.StatusOK {
		return nil, fmt.Errorf("bms26 raw id=%d: status %d", id, code)
	}
	return b, nil
}

// runBms26Poll — отдельный цикл опроса BMS (malina26). Пишет текущий снимок в Redis
// (HASH sunreceiver:bms26:current, поле = serial), каждое снятое показание — в
// месячный ряд (sunreceiver:bms26:series:<YYYY-MM>), а 5-минутные средние — в PG
// (sunreceiver.bms26_averages, аккумулятор в памяти процесса).
func runBms26Poll(store *redisStore, pg *pgStore, ctx context.Context) {
	acc := newBms26Accumulator()
	lastAlarms := map[string]map[string]bool{} // serial -> активные тревоги (для истории)
	ticker := time.NewTicker(bms26.pollInterval)
	defer ticker.Stop()
	log.Printf("bms26: опрос через %s (период %v, Redis current %s, PG %v)",
		bms26.http.baseURL, bms26.pollInterval, redisBms26CurrentKey, pg != nil)
	for {
		select {
		case <-ticker.C:
			snaps := pollBms26Once(ctx)
			if len(snaps) > 0 {
				cur := make(map[string]string, len(snaps))
				now := time.Now()
				for _, sn := range snaps {
					b, err := json.Marshal(sn)
					if err != nil {
						continue
					}
					cur[sn.Serial] = string(b)
					if err := store.SaveBms26Series(sn, now); err != nil {
						log.Printf("bms26 redis series %s: %v", sn.Serial, err)
					}
					acc.add(sn, now)
					// История ошибок: пишем только ПОЯВЛЕНИЕ новой тревоги/защиты.
					if pg != nil {
						curAlarms := map[string]bool{}
						for _, al := range sn.Alarms {
							curAlarms[al] = true
						}
						for _, pr := range sn.Protections {
							curAlarms[pr] = true
						}
						prev := lastAlarms[sn.Serial]
						for al := range curAlarms {
							if prev == nil || !prev[al] {
								if err := pg.InsertDeviceError(sn.Serial, "bms", al, al, now); err != nil {
									log.Printf("bms26 error pg %s: %v", sn.Serial, err)
								}
							}
						}
						lastAlarms[sn.Serial] = curAlarms
					}
				}
				if err := store.SaveBms26Current(cur); err != nil {
					log.Printf("bms26 redis current: %v", err)
				}
			}
			saveBms26ClosedBuckets(pg, acc.closed(time.Now()))
		case <-ctx.Done():
			return
		}
	}
}

// pollBms26Once опрашивает все BMS последовательно и возвращает снимки. При ошибке
// списка — nil. Слот, потерянный в 404, пропускается (следующий цикл повторит).
func pollBms26Once(ctx context.Context) []bms26Snapshot {
	devs, err := bms26.fetchBmsDevices(ctx)
	if err != nil {
		log.Printf("bms26: %v", err)
		return nil
	}
	out := make([]bms26Snapshot, 0, len(devs))
	now := time.Now()
	for _, d := range devs {
		raw, err := bms26.fetchBmsRaw(ctx, d.ID, d.Gen)
		if err != nil {
			log.Printf("bms26 id=%d: %v", d.ID, err)
			continue
		}
		if raw == nil {
			continue // слот потерян
		}
		sn, ok := decodeBms26Contract(raw)
		if !ok {
			log.Printf("bms26 id=%d: битый payload (len=%d)", d.ID, len(raw))
			continue
		}
		sn.Serial = d.Serial
		sn.Base = d.Base
		sn.BmsType = d.BmsType
		sn.DeviceName = d.Serial
		sn.Timestamp = now.Format(time.RFC3339)
		finalizeBms26(&sn)
		cacheBms26Snapshot(sn) // костыль: кэш serial для старых пулеров
		out = append(out, sn)
	}
	return out
}

// bms26ModeName — канонический код режима (bms-reader.md §1) → текст.
func bms26ModeName(code int) string {
	switch code {
	case 0:
		return "ожидание"
	case 1:
		return "разряд"
	case 2:
		return "заряд"
	case 3:
		return "поддержание"
	case 4:
		return "полный заряд"
	case 5:
		return "выключение"
	default:
		return ""
	}
}

// bmsDeviceFromBms26 приводит снимок BMS malina26 к форме bmsDevice (единый
// рендер страницы BMS и батареек на главной). flags: b0 charge_mos, b1
// discharge_mos, b2 balancer (bmscontract).
func bmsDeviceFromBms26(sn bms26Snapshot) bmsDevice {
	d := bmsDevice{
		Kind:          "bms26",
		DeviceName:    sn.Serial,
		Key:           sn.Serial,
		Port:          sn.BmsType,
		CellCount:     sn.CellCount,
		CellsV:        sn.CellsV,
		TotalV:        sn.TotalV,
		PortV:         sn.PortV,
		CurrentA:      sn.CurrentA,
		Soc:           int(math.Round(sn.Soc)),
		CapacityAh:    sn.CapacityAh,
		RemainingAh:   sn.RemainingAh,
		TemperaturesC: sn.TemperaturesC,
		PowerW:        sn.PowerW,
		ChargeMos:     boolBit(sn.Flags, 0),
		DischargeMos:  boolBit(sn.Flags, 1),
		Balancer:      boolBit(sn.Flags, 2),
		MaxCellIdx:    sn.MaxCellIdx,
		MaxCellV:      sn.MaxCellV,
		MinCellIdx:    sn.MinCellIdx,
		MinCellV:      sn.MinCellV,
		AvgCellV:      sn.AvgCellV,
		Soh:           sn.Soh,
		Cycles:        sn.Cycles,
		RatedAh:       sn.RatedAh,
		BalanceMask:   sn.BalanceMask,
		CellWarnMask:  sn.CellWarnMask,
		CellProtMask:  sn.CellProtMask,
		TempWarnMask:  sn.TempWarnMask,
		TempProtMask:  sn.TempProtMask,
		Model:         sn.BmsType,
		Mode:          bms26ModeName(sn.Mode),
		Alarms:        sn.Alarms,
		Protections:   sn.Protections,
	}
	if ts, err := time.Parse(time.RFC3339, sn.Timestamp); err == nil {
		d.Timestamp = ts.Unix()
		d.Time = ts.Format("15:04:05")
	}
	return d
}

// boolBit возвращает 1/0 по биту n.
func boolBit(v, n int) int {
	if v&(1<<uint(n)) != 0 {
		return 1
	}
	return 0
}

// bmsSeriesPointFromBms26 приводит снимок BMS malina26 к форме точки ряда ANT BMS.
func bmsSeriesPointFromBms26(sn bms26Snapshot) bmsSeriesPoint {
	return bmsSeriesPoint{
		Name:    sn.Serial,
		Display: sn.Serial,
		Ts:      sn.Timestamp,
		bmsAveraged: bmsAveraged{
			CurrentA:     sn.CurrentA,
			PowerW:       sn.PowerW,
			Soc:          sn.Soc,
			CapacityAh:   sn.CapacityAh,
			RemainingAh:  sn.RemainingAh,
			MaxCellV:     sn.MaxCellV,
			MinCellV:     sn.MinCellV,
			AvgCellV:     sn.AvgCellV,
			CellsV:       sn.CellsV,
			Temperatures: sn.TemperaturesC,
			CellCount:    sn.CellCount,
			Samples:      1,
		},
	}
}

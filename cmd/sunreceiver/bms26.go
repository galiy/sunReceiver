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
	"sort"
	"sync"
	"time"

	"github.com/galiy/sunReceiver/internal/bmscontract"
)

// Порог устаревания источника: возраст last_seen из /api/bms/devices больше
// этого значения → снимок помечается stale (не выдаётся в UI как актуальный).
// 10 с при периоде опроса 1 с — запас на сетевую/планировочную дрожь.
const bms26StaleAfter = 10 * time.Second

const (
	// bms26WarnCooldown — минимальный интервал между однотипными предупреждениями
	// (дубли serial, 404/потеря слота, stale, ошибки HTTP/декодирования). Лог не
	// должен расти с частотой опроса (1 Гц).
	bms26WarnCooldown = 60 * time.Second
	// Backoff при 5xx/сети/таймаутах: 1, 2, 4, … до 30 с; сбрасывается первым
	// успешным ответом. Отдельно для списка устройств и для каждого raw-слота.
	bms26BackoffBase = time.Second
	bms26BackoffMax  = 30 * time.Second
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
// sunreceiver.bms26_averages_r.

// bms26Source — источник BMS: HTTP-клиент «Малина26» (token/Bearer/401). Клиент
// переиспользует тот же источник, что и МАП/КЭС раздела map (общий
// host/port/password и токен) — отдельного раздела в конфиге у BMS нет.
type bms26Source struct {
	http         *malina26Source
	pollInterval time.Duration
}

// bms26 — глобальный источник BMS; nil — опрос отключён (раздел map выключен).
var bms26 *bms26Source

// newBms26Source создаёт источник BMS на общем HTTP-клиенте «Малина26».
// Вызывается только когда активен источник malina26 (раздел map включён).
func newBms26Source(httpSrc *malina26Source) *bms26Source {
	if httpSrc == nil {
		return nil
	}
	return &bms26Source{http: httpSrc, pollInterval: time.Second}
}

// bms26Device — запись списка /api/bms/devices (первоисточник —
// cleaning.malina/web/internal/web/bmsapi.go).
type bms26Device struct {
	Type         string `json:"type"`
	ID           int    `json:"id"`
	Gen          uint64 `json:"gen"`
	Serial       string `json:"serial"`
	Base         string `json:"base"`
	BmsType      string `json:"bms_type"`
	BmsTypeLabel string `json:"bms_type_label"`
	Source       string `json:"source"`
	Framing      int    `json:"framing"`
	LastSeen     int64  `json:"last_seen"`
}

// bms26Snapshot — текущий снимок одной BMS (JSON в Redis current и PG values).
// Имена полей совместимы с прежним ANT-снимком (cells_v, current_a, soc, …),
// чтобы страница BMS/графики переиспользовались на новом ключе.
type bms26Snapshot struct {
	Serial    string `json:"serial"`
	Base      string `json:"base"`
	BmsType   string `json:"bms_type"`
	Timestamp string `json:"timestamp"`
	// SourceIDs — все id записей /api/bms/devices с этим serial (при дубле — их
	// несколько); SourceID — id устройства, из которого получен ЭТОТ снимок.
	// В current при дубле попадает снимок с минимальным SourceID (R1), а в
	// series/агрегат — все устройства (принятый риск смешения общего ряда).
	SourceIDs []int `json:"source_ids,omitempty"`
	SourceID  int   `json:"source_id,omitempty"`
	// SourceLastSeen — last_seen источника (unix seconds) на момент снимка;
	// НЕ путать с Timestamp (время опроса sunReceiver).
	SourceLastSeen int64 `json:"source_last_seen,omitempty"`
	// Stale — возраст источника (now - last_seen) больше bms26StaleAfter.
	// Stale-снимок остаётся в current для индикации, но не пишется в ряд/средние.
	Stale bool `json:"stale,omitempty"`
	// DupCount — сколько устройств из списка делят этот serial (>1 — конфликт).
	DupCount      int       `json:"dup_serial_count,omitempty"`
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

// bms26API — минимальный контракт источника BMS для пулера (тестируемость).
// *bms26Source реализует его; тесты подставляют фейк без HTTP.
type bms26API interface {
	fetchBmsDevices(ctx context.Context) ([]bms26Device, error)
	fetchBmsRaw(ctx context.Context, id int, gen uint64) ([]byte, error)
}

// bms26Stats — счётчики наблюдаемости (R6). devices/dupSerial/stale — состояние
// последнего опроса; notFound/decodeErr/httpErr — накопленные события.
type bms26Stats struct {
	mu        sync.Mutex
	devices   int
	dupSerial int
	stale     int
	notFound  int64
	decodeErr int64
	httpErr   int64
	lastPoll  time.Time
}

type bms26StatsView struct {
	Devices    int    `json:"devices"`
	DupSerial  int    `json:"dup_serial"`
	Stale      int    `json:"stale"`
	NotFound   int64  `json:"404"`
	DecodeErr  int64  `json:"decode_err"`
	HTTPErr    int64  `json:"http_err"`
	LastPoll   string `json:"last_poll,omitempty"`
	StaleAfter int    `json:"stale_after_sec"`
}

// bms26Metrics — метрики единственного боевого пулера; читаются /api/bms/health.
var bms26Metrics = &bms26Stats{}

func (s *bms26Stats) setPoll(devices, dup, stale int, now time.Time) {
	s.mu.Lock()
	s.devices, s.dupSerial, s.stale = devices, dup, stale
	s.lastPoll = now
	s.mu.Unlock()
}

func (s *bms26Stats) incNotFound() {
	s.mu.Lock()
	s.notFound++
	s.mu.Unlock()
}

func (s *bms26Stats) incDecodeErr() {
	s.mu.Lock()
	s.decodeErr++
	s.mu.Unlock()
}

func (s *bms26Stats) incHTTPErr() {
	s.mu.Lock()
	s.httpErr++
	s.mu.Unlock()
}

func (s *bms26Stats) view() bms26StatsView {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := bms26StatsView{
		Devices: s.devices, DupSerial: s.dupSerial, Stale: s.stale,
		NotFound: s.notFound, DecodeErr: s.decodeErr, HTTPErr: s.httpErr,
		StaleAfter: int(bms26StaleAfter / time.Second),
	}
	if !s.lastPoll.IsZero() {
		v.LastPoll = s.lastPoll.Format(time.RFC3339)
	}
	return v
}

// bms26WarnLimiter — предупреждения с дедупликацией по ключу и cooldown.
// Лог не растёт на стабильном состоянии: повтор в пределах cooldown подавляется.
type bms26WarnLimiter struct {
	mu       sync.Mutex
	cooldown time.Duration
	now      func() time.Time
	sink     func(string)
	last     map[string]time.Time
}

func newBms26WarnLimiter(now func() time.Time) *bms26WarnLimiter {
	if now == nil {
		now = time.Now
	}
	return &bms26WarnLimiter{
		cooldown: bms26WarnCooldown,
		now:      now,
		sink:     func(msg string) { log.Print(msg) },
		last:     map[string]time.Time{},
	}
}

// warn пишет сообщение, если по ключу не было предупреждения дольше cooldown.
func (w *bms26WarnLimiter) warn(key, msg string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	now := w.now()
	if t, ok := w.last[key]; ok && now.Sub(t) < w.cooldown {
		return false
	}
	w.last[key] = now
	w.sink(msg)
	return true
}

// bms26DevFail — состояние backoff отдельного raw-слота.
type bms26DevFail struct {
	until   time.Time
	backoff time.Duration
}

// bms26Poller — состояние опроса: источник, часы, счётчики, backoff и limiter.
type bms26Poller struct {
	api        bms26API
	now        func() time.Time
	staleAfter time.Duration
	stats      *bms26Stats
	limiter    *bms26WarnLimiter

	listFail    int
	listRetryAt time.Time
	listBackoff time.Duration
	devFail     map[int]*bms26DevFail
}

func newBms26Poller(api bms26API, stats *bms26Stats) *bms26Poller {
	if stats == nil {
		stats = &bms26Stats{}
	}
	now := time.Now
	return &bms26Poller{
		api:        api,
		now:        now,
		staleAfter: bms26StaleAfter,
		stats:      stats,
		limiter:    newBms26WarnLimiter(now),
		devFail:    map[int]*bms26DevFail{},
	}
}

func bms26NextBackoff(cur time.Duration) time.Duration {
	if cur <= 0 {
		return bms26BackoffBase
	}
	cur *= 2
	if cur > bms26BackoffMax {
		return bms26BackoffMax
	}
	return cur
}

// poll опрашивает список и все слоты. Возвращает снимки (включая stale);
// при ошибке списка или активном backoff — nil. 404/битый payload — пропуск
// слота без спама; 5xx/сеть/таймаут — backoff и счётчик http_err.
func (p *bms26Poller) poll(ctx context.Context) []bms26Snapshot {
	if ctx.Err() != nil {
		return nil
	}
	now := p.now()
	if !p.listRetryAt.IsZero() && now.Before(p.listRetryAt) {
		return nil // активен backoff после ошибки списка
	}
	devs, err := p.api.fetchBmsDevices(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		p.stats.incHTTPErr()
		p.listFail++
		p.listBackoff = bms26NextBackoff(p.listBackoff)
		p.listRetryAt = now.Add(p.listBackoff)
		p.limiter.warn("list", fmt.Sprintf("bms26: список устройств: %v (backoff %v)", err, p.listBackoff))
		return nil
	}
	p.listFail = 0
	p.listBackoff = 0
	p.listRetryAt = time.Time{}

	idsBySerial := bms26SerialIDs(devs)
	dupExtra := 0
	for serial, ids := range idsBySerial {
		if len(ids) > 1 {
			dupExtra += len(ids) - 1
			p.limiter.warn("dup:"+serial, fmt.Sprintf(
				"bms26: дубль serial %q: id=%v — current выбирает id=%d (детерминированно), series/средние смешиваются (принятый риск)",
				serial, ids, ids[0]))
		}
	}

	out := make([]bms26Snapshot, 0, len(devs))
	staleN := 0
	for _, d := range devs {
		if f := p.devFail[d.ID]; f != nil && now.Before(f.until) {
			continue // активен backoff этого слота
		}
		raw, err := p.api.fetchBmsRaw(ctx, d.ID, d.Gen)
		if err != nil {
			if ctx.Err() != nil {
				return out
			}
			p.stats.incHTTPErr()
			f := p.devFail[d.ID]
			if f == nil {
				f = &bms26DevFail{}
				p.devFail[d.ID] = f
			}
			f.backoff = bms26NextBackoff(f.backoff)
			f.until = now.Add(f.backoff)
			p.limiter.warn(fmt.Sprintf("raw:%d", d.ID), fmt.Sprintf(
				"bms26 id=%d serial=%q: raw: %v (backoff %v)", d.ID, d.Serial, err, f.backoff))
			continue
		}
		delete(p.devFail, d.ID)
		if raw == nil {
			// 404/gen mismatch — слот потерян; список перечитается на следующем цикле.
			p.stats.incNotFound()
			p.limiter.warn(fmt.Sprintf("404:%d", d.ID), fmt.Sprintf(
				"bms26 id=%d serial=%q: слот потерян (404/gen mismatch) — пропуск", d.ID, d.Serial))
			continue
		}
		if len(raw) != bmscontract.Size {
			p.stats.incDecodeErr()
			p.limiter.warn(fmt.Sprintf("decode:%d", d.ID), fmt.Sprintf(
				"bms26 id=%d serial=%q: битый payload (len=%d, ожидается %d) — пропуск",
				d.ID, d.Serial, len(raw), bmscontract.Size))
			continue
		}
		sn, ok := decodeBms26Contract(raw)
		if !ok {
			p.stats.incDecodeErr()
			p.limiter.warn(fmt.Sprintf("decode:%d", d.ID), fmt.Sprintf(
				"bms26 id=%d serial=%q: ошибка Decode — пропуск", d.ID, d.Serial))
			continue
		}
		sn.Serial = d.Serial
		sn.Base = d.Base
		sn.BmsType = d.BmsType
		sn.DeviceName = d.Serial
		sn.SourceIDs = append([]int(nil), idsBySerial[d.Serial]...)
		sn.SourceID = d.ID
		sn.DupCount = len(idsBySerial[d.Serial])
		sn.Timestamp = now.Format(time.RFC3339)
		sn.SourceLastSeen = d.LastSeen
		if d.LastSeen > 0 {
			age := now.Unix() - d.LastSeen
			if age > int64(p.staleAfter/time.Second) {
				sn.Stale = true
			}
		}
		if sn.Stale {
			staleN++
			p.limiter.warn("stale:"+sn.Serial, fmt.Sprintf(
				"bms26 serial=%q: данные источника устарели (last_seen=%s, возраст %s) — снимок помечен stale",
				sn.Serial, time.Unix(d.LastSeen, 0).Format(time.RFC3339), time.Duration(now.Unix()-d.LastSeen)*time.Second))
		}
		finalizeBms26(&sn)
		out = append(out, sn)
	}
	p.stats.setPoll(len(devs), dupExtra, staleN, now)
	return out
}

// bms26SerialIDs возвращает serial → отсортированные id устройств списка.
// Одинаковый serial у нескольких устройств — конфликт (R1): все id видны в
// снимке (SourceIDs), current выбирает минимальный.
func bms26SerialIDs(devs []bms26Device) map[string][]int {
	m := make(map[string][]int, len(devs))
	for _, d := range devs {
		if d.Serial != "" {
			m[d.Serial] = append(m[d.Serial], d.ID)
		}
	}
	for serial := range m {
		sort.Ints(m[serial])
	}
	return m
}

// bms26SelectCurrent детерминированно выбирает по одному снимку на serial для
// Redis current: минимальный id источника (R1). Остальные снимки не теряются —
// они уже записаны в series/средние, конфликт виден в логе и DupCount.
func bms26SelectCurrent(snaps []bms26Snapshot) map[string]bms26Snapshot {
	out := make(map[string]bms26Snapshot, len(snaps))
	for _, sn := range snaps {
		if sn.Serial == "" {
			continue
		}
		prev, ok := out[sn.Serial]
		if !ok || bms26FirstSourceID(sn) < bms26FirstSourceID(prev) {
			out[sn.Serial] = sn
		}
	}
	return out
}

func bms26FirstSourceID(sn bms26Snapshot) int {
	if sn.SourceID != 0 {
		return sn.SourceID
	}
	if len(sn.SourceIDs) > 0 {
		return sn.SourceIDs[0]
	}
	return math.MaxInt
}

// runBms26Poll — отдельный цикл опроса BMS (malina26). Пишет текущий снимок в Redis
// (HASH sunreceiver:bms26:current, поле = serial), каждое снятое показание — в
// ряд RedisTimeSeries (sunreceiver:ts:bms26:*), а 5-минутные средние — в PG
// (sunreceiver.bms26_averages_r, аккумулятор в памяти процесса).
func runBms26Poll(store *redisStore, pg *pgStore, ctx context.Context) {
	acc := newBms26Accumulator()
	lastAlarms := map[string]map[string]bool{} // serial -> активные тревоги (для истории)
	poller := newBms26Poller(bms26, bms26Metrics)
	ticker := time.NewTicker(bms26.pollInterval)
	defer ticker.Stop()
	log.Printf("bms26: опрос через %s (период %v, Redis current %s, PG %v, stale >%v)",
		bms26.http.baseURL, bms26.pollInterval, redisBms26CurrentKey, pg != nil, bms26StaleAfter)
	for {
		select {
		case <-ticker.C:
			// Сначала сбрасываем ЗАКРЫТЫЕ бакеты (start < текущего 5-мин бакета), и
			// только потом принимаем новые: иначе acc.add() на границе бакета
			// заменяет предыдущий бакет новым до его финализации, и точка теряется.
			saveBms26ClosedBuckets(pg, acc.closed(time.Now()))
			snaps := poller.poll(ctx)
			if len(snaps) > 0 {
				now := poller.now()
				// current — детерминированно по одному снимку на serial (дубли R1).
				curSnaps := bms26SelectCurrent(snaps)
				cur := make(map[string]string, len(curSnaps))
				for serial, sn := range curSnaps {
					b, err := json.Marshal(sn)
					if err != nil {
						continue
					}
					cur[serial] = string(b)
				}
				for _, sn := range snaps {
					// Stale-снимок не выдаём в историю как новое измерение: значение
					// старое, новая метка времени исказила бы график. current хранит
					// его с флагом stale для индикации.
					if sn.Stale {
						continue
					}
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
		case <-ctx.Done():
			return
		}
	}
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
		Kind:           "bms26",
		DeviceName:     sn.Serial,
		Key:            sn.Serial,
		SourceLastSeen: sn.SourceLastSeen,
		Stale:          sn.Stale,
		DupSerial:      sn.DupCount,
		CellCount:      sn.CellCount,
		CellsV:         sn.CellsV,
		TotalV:         sn.TotalV,
		PortV:          sn.PortV,
		CurrentA:       sn.CurrentA,
		Soc:            int(math.Round(sn.Soc)),
		CapacityAh:     sn.CapacityAh,
		RemainingAh:    sn.RemainingAh,
		TemperaturesC:  sn.TemperaturesC,
		PowerW:         sn.PowerW,
		ChargeMos:      boolBit(sn.Flags, 0),
		DischargeMos:   boolBit(sn.Flags, 1),
		Balancer:       boolBit(sn.Flags, 2),
		MaxCellIdx:     sn.MaxCellIdx,
		MaxCellV:       sn.MaxCellV,
		MinCellIdx:     sn.MinCellIdx,
		MinCellV:       sn.MinCellV,
		AvgCellV:       sn.AvgCellV,
		Soh:            sn.Soh,
		Cycles:         sn.Cycles,
		RatedAh:        sn.RatedAh,
		BalanceMask:    sn.BalanceMask,
		CellWarnMask:   sn.CellWarnMask,
		CellProtMask:   sn.CellProtMask,
		TempWarnMask:   sn.TempWarnMask,
		TempProtMask:   sn.TempProtMask,
		Model:          sn.BmsType,
		Mode:           bms26ModeName(sn.Mode),
		Alarms:         sn.Alarms,
		Protections:    sn.Protections,
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

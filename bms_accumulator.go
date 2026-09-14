package main

import (
	"fmt"
	"math"
	"time"
)

// bmsAveraged — одна усреднённая за 5 минут (avgStep) точка ANT BMS.
// Хранится в PostgreSQL (sunreceiver.bms_averages.values, jsonb) и в Redis
// (месячные ZSET sunreceiver:bms:series:<YYYY-MM>, окно 2 календарных суток).
// Мгновенные параметры (ток/мощность/SOC/ёмкости/ячейки/температуры)
// усредняются за промежуток; дискретные флаги (MOS/балансировка/число ячеек)
// и счётчик кадров — по последнему значению в промежутке (усреднение для них
// неприменимо).
type bmsAveraged struct {
	CurrentA     float64   `json:"current_a"`      // A, среднее (знаковый)
	PowerW       float64   `json:"power_w"`        // W, среднее (знаковая)
	Soc          float64   `json:"soc"`            // %, среднее
	CapacityAh   float64   `json:"capacity_ah"`    // А·ч, среднее
	RemainingAh  float64   `json:"remaining_ah"`   // А·ч, среднее
	MaxCellV     float64   `json:"max_cell_v"`     // V, среднее
	MinCellV     float64   `json:"min_cell_v"`     // V, среднее
	AvgCellV     float64   `json:"avg_cell_v"`     // V, среднее
	CellsV       []float64 `json:"cells_v"`        // V, среднее по каждой ячейке
	Temperatures []float64 `json:"temperatures_c"` // °C, среднее по каналам T1..T6
	CellCount    int       `json:"cell_count"`     // число ячеек, последнее в промежутке
	ChargeMos    int       `json:"charge_mos"`     // MOS зарядки, последнее в промежутке
	DischargeMos int       `json:"discharge_mos"`  // MOS разряда, последнее в промежутке
	Balancer     int       `json:"balancer"`       // балансировка, последнее в промежутке
	Frames       uint32    `json:"frames"`         // счётчик кадров, последнее в промежутке
	Samples      int       `json:"samples"`        // сколько 1-секундных снимков вошло в точку
}

// bmsSeriesPoint — точка BMS-ряда в Redis: имя устройства, время (начало
// 5-минутного промежутка, RFC3339) и усреднённые значения.
type bmsSeriesPoint struct {
	Name string `json:"name"` // deviceName, напр. "AntBms 320 A/h"
	Ts   string `json:"ts"`   // начало 5-минутного промежутка (RFC3339)
	bmsAveraged
}

// bmsAvgPoint — завершённый (или выгружаемый при остановке) 5-минутный
// промежуток одной BMS для записи в хранилища.
type bmsAvgPoint struct {
	name  string
	start time.Time // начало 5-минутного промежутка
	avg   bmsAveraged
}

// bmsBucket — in-memory накопитель 1-секундных снимков ANT BMS за один
// 5-минутный промежуток. BMS (в отличие от инверторов) не имеет временного
// ряда в Redis — только текущее состояние (HASH sunreceiver:bms), поэтому
// аккумуляция идёт в памяти пулера: неполный промежуток до момента
// перезапуска процесса восстановить нельзя (источник истории — только живой
// опрос read_bms.php).
type bmsBucket struct {
	start      time.Time
	sums       map[string]float64 // суммы мгновенных параметров (ключ — поле/«cell_i»/«temp_i»)
	cnt        map[string]int
	maxCellIdx int // максимальный индекс ячейки, встреченный в снимках
	maxTempIdx int // максимальный индекс температурного канала
	last       bmsDevice // последний снимок (источник значений «по последнему»)
}

// newBmsBucket создаёт накопитель для промежутка, начинающегося в start.
func newBmsBucket(start time.Time) *bmsBucket {
	return &bmsBucket{start: start, sums: map[string]float64{}, cnt: map[string]int{}}
}

// add вносит один 1-секундный снимок в накопитель. Вызывающий (runBmsPoll)
// гарантирует, что снимок попадает в [start, start+avgStep).
func (b *bmsBucket) add(d bmsDevice) {
	b.last = d
	add := func(key string, v float64) {
		b.sums[key] += v
		b.cnt[key]++
	}
	add("current_a", d.CurrentA)
	add("power_w", d.PowerW)
	add("soc", float64(d.Soc))
	add("capacity_ah", d.CapacityAh)
	add("remaining_ah", d.RemainingAh)
	add("max_cell_v", d.MaxCellV)
	add("min_cell_v", d.MinCellV)
	add("avg_cell_v", d.AvgCellV)
	for i, c := range d.CellsV {
		add(fmt.Sprintf("cell_%d", i), c)
		if i > b.maxCellIdx {
			b.maxCellIdx = i
		}
	}
	for i, t := range d.TemperaturesC {
		add(fmt.Sprintf("temp_%d", i), t)
		if i > b.maxTempIdx {
			b.maxTempIdx = i
		}
	}
}

// avg строит усреднённую точку из накопленного: мгновенные параметры —
// среднее (округление до 0.1; напряжения ячеек — до 0.001 В), флаги и
// счётчик кадров — из последнего снимка промежутка.
func (b *bmsBucket) avg() bmsAveraged {
	avg1 := func(key string) float64 {
		n := b.cnt[key]
		if n == 0 {
			return 0
		}
		return math.Round(b.sums[key]/float64(n)*10) / 10
	}
	nCells := b.last.CellCount
	if l := len(b.last.CellsV); l > nCells {
		nCells = l
	}
	cells := make([]float64, 0, nCells)
	for i := 0; i < nCells && i <= b.maxCellIdx; i++ {
		key := fmt.Sprintf("cell_%d", i)
		if n := b.cnt[key]; n > 0 {
			cells = append(cells, math.Round(b.sums[key]/float64(n)*1000)/1000)
		} else {
			cells = append(cells, 0)
		}
	}
	temps := make([]float64, 0, b.maxTempIdx+1)
	for i := 0; i <= b.maxTempIdx; i++ {
		key := fmt.Sprintf("temp_%d", i)
		if n := b.cnt[key]; n > 0 {
			temps = append(temps, math.Round(b.sums[key]/float64(n)*10)/10)
		} else {
			temps = append(temps, 0)
		}
	}
	return bmsAveraged{
		CurrentA:     avg1("current_a"),
		PowerW:       avg1("power_w"),
		Soc:          avg1("soc"),
		CapacityAh:   avg1("capacity_ah"),
		RemainingAh:  avg1("remaining_ah"),
		MaxCellV:     avg1("max_cell_v"),
		MinCellV:     avg1("min_cell_v"),
		AvgCellV:     avg1("avg_cell_v"),
		CellsV:       cells,
		Temperatures: temps,
		CellCount:    b.last.CellCount,
		ChargeMos:    b.last.ChargeMos,
		DischargeMos: b.last.DischargeMos,
		Balancer:     b.last.Balancer,
		Frames:       b.last.Frames,
		Samples:      b.cnt["current_a"],
	}
}

// bmsAccumulator — in-memory аккумуляция 5-минутных усреднённых точек BMS
// (см. bmsBucket). Однопоточный: трогает только горутина runBmsPoll.
type bmsAccumulator struct {
	buckets map[string]*bmsBucket // key = deviceName
	pending []bmsAvgPoint         // закрытые, но ещё не выданные вызывающему
}

func newBmsAccumulator() *bmsAccumulator {
	return &bmsAccumulator{buckets: map[string]*bmsBucket{}}
}

// add кладёт 1-секундный снимок в накопитель 5-минутного промежутка,
// содержащего now (floorToStep(now) — начало промежутка). Если у устройства
// уже есть накопитель, и его промежуток к моменту now завершён, он переводится
// в pending (closed() выдаст его) — закрытый промежуток не теряется при
// переходе к новому.
func (a *bmsAccumulator) add(d bmsDevice, now time.Time) {
	if d.DeviceName == "" {
		return
	}
	start := floorToStep(now)
	b := a.buckets[d.DeviceName]
	if b == nil || !b.start.Equal(start) {
		if b != nil && !b.start.Add(avgStep).After(now) {
			a.pending = append(a.pending, bmsAvgPoint{name: d.DeviceName, start: b.start, avg: b.avg()})
		}
		b = newBmsBucket(start)
		a.buckets[d.DeviceName] = b
	}
	b.add(d)
}

// closed возвращает завершившиеся промежутки (конец = start+avgStep <= now),
// включая переведённые в pending при add. Вызывающий пишет точки в Redis/PG.
func (a *bmsAccumulator) closed(now time.Time) []bmsAvgPoint {
	var out []bmsAvgPoint
	for name, b := range a.buckets {
		if !b.start.Add(avgStep).After(now) {
			out = append(out, bmsAvgPoint{name: name, start: b.start, avg: b.avg()})
			delete(a.buckets, name)
		}
	}
	out = append(out, a.pending...)
	a.pending = nil
	return out
}

// drain возвращает ВСЕ оставшиеся (возможно неполные) промежутки — вызывается
// при остановке пулера, чтобы накопленные снимки не были потеряны.
func (a *bmsAccumulator) drain() []bmsAvgPoint {
	var out []bmsAvgPoint
	for name, b := range a.buckets {
		out = append(out, bmsAvgPoint{name: name, start: b.start, avg: b.avg()})
	}
	a.buckets = map[string]*bmsBucket{}
	return out
}

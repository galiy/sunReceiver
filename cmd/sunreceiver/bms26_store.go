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
	"errors"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// Ключи Redis модуля BMS malina26 (единый тип BMS: ANT + EnBMS). Обособлены от
// прежних ANT (sunreceiver:bms) и EnBMS (sunreceiver:enbms:*).
const (
	// redisBms26CurrentKey — HASH текущего снимка BMS: поле = serial,
	// значение = JSON bms26Snapshot.
	redisBms26CurrentKey = "sunreceiver:bms26:current"
	// sunreceiver:ts:bms26:* (серии RedisTimeSeries).
)

// SaveBms26Current записывает текущие снимки BMS в HASH (поле = serial).
func (s *redisStore) SaveBms26Current(cur map[string]string) error {
	if len(cur) == 0 {
		return nil
	}
	vals := make([]any, 0, len(cur)*2)
	for k, v := range cur {
		vals = append(vals, k, v)
	}
	return s.rdb.HSet(s.ctx, redisBms26CurrentKey, vals...).Err()
}

// SaveBms26Series кладёт мгновенную точку в ряд RedisTimeSeries (dedup по (serial, score)).
func (s *redisStore) SaveBms26Series(sn bms26Snapshot, ts time.Time) error {
	if sn.Serial == "" {
		return nil
	}
	name := sn.DeviceName
	if name == "" {
		name = sn.Serial
	}
	pipe := s.rdb.TxPipeline()
	s.tsAddValueContract(pipe, "bms26", sn.Serial, name, "", "", ts, bms26ScalarVC(sn))
	for i, v := range sn.CellsV {
		s.tsAdd(pipe, "bms26", sn.Serial, name, "", "", fmt.Sprintf("cell_%d", i+1), ts, v)
	}
	for i, v := range sn.TemperaturesC {
		s.tsAdd(pipe, "bms26", sn.Serial, name, "", "", fmt.Sprintf("temp_%d", i+1), ts, v)
	}
	for i, v := range sn.AlarmBytes {
		s.tsAdd(pipe, "bms26", sn.Serial, name, "", "", fmt.Sprintf("alarm_%d", i+1), ts, float64(v))
	}
	if _, err := pipe.Exec(s.ctx); err != nil {
		return fmt.Errorf("save bms26 series %s: %w", sn.Serial, err)
	}
	return nil
}

// bms26Accumulator — накопитель 5-минутных усреднённых точек BMS в памяти.
type bms26Accumulator struct {
	buckets map[string]*bms26Bucket
	// pending — финализированные бакеты, закрытые при смене 5-мин окна в add()
	// (граница прошла между тиками без вызова closed()) — забираются closed().
	pending []bms26Snapshot
}

type bms26Bucket struct {
	start time.Time
	n     int
	// последние значения метаданных/полей-«last»
	meta bms26Snapshot
	// суммы и счётчики для avg (per-index — для cells/temps)
	sumCells []float64
	cntCells []int
	sumTemps []float64
	cntTemps []int
	sumCur   float64
	sumPower float64
	sumSoc   float64
	sumSoh   float64
	sumRem   float64
	sumTotal float64
	sumPort  float64
	// OR-поля
	flags    int
	balMask  uint32
	cellWarn uint32
	cellProt uint32
	tempWarn uint32
	tempProt uint32
	alrm     [19]int
}

func newBms26Accumulator() *bms26Accumulator {
	return &bms26Accumulator{buckets: map[string]*bms26Bucket{}}
}

func bms26BucketStart(t time.Time) time.Time {
	return t.Truncate(5 * time.Minute)
}

func (a *bms26Accumulator) add(sn bms26Snapshot, now time.Time) {
	if sn.Serial == "" {
		return
	}
	st := bms26BucketStart(now)
	bk := a.buckets[sn.Serial]
	if bk == nil || !bk.start.Equal(st) {
		// Смена 5-мин окна: финализируем предыдущий бакет в pending (не теряем),
		// даже если closed() в этом тике ещё не вызывался.
		if bk != nil {
			a.pending = append(a.pending, bk.finalize())
		}
		bk = &bms26Bucket{start: st}
		a.buckets[sn.Serial] = bk
	}
	bk.n++
	bk.meta = sn // метаданные/«last» поля — из последнего снимка
	for i, v := range sn.CellsV {
		if i >= len(bk.sumCells) {
			bk.sumCells = append(bk.sumCells, 0)
			bk.cntCells = append(bk.cntCells, 0)
		}
		bk.sumCells[i] += v
		bk.cntCells[i]++
	}
	for i, v := range sn.TemperaturesC {
		if i >= len(bk.sumTemps) {
			bk.sumTemps = append(bk.sumTemps, 0)
			bk.cntTemps = append(bk.cntTemps, 0)
		}
		bk.sumTemps[i] += v
		bk.cntTemps[i]++
	}
	bk.sumCur += sn.CurrentA
	bk.sumPower += sn.PowerW
	bk.sumSoc += sn.Soc
	bk.sumSoh += sn.Soh
	bk.sumRem += sn.RemainingAh
	bk.sumTotal += sn.TotalV
	bk.sumPort += sn.PortV
	bk.flags |= sn.Flags
	bk.balMask |= sn.BalanceMask
	bk.cellWarn |= sn.CellWarnMask
	bk.cellProt |= sn.CellProtMask
	bk.tempWarn |= sn.TempWarnMask
	bk.tempProt |= sn.TempProtMask
	for i := 0; i < 19 && i < len(sn.AlarmBytes); i++ {
		bk.alrm[i] |= sn.AlarmBytes[i]
	}
}

// closed возвращает готовые (закрытые) бакеты и удаляет их из накопителя.
// Включает pending (закрытые при смене окна в add()) и бакеты старше текущего окна.
func (a *bms26Accumulator) closed(now time.Time) []bms26Snapshot {
	out := a.pending
	a.pending = nil
	cur := bms26BucketStart(now)
	for k, bk := range a.buckets {
		if !bk.start.Before(cur) {
			continue // текущий (незакрытый) бакет
		}
		out = append(out, bk.finalize())
		delete(a.buckets, k)
	}
	return out
}

func (bk *bms26Bucket) finalize() bms26Snapshot {
	n := float64(bk.n)
	s := bk.meta
	s.Timestamp = bk.start.Format(time.RFC3339)
	s.Flags = bk.flags
	s.BalanceMask = bk.balMask
	s.CellWarnMask = bk.cellWarn
	s.CellProtMask = bk.cellProt
	s.TempWarnMask = bk.tempWarn
	s.TempProtMask = bk.tempProt
	s.AlarmBytes = make([]int, 19)
	for i := 0; i < 19; i++ {
		s.AlarmBytes[i] = bk.alrm[i]
	}
	// cells/temps — среднее ПО ИНДЕКСУ (per-index счётчик), устойчиво к смене длины
	// массивов внутри окна.
	if len(bk.sumCells) > 0 {
		s.CellsV = make([]float64, len(bk.sumCells))
		for i := range bk.sumCells {
			if bk.cntCells[i] > 0 {
				s.CellsV[i] = bk.sumCells[i] / float64(bk.cntCells[i])
			}
		}
	}
	if len(bk.sumTemps) > 0 {
		s.TemperaturesC = make([]float64, len(bk.sumTemps))
		for i := range bk.sumTemps {
			if bk.cntTemps[i] > 0 {
				s.TemperaturesC[i] = bk.sumTemps[i] / float64(bk.cntTemps[i])
			}
		}
	}
	s.CurrentA = bk.sumCur / n
	s.PowerW = bk.sumPower / n
	s.Soc = bk.sumSoc / n
	s.Soh = bk.sumSoh / n
	s.RemainingAh = bk.sumRem / n
	s.TotalV = bk.sumTotal / n
	s.PortV = bk.sumPort / n
	return s
}

// saveBms26ClosedBuckets пишет готовые 5-минутные точки в PG
// (sunreceiver.bms26_averages_r). В Redis-ряд 5-минутные средние не пишутся.
func saveBms26ClosedBuckets(pg *pgStore, pts []bms26Snapshot) {
	if pg == nil || len(pts) == 0 {
		return
	}
	if err := retryPg(func() error {
		return pg.withTx(func(q pgExecer) error {
			for _, sn := range pts {
				ts, err := time.Parse(time.RFC3339, sn.Timestamp)
				if err != nil {
					// Битый timestamp бакета: не подменяем на now() (уедет по времени),
					// а пропускаем точку с логом.
					log.Printf("bms26 avg: битый ts %q (serial=%s) — точка пропущена", sn.Timestamp, sn.Serial)
					continue
				}
				if err := insertBms26AveragedExec(q, pg.ctx, sn.Serial, ts, sn); err != nil {
					return err
				}
			}
			return nil
		})
	}, 3); err != nil {
		log.Printf("bms26 avg pg: %v", err)
	}
}

// insertBms26AveragedExec раскладывает усреднённый бакет BMS в партиционированную
// колоночную таблицу bms26_averages_r (скаляры/метаданные — колонки) и в дочерние
// таблицы bms26_averages_r_cells/_temps/_alarm (массивы контракта по idx).
func insertBms26AveragedExec(q pgExecer, ctx context.Context, name string, ts time.Time, sn bms26Snapshot) error {
	sp := avgSpecByTable(bms26AvgTable)
	vc := valuesContract{
		"soc": sn.Soc, "soh": sn.Soh, "mode": float64(sn.Mode), "flags": float64(sn.Flags),
		"cycles": float64(sn.Cycles), "power_w": sn.PowerW, "rated_ah": sn.RatedAh,
		"current_a": sn.CurrentA, "avg_cell_v": sn.AvgCellV, "cell_count": float64(sn.CellCount),
		"max_cell_v": sn.MaxCellV, "min_cell_v": sn.MinCellV, "temp_count": float64(sn.TempCount),
		"capacity_ah": sn.CapacityAh, "balance_mask": float64(sn.BalanceMask),
		"cell_warn_mask": float64(sn.CellWarnMask), "cell_prot_mask": float64(sn.CellProtMask),
		"temp_warn_mask": float64(sn.TempWarnMask), "temp_prot_mask": float64(sn.TempProtMask),
		"max_cell_idx": float64(sn.MaxCellIdx), "min_cell_idx": float64(sn.MinCellIdx),
		"remaining_ah": sn.RemainingAh, "port_voltage_v": sn.PortV, "total_voltage_v": sn.TotalV,
	}
	meta := map[string]string{"base": sn.Base, "serial": sn.Serial, "bms_type": sn.BmsType, "device_name": sn.DeviceName}
	if err := insertAvgRow(q, ctx, sp, name, ts, meta, vc); err != nil {
		return err
	}
	if err := insertBms26Child(q, ctx, sp.table+"_cells", name, ts, floatVals(sn.CellsV)); err != nil {
		return err
	}
	if err := insertBms26Child(q, ctx, sp.table+"_temps", name, ts, floatVals(sn.TemperaturesC)); err != nil {
		return err
	}
	return insertBms26Child(q, ctx, sp.table+"_alarm", name, ts, intVals(sn.AlarmBytes))
}

// insertBms26Child пишет элементы массива (по idx) в дочернюю таблицу *_r_<suffix>
// одним многострочным INSERT (вместо поштучных Exec на элемент).
func insertBms26Child(q pgExecer, ctx context.Context, table, name string, ts time.Time, vals []any) error {
	if len(vals) == 0 {
		return nil
	}
	var sb strings.Builder
	args := make([]any, 0, len(vals)*4)
	fmt.Fprintf(&sb, "INSERT INTO %s (name, ts, idx, v) VALUES ", table)
	for i, v := range vals {
		if i > 0 {
			sb.WriteByte(',')
		}
		n := len(args)
		fmt.Fprintf(&sb, "($%d,$%d,$%d,$%d)", n+1, n+2, n+3, n+4)
		args = append(args, name, ts.UTC(), i, v)
	}
	sb.WriteString(" ON CONFLICT (name, ts, idx) DO UPDATE SET v = EXCLUDED.v")
	if _, err := q.Exec(ctx, sb.String(), args...); err != nil {
		return fmt.Errorf("pg insert %s %s: %w", table, name, err)
	}
	return nil
}

func floatVals(xs []float64) []any {
	out := make([]any, len(xs))
	for i, v := range xs {
		out[i] = v
	}
	return out
}

func intVals(xs []int) []any {
	out := make([]any, len(xs))
	for i, v := range xs {
		out[i] = v
	}
	return out
}

// bms26ScalarVC/bms26Meta — раскладка снимка BMS по колонкам (общие для записи и
// миграции), порядок согласован с avgSpecs для bms26.
func bms26ScalarVC(sn bms26Snapshot) valuesContract {
	return valuesContract{
		"soc": sn.Soc, "soh": sn.Soh, "mode": float64(sn.Mode), "flags": float64(sn.Flags),
		"cycles": float64(sn.Cycles), "power_w": sn.PowerW, "rated_ah": sn.RatedAh,
		"current_a": sn.CurrentA, "avg_cell_v": sn.AvgCellV, "cell_count": float64(sn.CellCount),
		"max_cell_v": sn.MaxCellV, "min_cell_v": sn.MinCellV, "temp_count": float64(sn.TempCount),
		"capacity_ah": sn.CapacityAh, "balance_mask": float64(sn.BalanceMask),
		"cell_warn_mask": float64(sn.CellWarnMask), "cell_prot_mask": float64(sn.CellProtMask),
		"temp_warn_mask": float64(sn.TempWarnMask), "temp_prot_mask": float64(sn.TempProtMask),
		"max_cell_idx": float64(sn.MaxCellIdx), "min_cell_idx": float64(sn.MinCellIdx),
		"remaining_ah": sn.RemainingAh, "port_voltage_v": sn.PortV, "total_voltage_v": sn.TotalV,
	}
}

// insertBms26AveragedExecNoClobber — как insertBms26AveragedExec, но ON CONFLICT
// DO NOTHING (для миграции истории: не затирает новые точки).
func insertBms26AveragedExecNoClobber(q pgExecer, ctx context.Context, name string, ts time.Time, sn bms26Snapshot) error {
	sp := avgSpecByTable(bms26AvgTable)
	meta := map[string]string{"base": sn.Base, "serial": sn.Serial, "bms_type": sn.BmsType, "device_name": sn.DeviceName}
	if err := insertAvgRowNoClobber(q, ctx, sp, name, ts, meta, bms26ScalarVC(sn)); err != nil {
		return err
	}
	if err := insertBms26ChildNoClobber(q, ctx, sp.table+"_cells", name, ts, floatVals(sn.CellsV)); err != nil {
		return err
	}
	if err := insertBms26ChildNoClobber(q, ctx, sp.table+"_temps", name, ts, floatVals(sn.TemperaturesC)); err != nil {
		return err
	}
	return insertBms26ChildNoClobber(q, ctx, sp.table+"_alarm", name, ts, intVals(sn.AlarmBytes))
}

func insertBms26ChildNoClobber(q pgExecer, ctx context.Context, table, name string, ts time.Time, vals []any) error {
	if len(vals) == 0 {
		return nil
	}
	var sb strings.Builder
	args := make([]any, 0, len(vals)*4)
	fmt.Fprintf(&sb, "INSERT INTO %s (name, ts, idx, v) VALUES ", table)
	for i, v := range vals {
		if i > 0 {
			sb.WriteByte(',')
		}
		n := len(args)
		fmt.Fprintf(&sb, "($%d,$%d,$%d,$%d)", n+1, n+2, n+3, n+4)
		args = append(args, name, ts.UTC(), i, v)
	}
	sb.WriteString(" ON CONFLICT (name, ts, idx) DO NOTHING")
	if _, err := q.Exec(ctx, sb.String(), args...); err != nil {
		return fmt.Errorf("pg insert %s %s: %w", table, name, err)
	}
	return nil
}

// Bms26Current возвращает текущие снимки всех BMS malina26 (поле = serial).
func (s *redisStore) Bms26Current() ([]bms26Snapshot, error) {
	m, err := s.rdb.HGetAll(s.ctx, redisBms26CurrentKey).Result()
	if err != nil {
		return nil, fmt.Errorf("HGETALL %s: %w", redisBms26CurrentKey, err)
	}
	out := make([]bms26Snapshot, 0, len(m))
	for _, v := range m {
		var sn bms26Snapshot
		if json.Unmarshal([]byte(v), &sn) == nil {
			out = append(out, sn)
		}
	}
	return out, nil
}

// Bms26One возвращает текущий снимок одной BMS malina26 по serial; nil — нет.
func (s *redisStore) Bms26One(serial string) (*bms26Snapshot, error) {
	b, err := s.rdb.HGet(s.ctx, redisBms26CurrentKey, serial).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var sn bms26Snapshot
	if err := json.Unmarshal(b, &sn); err != nil {
		return nil, err
	}
	return &sn, nil
}

// QueryBms26Series возвращает сырые точки одной BMS malina26 (по serial) за
// период [start, end] из RT-серий, по возрастанию времени.
func (s *redisStore) QueryBms26Series(serial string, start, end time.Time) ([]bms26Snapshot, error) {
	if start.After(end) {
		return nil, errors.New("start after end")
	}
	series, err := s.tsMRange(start, end, "kind=bms26", "device="+serial)
	if err != nil {
		return nil, err
	}
	return assembleBms26Series(series), nil
}

// QueryBms26SeriesAgg — как QueryBms26Series, но с серверной агрегацией avg
// (bucketMs; 0 — сырьё) для графиков BMS.
func (s *redisStore) QueryBms26SeriesAgg(serial string, start, end time.Time, bucketMs int64) ([]bms26Snapshot, error) {
	if start.After(end) {
		return nil, errors.New("start after end")
	}
	series, err := s.tsMRangeAgg(start, end, "avg", bucketMs, "kind=bms26", "device="+serial)
	if err != nil {
		return nil, err
	}
	return assembleBms26Series(series), nil
}

// assembleBms26Series собирает bms26Snapshot из per-tag серий BMS (скаляры +
// cell_i/temp_i/alarm_i) по (serial, ts).
func assembleBms26Series(series []tsSeries) []bms26Snapshot {
	type dk struct {
		serial string
		ts     int64
	}
	byKey := map[dk]*bms26Snapshot{}
	for _, ser := range series {
		serial := tsLabelUnescape(ser.Labels["device"])
		name := tsLabelUnescape(ser.Labels["name"])
		tag := tsLabelUnescape(ser.Labels["tag"])
		for _, p := range ser.Points {
			k := dk{serial, p.TS}
			sn := byKey[k]
			if sn == nil {
				sn = &bms26Snapshot{Serial: serial, DeviceName: name, Timestamp: time.UnixMilli(p.TS).Format(time.RFC3339)}
				byKey[k] = sn
			}
			assignBms26Tag(sn, tag, p.V)
		}
	}
	out := make([]bms26Snapshot, 0, len(byKey))
	for _, sn := range byKey {
		out = append(out, *sn)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Timestamp < out[j].Timestamp })
	return out
}

// assignBms26Tag кладёт значение тега в поле снимка BMS (скаляры) либо в массив
// (cell_/temp_/alarm_ по индексу 1-based).
func assignBms26Tag(sn *bms26Snapshot, tag string, v float64) {
	if i, ok := tagIndex(tag, "cell_"); ok {
		sn.CellsV = setIdx(sn.CellsV, i, v)
		return
	}
	if i, ok := tagIndex(tag, "temp_"); ok {
		sn.TemperaturesC = setIdx(sn.TemperaturesC, i, v)
		return
	}
	if i, ok := tagIndex(tag, "alarm_"); ok {
		sn.AlarmBytes = setIdxInt(sn.AlarmBytes, i, int(v))
		return
	}
	switch tag {
	case "soc":
		sn.Soc = v
	case "soh":
		sn.Soh = v
	case "mode":
		sn.Mode = int(v)
	case "flags":
		sn.Flags = int(v)
	case "cycles":
		sn.Cycles = int(v)
	case "power_w":
		sn.PowerW = v
	case "rated_ah":
		sn.RatedAh = v
	case "current_a":
		sn.CurrentA = v
	case "avg_cell_v":
		sn.AvgCellV = v
	case "cell_count":
		sn.CellCount = int(v)
	case "max_cell_v":
		sn.MaxCellV = v
	case "min_cell_v":
		sn.MinCellV = v
	case "temp_count":
		sn.TempCount = int(v)
	case "capacity_ah":
		sn.CapacityAh = v
	case "balance_mask":
		sn.BalanceMask = uint32(v)
	case "cell_warn_mask":
		sn.CellWarnMask = uint32(v)
	case "cell_prot_mask":
		sn.CellProtMask = uint32(v)
	case "temp_warn_mask":
		sn.TempWarnMask = uint32(v)
	case "temp_prot_mask":
		sn.TempProtMask = uint32(v)
	case "max_cell_idx":
		sn.MaxCellIdx = int(v)
	case "min_cell_idx":
		sn.MinCellIdx = int(v)
	case "remaining_ah":
		sn.RemainingAh = v
	case "port_voltage_v":
		sn.PortV = v
	case "total_voltage_v":
		sn.TotalV = v
	}
}

// tagIndex — индекс (0-based) для тега вида "<prefix><n>" (n 1-based).
func tagIndex(tag, prefix string) (int, bool) {
	if !strings.HasPrefix(tag, prefix) {
		return 0, false
	}
	n, err := strconv.Atoi(tag[len(prefix):])
	if err != nil || n < 1 {
		return 0, false
	}
	return n - 1, true
}

func setIdx(a []float64, i int, v float64) []float64 {
	for len(a) <= i {
		a = append(a, 0)
	}
	a[i] = v
	return a
}

func setIdxInt(a []int, i, v int) []int {
	for len(a) <= i {
		a = append(a, 0)
	}
	a[i] = v
	return a
}

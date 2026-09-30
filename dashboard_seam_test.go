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
	"testing"
	"time"
)

// TestSeamWindows проверяет разбиение диапазона на часть PG (старше cutoff) и
// Redis (cutoff и новее) без разрыва и дублей на стыке.
func TestSeamWindows(t *testing.T) {
	// cutoff = 2026-09-30 00:00:00 (как recentCutoff).
	cutoff := time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)

	// Диапазон захватывает стык: [29-е 12:00, 30-е 12:00].
	start := cutoff.Add(-12 * time.Hour)
	end := cutoff.Add(12 * time.Hour)
	pgS, pgE, pgOK, rS, rE, rOK := seamWindows(start, end, cutoff)
	if !pgOK || !rOK {
		t.Fatalf("ожидались обе части: pgOK=%v rOK=%v", pgOK, rOK)
	}
	if !pgS.Equal(start) {
		t.Fatalf("pgStart=%v, want %v", pgS, start)
	}
	// PG заканчивается за секунду до cutoff — точка ровно на cutoff у Redis.
	if want := cutoff.Add(-time.Second); !pgE.Equal(want) {
		t.Fatalf("pgEnd=%v, want %v", pgE, want)
	}
	if !rS.Equal(cutoff) {
		t.Fatalf("redisStart=%v, want %v (cutoff)", rS, cutoff)
	}
	if !rE.Equal(end) {
		t.Fatalf("redisEnd=%v, want %v", rE, end)
	}
	// Непрерывность: pgEnd + 1s == redisStart (без дыры и без перекрытия).
	if !pgE.Add(time.Second).Equal(rS) {
		t.Fatalf("стык разрывный: pgEnd=%v redisStart=%v", pgE, rS)
	}

	// Полностью исторический диапазон (end < cutoff): только PG.
	if _, _, pgOK2, _, _, rOK2 := seamWindows(cutoff.Add(-48*time.Hour), cutoff.Add(-24*time.Hour), cutoff); !pgOK2 || rOK2 {
		t.Fatalf("исторический: pgOK=%v rOK=%v, want true/false", pgOK2, rOK2)
	}
	// Полностью свежий диапазон (start >= cutoff): только Redis.
	if _, _, pgOK3, rS3, _, rOK3 := seamWindows(cutoff.Add(time.Hour), cutoff.Add(2*time.Hour), cutoff); pgOK3 || !rOK3 {
		t.Fatalf("свежий: pgOK=%v rOK=%v, want false/true", pgOK3, rOK3)
	} else if !rS3.Equal(cutoff.Add(time.Hour)) {
		t.Fatalf("свежий redisStart=%v", rS3)
	}
}

func mkBmsPt(ts time.Time, cur float64, cells []float64) bmsSeriesPoint {
	return bmsSeriesPoint{
		Name: "dev", Display: "dev", Ts: ts.Format(time.RFC3339),
		bmsAveraged: bmsAveraged{
			CurrentA: cur, PowerW: cur * 50, Soc: 50, CapacityAh: 100, RemainingAh: 50,
			MaxCellV: 3.30, MinCellV: 3.20, AvgCellV: 3.25, CellsV: cells,
			Temperatures: []float64{25}, CellCount: len(cells), Samples: 1,
		},
	}
}

// TestDownsampleBMSSeries проверяет, что длинная BMS-серия прореживается до
// порога с усреднением полей и сохранением ячеек.
// TestDownsampleBMSSeriesPerIndexCells: при разной длине cells_v у точек
// усреднение идёт по счётчику на индекс — отсутствующие элементы не разбавляют
// среднее нулями. Все точки попадают в один бин (span = 1 с).
func TestDownsampleBMSSeriesPerIndexCells(t *testing.T) {
	from := time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)
	to := from.Add(time.Second)
	var pts []bmsSeriesPoint
	for i := 0; i < 1200; i++ { // > maxSeriesPoints
		var cells []float64
		if i%2 == 0 { // половина точек — без блока ячеек
			cells = nil
		} else {
			cells = []float64{3.0, 3.1}
		}
		sp := mkBmsPt(from, 1, cells)
		sp.CellCount = len(cells)
		pts = append(pts, sp)
	}
	out := downsampleBMSSeries(pts, from, to)
	// k = ceil(1200/1000) = 2 → ~600 групп, в каждой одна точка с ячейками.
	if len(out) != 600 {
		t.Fatalf("групп %d, want 600 (k=2)", len(out))
	}
	if len(out[0].CellsV) != 2 || out[0].CellsV[0] != 3.0 || out[0].CellsV[1] != 3.1 {
		t.Fatalf("ячейки усреднены неверно (разбавлены нулями?): %v", out[0].CellsV)
	}
	if out[0].CellCount < 2 {
		t.Fatalf("CellCount=%d, want >=2", out[0].CellCount)
	}
}

// TestDownsampleHybridSparsePreserved: разрежённые точки (шаг больше ожидаемого
// span/maxSeriesPoints) не сшиваются в группы с соседями и сохраняются как есть,
// даже если суммарно точек больше порога.
func TestDownsampleHybridSparsePreserved(t *testing.T) {
	from := time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)
	to := from.Add(24 * time.Hour) // gap = 86400/1000 = 86.4 с
	var pts []seriesPoint
	for k := 0; k < 3; k++ { // разрежённые: шаг 300 с > 86.4
		pts = append(pts, seriesPoint{T: from.Add(time.Duration(k) * 300 * time.Second).Format(time.RFC3339), V: 42})
	}
	for i := 0; i < 2000; i++ { // плотный ряд 1 с
		pts = append(pts, seriesPoint{T: from.Add(time.Duration(1000+i) * time.Second).Format(time.RFC3339), V: 1})
	}
	out := downsampleSeries(pts, from, to)
	if len(out) > maxSeriesPoints {
		t.Fatalf("точек после прореживания %d > %d", len(out), maxSeriesPoints)
	}
	sparse := 0
	for _, p := range out {
		if p.V == 42 {
			sparse++
		}
	}
	if sparse != 3 {
		t.Fatalf("разрежённые точки не сохранены поштучно: найдено %d, want 3", sparse)
	}
}

func TestDownsampleBMSSeries(t *testing.T) {
	from := time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)
	to := from.Add(24 * time.Hour)
	// Больше порога: по точке раз в секунду за сутки.
	var pts []bmsSeriesPoint
	for i := 0; i < 24*3600; i += 1 {
		pts = append(pts, mkBmsPt(from.Add(time.Duration(i)*time.Second), float64(i%20), []float64{3.2, 3.3}))
	}
	out := downsampleBMSSeries(pts, from, to)
	if len(out) == 0 || len(out) > maxSeriesPoints {
		t.Fatalf("после прореживания точек %d, want 1..%d", len(out), maxSeriesPoints)
	}
	// Первая точка сохраняет имя/массивы.
	if out[0].Name != "dev" || len(out[0].CellsV) != 2 {
		t.Fatalf("первая точка: %+v", out[0])
	}
	// Ряд короче порога возвращается как есть.
	short := pts[:10]
	if got := downsampleBMSSeries(short, from, to); len(got) != 10 {
		t.Fatalf("короткий ряд изменён: %d", len(got))
	}
}

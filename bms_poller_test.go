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
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestPollAndSaveBMSUpdatedZero: read_bms.php при сбое чтения shm отдаёт
// {"updated":0,"devices":[]} (HTTP 200). bmslistener никогда не публикует
// updated=0 — это маркер сбоя. pollAndSaveBMS НЕ должен трогать коллекцию в
// Redis (иначе одиночная shm-гонка вычистит весь дашборд BMS).
func TestPollAndSaveBMSUpdatedZero(t *testing.T) {
	s := testStore(t)
	cleanTestKeys(t, s)
	t.Cleanup(func() { cleanTestKeys(t, s) })

	// Тестовый httptest-сервер отдаёт сбойный «пустой» ответ.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"updated":0,"devices":[]}`))
	}))
	defer srv.Close()

	// Подменяем глобальный bmsSite на тестовый (возвращаем предыдущий).
	oldSite := bmsSite
	bmsSite = &bmsApiClient{
		url:     srv.URL,
		client:  &http.Client{Timeout: 2 * time.Second},
		authHdr: "Basic test",
	}
	t.Cleanup(func() { bmsSite = oldSite })

	// Досеяли тестовое устройство в sunreceiver:bms — оно должно пережить сбойный ответ.
	const seededName = "AntBms 320 A/h (/dev/ttyUSB0)"
	if err := s.SetBMS(map[string]string{seededName: `{"deviceName":"` + seededName + `"}`}); err != nil {
		t.Fatalf("SetBMS seed: %v", err)
	}

	// Сбойный ответ → коллекция не трогается, возвращён nil.
	if col := pollAndSaveBMS(context.Background(), s); col != nil {
		t.Fatalf("pollAndSaveBMS при updated=0 вернул %v, want nil", col)
	}

	cur, err := s.BMSCurrent()
	if err != nil {
		t.Fatalf("BMSCurrent: %v", err)
	}
	if _, ok := cur[seededName]; !ok {
		t.Fatalf("seeded BMS удалён сбойным ответом: %+v", cur)
	}
}

// TestPollAndSaveBMSValidEmptyTolerance: валидно-пустая коллекция (updated>0,
// devices=[]) НЕ стирает дашборд при одиночном/коротком всплеске пустоты (K1):
// одиночный перезапуск bmslistener или временный сбой shm не должен сносить HASH
// sunreceiver:bms. Пока счётчик подряд идущих пустых ответов < bmsEmptyTolerance —
// коллекция не трогается.
func TestPollAndSaveBMSValidEmptyTolerance(t *testing.T) {
	s := testStore(t)
	cleanTestKeys(t, s)
	t.Cleanup(func() { cleanTestKeys(t, s) })
	bmsEmptyStreak = 0

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"updated":1750000000,"devices":[]}`))
	}))
	defer srv.Close()

	oldSite := bmsSite
	bmsSite = &bmsApiClient{
		url:     srv.URL,
		client:  &http.Client{Timeout: 2 * time.Second},
		authHdr: "Basic test",
	}
	t.Cleanup(func() { bmsSite = oldSite })

	const seededName = "AntBms 320 A/h (/dev/ttyUSB0)"
	if err := s.SetBMS(map[string]string{seededName: `{"deviceName":"` + seededName + `"}`}); err != nil {
		t.Fatalf("SetBMS seed: %v", err)
	}

	// 1–2 пустых ответа (меньше bmsEmptyTolerance=3) — дашборд сохраняется.
	for i := 0; i < 2; i++ {
		if col := pollAndSaveBMS(context.Background(), s); col == nil {
			t.Fatalf("pollAndSaveBMS при updated>0 вернул nil, want коллекцию")
		}
		cur, err := s.BMSCurrent()
		if err != nil {
			t.Fatalf("BMSCurrent: %v", err)
		}
		if _, ok := cur[seededName]; !ok {
			t.Fatalf("seeded BMS удалён пустым ответом #%d (счётчик %d): %+v", i+1, bmsEmptyStreak, cur)
		}
	}
}

// TestPollAndSaveBMSValidEmpty: при устойчивой пустоте (>= bmsEmptyTolerance
// подряд идущих пустых ответов) дашборд очищается — это «действительно 0 батарей».
func TestPollAndSaveBMSValidEmpty(t *testing.T) {
	s := testStore(t)
	cleanTestKeys(t, s)
	t.Cleanup(func() { cleanTestKeys(t, s) })
	bmsEmptyStreak = 0
	saved := bmsEmptyTolerance
	bmsEmptyTolerance = 2
	t.Cleanup(func() { bmsEmptyTolerance = saved })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"updated":1750000000,"devices":[]}`))
	}))
	defer srv.Close()

	oldSite := bmsSite
	bmsSite = &bmsApiClient{
		url:     srv.URL,
		client:  &http.Client{Timeout: 2 * time.Second},
		authHdr: "Basic test",
	}
	t.Cleanup(func() { bmsSite = oldSite })

	const seededName = "AntBms 320 A/h (/dev/ttyUSB0)"
	if err := s.SetBMS(map[string]string{seededName: `{"deviceName":"` + seededName + `"}`}); err != nil {
		t.Fatalf("SetBMS seed: %v", err)
	}

	// Первый пустой ответ (счётчик 1 < 2) — коллекция не трогается.
	if col := pollAndSaveBMS(context.Background(), s); col == nil {
		t.Fatal("pollAndSaveBMS вернул nil, want коллекцию")
	}
	cur, err := s.BMSCurrent()
	if err != nil {
		t.Fatalf("BMSCurrent: %v", err)
	}
	if _, ok := cur[seededName]; !ok {
		t.Fatalf("seeded BMS удалён: %+v", cur)
	}

	// Второй подряд пустой ответ — достигнут порог, коллекция очищается.
	if col := pollAndSaveBMS(context.Background(), s); col == nil {
		t.Fatal("pollAndSaveBMS вернул nil, want коллекцию")
	}
	cur, err = s.BMSCurrent()
	if err != nil {
		t.Fatalf("BMSCurrent: %v", err)
	}
	if _, ok := cur[seededName]; ok {
		t.Fatalf("seeded BMS не удалён устойчивой пустотой: %+v", cur)
	}
}

// TestBmsKey: коллизия имён двух одинаковых батарей с разными USB-портами
// разрешается ключом deviceName@port; при пустом port — фолбэк на deviceName.
func TestBmsKey(t *testing.T) {
	a := bmsDevice{DeviceName: "AntBms 320 A/h", Port: "/dev/ttyUSB0"}
	b := bmsDevice{DeviceName: "AntBms 320 A/h", Port: "/dev/ttyUSB1"}
	if bmsKey(a) == bmsKey(b) {
		t.Fatalf("коллизия: одинаковые ключи для разных портов: %q", bmsKey(a))
	}
	if bmsKey(a) != "AntBms 320 A/h@/dev/ttyUSB0" {
		t.Fatalf("bmsKey(a) = %q", bmsKey(a))
	}
	noPort := bmsDevice{DeviceName: "AntBms 320 A/h"}
	if bmsKey(noPort) != "AntBms 320 A/h" {
		t.Fatalf("bmsKey(noPort) = %q", bmsKey(noPort))
	}
}

// TestResolveBMSKey: уникальное (неколлизионное) имя КЛЮЧА НЕ меняется — сохраняет
// непрерывность исторических рядов Redis/PG; при коллизии двух одинаковых имён в
// коллекции ключ разводится по USB-порту (V1).
func TestResolveBMSKey(t *testing.T) {
	unique := bmsDevice{DeviceName: "AntBms 320 A/h (/dev/ttyUSB2)", Port: "/dev/ttyUSB2"}
	if k := resolveBMSKey(unique, map[string]int{unique.DeviceName: 1}); k != "AntBms 320 A/h (/dev/ttyUSB2)" {
		t.Fatalf("unique resolveBMSKey = %q, want исходный DeviceName", k)
	}

	a := bmsDevice{DeviceName: "AntBms 320 A/h", Port: "/dev/ttyUSB0"}
	b := bmsDevice{DeviceName: "AntBms 320 A/h", Port: "/dev/ttyUSB1"}
	coll := map[string]int{"AntBms 320 A/h": 2}
	if k := resolveBMSKey(a, coll); k != "AntBms 320 A/h@/dev/ttyUSB0" {
		t.Fatalf("collide resolveBMSKey(a) = %q, want @/dev/ttyUSB0", k)
	}
	if k := resolveBMSKey(b, coll); k != "AntBms 320 A/h@/dev/ttyUSB1" {
		t.Fatalf("collide resolveBMSKey(b) = %q, want @/dev/ttyUSB1", k)
	}
}

// TestRecomputeMinMaxCells: индексы/напряжения max/min пересчитываются из
// фактических cells_v, а не из метки кадра BMS (f[115]/f[118]), которая может
// не соответствовать реальным напряжениям. При равных напряжениях берётся
// первая ячейка (наименьший индекс) — подсветка стабильна. Индексы 1-based.
func TestRecomputeMinMaxCells(t *testing.T) {
	// В кадре BMS «ошибочно» заявлены max=2, min=16; фактические min — ячейка 10.
	d := bmsDevice{
		CellsV:     []float64{3.26, 3.26, 3.24, 3.25, 3.24, 3.25, 3.24, 3.25, 3.24, 3.237, 3.24, 3.25, 3.24, 3.25, 3.26, 3.26},
		MaxCellIdx: 2,
		MinCellIdx: 16,
		MaxCellV:   3.261,
		MinCellV:   3.234,
	}
	recomputeMinMaxCells(&d)
	if d.MaxCellIdx != 1 || d.MaxCellV != 3.26 {
		t.Fatalf("max = %d / %v, want первая max-ячейка: 1 / 3.26", d.MaxCellIdx, d.MaxCellV)
	}
	if d.MinCellIdx != 10 || d.MinCellV != 3.237 {
		t.Fatalf("min = %d / %v, want мин-ячейка: 10 / 3.237", d.MinCellIdx, d.MinCellV)
	}
	// Пустой cells_v — функция ничего не перезаписывает.
	empty := bmsDevice{CellsV: nil, MaxCellIdx: 2, MinCellIdx: 16}
	recomputeMinMaxCells(&empty)
	if empty.MaxCellIdx != 2 || empty.MinCellIdx != 16 {
		t.Fatalf("empty: индексы не должны меняться, got %d/%d", empty.MaxCellIdx, empty.MinCellIdx)
	}
}

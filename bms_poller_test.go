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

// TestPollAndSaveBMSValidEmpty: валидная ПУСТАЯ коллекция (updated>0, devices=[])
// по-прежнему чистит дашборд (это не сбой, а «действительно 0 батарей»).
func TestPollAndSaveBMSValidEmpty(t *testing.T) {
	s := testStore(t)
	cleanTestKeys(t, s)
	t.Cleanup(func() { cleanTestKeys(t, s) })

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

	if col := pollAndSaveBMS(context.Background(), s); col == nil {
		t.Fatal("pollAndSaveBMS при updated>0 вернул nil, want коллекцию")
	}

	cur, err := s.BMSCurrent()
	if err != nil {
		t.Fatalf("BMSCurrent: %v", err)
	}
	if _, ok := cur[seededName]; ok {
		t.Fatalf("seeded BMS не удалён валидной пустой коллекцией: %+v", cur)
	}
}

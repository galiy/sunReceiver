//go:build linux && ce308probe

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

// Диагностический пробник BLE-счётчика Энергомера СЕ308/СЕ208 с ЛОКАЛЬНОГО ПК.
// Не входит в обычную сборку (build tag ce308probe) и переиспользует боевой код
// (ce308_client.go/ce308.go/ce308_agent_linux.go), поэтому проверяет ровно тот
// путь подключения/чтения, что и прод-пулер.
//
// Запуск: см. ce308-probe.sh или
//   CE308_MAC=.. CE308_PIN=.. go test -tags ce308probe -run '^TestCE308Probe$' -v -count=1 -timeout 300s .
//
// MAC/PIN берутся из env CE308_MAC/CE308_PIN, иначе — из раздела ce308
// sunReceiver.json (CWD или рядом с бинарником).

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
)

func ce308ProbeLookup(t *testing.T) (mac, pin, name string) {
	t.Helper()
	mac = os.Getenv("CE308_MAC")
	pin = os.Getenv("CE308_PIN")
	if mac != "" && pin != "" {
		return mac, pin, mac
	}
	for _, p := range []string{"sunReceiver.json", configPath()} {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var cf struct {
			Ce308 *ce308Section `json:"ce308"`
		}
		if json.Unmarshal(b, &cf) != nil || cf.Ce308 == nil {
			continue
		}
		if cf.Ce308.Disabled != nil && *cf.Ce308.Disabled {
			continue
		}
		return cf.Ce308.MAC, cf.Ce308.PIN, cf.Ce308.Name
	}
	t.Fatalf("не найден MAC/PIN: задай CE308_MAC и CE308_PIN или положи sunReceiver.json с разделом ce308")
	return "", "", ""
}

// ce308ProbeRSSI читает RSSI активного соединения из BlueZ (org.bluez.Device1).
func ce308ProbeRSSI(mac string) (int16, error) {
	bus, err := dbus.SystemBus()
	if err != nil {
		return 0, err
	}
	id, err := ce308AdapterID()
	if err != nil {
		return 0, err
	}
	path := dbus.ObjectPath("/org/bluez/" + id + "/dev_" + strings.Replace(strings.ToUpper(mac), ":", "_", -1))
	var rssi int16
	if err := bus.Object("org.bluez", path).
		Call("org.freedesktop.DBus.Properties.Get", 0, "org.bluez.Device1", "RSSI").Store(&rssi); err != nil {
		return 0, err
	}
	return rssi, nil
}

func TestCE308Probe(t *testing.T) {
	mac, pin, name := ce308ProbeLookup(t)
	rounds := 5
	if v, err := strconv.Atoi(os.Getenv("CE308_PROBE_ROUNDS")); err == nil && v > 0 {
		rounds = v
	}
	if id, err := ce308AdapterID(); err == nil {
		log.Printf("probe: локальный BLE-адаптер BlueZ = %s", id)
	} else {
		log.Printf("probe: BLE-адаптер BlueZ не найден: %v", err)
	}
	log.Printf("probe: счётчик %s (%s), адрес %s", name, mac, mac)

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	start := time.Now()
	m, err := openCE308(mac, pin, ctx)
	if err != nil {
		t.Fatalf("probe: подключение к %s не удалось за %s: %v", mac, time.Since(start).Round(time.Millisecond), err)
	}
	defer func() {
		if e := m.Close(); e != nil {
			log.Printf("probe: disconnect: %v", e)
		}
	}()
	log.Printf("probe: ПОДКЛЮЧЕНО к %s за %s", mac, time.Since(start).Round(time.Millisecond))
	if rssi, err := ce308ProbeRSSI(mac); err == nil {
		log.Printf("probe: RSSI активного соединения = %d dBm", rssi)
	} else {
		log.Printf("probe: RSSI недоступен: %v", err)
	}

	ok := 0
	for i := 1; i <= rounds; i++ {
		t0 := time.Now()
		r, err := buildCE308Reads(m)
		dur := time.Since(t0).Round(time.Millisecond)
		if err != nil {
			log.Printf("probe: опрос %d/%d: ОШИБКА за %s: %v", i, rounds, dur, err)
			continue
		}
		if !ce308ReadsValid(r) {
			log.Printf("probe: опрос %d/%d: НЕВАЛИДНЫЕ данные за %s (U=%v I=%v P=%v Q=%v)", i, rounds, dur, r.Volta, r.Curre, r.ActiveP, r.ReactiveP)
			continue
		}
		v := valuesCE308(r)
		ok++
		log.Printf("probe: опрос %d/%d OK за %s: U=%.1f/%.1f/%.1f В I=%.1f/%.1f/%.1f А P=%+.1f Вт Q=%+.1f вар",
			i, rounds, dur,
			v[ce308L1Voltage], v[ce308L2Voltage], v[ce308L3Voltage],
			v[ce308L1Current], v[ce308L2Current], v[ce308L3Current],
			v[ce308ActiveP], v[ce308ReactP])
	}
	log.Printf("probe: ИТОГ: %d/%d успешных опросов", ok, rounds)
	if ok == 0 {
		t.Fatalf("probe: ни одного успешного опроса")
	}
	fmt.Println() // отделить вывод теста от служебных строк go test
}

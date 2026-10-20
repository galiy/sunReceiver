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
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Опрос сетевых инверторов через API «Малина26».
//
// Источник данных — только list (`GET /api/grid/status`) и снимок устройства
// (`GET /api/grid/device?serial=`). Значения в снимке уже приведены к физическим
// единицам (В, А, Вт, вар, Гц, °C, кВт·ч) — повторно не масштабируются.
// Собственные настройки инверторов (ip/logger_sn/type/name/placement) удалены:
// всё берётся из API. Время точки — last_seen из API.

const (
	gridCycleTimeout   = 4 * time.Second // дедлайн одного цикла внутри 5-секундного периода
	gridFetchWorkers   = 6
	gridConfigPrefix   = "stats.f.grid."
	gridPlacementHouse = "Дом"
	gridPlacementGar   = "Гараж"
)

// gridStatusItem — запись списка /api/grid/status.
type gridStatusItem struct {
	ID        int    `json:"id"`
	Gen       uint64 `json:"gen"`
	Serial    string `json:"serial"`
	Addr      string `json:"addr"`
	Kind      uint8  `json:"kind"`
	Status    uint8  `json:"status"`
	LastSeen  int64  `json:"last_seen"`
	Name      string `json:"name"`
	Placement string `json:"placement"`
}

type gridStatusResponse struct {
	Updated   int64            `json:"updated"`
	Inverters []gridStatusItem `json:"inverters"`
}

// gridDeviceField — поле снимка /api/grid/device.
type gridDeviceField struct {
	ID      uint16 `json:"id"`
	NameKey string `json:"name_key"`
	DescKey string `json:"desc_key"`
	Value   string `json:"value"`
	UnitKey string `json:"unit_key"`
	ND      bool   `json:"nd"`
}

type gridDeviceAlarm struct {
	Word    int    `json:"word"`
	Code    string `json:"code"`
	NameKey string `json:"name_key"`
}

// gridDeviceDoc — JSON-документ инвертора.
type gridDeviceDoc struct {
	Serial         string            `json:"serial"`
	Name           string            `json:"name"`
	Placement      string            `json:"placement"`
	Type           string            `json:"type"`
	Kind           uint8             `json:"kind"`
	Gen            uint64            `json:"gen"`
	Status         uint8             `json:"status"`
	InverterStatus uint8             `json:"inverter_status"`
	LastSeen       int64             `json:"last_seen"`
	EnergyToday    string            `json:"energy_today"`
	EnergyTotal    string            `json:"energy_total"`
	EnergyTodayND  bool              `json:"energy_today_nd"`
	EnergyTotalND  bool              `json:"energy_total_nd"`
	Fields         []gridDeviceField `json:"fields"`
	AlarmWords     []string          `json:"alarm_words"`
	Alarms         []gridDeviceAlarm `json:"alarms"`
}

// gridStatus читает список сетевых инверторов.
func (s *malina26Source) gridStatus(ctx context.Context) ([]gridStatusItem, error) {
	code, b, err := s.get(ctx, "/api/grid/status")
	if err != nil {
		return nil, err
	}
	if code != 200 {
		return nil, fmt.Errorf("grid/status: status %d", code)
	}
	var resp gridStatusResponse
	if err := json.Unmarshal(b, &resp); err != nil {
		return nil, fmt.Errorf("grid/status: %w", err)
	}
	return resp.Inverters, nil
}

// gridDevice читает снимок инвертора по серийнику.
func (s *malina26Source) gridDevice(ctx context.Context, serial string) (gridDeviceDoc, error) {
	code, b, err := s.get(ctx, "/api/grid/device?serial="+url.QueryEscape(serial))
	if err != nil {
		return gridDeviceDoc{}, err
	}
	if code != 200 {
		return gridDeviceDoc{}, fmt.Errorf("grid/device %s: status %d", serial, code)
	}
	var doc gridDeviceDoc
	if err := json.Unmarshal(b, &doc); err != nil {
		return gridDeviceDoc{}, fmt.Errorf("grid/device %s: %w", serial, err)
	}
	return doc, nil
}

// gridBrand нормализует тип инвертора. Приоритет — строковый type из снимка;
// резерв — kind из списка (0=Deye, 1=Sofar). Пустое/неизвестное — "".
func gridBrand(typ string, kind uint8) string {
	t := strings.ToLower(strings.TrimSpace(typ))
	if t != "" {
		switch t {
		case deyeBrand.name:
			return deyeBrand.name
		case sofarBrand.name:
			return sofarBrand.name
		}
		return "" // неизвестный непустой type — пропуск (резерв kind только при пустом)
	}
	switch kind {
	case 0:
		return deyeBrand.name
	case 1:
		return sofarBrand.name
	}
	return ""
}

// gridPlacement нормализует размещение: пустое — «Дом».
func gridPlacement(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return gridPlacementHouse
	}
	return p
}

// gridFieldTag переводит name_key API в тег нашего values. Второй результат —
// писать ли поле вообще (status/alarm-слова в values не входят).
func gridFieldTag(nameKey, brand string) (string, bool) {
	name := strings.TrimPrefix(strings.TrimSpace(nameKey), gridConfigPrefix)
	if name == "" {
		return "", false
	}
	switch name {
	case "temperature_1":
		if brand == deyeBrand.name {
			return "temperature_igbt", true
		}
		return "temperature_module", true
	case "temperature_2":
		if brand == deyeBrand.name {
			return "temperature_radiator", true
		}
		return "temperature_inner", true
	case "inverter_status", "alarm_w0", "alarm_w1", "alarm_w2", "alarm_w3", "alarm_w4", "alarm_w5":
		return "", false
	}
	for _, t := range commonContractTags {
		if t == name {
			return t, true
		}
	}
	return "", false
}

func gridFloat(s string) (float64, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// gridValues собирает наш универсальный контракт из снимка API.
func gridValues(doc gridDeviceDoc, brand string) valuesContract {
	vc := valuesContract{}
	for _, f := range doc.Fields {
		if f.ND {
			continue
		}
		tag, ok := gridFieldTag(f.NameKey, brand)
		if !ok {
			continue
		}
		v, ok := gridFloat(f.Value)
		if !ok {
			continue
		}
		vc[tag] = v
	}
	if !doc.EnergyTodayND {
		if v, ok := gridFloat(doc.EnergyToday); ok {
			vc["energy_today"] = v
		}
	}
	if !doc.EnergyTotalND {
		if v, ok := gridFloat(doc.EnergyTotal); ok {
			vc["energy_total"] = v
		}
	}
	return vc
}

// gridAlarmCodes возвращает стабильные коды активных аварий: первично alarms[],
// резерв — непустые alarm_words. Собственной расшифровки не заводим.
func gridAlarmCodes(doc gridDeviceDoc) []string {
	var out []string
	seen := map[string]bool{}
	add := func(code string) {
		code = strings.TrimSpace(code)
		if code == "" || seen[code] {
			return
		}
		seen[code] = true
		out = append(out, code)
	}
	for _, a := range doc.Alarms {
		add(a.Code)
	}
	if len(out) == 0 {
		for i, w := range doc.AlarmWords {
			w = strings.TrimSpace(w)
			if w == "" || w == "0x0000" || w == "0" {
				continue
			}
			add(fmt.Sprintf("alarm_w%d:%s", i, w))
		}
	}
	return out
}

// gridSnapshot строит снимок устройства из списка и снимка API.
func gridSnapshot(item gridStatusItem, doc gridDeviceDoc, brand string) deviceSnapshot {
	serial := strings.TrimSpace(item.Serial)
	if serial == "" {
		serial = strings.TrimSpace(doc.Serial)
	}
	serial = normalizeInverterSN(brand, serial)
	name := strings.TrimSpace(item.Name)
	if name == "" {
		name = strings.TrimSpace(doc.Name)
	}
	if name == "" {
		name = serial
	}
	placement := gridPlacement(item.Placement)
	if strings.TrimSpace(doc.Placement) != "" {
		placement = gridPlacement(doc.Placement)
	}
	last := item.LastSeen
	if doc.LastSeen > 0 {
		last = doc.LastSeen
	}
	ts := time.Unix(last, 0)
	return deviceSnapshot{
		Name:       name,
		Timestamp:  ts.Format(time.RFC3339),
		InverterSN: serial,
		Placement:  placement,
		Kind:       brand,
		Values:     gridValues(doc, brand),
	}
}

type gridFetchResult struct {
	item gridStatusItem
	doc  gridDeviceDoc
	err  error
}

// runGridPoll — цикл опроса всех сетевых инверторов через API malina26.
// Список читается каждый цикл, снимки — параллельно с ограничением и общим
// дедлайном; не успевшие устройства пропускают цикл.
func runGridPoll(store *redisStore, pg *pgStore, src *malina26Source, stop context.Context) {
	if src == nil {
		return
	}
	ticker := time.NewTicker(gridPollInterval)
	defer ticker.Stop()
	lastSeen := map[string]int64{}
	lastAlarms := map[string]map[string]bool{}
	newKeyWarned := map[string]bool{}
	for {
		select {
		case <-stop.Done():
			return
		case <-ticker.C:
		}
		if stop.Err() != nil {
			return
		}
		ctx, cancel := context.WithTimeout(stop, gridCycleTimeout)
		items, err := src.gridStatus(ctx)
		if err != nil {
			log.Printf("grid: список: %v", err)
			cancel()
			continue
		}
		results := make([]gridFetchResult, 0, len(items))
		var mu sync.Mutex
		var wg sync.WaitGroup
		sem := make(chan struct{}, gridFetchWorkers)
		for _, it := range items {
			if strings.TrimSpace(it.Serial) == "" {
				continue
			}
			wg.Add(1)
			go func(it gridStatusItem) {
				defer wg.Done()
				select {
				case sem <- struct{}{}:
				case <-ctx.Done():
					return
				}
				defer func() { <-sem }()
				doc, derr := src.gridDevice(ctx, it.Serial)
				mu.Lock()
				results = append(results, gridFetchResult{item: it, doc: doc, err: derr})
				mu.Unlock()
			}(it)
		}
		wg.Wait()
		cancel()
		active := make(map[string]bool, len(results))
		for _, r := range results {
			if r.err != nil {
				log.Printf("grid: %s: %v", r.item.Serial, r.err)
				continue
			}
			last := r.item.LastSeen
			if r.doc.LastSeen > 0 {
				last = r.doc.LastSeen
			}
			if last <= 0 {
				// Ещё не было успешного опроса в malina26: не пишем ни current, ни
				// историю, иначе устройство выглядело бы свежим с нулями.
				continue
			}
			brand := gridBrand(r.doc.Type, r.item.Kind)
			if brand == "" {
				log.Printf("grid: %s: неизвестный тип %q/kind=%d — пропуск", r.item.Serial, r.doc.Type, r.item.Kind)
				continue
			}
			snap := gridSnapshot(r.item, r.doc, brand)
			if snap.InverterSN == "" {
				continue
			}
			active[snap.InverterSN] = true
			if !newKeyWarned[snap.InverterSN] {
				newKeyWarned[snap.InverterSN] = true
				if !store.invKeyKnown(*invBrandByKind(brand), snap.InverterSN) {
					log.Printf("grid: новый ключ истории %s (%s, %s) — прежняя история не пересекается",
						snap.InverterSN, snap.Name, brand)
				}
			}
			ts, terr := time.Parse(time.RFC3339, snap.Timestamp)
			if terr != nil {
				ts = time.Now()
			}
			advance := last > lastSeen[snap.InverterSN]
			if err := store.saveInvSnapshotState(*invBrandByKind(brand), snap, ts, advance); err != nil {
				log.Printf("grid: сохранение %s: %v", snap.InverterSN, err)
				continue
			}
			if advance {
				lastSeen[snap.InverterSN] = last
			}
			// Ошибки/аварии: только появление, ключ (serial, kind, code).
			codes := gridAlarmCodes(r.doc)
			cur := make(map[string]bool, len(codes))
			for _, c := range codes {
				cur[c] = true
			}
			prev := lastAlarms[snap.InverterSN]
			if pg != nil {
				for _, code := range codes {
					if prev != nil && prev[code] {
						continue
					}
					if e := pg.InsertDeviceErrorNamed(snap.InverterSN, snap.Name, "inverter", code, code, ts); e != nil {
						log.Printf("error pg grid %s: %v", snap.InverterSN, e)
					}
				}
			}
			lastAlarms[snap.InverterSN] = cur
		}
		// Чистим состояние исчезнувших из списка устройств (без утечки карт).
		for k := range lastSeen {
			if !active[k] {
				delete(lastSeen, k)
			}
		}
		for k := range lastAlarms {
			if !active[k] {
				delete(lastAlarms, k)
			}
		}
		for k := range newKeyWarned {
			if !active[k] {
				delete(newKeyWarned, k)
			}
		}
	}
}

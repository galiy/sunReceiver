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

// mapsettings.go — модуль «Настройки МАП»: чтение, инспектирование и
// редактирование ячеек МАП через веб-UI дашборда.
//
// Источник каталога ячеек — mapsettings/catalog.json, сгенерированный из
// protocol_MAP_cells_2026_07_15.doc (последняя версия документации; Malina web
// API как эталон НЕ используется). Каталог встроен в бинарник через go:embed.
//
// Запись выполняется по протоколу МАП в Modbus-обёртке со служебным обрамлением,
// как это делает mapd: ComMAP_EEPromWR (0x03) → запись ячейки(ек) →
// ComMAP_Call_load_EEProm (0x07), затем чтение-обратно (верификация).

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"math"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed catalog.json
var mapSettingsCatalogJSON []byte

// Режимы МАП (от них зависит набор отображаемых параметров).
const (
	mapModeTitanator = "titanator"
	mapModeDominator = "dominator"
)

// Ячейки времени (protocol_MAP_cells_2026_07_15.doc, стр.1394–1396).
const (
	mapCellTimeCyr   = 0x1B6 // _LCD_TimeCyr = (hh<<3)|(mm/10) — EEPROM
	mapCellTimeMin   = 0x44B // _TimeCyr_MINUT = mm%10 — RAM
	mapCellPutEEPROM = 0x403 // _put_eeprom — флаг записи в EEPROM
)

// mapBit — битовое поле ячейки.
type mapBit struct {
	Bit  int    `json:"bit"`
	Name string `json:"name"`
}

// mapParamSpec — дескриптор одного параметра (ячейки) из каталога.
type mapParamSpec struct {
	Key    string            `json:"key"`
	Cell   string            `json:"cell"`
	Addr   uint16            `json:"addr"`
	Width  int               `json:"width"`
	Kind   string            `json:"kind"`   // ram|eeprom
	Access string            `json:"access"` // ro|rw
	Name   string            `json:"name"`   // русское имя
	Group  string            `json:"group"`  // логический блок
	Modes  []string          `json:"modes"`  // titanator|dominator
	Unit   string            `json:"unit"`
	Scale  float64           `json:"scale"`
	Offset float64           `json:"offset"`
	Min    *float64          `json:"min"`
	Max    *float64          `json:"max"`
	Desc   string            `json:"desc"`
	Enum   map[string]string `json:"enum"`
	Bits   []mapBit          `json:"bits"`
	// Order задаёт порядок байт для Width==2: "hl" (первая ячейка — старший
	// байт, по умолчанию) или "lh" (первая ячейка — младший байт).
	Order string `json:"order"`
}

type mapSettingsCatalog struct {
	Source string         `json:"source"`
	Title  string         `json:"title"`
	Params []mapParamSpec `json:"params"`
}

var (
	mapSettingsCatalogOnce sync.Once
	mapSettingsCatalogData *mapSettingsCatalog
	mapSettingsCatalogErr  error
)

// loadMapSettingsCatalog разбирает встроенный JSON-каталог (единожды).
func loadMapSettingsCatalog() (*mapSettingsCatalog, error) {
	mapSettingsCatalogOnce.Do(func() {
		var c mapSettingsCatalog
		if err := json.Unmarshal(mapSettingsCatalogJSON, &c); err != nil {
			mapSettingsCatalogErr = fmt.Errorf("map-settings: разбор каталога: %w", err)
			return
		}
		for i := range c.Params {
			p := &c.Params[i]
			if p.Width <= 0 {
				p.Width = 1
			}
			if p.Scale == 0 {
				p.Scale = 1
			}
			if p.Access == "" {
				p.Access = "ro"
			}
		}
		c.Params = normalizeCatalogPairs(c.Params)
		mapSettingsCatalogData = &c
	})
	return mapSettingsCatalogData, mapSettingsCatalogErr
}

// cellPairRole разбирает имя ячейки на базовое имя пары и роль байта:
// 'L' — младший байт (_L/_VL/_L[n]), 'H' — старший (_H/_VH/_H[n]), 0 — не пара.
func cellPairRole(cell string) (string, byte) {
	head, indexed := cell, ""
	if i := strings.LastIndexByte(cell, '['); i >= 0 {
		head, indexed = cell[:i], cell[i:]
	}
	switch {
	case strings.HasSuffix(head, "_VL"):
		return head[:len(head)-3] + indexed, 'L'
	case strings.HasSuffix(head, "_L"):
		return head[:len(head)-2] + indexed, 'L'
	case strings.HasSuffix(head, "_VH"):
		return head[:len(head)-3] + indexed, 'H'
	case strings.HasSuffix(head, "_H"):
		return head[:len(head)-2] + indexed, 'H'
	}
	return "", 0
}

// normalizeCatalogPairs приводит парные ячейки _L/_H (_VL/_VH) к одному
// параметру width==2 с явным порядком байт. Идемпотентна: если каталог уже
// нормализован, ничего не меняет.
//
// Правила:
//   - если младший и старший байты на СОСЕДНИХ адресах — объединяем в один
//     width==2 (addr — меньший, order "lh" если младший ниже по адресу, иначе
//     "hl"), партнёр удаляется;
//   - если адреса НЕ соседние — оба остаются width==1 (объединять нельзя);
//   - width==2 без order и без пары — order по роли имени; при пересечении с
//     соседним адресом — width==1.
func normalizeCatalogPairs(params []mapParamSpec) []mapParamSpec {
	type pair struct{ low, high int }
	byBase := map[string]*pair{}
	for i := range params {
		base, role := cellPairRole(params[i].Cell)
		if role == 0 {
			continue
		}
		e := byBase[base]
		if e == nil {
			e = &pair{low: -1, high: -1}
			byBase[base] = e
		}
		if role == 'L' {
			e.low = i
		} else {
			e.high = i
		}
	}
	removed := make([]bool, len(params))
	for _, e := range byBase {
		if e.low < 0 || e.high < 0 {
			continue
		}
		aL, aH := params[e.low].Addr, params[e.high].Addr
		if aL > aH {
			aL, aH = aH, aL
		}
		if aH-aL != 1 {
			params[e.low].Width = 1
			params[e.high].Width = 1
			continue
		}
		if params[e.low].Addr < params[e.high].Addr {
			params[e.low].Width = 2
			params[e.low].Order = "lh"
			removed[e.high] = true
		} else {
			params[e.high].Width = 2
			params[e.high].Order = "hl"
			removed[e.low] = true
		}
	}
	// Второй проход: оставшиеся width==2 без order; при пересечении — width==1.
	addrSet := map[uint16]bool{}
	for i := range params {
		if !removed[i] {
			addrSet[params[i].Addr] = true
		}
	}
	for i := range params {
		if removed[i] {
			continue
		}
		p := &params[i]
		if p.Width == 2 {
			// Пересечение со следующей ячейкой недопустимо: только один байт.
			if addrSet[p.Addr+1] {
				p.Width = 1
				p.Order = ""
				continue
			}
			if _, role := cellPairRole(p.Cell); role == 'L' {
				p.Order = "lh"
			} else if role == 'H' {
				p.Order = "hl"
			} else if p.Order == "" {
				p.Order = "hl"
			}
		}
	}
	out := make([]mapParamSpec, 0, len(params))
	seen := map[string]bool{}
	for i := range params {
		if removed[i] {
			continue
		}
		if seen[params[i].Key] {
			params[i].Key = fmt.Sprintf("%s_%03x", params[i].Key, params[i].Addr)
		}
		seen[params[i].Key] = true
		out = append(out, params[i])
	}
	return out
}

// paramInMode сообщает, актуален ли параметр для выбранного режима.
func paramInMode(p mapParamSpec, mode string) bool {
	if len(p.Modes) == 0 {
		return true
	}
	for _, m := range p.Modes {
		m = strings.ToLower(strings.TrimSpace(m))
		if m == mode || m == "both" || m == "all" {
			return true
		}
	}
	return false
}

// normalizeMapMode приводит режим к каноническому виду.
func normalizeMapMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case mapModeTitanator, "титанатор", "titan":
		return mapModeTitanator
	default:
		return mapModeDominator
	}
}

// --- Чтение снимка ячеек ---

// neededAddrs возвращает отсортированный уникальный список байтовых адресов,
// необходимых для параметров (для Width==2 — плюс следующий байт).
func neededAddrs(params []mapParamSpec) []uint16 {
	set := map[uint16]struct{}{}
	for _, p := range params {
		set[p.Addr] = struct{}{}
		if p.Width >= 2 {
			set[p.Addr+1] = struct{}{}
		}
	}
	out := make([]uint16, 0, len(set))
	for a := range set {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// readCellRanges читает ячейки (по списку адресов) блоками. Соседние адреса
// (и близкие с зазором до 16 байт) объединяются в один Modbus-запрос; блок
// режется по 240 байт (лимит ReadRegisters — 120 слов). Возвращает карту
// адрес→байт и список ошибок по блокам (частичный снимок допускается).
func readCellRanges(ctx context.Context, c *Client, addrs []uint16) (map[uint16]byte, []string) {
	cells := make(map[uint16]byte, len(addrs))
	var errs []string
	if len(addrs) == 0 {
		return cells, errs
	}
	const maxBytes = 240
	const gapTolerance = 16
	// Группируем в диапазоны [start,end] с допустимым зазором.
	type span struct{ start, end int }
	var spans []span
	start := int(addrs[0])
	end := start
	for _, a := range addrs[1:] {
		x := int(a)
		if x-end-1 <= gapTolerance {
			end = x
			continue
		}
		spans = append(spans, span{start, end})
		start, end = x, x
	}
	spans = append(spans, span{start, end})

	for _, sp := range spans {
		for from := sp.start; from <= sp.end; {
			to := sp.end
			if to-from+1 > maxBytes {
				to = from + maxBytes - 1
			}
			n := to - from + 1
			count := uint16((n + 1) / 2)
			raw, err := c.ReadRegisters(ctx, uint16(from), count)
			if err != nil {
				errs = append(errs, fmt.Sprintf("чтение 0x%03X..0x%03X: %v", from, to, err))
				from = to + 1
				continue
			}
			for i := 0; i < n && i < len(raw); i++ {
				cells[uint16(from+i)] = raw[i]
			}
			from = to + 1
		}
	}
	return cells, errs
}

// rawValue собирает сырое значение параметра из карты ячеек.
func rawValue(p mapParamSpec, cells map[uint16]byte) (int, bool) {
	b0, ok := cells[p.Addr]
	if !ok {
		return 0, false
	}
	if p.Width < 2 {
		return int(b0), true
	}
	b1, ok := cells[p.Addr+1]
	if !ok {
		return 0, false
	}
	if p.Order == "lh" {
		return int(b1)<<8 | int(b0), true
	}
	return int(b0)<<8 | int(b1), true
}

// displayValue переводит сырое значение в отображаемое (scale/offset).
func displayValue(p mapParamSpec, raw int) float64 {
	return mapRound3(float64(raw)*p.Scale + p.Offset)
}

// splitRaw раскладывает отображаемое значение в 1–2 байта ячейки.
func splitRaw(p mapParamSpec, value float64) ([]byte, error) {
	if p.Scale == 0 {
		return nil, fmt.Errorf("нулевой масштаб")
	}
	raw := int(math.Round((value - p.Offset) / p.Scale))
	if raw < 0 {
		return nil, fmt.Errorf("значение %g вне допустимого (получается %d)", value, raw)
	}
	if p.Width < 2 {
		if raw > 255 {
			return nil, fmt.Errorf("значение %g больше байта", value)
		}
		return []byte{byte(raw)}, nil
	}
	if raw > 65535 {
		return nil, fmt.Errorf("значение %g больше слова", value)
	}
	hi, lo := byte(raw>>8), byte(raw)
	if p.Order == "lh" {
		return []byte{lo, hi}, nil
	}
	return []byte{hi, lo}, nil
}

// enumText возвращает текстовую расшифровку значения (enum или битовые поля).
func enumText(p mapParamSpec, raw int) string {
	if len(p.Enum) > 0 {
		if s, ok := p.Enum[strconv.Itoa(raw)]; ok {
			return s
		}
	}
	if len(p.Bits) > 0 {
		var on []string
		for _, b := range p.Bits {
			if raw&(1<<uint(b.Bit)) != 0 {
				on = append(on, b.Name)
			}
		}
		return strings.Join(on, "; ")
	}
	return ""
}

func mapRound3(v float64) float64 { return math.Round(v*1000) / 1000 }

// mapOption — вариант перечислимого параметра (значение + подпись из документа).
type mapOption struct {
	Value float64 `json:"value"`
	Label string  `json:"label"`
}

// mapSettingView — параметр в ответе API.
type mapSettingView struct {
	Key      string      `json:"key"`
	Cell     string      `json:"cell"`
	Addr     string      `json:"addr"`
	Name     string      `json:"name"`
	Unit     string      `json:"unit"`
	Kind     string      `json:"kind"`
	Access   string      `json:"access"`
	Writable bool        `json:"writable"`
	Value    *float64    `json:"value"`
	Raw      *int        `json:"raw"`
	Text     string      `json:"text,omitempty"`
	Min      *float64    `json:"min,omitempty"`
	Max      *float64    `json:"max,omitempty"`
	Desc     string      `json:"desc,omitempty"`
	Help     string      `json:"help,omitempty"`
	Error    string      `json:"error,omitempty"`
	Options  []mapOption `json:"options,omitempty"`
}

// paramOptions строит список вариантов для перечислимого параметра: значение —
// в отображаемых единицах (с учётом scale/offset), подпись — из документа.
func paramOptions(p mapParamSpec) []mapOption {
	if len(p.Enum) == 0 {
		return nil
	}
	keys := make([]int, 0, len(p.Enum))
	for k := range p.Enum {
		if n, err := strconv.Atoi(k); err == nil {
			keys = append(keys, n)
		}
	}
	sort.Ints(keys)
	opts := make([]mapOption, 0, len(keys))
	for _, k := range keys {
		opts = append(opts, mapOption{
			Value: mapRound3(float64(k)*p.Scale + p.Offset),
			Label: p.Enum[strconv.Itoa(k)],
		})
	}
	return opts
}

// paramHelp собирает текст подсказки из документации: описание, единица/формула,
// расшифровки значений (enum) и битовые поля.
func paramHelp(p mapParamSpec) string {
	var b strings.Builder
	b.WriteString(p.Name)
	if p.Cell != "" {
		fmt.Fprintf(&b, " (%s, адрес 0x%03X)", p.Cell, p.Addr)
	}
	if p.Desc != "" {
		b.WriteString("\n\n")
		b.WriteString(p.Desc)
	}
	if p.Unit != "" || p.Scale != 1 || p.Offset != 0 {
		fmt.Fprintf(&b, "\n\nЕдиница: %s", p.Unit)
		if p.Scale != 1 || p.Offset != 0 {
			fmt.Fprintf(&b, "; отображение = значение×%g%+g", p.Scale, p.Offset)
		}
	}
	if len(p.Enum) > 0 {
		keys := make([]int, 0, len(p.Enum))
		for k := range p.Enum {
			if n, err := strconv.Atoi(k); err == nil {
				keys = append(keys, n)
			}
		}
		sort.Ints(keys)
		b.WriteString("\n\nЗначения:")
		for _, k := range keys {
			fmt.Fprintf(&b, "\n• %d — %s", k, p.Enum[strconv.Itoa(k)])
		}
	}
	if len(p.Bits) > 0 {
		bits := append([]mapBit(nil), p.Bits...)
		sort.Slice(bits, func(i, j int) bool { return bits[i].Bit < bits[j].Bit })
		b.WriteString("\n\nБиты:")
		for _, bit := range bits {
			fmt.Fprintf(&b, "\n• бит %d — %s", bit.Bit, bit.Name)
		}
	}
	return b.String()
}

type mapSettingsGroup struct {
	Name   string           `json:"name"`
	Params []mapSettingView `json:"params"`
}

type mapSettingsActionView struct {
	Key     string `json:"key"`
	Name    string `json:"name"`
	Desc    string `json:"desc"`
	Confirm bool   `json:"confirm"`
}

// mapSettingsSnapshot — полный ответ GET /api/map-settings.
type mapSettingsSnapshot struct {
	Mode     string                  `json:"mode"`
	IP       string                  `json:"ip"`
	Port     int                     `json:"port"`
	Unit     int                     `json:"unit"`
	ReadAt   string                  `json:"read_at"`
	Settings []mapSettingsGroup      `json:"settings"`
	Monitor  []mapSettingsGroup      `json:"monitor"`
	Actions  []mapSettingsActionView `json:"actions"`
	Errors   []string                `json:"errors,omitempty"`
}

// buildSnapshot формирует снимок: настройки (rw) и мониторинг (ro) по группам.
func buildSnapshot(mode, ip string, port int, unit byte, params []mapParamSpec, cells map[uint16]byte, readErrs []string) *mapSettingsSnapshot {
	snap := &mapSettingsSnapshot{
		Mode: mode, IP: ip, Port: port, Unit: int(unit),
		ReadAt:  time.Now().Format(time.RFC3339),
		Actions: mapSettingsActionsView(),
		Errors:  readErrs,
	}
	snap.Settings = []mapSettingsGroup{}
	snap.Monitor = []mapSettingsGroup{}
	add := func(section *[]mapSettingsGroup, index map[string]int, group string) int {
		if idx, ok := index[group]; ok {
			return idx
		}
		*section = append(*section, mapSettingsGroup{Name: group})
		idx := len(*section) - 1
		index[group] = idx
		return idx
	}
	setIdx := map[string]int{}
	monIdx := map[string]int{}
	for _, p := range params {
		if !paramInMode(p, mode) {
			continue
		}
		v := mapSettingView{
			Key: p.Key, Cell: p.Cell, Addr: fmt.Sprintf("0x%03X", p.Addr),
			Name: p.Name, Unit: p.Unit, Kind: p.Kind, Access: p.Access,
			Writable: p.Access == "rw", Min: p.Min, Max: p.Max, Desc: p.Desc,
			Help: paramHelp(p),
		}
		raw, ok := rawValue(p, cells)
		if !ok {
			v.Error = "нет данных"
		} else {
			r := raw
			val := displayValue(p, raw)
			v.Raw = &r
			v.Value = &val
			v.Text = enumText(p, raw)
		}
		if p.Access == "rw" {
			if opts := paramOptions(p); len(opts) > 0 {
				v.Options = opts
			}
			i := add(&snap.Settings, setIdx, p.Group)
			snap.Settings[i].Params = append(snap.Settings[i].Params, v)
		} else {
			i := add(&snap.Monitor, monIdx, p.Group)
			snap.Monitor[i].Params = append(snap.Monitor[i].Params, v)
		}
	}
	return snap
}

// readMapSettingsSnapshot читает все параметры (для режима) с МАП.
func readMapSettingsSnapshot(ctx context.Context, target mapSettingsTarget) (*mapSettingsSnapshot, error) {
	cat, err := loadMapSettingsCatalog()
	if err != nil {
		return nil, err
	}
	params := make([]mapParamSpec, 0, len(cat.Params))
	for _, p := range cat.Params {
		if paramInMode(p, target.mode) {
			params = append(params, p)
		}
	}
	c := &Client{Address: target.address(), Unit: target.unit}
	defer c.Close()
	addrs := neededAddrs(params)
	cells, errs := readCellRanges(ctx, c, addrs)
	return buildSnapshot(target.mode, target.ip, target.port, target.unit, params, cells, errs), nil
}

// --- Запись ---

// mapSettingsTarget — адрес/режим, с которыми работает UI (задаются клиентом).
type mapSettingsTarget struct {
	mode string
	ip   string
	port int
	unit byte
}

func (t mapSettingsTarget) address() string {
	return net.JoinHostPort(t.ip, strconv.Itoa(t.port))
}

// validateMapTarget проверяет host/port, чтобы клиент не мог обратиться куда угодно.
func validateMapTarget(mode, ip string, port int) (mapSettingsTarget, error) {
	ip = strings.TrimSpace(ip)
	if ip == "" {
		return mapSettingsTarget{}, fmt.Errorf("не задан IP МАП")
	}
	for _, r := range ip {
		if !(r == '.' || r == '-' || r == ':' || (r >= '0' && r <= '9') ||
			(r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')) {
			return mapSettingsTarget{}, fmt.Errorf("недопустимый IP/хост: %q", ip)
		}
	}
	if port <= 0 || port > 65535 {
		return mapSettingsTarget{}, fmt.Errorf("недопустимый порт %d", port)
	}
	return mapSettingsTarget{mode: normalizeMapMode(mode), ip: ip, port: port, unit: 1}, nil
}

// applyMapSettings применяет изменения (только изменённые параметры) со служебным
// обрамлением и верификацией. changes: key → новое отображаемое значение.
func applyMapSettings(ctx context.Context, target mapSettingsTarget, changes map[string]float64) (map[string]string, error) {
	cat, err := loadMapSettingsCatalog()
	if err != nil {
		return nil, err
	}
	byKey := make(map[string]mapParamSpec, len(cat.Params))
	for _, p := range cat.Params {
		if paramInMode(p, target.mode) {
			byKey[p.Key] = p
		}
	}
	type write struct {
		addr uint16
		data []byte
	}
	var writes []write
	results := map[string]string{}
	for key, val := range changes {
		p, ok := byKey[key]
		if !ok {
			results[key] = "неизвестный параметр"
			continue
		}
		if p.Access != "rw" {
			results[key] = "параметр только для чтения"
			continue
		}
		// Ячейки 0x000..0x004 — служебная область команд/идентификации: запись
		// настройкой недопустима (команды идут отдельным путём, см. actions).
		if p.Addr <= 4 {
			results[key] = "служебная ячейка — запись запрещена"
			continue
		}
		if p.Min != nil && val < *p.Min {
			results[key] = fmt.Sprintf("ниже минимума %g", *p.Min)
			continue
		}
		if p.Max != nil && val > *p.Max {
			results[key] = fmt.Sprintf("выше максимума %g", *p.Max)
			continue
		}
		data, err := splitRaw(p, val)
		if err != nil {
			results[key] = err.Error()
			continue
		}
		for i, b := range data {
			writes = append(writes, write{addr: p.Addr + uint16(i), data: []byte{b}})
		}
	}
	if len(writes) == 0 {
		return results, nil
	}

	c := &Client{Address: target.address(), Unit: target.unit}
	defer c.Close()
	// 1) разрешение записи (как mapd: команда 0x03).
	if err := c.WriteCommand(ctx, ComMAPEEPromWR); err != nil {
		return nil, fmt.Errorf("разрешение записи (ComMAP_EEPromWR): %w", err)
	}
	// 2) запись ячеек.
	for _, w := range writes {
		if err := c.WriteCell(ctx, w.addr, w.data[0]); err != nil {
			return results, fmt.Errorf("запись 0x%03X: %w", w.addr, err)
		}
	}
	// 3) фиксация EEPROM (как mapd: команда 0x07).
	if err := c.WriteCommand(ctx, ComMAPCallLoadEEProm); err != nil {
		return results, fmt.Errorf("фиксация (ComMAP_Call_load_EEProm): %w", err)
	}
	// 4) чтение-обратно затронутых ячеек.
	addrs := make([]uint16, 0, len(writes))
	for _, w := range writes {
		addrs = append(addrs, w.addr)
	}
	cells, _ := readCellRanges(ctx, c, uniqueU16(addrs))
	for _, w := range writes {
		got, ok := cells[w.addr]
		if !ok {
			continue
		}
		if got != w.data[0] {
			key := fmt.Sprintf("0x%03X", w.addr)
			results[key] = fmt.Sprintf("верификация: получено %d, ждали %d", got, w.data[0])
		}
	}
	return results, nil
}

func uniqueU16(in []uint16) []uint16 {
	seen := map[uint16]struct{}{}
	out := in[:0:0]
	for _, v := range in {
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// --- Управляющие воздействия ---

type mapAction struct {
	Key     string
	Name    string
	Desc    string
	Cmd     byte
	Confirm bool
}

// mapSettingsActions — команды МАП (запись по адресу 0). Не являются изменением
// настроек; состав — по protocol_MAP_cells_2026_07_15.doc.
var mapSettingsActions = []mapAction{
	{Key: "on", Name: "Включить МАП", Desc: "Включить генерацию (ComMAP_ON)", Cmd: ComMAPON, Confirm: true},
	{Key: "off", Name: "Выключить МАП", Desc: "Выключить генерацию (ComMAP_OFF)", Cmd: ComMAPOFF, Confirm: true},
	{Key: "charge_on", Name: "Включить заряд", Desc: "Включить заряд от сети (ComMAP_ChargeON)", Cmd: ComMAPChargeON, Confirm: true},
	{Key: "charge_off", Name: "Выключить заряд", Desc: "Выключить заряд от сети (ComMAP_ChargeOFF)", Cmd: ComMAPChargeOFF, Confirm: true},
	{Key: "stat_reset", Name: "Сброс статистики", Desc: "Сбросить накопленную статистику (ComMAP_StatReset)", Cmd: ComMAPStatReset, Confirm: true},
	{Key: "disch_off", Name: "Запретить разряд", Desc: "Выключение генерации по полному разряду (ComMAP_DischOff)", Cmd: ComMAPDischOff, Confirm: true},
	{Key: "load_eeprom", Name: "Загрузить настройки из EEPROM", Desc: "Инициализация данных из EEPROM (ComMAP_Call_load_EEProm)", Cmd: ComMAPCallLoadEEProm, Confirm: false},
	{Key: "reset", Name: "Сброс контроллера", Desc: "Полная перезагрузка МАП — крайняя мера (ComMAP_Reset)", Cmd: ComMAPReset, Confirm: true},
}

func mapSettingsActionsView() []mapSettingsActionView {
	out := make([]mapSettingsActionView, 0, len(mapSettingsActions))
	for _, a := range mapSettingsActions {
		out = append(out, mapSettingsActionView{Key: a.Key, Name: a.Name, Desc: a.Desc, Confirm: a.Confirm})
	}
	return out
}

// runMapSettingsAction выполняет команду по ключу.
func runMapSettingsAction(ctx context.Context, target mapSettingsTarget, key string) error {
	var cmd byte
	found := false
	for _, a := range mapSettingsActions {
		if a.Key == key {
			cmd, found = a.Cmd, true
			break
		}
	}
	if !found {
		return fmt.Errorf("неизвестное действие %q", key)
	}
	c := &Client{Address: target.address(), Unit: target.unit}
	defer c.Close()
	// Сервисные команды, меняющие EEPROM (сброс статистики/загрузка), обрамляем
	// разрешением записи — как и обычную запись.
	if key == "stat_reset" || key == "load_eeprom" {
		if err := c.WriteCommand(ctx, ComMAPEEPromWR); err != nil {
			return err
		}
	}
	return c.WriteCommand(ctx, cmd)
}

// --- Текущее время (отдельная форма) ---

type mapTimeState struct {
	Hour   int    `json:"hour"`
	Minute int    `json:"minute"`
	Cell1  int    `json:"cell_time_cyr"`
	Cell2  int    `json:"cell_time_min"`
	Raw    string `json:"raw"`
}

// readMapTime читает текущее время МАП из ячеек _LCD_TimeCyr/_TimeCyr_MINUT.
func readMapTime(ctx context.Context, target mapSettingsTarget) (*mapTimeState, error) {
	c := &Client{Address: target.address(), Unit: target.unit}
	defer c.Close()
	cells, errs := readCellRanges(ctx, c, []uint16{mapCellTimeCyr, mapCellTimeMin})
	if len(errs) > 0 {
		return nil, fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	a, ok1 := cells[mapCellTimeCyr]
	b, ok2 := cells[mapCellTimeMin]
	if !ok1 || !ok2 {
		return nil, fmt.Errorf("ячейки времени недоступны")
	}
	hh := int(a) >> 3
	mm := (int(a)&0x07)*10 + int(b)%10
	return &mapTimeState{
		Hour: hh, Minute: mm, Cell1: int(a), Cell2: int(b),
		Raw: fmt.Sprintf("0x%03X=%d, 0x%03X=%d", mapCellTimeCyr, a, mapCellTimeMin, b),
	}, nil
}

// writeMapTime записывает время МАП (часы + десятки/единицы минут) с обрамлением.
func writeMapTime(ctx context.Context, target mapSettingsTarget, hour, minute int) error {
	if hour < 0 || hour > 23 || minute < 0 || minute > 59 {
		return fmt.Errorf("некорректное время %02d:%02d", hour, minute)
	}
	c1 := byte((hour << 3) | (minute / 10))
	c2 := byte(minute % 10)
	c := &Client{Address: target.address(), Unit: target.unit}
	defer c.Close()
	if err := c.WriteCommand(ctx, ComMAPEEPromWR); err != nil {
		return fmt.Errorf("разрешение записи: %w", err)
	}
	if err := c.WriteCell(ctx, mapCellTimeCyr, c1); err != nil {
		return fmt.Errorf("запись 0x%03X: %w", mapCellTimeCyr, err)
	}
	if err := c.WriteCell(ctx, mapCellTimeMin, c2); err != nil {
		return fmt.Errorf("запись 0x%03X: %w", mapCellTimeMin, err)
	}
	if err := c.WriteCommand(ctx, ComMAPCallLoadEEProm); err != nil {
		return fmt.Errorf("фиксация: %w", err)
	}
	cells, _ := readCellRanges(ctx, c, []uint16{mapCellTimeCyr, mapCellTimeMin})
	if got, ok := cells[mapCellTimeCyr]; ok && got != c1 {
		return fmt.Errorf("верификация 0x%03X: получено %d, ждали %d", mapCellTimeCyr, got, c1)
	}
	if got, ok := cells[mapCellTimeMin]; ok && got != c2 {
		return fmt.Errorf("верификация 0x%03X: получено %d, ждали %d", mapCellTimeMin, got, c2)
	}
	return nil
}

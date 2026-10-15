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
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Пулер МАП/КЭС через новый HTTP-API ПАК «Малина26» (method=malina26).
//
// Протокол (см. проект cleaning.malina, docs/api-auth.md, docs/map-reader.md,
// docs/map-contract.md, docs/kes-contract.md):
//   - POST /api/token {password} → {token,expires}; далее Authorization: Bearer;
//   - GET /api/{map,kes}/devices → [{type,id,gen,label}];
//   - GET /api/{map,kes}/raw?id=&gen= → бинарный контракт (application/octet-stream);
//     несовпадение id+gen → 404 (потеря устройства, ищем заново).
// serial «своего» МАП — второй токен label (label = "<Модель> · <serial> · <port> · <baud>").
//
// Версии контрактов жёстко пиннятся: при расхождении payload отвергается (лог),
// не паникуем. Где смотреть новые версии — приватная заметка .kilo/malina26-contracts.md.

const (
	malina26MapContractVersion = 5   // MapData version (daemons/map-reader/contract.go)
	malina26MapNFields         = 402 // documentedCells
	malina26KesrVersion        = 1   // KESR version (daemons/kes-reader)

	malina26RequestTimeout = 3 * time.Second
	malina26ClientTimeout  = 5 * time.Second

	// Порядок МАП на дашборде (инверторы — 0..N, MPPT — mpptOrderBase+slot).
	malina26MapOrder = 9000
)

// malina26Source — клиент API «Малина26»: токен, найденный МАП (id+gen) и КЭС.
type malina26Source struct {
	baseURL  string
	host     string
	name     string
	password string
	serial   string // серийник «своего» МАП (число строкой)

	client *http.Client

	mu     sync.Mutex
	token  string
	mapID  int
	mapGen uint64
	haveMap bool
}

// malina26 — глобальный источник (method=malina26); nil — метод не выбран.
var malina26 *malina26Source

// loadMalina26 собирает источник из блока map.malina26. Возвращает nil, если
// method != malina26, раздел отключён или блок неполный (тогда опрос не запускается).
func loadMalina26(sec *mapSection) *malina26Source {
	if sec == nil || (sec.Disabled != nil && *sec.Disabled) {
		return nil
	}
	if sec.Method != "malina26" {
		return nil
	}
	m := sec.Malina26
	if m == nil {
		return nil
	}
	if m.Host == "" || m.Password == "" || strings.TrimSpace(m.Serial) == "" {
		log.Printf("map malina26: блок неполный (нужны host, password, serial) — опрос не запускается")
		return nil
	}
	port := m.Port
	if port == 0 {
		port = 80
	}
	scheme := "http"
	if port == 443 {
		scheme = "https"
	}
	return &malina26Source{
		baseURL:  fmt.Sprintf("%s://%s:%d", scheme, m.Host, port),
		host:     m.Host,
		name:     "MAP (батарея/сеть)",
		password: m.Password,
		serial:   strings.TrimSpace(m.Serial),
		client:   &http.Client{Timeout: malina26ClientTimeout},
	}
}

// ---- HTTP ----

// loginLocked выпускает токен (вызывать под s.mu). Наблюдались плавающие 401 на
// самом логине при верном пароле — делаем несколько попыток.
func (s *malina26Source) loginLocked(ctx context.Context) error {
	body, _ := json.Marshal(map[string]string{"password": s.password})
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		rctx, cancel := context.WithTimeout(ctx, malina26RequestTimeout)
		req, err := http.NewRequestWithContext(rctx, http.MethodPost, s.baseURL+"/api/token", bytes.NewReader(body))
		if err == nil {
			req.Header.Set("Content-Type", "application/json")
			var resp *http.Response
			resp, err = s.client.Do(req)
			if err == nil {
				b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
				resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					var tok struct {
						Token string `json:"token"`
					}
					if json.Unmarshal(b, &tok) == nil && tok.Token != "" {
						s.token = tok.Token
						cancel()
						return nil
					}
					err = fmt.Errorf("token: пустой ответ")
				} else {
					err = fmt.Errorf("token: status %d", resp.StatusCode)
				}
			}
		}
		cancel()
		lastErr = err
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
	}
	return fmt.Errorf("malina26 login: %w", lastErr)
}

// get выполняет GET с Bearer-токеном. При 401 — одна переавторизация и повтор.
// Возвращает HTTP-статус и тело.
func (s *malina26Source) get(ctx context.Context, path string) (int, []byte, error) {
	s.mu.Lock()
	if s.token == "" {
		if err := s.loginLocked(ctx); err != nil {
			s.mu.Unlock()
			return 0, nil, err
		}
	}
	tok := s.token
	s.mu.Unlock()

	for attempt := 0; attempt < 2; attempt++ {
		rctx, cancel := context.WithTimeout(ctx, malina26RequestTimeout)
		req, err := http.NewRequestWithContext(rctx, http.MethodGet, s.baseURL+path, nil)
		if err != nil {
			cancel()
			return 0, nil, err
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := s.client.Do(req)
		if err != nil {
			cancel()
			return 0, nil, err
		}
		code := resp.StatusCode
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		cancel()
		if code == http.StatusUnauthorized && attempt == 0 {
			// Токен протух — переавторизуемся и повторяем один раз.
			s.mu.Lock()
			s.token = ""
			err := s.loginLocked(ctx)
			tok = s.token
			s.mu.Unlock()
			if err != nil {
				return code, b, err
			}
			continue
		}
		return code, b, nil
	}
	return http.StatusUnauthorized, nil, fmt.Errorf("malina26: авторизация не удалась")
}

// malina26Device — запись списка /api/{type}/devices.
// malina26Device — запись списка /api/{type}/devices.
// serial — числовой серийник (без буквы) отдельным полем; введён в API «Малина26»
// позже — на старых сборках поля нет, тогда серийник берётся из label (см. deviceSerial).
type malina26Device struct {
	Type   string `json:"type"`
	ID     int    `json:"id"`
	Gen    uint64 `json:"gen"`
	Serial string `json:"serial"`
	Label  string `json:"label"`
}

// devices запрашивает список устройств типа typ ("map"|"kes").
func (s *malina26Source) devices(ctx context.Context, typ string) ([]malina26Device, error) {
	code, b, err := s.get(ctx, "/api/"+typ+"/devices")
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, fmt.Errorf("malina26 %s/devices: status %d", typ, code)
	}
	var out []malina26Device
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("malina26 %s/devices: json: %w", typ, err)
	}
	return out, nil
}

// deviceSerial возвращает серийник устройства: явное поле serial, если сервер его
// отдаёт (новый контракт списка), иначе — второй токен метки label (старые сборки).
func deviceSerial(d malina26Device) string {
	if s := strings.TrimSpace(d.Serial); s != "" {
		return s
	}
	return serialFromLabel(d.Label)
}

// serialFromLabel извлекает серийник из метки label = "<...> · <serial> · <port> · <baud>".
func serialFromLabel(label string) string {
	parts := strings.Split(label, " · ")
	if len(parts) >= 2 {
		return strings.TrimSpace(parts[1])
	}
	return ""
}

// ---- Пулер МАП ----

// pollMAPMalina26 опрашивает «свой» МАП через новый API (method=malina26).
func pollMAPMalina26(ctx context.Context) DeviceResult {
	res := DeviceResult{OK: true}
	now := time.Now()
	if malina26 == nil {
		res.OK = false
		return res
	}
	vals, lastSeen, err := malina26.fetchMAP(ctx)
	if err != nil {
		res.OK = false
		log.Printf("map malina26: %v", err)
		mapTracker.trackErr("malina26", fmt.Sprintf("API «Малина26» недоступно (%v)", err), now)
		return res
	}
	if vals == nil {
		// МАП ещё не найден в списке (или потерян) — данные не готовы, но это не
		// транспортная ошибка: следующий цикл повторит поиск.
		res.OK = false
		return res
	}
	res.HasData = true
	res.Values = vals
	res.DeviceSN = mapDevKey
	mapTracker.trackAPITS(lastSeen, now)
	hasGrid := false
	grid := 0.0
	if v, ok := vals["grid_voltage"]; ok {
		grid, hasGrid = toFloat(v)
	}
	mapTracker.trackOK("malina26", now, hasGrid, grid)
	return res
}

// fetchMapRaw возвращает сырой payload MapData «своего» МАП. nil,nil — МАП не найден
// в списке или потерян (id+gen протух) — вызывающий повторит поиск в следующем цикле.
func (s *malina26Source) fetchMapRaw(ctx context.Context) ([]byte, error) {
	s.mu.Lock()
	haveMap, id, gen := s.haveMap, s.mapID, s.mapGen
	s.mu.Unlock()

	if !haveMap {
		devs, err := s.devices(ctx, "map")
		if err != nil {
			return nil, err
		}
		found := false
		for _, d := range devs {
			if deviceSerial(d) == s.serial {
				s.mu.Lock()
				s.mapID, s.mapGen, s.haveMap = d.ID, d.Gen, true
				s.mu.Unlock()
				id, gen, found = d.ID, d.Gen, true
				break
			}
		}
		if !found {
			return nil, nil // наш МАП не в списке — ждём
		}
	}

	code, b, err := s.get(ctx, fmt.Sprintf("/api/map/raw?id=%d&gen=%d", id, gen))
	if err != nil {
		return nil, err
	}
	if code == http.StatusNotFound {
		// Потеря (id+gen протух) — в следующем цикле ищем заново.
		s.mu.Lock()
		s.haveMap = false
		s.mu.Unlock()
		return nil, nil
	}
	if code != http.StatusOK {
		return nil, fmt.Errorf("map/raw: status %d", code)
	}
	return b, nil
}

// fetchMAP находит «свой» МАП (по serial) и декодирует его raw-payload. nil,nil —
// МАП отсутствует в списке/потерян (не ошибка; повторим в следующем цикле).
func (s *malina26Source) fetchMAP(ctx context.Context) (valuesContract, int64, error) {
	b, err := s.fetchMapRaw(ctx)
	if err != nil {
		return nil, 0, err
	}
	if b == nil {
		return nil, 0, nil
	}
	vals, lastSeen, ok := decodeMalina26Map(b)
	if !ok {
		return nil, 0, fmt.Errorf("map/raw: не удалось декодировать payload (len=%d)", len(b))
	}
	if vals == nil {
		// Контракт валиден, но данных нет (например, нет напряжения АКБ) — это не
		// ошибка транспорта/контракта: HasData=false, без записи в трекер ошибок.
		return nil, lastSeen, nil
	}
	return vals, lastSeen, nil
}

// mapErrCells возвращает сырые байт-ячейки ошибок МАП из бинарного MapData (ячейки
// 0x41C..0x449). Используется пулером ошибок при method=malina26.
func (s *malina26Source) mapErrCells(ctx context.Context) (map[uint16]byte, error) {
	b, err := s.fetchMapRaw(ctx)
	if err != nil {
		return nil, err
	}
	if b == nil {
		return nil, errNoMapErrSource
	}
	p, ok := parseMapPayload(b)
	if !ok {
		return nil, fmt.Errorf("map/raw: битый payload (len=%d)", len(b))
	}
	cells := map[uint16]byte{}
	for addr, id := range malina26ErrCellIDs {
		if v, ok := p.value(id); ok {
			cells[addr] = byte(v)
		}
	}
	return cells, nil
}

// ---- Пулер КЭС ----

// kesReading — один КЭС: UID (серийник) и результат опроса.
type kesReading struct {
	UID    string
	Slot   int
	Result DeviceResult
}

// FetchKES опрашивает все КЭС последовательно. Список берётся из /api/kes/devices,
// каждый КЭС — через /api/kes/raw (id+gen). При ошибке списка — общая ошибка; при
// ошибке одного КЭС — его Result.OK=false.
func (s *malina26Source) FetchKES(ctx context.Context) ([]kesReading, error) {
	devs, err := s.devices(ctx, "kes")
	if err != nil {
		return nil, err
	}
	out := make([]kesReading, 0, len(devs))
	for i, d := range devs {
		r := DeviceResult{}
		code, b, err := s.get(ctx, fmt.Sprintf("/api/kes/raw?id=%d&gen=%d", d.ID, d.Gen))
		if err != nil {
			r.OK = false
			log.Printf("kes malina26: id=%d: %v", d.ID, err)
		} else if code != http.StatusOK {
			r.OK = false
			log.Printf("kes malina26: id=%d: status %d", d.ID, code)
		} else if vals, serial, lastSeen, ok := decodeMalina26KES(b); ok {
			r.OK, r.HasData = true, true
			r.Values = vals
			r.Time = lastSeen
			r.DeviceSN = fmt.Sprintf("mppt-%d", serial)
			out = append(out, kesReading{UID: fmt.Sprintf("%d", serial), Slot: i, Result: r})
			continue
		} else {
			r.OK = false
			log.Printf("kes malina26: id=%d: битый payload (len=%d)", d.ID, len(b))
		}
		// fallback: UID из метки, чтобы ключ не терялся при сбое декода.
		out = append(out, kesReading{UID: deviceSerial(d), Slot: i, Result: r})
	}
	return out, nil
}

// ---- Декодеры ----

// id-позиции нужных ячеек МАП в documentedCells (cells.go, version=5).
const (
	mapCellMODE_RAM         = 131 // 0x400 (копия MODE; ==4 → заряд)
	mapCellUAccMedVH        = 135 // 0x405
	mapCellUAccMedVL        = 136 // 0x406
	mapCellUNET             = 147 // 0x422
	mapCellTFNET            = 150 // 0x425
	mapCellTempGrad0        = 159 // 0x42E (АКБ)
	mapCellTempGrad1        = 160 // 0x42F (тор)
	mapCellTempGrad2        = 161 // 0x430 (транзисторы)
	mapCellIAccMedU16L      = 163 // 0x432
	mapCellIAccMedU16H      = 164 // 0x433
	mapCellTempOff          = 166 // 0x43C
	mapCellPNETSignP        = 382 // 0x587
	mapCellPNET8            = 387 // 0x59A, 2Б
)

// malina26ErrCellIDs — адрес ячейки ошибок МАП → id в documentedCells (cells.go, v5).
var malina26ErrCellIDs = map[uint16]int{
	0x41C: 145, // F_AccOver
	0x41D: 146, // F_NETOver
	0x42A: 155, // RSErrSis
	0x42B: 156, // RSErrJobM
	0x42C: 157, // RSErrJob
	0x42D: 158, // RSWarning
	0x447: 171, // RSErrDop
	0x449: 172, // ThFMAPSync
}

// mapPayload — разобранный заголовок MapData с доступом к значениям по id.
type mapPayload struct {
	nfields  int
	presence []byte
	raw      []byte
	valOff   int
	lastSeen int64
}

func (p mapPayload) value(id int) (uint32, bool) {
	if id < 0 || id >= p.nfields || p.presence[id>>3]&(1<<uint(id&7)) == 0 {
		return 0, false
	}
	return binary.LittleEndian.Uint32(p.raw[p.valOff+id*4:]), true
}

// parseMapPayload проверяет заголовок MapData (магия/версия/размер) и возвращает
// разобранную структуру. ok=false при неверной магии/версии/размере.
func parseMapPayload(b []byte) (mapPayload, bool) {
	const hdr = 64
	if len(b) < hdr || string(b[0:4]) != "MPAD" {
		return mapPayload{}, false
	}
	version := binary.LittleEndian.Uint16(b[4:6])
	nfields := int(binary.LittleEndian.Uint16(b[6:8]))
	if version != malina26MapContractVersion || nfields != malina26MapNFields {
		log.Printf("map malina26: контракт разошёлся (version=%d nfields=%d, ждём %d/%d) — payload отвергнут",
			version, nfields, malina26MapContractVersion, malina26MapNFields)
		return mapPayload{}, false
	}
	presLen := (nfields + 7) / 8
	valOff := hdr + presLen
	if len(b) < valOff+nfields*4 {
		return mapPayload{}, false
	}
	return mapPayload{
		nfields:  nfields,
		presence: b[hdr:valOff],
		raw:      b,
		valOff:   valOff,
		lastSeen: int64(binary.LittleEndian.Uint64(b[56:64])),
	}, true
}

// decodeMalina26Map разбирает бинарный payload MapData:
//
//	[magic "MPAD" u32][version u16][nfields u16][port 32][model u8][protocol u8]
//	[unit u8][reserved u8][reserved u32][gen u64][lastSeen u64]
//	[presence ceil(nfields/8)][values nfields×uint32 LE]
//
// Возвращает значения универсального контракта теми же формулами, что rs485-ветка
// (mapMAPRegisters), и lastSeen (unix). ok=false при неверной магии/версии/размере.
func decodeMalina26Map(b []byte) (valuesContract, int64, bool) {
	p, ok := parseMapPayload(b)
	if !ok {
		return nil, 0, false
	}
	val := p.value
	lastSeen := p.lastSeen

	out := valuesContract{}
	vh, ok1 := val(mapCellUAccMedVH)
	vl, ok2 := val(mapCellUAccMedVL)
	if !ok1 || !ok2 {
		// Контракт валиден, но ячеек напряжения АКБ нет — данных пока нет.
		return nil, lastSeen, true
	}
	uAcc := (float64(vh)*256 + float64(vl)) / 10
	if uAcc <= 0 {
		// Нулевое/ещё-не-готовое напряжение — «данных нет», а не битый контракт.
		return nil, lastSeen, true
	}
	out["battery_voltage"] = uAcc
	out["l1_voltage"] = uAcc

	// Ток АКБ: (L+H*256)/16; знак контрактный (заряд −) — как rs485: MODE_RAM==4.
	if il, okL := val(mapCellIAccMedU16L); okL {
		if ih, okH := val(mapCellIAccMedU16H); okH {
			iAcc := (float64(il) + float64(ih)*256) / 16
			if mode, okM := val(mapCellMODE_RAM); okM && mode == 4 {
				iAcc = -iAcc
			}
			out["battery_current"] = iAcc
			out["battery_power"] = uAcc * iAcc
		}
	}

	// Напряжение сети: UNET=0 → нет сети; иначе (+100), как rs485.
	if un, ok := val(mapCellUNET); ok {
		if un == 0 {
			out["grid_voltage"] = 0.0
		} else {
			out["grid_voltage"] = float64(un) + 100
		}
	}
	// Мощность сети: PNET_8 (2Б) → (v/8)*100; знак по PNET_Sign_P (1 — потребление +).
	if p8, ok := val(mapCellPNET8); ok {
		gp := (float64(p8) / 8) * 100
		if sign, okS := val(mapCellPNETSignP); okS && sign == 0 {
			gp = -gp
		}
		out["grid_power"] = gp
	}
	// Частота сети: 6250/TFNET.
	if tf, ok := val(mapCellTFNET); ok && tf > 0 {
		out["grid_frequency"] = 6250.0 / float64(tf)
	}
	// Температуры: raw−50; Temp_off биты 0/1/2 — нет датчика.
	tempOff := uint32(0)
	if o, ok := val(mapCellTempOff); ok {
		tempOff = o
	}
	putTemp := func(id int, bit uint, tag string) {
		if tempOff&(1<<bit) != 0 {
			return
		}
		if t, ok := val(id); ok {
			out[tag] = float64(t) - 50
		}
	}
	putTemp(mapCellTempGrad0, 0, "map_temp_battery")
	putTemp(mapCellTempGrad1, 1, "map_temp_tor")
	putTemp(mapCellTempGrad2, 2, "map_temp_transistor")

	return out, lastSeen, true
}

// decodeMalina26KES разбирает бинарный payload КЭС (1536 Б = [KESE 512][KESR 512][KESS 512])
// и возвращает те же теги, что legacy read_json.php?device=mppt (mapMPPTAPI):
// pv1_voltage/current/power, l1_voltage/current, ac_active_power, energy_today.
// Также возвращает серийник КЭС (KESE.Serial) и время снимка (KESR.LastSeen).
func decodeMalina26KES(b []byte) (valuesContract, uint16, time.Time, bool) {
	if len(b) < 1536 {
		return nil, 0, time.Time{}, false
	}
	kese := b[0:512]
	kesr := b[512:1024]
	if string(kese[0:4]) != "KESE" || kese[4] != 1 {
		return nil, 0, time.Time{}, false
	}
	if string(kesr[0:4]) != "KESR" || kesr[4] != malina26KesrVersion {
		return nil, 0, time.Time{}, false
	}
	le16 := func(o int) uint16 { return binary.LittleEndian.Uint16(kesr[o:]) }
	serial := binary.LittleEndian.Uint16(kese[6:8])
	lastSeen := int64(binary.LittleEndian.Uint64(kesr[6:14]))

	out := valuesContract{}
	out["pv1_voltage"] = float64(le16(18)) / 100
	out["pv1_current"] = float64(le16(20)) / 100
	out["pv1_power"] = float64(le16(24))
	out["l1_voltage"] = float64(le16(22)) / 100
	out["l1_current"] = float64(le16(32)) / 100
	out["ac_active_power"] = float64(le16(26))
	// Суммарная энергия за сутки, кВт·ч. Воспроизводим РОВНО поле legacy-API ПАК
	// «Малина» `Pwr_kW = fmt("%d.%03d", PwrKW, PwrW)` (cleaning.malina
	// web/internal/web/kesmem.go:333) — именно это значение sunReceiver исторически
	// писал в energy_today через legacy-путь (read_json.php?device=mppt), поэтому при
	// смене источника оно не должно меняться. Численно строка равна `PwrKW + PwrW/1000`
	// при 3-значном PwrW, но при 4-значном (реальный аппарат отдаёт PwrW≈6326) — уже
	// `PwrKW + PwrW/10000`. Значения поля «сырые», масштаб задаёт формат строки.
	out["energy_today"], _ = strconv.ParseFloat(fmt.Sprintf("%d.%03d", le16(40), le16(38)), 64)

	ts := time.Time{}
	if lastSeen > 0 {
		ts = time.Unix(lastSeen, 0)
	}
	return out, serial, ts, true
}

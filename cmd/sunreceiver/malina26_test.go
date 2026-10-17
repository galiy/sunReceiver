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
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// buildMapPayload собирает MapData с заданными значениями ячеек (id→value).
func buildMapPayload(cells map[int]uint32) []byte {
	const hdr = 64
	presLen := (malina26MapNFields + 7) / 8
	b := make([]byte, hdr+presLen+malina26MapNFields*4)
	copy(b[0:4], "MPAD")
	binary.LittleEndian.PutUint16(b[4:6], malina26MapContractVersion)
	binary.LittleEndian.PutUint16(b[6:8], malina26MapNFields)
	copy(b[8:40], []byte("/dev/ttyAMA0"))
	b[40] = 'T'
	binary.LittleEndian.PutUint64(b[56:64], 1792013071) // lastSeen
	pres := b[hdr : hdr+presLen]
	valOff := hdr + presLen
	for id, v := range cells {
		pres[id>>3] |= 1 << uint(id&7)
		binary.LittleEndian.PutUint32(b[valOff+id*4:], v)
	}
	return b
}

func TestDecodeMalina26Map(t *testing.T) {
	payload := buildMapPayload(map[int]uint32{
		mapCellUAccMedVH:   2,
		mapCellUAccMedVL:   21,
		mapCellIAccMedU16L: 204,
		mapCellIAccMedU16H: 7,
		mapCellMODE_RAM:    4, // заряд → ток отрицательный
		mapCellUNET:        116,
		mapCellPNET8:       628,
		mapCellPNETSignP:   1,
		mapCellTFNET:       125,
		mapCellTempGrad0:   77,
		mapCellTempGrad1:   97,
		mapCellTempGrad2:   97,
		mapCellTempOff:     0,
	})
	vals, lastSeen, ok := decodeMalina26Map(payload)
	if !ok {
		t.Fatalf("payload не декодирован")
	}
	if lastSeen != 1792013071 {
		t.Fatalf("lastSeen=%d", lastSeen)
	}
	approx := func(tag string, want float64) {
		got, _ := toFloat(vals[tag])
		if d := got - want; d > 1e-6 || d < -1e-6 {
			t.Errorf("%s=%v, ожидалось %v", tag, got, want)
		}
	}
	approx("battery_voltage", 53.3)
	approx("l1_voltage", 53.3)
	approx("battery_current", -124.75) // заряд: MODE_RAM==4
	approx("battery_power", 53.3*-124.75)
	approx("grid_voltage", 216)
	approx("grid_power", 7850)     // (628/8)*100
	approx("grid_frequency", 50)   // 6250/125
	approx("map_temp_battery", 27) // 77-50
	approx("map_temp_tor", 47)     // 97-50
	approx("map_temp_transistor", 47)
}

func TestDecodeMalina26MapVersionMismatch(t *testing.T) {
	b := buildMapPayload(map[int]uint32{mapCellUAccMedVH: 2, mapCellUAccMedVL: 21})
	binary.LittleEndian.PutUint16(b[4:6], 99) // неверная версия
	if _, _, ok := decodeMalina26Map(b); ok {
		t.Fatalf("payload с чужой версией должен быть отвергнут")
	}
}

// Нулевое напряжение АКБ — валидный контракт, но данных нет: vals==nil, ok==true
// (не ошибка декода/транспорта).
func TestDecodeMalina26MapZeroBattery(t *testing.T) {
	b := buildMapPayload(map[int]uint32{mapCellUAccMedVH: 0, mapCellUAccMedVL: 0})
	vals, _, ok := decodeMalina26Map(b)
	if !ok {
		t.Fatalf("валидный контракт не должен отвергаться")
	}
	if vals != nil {
		t.Fatalf("при нулевом напряжении ожидались пустые значения, got %v", vals)
	}
}

func TestDecodeMalina26KES(t *testing.T) {
	b := make([]byte, 1536)
	kese, kesr := b[0:512], b[512:1024]
	copy(kese[0:4], "KESE")
	kese[4] = 1
	binary.LittleEndian.PutUint16(kese[6:8], 623)
	copy(kesr[0:4], "KESR")
	kesr[4] = malina26KesrVersion
	binary.LittleEndian.PutUint64(kesr[6:14], 1792013077)
	le := func(o int, v uint16) { binary.LittleEndian.PutUint16(kesr[o:], v) }
	le(18, 10436) // VcPV
	le(20, 517)   // IcPV
	le(22, 5226)  // VBat
	le(24, 540)   // PPV
	le(26, 529)   // POut
	le(32, 1012)  // ICh
	le(38, 4331)  // PwrW (дробная часть)
	le(40, 4)     // PwrKW (целая часть)

	vals, serial, ts, ok := decodeMalina26KES(b)
	if !ok || serial != 623 {
		t.Fatalf("KES не декодирован (ok=%v serial=%d)", ok, serial)
	}
	if ts.Unix() != 1792013077 {
		t.Fatalf("ts=%v", ts)
	}
	approx := func(tag string, want float64) {
		got, _ := toFloat(vals[tag])
		if d := got - want; d > 1e-6 || d < -1e-6 {
			t.Errorf("%s=%v, ожидалось %v", tag, got, want)
		}
	}
	approx("pv1_voltage", 104.36)
	approx("pv1_current", 5.17)
	approx("pv1_power", 540)
	approx("l1_voltage", 52.26)
	approx("l1_current", 10.12)
	approx("ac_active_power", 529)
	approx("energy_today", 4.4331) // PwrKW + PwrW/10000
}

func TestSerialFromLabel(t *testing.T) {
	if got := serialFromLabel("Titanator · 51510 · /dev/ttyAMA0 · 115200"); got != "51510" {
		t.Fatalf("serialFromLabel=%q", got)
	}
	if got := serialFromLabel("KES · 623 · /dev/ttyUSB10 · 115200"); got != "623" {
		t.Fatalf("serialFromLabel=%q", got)
	}
}

func TestIsMPPTKey(t *testing.T) {
	if !isMPPTKey("mppt-623") {
		t.Fatalf("mppt-623 должен быть ключом MPPT")
	}
	if isMPPTKey("map") || isMPPTKey("192.168.0.10") {
		t.Fatalf("ложное срабатывание isMPPTKey")
	}
}

// energy_today должен точно повторять legacy-поле Pwr_kW = fmt("%d.%03d", PwrKW, PwrW):
// при 3-значном PwrW — PwrKW+PwrW/1000, при 4-значном (реальный аппарат ~6000) —
// PwrKW+PwrW/10000. Проверяем оба диапазона.
func TestDecodeMalina26KESEnergyRanges(t *testing.T) {
	build := func(pwrKW, pwrW uint16) []byte {
		b := make([]byte, 1536)
		copy(b[0:4], "KESE")
		b[4] = 1
		copy(b[512:516], "KESR")
		b[516] = malina26KesrVersion
		binary.LittleEndian.PutUint16(b[512+40:], pwrKW)
		binary.LittleEndian.PutUint16(b[512+38:], pwrW)
		return b
	}
	cases := []struct {
		kw, w   uint16
		wantKWh float64
	}{
		{4, 331, 4.331},   // <1000: PwrKW + PwrW/1000
		{4, 4331, 4.4331}, // 4-значный (как реальный аппарат)
		{6, 50, 6.050},    // %03d паддинг
		{6, 6326, 6.6326}, // live-подобное
	}
	for _, c := range cases {
		vals, _, _, ok := decodeMalina26KES(build(c.kw, c.w))
		if !ok {
			t.Fatalf("PwrW=%d PwrKW=%d: декод не удался", c.w, c.kw)
		}
		got, _ := toFloat(vals["energy_today"])
		if d := got - c.wantKWh; d > 1e-9 || d < -1e-9 {
			t.Errorf("PwrKW=%d PwrW=%d: energy_today=%v, want %v", c.kw, c.w, got, c.wantKWh)
		}
	}
}

// deviceSerial: предпочитает явное поле serial (новый контракт списка), при его
// отсутствии — второй токен label (старые сборки).
func TestDeviceSerial(t *testing.T) {
	withField := malina26Device{Serial: "623", Label: "KES · 999 · /dev/ttyUSB9 · 115200"}
	if got := deviceSerial(withField); got != "623" {
		t.Fatalf("deviceSerial с полем serial = %q, want 623", got)
	}
	noField := malina26Device{Label: "Titanator · 51510 · /dev/ttyAMA0 · 115200"}
	if got := deviceSerial(noField); got != "51510" {
		t.Fatalf("deviceSerial без поля serial = %q, want 51510", got)
	}
}

// fallbackTestServer поднимает минимальный API «Малина26»: token, список МАП и raw.
type fallbackTestServer struct {
	mu         sync.Mutex
	devices    []malina26Device
	primary    []byte
	fallback   []byte
	broken     bool // raw основного отдаёт 500
	primary404 bool // raw основного отдаёт 404
}

func (f *fallbackTestServer) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/token":
			_, _ = w.Write([]byte(`{"token":"t"}`))
		case r.URL.Path == "/api/map/devices":
			f.mu.Lock()
			b, _ := json.Marshal(f.devices)
			f.mu.Unlock()
			_, _ = w.Write(b)
		case r.URL.Path == "/api/map/raw":
			id := r.URL.Query().Get("id")
			f.mu.Lock()
			broken, notFound, pr, fb := f.broken, f.primary404, f.primary, f.fallback
			f.mu.Unlock()
			switch id {
			case "1":
				if notFound {
					http.NotFound(w, r)
					return
				}
				if broken {
					http.Error(w, "broken", http.StatusInternalServerError)
					return
				}
				_, _ = w.Write(pr)
			case "2":
				_, _ = w.Write(fb)
			default:
				http.NotFound(w, r)
			}
		default:
			http.NotFound(w, r)
		}
	})
}

func newFallbackSource(t *testing.T, f *fallbackTestServer) (*malina26Source, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(f.handler())
	s := &malina26Source{
		baseURL:        srv.URL,
		serial:         "10001",
		fallbackSerial: "20002",
		client:         srv.Client(),
	}
	return s, srv
}

func mapPayloadForTest() []byte {
	return buildMapPayload(map[int]uint32{
		mapCellUAccMedVH: 2,
		mapCellUAccMedVL: 21,
	})
}

func TestMalina26FallbackSelectsReserve(t *testing.T) {
	payload := mapPayloadForTest()
	f := &fallbackTestServer{
		devices:  []malina26Device{{Type: "map", ID: 2, Gen: 1, Serial: "20002"}},
		fallback: payload,
	}
	s, srv := newFallbackSource(t, f)
	defer srv.Close()

	vals, _, err := s.fetchMAP(context.Background())
	if err != nil || vals == nil {
		t.Fatalf("fallback: vals=%v err=%v", vals, err)
	}
	s.mu.Lock()
	activeFallback := s.activeFallback
	s.mu.Unlock()
	if !activeFallback {
		t.Fatal("должен быть активен резервный МАП")
	}
	// Идентичность снимка остаётся по основному серийнику, даже если опрошен резервный.
	old := malina26
	malina26 = s
	defer func() { malina26 = old }()
	res := pollMAPMalina26(context.Background())
	if !res.HasData || res.DeviceSN != "10001" {
		t.Fatalf("DeviceSN=%q HasData=%v, ожидали основной серийник 10001", res.DeviceSN, res.HasData)
	}
}

func TestMalina26FallbackReturnsToPrimaryAfterSuccess(t *testing.T) {
	payload := mapPayloadForTest()
	f := &fallbackTestServer{
		devices: []malina26Device{
			{Type: "map", ID: 1, Gen: 1, Serial: "10001"},
			{Type: "map", ID: 2, Gen: 1, Serial: "20002"},
		},
		primary:  payload,
		fallback: payload,
	}
	s, srv := newFallbackSource(t, f)
	defer srv.Close()
	// Сначала основной «отсутствует» — уходим на резервный.
	f.devices = []malina26Device{{Type: "map", ID: 2, Gen: 1, Serial: "20002"}}
	if _, _, err := s.fetchMAP(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Появился основной; имитируем истечение 60-с окна перепроверки.
	f.mu.Lock()
	f.devices = []malina26Device{
		{Type: "map", ID: 1, Gen: 1, Serial: "10001"},
		{Type: "map", ID: 2, Gen: 1, Serial: "20002"},
	}
	f.mu.Unlock()
	s.mu.Lock()
	s.lastListCheck = time.Now().Add(-61 * time.Second)
	s.mu.Unlock()

	if _, _, err := s.fetchMAP(context.Background()); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	activeFallback, broken := s.activeFallback, !s.primaryBroken.IsZero()
	s.mu.Unlock()
	if activeFallback || broken {
		t.Fatalf("ожидали возврат на основной: activeFallback=%v broken=%v", activeFallback, broken)
	}
}

func TestMalina26FallbackAfterTwoPrimaryFailures(t *testing.T) {
	payload := mapPayloadForTest()
	f := &fallbackTestServer{
		devices: []malina26Device{
			{Type: "map", ID: 1, Gen: 1, Serial: "10001"},
			{Type: "map", ID: 2, Gen: 1, Serial: "20002"},
		},
		primary:  payload,
		fallback: payload,
		broken:   true,
	}
	s, srv := newFallbackSource(t, f)
	defer srv.Close()
	for i := 0; i < 2; i++ {
		_, _, _ = s.fetchMAP(context.Background())
	}
	s.mu.Lock()
	broken := !s.primaryBroken.IsZero()
	s.mu.Unlock()
	if !broken {
		t.Fatal("после 2 неуспехов основной должен быть в backoff")
	}
	// Следующий цикл — резервный.
	vals, _, err := s.fetchMAP(context.Background())
	if err != nil || vals == nil {
		t.Fatalf("после backoff ожидали данные с резервного: vals=%v err=%v", vals, err)
	}
}

func TestMalina26FallbackAfterDecodeFailures(t *testing.T) {
	f := &fallbackTestServer{
		devices: []malina26Device{
			{Type: "map", ID: 1, Gen: 1, Serial: "10001"},
			{Type: "map", ID: 2, Gen: 1, Serial: "20002"},
		},
		primary:  []byte("broken"),
		fallback: mapPayloadForTest(),
	}
	s, srv := newFallbackSource(t, f)
	defer srv.Close()
	// Две ошибки декодирования основного → backoff.
	for i := 0; i < 2; i++ {
		if _, _, err := s.fetchMAP(context.Background()); err == nil {
			t.Fatal("ожидали ошибку декодирования основного")
		}
	}
	vals, _, err := s.fetchMAP(context.Background())
	if err != nil || vals == nil {
		t.Fatalf("после backoff ожидали данные с резервного: vals=%v err=%v", vals, err)
	}
	s.mu.Lock()
	fallback := s.activeFallback
	s.mu.Unlock()
	if !fallback {
		t.Fatal("должен быть активен резервный")
	}
}

func TestMalina26NoDevices(t *testing.T) {
	f := &fallbackTestServer{devices: nil}
	s, srv := newFallbackSource(t, f)
	defer srv.Close()
	vals, _, err := s.fetchMAP(context.Background())
	if err != nil || vals != nil {
		t.Fatalf("нет устройств: vals=%v err=%v", vals, err)
	}
}

func TestMalina26FallbackAfter404(t *testing.T) {
	f := &fallbackTestServer{
		devices: []malina26Device{
			{Type: "map", ID: 1, Gen: 1, Serial: "10001"},
			{Type: "map", ID: 2, Gen: 1, Serial: "20002"},
		},
		fallback:   mapPayloadForTest(),
		primary404: true,
	}
	s, srv := newFallbackSource(t, f)
	defer srv.Close()
	for i := 0; i < 2; i++ {
		_, _, _ = s.fetchMAP(context.Background())
	}
	vals, _, err := s.fetchMAP(context.Background())
	if err != nil || vals == nil {
		t.Fatalf("после 2×404 ожидали резервный: vals=%v err=%v", vals, err)
	}
}

func TestMalina26ProbeFailureKeepsFallback(t *testing.T) {
	f := &fallbackTestServer{
		devices:  []malina26Device{{Type: "map", ID: 2, Gen: 1, Serial: "20002"}},
		fallback: mapPayloadForTest(),
	}
	s, srv := newFallbackSource(t, f)
	defer srv.Close()
	if _, _, err := s.fetchMAP(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Основной «появился», но raw отдаёт 404; подошло окно перепроверки.
	f.mu.Lock()
	f.devices = []malina26Device{
		{Type: "map", ID: 1, Gen: 1, Serial: "10001"},
		{Type: "map", ID: 2, Gen: 1, Serial: "20002"},
	}
	f.primary404 = true
	f.mu.Unlock()
	s.mu.Lock()
	s.lastListCheck = time.Now().Add(-61 * time.Second)
	s.mu.Unlock()
	for i := 0; i < 2; i++ {
		if _, _, err := s.fetchMAP(context.Background()); err == nil {
			// 404 трактуется как «нет данных», не ошибка — это ок.
		}
	}
	vals, _, err := s.fetchMAP(context.Background())
	if err != nil || vals == nil {
		t.Fatalf("после неудачной пробы ожидали резервный: vals=%v err=%v", vals, err)
	}
}

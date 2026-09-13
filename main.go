package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/galiy/sunReceiver/modbusmap"
	"github.com/galiy/sunReceiver/solarman"
)

const (
	port       = "8899"
	pollPeriod = 10 * time.Second
	timeout    = 15 * time.Second
)

type targetKind int

const (
	kindSofar targetKind = iota
	kindDeyeString
	kindMAP
	kindMPPT // MPPT-контроллер (КЭС) через веб-API read_json.php?device=mppt ПАК «Малина»
)

// mpptOrderBase — базовый порядок для MPPT-контроллеров (динамических, из API ПАК
// «Малина»): они всегда размещаются на дашборде последними, после всех статических
// устройств из конфига (инверторы + МАП). Order = mpptOrderBase + slot.
const mpptOrderBase = 10000

// String — короткое имя типа для логов.
func (k targetKind) String() string {
	switch k {
	case kindSofar:
		return "sofar"
	case kindDeyeString:
		return "deye"
	case kindMAP:
		return "map"
	case kindMPPT:
		return "mppt"
	default:
		return "unknown"
	}
}

// invTarget — целевой инвертор. LoggerSN — серийный номер даталоггера,
// обязателен для Deye (иначе логгер отвечает кодом 0x06 "serial number not match").
// Name — логическое имя из sunReceiver.json (например, "Deye Left").
// Unit — Modbus-адрес устройства для МАП (kindMAP), по умолчанию 1.
// Slot — для kindMAP: индекс MPPT-контроллера (0..15), -1 = агрегат/база батареи-сети;
//
//	для kindMPPT: индекс контроллера (слот) в ответе read_json.php?device=mppt.
type invTarget struct {
	IP       string
	Name     string
	LoggerSN uint32
	Kind     targetKind
	Unit     byte
	Slot     int
	Order    int // порядок устройства на дашборде (индекс в конфиге; MPPT — всегда последними)
}

// configInverter — запись инвертора (Deye/Sofar) в sunReceiver.json разделе "invertors".
// Disabled — ОБЯЗАТЕЛЬНОЕ поле (отсутствие = ошибка конфига): false = опрашивается,
// true = временно отключён (устройство в конфиге, но не опрашивается).
type configInverter struct {
	IP       string `json:"ip"`
	Name     string `json:"name"`
	Type     string `json:"type"`
	LoggerSN uint32 `json:"logger_sn"`
	Disabled *bool  `json:"disabled"`
}

// mapSection — отдельный блок настройки МАП Титанатор («КЭС», батарея/сеть) в
// sunReceiver.json. Не входит в invertors: это устройство Modbus TCP, а не инвертор.
// Disabled — ОБЯЗАТЕЛЬНОЕ поле (отсутствие = ошибка конфига): false = МАП
// опрашивается через Modbus TCP (как раньше); true = пулер по Modbus НЕ запускается,
// а все параметры МАП (батарея/сеть) берутся из веб-API ПАК «Малина»
// read_json.php?device=map (для этого требуется настроенный раздел "mppt").
type mapSection struct {
	Name     string `json:"name"`
	IP       string `json:"ip"`
	Unit     int    `json:"unit,omitempty"`
	Disabled *bool  `json:"disabled"`
}

// dbConfig — расположение баз данных. Задаётся в sunReceiver.json в разделе "db".
// Пароль указывается прямо в pg-DSN (sunReceiver.json — приватный конфиг, в git не
// коммитится).
type dbConfig struct {
	Redis string `json:"redis"` // адрес Redis в формате host:port
	PG    string `json:"pg"`    // DSN PostgreSQL (с паролем)
}

// meterSection — конфигурация электросчётчика DDS238, заданная в sunReceiver.json
// разделом "meter" (обратная совместимость — отдельный dds238.json).
type meterSection struct {
	Name        string `json:"name"`
	IP          string `json:"ip"`
	Port        int    `json:"port"`
	Unit        byte   `json:"unit"`
	FirstReg    uint16 `json:"first_reg"`
	RegisterCnt uint16 `json:"register_count"`
}

// mpptSection — конфигурация веб-API ПАК «Малина» для мониторинга MPPT-контроллеров.
// Пароль хранится в открытом виде (sunReceiver.json — приватный, в git не выгружается).
type mpptSection struct {
	BaseURL  string `json:"base_url"`
	MPPTPath string `json:"mppt_path"`
	Login    string `json:"login"`
	Password string `json:"password"`
}

type configFile struct {
	Invertors []configInverter `json:"invertors"`
	Map       *mapSection      `json:"map"`
	DB        *dbConfig        `json:"db"`
	Meter     *meterSection    `json:"meter"`
	MPPT      *mpptSection     `json:"mppt"`
}

// configPath — sunReceiver.json в каталоге исполняемого файла.
func configPath() string {
	exe, err := os.Executable()
	if err != nil {
		return "sunReceiver.json"
	}
	return filepath.Join(filepath.Dir(exe), "sunReceiver.json")
}

// loadConfig читает и проверяет sunReceiver.json, возвращает список целей
// (инверторы + МАП, без отключённых) и настройки БД/счётчика/MPPT.
func loadConfig(path string) ([]invTarget, *dbConfig, *meterSection, *mpptSection, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("read config %s: %w", path, err)
	}
	var cf configFile
	if err := json.Unmarshal(b, &cf); err != nil {
		return nil, nil, nil, nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	targets := make([]invTarget, 0, len(cf.Invertors)+1)
	nextOrder := 0 // порядок устройства на дашборде = позиция в конфиге (в порядке invertors, затем map)

	// Инверторы (Deye/Sofar) из invertors; отключённые (disabled=true) пропускаются.
	for _, t := range cf.Invertors {
		if t.Disabled == nil {
			return nil, nil, nil, nil, fmt.Errorf("config %s: для %s (%s) не задано обязательное поле disabled (false/true)", path, t.Name, t.IP)
		}
		if *t.Disabled {
			log.Printf("config: %s (%s) отключён (disabled=true) — не опрашивается", t.Name, t.IP)
			continue
		}
		var kind targetKind
		switch t.Type {
		case "deye":
			kind = kindDeyeString
		case "sofar":
			kind = kindSofar
		case "map", "mppt":
			// map/mppt вынесены в отдельные разделы; записи в invertors игнорируются.
			log.Printf("config: тип %q для %s не ожидается в invertors (используйте раздел map/mppt) — пропущен", t.Type, t.IP)
			continue
		default:
			return nil, nil, nil, nil, fmt.Errorf("config %s: неизвестный тип %q для %s", path, t.Type, t.IP)
		}
		if t.IP == "" {
			return nil, nil, nil, nil, fmt.Errorf("config %s: пустой ip (type=%s)", path, t.Type)
		}
		if t.Name == "" {
			return nil, nil, nil, nil, fmt.Errorf("config %s: пустое имя name для %s", path, t.IP)
		}
		targets = append(targets, invTarget{IP: t.IP, Name: t.Name, LoggerSN: t.LoggerSN, Kind: kind, Unit: 1, Slot: -1, Order: nextOrder})
		nextOrder++
	}

	// МАП (батарея/сеть) — отдельный блок "map". Disabled обязателен: false —
	// Modbus TCP, true — данные берутся из веб-API ПАК «Малина» (mapAPI). При true
	// цель в targets не добавляется (обычный Modbus-пулер не запускается).
	if cf.Map != nil {
		if cf.Map.Disabled == nil {
			return nil, nil, nil, nil, fmt.Errorf("config %s: в разделе map не задано обязательное поле disabled (false/true)", path)
		}
		if cf.Map.IP == "" {
			return nil, nil, nil, nil, fmt.Errorf("config %s: пустой ip в разделе map", path)
		}
		name := cf.Map.Name
		if name == "" {
			name = "MAP (батарея/сеть)"
		}
		if *cf.Map.Disabled {
			log.Printf("config: МАП (%s) disabled=true — опрашивается через веб-API ПАК «Малина», а не через Modbus", name)
			mapAPI = &mapAPISource{name: name, ip: cf.Map.IP, order: nextOrder}
		} else {
			unit := byte(1)
			if cf.Map.Unit > 0 {
				unit = byte(cf.Map.Unit)
			}
			targets = append(targets, invTarget{IP: cf.Map.IP, Name: name, Kind: kindMAP, Unit: unit, Slot: -1, Order: nextOrder})
			nextOrder++
		}
	}

	if len(targets) == 0 && mapAPI == nil {
		// Если активными остались только MPPT-контроллеры из API ПАК «Малина»,
		// targets может быть пуст — это допустимо: цели собираются динамически.
		mpptOk := cf.MPPT != nil && cf.MPPT.BaseURL != "" && cf.MPPT.MPPTPath != "" &&
			cf.MPPT.Login != "" && cf.MPPT.Password != ""
		if !mpptOk {
			return nil, nil, nil, nil, fmt.Errorf("config %s: нет ни одного активного устройства", path)
		}
	}
	return targets, cf.DB, cf.Meter, cf.MPPT, nil
}

// defaultRedisAddr возвращает адрес Redis: из раздела db конфига (приоритет),
// иначе — дефолт 127.0.0.1:6379.
func defaultRedisAddr(db *dbConfig) string {
	if db != nil && db.Redis != "" {
		return db.Redis
	}
	return "127.0.0.1:6379"
}

// defaultPGDSN возвращает DSN PostgreSQL: из раздела db конфига (приоритет),
// иначе — дефолт localhost.
func defaultPGDSN(db *dbConfig) string {
	if db != nil && db.PG != "" {
		return db.PG
	}
	return "postgres://localhost:5432/sunreceiver?sslmode=disable"
}

// devKey возвращает ключ устройства в хранилище (поле IP снимка): для обычных
// инверторов это IP; для kindMAP с slot и для kindMPPT — IP с суффиксом номера
// контроллера (например, 192.168.13.74#mppt0 / 192.168.13.60#mppt0), чтобы разные
// контроллеры одного гейта не сливались в одну колонку/ряд Redis и PG.
func devKey(t invTarget) string {
	if t.Kind == kindMPPT {
		return fmt.Sprintf("%s#mppt%d", t.IP, t.Slot)
	}
	if t.Kind == kindMAP && t.Slot >= 0 {
		return fmt.Sprintf("%s#mppt%d", t.IP, t.Slot)
	}
	return t.IP
}

var targets []invTarget

// mppt — конфигурация доступа к веб-API ПАК «Малина» для мониторинга MPPT (КЭС)
// через read_json.php?device=mppt. Заполняется в main() из раздела "mppt"
// sunReceiver.json.
var mppt *mpptSite

// mapAPI — когда МАП опрашивается НЕ через Modbus, а через веб-API ПАК «Малина»
// (read_json.php?device=map). Заполняется в loadConfig, если в разделе "map"
// sunReceiver.json задано disabled=true. name/ip — логическое имя и ключ устройства
// (IP МАП из конфига, тот же devKey, что у МАП по Modbus). Если nil — МАП
// опрашивается через Modbus по-прежнему.
var mapAPI *mapAPISource

type mapAPISource struct {
	name  string
	ip    string
	order int
}

var statusNames = map[uint16]string{
	0: "standby", 1: "self-checking", 2: "normal", 3: "fault", 4: "permanent",
}

var faultBits = []struct {
	Bit  uint16
	Name string
}{
	{1, "grid_over_voltage"}, {2, "grid_under_voltage"}, {4, "grid_over_frequency"},
	{8, "grid_under_frequency"}, {16, "pv_under_voltage"}, {32, "grid_low_voltage_ride_through"},
	{256, "pv_over_voltage"}, {512, "pv_current_unbalanced"}, {1024, "pv_input_mode_wrong"},
	{2048, "gfc_i_fault"}, {4096, "phase_sequence"}, {8192, "boost_over_current"},
	{16384, "ac_over_current"}, {32768, "grid_current_high"},
}

var countries = map[uint16]string{
	0: "Germany", 1: "CEI0-21 Internal", 2: "Australia", 3: "Spain RD1699",
	4: "Turkey", 5: "Denmark", 6: "Greece", 7: "Netherland", 8: "Belgium",
	9: "UK-G59", 10: "China", 11: "France", 12: "Poland", 13: "Germany BDEW",
	14: "Germany VDE0126", 15: "Italy CEI0-16", 16: "UK-G83", 17: "Greece Islands",
	18: "EU EN50438", 19: "EU EN61727", 20: "Korea", 21: "Sweden",
	22: "Europe General", 23: "CEI0-21 External", 24: "Cyprus", 25: "India",
	26: "Philippines", 27: "New Zealand",
}

// valuesContract — универсальный контракт значений. В файл выводит ТОЛЬКО общие
// для обеих марок теги (commonContractTags) в фиксированном порядке; числовые значения
// тегов *voltage / *current / temperature* / energy* / *power округляются до 1 знака
// после запятой.
type valuesContract map[string]any

// commonContractTags — теги, которые обе марки (Deye и Sofar) отдают в одинаковых
// единицах измерения. Только эти теги попадают в values; порядок в файле фиксирован.
var commonContractTags = []string{
	// PV входы
	"pv1_voltage", "pv1_current", "pv1_power",
	"pv2_voltage", "pv2_current", "pv2_power",
	// AC выход
	"ac_active_power",
	"ac_reactive_power",
	"grid_frequency",
	// Фазы L1/L2/L3
	"l1_voltage", "l1_current",
	"l2_voltage", "l2_current",
	"l3_voltage", "l3_current",
	// Энергия
	"energy_today",
	"energy_total",
	// МАП modbus: батарея и сеть (для дашборда КЭС)
	"grid_voltage",
	"grid_power",
	"battery_voltage",
	"battery_power",
	// Электросчётчик DDS238: мгновенные значения (V, A, W, var, Hz, kWh)
	"meter_voltage",
	"meter_current",
	"meter_active_power",
	"meter_reactive_power",
	"meter_power_factor",
	"meter_frequency",
	"meter_import",
	"meter_export",
	"meter_total",
}

// needsRounding — true, если тэг относится к величинам, которые округляются до
// 1 знака после запятой (напряжение, ток, мощность, энергия, температура).
func needsRounding(tag string) bool {
	return strings.HasSuffix(tag, "voltage") ||
		strings.HasSuffix(tag, "current") ||
		strings.HasSuffix(tag, "power") ||
		strings.HasSuffix(tag, "frequency") ||
		strings.Contains(tag, "energy") ||
		strings.Contains(tag, "temperature")
}

// round1 округляет числовое значение до 1 знака после запятой (для тегов,
// для которых needsRounding). Числа возвращает как float, прочее — как есть.
func round1(tag string, v any) any {
	if !needsRounding(tag) {
		return v
	}
	switch n := v.(type) {
	case int:
		return math.Round(float64(n)*10) / 10
	case float64:
		return math.Round(n*10) / 10
	}
	return v
}

// MarshalJSON выводит только общие теги (commonContractTags) в фиксированном
// порядке; отсутствующие пропускаются. Значения числовых тегов округляются
// до 1 знака после запятой (round1).
func (v valuesContract) MarshalJSON() ([]byte, error) {
	if len(v) == 0 {
		return []byte("{}"), nil
	}
	var buf bytes.Buffer
	buf.WriteByte('{')
	first := true
	for _, key := range commonContractTags {
		raw, ok := v[key]
		if !ok {
			continue
		}
		k, err := json.Marshal(key)
		if err != nil {
			return nil, err
		}
		val, err := json.Marshal(round1(key, raw))
		if err != nil {
			return nil, err
		}
		if !first {
			buf.WriteByte(',')
		}
		first = false
		buf.Write(k)
		buf.WriteByte(':')
		buf.Write(val)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// DeviceResult — результат опроса одного инвертора. Поля для снимка
// (см. deviceSnapshot) отдельные.
type DeviceResult struct {
	OK         bool
	HasData    bool
	DeviceSN   string // серийный номер даталоггера (десятичный)
	InverterSN string // серийный номер инвертора (строка; для Sofar это ASCII-строка HW-регистров)
	Values     valuesContract
	Time       time.Time // время актуальности данных (для MPPT API — timestamp ответа)
}

// deviceSnapshot — структура снимка, сохраняемого в Redis (HASH current и
// временной ряд series). JSON-представление — контракт для дашборда и PG.
type deviceSnapshot struct {
	Name       string         `json:"name"`
	IP         string         `json:"ip"`
	Timestamp  string         `json:"timestamp"`
	DeviceSN   string         `json:"device_sn,omitempty"`
	InverterSN string         `json:"inverter_sn,omitempty"`
	Order      int            `json:"order,omitempty"`
	Values     valuesContract `json:"values"`
}

func int16val(v uint16) int {
	return int(int16(v))
}

func faultNames(mask uint16) []string {
	var names []string
	for _, fb := range faultBits {
		if mask&fb.Bit != 0 {
			names = append(names, fb.Name)
		}
	}
	if names == nil {
		names = []string{"no_error"}
	}
	return names
}

// Универсальный контракт значений (values) — в файл попадают ТОЛЬКО теги, которые
// обе марки (Deye и Sofar) отдают в одинаковых единицах: 15 общих тегов
// (commonContractTags). Единица зашита в суффикс имени: *_voltage — V, *_current — A,
// ac_*_power — W (ac_reactive_power — var), grid_frequency — Hz, energy_* — kWh.
// Числовые значения *voltage/*current/*power/energy*/temperature* округляются до
// 1 знака (round1). Бренд-специфичные поля в values не попадают — они в raw_registers.

// putSofarSimple пишет 16-битный регистр в контракт: int при ratio==1, иначе float.
func putSofarSimple(out valuesContract, regs map[uint16]uint16, addr uint16, key string, ratio float64) {
	v, ok := regs[addr]
	if !ok {
		return
	}
	if ratio == 1 {
		out[key] = int16val(v)
	} else {
		out[key] = float64(int16val(v)) * ratio
	}
}

// putSofarU32 пишет 32-битное значение (hi*65536+lo) с ratio.
func putSofarU32(out valuesContract, regs map[uint16]uint16, hiAddr, loAddr uint16, key string, ratio float64) {
	hi, ok1 := regs[hiAddr]
	lo, ok2 := regs[loAddr]
	if !ok1 || !ok2 {
		return
	}
	val := float64(hi)*65536 + float64(lo)
	if ratio == 1 {
		out[key] = val
	} else {
		out[key] = val * ratio
	}
}

// mapSofarRegisters строит значения универсального контракта из блока 0x0000-0x0027.
func mapSofarRegisters(regs map[uint16]uint16) valuesContract {
	out := valuesContract{}

	if v, ok := regs[0x0000]; ok {
		s, ok := statusNames[v]
		if !ok {
			s = fmt.Sprintf("unknown(%d)", v)
		}
		out["inverter_status"] = s
	}
	for i, name := range []string{"fault_1", "fault_2", "fault_3", "fault_4", "fault_5"} {
		if v, ok := regs[uint16(0x0001+i)]; ok {
			out[name] = faultNames(v)
		}
	}

	// PV входы (V, A, W)
	putSofarSimple(out, regs, 0x0006, "pv1_voltage", 0.1)
	putSofarSimple(out, regs, 0x0007, "pv1_current", 0.01)
	putSofarSimple(out, regs, 0x0008, "pv2_voltage", 0.1)
	putSofarSimple(out, regs, 0x0009, "pv2_current", 0.01)
	putSofarSimple(out, regs, 0x000A, "pv1_power", 10)
	putSofarSimple(out, regs, 0x000B, "pv2_power", 10)

	// AC выход: активная W, реактивная var, частота Hz
	putSofarSimple(out, regs, 0x000C, "ac_active_power", 10)
	putSofarSimple(out, regs, 0x000D, "ac_reactive_power", 10) // ×0.01 kVar → var
	putSofarSimple(out, regs, 0x000E, "grid_frequency", 0.01)

	// Фазы L1/L2/L3 (V, A)
	putSofarSimple(out, regs, 0x000F, "l1_voltage", 0.1)
	putSofarSimple(out, regs, 0x0010, "l1_current", 0.01)
	putSofarSimple(out, regs, 0x0011, "l2_voltage", 0.1)
	putSofarSimple(out, regs, 0x0012, "l2_current", 0.01)
	putSofarSimple(out, regs, 0x0013, "l3_voltage", 0.1)
	putSofarSimple(out, regs, 0x0014, "l3_current", 0.01)

	// Энергия (kWh) и время
	putSofarU32(out, regs, 0x0015, 0x0016, "energy_total", 1) // 32 бит, уже kWh
	putSofarU32(out, regs, 0x0017, 0x0018, "time_total", 1)   // 32 бит, h
	putSofarSimple(out, regs, 0x0019, "energy_today", 0.01)   // ×10 Wh → kWh
	putSofarSimple(out, regs, 0x001A, "time_today", 1)        // min

	// Температуры (C), шина
	putSofarSimple(out, regs, 0x001B, "temperature_module", 1)
	putSofarSimple(out, regs, 0x001C, "temperature_inner", 1)
	putSofarSimple(out, regs, 0x001D, "bus_voltage", 0.1)

	// Диагностика Sofar
	putSofarSimple(out, regs, 0x001E, "pv1_sample_cpu_voltage", 0.1)
	putSofarSimple(out, regs, 0x001F, "pv1_sample_cpu_current", 0.01)
	putSofarSimple(out, regs, 0x0020, "countdown_time", 1)
	putSofarSimple(out, regs, 0x0021, "alert", 1)
	putSofarSimple(out, regs, 0x0022, "input_mode", 1)
	putSofarSimple(out, regs, 0x0023, "comm_board_msg", 1)
	putSofarSimple(out, regs, 0x0024, "insulation_pv1_to_ground", 1)
	putSofarSimple(out, regs, 0x0025, "insulation_pv2_to_ground", 1)
	putSofarSimple(out, regs, 0x0026, "insulation_pv_minus_to_ground", 1)

	if v, ok := regs[0x0027]; ok {
		c, ok := countries[v]
		if !ok {
			c = fmt.Sprintf("unknown(%d)", v)
		}
		out["country"] = c
	}
	return out
}

// probableSofarBlock возвращает true, если блок регистров выглядит как настоящий
// полный блок LSW-3 (0x0000-0x0027), а не сбойный кадр: логгер иногда отдаёт PDU
// с валидным CRC, но мусором (повторяющееся слово вместо регистров, частота ~220 Гц,
// пустая энергия), что даёт абсурдные пики мощности вроде ac_active_power = 220680 W.
// Валидных полных блоков 20-40 регистров; «дубль» 16 регистров (0x0010-0x001F) и
// Placeholder тоже не подходят. Физические пороги консервативные (с запасом от
// реальных значений инвертора 2.5 kVA), чтобы не отсечь аномальные, но реальные данные.
func probableSofarBlock(r map[uint16]uint16) bool {
	if len(r) < 20 {
		return false
	}

	// Частота сети: 0 или 40..80 Гц (raw ×0.01 → 4000..8000). ~220 Гц (22000) — мусор.
	if v, ok := r[0x000E]; ok {
		if hz := int16(v); hz != 0 && (hz < 4000 || hz > 8000) {
			return false
		}
	}

	// Напряжения фаз L1/L2/L3 (raw ×0.1): физически 0..270 В.
	for _, addr := range []uint16{0x000F, 0x0011, 0x0013} {
		if v, ok := r[addr]; ok {
			if volts := float64(int16(v)) * 0.1; volts < -300 || volts > 300 {
				return false
			}
		}
	}

	// PV1/PV2 напряжение (raw ×0.1): физически 0..450 В.
	for _, addr := range []uint16{0x0006, 0x0008} {
		if v, ok := r[addr]; ok {
			if volts := float64(int16(v)) * 0.1; volts < -450 || volts > 450 {
				return false
			}
		}
	}

	// Активная и реактивная мощность (raw ×10): для 2.5 kVA инвертора
	// |W| не бывает > 10 кВт. Пик 220680 Вт (raw 22068) — отсекаем.
	for _, addr := range []uint16{0x000C, 0x000D} {
		if v, ok := r[addr]; ok {
			if w := int64(int16(v)) * 10; w > 10000 || w < -10000 {
				return false
			}
		}
	}

	return true
}

// deyeSensor — маппинг регистра Deye string/grid-tie инвертора
// (из kbialek/deye-inverter-mqtt, диапазоны 0x3C-0x74 и 0xC6-0xD2).
// Name — имя для raw_registers; Tag — имя универсального контракта для values.
type deyeSensor struct {
	Name   string
	Tag    string
	Ratio  float64
	Unit   string
	Signed bool
	Offset float64
	Double bool // 32-бит (2 регистра), low word first
}

// 0x58 AC reactive power Deye — ×0.01 kvar (как у Sofar ×0.01 kVar), в var → ratio 10.
var deyeRegMap = map[uint16]deyeSensor{
	0x3C: {"production_today", "energy_today", 0.1, "kWh", false, 0, false},
	0x3E: {"uptime", "uptime", 1, "min", false, 0, false},
	0x3F: {"total_production", "energy_total", 0.1, "kWh", false, 0, true},
	0x46: {"grid_l12_voltage", "grid_l12_voltage", 0.1, "V", false, 0, false},
	0x47: {"grid_l23_voltage", "grid_l23_voltage", 0.1, "V", false, 0, false},
	0x48: {"grid_l31_voltage", "grid_l31_voltage", 0.1, "V", false, 0, false},
	0x49: {"l1_voltage", "l1_voltage", 0.1, "V", false, 0, false},
	0x4A: {"l2_voltage", "l2_voltage", 0.1, "V", false, 0, false},
	0x4B: {"l3_voltage", "l3_voltage", 0.1, "V", false, 0, false},
	0x4C: {"l1_current", "l1_current", 0.1, "A", false, 0, false},
	0x4D: {"l2_current", "l2_current", 0.1, "A", false, 0, false},
	0x4E: {"l3_current", "l3_current", 0.1, "A", false, 0, false},
	0x4F: {"ac_frequency", "grid_frequency", 0.01, "Hz", false, 0, false},
	0x52: {"dc_total_power", "dc_total_power", 0.1, "W", false, 0, false},
	0x54: {"ac_apparent_power", "ac_apparent_power", 0.1, "W", false, 0, false},
	0x56: {"ac_active_power", "ac_active_power", 0.1, "W", false, 0, true},
	// 0x58 AC reactive power: ×0.1 var (raw 365 → 36.5 var, физически правдоподобно;
	// ×10 давал 3650 var — абсурд при P≈404W). Единицы совпадают с Sofar (var).
	0x58: {"ac_reactive_power", "ac_reactive_power", 0.1, "var", false, 0, false},
	0x5A: {"radiator_temperature", "temperature_radiator", 0.1, "C", false, -100, false},
	0x5B: {"igbt_temperature", "temperature_igbt", 0.1, "C", false, -100, false},
	0x6D: {"pv1_voltage", "pv1_voltage", 0.1, "V", false, 0, false},
	0x6E: {"pv1_current", "pv1_current", 0.1, "A", false, 0, false},
	0x6F: {"pv2_voltage", "pv2_voltage", 0.1, "V", false, 0, false},
	0x70: {"pv2_current", "pv2_current", 0.1, "A", false, 0, false},
	0x71: {"pv3_voltage", "pv3_voltage", 0.1, "V", false, 0, false},
	0x72: {"pv3_current", "pv3_current", 0.1, "A", false, 0, false},
	0x73: {"pv4_voltage", "pv4_voltage", 0.1, "V", false, 0, false},
	0x74: {"pv4_current", "pv4_current", 0.1, "A", false, 0, false},
	0xC6: {"load_power", "load_power", 1, "W", true, 0, true},
	0xC8: {"daily_load_consumption", "energy_load_today", 0.01, "kWh", false, 0, false},
	0xC9: {"total_load_consumption", "energy_load_total", 0.1, "kWh", false, 0, true},
	// 0xCB "Grid power" Deye НЕ маппится: тег grid_power зарезервирован контрактом
	// за устройством МАП (kindMAP) — плашки/график МАП строятся по нему. Маппинг
	// сюда из Deye-регистра давал бы ложные нули в ряду мощности сети МАП.
	0xCD: {"daily_energy_sold", "energy_sold_today", 0.01, "kWh", false, 0, false},
	0xCE: {"total_energy_sold", "energy_sold_total", 0.1, "kWh", false, 0, true},
	0xD0: {"daily_energy_bought", "energy_bought_today", 0.01, "kWh", false, 0, false},
	0xD1: {"total_energy_bought", "energy_bought_total", 0.1, "kWh", false, 0, true},
}

// mapDeyeRegisters строит значения универсального контракта Deye string-инвертора
// из набора регистров (по абсолютному адресу). Пишет по Tag (контрактное имя).
func mapDeyeRegisters(regs map[uint16]uint16) valuesContract {
	out := valuesContract{}
	for addr, def := range deyeRegMap {
		if !def.Double {
			v, ok := regs[addr]
			if !ok {
				continue
			}
			iv := int(int16(v))
			if def.Signed {
				out[def.Tag] = float64(iv)*def.Ratio + def.Offset
			} else {
				out[def.Tag] = float64(v)*def.Ratio + def.Offset
			}
			continue
		}
		// 32-бит, low word first (addr = low, addr+1 = high)
		lo, ok1 := regs[addr]
		hi, ok2 := regs[addr+1]
		if !ok1 || !ok2 {
			continue
		}
		val := uint32(hi)<<16 | uint32(lo)
		if def.Signed {
			out[def.Tag] = float64(int32(val))*def.Ratio + def.Offset
		} else {
			out[def.Tag] = float64(val)*def.Ratio + def.Offset
		}
	}
	// Мощность PV-входов: Deye string отдаёт только V и I на каждый вход
	// (0x6D/0x6E — PV1, 0x6F/0x70 — PV2); мощность считаем как P = V * I (W).
	if v1, ok := out["pv1_voltage"]; ok {
		if i1, ok := out["pv1_current"]; ok {
			out["pv1_power"] = v1.(float64) * i1.(float64)
		}
	}
	if v2, ok := out["pv2_voltage"]; ok {
		if i2, ok := out["pv2_current"]; ok {
			out["pv2_power"] = v2.(float64) * i2.(float64)
		}
	}
	return out
}

// clients — кэш объектов solarman.Client по ip (конфиг + счётчик порядкового
// номера кадра). Сам TCP-сокет НЕ переиспользуется: Client.Exchange открывает
// свежее соединение на каждый запрос и закрывает его после чтения ответа
// (логгеры отвечают медленно и могут оставлять «висящее» соединение).
// Опрос одного инвертора всегда идёт из одной горутины, поэтому счётчик кадра
// не гоняется между опросами.
var (
	clientsMu    sync.Mutex
	clientsByKey = map[string]*solarman.Client{}
)

func clientFor(ip string, sn uint32) *solarman.Client {
	key := ip + ":" + port
	clientsMu.Lock()
	defer clientsMu.Unlock()
	if c, ok := clientsByKey[key]; ok {
		return c
	}
	c := &solarman.Client{
		Address:    key,
		DeviceSN:   sn,
		IdleWindow: 4 * time.Second,
		Timeout:    timeout,
	}
	clientsByKey[key] = c
	return c
}

// MAP clients — переиспользуемое TCP-соединение, но для МАП гейт отвечает
// медленно, и соединение живёт. Каждая цель (КЭС) опрашивается из одной горутины,
// поэтому сокет не гоняется между опросами.
var (
	mapClientsMu    sync.Mutex
	mapClientsByKey = map[string]*modbusmap.Client{}
)

// mapClientFor возвращает (и при первом обращении создаёт) клиент МАП-гейта
// для адреса ip:502 с Modbus-адресом unit.
func mapClientFor(ip string, unit byte) *modbusmap.Client {
	key := ip + ":" + modbusmap.DefaultPort
	mapClientsMu.Lock()
	defer mapClientsMu.Unlock()
	if c, ok := mapClientsByKey[key]; ok {
		return c
	}
	c := &modbusmap.Client{Address: key, Unit: unit}
	mapClientsByKey[key] = c
	return c
}

// mapMAPRegisters строит значения универсального контракта цели «КЭС» (МАП
// Титанатор — агрегат батареи/сети) из байт-ячеек МАП. Per-слотовые MPPT
// контроллеры больше не читаются через Modbus (они переехали на веб-API read_json);
// эта функция обслуживает единственный агрегат (slot = -1) из sunReceiver.json.
//
// Маппинг полей (ТЗ КЭС):
//   - l1_voltage = напряжение АКБ МАП _UAcc_med_VH/VL (0x405/0x406), (VH*256+VL)/10.
//     V_Bat самих контроллеров гейт МАП не отдаёт (проверено живьём 0x4D5=0),
//     поэтому источник — то же напряжение АКБ, что заряжают контроллеры.
//   - l1_current = ток АКБ _IAcc_med_A_u16_L/H (0x432/0x433), I[А]=(L+H*256)/16.
//     Ток АКБ (а не токи отдельных MPPT-контроллеров 0x530+2*slot).
//   - ac_active_power = l1_voltage × l1_current (W).
//   - grid_frequency = 0 (частоты сети инвертор МППТ не отдаёт).
//   - pv1/pv2, ac_reactive_power, l2/l3 — в values не пишутся (нет данных).
func mapMAPRegisters(cells map[uint16]byte) valuesContract {
	out := valuesContract{}

	// Напряжение АКБ МАП: _UAcc_med_VH=0x405, _UAcc_med_VL=0x406, U=(VH*256+VL)/10.
	vh, ok1 := cells[0x405]
	vl, ok2 := cells[0x406]
	if !ok1 || !ok2 {
		return out
	}
	uAcc := float64(vh)*256 + float64(vl)
	if uAcc <= 0 {
		return out
	}
	uAcc /= 10
	out["l1_voltage"] = uAcc
	out["battery_voltage"] = uAcc

	// Ток АКБ: _IAcc_med_A_u16_L=0x432, _IAcc_med_A_u16_H=0x433,
	// I[А] = (L + H*256)/16. Это более точный ток батареи (заряд/разряд АКБ);
	// токи MPPT (0x530) — это токи контроллеров, а не ток самой АКБ.
	var iAcc float64
	if l, okL := cells[0x432]; okL {
		if h, okH := cells[0x433]; okH {
			iAcc = float64(uint16(h)<<8|uint16(l)) / 16
		}
	}

	out["l1_current"] = iAcc
	out["ac_active_power"] = uAcc * iAcc
	out["grid_frequency"] = 0.0

	// ---- Данные батареи и сети МАП (для дашборда КЭС) ----
	// Напряжение сети: _UNET=0x422; UNET=0 → нет сети, иначе U=(UNET+100).
	if un, ok := cells[0x422]; ok {
		if un == 0 {
			out["grid_voltage"] = 0.0
		} else {
			out["grid_voltage"] = float64(un) + 100
		}
	}
	// Мощность сети: _PNET (16 бит) = 0x59A(L),0x59B(H), Pnet=((H*256+L)/8)*100.
	// Знак по _PNET_Sign_P=0x587 (как в mapread.py) семантики дашборда:
	// 1 — положительная (потребляем из сети), 0 — отрицательная (продажа/отдача в сеть).
	if lo, okL := cells[0x59A]; okL {
		if hi, okH := cells[0x59B]; okH {
			pnet := (float64(hi)*256 + float64(lo)) / 8 * 100
			if sign, okS := cells[0x587]; okS && sign == 0 {
				pnet = -pnet
			}
			out["grid_power"] = pnet
		}
	}
	// Мощность батареи со знаком (семантика дашборда: заряд = отрицательная,
	// отдача в нагрузку/сеть = положительная). Источник — фирменная логика mapread.py:
	//   - режим заряда (MODE=0x400 == 4): P = −(I_АКБ × U_АКБ) (заряд — отрицательная),
	//     ток АКБ _IAcc_med = 0x432/0x433, I[А]=(L+H*256)/16;
	//   - иначе (генерация/подкачка/трансляция): P = +PLoad (отдача в нагрузку — положительная).
	//     Предпочитаем 16-битную PLoad_8 (0x59E/0x59F), но гейт чаще отдаёт её нулём —
	//     тогда берём 8-битную _PLoad_L (0x409, ×100).
	var batP float64
	if mode, okM := cells[0x400]; okM && mode == 4 {
		if l, okL := cells[0x432]; okL {
			if h, okH := cells[0x433]; okH {
				batP = -(float64(uint16(h)<<8|uint16(l)) / 16) * uAcc
			}
		}
	} else {
		if lo, okL := cells[0x59E]; okL {
			if hi, okH := cells[0x59F]; okH {
				if uv := uint16(hi)<<8 | uint16(lo); uv > 0 {
					batP = float64(uv) / 8 * 100
				}
			}
		}
		if batP == 0 {
			if raw, ok := cells[0x409]; ok {
				batP = float64(raw) * 100
			}
		}
	}
	out["battery_power"] = batP
	return out
}

// asciiFromRegisters собирает ASCII-строку из регистров (2 байта/регистр,
// старший байт первый — как в строках SN-областей Deye и Sofar). Не-печатные
// байты (напр. регистр длины 0x2000 у Sofar) пропускаются.
func asciiFromRegisters(vals []uint16) string {
	var b strings.Builder
	for _, v := range vals {
		hi := byte(v >> 8)
		lo := byte(v & 0xFF)
		if hi >= 0x20 && hi <= 0x7E {
			b.WriteByte(hi)
		}
		if lo >= 0x20 && lo <= 0x7E {
			b.WriteByte(lo)
		}
	}
	if b.Len() == 0 {
		return ""
	}
	return b.String()
}

// sofarVersionsRe — суффикс версий HW-строки Sofar: три подряд идущих токена
// «V<цифры>» в конце (аппаратная/программная/польз. платформы), напр.
// "…V480V100V480". Такие версии выводить в качестве серийного номера НЕ нужно.
var sofarVersionsRe = regexp.MustCompile(`V\d+V\d+V\d+$`)

// trimSofarVersions отрезает от HW-строки Sofar хвост-версии (если он есть),
// оставляя только серийный код инвертора. Напр. "SA3ES033KAQ846V480V100V480" →
// "SA3ES033KAQ846".
func trimSofarVersions(s string) string {
	loc := sofarVersionsRe.FindStringIndex(s)
	if loc == nil || loc[0] == 0 {
		return s
	}
	return s[:loc[0]]
}

func pollDevice(t invTarget) DeviceResult {
	res := DeviceResult{OK: true}
	client := clientFor(t.IP, t.LoggerSN)
	// Sofar LSW-3 шлёт кадры с паузами до ~6.5 с (pacing) — окно тишины шире.
	if t.Kind == kindSofar {
		client.IdleWindow = 8 * time.Second
	}

	var frames []solarman.Frame

	switch t.Kind {
	case kindDeyeString:
		result := map[uint16]uint16{}
		for _, r := range [][2]uint16{{0x3C, 0x39}, {0xC6, 0x0D}} {
			pdus, fr, err := client.ReadRegistersDeye(r[0], r[1], 1)
			if err != nil {
				continue
			}
			frames = append(frames, fr...)
			for _, p := range pdus {
				if p.CRC != p.CRCCalc {
					continue
				}
				for k := 0; k < len(p.Values); k++ {
					result[r[0]+uint16(k)] = p.Values[k]
				}
			}
		}
		if len(result) > 0 {
			res.HasData = true
			res.Values = mapDeyeRegisters(result)
		}
		// Серийный номер инвертора Deye — ASCII-строка в регистрах 0x0003-0x0007
		// (10 цифр; проверено на живых .70/.79/.91/.92/.93, напр. .70 = "2405018274").
		// Чтение с ретраями: логгер иногда молчит/отвечает не полностью.
		for attempt := 1; attempt <= 3 && res.InverterSN == ""; attempt++ {
			pdus, _, err := client.ReadRegistersDeye(0x0003, 0x0005, 1)
			if err != nil {
				log.Printf("%s: serial func03 read err (attempt %d): %v", t.IP, attempt, err)
				continue
			}
			for _, p := range pdus {
				if p.CRC != p.CRCCalc {
					continue
				}
				if sn := asciiFromRegisters(p.Values); sn != "" {
					res.InverterSN = sn
					break
				}
			}
		}

	case kindSofar:
		// Sofar LSW-3/.76 отвечает данными ТОЛЬКО на кадр с 15-байтным datafield
		// (как Deye) и реальным SN логгера, а не на 12-байтный BuildReadFrame
		// (на него отдаёт только heartbeat с кодом 0x05). Проверено 2026-09-07:
		// на live .76 кадр 15b+SN даёт полный блок 0x0000-0x0027 (40 reg, func 03).
		pdus, fr, err := client.ReadRegistersDeye(0x0000, 0x0028, 1)
		if err != nil {
			res.OK = false
			return res
		}
		frames = fr
		// Может прийти как полный блок (bytecount 80), так и «дубль» (16 reg)
		// и сбойные кадры — берём самую большую валидную PDU, которая выглядит
		// как НАСТОЯЩИЙ полный блок (см. probableSofarBlock).
		var bestResult map[uint16]uint16
		bestLen := 0
		for i := range pdus {
			p := pdus[i]
			if p.CRC != p.CRCCalc {
				continue
			}
			result := make(map[uint16]uint16, len(p.Values))
			for k := 0; k < len(p.Values); k++ {
				result[uint16(k)] = p.Values[k]
			}
			if !probableSofarBlock(result) {
				continue
			}
			if len(p.Values) > bestLen {
				bestLen = len(p.Values)
				bestResult = result
			}
		}
		result := bestResult
		if len(result) > 0 {
			res.HasData = true
			res.Values = mapSofarRegisters(result)
		}
		// Серийный номер инвертора Sofar — ASCII-строка HW-диапазона 0x2000-0x200D
		// (func 04; первые 2 байта = длина строки, затем ASCII на 2 байта/регистр).
		// Может быть НЕ числом (строка), напр. .76 = "SA3ES127LC1055V480V100V480".
		for attempt := 1; attempt <= 3 && res.InverterSN == ""; attempt++ {
			hwpdus, _, herr := client.ReadRegistersDeyeFn(0x2000, 0x000E, 1, 0x04)
			if herr != nil {
				log.Printf("%s: serial func04 read err (attempt %d): %v", t.IP, attempt, herr)
				continue
			}
			for _, p := range hwpdus {
				if p.CRC != p.CRCCalc {
					continue
				}
				if sn := asciiFromRegisters(p.Values); sn != "" {
					res.InverterSN = trimSofarVersions(sn)
					break
				}
			}
		}

	case kindMAP:
		// МАП Титанатор («КЭС») — Modbus TCP (порт 502), не Solarman-кадр.
		// Читаем блоки байт-ячеек: 0x400 (0x400..0x43F — режим/АКБ/сети),
		// токи MPPT (0x530-0x551) и мощности сети/батареи (0x580-0x5A3, т.ч.
		// 0x587 sign, 0x59A/0x59B PNET, 0x59E/0x59F PLOAD).
		mc := mapClientFor(t.IP, t.Unit)
		cells := map[uint16]byte{}
		// Блок 0x400 (0x20 слов = 0x400..0x43F) охватывает режим (0x400), мощности
		// (_PLoad_L 0x409), напряжения (_UNET 0x422, _INET 0x423, _PNET_L 0x424)
		// и ток АКБ (_IAcc_med 0x432/0x433). Отдельного чтения 0x420 нет — оно было
		// строгим подмножеством этого блока (ReadRegisters проверяет bytecount,
		// поэтому при успехе 0x422 уже в cells).
		if b, err := mc.ReadRegisters(0x0400, 0x20); err == nil {
			for i := 0; i < len(b); i++ {
				cells[0x0400+uint16(i)] = b[i]
			}
		}
		if b, err := mc.ReadRegisters(0x0530, 0x40); err == nil {
			for i := 0; i < len(b); i++ {
				cells[0x0530+uint16(i)] = b[i]
			}
		}
		if b, err := mc.ReadRegisters(0x0580, 0x24); err == nil {
			for i := 0; i < len(b); i++ {
				cells[0x0580+uint16(i)] = b[i]
			}
		}
		res.Values = mapMAPRegisters(cells)
		if _, has := cells[0x405]; has && cells[0x406] > 0 {
			res.HasData = true
		}
		if len(res.Values) > 0 {
			res.HasData = true
			res.DeviceSN = fmt.Sprintf("map-%s", devKey(t))
		}

	}

	for _, f := range frames {
		if f.DeviceSN != 0 {
			res.DeviceSN = strconv.FormatUint(uint64(f.DeviceSN), 10)
			break
		}
	}

	return res
}

// pollMPPTFromArr строит DeviceResult для MPPT-контроллера (t.Slot) из УЖЕ
// полученного ответа ПАК «Малина» (arr = read_json.php?device=mppt). Вынесена из
// pollDevice, чтобы один HTTP-запрос FetchMPPTs переиспользовался для всех слотов
// за цикл, а не повторялся на каждый контроллер.
func pollMPPTFromArr(t invTarget, arr []mpptRaw) DeviceResult {
	res := DeviceResult{OK: true}
	if t.Slot < 0 || t.Slot >= len(arr) {
		res.OK = false
		log.Printf("%s: mppt api: слот %d вне диапазона (получено %d контроллеров)", t.IP, t.Slot, len(arr))
		return res
	}
	vals, ts, ok := mapMPPTAPI(arr[t.Slot])
	if !ok {
		res.OK = false
		log.Printf("%s: mppt api: слот %d не дал данных", t.IP, t.Slot)
		return res
	}
	res.OK = true
	res.HasData = true
	res.Values = vals
	if arr[t.Slot].UID != "" {
		res.DeviceSN = fmt.Sprintf("mppt-%s", arr[t.Slot].UID)
	}
	// Актуальность данных из API (для таймстампа снимка).
	res.Time = ts
	return res
}

// runMapPoll — отдельный 1-секундный цикл опроса быстрых целей — МАП (kindMAP,
// Modbus TCP) и MPPT-контроллеров (веб-API ПАК «Малина») — и записи в Redis через
// SaveSnapshotWindow: в пределах каждого 10-секундного окна остаётся ровно одна
// (последняя) строка на устройство. МАП-цели исключены из 10-сек циклов инверторов
// (см. runInverterPoll); MPPT не регистрируются в конфиге вовсе (см. pollAndSaveMap).
func runMapPoll(store *redisStore, stop <-chan struct{}) {
	const pollEvery = time.Second
	ticker := time.NewTicker(pollEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			pollAndSaveMap(store, time.Now())
		case <-stop:
			return
		}
	}
}

// pollAndSaveMap опрашивает быстрые источники параллельно и пишет в Redis через
// SaveSnapshotWindow (одна строка за каждые 10 секунд + актуальное current):
//   - МАП (kindMAP, Modbus TCP) — цели из targets;
//   - MPPT-контроллеры — ДИНАМИЧЕСКИ: состав определяется фактически подключёнными
//     к ПАК «Малина» контроллерами из ответа read_json.php?device=mppt, а не из
//     sunReceiver.json. Поэтому MPPT появляются/исчезают с дашборда по факту наличия
//     в ответе API; исчезнувшие удаляются и из HASH current (их строка не числится
//     «актуальной».
//
// saveWindowSnapshot складывает результат опроса в deviceSnapshot (ts = res.Time
// при наличии, иначе now) и пишет в Redis через SaveSnapshotWindow. Общий для
// МАП- и MPPT-веток pollAndSaveMap.
func saveWindowSnapshot(store *redisStore, t invTarget, res DeviceResult, now time.Time) {
	if !res.OK || !res.HasData {
		log.Printf("%s: %s %s", t.IP, t.Kind, describeResult(res))
		return
	}
	ts := now
	if !res.Time.IsZero() {
		ts = res.Time
	}
	snap := deviceSnapshot{
		Name:       t.Name,
		IP:         devKey(t),
		Timestamp:  ts.Format(time.RFC3339),
		DeviceSN:   res.DeviceSN,
		InverterSN: res.InverterSN,
		Order:      t.Order,
		Values:     res.Values,
	}
	if err := store.SaveSnapshotWindow(snap, ts); err != nil {
		log.Printf("redis save %s: %v", devKey(t), err)
	}
}

func pollAndSaveMap(store *redisStore, now time.Time) {
	var wg sync.WaitGroup
	activeMPPT := map[string]struct{}{}
	// МАП (батарея/сеть) — из targets (Modbus) или через веб-API ПАК «Малина».
	for i := range targets {
		t := targets[i]
		if t.Kind != kindMAP {
			continue
		}
		wg.Add(1)
		go func(t invTarget) {
			defer wg.Done()
			saveWindowSnapshot(store, t, pollDevice(t), now)
		}(t)
	}
	// МАП через веб-API (map.disabled=true): Modbus-пулер не запущен (цели kindMAP в
	// targets нет), параметры батареи/сети берём из read_json.php?device=map.
	if mapAPI != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			t := invTarget{IP: mapAPI.ip, Name: mapAPI.name, Kind: kindMAP, Slot: -1, Order: mapAPI.order}
			saveWindowSnapshot(store, t, pollMAPAPI(), now)
		}()
	}
	// MPPT-контроллеры — по факту подключённых из API. Состав определяется
	// фактически подключёнными к ПАК «Малина» контроллерами (один HTTP-запрос
	// FetchMPPTs переиспользуется для всех слотов за цикл). Каждый контроллер
	// ответа — отдельное устройство с ключом devKey(MPPT слотом); имя MPPT-<n+1>.
	if mppt != nil {
		arr, err := mppt.FetchMPPTs()
		if err != nil {
			log.Printf("mppt api: %v", err)
		} else {
			for slot := range arr {
				slot := slot
				activeMPPT[saveMPPTKey(slot)] = struct{}{}
				wg.Add(1)
				go func(slot int) {
					defer wg.Done()
					t := invTarget{IP: mppt.Host, Name: fmt.Sprintf("MPPT-%d", slot+1), Kind: kindMPPT, Slot: slot, Order: mpptOrderBase + slot}
					saveWindowSnapshot(store, t, pollMPPTFromArr(t, arr), now)
				}(slot)
			}
		}
	}
	wg.Wait()
	// Исчезнувшие MPPT-контроллеры (не в ответе API) убираем из HASH current, чтобы
	// их строка не показывалась на дашборде как актуальная.
	store.PruneMPPT(activeMPPT)
}

// saveMPPTKey возвращает devKey (поле IP снимка) для MPPT-слота: host#mppt<slot>.
func saveMPPTKey(slot int) string {
	return fmt.Sprintf("%s#mppt%d", mppt.Host, slot)
}

func main() {
	log.SetFlags(log.Ltime)

	cfgPath := configPath()
	if _, err := os.Stat(cfgPath); os.IsNotExist(err) {
		// `go run .`: бинарник во временном каталоге go-сборки — ищем sunReceiver.json в CWD.
		cfgPath = "sunReceiver.json"
	}
	var dbCfg *dbConfig
	var meterSec *meterSection
	var mpptSec *mpptSection
	var err error
	targets, dbCfg, meterSec, mpptSec, err = loadConfig(cfgPath)
	if err != nil {
		log.Fatal(err)
	}

	// Дефолты адресов БД берутся из раздела "db" sunReceiver.json, иначе — локальные.
	redisAddr := flag.String("redis", defaultRedisAddr(dbCfg), "адрес Redis (хост:порт)")
	pgDSN := flag.String("pg", defaultPGDSN(dbCfg), "DSN PostgreSQL для persistent-хранилища (пустая строка — выключить)")
	restoreWindow := flag.Duration("pg-restore-window", 30*24*time.Hour, "окно РЕСТАВРАЦИИ Redis из PG при пустом Redis")
	dashboardAddr := flag.String("dashboard", ":8080", "адрес веб-дашборда (пустая строка — выключить)")
	flag.Parse()

	log.Printf("poller started: config=%s targets=%v period=%s", cfgPath, targets, pollPeriod)

	// Конфигурация веб-API ПАК «Малина» для мониторинга MPPT (КЭС) — раздел "mppt"
	// sunReceiver.json (бывший malina.json).
	mppt = loadMPPTSite(mpptSec)
	// Если МАП опрашивается через веб-API (map.disabled=true), обязателен доступ к
	// ПАК «Малина» (раздел "mppt") — иначе неоткуда взять параметры батареи/сети.
	if mapAPI != nil && mppt == nil {
		log.Fatalf("config: МАП настроен через веб-API (map.disabled=true), но раздел mppt неполный — нужны base_url, mppt_path, login, password")
	}
	// Конфигурация электросчётчика DDS238 — раздел "meter" sunReceiver.json
	// или файл dds238.json (обратная совместимость).
	meterCfg := loadMeterConfig(meterSec)
	if desc := describeMeterConfig(meterCfg); desc != "" {
		log.Printf("meter: %s", desc)
	} else {
		log.Printf("meter: не настроен (нет dds238.json рядом с бинарником) — опрос счётчика отключён")
	}

	rdb, err := openRedis(*redisAddr)
	if err != nil {
		log.Fatalf("redis: %v", err)
	}
	defer rdb.Close()
	store := &redisStore{rdb: rdb, ctx: context.Background()}

	// Persistent-хранилище PostgreSQL: только усреднённые 5-минутные точки,
	// пишутся фоновым процессом аккумуляции (см. accumulator.go) после накопления
	// данных за 5 минут. Реставрация Redis при пустом хранилище.
	stopBG := make(chan struct{})
	var pg *pgStore
	if *pgDSN != "" {
		pg, err = openPG(*pgDSN)
		if err != nil {
			log.Printf("pg: %v (persistent-хранилище отключено)", err)
		} else {
			defer pg.Close()
			log.Printf("pg: persistent-хранилище подключено (5-минутные усреднённые точки)")
			// Конвертируем старую сырую таблицу snapshots в 5-минутные средние.
			if merr := pg.MigrateLegacy(); merr != nil {
				log.Printf("pg legacy миграция: %v", merr)
			}
			// Если Redis пуст — восстановить в нём данные из PG в фоне.
			empty, cerr := store.IsEmpty()
			if cerr != nil {
				log.Printf("redis empty-check: %v", cerr)
			} else if empty {
				go restoreRedisFromPG(store, pg, *restoreWindow)
			}
		}
	} else {
		log.Printf("pg: отключено (флаг -pg пустой); работаем только через Redis")
	}

	// Фоновые процессы: усреднение данных за 5 минут в PG и очистка старых
	// данных Redis (старше 2 календарных суток).
	go runAccumulator(store, pg, stopBG)
	go runRedisCleanup(store, stopBG)
	// МАП («КЭС», Modbus TCP) и MPPT-контроллеры (веб-API ПАК «Малина»)
	// опрашиваются отдельно, 1 раз в секунду, и пишутся в Redis со специальной
	// логикой «одна строка за 10 с» (см. SaveSnapshotWindow).
	go runMapPoll(store, stopBG)
	// Электросчётчик DDS238 — 1 раз в секунду (мгновенные значения в Redis +
	// посуточные тарифные захваты в PG, см. runMeterPoll и meter_tariff.go).
	if meterCfg != nil {
		go runMeterPoll(store, pg, meterCfg, stopBG)
		// Добор пропущенных тарифных границ («ближайшее из зафиксированного»),
		// см. meter_backfill.go.
		go runMeterBackfill(store, pg, meterCfg, stopBG)
	}
	defer close(stopBG)

	if *dashboardAddr != "" {
		go serveDashboard(*dashboardAddr, store, pg)
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)

	// Каждый инвертор (Deye/Sofar) опрашивается в СВОЁМ независимом цикле с
	// периодом pollPeriod (10 с). Завис/таймаутит один текущий опрос одного
	// инвертора — остальные продолжают опрашиваться и сохранять снимки строго
	// раз в 10 секунд, влияния друг на друга нет вовсе. Снятие делается по
	// закрытию stopBG (см. defer close(stopBG)).
	for _, t := range targets {
		if t.Kind == kindMAP || t.Kind == kindMPPT {
			continue // МАП и MPPT API опрашиваются отдельным 1-сек циклом (runMapPoll)
		}
		go runInverterPoll(store, t, stopBG)
	}

	<-sig
	log.Println("shutting down")
}

// runInverterPoll — непрерывный цикл опроса ОДНОГО инвертора (Deye/Sofar) с
// периодом pollPeriod. Каждая итерация:
//   - ждёт тик собственного таймера (отсчитывается от старта итерации, поэтому
//     интервал остаётся ровно pollPeriod независимо от длительности опроса);
//   - опрашивает инвертор (pollDevice) и, если данные получены, пишет снимок в
//     Redis СРАЗУ, с фактическим временем получения.
//
// Так как у каждого инвертора свой таймер и своя горутина, зависший/таймаутящий
// инвертор никак не влияет на периодичность и запись других. Клиент solarman на
// этот IP используется только из этой горутины (инвариант «один IP — один
// опрос», см. clientsByKey). Останавливается по закрытию канала stop.
func runInverterPoll(store *redisStore, t invTarget, stop <-chan struct{}) {
	ticker := time.NewTicker(pollPeriod)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			t0 := time.Now()
			res := pollDevice(t)
			now := time.Now() // фактическое время получения данных этого инвертора
			log.Printf("%s: %s (%s)", t.IP, describeResult(res), now.Sub(t0).Round(time.Millisecond))
			if !res.OK || !res.HasData {
				// heartbeat_only / no data / ошибка — снимок не сохраняем
				continue
			}
			snap := deviceSnapshot{
				Name:       t.Name,
				IP:         t.IP,
				Timestamp:  now.Format(time.RFC3339),
				DeviceSN:   res.DeviceSN,
				InverterSN: res.InverterSN,
				Order:      t.Order,
				Values:     res.Values,
			}
			if err := store.SaveSnapshot(snap, now); err != nil {
				log.Printf("redis save %s: %v", t.IP, err)
				continue
			}
			log.Printf("saved %s: %s", t.Name, t.IP)
		case <-stop:
			return
		}
	}
}

func describeResult(res DeviceResult) string {
	if !res.OK {
		return "error"
	}
	if res.HasData {
		return "data=OK registers"
	}
	return "heartbeat_only"
}

// restoreRedisFromPG восстанавливает Redis из persistent-хранилища PostgreSQL
// (5-минутные усреднённые точки) за период [now-window, now], но не старше окна
// удержания Redis (последние 2 календарных суток), иначе фоновая очистка сразу
// удалит восстановленное. Запускается в фоне при пустом Redis. Для каждой точки
// вызывается SaveSnapshot (обновляет current и кладёт точку в месячный ZSET ряда).
func restoreRedisFromPG(store *redisStore, pg *pgStore, window time.Duration) {
	end := time.Now()
	start := recentCutoff(end)
	if w := end.Add(-window); w.After(start) {
		start = w
	}
	log.Printf("pg restore: восстанавливаю Redis из PG за [%s, %s]",
		start.Format(time.RFC3339), end.Format(time.RFC3339))
	snaps, err := pg.Averages(start, end)
	if err != nil {
		log.Printf("pg restore: query: %v", err)
		return
	}
	var restored int
	for _, snap := range snaps {
		ts, perr := time.Parse(time.RFC3339, snap.Timestamp)
		if perr != nil {
			continue
		}
		if serr := store.SaveSnapshot(snap, ts); serr != nil {
			log.Printf("pg restore: save %s: %v", snap.IP, serr)
			continue
		}
		restored++
	}
	log.Printf("pg restore: завершено, восстановлено точек: %d", restored)
}

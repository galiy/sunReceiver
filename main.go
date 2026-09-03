package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/galiy/sunReceiver/solarman"
)

const (
	port       = "8899"
	pollPeriod = 10 * time.Second
	timeout    = 15 * time.Second
	outDir     = "data"
)

type targetKind int

const (
	kindSofar targetKind = iota
	kindDeyeString
)

// invTarget — целевой инвертор. LoggerSN — серийный номер даталоггера,
// обязателен для Deye (иначе логгер отвечает кодом 0x06 "serial number not match").
// Name — логическое имя из config.json (например, "Deye Left").
type invTarget struct {
	IP       string
	Name     string
	LoggerSN uint32
	Kind     targetKind
}

// configTarget — запись инвертора в config.json.
type configTarget struct {
	IP       string `json:"ip"`
	Name     string `json:"name"`
	Type     string `json:"type"`
	LoggerSN uint32 `json:"logger_sn"`
}

type configFile struct {
	Targets []configTarget `json:"targets"`
}

// configPath — config.json в каталоге исполняемого файла.
func configPath() string {
	exe, err := os.Executable()
	if err != nil {
		return "config.json"
	}
	return filepath.Join(filepath.Dir(exe), "config.json")
}

// loadConfig читает и проверяет config.json, возвращает список целей.
func loadConfig(path string) ([]invTarget, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	var cf configFile
	if err := json.Unmarshal(b, &cf); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	if len(cf.Targets) == 0 {
		return nil, fmt.Errorf("config %s: пустой список targets", path)
	}
	targets := make([]invTarget, 0, len(cf.Targets))
	for _, t := range cf.Targets {
		var kind targetKind
		switch t.Type {
		case "deye":
			kind = kindDeyeString
		case "sofar":
			kind = kindSofar
		default:
			return nil, fmt.Errorf("config %s: неизвестный тип %q для %s", path, t.Type, t.IP)
		}
		if t.IP == "" {
			return nil, fmt.Errorf("config %s: пустой ip (type=%s)", path, t.Type)
		}
		if t.Name == "" {
			return nil, fmt.Errorf("config %s: пустое логическое имя name для %s", path, t.IP)
		}
		targets = append(targets, invTarget{IP: t.IP, Name: t.Name, LoggerSN: t.LoggerSN, Kind: kind})
	}
	return targets, nil
}

var targets []invTarget

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
	"pv1_voltage", "pv1_current",
	"pv2_voltage", "pv2_current",
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
}

// needsRounding — true, если тэг относится к величинам, которые округляются до
// 1 знака после запятой (напряжение, ток, мощность, энергия, температура).
func needsRounding(tag string) bool {
	return strings.HasSuffix(tag, "voltage") ||
		strings.HasSuffix(tag, "current") ||
		strings.HasSuffix(tag, "power") ||
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

// DeviceResult — результат опроса одного инвертора. Поля для JSON-файла
// отдельные (см. deviceSnapshot).
type DeviceResult struct {
	OK       bool
	HasData  bool
	DeviceSN string
	Values   valuesContract
}

// deviceSnapshot — структура, сохраняемая в JSON-файл инвертора.
type deviceSnapshot struct {
	Name     string         `json:"name"`
	IP       string         `json:"ip"`
	Timestamp string        `json:"timestamp"`
	DeviceSN string         `json:"device_sn,omitempty"`
	Values   valuesContract `json:"values"`
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
	putSofarU32(out, regs, 0x0015, 0x0016, "energy_total", 1)         // 32 бит, уже kWh
	putSofarU32(out, regs, 0x0017, 0x0018, "time_total", 1)            // 32 бит, h
	putSofarSimple(out, regs, 0x0019, "energy_today", 0.01)            // ×10 Wh → kWh
	putSofarSimple(out, regs, 0x001A, "time_today", 1)                 // min

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
	0xCB: {"grid_power", "grid_power", 1, "W", false, 0, true},
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
	return out
}

func pollDevice(t invTarget) DeviceResult {
	res := DeviceResult{OK: true}
	client := solarman.NewClient(t.IP+":"+port, t.LoggerSN, timeout)
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

	case kindSofar:
		pdus, fr, err := client.ReadRegisters(0x0000, 0x0028)
		if err != nil {
			res.OK = false
			return res
		}
		frames = fr
		// Sofar LSW-3 возвращает весь блок 0x0000-0x0027 (40 рег., bytecount 80)
		// И «дубль» — 16 рег. 0x0010-0x001F в отдельном кадре (bytecount 32).
		// Если слить все PDU от базы 0, дубль затирает 0x0000-0x000F (битый статус/
		// PV/частота). Берём только самую большую валидную PDU — полный блок от 0x0000.
		var best *solarman.ModbusPDU
		for i := range pdus {
			p := pdus[i]
			if p.CRC != p.CRCCalc {
				continue
			}
			if best == nil || len(p.Values) > len(best.Values) {
				best = &pdus[i]
			}
		}
		result := map[uint16]uint16{}
		if best != nil {
			for k := 0; k < len(best.Values); k++ {
				result[uint16(k)] = best.Values[k]
			}
		}
		if len(result) > 0 {
			res.HasData = true
			res.Values = mapSofarRegisters(result)
		}
	}

	for _, f := range frames {
		if f.DeviceSN != 0 {
			res.DeviceSN = fmt.Sprintf("%08x", f.DeviceSN)
			break
		}
	}

	return res
}

func main() {
	log.SetFlags(log.Ltime)
	cfgPath := configPath()
	if _, err := os.Stat(cfgPath); os.IsNotExist(err) {
		// `go run .`: бинарник во временном каталоге go-сборки — ищем config.json в CWD.
		cfgPath = "config.json"
	}
	var err error
	targets, err = loadConfig(cfgPath)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("poller started: config=%s targets=%v period=%s", cfgPath, targets, pollPeriod)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)

	ticker := time.NewTicker(pollPeriod)
	defer ticker.Stop()

	doPoll := func() {
		now := time.Now()
		results := make([]DeviceResult, len(targets))
		var wg sync.WaitGroup
		for i, t := range targets {
			wg.Add(1)
			go func(i int, t invTarget) {
				defer wg.Done()
				t0 := time.Now()
				results[i] = pollDevice(t)
				log.Printf("%s: %s (%s)", t.IP, describeResult(results[i]), time.Since(t0).Round(time.Millisecond))
			}(i, t)
		}
		wg.Wait()

		dayDir := filepath.Join(outDir, now.Format("2006-01-02"))
		if err := os.MkdirAll(dayDir, 0o755); err != nil {
			log.Printf("mkdir %s: %v", dayDir, err)
			return
		}
		ts := now.Format(time.RFC3339)
		for i, res := range results {
			if !res.OK || !res.HasData {
				// heartbeat_only / no data / ошибка — файл не сохраняем
				continue
			}
			snap := deviceSnapshot{
				Name:      targets[i].Name,
				IP:        targets[i].IP,
				Timestamp: ts,
				DeviceSN:  res.DeviceSN,
				Values:    res.Values,
			}
			b, err := json.MarshalIndent(snap, "", "  ")
			if err != nil {
				log.Printf("marshal %s: %v", targets[i].IP, err)
				continue
			}
			path := filepath.Join(dayDir, targets[i].IP+"-"+now.Format("150405")+".json")
			if err := os.WriteFile(path, b, 0o644); err != nil {
				log.Printf("write %s: %v", path, err)
				continue
			}
			log.Printf("saved %s", path)
		}
	}

	doPoll()
	for {
		select {
		case <-ticker.C:
			doPoll()
		case <-sig:
			log.Println("shutting down")
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

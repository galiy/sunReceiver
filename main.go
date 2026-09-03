package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
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

var targets = []string{"192.168.13.91", "192.168.13.70"}

type regDef struct {
	Name  string
	Ratio float64
	Unit  string
}

var regMap = map[uint16]regDef{
	0x0000: {"inverter_status", 1, ""},
	0x0001: {"fault_1", 1, ""},
	0x0002: {"fault_2", 1, ""},
	0x0003: {"fault_3", 1, ""},
	0x0004: {"fault_4", 1, ""},
	0x0005: {"fault_5", 1, ""},
	0x0006: {"pv1_voltage", 0.1, "V"},
	0x0007: {"pv1_current", 0.01, "A"},
	0x0008: {"pv2_voltage", 0.1, "V"},
	0x0009: {"pv2_current", 0.01, "A"},
	0x000A: {"pv1_power", 10, "W"},
	0x000B: {"pv2_power", 10, "W"},
	0x000C: {"output_active_power", 10, "W"},
	0x000D: {"output_reactive_power", 0.01, "kVar"},
	0x000E: {"grid_frequency", 0.01, "Hz"},
	0x000F: {"l1_voltage", 0.1, "V"},
	0x0010: {"l1_current", 0.01, "A"},
	0x0011: {"l2_voltage", 0.1, "V"},
	0x0012: {"l2_current", 0.01, "A"},
	0x0013: {"l3_voltage", 0.1, "V"},
	0x0014: {"l3_current", 0.01, "A"},
	0x0015: {"total_production_hi", 1, ""},
	0x0016: {"total_production_lo", 1, ""},
	0x0017: {"total_generation_time_hi", 1, ""},
	0x0018: {"total_generation_time_lo", 1, ""},
	0x0019: {"today_production", 10, "Wh"},
	0x001A: {"today_generation_time", 1, "min"},
	0x001B: {"module_temperature", 1, "C"},
	0x001C: {"inner_temperature", 1, "C"},
	0x001D: {"bus_voltage", 0.1, "V"},
	0x001E: {"pv1_sample_cpu_voltage", 0.1, "V"},
	0x001F: {"pv1_sample_cpu_current", 0.01, "A"},
	0x0020: {"countdown_time", 1, "s"},
	0x0021: {"alert", 1, ""},
	0x0022: {"input_mode", 1, ""},
	0x0023: {"comm_board_msg", 1, ""},
	0x0024: {"insulation_pv1_to_ground", 1, "Ohm"},
	0x0025: {"insulation_pv2_to_ground", 1, "Ohm"},
	0x0026: {"insulation_pv_minus_to_ground", 1, "Ohm"},
	0x0027: {"country", 1, ""},
}

var statusNames = map[uint16]string{
	0: "standby", 1: "self-checking", 2: "normal", 3: "fault", 4: "permanent",
}

var faultBits = []struct {
	Bit   uint16
	Name  string
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

type Heartbeat struct {
	DeliveryTime uint32 `json:"delivery_time"`
	PowerOnTime  uint32 `json:"power_on_time"`
	OffsetTime   uint32 `json:"offset_time"`
}

type DeviceResult struct {
	IP          string            `json:"ip"`
	OK          bool              `json:"ok"`
	Error       string            `json:"error,omitempty"`
	DeviceSN    string            `json:"device_sn,omitempty"`
	Frames      int               `json:"frames"`
	HasData     bool              `json:"register_data"`
	Heartbeat   *Heartbeat        `json:"heartbeat,omitempty"`
	Values      map[string]any    `json:"values,omitempty"`
	RawRegs     map[string]uint16 `json:"raw_registers,omitempty"`
}

type PollResult struct {
	Timestamp string         `json:"timestamp"`
	Devices   map[string]any `json:"devices"`
}

func int16val(v uint16) int {
	if v&0x8000 != 0 {
		return int(int32(v)) - 0x10000
	}
	return int(v)
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

// mapRegisters строит человекочитаемые значения из блока регистров 0x0000-0x0027.
func mapRegisters(regs []uint16) map[string]any {
	out := map[string]any{}
	get := func(addr uint16) *uint16 {
		if int(addr) < len(regs) {
			v := regs[addr]
			return &v
		}
		return nil
	}

	if v := get(0x0000); v != nil {
		s, ok := statusNames[*v]
		if !ok {
			s = fmt.Sprintf("unknown(%d)", *v)
		}
		out["inverter_status"] = s
	}
	for i, name := range []string{"fault_1", "fault_2", "fault_3", "fault_4", "fault_5"} {
		if v := get(uint16(0x0001 + i)); v != nil {
			out[name] = faultNames(*v)
		}
	}
	simple := func(addr uint16) {
		if v := get(addr); v != nil {
			def := regMap[addr]
			if def.Ratio == 1 {
				out[def.Name] = int16val(*v)
			} else {
				out[def.Name] = float64(int16val(*v)) * def.Ratio
			}
		}
	}
	for _, addr := range []uint16{0x0006, 0x0007, 0x0008, 0x0009, 0x000A, 0x000B, 0x000C, 0x000D,
		0x000E, 0x000F, 0x0010, 0x0011, 0x0012, 0x0013, 0x0014, 0x001B, 0x001C, 0x001D,
		0x001E, 0x001F, 0x0020, 0x0021, 0x0022, 0x0023, 0x0024, 0x0025, 0x0026} {
		simple(addr)
	}
	// 32-битные пары
	if hi := get(0x0015); hi != nil {
		if lo := get(0x0016); lo != nil {
			out["total_production_kwh"] = float64(*hi)*65536 + float64(*lo)
		}
	}
	if hi := get(0x0017); hi != nil {
		if lo := get(0x0018); lo != nil {
			out["total_generation_time_h"] = float64(*hi)*65536 + float64(*lo)
		}
	}
	if v := get(0x0019); v != nil {
		out["today_production_wh"] = float64(*v) * 10
	}
	if v := get(0x001A); v != nil {
		out["today_generation_time_min"] = int16val(*v)
	}
	if v := get(0x0027); v != nil {
		c, ok := countries[*v]
		if !ok {
			c = fmt.Sprintf("unknown(%d)", *v)
		}
		out["country"] = c
	}
	return out
}

func pollDevice(ip string) DeviceResult {
	res := DeviceResult{IP: ip, OK: true}
	client := solarman.NewClient(ip+":"+port, 0, timeout)
	pdus, frames, err := client.ReadRegisters(0x0000, 0x0028)
	if err != nil {
		res.OK = false
		res.Error = err.Error()
		return res
	}
	res.Frames = len(frames)

	// серийник логгера из ответа
	for _, f := range frames {
		if f.DeviceSN != 0 {
			res.DeviceSN = fmt.Sprintf("%08x", f.DeviceSN)
			break
		}
	}

	// heartbeat: заголовок первого кадра
	if len(frames) > 0 {
		if h, ok := solarman.ParseResponseHeader(frames[0].Payload); ok {
			res.Heartbeat = &Heartbeat{
				DeliveryTime: h.DeliveryTime,
				PowerOnTime:  h.PowerOnTime,
				OffsetTime:   h.OffsetTime,
			}
		}
	}

	// данные регистров: prefer PDU с валидным CRC и >= 40 регистрами;
	// иначе — самый большой PDU.
	var best *solarman.ModbusPDU
	for i := range pdus {
		p := &pdus[i]
		if p.ByteCount < 80 {
			continue
		}
		if p.CRC == p.CRCCalc && best == nil {
			best = p
			break
		}
		if best == nil || p.ByteCount > best.ByteCount {
			best = p
		}
	}
	if best != nil {
		res.HasData = best.CRC == best.CRCCalc
		res.Values = mapRegisters(best.Values[:40])
		res.RawRegs = make(map[string]uint16, len(best.Values))
		for k, v := range best.Values {
			res.RawRegs[fmt.Sprintf("0x%04X", k)] = v
		}
	}
	return res
}

func main() {
	log.SetFlags(log.Ltime)
	log.Printf("poller started: targets=%v period=%s", targets, pollPeriod)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)

	ticker := time.NewTicker(pollPeriod)
	defer ticker.Stop()

	doPoll := func() {
		now := time.Now()
		result := PollResult{
			Timestamp: now.Format(time.RFC3339),
			Devices:   map[string]any{},
		}

		results := make([]DeviceResult, len(targets))
		var wg sync.WaitGroup
		for i, ip := range targets {
			wg.Add(1)
			go func(i int, ip string) {
				defer wg.Done()
				t0 := time.Now()
				results[i] = pollDevice(ip)
				log.Printf("%s: %s (%s)", ip, describeResult(results[i]), time.Since(t0).Round(time.Millisecond))
			}(i, ip)
		}
		wg.Wait()

		for _, res := range results {
			result.Devices[res.IP] = res
		}

		dayDir := filepath.Join(outDir, now.Format("2006-01-02"))
		if err := os.MkdirAll(dayDir, 0o755); err != nil {
			log.Printf("mkdir %s: %v", dayDir, err)
			return
		}
		path := filepath.Join(dayDir, now.Format("150405")+".json")
		b, err := json.MarshalIndent(result, "", "  ")
		if err != nil {
			log.Printf("marshal: %v", err)
			return
		}
		if err := os.WriteFile(path, b, 0o644); err != nil {
			log.Printf("write %s: %v", path, err)
			return
		}
		log.Printf("saved %s", path)
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
		return "error: " + res.Error
	}
	if res.HasData {
		return fmt.Sprintf("frames=%d data=OK registers", res.Frames)
	}
	return fmt.Sprintf("frames=%d heartbeat_only", res.Frames)
}

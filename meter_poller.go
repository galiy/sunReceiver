package main

import (
	"fmt"
	"log"
	"time"
)

// Регистры электросчётчика DDS238 (per dds238read.py, read_holding_registers(0, 27)):
//
//	0-1   TotalEnergy  = (regs[0]*65536 + regs[1]) / 100        kWh (общая энергия)
//	8-9   ExportEnergy = (regs[8]*65536 + regs[9]) / 100        kWh (отдача в сеть)
//	10-11 ImportEnergy = (regs[10]*65536 + regs[11]) / 100      kWh (потребление из сети)
//	12    Voltage      = regs[12] / 10                           V
//	13    Current      = regs[13] / 100                          A
//	14    ActivePower  = int16(regs[14])                        W (знаковое; отриц. = отдача)
//	15    ReactivePower= int16(regs[15])                       var (знаковое)
//	16    PowerFactor  = regs[16] / 1000
//	17    Frequency    = regs[17] / 100                         Hz
const (
	meterRegTotalH = 0
	meterRegTotalL = 1
	meterRegExpH   = 8
	meterRegExpL   = 9
	meterRegImpH   = 10
	meterRegImpL   = 11
	meterRegVolt   = 12
	meterRegCurr   = 13
	meterRegActive = 14
	meterRegReact  = 15
	meterRegPF     = 16
	meterRegFreq   = 17
)

// meterTags — метки мгновенных значений счётчика (приставка meter_), попадающие
// в универсальный контракт values для дашборда (единица в суффиксе).
var meterTags = []string{
	"meter_voltage",       // V
	"meter_current",       // A
	"meter_active_power",  // W (знаковое: + потребление, − отдача)
	"meter_reactive_power",// var
	"meter_power_factor",
	"meter_frequency",     // Hz
	"meter_import",        // kWh (накопительный, потребление из сети)
	"meter_export",        // kWh (накопительный, отдача в сеть)
	"meter_total",         // kWh (накопительный, общая)
}

// meterReadings — необработанные показания счётчика за один опрос; используются
// захватом тарифных границ (meter_tariff.go), а не сохраняются в values.
type meterReadings struct {
	Import  float64 // kWh потребление из сети
	Export  float64 // kWh отдача в сеть
	Total   float64 // kWh общая
	Voltage float64 // V
	Current float64 // A
	ActiveP float64 // W, знаковое
	ReactP  float64 // var, знаковое
	PF      float64
	Freq    float64 // Hz
}

// decodeMeterРегs расшифровывает 27 регистров DDS238 в meterReadings.
func decodeMeterRegs(regs []uint16) meterReadings {
	u32 := func(hi, lo uint16) float64 {
		return (float64(regs[hi])*65536 + float64(regs[lo])) / 100
	}
	toSigned := func(i uint16) int { return int(int16(i)) }
	var r meterReadings
	if len(regs) > meterRegTotalL {
		r.Total = u32(meterRegTotalH, meterRegTotalL)
	}
	if len(regs) > meterRegExpL {
		r.Export = u32(meterRegExpH, meterRegExpL)
	}
	if len(regs) > meterRegImpL {
		r.Import = u32(meterRegImpH, meterRegImpL)
	}
	if len(regs) > meterRegVolt {
		r.Voltage = float64(regs[meterRegVolt]) / 10
	}
	if len(regs) > meterRegCurr {
		r.Current = float64(regs[meterRegCurr]) / 100
	}
	if len(regs) > meterRegActive {
		r.ActiveP = float64(toSigned(regs[meterRegActive]))
	}
	if len(regs) > meterRegReact {
		r.ReactP = float64(toSigned(regs[meterRegReact]))
	}
	if len(regs) > meterRegPF {
		r.PF = float64(regs[meterRegPF]) / 1000
	}
	if len(regs) > meterRegFreq {
		r.Freq = float64(regs[meterRegFreq]) / 100
	}
	return r
}

// mapMeterValues строит универсальный контракт meter_* из показаний счётчика.
func mapMeterValues(r meterReadings) valuesContract {
	out := valuesContract{}
	out["meter_voltage"] = r.Voltage
	out["meter_current"] = r.Current
	out["meter_active_power"] = r.ActiveP
	out["meter_reactive_power"] = r.ReactP
	out["meter_power_factor"] = r.PF
	out["meter_frequency"] = r.Freq
	out["meter_import"] = r.Import
	out["meter_export"] = r.Export
	out["meter_total"] = r.Total
	return out
}

// pollMeter читает регистры счётчика и возвращает мгновенные значения контракта
// и расшифрованные показания (для тарифного захвата). При ошибке — ok=false.
func pollMeter(c *meterClient, cfg *meterConfig) (valuesContract, meterReadings, bool) {
	regs, err := c.ReadHoldingRegisters(cfg.FirstReg, cfg.RegisterCnt)
	if err != nil {
		return nil, meterReadings{}, false
	}
	r := decodeMeterRegs(regs)
	return mapMeterValues(r), r, true
}

// runMeterPoll — фоновый 1-секундный цикл опроса электросчётчика DDS238:
//   - каждую секунду читает регистры и пишет снимок в Redis через
//     SaveSnapshotWindow (одна строка за 10 с + актуальное current) —
//     так же, как МАП/MPPT (см. runMapPoll). Из Redis усреднение в PG
//     (5-минутные агрегаты) делает общий аккумулятор (accumulator.go);
//   - на каждой границе тарифного дня (00:00, 07:00, 23:00) захватывает
//     показания Import/Export для посуточной статистики в PG (meter_tariff.go).
//
// Останавливается по закрытию канала stop.
func runMeterPoll(store *redisStore, pg *pgStore, cfg *meterConfig, stop <-chan struct{}) {
	if cfg == nil {
		return
	}
	client := newMeterClient(fmt.Sprintf("%s:%d", cfg.IP, cfg.Port), cfg.Unit)
	var capture *meterTariffCapture
	if pg != nil {
		capture = newMeterTariffCapture(pg)
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			now := time.Now()
			vals, readings, ok := pollMeter(client, cfg)
			if !ok {
				log.Printf("%s: meter опрос не удался", cfg.IP)
				continue
			}
			snap := deviceSnapshot{
				Name:      cfg.Name,
				IP:        cfg.IP,
				Timestamp: now.Format(time.RFC3339),
				Values:    vals,
			}
			if err := store.SaveSnapshotWindow(snap, now); err != nil {
				log.Printf("redis meter save %s: %v", cfg.IP, err)
			}
			if capture != nil {
				capture.capture(readings, now)
			}
		case <-stop:
			return
		}
	}
}
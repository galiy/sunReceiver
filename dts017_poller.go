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
	"fmt"
	"log"
	"time"
)

// pollDts017 читает телеметрию (0x0000, 29 рег.) и блок энергий (0x001D, 52 рег.)
// счётчика DTS017M и возвращает снимок values и накопительные показания Import/Export.
// При ошибке — ok=false.
func pollDts017(ctx context.Context, c *meterClient) (map[string]float64, dts017Readings, bool) {
	tele, err := c.ReadHoldingRegisters(ctx, dts017RegTelemetry, dts017TelemetryCount)
	if err != nil {
		return nil, dts017Readings{}, false
	}
	energy, err := c.ReadHoldingRegisters(ctx, dts017RegEnergy, dts017EnergyCount)
	if err != nil {
		return nil, dts017Readings{}, false
	}
	vals, r := decodeDts017(tele, energy)
	return vals, r, true
}

// runDts017Poll — цикл опроса счётчика DTS017M с периодом cfg.PollInterval:
//   - пишет снимок в СОБСТВЕННЫЕ ключи Redis (current + месячный ряд, каждое
//     показание), окно удержания 2 календарных суток;
//   - на границах тарифных зон (00:00, 07:00, 23:00) захватывает Import/Export
//     для посуточной статистики в собственной таблице PG (dts017_tariff.go).
//
// Историю счётчика (0x0300+) не читаем — статистика считается самостоятельно.
// Останавливается по закрытию ctx.
func runDts017Poll(store *redisStore, pg *pgStore, cfg *dts017Config, ctx context.Context) {
	if cfg == nil {
		return
	}
	client := newMeterClient(fmt.Sprintf("%s:%d", cfg.IP, cfg.Port), cfg.Unit, cfg.RTU)
	// DTS017M документирован под функцию чтения 0x04 (input registers).
	client.Func = 0x04
	var capture *dts017TariffCapture
	if pg != nil {
		capture = newDts017TariffCapture(pg)
	}
	ticker := time.NewTicker(cfg.PollInterval)
	defer ticker.Stop()
	const failLogInterval = 10 * time.Minute
	var lastFailLog time.Time
	var wasFailing bool
	// lastTimeCheck — время последней проверки часов счётчика (в памяти; при старте
	// нулевое => стартовая проверка на первом же тике). Раз в dts017TimeCheck
	// сверяем время счётчика с локальным и при расхождении > dts017TimeTol пишем.
	var lastTimeCheck time.Time
	for {
		select {
		case <-ticker.C:
			now := time.Now()
			if lastTimeCheck.IsZero() || now.Sub(lastTimeCheck) >= dts017TimeCheck {
				lastTimeCheck = now
				mt, corrected, terr := checkAndSyncDts017Time(ctx, client, now)
				switch {
				case terr != nil:
					log.Printf("dts017m %s: синхронизация времени: %v", cfg.IP, terr)
				case corrected:
					log.Printf("dts017m %s: время счётчика скорректировано (было %s)",
						cfg.IP, mt.Format("2006-01-02 15:04:05"))
				default:
					log.Printf("dts017m %s: время счётчика в норме (%s)",
						cfg.IP, mt.Format("2006-01-02 15:04:05"))
				}
			}
			vals, readings, ok := pollDts017(ctx, client)
			if !ok {
				if !wasFailing || now.Sub(lastFailLog) >= failLogInterval {
					log.Printf("dts017m %s: опрос не удался", cfg.IP)
					lastFailLog = now
				}
				wasFailing = true
				continue
			}
			if wasFailing {
				log.Printf("dts017m %s: опрос восстановлен", cfg.IP)
				wasFailing = false
			}
			snap := dts017Snapshot{
				Name:      cfg.Name,
				Timestamp: now.Format(time.RFC3339),
				Values:    vals,
			}
			if err := store.SaveDts017Current(snap); err != nil {
				log.Printf("redis dts017m current %s: %v", cfg.IP, err)
			}
			if err := store.SaveDts017Series(snap, now); err != nil {
				log.Printf("redis dts017m series %s: %v", cfg.IP, err)
			}
			if capture != nil {
				capture.capture(readings, now)
			}
		case <-ctx.Done():
			return
		}
	}
}

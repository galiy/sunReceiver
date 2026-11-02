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

// dts017RetryDelay — пауза перед повтором неудачного обмена. На границе минуты
// счётчик может «замолчать» на несколько секунд; один быстрый повтор часто
// успевает после паузы и не должен превращаться в запись «опрос не удался».
const dts017RetryDelay = 500 * time.Millisecond

// dts017FailLogAfter — сколько подряд неудачных опросов (с учётом повтора) должно
// пройти, прежде чем писать «опрос не удался». Одиночные минутные паузы не шумят.
const dts017FailLogAfter = 3

// dts017FailLogInterval — как часто повторять лог, если отказ длится дальше.
const dts017FailLogInterval = 10 * time.Minute

// readDts017Block читает один блок с одним повтором при таймауте. Соединение
// при таймауте без полученных байтов не закрывается (KeepConnOnTimeout): повтор
// идёт по тому же TCP. Если счётчик успел ответить на первый запрос, ответ будет
// вычитан как ответ на повтор — это тот же блок и те же регистры.
func readDts017Block(ctx context.Context, c *meterClient, start, count uint16) ([]uint16, error) {
	regs, err := c.ReadHoldingRegisters(ctx, start, count)
	if err == nil || !isMeterTimeout(err) {
		return regs, err
	}
	timer := time.NewTimer(dts017RetryDelay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer.C:
	}
	return c.ReadHoldingRegisters(ctx, start, count)
}

// pollDts017 — один цикл: телеметрия (0x0000, 29 рег.) + энергии (0x001D, 52 рег.),
// каждый блок с повтором при таймауте.
func pollDts017(ctx context.Context, c *meterClient) (map[string]float64, dts017Readings, error) {
	tele, err := readDts017Block(ctx, c, dts017RegTelemetry, dts017TelemetryCount)
	if err != nil {
		return nil, dts017Readings{}, err
	}
	energy, err := readDts017Block(ctx, c, dts017RegEnergy, dts017EnergyCount)
	if err != nil {
		return nil, dts017Readings{}, err
	}
	vals, r := decodeDts017(tele, energy)
	return vals, r, nil
}

// runDts017Poll — цикл опроса счётчика DTS017M с периодом cfg.PollInterval:
//   - пишет снимок в СОБСТВЕННЫЕ ключи Redis (current + ряд RedisTimeSeries, каждое
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
	// На границе минуты счётчик ~4–5 с не отвечает; запрос в это окно теряется.
	// Ждём ответ 1 с, не рвём TCP и повторяем тот же блок через 500 мс.
	client.IOTimeout = time.Second
	client.KeepConnOnTimeout = true
	var capture *dts017TariffCapture
	if pg != nil {
		capture = newDts017TariffCapture(pg)
	}
	ticker := time.NewTicker(cfg.PollInterval)
	defer ticker.Stop()
	failStreak := 0
	var lastFailLog time.Time
	// lastTimeCheck — время последней проверки часов счётчика (в памяти; при старте
	// нулевое => стартовая проверка на первом же тике). Раз в dts017TimeCheck
	// сверяем время счётчика с локальным и при расхождении > dts017TimeTol пишем.
	var lastTimeCheck time.Time
	for {
		if ctx.Err() != nil {
			return
		}
		select {
		case <-ticker.C:
			now := time.Now()
			if lastTimeCheck.IsZero() || now.Sub(lastTimeCheck) >= dts017TimeCheck {
				lastTimeCheck = now
				mt, corrected, terr := checkAndSyncDts017Time(ctx, client)
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
			vals, readings, perr := pollDts017(ctx, client)
			if ctx.Err() != nil {
				return
			}
			if perr != nil {
				failStreak++
				logNow := time.Now()
				if failStreak >= dts017FailLogAfter &&
					(failStreak == dts017FailLogAfter || logNow.Sub(lastFailLog) >= dts017FailLogInterval) {
					log.Printf("dts017m %s: опрос не удался (%d подряд): %v", cfg.IP, failStreak, perr)
					lastFailLog = logNow
				}
				continue
			}
			if failStreak > 0 {
				if failStreak >= dts017FailLogAfter {
					log.Printf("dts017m %s: опрос восстановлен (после %d неудачных)", cfg.IP, failStreak)
				}
				failStreak = 0
			}
			// Время успешного снятия, а не начала тика: повтор мог занять секунды.
			now = time.Now()
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

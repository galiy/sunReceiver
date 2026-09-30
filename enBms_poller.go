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
	"time"
)

// Параметры опроса EnBMS.
const (
	// enbmsPollInterval — общий период опроса всех устройств: цикл выполняется
	// не чаще 1 раза в секунду. Устройства опрашиваются последовательно одно за
	// другим; если суммарное чтение заняло больше секунды — паузы нет.
	enbmsPollInterval = time.Second
	// enbmsReconnectDelay / enbmsReconnectMaxDelay — базовый и максимальный
	// интервалы переподключения устройства (ограниченный бэкофф).
	enbmsReconnectDelay    = 2 * time.Second
	enbmsReconnectMaxDelay = 30 * time.Second
	// enbmsReadRetries — повторов чтения при таймауте (итого +1 попыток).
	enbmsReadRetries = 2
	// enbmsConsecutiveFailLimit — число подряд неудачных опросов, после которых
	// соединение разрывается и переустанавливается.
	enbmsConsecutiveFailLimit = 3
	// enbmsConnFailLogInterval — троттлинг логов о неудачных подключениях.
	enbmsConnFailLogInterval = 10 * time.Minute
)

// enbmsPollerDev — состояние опроса одного устройства EnBMS. Соединение
// постоянное (conn != nil между опросами); при обрыве conn сбрасывается и
// переустанавливается по nextRetry с ограниченным бэкоффом.
type enbmsPollerDev struct {
	cfg         enBmsDeviceConfig
	conn        *enbmsConn
	reconnect   time.Duration
	nextRetry   time.Time
	lastFailLog time.Time
	lastErrLog  time.Time
	consecFails int
}

// closeConn закрывает текущее соединение (если есть).
func (d *enbmsPollerDev) closeConn() {
	if d.conn == nil {
		return
	}
	if err := d.conn.Close(); err != nil {
		logEnBms("закрытие соединения с %s: %v", d.cfg.MAC, err)
	}
	d.conn = nil
}

// runEnBmsPoll — отдельный цикл опроса BMS EnBMS по BLE. Устройства
// опрашиваются последовательно один за другим; общий цикл — не чаще 1 раза в
// секунду; соединение с каждым устройством не рвётся между опросами (см.
// enbmsPollerDev). Каждое успешное чтение пишет текущий снимок в Redis (HASH
// current) и СЫРОЕ показание в Redis-ряд; in-memory аккумулятор считает
// 5-минутные усреднённые точки для PG (enbms_averages) — гранулярность PG =
// 1 запись / 5 минут.
func runEnBmsPoll(store *redisStore, pg *pgStore, cfg *enBmsConfig, ctx context.Context) {
	if cfg == nil || len(cfg.Devices) == 0 {
		return
	}
	acc := newEnBmsAccumulator()
	devs := make([]*enbmsPollerDev, 0, len(cfg.Devices))
	for _, dc := range cfg.Devices {
		devs = append(devs, &enbmsPollerDev{cfg: dc, reconnect: enbmsReconnectDelay})
	}
	logEnBms("avg: накопление 5-минутных усреднённых точек (в памяти процесса; PG %v)", pg != nil)

	shutdown := func() {
		// Неполный 5-минутный промежуток в PG не пишем (только полные бакеты); в
		// Redis он не нужен — каждое показание уже записано сырым (saveEnBmsReading).
		for _, d := range devs {
			d.closeConn()
		}
	}

	for {
		cycleStart := time.Now()
		for _, d := range devs {
			if ctx.Err() != nil {
				break
			}
			pollEnBmsDevice(store, acc, d, ctx)
		}
		saveEnBmsClosedBuckets(pg, acc.closed(time.Now()))
		if ctx.Err() != nil {
			shutdown()
			return
		}
		if wait := enbmsPollInterval - time.Since(cycleStart); wait > 0 {
			if !waitCtx(ctx, wait) {
				shutdown()
				return
			}
		}
	}
}

// pollEnBmsDevice выполняет один опрос одного устройства: при необходимости
// подключается (неблокирующе, по nextRetry), читает Battery с ретраями таймаута,
// сохраняет снимок и кормит аккумулятор.
func pollEnBmsDevice(store *redisStore, acc *enbmsAccumulator, d *enbmsPollerDev, ctx context.Context) {
	if d.conn == nil {
		if time.Now().Before(d.nextRetry) {
			return
		}
		// Сбрасываем кэш id адаптера: внешняя переинициализация USB-контроллера
		// могла сменить его номер (hci0→hci1) — без сброса переподключение не
		// прошло бы без рестарта сервиса (см. ce308AdapterIDReset).
		ce308AdapterIDReset()
		c, err := openEnBms(d.cfg.MAC, ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if time.Since(d.lastFailLog) >= enbmsConnFailLogInterval {
				logEnBms("подключение к %s (%s) не удалось: %v", d.cfg.Name, d.cfg.MAC, err)
				d.lastFailLog = time.Now()
			}
			d.nextRetry = time.Now().Add(d.reconnect)
			d.reconnect = enbmsBackoff(d.reconnect)
			return
		}
		d.conn = c
		d.reconnect = enbmsReconnectDelay
		d.consecFails = 0
		d.lastErrLog = time.Time{}
		logEnBms("подключено к %s (%s)", d.cfg.Name, d.cfg.MAC)
	}

	payload, err := d.readWithRetries()
	if err != nil {
		if isEnBmsClosed(err) {
			d.closeConn()
			return
		}
		d.consecFails++
		if d.consecFails >= enbmsConsecutiveFailLimit {
			logEnBms("опрос %s: %d опросов подряд не удались (последняя: %v) — переподключение",
				d.cfg.MAC, d.consecFails, err)
			d.closeConn()
			d.nextRetry = time.Now().Add(d.reconnect)
			d.reconnect = enbmsBackoff(d.reconnect)
		} else {
			// Троттлинг: при деградации канала не писать об ошибке каждый цикл.
			if time.Since(d.lastErrLog) >= enbmsConnFailLogInterval {
				logEnBms("опрос %s: не удался (%d подряд): %v — соединение сохраняю",
					d.cfg.MAC, d.consecFails, err)
				d.lastErrLog = time.Now()
			}
		}
		return
	}

	parsed, err := parseEnBmsBattery(payload)
	if err != nil {
		logEnBms("опрос %s: разбор Battery: %v", d.cfg.MAC, err)
		return
	}
	if !enbmsParsedValid(parsed) {
		// Нестабильный BLE-канал: кадр искажён — снимок отбрасываем, соединение
		// не рвём.
		logEnBms("опрос %s: невалидные показания — снимок отброшен", d.cfg.MAC)
		return
	}
	if d.consecFails > 0 {
		logEnBms("опрос %s: связь восстановлена (%d неудачных сброшены)", d.cfg.MAC, d.consecFails)
		d.consecFails = 0
		d.lastErrLog = time.Time{}
	}

	now := time.Now()
	snap := enbmsSnapshotFromParsed(d.cfg, parsed, now)
	// Текущее состояние — в HASH (перезапись); каждое снятое показание — в
	// Redis-ряд (сырое, samples=1). 5-минутные средние для PG накапливает
	// аккумулятор (как ANT BMS).
	if err := store.SaveEnBmsCurrent(snap); err != nil {
		logEnBms("redis current %s: %v", d.cfg.MAC, err)
	}
	saveEnBmsReading(store, snap, now)
	acc.add(snap, now)
}

// saveEnBmsReading пишет МГНОВЕННОЕ (одно снятое) показание EnBMS в Redis-ряд
// (month ZSET, score = секунда, samples=1). 5-минутные средние для PG считает
// enBms_accumulator.go; в Redis-ряду средних нет — только сырые показания.
func saveEnBmsReading(store *redisStore, s enbmsSnapshot, ts time.Time) {
	sp := enbmsSeriesPoint{Name: s.MAC, Display: s.Name, Ts: ts.Format(time.RFC3339)}
	sp.enbmsAveraged = enbmsAveraged{
		CurrentA:      s.CurrentA,
		PowerW:        s.PowerW,
		Soc:           s.Soc,
		CapacityAh:    s.CapacityAh,
		RemainingAh:   s.RemainingAh,
		TotalVoltageV: s.TotalVoltageV,
		PortVoltageV:  s.PortVoltageV,
		Soh:           s.Soh,
		MaxCellV:      s.MaxCellV,
		MinCellV:      s.MinCellV,
		AvgCellV:      s.AvgCellV,
		CellsV:        s.CellsV,
		Temperatures:  s.TemperaturesC,
		CellCount:     s.CellCount,
		Cycles:        s.Cycles,
		Samples:       1,
	}
	if err := store.SaveEnBmsSeries(sp, ts); err != nil {
		logEnBms("redis series %s: %v", s.MAC, err)
	}
}

// readWithRetries читает Battery, повторяя только таймаут ответа (транзиентный
// сбой радио). Ошибка записи/транспорта возвращается сразу — пулер решит о
// переподключении.
func (d *enbmsPollerDev) readWithRetries() ([]byte, error) {
	var lastErr error
	for attempt := 0; attempt <= enbmsReadRetries; attempt++ {
		p, err := d.conn.readEnBmsBattery()
		if err == nil {
			return p, nil
		}
		lastErr = err
		if isEnBmsReadTimeout(err) && attempt < enbmsReadRetries {
			logEnBms("чтение %s: таймаут (попытка %d/%d) — повтор",
				d.cfg.MAC, attempt+1, enbmsReadRetries+1)
			continue
		}
		return nil, err
	}
	return nil, lastErr
}

// enbmsBackoff — следующая ступень ограниченного бэкоффа переподключения.
func enbmsBackoff(d time.Duration) time.Duration {
	d *= 2
	if d > enbmsReconnectMaxDelay {
		return enbmsReconnectMaxDelay
	}
	return d
}

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
	"encoding/hex"
	"sync"
	"time"
)

// Параметры опроса EnBMS.
const (
	// Период общего цикла опроса задаётся в конфиге ОТДЕЛЬНО ДЛЯ КАЖДОГО метода
	// (enBms.poll_interval_ble / enBms.poll_interval_rs485, секунды) и хранится в
	// enBmsConfig.PollInterval. Устройства опрашиваются последовательно одно за
	// другим; если суммарное чтение заняло больше периода — паузы нет.
	//
	// enbmsReconnectDelay / enbmsReconnectMaxDelay — базовый и максимальный
	// интервалы переподключения BLE-устройства (ограниченный бэкофф).
	enbmsReconnectDelay    = 2 * time.Second
	enbmsReconnectMaxDelay = 30 * time.Second
	// enbmsRS485ReconnectDelay — пауза перед повтором подключения по RS485, если
	// попытка (до 5 с, enBmsRS485DialTimeout) не удалась: ждём 15 с и пробуем снова
	// (постоянное TCP-соединение со шлюзом переустанавливается). Фиксированная,
	// без экспоненциального роста.
	enbmsRS485ReconnectDelay = 15 * time.Second
	// enbmsReadRetries — повторов чтения при таймауте (итого +1 попыток).
	enbmsReadRetries = 2
	// enbmsConsecutiveFailLimit — число подряд неудачных опросов, после которых
	// соединение разрывается и переустанавливается.
	enbmsConsecutiveFailLimit = 3
	// enbmsConnFailLogInterval — троттлинг логов о неудачных подключениях.
	enbmsConnFailLogInterval = 10 * time.Minute
	// enbmsRawLogInterval — минимальный интервал фонового (heartbeat) лога сырого
	// payload Battery: пишем при каждом изменении хвоста (сигналы/аварии) и не
	// реже одного раза в этот интервал. Нужен был для калибровки раскладки хвоста.
	enbmsRawLogInterval = 60 * time.Second
	// enbmsRawTailLen — длина сигнального хвоста Battery (см. parseEnBmsTail).
	enbmsRawTailLen = 39
	// enbmsRawLog — сырой лог payload Battery/TeleMeter (`enbms: RAW ...`).
	// ОТКЛЮЧЁН: раскладка хвоста откалибрована по снятым логам (2026-10-01…05:
	// повторяющееся предупреждение ячейки 9, реже 16). Для повторной калибровки
	// поставить true.
	enbmsRawLog = false
	// enbmsAlarmHistory — писать ли алармы EnBMS в device_errors. Включено: warn-область
	// хвоста Battery откалибрована по APK и подтверждена живыми событиями (ячейки 9/16),
	// ложных срабатываний на здоровом кадре нет. Опрос EnBMS при этом выключен
	// пользователем (см. AGENTS.md) — история сработает только если/когда его включат.
	enbmsAlarmHistory = true
)

// enbmsPollerDev — состояние опроса одного устройства EnBMS. Соединение
// постоянное (conn != nil между опросами); при обрыве conn сбрасывается и
// переустанавливается по nextRetry с ограниченным бэкоффом. method/rs485 задают
// активный транспорт (BLE или RS485).
type enbmsPollerDev struct {
	cfg         enBmsDeviceConfig
	method      enBmsMethod
	rs485       *enBmsRS485Config
	conn        enbmsLink
	reconnect   time.Duration
	nextRetry   time.Time
	lastFailLog time.Time
	lastErrLog  time.Time
	consecFails int
	model       string          // модель/протокол из BasicInfo (читается один раз)
	lastAlarms  map[string]bool // активные алармы (для истории появления)
	commErr     bool            // была ли зафиксирована ошибка связи
	lastTail    string          // hex сигнального хвоста прошлого кадра (для лога изменений)
	lastRawLog  time.Time       // время последнего сырого лога (heartbeat)
}

// logEnBmsRaw пишет СЫРОЙ payload Battery в журнал с таймстампом снятия:
// при изменении сигнального хвоста (появились/сменились сигналы/аварии) либо не
// реже enbmsRawLogInterval. Нужен для последующей калибровки раскладки хвоста.
func (d *enbmsPollerDev) logEnBmsRaw(payload []byte, ts time.Time) {
	// Сигнальный хвост (39 Б) есть только у BLE Battery (0x61); у RS485 TeleMeter
	// (0x42) после напряжения клемм идут другие поля, поэтому tail не выделяем.
	var tailHex string
	if d.method == enBmsMethodBLE && len(payload) >= enbmsRawTailLen {
		tailHex = hex.EncodeToString(payload[len(payload)-enbmsRawTailLen:])
	}
	if tailHex == d.lastTail && time.Since(d.lastRawLog) < enbmsRawLogInterval {
		return
	}
	d.lastTail = tailHex
	d.lastRawLog = ts
	logEnBms("RAW %s (%s) ts=%s tail=%s payload=%s",
		d.cfg.Name, d.cfg.Key, ts.Format(time.RFC3339), tailHex, hex.EncodeToString(payload))
}

// closeConn закрывает текущее соединение (если есть).
func (d *enbmsPollerDev) closeConn() {
	if d.conn == nil {
		return
	}
	if err := d.conn.Close(); err != nil {
		logEnBms("закрытие соединения с %s: %v", d.cfg.Key, err)
	}
	d.conn = nil
}

// runEnBmsPoll опрашивает все активные устройства EnBMS. Так как метод задаётся
// у КАЖДОГО устройства, устройства группируются по методу, и каждая группа
// опрашивается СВОИМ независимым циклом со своим периодом
// (poll_interval_ble / poll_interval_rs485) — в одной коллекции одновременно
// могут работать и BLE-, и RS485-устройства. Внутри группы устройства
// опрашиваются последовательно; соединение с каждым постоянное (см.
// enbmsPollerDev). Каждое успешное чтение пишет снимок в Redis (current и ряд);
// in-memory аккумулятор группы считает 5-минутные средние для PG.
func runEnBmsPoll(store *redisStore, pg *pgStore, cfg *enBmsConfig, ctx context.Context) {
	if cfg == nil || len(cfg.Devices) == 0 {
		return
	}
	groups := map[enBmsMethod][]*enbmsPollerDev{}
	for _, dc := range cfg.Devices {
		reconnect := enbmsReconnectDelay
		if dc.Method == enBmsMethodRS485 {
			reconnect = enbmsRS485ReconnectDelay
		}
		dev := &enbmsPollerDev{cfg: dc, method: dc.Method, rs485: dc.RS485, reconnect: reconnect}
		groups[dc.Method] = append(groups[dc.Method], dev)
	}
	var wg sync.WaitGroup
	for method, devs := range groups {
		interval := cfg.PollBLE
		if method == enBmsMethodRS485 {
			interval = cfg.PollRS485
		}
		wg.Add(1)
		go func(method enBmsMethod, devs []*enbmsPollerDev, interval time.Duration) {
			defer wg.Done()
			runEnBmsGroup(store, pg, method, devs, interval, ctx)
		}(method, devs, interval)
	}
	wg.Wait()
}

// runEnBmsGroup — цикл опроса одной группы устройств (одного метода) с периодом
// interval. Свой in-memory аккумулятор 5-минутных средних для PG (аккумулятор не
// потокобезопасен, поэтому у каждой группы он свой).
func runEnBmsGroup(store *redisStore, pg *pgStore, method enBmsMethod, devs []*enbmsPollerDev, interval time.Duration, ctx context.Context) {
	acc := newEnBmsAccumulator()
	logEnBms("метод=%s период=%s устройств=%d — опрос запущен (avg: PG %v)",
		method, interval, len(devs), pg != nil)

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
			pollEnBmsDevice(pg, store, acc, d, ctx)
		}
		saveEnBmsClosedBuckets(pg, acc.closed(time.Now()))
		if ctx.Err() != nil {
			shutdown()
			return
		}
		if wait := interval - time.Since(cycleStart); wait > 0 {
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
func pollEnBmsDevice(pg *pgStore, store *redisStore, acc *enbmsAccumulator, d *enbmsPollerDev, ctx context.Context) {
	if d.conn == nil {
		if time.Now().Before(d.nextRetry) {
			return
		}
		var (
			c   enbmsLink
			err error
		)
		if d.method == enBmsMethodRS485 {
			c, err = openEnBmsRS485(d.rs485, ctx)
		} else {
			// Сбрасываем кэш id адаптера: внешняя переинициализация USB-контроллера
			// могла сменить его номер (hci0→hci1) — без сброса переподключение не
			// прошло бы без рестарта сервиса (см. ce308AdapterIDReset).
			ce308AdapterIDReset()
			c, err = openEnBms(d.cfg.MAC, ctx)
		}
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if time.Since(d.lastFailLog) >= enbmsConnFailLogInterval {
				logEnBms("подключение к %s (%s) не удалось: %v", d.cfg.Name, d.cfg.Key, err)
				d.lastFailLog = time.Now()
			}
			if pg != nil && !d.commErr {
				d.commErr = true
				if e := pg.InsertDeviceError(d.cfg.Key, "enbms", "comm", err.Error(), time.Now()); e != nil {
					logEnBms("error pg %s: %v", d.cfg.Key, e)
				}
			}
			// RS485: фиксированная пауза 15 с (попытка ограничена 5 с диалом);
			// BLE: экспоненциальный бэкофф 2…30 с.
			d.nextRetry = time.Now().Add(d.reconnect)
			if d.method != enBmsMethodRS485 {
				d.reconnect = enbmsBackoff(d.reconnect)
			}
			return
		}
		d.conn = c
		if d.method == enBmsMethodRS485 {
			d.reconnect = enbmsRS485ReconnectDelay
		} else {
			d.reconnect = enbmsReconnectDelay
		}
		d.consecFails = 0
		d.lastErrLog = time.Time{}
		d.commErr = false
		logEnBms("подключено к %s (%s) по %s", d.cfg.Name, d.cfg.Key, d.method)
		if d.model == "" {
			if bi, serr := c.readEnBmsBasicInfo(); serr == nil {
				if m := parseEnBmsModel(bi); m != "" {
					d.model = m
					logEnBms("модель %s (%s): %s", d.cfg.Name, d.cfg.Key, m)
				}
			}
		}
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
				d.cfg.Key, d.consecFails, err)
			d.closeConn()
			d.nextRetry = time.Now().Add(d.reconnect)
			if d.method != enBmsMethodRS485 {
				d.reconnect = enbmsBackoff(d.reconnect)
			}
		} else {
			// Троттлинг: при деградации канала не писать об ошибке каждый цикл.
			if time.Since(d.lastErrLog) >= enbmsConnFailLogInterval {
				logEnBms("опрос %s: не удался (%d подряд): %v — соединение сохраняю",
					d.cfg.Key, d.consecFails, err)
				d.lastErrLog = time.Now()
			}
		}
		return
	}

	// Разбор телеметрии. Хвост предупреждений есть только у BLE Battery (0x61);
	// у RS485 TeleMeter (0x42) после напряжения клемм идут другие поля.
	parsed, err := parseEnBmsBatteryPayload(payload, d.method == enBmsMethodBLE)
	if err != nil {
		d.logErrThrottled("опрос %s: разбор Battery: %v", d.cfg.Key, err)
		return
	}
	if !enbmsParsedValid(parsed) {
		// Нестабильный канал: кадр искажён — снимок отбрасываем, соединение
		// не рвём. Лог троттлим: при деградации канала ветка срабатывает каждый цикл.
		d.logErrThrottled("опрос %s: невалидные показания — снимок отброшен", d.cfg.Key)
		return
	}
	if d.consecFails > 0 {
		logEnBms("опрос %s: связь восстановлена (%d неудачных сброшены)", d.cfg.Key, d.consecFails)
		d.consecFails = 0
		d.lastErrLog = time.Time{}
	}

	now := time.Now()
	if enbmsRawLog {
		d.logEnBmsRaw(payload, now)
	}
	snap := enbmsSnapshotFromParsed(d.cfg, parsed, now)
	snap.Model = d.model
	// Текущее состояние — в HASH (перезапись); каждое снятое показание — в
	// Redis-ряд (сырое, samples=1). 5-минутные средние для PG накапливает
	// аккумулятор (как ANT BMS).
	if err := store.SaveEnBmsCurrent(snap); err != nil {
		logEnBms("redis current %s: %v", d.cfg.Key, err)
	}
	saveEnBmsReading(store, snap, now)
	acc.add(snap, now)
	// История ошибок: появление новых алармов. Пока enbmsAlarmHistory=false —
	// раскладка Ext_Bit не откалибрована (ложные срабатывания на здоровом кадре).
	if pg != nil && enbmsAlarmHistory {
		cur := map[string]bool{}
		for _, a := range snap.Alarms {
			cur[a] = true
		}
		for a := range cur {
			if d.lastAlarms == nil || !d.lastAlarms[a] {
				if e := pg.InsertDeviceError(d.cfg.Key, "enbms", a, a, now); e != nil {
					logEnBms("error pg %s: %v", d.cfg.Key, e)
				}
			}
		}
		d.lastAlarms = cur
	}
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

// logErrThrottled пишет диагностику деградации канала не чаще
// enbmsConnFailLogInterval (иначе разбор/валидация/таймауты засоряют журнал —
// при опросе раз в секунду каждая ветка срабатывала бы каждый цикл).
func (d *enbmsPollerDev) logErrThrottled(format string, args ...any) {
	if time.Since(d.lastErrLog) < enbmsConnFailLogInterval {
		return
	}
	d.lastErrLog = time.Now()
	logEnBms(format, args...)
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
			d.logErrThrottled("чтение %s: таймаут (попытка %d/%d) — повтор",
				d.cfg.Key, attempt+1, enbmsReadRetries+1)
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

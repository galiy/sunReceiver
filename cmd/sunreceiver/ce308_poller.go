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

// ce308EnergyInterval — период АВТОНОМНОГО снимка накопленной энергии: раз в
// 30 минут пулер сам (без внешнего HTTP-запроса) перечитывает END01..END04 и
// обновляет разовый снимок. Первый запуск ориентируется на время последнего
// снимка (см. ce308EnergyDelay).
const ce308EnergyInterval = 30 * time.Minute

// ce308FinalReadingsInterval — период снятия итоговых показаний.
const ce308FinalReadingsInterval = ce308EnergyInterval

// ce308ReconnectDelay — базовая пауза между попытками переподключения при обрыве связи.
const ce308ReconnectDelay = 2 * time.Second

// ce308ReconnectMaxDelay — верхняя граница ограниченного бэкоффа переподключения.
const ce308ReconnectMaxDelay = 30 * time.Second

// ce308ConnFailLogInterval — порог логирования неудачных подключений:
// первый сбой — сразу, далее не чаще раза в 10 минут.
const ce308ConnFailLogInterval = 10 * time.Minute

// ce308CommErrPersist — сколько «нет связи» должно держаться непрерывно (без
// успешного подключения), прежде чем попасть в историю ошибок (антишум).
const ce308CommErrPersist = time.Hour

// runCe308FinalReadings — единственный режим CE308: только итоговые показания.
//   - НЕ держит постоянное BLE-соединение (подключается только на время снятия);
//   - НЕ читает и НЕ пишет мгновенную телеметрию;
//   - раз в ce308FinalReadingsInterval (30 мин) подключается, однократно снимает
//     накопленную энергию END01..END04 (панель «показания») и отключается;
//   - дополнительно снимает снимок по сигналу (кнопка «Обновить», triggerCE308EnergySnapshot).
//
// При инициализации момент первого снятия ориентируется на время последнего
// снимка энергии в Redis (ce308EnergyDelay). Ошибка подключения/чтения не роняет
// поток: следующая попытка — через ограниченный бэкофф (база 2 с, до 30 с).
func runCe308FinalReadings(store *redisStore, pg *pgStore, cfg *ce308Config, ctx context.Context) {
	if cfg == nil {
		return
	}
	// Явный BLE-адаптер (ce308.hci): hciN или BD_ADDR. Пусто — авто-детект.
	ce308SetAdapterID(cfg.HCI)
	trig := make(chan struct{}, 1)
	setCE308TriggerChan(trig)
	defer setCE308TriggerChan(nil)

	logCE308("режим final: итоговые показания раз в %s (или по кнопке)", ce308FinalReadingsInterval)
	var lastFailLog time.Time
	reconnect := ce308ReconnectDelay
	// История ошибок: пишем «нет связи» только если держится ≥ ce308CommErrPersist.
	var commErrSince time.Time
	commErrLogged := false
	// next — момент следующего ПЛАНОВОГО снятия.
	next := time.Now().Add(ce308EnergyDelay(store))
	for {
		d := time.Until(next)
		if d < 0 {
			d = 0
		}
		timer := time.NewTimer(d)
		manual := false
		select {
		case <-timer.C:
		case <-trig:
			manual = true
			stopTimer(timer)
		case <-ctx.Done():
			stopTimer(timer)
			return
		}

		ce308AdapterIDReset()
		m, err := openCE308(cfg.MAC, cfg.PIN, ctx)
		if err != nil {
			if time.Since(lastFailLog) >= ce308ConnFailLogInterval {
				logCE308("подключение к %s не удалось: %v", cfg.MAC, err)
				lastFailLog = time.Now()
			}
			now := time.Now()
			if commErrSince.IsZero() {
				commErrSince = now
			}
			if pg != nil && !commErrLogged && now.Sub(commErrSince) >= ce308CommErrPersist {
				if e := pg.InsertDeviceError(cfg.Name, "ce308", "comm", err.Error(), now); e != nil {
					logCE308("error pg: %v", e)
				}
				commErrLogged = true
			}
			reconnect = ce308Backoff(reconnect)
			next = time.Now().Add(reconnect)
			continue
		}
		reconnect = ce308ReconnectDelay
		commErrSince = time.Time{}
		commErrLogged = false
		err = ce308CaptureEnergy(store, cfg, m)
		if cerr := m.Close(); cerr != nil {
			logCE308("закрытие соединения с %s: %v", cfg.MAC, cerr)
		}
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			if !isCE308Closed(err) {
				logCE308("снятие итоговых показаний не удалось: %v", err)
			}
			reconnect = ce308Backoff(reconnect)
			next = time.Now().Add(reconnect)
			continue
		}
		if manual {
			logCE308("итоговые показания сняты по запросу")
		}
		next = time.Now().Add(ce308FinalReadingsInterval)
	}
}

// ce308Backoff возвращает следующую ступень ограниченного бэкоффа переподключения.
func ce308Backoff(d time.Duration) time.Duration {
	d *= 2
	if d > ce308ReconnectMaxDelay {
		return ce308ReconnectMaxDelay
	}
	return d
}

// ce308EnergyDelay — задержка до первого автономного снимка энергии (при
// инициализации пулера). Ориентируется на время последнего снимка в Redis:
//   - снимка ещё нет или время некорректно — снимаем сразу (0);
//   - снимок свежее ce308EnergyInterval — ждём до (last + период);
//   - снимок устарел (старше периода) — снимаем сразу (0).
func ce308EnergyDelay(store *redisStore) time.Duration {
	last, err := store.CE308Energy()
	if err != nil || last == nil || last.Timestamp == "" {
		return 0
	}
	ts, err := time.Parse(time.RFC3339, last.Timestamp)
	if err != nil {
		return 0
	}
	if d := time.Until(ts.Add(ce308EnergyInterval)); d > 0 {
		return d
	}
	return 0
}

// ce308CaptureEnergy снимает накопленную энергию и сохраняет разовый снимок.
func ce308CaptureEnergy(store *redisStore, cfg *ce308Config, m *ce308Meter) error {
	logCE308("снимок энергии: чтение END01..END04")
	snap, err := readCE308Energy(m)
	if err != nil {
		return err
	}
	snap.Name = cfg.Name
	snap.Timestamp = ce308Timestamp(time.Now())
	if err := store.SaveCE308Energy(snap); err != nil {
		logCE308("redis energy: %v", err)
		return err
	}
	logCE308("снимок энергии сохранён: A+ %.3f/%.3f, A− %.3f/%.3f, R+ %.3f/%.3f, R− %.3f/%.3f кВт·ч/квар·ч (день/ночь)",
		snap.ActiveConsumptionDay, snap.ActiveConsumptionNight,
		snap.ActiveDeliveryDay, snap.ActiveDeliveryNight,
		snap.ReactiveConsumptionDay, snap.ReactiveConsumptionNight,
		snap.ReactiveDeliveryDay, snap.ReactiveDeliveryNight)
	return nil
}

// waitCtx ждёт d или отмены ctx. Возвращает false, если ctx отменён (пора выходить).
func waitCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

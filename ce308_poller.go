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
	"time"
)

// ce308PollInterval — целевой период опроса мгновенных значений CE308.
// Фактический период ограничен временем последовательного чтения команд BLE
// (VOLTA/CURRE/POWEP/POWEQ ≈ 4-5 с), поэтому цикл не успевает за 2 с — он
// просто не блокируется ожиданием следующего таймера (ticker буферизует один
// тик и сбрасывает лишние).
const ce308PollInterval = 2 * time.Second

// ce308EnergyInterval — период АВТОНОМНОГО снимка накопленной энергии: раз в
// 30 минут пулер сам (без внешнего HTTP-запроса) перечитывает END01..END04 и
// обновляет разовый снимок. Первый запуск при инициализации ориентируется на
// время последнего снимка (см. ce308EnergyDelay).
const ce308EnergyInterval = 30 * time.Minute

// ce308ReconnectDelay — базовая пауза между попытками переподключения при обрыве связи.
const ce308ReconnectDelay = 2 * time.Second

// ce308ReconnectMaxDelay — верхняя граница ограниченного бэкоффа переподключения
// (база удваивается после каждой неудачной попытки): не долбить радиоканал
// непрерывно при длительном отсутствии связи, но и не ждать слишком долго.
const ce308ReconnectMaxDelay = 30 * time.Second

// ce308ReadRetries — число ПОВТОРОВ после первой неудачной попытки чтения при
// таймауте (итого до ce308ReadRetries+1 попыток на одну команду). Транзиентный
// сбой радио лечится повтором того же чтения, не разрывая постоянное соединение.
const ce308ReadRetries = 2

// ce308ConsecutiveFailLimit — число подряд неудачных опросов, после которых пулер
// разрывает постоянное соединение и переподключается. Ниже порога одиночные сбои
// (в т.ч. после исчерпания ретраев) не рвут связь.
const ce308ConsecutiveFailLimit = 3

// ce308ConnFailLogInterval — порог логирования неудачных подключений:
// первый сбой — сразу, далее не чаще раза в 10 минут (при длительном отсутствии
// связи журнал не засоряется).
const ce308ConnFailLogInterval = 10 * time.Minute

// runCe308Poll — отдельный поток опроса счётчика Энергомера CE308 по BLE:
//   - держит BLE-соединение постоянно (не закрывает между опросами);
//   - каждые ce308PollInterval читает мгновенные значения (напряжения/токи/
//     активная и реактивная мощность по фазам) и пишет снимок в Redis:
//     текущее значение (SaveCE308Current, перезапись) + точка истории
//     (SaveCE308History ~1 за 2 с);
//   - усреднение истории до 1 записи за 10 с в PG делает runCe308Accumulator;
//   - при обрыве соединения сам его восстанавливает (переподключение с паузой);
//   - по сигналу (triggerCE308EnergySnapshot) в ближайшем цикле снимает
//     накопленную электроэнергию (Актив/Реактив × День/Ночь × Потребление/Отдача)
//     и сохраняет разовый снимок в отдельный ключ Redis (истории нет).
func runCe308Poll(store *redisStore, pg *pgStore, cfg *ce308Config, ctx context.Context) {
	if cfg == nil {
		return
	}
	// Канал запроса снимка энергии: привязываем к пулеру, чтобы внешний сигнал
	// (HTTP /api/ce308/energy) попадал в этот цикл.
	trig := make(chan struct{}, 1)
	setCE308TriggerChan(trig)
	defer setCE308TriggerChan(nil)

	var lastFailLog time.Time
	reconnect := ce308ReconnectDelay
	for {
		// Внешняя переинициализация контроллера (watchdog перезагружает драйвер
		// btusb, см. ce308_agent_linux.go) может сменить его номер/объект BlueZ —
		// сбрасываем кэш id, чтобы каждая попытка подключения заново обнаружила
		// реальный адаптер. Иначе ремонт BT без рестарта сервиса не привёл бы к
		// переподключению (пулер остался бы на мёртвом hci0).
		ce308AdapterIDReset()
		m, err := openCE308(cfg.MAC, cfg.PIN, ctx)
		if err != nil {
			if time.Since(lastFailLog) >= ce308ConnFailLogInterval {
				logCE308("подключение к %s не удалось: %v", cfg.MAC, err)
				lastFailLog = time.Now()
			}
			if !waitCtx(ctx, reconnect) {
				return
			}
			// Ограниченный бэкофф: при устойчивой проблеме с BLE не долбить повторно.
			reconnect = ce308Backoff(reconnect)
			continue
		}
		// Успешное подключение сбрасывает бэкофф к базовой паузе.
		reconnect = ce308ReconnectDelay
		logCE308("подключено к %s", cfg.MAC)
		err = ce308PollConnected(store, cfg, m, trig, ctx)
		// Закрытие соединения: фиксируем результат (неуспешный Disconnect при
		// остановке оставляет полу-открытую связь на адаптере — см. Close).
		if cerr := m.Close(); cerr != nil {
			logCE308("закрытие соединения с %s: %v", cfg.MAC, cerr)
		}
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			logCE308("соединение с %s потеряно: %v — переподключение", cfg.MAC, err)
		}
		if !waitCtx(ctx, reconnect) {
			return
		}
		reconnect = ce308Backoff(reconnect)
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

// ce308PollConnected — цикл опроса на установленном соединении. Возвращает
// ошибку при потере связи (тогда вызывающий переподключается).
func ce308PollConnected(store *redisStore, cfg *ce308Config, m *ce308Meter, trig chan struct{}, ctx context.Context) error {
	ticker := time.NewTicker(ce308PollInterval)
	defer ticker.Stop()
	// Автономный снимок энергии (без внешнего запроса): раз в ce308EnergyInterval.
	// Первый запуск при инициализации ориентируется на время последнего снимка в
	// Redis (см. ce308EnergyDelay): если он свежее периода — ждём до (last+период),
	// иначе снимаем сразу. После каждого автоперечитывания таймер взводится заново.
	energyT := time.NewTimer(ce308EnergyDelay(store))
	defer energyT.Stop()
	consecFails := 0
	for {
		select {
		case <-ticker.C:
			if err := ce308PollOnce(store, cfg, m); err != nil {
				if isCE308Closed(err) {
					// Остановка сервиса: чтение прервано — выходим и закрываем соединение.
					return err
				}
				consecFails++
				if consecFails >= ce308ConsecutiveFailLimit {
					// Счётчик подряд молчит / чтения не проходят — соединение считаем
					// нерабочим, разрываем и переподключаемся.
					logCE308("опрос %s: %d опросов подряд не удались (последняя: %v) — переподключение",
						cfg.MAC, consecFails, err)
					return err
				}
				// Одиночный сбой не рвёт постоянное соединение: он мог быть
				// транзиентным (радио/нет ответа от счётчика).
				logCE308("опрос %s: не удался (%d подряд): %v — соединение сохраняю",
					cfg.MAC, consecFails, err)
			} else {
				if consecFails > 0 {
					logCE308("опрос %s: связь восстановлена (%d неудачных сброшены)", cfg.MAC, consecFails)
				}
				consecFails = 0
			}
		case <-energyT.C:
			if err := ce308CaptureEnergy(store, cfg, m); err != nil {
				if isCE308Closed(err) {
					return err
				}
				// Ошибка чтения энергии не рвёт постоянное соединение — просто
				// логируем; следующий опрос мгновенных значений продолжится.
				logCE308("автоснимок энергии не удался: %v", err)
			}
			energyT.Reset(ce308EnergyInterval)
		case <-trig:
			if err := ce308CaptureEnergy(store, cfg, m); err != nil {
				if isCE308Closed(err) {
					return err
				}
				// Ошибка чтения энергии не рвёт постоянное соединение — просто
				// логируем; следующий опрос мгновенных значений продолжится.
				logCE308("снимок энергии не удался: %v", err)
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
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

// ce308PollOnce выполняет один опрос мгновенных значений и пишет снимок.
func ce308PollOnce(store *redisStore, cfg *ce308Config, m *ce308Meter) error {
	now := time.Now()
	reads, err := buildCE308Reads(m)
	if err != nil {
		return fmt.Errorf("опрос %s: %w", cfg.MAC, err)
	}
	if !ce308ReadsValid(reads) {
		// Нестабильный BLE-канал: кадр искажён (NaN/Inf или нереальные значения) —
		// снимок отбрасываем, но постоянное соединение не разрываем.
		logCE308("опрос %s: невалидные показания — снимок отброшен", cfg.MAC)
		return nil
	}
	snap := ce308Snapshot{
		Name:      cfg.Name,
		IP:        cfg.MAC,
		Timestamp: ce308Timestamp(now),
		Values:    valuesCE308(reads),
	}
	if err := store.SaveCE308Current(snap); err != nil {
		logCE308("redis current %s: %v", cfg.MAC, err)
	}
	if err := store.SaveCE308History(snap, now); err != nil {
		logCE308("redis history %s: %v", cfg.MAC, err)
	}
	return nil
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

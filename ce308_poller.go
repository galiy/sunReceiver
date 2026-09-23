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

// ce308ReconnectDelay — пауза между попытками переподключения при обрыве связи.
const ce308ReconnectDelay = 2 * time.Second

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
	for {
		m, err := openCE308(cfg.MAC, cfg.PIN)
		if err != nil {
			if time.Since(lastFailLog) >= ce308ConnFailLogInterval {
				logCE308("подключение к %s не удалось: %v", cfg.MAC, err)
				lastFailLog = time.Now()
			}
			if !waitCtx(ctx, ce308ReconnectDelay) {
				return
			}
			continue
		}
		logCE308("подключено к %s", cfg.MAC)
		err = ce308PollConnected(store, cfg, m, trig, ctx)
		m.Close()
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			logCE308("соединение с %s потеряно: %v — переподключение", cfg.MAC, err)
		}
		if !waitCtx(ctx, ce308ReconnectDelay) {
			return
		}
	}
}

// ce308PollConnected — цикл опроса на установленном соединении. Возвращает
// ошибку при потере связи (тогда вызывающий переподключается).
func ce308PollConnected(store *redisStore, cfg *ce308Config, m *ce308Meter, trig chan struct{}, ctx context.Context) error {
	ticker := time.NewTicker(ce308PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if err := ce308PollOnce(store, cfg, m); err != nil {
				return err
			}
		case <-trig:
			if err := ce308CaptureEnergy(store, cfg, m); err != nil {
				// Ошибка чтения энергии не рвёт постоянное соединение — просто
				// логируем; следующий опрос мгновенных значений продолжится.
				logCE308("снимок энергии не удался: %v", err)
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// ce308PollOnce выполняет один опрос мгновенных значений и пишет снимок.
func ce308PollOnce(store *redisStore, cfg *ce308Config, m *ce308Meter) error {
	now := time.Now()
	reads, err := buildCE308Reads(m)
	if err != nil {
		return fmt.Errorf("опрос %s: %w", cfg.MAC, err)
	}
	snap := deviceSnapshot{
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

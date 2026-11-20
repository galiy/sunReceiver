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
	"errors"
	"fmt"
	"log"
	"sync"
	"time"
)

// Поток AGM: независимые циклы PZEM-017 и 4×BK300. PZEM пишет свой current/ряд
// при определённом знаке по current МАП (±5 с); BK300 — только полный снимок
// 4/4. Потеря PZEM не останавливает BLE. Документация —
// docs/modules/agm-monitor.md.

// agmCycleTimeout — верхняя граница ожидания всех ответов одного цикла.
// Опросчики ограничены своими таймаутами; это страховка от зависания D-Bus.
const agmCycleTimeout = 15 * time.Second

// agmPZEMErrKey — стабильный ключ устройства PZEM-017 в журнале ошибок
// (kind=agm); имя сборки передаётся в поле name.
const agmPZEMErrKey = "agm-pzem"

// agmBeforeSearchDelay — пауза перед повторным поиском порта после потери PZEM.
const agmBeforeSearchDelay = 2 * time.Second

// runAGMPoll — цикл жизни модуля: поиск/закрепление PZEM-017, затем мониторинг
// до потери PZEM или остановки.
func runAGMPoll(store *redisStore, pg *pgStore, cfg *agmConfig, ctx context.Context) {
	if cfg == nil {
		return
	}
	d := &agmAGM{cfg: cfg, store: store, pg: pg, errs: newAGMErrTracker(pg, cfg.Name)}
	// BLE-ветка живёт независимо от PZEM: потеря/поиск порта PZEM не должен
	// останавливать опрос и рвать соединения BK300.
	var bleWG sync.WaitGroup
	bleWG.Add(1)
	go func() {
		defer bleWG.Done()
		d.runBLESupervisor(ctx)
	}()
	defer bleWG.Wait()
	for {
		if ctx.Err() != nil {
			return
		}
		pz, ok := findAGMPZEM(ctx, cfg, d.errs)
		if !ok {
			return
		}
		d.monitorPZEM(ctx, pz)
		if ctx.Err() != nil {
			return
		}
		if !agmSleepCtx(ctx, agmBeforeSearchDelay) {
			return
		}
	}
}

// runBLESupervisor — независимый супервизор BLE-менеджера и цикла BK300.
func (d *agmAGM) runBLESupervisor(ctx context.Context) {
	for ctx.Err() == nil {
		m := newAGMBLEManager(d.cfg.Devices, ctx.Done())
		go m.run()
		d.runBKCycles(ctx, m)
		m.CloseAll()
		m.waitDone(5 * time.Second)
		if ctx.Err() == nil {
			agmSleepCtx(ctx, agmBeforeSearchDelay)
		}
	}
}

// agmAGM — состояние активной фазы (одна сборка).
type agmAGM struct {
	cfg   *agmConfig
	store *redisStore
	pg    *pgStore
	errs  *agmErrTracker
}

// monitorPZEM крутит цикл PZEM до потери порта/остановки.
func (d *agmAGM) monitorPZEM(ctx context.Context, pz *agmPZEMClient) {
	defer pz.Close()
	log.Printf("agm: состояние: опрос PZEM (BLE-ветка независима), период %s", d.cfg.PollInterval)
	if err := d.runPZEMLoop(ctx, pz); err != nil && ctx.Err() == nil {
		log.Printf("agm: PZEM остановлен — возврат к поиску порта: %v", err)
	}
}

// runPZEMLoop — независимый цикл PZEM-017: чтение, знак по МАП, запись своего
// current и ряда. Ошибка errAGMPZEMLost завершает фазу мониторинга.
func (d *agmAGM) runPZEMLoop(ctx context.Context, pz *agmPZEMClient) error {
	var lastWarn, lastPGWarn time.Time
	for {
		if ctx.Err() != nil {
			return nil
		}
		start := time.Now()
		r, err := pz.Read()
		if errors.Is(err, errAGMPZEMLost) {
			d.errs.report(agmPZEMErrKey, err.Error())
			return err
		}
		if err != nil {
			d.errs.report(agmPZEMErrKey, err.Error())
			if time.Since(lastWarn) >= 10*time.Second {
				lastWarn = time.Now()
				log.Printf("agm: PZEM-017: %v", err)
			}
			agmWaitUntil(start.Add(d.cfg.PollInterval), ctx.Done())
			if ctx.Err() != nil {
				return nil
			}
			continue
		}
		d.errs.clear(agmPZEMErrKey)
		signed, serr := agmSignReading(d.store, r, start)
		if serr != nil {
			if time.Since(lastWarn) >= 10*time.Second {
				lastWarn = time.Now()
				log.Printf("agm: знак тока/мощности не определён (%v) — снимок PZEM отброшен", serr)
			}
			agmWaitUntil(start.Add(d.cfg.PollInterval), ctx.Done())
			if ctx.Err() != nil {
				return nil
			}
			continue
		}
		snap := agmSnapshot{
			Name:      d.cfg.Name,
			Timestamp: agmTimestamp(start),
			Values:    agmBuildPZEMValues(signed),
		}
		if err := d.store.SaveAGMPZEMCurrent(snap); err != nil && time.Since(lastPGWarn) >= time.Minute {
			lastPGWarn = time.Now()
			log.Printf("agm pzem: current: %v", err)
		}
		if err := d.store.SaveAGMPZEMSeries(snap, start); err != nil && time.Since(lastPGWarn) >= time.Minute {
			lastPGWarn = time.Now()
			log.Printf("agm pzem: series: %v", err)
		}
		agmWaitUntil(start.Add(d.cfg.PollInterval), ctx.Done())
		if ctx.Err() != nil {
			return nil
		}
	}
}

// runBKCycles — независимый цикл 4×BK300. Снимок пишется только когда ответили
// все четыре; ошибки источника ведутся по каждому устройству.
func (d *agmAGM) runBKCycles(ctx context.Context, m *agmBLEManager) {
	var lastWarn, lastPGWarn time.Time
	for {
		if ctx.Err() != nil {
			return
		}
		start := time.Now()
		gate := make(chan struct{})
		var wg sync.WaitGroup
		bkCh := make(chan agmBKResult, len(m.devices))
		for i, dev := range m.devices {
			i, dev := i, dev
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-gate
				r, err := m.read(dev)
				bkCh <- agmBKResult{idx: i, r: r, err: err}
			}()
		}
		close(gate)
		done := make(chan struct{})
		go func() { wg.Wait(); close(done) }()
		timedOut := false
		timer := time.NewTimer(agmCycleTimeout)
		select {
		case <-done:
			stopTimer(timer)
		case <-timer.C:
			timedOut = true
			wait := time.NewTimer(5 * time.Second)
			select {
			case <-done:
				stopTimer(wait)
			case <-ctx.Done():
				stopTimer(wait)
				return
			case <-wait.C:
				log.Printf("agm bk: опросчики не завершились за таймаут — перезапуск фазы")
				return
			}
		case <-ctx.Done():
			stopTimer(timer)
			<-done
			return
		}

		bks := make([]agmBKResult, len(m.devices))
		for i := range bks {
			bks[i] = agmBKResult{idx: i, err: errors.New("нет результата BK300")}
		}
		for {
			got := false
			select {
			case b := <-bkCh:
				bks[b.idx] = b
				got = true
			default:
			}
			if !got {
				break
			}
		}

		samples := make([]agmBKSample, 0, len(bks))
		allOK := !timedOut
		for _, b := range bks {
			dev := m.devices[b.idx]
			if b.err != nil {
				allOK = false
				d.errs.report(dev.cfg.Name, dev.cfg.Name+" "+b.err.Error())
				continue
			}
			d.errs.clear(dev.cfg.Name)
			samples = append(samples, agmBKSample{Position: dev.cfg.Position, Reading: b.r})
		}
		if allOK && len(samples) == len(m.devices) {
			snap := agmSnapshot{
				Name:      d.cfg.Name,
				Timestamp: agmTimestamp(start),
				Values:    agmBuildBKValues(samples),
			}
			if err := d.store.SaveAGMBKCurrent(snap); err != nil && time.Since(lastPGWarn) >= time.Minute {
				lastPGWarn = time.Now()
				log.Printf("agm bk: current: %v", err)
			}
			if err := d.store.SaveAGMBKSeries(snap, start); err != nil && time.Since(lastPGWarn) >= time.Minute {
				lastPGWarn = time.Now()
				log.Printf("agm bk: series: %v", err)
			}
		} else if time.Since(lastWarn) >= 10*time.Second {
			lastWarn = time.Now()
			if timedOut {
				log.Printf("agm bk: цикл просрочен (>%s) — снимок не записан", agmCycleTimeout)
			} else {
				log.Printf("agm bk: неполный снимок: %d/4 ответов — не записан", len(samples))
			}
		}
		agmWaitUntil(start.Add(d.cfg.PollInterval), ctx.Done())
		if ctx.Err() != nil {
			return
		}
	}
}

// agmBKResult — результат чтения одного BK300.
type agmBKResult struct {
	idx int
	r   agmBKReading
	err error
}

// agmSignReading получает current МАП, проверяет свежесть (±5 с) и
// возвращает PZEM-показания со знаком. Ошибка — знак определить нельзя.
func agmSignReading(store *redisStore, p agmPZEMReading, ts time.Time) (agmPZEMReading, error) {
	ms, err := store.CurrentOne(mapDevKey)
	if err != nil {
		return p, fmt.Errorf("current МАП: %w", err)
	}
	if ms.Timestamp == "" {
		return p, errors.New("current МАП без метки времени")
	}
	mt, err := time.Parse(time.RFC3339Nano, ms.Timestamp)
	if err != nil {
		return p, fmt.Errorf("время current МАП: %w", err)
	}
	skew := ts.Sub(mt)
	if skew < 0 {
		skew = -skew
	}
	if skew > 5*time.Second {
		return p, fmt.Errorf("current МАП отстаёт на %s (>5 с)", skew.Round(time.Millisecond))
	}
	mapV, ok := snapFloat(ms.Values, "battery_voltage")
	if !ok || mapV <= 0 {
		return p, errors.New("в current МАП нет battery_voltage")
	}
	if p.VoltageV <= 0 {
		return p, errors.New("PZEM без напряжения")
	}
	return agmApplySign(p, mapV), nil
}

// agmWaitUntil ждёт момент t или остановку.
func agmWaitUntil(t time.Time, stop <-chan struct{}) {
	d := time.Until(t)
	if d <= 0 {
		return
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-stop:
	}
}

// agmErrorAfterFailures — сколько НЕПРЕРЫВНЫХ неудачных опросов устройства
// нужно, чтобы зафиксировать ошибку связи в журнале PG. Кратковременные
// пропуски (BLE/USB) не должны засорять журнал.
const agmErrorAfterFailures = 3

// agmErrTracker пишет в журнал ошибок PG только ПЕРВОЕ появление ошибки связи
// по устройству ПОСЛЕ agmErrorAfterFailures неудач подряд; повтор не пишется,
// пока ошибка не снята. Ошибки в телеметрию не попадают (правило проекта).
type agmErrTracker struct {
	mu    sync.Mutex
	pg    *pgStore
	name  string
	fails map[string]int
	seen  map[string]bool
}

func newAGMErrTracker(pg *pgStore, name string) *agmErrTracker {
	return &agmErrTracker{pg: pg, name: name, fails: map[string]int{}, seen: map[string]bool{}}
}

// report — учесть неудачу и записать ошибку связи, когда неудач подряд стало
// не меньше agmErrorAfterFailures (и ещё не записано текущее появление).
func (t *agmErrTracker) report(device, msg string) {
	t.mu.Lock()
	t.fails[device]++
	if t.seen[device] || t.fails[device] < agmErrorAfterFailures {
		t.mu.Unlock()
		return
	}
	t.seen[device] = true
	pg := t.pg
	t.mu.Unlock()
	if pg == nil {
		return
	}
	if err := pg.InsertDeviceErrorNamed(device, t.name, "agm", "comm", msg, time.Now()); err != nil {
		log.Printf("agm: error pg %s: %v", device, err)
	}
}

// clear — ошибка связи снята: счётчик и признак «записано» сбрасываются.
func (t *agmErrTracker) clear(device string) {
	t.mu.Lock()
	delete(t.fails, device)
	delete(t.seen, device)
	t.mu.Unlock()
}

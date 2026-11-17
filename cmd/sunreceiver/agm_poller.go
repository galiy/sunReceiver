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

// Поток AGM: состояния «поиск PZEM-017» → «мониторинг 5 источников». Цикл —
// синхронный старт пяти опросчиков по общему барьеру (снимок «на один момент»),
// следующий цикл не чаще poll_interval от старта предыдущего. Знак тока/мощности
// выставляется до записи по current МАП; без свежего МАП (±5 с) снимок
// отбрасывается. Снимок пишется только при данных от всех 5 источников.
// Документация — docs/modules/agm-monitor.md.

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
	for {
		if ctx.Err() != nil {
			return
		}
		pz, ok := findAGMPZEM(ctx, cfg, d.errs)
		if !ok {
			return
		}
		d.monitor(ctx, pz)
		if ctx.Err() != nil {
			return
		}
		if !agmSleepCtx(ctx, agmBeforeSearchDelay) {
			return
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

// monitor поднимает BLE-менеджер и крутит циклы до потери PZEM/остановки.
func (d *agmAGM) monitor(ctx context.Context, pz *agmPZEMClient) {
	m := newAGMBLEManager(d.cfg.Devices, ctx.Done())
	go m.run()
	defer func() {
		m.CloseAll()
		m.waitDone(5 * time.Second)
		pz.Close()
	}()

	log.Printf("agm: состояние: мониторинг 5 источников, период %s", d.cfg.PollInterval)
	if err := d.runCycles(ctx, pz, m); err != nil && ctx.Err() == nil {
		log.Printf("agm: мониторинг остановлен — возврат к поиску порта: %v", err)
	}
}

// agmPZEMResult — результат PZEM-опросчика цикла.
type agmPZEMResult struct {
	r   agmPZEMReading
	err error
}

// agmBKResult — результат опросчика одного BK300.
type agmBKResult struct {
	idx int
	r   agmBKReading
	err error
}

// runCycles крутит полные циклы опроса. Возвращает ошибку только при потере
// PZEM (нужен переход в режим поиска); при остановке — nil.
func (d *agmAGM) runCycles(ctx context.Context, pz *agmPZEMClient, m *agmBLEManager) error {
	var lastWarn time.Time
	var lastPGWarn time.Time
	for {
		if ctx.Err() != nil {
			return nil
		}
		start := time.Now()

		gate := make(chan struct{})
		var wg sync.WaitGroup
		pzCh := make(chan agmPZEMResult, 1)
		bkCh := make(chan agmBKResult, len(m.devices))

		wg.Add(1)
		go func() {
			defer wg.Done()
			<-gate
			r, err := pz.Read()
			pzCh <- agmPZEMResult{r: r, err: err}
		}()
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
			// Не начинаем следующий цикл, пока опросчики не завершились: иначе
			// запросы к тем же соединениям накладываются.
			if time.Since(lastWarn) >= 10*time.Second {
				lastWarn = time.Now()
				log.Printf("agm: цикл: ответы не собраны за %s — жду завершения опросчиков", agmCycleTimeout)
			}
			// Ограниченное ожидание: зависший serial/D-Bus сверх своих таймаутов не
			// должен вешать модуль навсегда. Не дождались — выходим из фазы
			// (соединения пересоздаст следующий цикл мониторинга).
			wait := time.NewTimer(5 * time.Second)
			select {
			case <-done:
				stopTimer(wait)
			case <-ctx.Done():
				stopTimer(wait)
				return nil
			case <-wait.C:
				log.Printf("agm: опросчики не завершились и за 5 с после таймаута — перезапуск фазы")
				// Фиксируем ошибку связи только по тем опросчикам, чьих
				// результатов нет (недренирующее чтение каналов): PZEM с ошибкой
				// или без результата, BK300 — по отсутствующему idx. Повторы не
				// пишутся (только «не было → есть»), очистка — при успехе.
				pzFailed := true
				select {
				case r := <-pzCh:
					pzFailed = r.err != nil
				default:
				}
				if pzFailed {
					d.errs.report(agmPZEMErrKey, "PZEM-017: обмен не завершился за таймаут цикла")
				}
				got := make(map[int]bool, len(bkCh))
				for {
					drained := false
					select {
					case r := <-bkCh:
						drained = true
						if r.err != nil {
							d.errs.report(m.devices[r.idx].cfg.Name,
								m.devices[r.idx].cfg.Name+" "+r.err.Error())
						} else {
							got[r.idx] = true
						}
					default:
					}
					if !drained {
						break
					}
				}
				for i, dev := range m.devices {
					if !got[i] {
						d.errs.report(dev.cfg.Name, dev.cfg.Name+" не ответил за таймаут цикла")
					}
				}
				return errors.New("опросчики зависли")
			}
		case <-ctx.Done():
			stopTimer(timer)
			select {
			case <-done:
			case <-time.After(5 * time.Second):
			}
			return nil
		}

		var pzRes agmPZEMResult
		select {
		case pzRes = <-pzCh:
		default:
			pzRes.err = errors.New("нет результата PZEM-017")
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

		if errors.Is(pzRes.err, errAGMPZEMLost) {
			d.errs.report(agmPZEMErrKey, pzRes.err.Error())
			return pzRes.err
		}
		// Знак тока/мощности определяется ДО сохранения: PZEM отдаёт модуль,
		// сравниваем UАКБ МАП с напряжением сборки. Нет свежего МАП (±5 с) —
		// снимок не сохраняем вообще: достоверный знак неизвестен.
		signFailed := false
		if pzRes.err == nil && !timedOut {
			signed, serr := agmSignReading(d.store, pzRes.r, start)
			if serr != nil {
				signFailed = true
				if time.Since(lastWarn) >= 10*time.Second {
					lastWarn = time.Now()
					log.Printf("agm: знак тока/мощности не определён (%v) — снимок отброшен", serr)
				}
			} else {
				pzRes.r = signed
			}
		}
		if signFailed {
			agmWaitUntil(start.Add(d.cfg.PollInterval), ctx.Done())
			continue
		}
		if pzRes.err != nil {
			d.errs.report(agmPZEMErrKey, pzRes.err.Error())
		} else {
			d.errs.clear(agmPZEMErrKey)
		}
		for _, dev := range m.devices {
			up, msg := dev.health()
			if up {
				d.errs.clear(dev.cfg.Name)
				continue
			}
			// Пока не было ни одной неудачной попытки (lastErr пуст — идёт
			// первичное подключение), ложную ошибку не пишем.
			if msg == "" {
				continue
			}
			d.errs.report(dev.cfg.Name, dev.cfg.Name+" "+msg)
		}

		// Снимок сохраняется ТОЛЬКО если ответили все 5 источников и знак
		// определён. Нет данных хотя бы по одному устройству — снимок не пишем
		// ни в current, ни в series (дырка в графике честнее частичного снимка).
		pzOK := !timedOut && pzRes.err == nil
		allOK := pzOK
		samples := make([]agmBKSample, 0, len(bks))
		for _, b := range bks {
			if b.err != nil {
				allOK = false
				continue
			}
			samples = append(samples, agmBKSample{Position: m.devices[b.idx].cfg.Position, Reading: b.r})
		}
		if allOK {
			snap := agmSnapshot{
				Name:      d.cfg.Name,
				Timestamp: agmTimestamp(start),
				Values:    agmBuildValues(pzRes.r, samples),
			}
			if err := d.store.SaveAGMCurrent(snap); err != nil {
				if time.Since(lastPGWarn) >= time.Minute {
					lastPGWarn = time.Now()
					log.Printf("agm: current: %v", err)
				}
			}
			if err := d.store.SaveAGMSeries(snap, start); err != nil {
				if time.Since(lastPGWarn) >= time.Minute {
					lastPGWarn = time.Now()
					log.Printf("agm: series: %v", err)
				}
			}
		} else if time.Since(lastWarn) >= 10*time.Second {
			lastWarn = time.Now()
			missing := 0
			if pzRes.err != nil {
				missing++
			}
			for _, b := range bks {
				if b.err != nil {
					missing++
				}
			}
			if timedOut && missing == 0 {
				log.Printf("agm: цикл просрочен (>%s) — снимок не записан", agmCycleTimeout)
			} else {
				log.Printf("agm: неполный цикл: нет данных от %d из 5 источников — снимок не записан", missing)
			}
		}

		agmWaitUntil(start.Add(d.cfg.PollInterval), ctx.Done())
	}
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

// agmErrTracker пишет в журнал ошибок PG только ПЕРВОЕ появление ошибки связи
// по устройству; повтор не пишется, пока ошибка не снята. Ошибки в телеметрию
// не попадают (правило проекта).
type agmErrTracker struct {
	mu   sync.Mutex
	pg   *pgStore
	name string
	seen map[string]bool
}

func newAGMErrTracker(pg *pgStore, name string) *agmErrTracker {
	return &agmErrTracker{pg: pg, name: name, seen: map[string]bool{}}
}

// report — записать ошибку связи устройства, если она ещё не залогирована.
func (t *agmErrTracker) report(device, msg string) {
	t.mu.Lock()
	if t.seen[device] {
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

// clear — ошибка связи снята: следующее появление снова попадёт в журнал.
func (t *agmErrTracker) clear(device string) {
	t.mu.Lock()
	delete(t.seen, device)
	t.mu.Unlock()
}

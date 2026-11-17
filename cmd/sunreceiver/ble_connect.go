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
	"log"
	"sync"
	"time"
)

// bleConnectGate — процесс-глобальный гейт подключений BlueZ. Один BLE-адаптер
// (общий у CE308 и AGM) не поддерживает параллельные Connect: одновременные
// попытки дают org.bluez.Error.InProgress. Гейт сериализует Connect между
// модулями и удерживается владельцем до ФАКТИЧЕСКОГО возврата Connect; если
// вызов завис в D-Bus дольше bleConnectPendingMax, гейт принудительно снимается
// с предупреждением (иначе один зависший Connect заблокировал бы всё навсегда).
// Чтения по уже установленным соединениям не блокируются.
const bleConnectPendingMax = 120 * time.Second

var bleConnectGate struct {
	mu      sync.Mutex
	pending bool
	since   time.Time
	gen     uint64
}

// bleConnectAcquire занимает гейт и возвращает его поколение (владелец).
// false — остановка. Ожидание чужого Connect ограничено bleConnectPendingMax.
func bleConnectAcquire(stop <-chan struct{}) (gen uint64, ok bool) {
	for {
		bleConnectGate.mu.Lock()
		if bleConnectGate.pending && time.Since(bleConnectGate.since) < bleConnectPendingMax {
			bleConnectGate.mu.Unlock()
			timer := time.NewTimer(200 * time.Millisecond)
			select {
			case <-timer.C:
			case <-stop:
				timer.Stop()
				return 0, false
			}
			continue
		}
		if bleConnectGate.pending {
			log.Printf("BLE: Connect завис дольше %s — снимаю блокировку гейта", bleConnectPendingMax)
			// Поколение изменится ниже: старый владелец уже не сможет снять
			// новый гейт своим release.
		}
		bleConnectGate.pending = true
		bleConnectGate.since = time.Now()
		bleConnectGate.gen++
		gen = bleConnectGate.gen
		bleConnectGate.mu.Unlock()
		return gen, true
	}
}

// bleConnectRelease освобождает гейт, только если он всё ещё принадлежит gen.
func bleConnectRelease(gen uint64) {
	bleConnectGate.mu.Lock()
	if bleConnectGate.gen == gen {
		bleConnectGate.pending = false
	}
	bleConnectGate.mu.Unlock()
}

// bleConnectOps учитывает «сырые» Connect, ещё не вернувшиеся фактически.
// connectCE308Bounded может вернуть ошибку по таймауту раньше, чем зависший
// D-Bus-вызов Connect завершится; гейт нельзя отпускать, пока такой вызов жив
// (иначе следующий Connect получит org.bluez.Error.InProgress).
//
// Учёт — ПО ПОКОЛЕНИЮ гейта: вечно зависший Connect старого поколения не должен
// блокировать release новых (гейт старого всё равно снимается принудительно по
// bleConnectPendingMax).
var bleConnectOps struct {
	mu   sync.Mutex
	cond *sync.Cond
	gens map[uint64]int
}

func init() {
	bleConnectOps.cond = sync.NewCond(&bleConnectOps.mu)
	bleConnectOps.gens = map[uint64]int{}
}

// bleConnectOpBegin/End — границы фактического (raw) Connect. Begin вызывается
// СИНХРОННО до запуска горутины, иначе быстрый release успеет увидеть 0.
func bleConnectOpBegin(gen uint64) {
	bleConnectOps.mu.Lock()
	bleConnectOps.gens[gen]++
	bleConnectOps.mu.Unlock()
}

func bleConnectOpEnd(gen uint64) {
	bleConnectOps.mu.Lock()
	if bleConnectOps.gens[gen] > 0 {
		bleConnectOps.gens[gen]--
	}
	if bleConnectOps.gens[gen] <= 0 {
		delete(bleConnectOps.gens, gen)
	}
	bleConnectOps.cond.Broadcast()
	bleConnectOps.mu.Unlock()
}

// bleConnectWaitIdle ждёт фактического завершения raw Connect ТОЛЬКО своего
// поколения. Ограничено bleConnectPendingMax: после force-steal старый владелец
// не должен держать горутину вечно.
func bleConnectWaitIdle(gen uint64) {
	done := make(chan struct{})
	go func() {
		bleConnectOps.mu.Lock()
		for bleConnectOps.gens[gen] > 0 {
			bleConnectOps.cond.Wait()
		}
		delete(bleConnectOps.gens, gen)
		bleConnectOps.mu.Unlock()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(bleConnectPendingMax):
		// Снимаем ожидание своего поколения: иначе waiter навсегда остался бы
		// в cond.Wait (raw Connect так и не вернулся).
		bleConnectOps.mu.Lock()
		delete(bleConnectOps.gens, gen)
		bleConnectOps.cond.Broadcast()
		bleConnectOps.mu.Unlock()
	}
}

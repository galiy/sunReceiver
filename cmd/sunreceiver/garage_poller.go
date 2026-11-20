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
	"sync"
	"time"
)

// Модуль «Управление гаражом»: отдельный поток (решение оператора), держит
// постоянное TCP-соединение с relay4, раз в poll_interval принимает решение по
// данным current Redis (DTS017M для реле 2–4, инверторы «Гараж» для реле 1),
// устанавливает состояние реле и ведёт журнал переключений в PG.
//
// Состояния модуля: 1 — выключен конфигом; 2 — ожидание (нет TCP-связи или нет
// свежих данных DTS017M); 3 — управляет. Состояние и реле сохраняются в Redis
// (sunreceiver:garage:state), уведомления — только при смене состояния.

const (
	garageDeviceKey = "garage"
	garageKind      = "garage"
)

// garageInverterView — инвертор «Гараж» в снимке для дашборда.
type garageInverterView struct {
	Name      string  `json:"name"`
	Serial    string  `json:"serial"`
	Placement string  `json:"placement"`
	PV1       float64 `json:"pv1_voltage"`
	PV2       float64 `json:"pv2_voltage"`
	Fresh     bool    `json:"fresh"`
	AgeSec    float64 `json:"age_sec"`
}

// garageSnapshot — снимок модуля для API/дашборда.
type garageSnapshot struct {
	Disabled       bool                 `json:"disabled"`
	ModuleState    int                  `json:"module_state"`
	StateChangedAt time.Time            `json:"state_changed_at"`
	Relays         [4]bool              `json:"relays"`
	RelayChangedAt [4]time.Time         `json:"relay_changed_at"`
	DeviceUp       bool                 `json:"device_up"`
	HostReady      bool                 `json:"host_ready"`
	Fault          bool                 `json:"phase_fault"`
	Phases         [3]float64           `json:"phases"`
	MeterFresh     bool                 `json:"meter_fresh"`
	MeterAgeSec    float64              `json:"meter_age_sec"`
	Inverters      []garageInverterView `json:"inverters"`
	LastError      string               `json:"last_error"`
	UpdatedAt      time.Time            `json:"updated_at"`
}

// garageModule — состояние горутины модуля и снимок для дашборда.
type garageModule struct {
	cfg   *garageConfig
	store *redisStore
	pg    *pgStore
	cli   *garageClient
	dec   *garageDecider

	mu   sync.RWMutex
	snap garageSnapshot

	// linkLost — связь терялась в текущем запуске (для лога восстановления).
	linkLost bool
	// pendingEvents — события журнала текущего такта: пишутся одним пакетным
	// INSERT, чтобы «повисший» PG не растягивал цикл на секунды на каждое реле.
	pendingEvents []garageRelayEvent
	// lastReconcile — время последней успешной плановой сверки (reconcile_interval_sec).
	lastReconcile time.Time
	// lastMonitor — время последнего продления аренды устройству (MONITOR 1).
	lastMonitor time.Time
	// monitorDue — аренду нужно продлить немедленно (после потери связи).
	monitorDue bool
	// hostReady — хост-МК завершил AT-инициализацию и исполняет кадры реле.
	// До готовности любые SET бесполезны (кадры игнорируются).
	hostReady bool
	// hostNotReadySince — начало непрерывной неготовности хоста: состояние 3
	// допускает его до host_ready_timeout, затем переходим в ожидание.
	hostNotReadySince time.Time
	// meterStaleSince — начало непрерывной строгой неактуальности счётчика:
	// состояние 2 и уведомление — только после state_grace_sec (антидребезг).
	meterStaleSince time.Time
	// lastEventTS — время последней записи журнала по каждому реле: гарантирует
	// строго возрастающие метки в рамках такта (PK журнала — (relay, ts)).
	lastEventTS [4]time.Time
}

// garageMod — активный модуль (nil — выключен конфигом). Пакетная переменная по
// образцу relayCtl: дашборд читает снимок без изменения сигнатуры serveDashboard.
var garageMod *garageModule

// GarageSnapshot возвращает копию снимка (для API дашборда).
func (m *garageModule) GarageSnapshot() garageSnapshot {
	if m == nil {
		return garageSnapshot{Disabled: true, ModuleState: garageStateDisabled}
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.snap
}

func (m *garageModule) setSnapshot(fn func(*garageSnapshot)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	fn(&m.snap)
}

// newGarageModule создаёт модуль (вызывается в main ДО старта горутины и до
// serveDashboard: указатель garageMod должен быть опубликован без гонки).
func newGarageModule(store *redisStore, pg *pgStore, cfg *garageConfig) *garageModule {
	return &garageModule{
		cfg:   cfg,
		store: store,
		pg:    pg,
		cli:   newGarageClient(fmt.Sprintf("%s:%d", cfg.IP, cfg.Port)),
		dec:   newGarageDecider(cfg),
		snap:  garageSnapshot{ModuleState: garageStateWaiting, UpdatedAt: time.Now()},
	}
}

// runGarageControl — цикл модуля гаража (отдельная горутина).
func runGarageControl(m *garageModule, stop context.Context) {
	if m == nil || m.cfg == nil {
		return
	}
	store := m.store
	cfg := m.cfg
	// Восстановление состояния из Redis: идемпотентность после рестарта сервиса.
	if st, err := store.loadGarageState(); err != nil {
		log.Printf("garage: чтение состояния из Redis: %v", err)
	} else if st != nil {
		m.setSnapshot(func(s *garageSnapshot) {
			s.ModuleState = st.ModuleState
			s.StateChangedAt = st.StateChangedAt
			s.Relays = st.Relays
			s.RelayChangedAt = st.RelayChangedAt
			// Авария фаз могла идти до рестарта: восстанавливаем флаг, чтобы не
			// отправить ложное «пропадание фазы» повторно и держать выдержку.
			s.Fault = st.Fault
		})
		m.dec.faultActive = st.Fault
	}
	defer func() {
		// Штатное завершение: гасим аренду (MONITOR 0), иначе устройство будет
		// циклически сбрасываться каждые 5 минут после остановки сервиса.
		cctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		if _, err := m.cli.Monitor(cctx, false); err != nil {
			log.Printf("garage: MONITOR 0 при остановке не доставлен: %v", err)
		}
		cancel()
		m.cli.Close()
	}()

	log.Printf("garage: модуль запущен (%s)", describeGarageConfig(cfg))
	tick := time.NewTicker(cfg.PollInterval)
	defer tick.Stop()
	m.iterate(stop)
	for {
		select {
		case <-stop.Done():
			log.Printf("garage: остановлен")
			return
		case <-tick.C:
			m.iterate(stop)
		}
	}
}

// iterate — один такт: чтение данных мониторинга, при необходимости обмен с
// устройством, установка реле, журнал, уведомления, сохранение состояния.
func (m *garageModule) iterate(ctx context.Context) {
	defer m.flushEvents()
	now := time.Now()
	meter, invs, err := m.readInputs(now)
	if err != nil {
		// Ошибка чтения Redis — это НЕ «данные устарели»: решение по неполным
		// данным не принимаем и реле не трогаем (иначе краткие i/o timeout при
		// старте/нагрузке дёргают реле 4).
		log.Printf("garage: чтение данных мониторинга: %v — такт пропущен", err)
		m.publishInputs(now, meter)
		return
	}
	mem := m.GarageSnapshot().Relays

	// Предварительный расчёт на КОПИИ решателя (таймеры гистерезисов живут в нём):
	// нужен только чтобы понять, требуется ли установка реле — а перед каждой
	// установкой обязателен запрос реального состояния устройства.
	pre := *m.dec
	_, planned := pre.decide(now, mem, meter, invs)
	needMonitor := m.monitorDue || m.lastMonitor.IsZero() ||
		now.Sub(m.lastMonitor) >= m.cfg.MonitorInterval
	needProbe := len(planned) > 0 || !m.GarageSnapshot().DeviceUp ||
		m.cli.LastIO().IsZero() || now.Sub(m.cli.LastIO()) >= m.cfg.TCPKeepalive ||
		m.lastReconcile.IsZero() || now.Sub(m.lastReconcile) >= m.cfg.ReconcileInterval ||
		needMonitor || m.linkLost
	probed := false
	if needProbe {
		// Аренду продлевает только MONITOR 1; он же возвращает состояние реле,
		// поэтому плановый минутный опрос совмещён с продлением.
		var real [4]bool
		var err error
		if needMonitor {
			real, err = m.cli.Monitor(ctx, true)
			if err == nil {
				m.lastMonitor = now
				m.monitorDue = false
			}
		} else {
			real, err = m.cli.State(ctx)
		}
		if err != nil {
			if ctx.Err() != nil {
				return // штатная остановка сервиса — не авария
			}
			m.markDeviceDown(now, err)
			// Таймеры гистерезисов всё равно продвигаем (реле не трогаем).
			m.dec.decide(now, mem, meter, invs)
			m.publishInputs(now, meter)
			m.updateState(now, meter)
			return
		}
		probed = true
		m.lastReconcile = now
		m.setSnapshot(func(s *garageSnapshot) {
			s.DeviceUp = true
			s.LastError = ""
		})
		if m.linkLost {
			log.Printf("garage: связь с реле восстановлена")
			m.linkLost = false
		}
		// Готовность хоста: до неё кадры реле игнорируются — не сверяем и не
		// решаем (таймеры решателя не двигаем), SET слать нельзя.
		ready, err := m.cli.Host(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			m.setSnapshot(func(s *garageSnapshot) { s.HostReady = false })
			m.hostReady = false
			m.markDeviceDown(now, err)
			m.dec.decide(now, mem, meter, invs) // симметрично с ошибкой STATE
			m.publishInputs(now, meter)
			m.updateState(now, meter)
			return
		}
		m.hostReady = ready
		m.setSnapshot(func(s *garageSnapshot) { s.HostReady = ready })
		if !ready {
			m.publishInputs(now, meter)
			m.updateState(now, meter)
			return
		}
		// Устройство — источник истины: приводим локальную память к реальному
		// состоянию (журнал — reason=reconcile) только когда хост готов.
		if real != mem {
			m.reconcileRelays(now, mem, real)
			mem = m.GarageSnapshot().Relays
		}
	}
	// Единственный «настоящий» расчёт такта (таймеры копии не учитываются).
	_, changes := m.dec.decide(now, mem, meter, invs)
	if probed && len(changes) > 0 {
		m.applyChanges(ctx, now, changes)
	}
	m.updateState(now, meter)
	m.publishInputs(now, meter)
}

// publishInputs обновляет снимок входных данных (в т.ч. на пути ошибки связи).
func (m *garageModule) publishInputs(now time.Time, meter garageMeterInput) {
	m.setSnapshot(func(s *garageSnapshot) {
		s.UpdatedAt = now
		s.Phases = meter.Voltage
		s.MeterFresh = meter.Fresh
	})
}

// applyChanges устанавливает реле по решению. Перед установкой реальное состояние
// уже прочитано (см. iterate); каждая команда идемпотентна.
func (m *garageModule) applyChanges(ctx context.Context, now time.Time, changes []garageRelayChange) {
	for _, ch := range changes {
		if err := m.cli.Set(ctx, ch.Relay, ch.On); err != nil {
			if ctx.Err() != nil {
				return // штатная остановка — не авария
			}
			m.markDeviceDown(now, err)
			return
		}
		old := m.GarageSnapshot().Relays[ch.Relay-1]
		if old == ch.On {
			continue
		}
		m.noteRelayChange(now, ch.Relay, old, ch.On, ch.Reason)
		m.dec.noteSwitch(ch.Relay, now)
	}
}

// reconcileRelays приводит локальную память к реальному состоянию устройства.
func (m *garageModule) reconcileRelays(now time.Time, mem, real [4]bool) {
	for i := 0; i < 4; i++ {
		if mem[i] == real[i] {
			continue
		}
		// Приведение к реальному состоянию НЕ запускает гистерезис реле 1:
		// после перезагрузки/сброса устройства (все реле off) модуль должен
		// восстановить расчётное состояние сразу, а не ждать 5 минут.
		m.noteRelayChange(now, i+1, mem[i], real[i], "reconcile")
	}
}

// noteRelayChange обновляет память реле, пишет событие в журнал PG и сохраняет
// состояние в Redis.
func (m *garageModule) noteRelayChange(now time.Time, relay int, old, on bool, reason string) {
	// Строго возрастающая метка на реле: reconcile и установка в одном такте
	// иначе получили бы одинаковый ts и второе событие потерялось бы по PK.
	if !now.After(m.lastEventTS[relay-1]) {
		now = m.lastEventTS[relay-1].Add(time.Millisecond)
	}
	m.lastEventTS[relay-1] = now
	m.setSnapshot(func(s *garageSnapshot) {
		s.Relays[relay-1] = on
		s.RelayChangedAt[relay-1] = now
	})
	m.pendingEvents = append(m.pendingEvents, garageRelayEvent{
		TS: now, Relay: relay, Old: old, New: on, Reason: reason,
	})
	m.persist()
}

// flushEvents пишет накопленные за такт события журнала одним пакетным INSERT.
func (m *garageModule) flushEvents() {
	if len(m.pendingEvents) == 0 {
		return
	}
	events := m.pendingEvents
	m.pendingEvents = nil
	if m.pg == nil {
		return
	}
	if err := m.pg.insertGarageRelayEvents(events); err != nil {
		log.Printf("garage: журнал переключений PG (%d событий): %v", len(events), err)
	}
}

// markDeviceDown фиксирует потерю связи с relay4 (появление ошибки — в PG один раз).
func (m *garageModule) markDeviceDown(now time.Time, err error) {
	was := m.GarageSnapshot().DeviceUp
	m.cli.Close()
	m.setSnapshot(func(s *garageSnapshot) {
		s.DeviceUp = false
		s.LastError = err.Error()
	})
	if !m.linkLost {
		log.Printf("garage: потеря связи с реле: %v", err)
	}
	m.linkLost = true
	m.monitorDue = true
	if m.pg != nil && was {
		if e := m.pg.InsertDeviceError(garageDeviceKey, garageKind, "link", "нет связи с реле гаража", now); e != nil {
			log.Printf("garage: device_errors: %v", e)
		}
	}
}

// updateState вычисляет состояние модуля и рассылает уведомления при переходе.
func (m *garageModule) updateState(now time.Time, meter garageMeterInput) {
	prev := m.GarageSnapshot().ModuleState
	if m.hostReady {
		m.hostNotReadySince = time.Time{}
	} else if m.hostNotReadySince.IsZero() {
		m.hostNotReadySince = now
	}
	hostWaiting := !m.hostReady && !m.hostNotReadySince.IsZero() &&
		now.Sub(m.hostNotReadySince) >= m.cfg.HostReadyTimeout
	want := garageStateControl
	if !m.GarageSnapshot().DeviceUp {
		want = garageStateWaiting
	} else if hostWaiting {
		want = garageStateWaiting
	} else if !meter.FreshStrict {
		// Кратковременные пропуски неактуальности не должны дёргать состояние и
		// слать уведомления: ждём непрерывно state_grace_sec.
		if m.meterStaleSince.IsZero() {
			m.meterStaleSince = now
		}
		if now.Sub(m.meterStaleSince) < m.cfg.StateGrace {
			want = prev
		} else {
			want = garageStateWaiting
		}
	} else {
		m.meterStaleSince = time.Time{}
	}
	if prev != want {
		m.setSnapshot(func(s *garageSnapshot) {
			s.ModuleState = want
			s.StateChangedAt = now
		})
		m.persist()
		// Уведомления — только по смене состояния модуля (решение оператора:
		// идемпотентно к рестарту). Текст зависит от причины.
		switch {
		case prev == garageStateControl && want == garageStateWaiting:
			if hostWaiting {
				m.notify("⚠️ Гараж: хост реле не готов — управление приостановлено")
			} else if m.GarageSnapshot().DeviceUp {
				m.notify("⚠️ Гараж: нет свежих данных счётчика — управление приостановлено")
			} else {
				m.notify("⚠️ Гараж: нет связи с реле — управление приостановлено")
			}
		case prev == garageStateWaiting && want == garageStateControl:
			m.notify("✅ Гараж: управление возобновлено")
		}
	}
	// Авария фаз: уведомления по фронтам только в состоянии управления. Без
	// свежих данных счётчика или без связи с реле фактическое состояние реле
	// неизвестно — уведомления не шлём, флаг храним по состоянию решателя.
	fault := m.dec.faultActive
	if !meter.Fresh || want != garageStateControl {
		// Фактическое состояние реле неизвестно — уведомлений нет, но флаг
		// синхронизируем с решателем и сохраняем: иначе после рестарта
		// выдержка восстановления потеряется, а затем придёт ложное «восстановлено».
		if m.GarageSnapshot().Fault != fault {
			m.setSnapshot(func(s *garageSnapshot) { s.Fault = fault })
			m.persist()
		}
		return
	}
	if fault != m.GarageSnapshot().Fault {
		m.setSnapshot(func(s *garageSnapshot) { s.Fault = fault })
		m.persist()
		if fault {
			m.notify(fmt.Sprintf("⚠️ Гараж: пропадание фазы (A=%.0f В, B=%.0f В, C=%.0f В) — подача отключена",
				meter.Voltage[0], meter.Voltage[1], meter.Voltage[2]))
		} else {
			m.notify("✅ Гараж: напряжение на всех фазах восстановлено")
		}
	}
}

// notify пишет событие в Redis Stream уведомлений (отправка — cmd/maxbot).
func (m *garageModule) notify(msg string) {
	if m.store == nil {
		return
	}
	if err := m.store.enqueueNotifyEvent(msg); err != nil {
		log.Printf("garage: уведомление не записано: %v", err)
	}
}

// persist сохраняет состояние модуля и реле в Redis.
func (m *garageModule) persist() {
	if m.store == nil {
		return
	}
	s := m.GarageSnapshot()
	st := garagePersistState{
		ModuleState:    s.ModuleState,
		StateChangedAt: s.StateChangedAt,
		Relays:         s.Relays,
		RelayChangedAt: s.RelayChangedAt,
		Fault:          s.Fault,
	}
	if err := m.store.saveGarageState(st); err != nil {
		log.Printf("garage: сохранение состояния в Redis: %v", err)
	}
}

// readInputs читает DTS017M и инверторы «Гараж» из current Redis.
func (m *garageModule) readInputs(now time.Time) (garageMeterInput, []garageInverterInput, error) {
	var meter garageMeterInput
	var invs []garageInverterInput
	var views []garageInverterView
	var firstErr error

	if dts017Cfg != nil {
		snap, err := m.store.Dts017Current()
		if err != nil {
			firstErr = err
		} else if snap != nil {
			age := time.Duration(0)
			ts, ok := parseSnapshotTime(snap.Timestamp)
			if ok {
				age = now.Sub(ts)
			}
			meter.Voltage = [3]float64{
				snap.Values[dts017VoltageA],
				snap.Values[dts017VoltageB],
				snap.Values[dts017VoltageC],
			}
			if ok {
				meter.Fresh, meter.FreshStrict = meterFreshness(age, m.cfg.MeterStale, m.cfg.MeterStaleGrace)
			}
			m.setSnapshot(func(s *garageSnapshot) {
				s.MeterAgeSec = age.Seconds()
			})
		}
	}

	for _, b := range []*invBrand{&deyeBrand, &sofarBrand} {
		snaps, err := m.store.invCurrent(*b)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		for _, sn := range snaps {
			if sn.Placement != gridPlacementGar {
				continue
			}
			age := time.Duration(0)
			ts, ok := parseSnapshotTime(sn.Timestamp)
			if ok {
				age = now.Sub(ts)
			}
			fresh := ok && age >= 0 && age <= m.cfg.InverterStale
			pv1 := valueFloat(sn.Values, "pv1_voltage")
			pv2 := valueFloat(sn.Values, "pv2_voltage")
			invs = append(invs, garageInverterInput{Name: sn.Name, Fresh: fresh, PV1: pv1, PV2: pv2})
			views = append(views, garageInverterView{
				Name: sn.Name, Serial: sn.InverterSN, Placement: sn.Placement,
				PV1: pv1, PV2: pv2, Fresh: fresh, AgeSec: age.Seconds(),
			})
		}
	}
	m.setSnapshot(func(s *garageSnapshot) { s.Inverters = views })
	return meter, invs, firstErr
}

// parseSnapshotTime разбирает RFC3339-время снимка из current Redis.
func parseSnapshotTime(v string) (time.Time, bool) {
	if v == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// valueFloat достаёт числовое значение тега из контракта values (после JSON —
// float64).
func valueFloat(vc valuesContract, tag string) float64 {
	v, ok := vc[tag]
	if !ok {
		return 0
	}
	switch x := v.(type) {
	case float64:
		return x
	case float32:
		return float64(x)
	case int:
		return float64(x)
	case int64:
		return float64(x)
	}
	return 0
}

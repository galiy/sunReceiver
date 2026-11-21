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
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"tinygo.org/x/bluetooth"
)

// BLE-клиент BK300 и менеджер четырёх соединений. Адаптер — общий с CE308
// (решение оператора): отдельного hci у AGM нет, используется тот же
// bluetooth.DefaultAdapter и те же помощники BlueZ (ce308EnsurePowered и т.д.;
// см. ce308_agent_linux.go / ble_connect.go). Соединения постоянные; Connect
// сериализуется процесс-глобальным гейтом (bleConnectAcquire), который держится
// до фактического возврата Connect.

const (
	agmBKServiceUUID = "0000fff0-0000-1000-8000-00805f9b34fb"
	agmBKNotifyUUID  = "0000fff1-0000-1000-8000-00805f9b34fb"
	agmBKWriteUUID   = "0000fff2-0000-1000-8000-00805f9b34fb"
)

// Таймауты/бэкоффы BK300 (константы: в конфиг не выносятся).
const (
	agmBKConnectTimeout = 60 * time.Second
	agmBKReadTimeout    = 5 * time.Second
	agmBKReconnectMin   = 2 * time.Second
	agmBKReconnectMax   = 30 * time.Second
	agmBKSerialDelay    = 300 * time.Millisecond // пауза после подписки на нотификации
	agmBKGATTOpTimeout  = 3 * time.Second
	// agmBKSerialMismatchDelay — пауза до повторной попытки после валидного, но
	// ЧУЖОГО серийника: переподключать постоянно бессмысленно (документация:
	// устройство считается недоступным), но и «навсегда» не блокируем — вдруг
	// на точке заменили устройство.
	agmBKSerialMismatchDelay = 30 * time.Minute
)

// errAGMBKTimeout — ответ не пришёл за отведённое время.
var errAGMBKTimeout = errors.New("нет ответа BK300 (таймаут)")

// errAGMBKSerialMismatch — прочитан валидный ASCII-серийник, но он не совпал с
// конфигом (долгая пауза, а не обычный reconnect-бэкофф).
var errAGMBKSerialMismatch = errors.New("серийник BK300 не совпадает с конфигом")

// agmBKClient — установленное соединение с одним BK300.
type agmBKClient struct {
	mac string
	dev bluetooth.Device
	wr  bluetooth.DeviceCharacteristic
	ntf bluetooth.DeviceCharacteristic

	reqMu   sync.Mutex // один запрос на устройство за раз
	mu      sync.Mutex
	reader  agmBKFrameReader
	pending [2]byte
	hasPend bool
	reply   chan agmBKFrame
	closed  bool
	closedC chan struct{}
}

// connectAGMBK подключается к BK300 и настраивает GATT. Держит общий гейт
// Connect до фактического завершения попытки (включая подготовку адаптера и
// discovery), чтобы не пересечься с CE308.
func connectAGMBK(mac string, connectTimeout, readTimeout time.Duration, stop <-chan struct{}) (*agmBKClient, error) {
	gen, ok := bleConnectAcquire(stop)
	if !ok {
		return nil, fmt.Errorf("подключение к %s прервано остановкой", mac)
	}

	type res struct {
		c   *agmBKClient
		err error
	}
	ch := make(chan res, 1)
	go func() {
		c, cerr := connectAGMBKInner(mac)
		ch <- res{c, cerr}
	}()

	timer := time.NewTimer(connectTimeout)
	defer timer.Stop()
	drain := func() {
		go func() {
			r := <-ch
			if r.c != nil {
				_ = r.c.Close()
			}
			bleConnectRelease(gen)
		}()
	}
	select {
	case r := <-ch:
		bleConnectRelease(gen)
		return r.c, r.err
	case <-timer.C:
		// Connect может «зависнуть» в D-Bus. Не ждём его, но гейт освободит
		// только сторож, когда Connect фактически вернётся, — иначе следующий
		// Connect получит org.bluez.Error.InProgress.
		drain()
		return nil, fmt.Errorf("таймаут подключения к %s (%s)", mac, connectTimeout)
	case <-stop:
		drain()
		return nil, fmt.Errorf("подключение к %s прервано остановкой", mac)
	}
}

// connectAGMBKInner — подготовка адаптера, discovery и Connect без внешнего
// таймаута (таймаут снаружи). Выполняется под гейтом Connect.
func connectAGMBKInner(mac string) (*agmBKClient, error) {
	// Перед попыткой — заново определить реальный контроллер: после внешней
	// переинициализации USB-адаптера hci-индекс мог смениться.
	ce308AdapterIDReset()
	if err := ce308EnsurePowered(); err != nil {
		log.Printf("agm: BLE-адаптер: %v (не критично)", err)
	}
	a := btDefaultAdapter()
	if err := a.Enable(); err != nil {
		return nil, fmt.Errorf("включение BLE-адаптера: %w", err)
	}
	if err := ensureCE308Known(mac); err != nil {
		return nil, fmt.Errorf("обнаружение %s: %w", mac, err)
	}

	mac6, err := bluetooth.ParseMAC(mac)
	if err != nil {
		return nil, fmt.Errorf("неверный MAC %q: %w", mac, err)
	}
	addr := bluetooth.Address{MACAddress: bluetooth.MACAddress{MAC: mac6}}

	dev, err := a.Connect(addr, bluetooth.ConnectionParams{})
	if err != nil {
		return nil, fmt.Errorf("Connect %s: %w", mac, err)
	}
	ok := false
	defer func() {
		if !ok {
			_ = disconnectAGMBKBounded(dev)
		}
	}()

	svcUUID, _ := bluetooth.ParseUUID(agmBKServiceUUID)
	notifyUUID, _ := bluetooth.ParseUUID(agmBKNotifyUUID)
	writeUUID, _ := bluetooth.ParseUUID(agmBKWriteUUID)

	svcs, err := dev.DiscoverServices([]bluetooth.UUID{svcUUID})
	if err != nil {
		return nil, fmt.Errorf("DiscoverServices %s: %w", mac, err)
	}
	if len(svcs) == 0 {
		return nil, fmt.Errorf("сервис FFF0 не найден у %s", mac)
	}
	chars, err := svcs[0].DiscoverCharacteristics([]bluetooth.UUID{notifyUUID, writeUUID})
	if err != nil {
		return nil, fmt.Errorf("DiscoverCharacteristics %s: %w", mac, err)
	}
	if len(chars) != 2 {
		return nil, fmt.Errorf("характеристики FFF1/FFF2 не найдены у %s", mac)
	}
	var ntf, wr *bluetooth.DeviceCharacteristic
	for i := range chars {
		switch chars[i].UUID() {
		case notifyUUID:
			ntf = &chars[i]
		case writeUUID:
			wr = &chars[i]
		}
	}
	if ntf == nil || wr == nil {
		return nil, fmt.Errorf("характеристики FFF1/FFF2 не сопоставлены у %s", mac)
	}
	c := &agmBKClient{dev: dev, ntf: *ntf, wr: *wr, mac: mac, closedC: make(chan struct{})}
	// StartNotify заранее и с таймаутом: иначе tinygo может зависнуть внутри
	// библиотеки с уже зарегистрированным (и нечитаемым) signal-каналом.
	if err := bluezPreStartNotify(mac, agmBKNotifyUUID, 5*time.Second); err != nil {
		return nil, fmt.Errorf("подготовка нотификаций %s: %w", mac, err)
	}
	if err := (&c.ntf).EnableNotifications(c.onNotify); err != nil {
		// StartNotify мог успеть зарегистрировать D-Bus signal-канал до ошибки.
		// Без явного EnableNotifications(nil) канал остаётся в общем bus и копит
		// deferredDeliver-горутины (утечка памяти до OOM).
		_ = (&c.ntf).EnableNotifications(nil)
		return nil, fmt.Errorf("EnableNotifications %s: %w", mac, err)
	}
	// Даём устройству завершить стартовую последовательность: сразу после
	// подписки ответ на 02 01 у части экземпляров приходит мусором.
	time.Sleep(agmBKSerialDelay)
	ok = true
	return c, nil
}

// onNotify принимает фрагменты нотификаций и раздаёт собранные кадры ожидающему
// запросу.
func (c *agmBKClient) onNotify(buf []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	for _, f := range c.reader.Push(buf) {
		if !c.hasPend || f.Cmd != agmBKReplyCmd(c.pending) || c.reply == nil {
			continue
		}
		select {
		case c.reply <- f:
		default: // ожидающий уже получил кадр/не успел — не копим
		}
	}
}

// agmBKReplyCmd — команда ответа: к первому байту запроса добавлен бит 0x40.
func agmBKReplyCmd(cmd [2]byte) [2]byte { return [2]byte{cmd[0] | 0x40, cmd[1]} }

// request отправляет команду и ждёт ответ с командой agmBKReplyCmd(cmd).
func (c *agmBKClient) request(cmd [2]byte, timeout time.Duration) ([]byte, error) {
	c.reqMu.Lock()
	defer c.reqMu.Unlock()

	ch := make(chan agmBKFrame, 1)

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, errors.New("соединение закрыто")
	}
	c.reader.Reset()
	c.pending = cmd
	c.hasPend = true
	c.reply = ch
	closedC := c.closedC
	c.mu.Unlock()

	clearPending := func() {
		c.mu.Lock()
		c.hasPend = false
		c.reply = nil
		c.mu.Unlock()
	}

	if err := c.writeFrame(buildAGMBKRequest(cmd, nil)); err != nil {
		clearPending()
		return nil, fmt.Errorf("запись %s: %w", c.mac, err)
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case f := <-ch:
		clearPending()
		return parseAGMBKReply(cmd, f)
	case <-closedC:
		clearPending()
		return nil, errors.New("соединение закрыто")
	case <-timer.C:
		clearPending()
		return nil, fmt.Errorf("%s: %w", c.mac, errAGMBKTimeout)
	}
}

// readVoltage — один опрос 0B0B.
func (c *agmBKClient) readVoltage(timeout time.Duration) (agmBKReading, error) {
	payload, err := c.request(agmCmdBKVoltage, timeout)
	if err != nil {
		return agmBKReading{}, err
	}
	return parseAGMBKVoltage(payload)
}

// verifySerial сверяет серийник устройства с конфигом (команда 02 01). Повторяем
// только при сбое обмена; валидный, но чужой серийник — сразу отказ.
func (c *agmBKClient) verifySerial(want string, timeout time.Duration) error {
	var last, parseErr error
	lastWasParse := false
	for i := 0; i < 2; i++ {
		if i > 0 {
			time.Sleep(200 * time.Millisecond)
		}
		payload, err := c.request(agmCmdBKSerial, timeout)
		if err != nil {
			last, lastWasParse = err, false
			continue
		}
		got, err := parseAGMBKSerial(payload)
		if err != nil {
			// Нечитаемый payload (часть прошивок на 02 01 отдаёт не серийник):
			// пробуем ещё раз, решение — по исходу последней попытки.
			last, parseErr, lastWasParse = err, err, true
			continue
		}
		if got != strings.TrimSpace(want) {
			return fmt.Errorf("%w: %s: %q != %q",
				errAGMBKSerialMismatch, c.mac, got, want)
		}
		return nil
	}
	if lastWasParse {
		// Последняя попытка дала валидный ответ 42 01 с нечитаемым payload —
		// идентификация по MAC из конфига (серийник справочный).
		log.Printf("agm: BK300 %s: серийник не прочитан (%v) — идентификация по MAC", c.mac, parseErr)
		return nil
	}
	if last == nil {
		last = errors.New("серийник не прочитан")
	}
	return last // транспортная ошибка — обычный reconnect-бэкофф
}

// Close разрывает соединение и будит ожидающий запрос.
func (c *agmBKClient) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	c.hasPend = false
	c.reply = nil
	close(c.closedC)
	c.mu.Unlock()
	// Снимаем подписку на нотификации: tinygo bluetooth на Linux держит для
	// каждой EnableNotifications отдельный D-Bus signal-канал и без явного
	// EnableNotifications(nil) не освобождает его. Иначе каждый реконнект BK300
	// оставляет канал и растущие deferredDeliver-горутины (OOM за ~9-11 ч).
	_ = (&c.ntf).EnableNotifications(nil)
	return disconnectAGMBKBounded(c.dev)
}

// agmBLOpSem ограничивает число одновременно «висящих» BLE-операций: если
// bluetoothd завис, горутины не накапливаются без предела.
var agmBLOpSem = make(chan struct{}, 8)

// runAGMBounded выполняет f с таймаутом, занимая слот семафора до фактического
// завершения f (даже если мы уже вернули таймаут).
func runAGMBounded(d time.Duration, what string, f func() error) error {
	select {
	case agmBLOpSem <- struct{}{}:
	default:
		return fmt.Errorf("%s: слишком много зависших BLE-операций", what)
	}
	ch := make(chan error, 1)
	go func() {
		defer func() { <-agmBLOpSem }()
		ch <- f()
	}()
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case err := <-ch:
		return err
	case <-t.C:
		return fmt.Errorf("%s: таймаут %s", what, d)
	}
}

// writeFrame пишет кадр с таймаутом.
func (c *agmBKClient) writeFrame(p []byte) error {
	return runAGMBounded(agmBKGATTOpTimeout, "запись GATT", func() error {
		_, err := c.wr.WriteWithoutResponse(p)
		return err
	})
}

// disconnectAGMBKBounded разрывает соединение с таймаутом.
func disconnectAGMBKBounded(dev bluetooth.Device) error {
	return runAGMBounded(agmBKGATTOpTimeout, "Disconnect", dev.Disconnect)
}

// agmBKDevice — состояние одного BK300.
type agmBKDevice struct {
	cfg agmDeviceConfig

	mu      sync.Mutex
	conn    *agmBKClient
	up      bool
	nextTry time.Time
	backoff time.Duration
	lastErr string
	lastLog time.Time
}

// agmBLEManager держит соединения с четырьмя BK300 и восстанавливает их.
// Подключения выполняются строго по одному (BlueZ не умеет параллельный Connect).
type agmBLEManager struct {
	devices []*agmBKDevice

	mu           sync.Mutex
	closed       bool
	adapterFails int

	stop     chan struct{}
	stopOnce sync.Once
	done     chan struct{}
}

// newAGMBLEManager создаёт менеджер. parent — внешний сигнал остановки сервиса:
// по нему менеджер закрывает свои соединения и завершает run.
func newAGMBLEManager(cfg []agmDeviceConfig, parent <-chan struct{}) *agmBLEManager {
	m := &agmBLEManager{stop: make(chan struct{}), done: make(chan struct{})}
	for _, d := range cfg {
		m.devices = append(m.devices, &agmBKDevice{cfg: d, nextTry: time.Now()})
	}
	go func() {
		select {
		case <-parent:
			m.CloseAll()
		case <-m.done:
		}
	}()
	return m
}

func (m *agmBLEManager) run() {
	defer close(m.done)
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-m.stop:
			return
		case <-t.C:
			m.connectDue()
		}
	}
}

// connectDue подключает все устройства, у которых наступил момент попытки.
func (m *agmBLEManager) connectDue() {
	for _, d := range m.devices {
		if m.isClosed() {
			return
		}
		d.mu.Lock()
		due := !d.up && !time.Now().Before(d.nextTry)
		d.mu.Unlock()
		if !due {
			continue
		}
		m.connectOne(d)
	}
}

// connectOne подключается к устройству и сверяет серийник.
func (m *agmBLEManager) connectOne(d *agmBKDevice) {
	if m.isClosed() {
		return
	}
	c, err := connectAGMBK(d.cfg.MAC, agmBKConnectTimeout, agmBKReadTimeout, m.stop)
	transportFail := err != nil
	serialMismatch := false
	if err == nil {
		if err = c.verifySerial(d.cfg.Serial, agmBKReadTimeout); err != nil {
			// Долгая пауза — только при реально прочитанном чужом серийнике;
			// таймауты/шум 02 01 — обычный reconnect-бэкофф.
			serialMismatch = errors.Is(err, errAGMBKSerialMismatch)
			_ = c.Close()
		}
	}
	if err != nil {
		if transportFail && isAGMConnectStuck(err) {
			ce308ClearStuck(d.cfg.MAC)
		}
		m.connectFailed(d, err, transportFail, serialMismatch)
		return
	}
	// Повторная проверка под замком: CloseAll мог пройти, пока шёл Connect.
	d.mu.Lock()
	if mClosed := m.isClosed(); mClosed || d.cfg.MAC == "" {
		d.mu.Unlock()
		_ = c.Close()
		return
	}
	d.conn = c
	d.up = true
	d.lastErr = ""
	d.mu.Unlock()
	if m.isClosed() {
		d.mu.Lock()
		if d.conn == c {
			d.conn = nil
			d.up = false
		}
		d.mu.Unlock()
		_ = c.Close()
		return
	}
	m.mu.Lock()
	m.adapterFails = 0
	m.mu.Unlock()
	log.Printf("agm: BK300 %s (%s, позиция %d): подключено", d.cfg.Name, d.cfg.MAC, d.cfg.Position)
}

func (m *agmBLEManager) connectFailed(d *agmBKDevice, err error, transportFail, serialMismatch bool) {
	if m.isClosed() {
		return
	}
	now := time.Now()
	d.mu.Lock()
	if serialMismatch {
		// Валидный, но чужой серийник: не долбим устройство постоянными
		// переподключениями; редкая повторная проверка — вдруг заменили.
		d.backoff = agmBKSerialMismatchDelay
	} else {
		d.backoff = agmNextBackoff(d.backoff)
	}
	d.nextTry = now.Add(d.backoff)
	quiet := now.Sub(d.lastLog) < 5*time.Minute
	if !quiet {
		d.lastLog = now
	}
	d.lastErr = err.Error()
	backoff := d.backoff
	d.mu.Unlock()
	if !quiet {
		log.Printf("agm: BK300 %s: подключение не удалось (%v), пауза %s", d.cfg.Name, err, backoff)
	}
	if !transportFail {
		return // несовпадение серийника — не проблема адаптера
	}

	m.mu.Lock()
	m.adapterFails++
	fails := m.adapterFails
	m.mu.Unlock()
	// Адаптер, похоже, «мёртв»: все устройства не подключаются подряд. Внутренняя
	// мера — заново определить контроллер и включить его; глубокий сброс (rfkill/
	// btusb) — задача внешнего BLE-watchdog, демон чужие соединения не трогает.
	if fails >= 3 && m.allDown() {
		ce308AdapterIDReset()
		ce308EnsurePowered()
		m.mu.Lock()
		m.adapterFails = 0
		m.mu.Unlock()
		log.Printf("agm: BLE-адаптер: повторная инициализация после серии отказов подключений")
	}
}

func (m *agmBLEManager) allDown() bool {
	for _, d := range m.devices {
		d.mu.Lock()
		up := d.up
		d.mu.Unlock()
		if up {
			return false
		}
	}
	return true
}

// read опрашивает устройство; при ошибке соединение сбрасывается.
func (m *agmBLEManager) read(d *agmBKDevice) (agmBKReading, error) {
	d.mu.Lock()
	c := d.conn
	up := d.up
	d.mu.Unlock()
	if !up || c == nil {
		return agmBKReading{}, errors.New("нет BLE-соединения")
	}
	r, err := c.readVoltage(agmBKReadTimeout)
	if err != nil {
		// Повтор на том же соединении опасен: запоздавший ответ 0B0B можно
		// принять за свежий. Поэтому при любой ошибке соединение пересоздаётся.
		m.drop(d, c, err)
		return agmBKReading{}, err
	}
	d.mu.Lock()
	d.backoff = 0 // успешное чтение — сброс бэкоффа
	d.mu.Unlock()
	return r, nil
}

// drop помечает устройство упавшим и запускает переподключение с бэкоффом.
func (m *agmBLEManager) drop(d *agmBKDevice, c *agmBKClient, err error) {
	d.mu.Lock()
	wasUp := d.up
	dropped := d.conn == c
	backoff := d.backoff
	if dropped {
		d.conn = nil
		d.up = false
		d.backoff = agmNextBackoff(d.backoff)
		d.nextTry = time.Now().Add(d.backoff)
		d.lastErr = err.Error()
		backoff = d.backoff
	}
	d.mu.Unlock()
	if c != nil {
		_ = c.Close()
	}
	if wasUp && dropped {
		log.Printf("agm: BK300 %s: связь потеряна (%v), переподключение через %s", d.cfg.Name, err, backoff)
	}
}

// CloseAll закрывает все соединения и останавливает менеджер. Идемпотентна.
func (m *agmBLEManager) CloseAll() {
	m.stopOnce.Do(func() { close(m.stop) })
	m.mu.Lock()
	m.closed = true
	m.mu.Unlock()
	for _, d := range m.devices {
		d.mu.Lock()
		c := d.conn
		d.conn = nil
		d.up = false
		d.mu.Unlock()
		if c != nil {
			_ = c.Close()
		}
	}
}

func (m *agmBLEManager) isClosed() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.closed
}

// waitDone ждёт завершения run (с таймаутом).
func (m *agmBLEManager) waitDone(d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-m.done:
	case <-t.C:
		log.Printf("agm: BLE-менеджер не завершился за %s", d)
	}
}

// health — состояние устройства (up + последняя ошибка).
func (d *agmBKDevice) health() (bool, string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.up, d.lastErr
}

// agmNextBackoff — ограниченный экспоненциальный бэкофф.
func agmNextBackoff(cur time.Duration) time.Duration {
	if cur < agmBKReconnectMin {
		return agmBKReconnectMin
	}
	cur *= 2
	if cur > agmBKReconnectMax {
		cur = agmBKReconnectMax
	}
	return cur
}

// isAGMConnectStuck — ошибка BlueZ о незавершённой операции.
func isAGMConnectStuck(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	for _, frag := range []string{"in progress", "busy", "already"} {
		if strings.Contains(s, frag) {
			return true
		}
	}
	return false
}

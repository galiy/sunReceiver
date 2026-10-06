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
	"sync"
	"time"

	"tinygo.org/x/bluetooth"
)

// BLE-транспорт BMS EnBMS (Enjie EMU110x): GATT-сервис 0000ff00, запись команд
// в 0000ff02 (write without response), ответы — уведомления на 0000ff01
// несколькими фрагментами. Кадр собирается ПО ДЛИНЕ (см. extractEnBmsFrame).
// Структура работы с BLE (постоянное соединение, переподключение с бэкоффом,
// классификация ошибок) повторяет модуль CE308 (ce308_client.go).

// errEnBmsReadTimeout — таймаут ожидания ответа BMS (транзиентный сбой радио):
// пулер ретраит именно упавшее чтение, не разрывая постоянное соединение.
var errEnBmsReadTimeout = errors.New("нет ответа BMS EnBMS по BLE (таймаут)")

// errEnBmsStateUnsupported — блок состояния/защит (RS485 CID2 0x44 TeleState)
// недоступен по BLE: у BLE-блока Battery (0x61) состояния идут в сигнальном
// хвосте (parseEnBmsTail).
var errEnBmsStateUnsupported = errors.New("TeleState (0x44) недоступен по BLE")

// errEnBmsClosed — чтение прервано намеренно при остановке сервиса: пулер
// должен немедленно выйти и корректно закрыть BLE-соединение.
var errEnBmsClosed = errors.New("чтение EnBMS прервано (остановка сервиса)")

// isEnBmsReadTimeout — true, если ошибка является таймаутом чтения.
func isEnBmsReadTimeout(err error) bool { return errors.Is(err, errEnBmsReadTimeout) }

// isEnBmsClosed — true, если ошибка — намеренное прерывание при остановке.
func isEnBmsClosed(err error) bool { return errors.Is(err, errEnBmsClosed) }

// Времена/лимиты BLE-операций EnBMS.
const (
	// enBmsConnectTimeout — лимит на одну попытку подключения (зависший Connect
	// на D-Bus может блокироваться бесконечно).
	enBmsConnectTimeout = 20 * time.Second
	// enBmsReadTimeout — лимит ожидания ответа на запрос Battery (живой срез: ~0.3 с;
	// запас на фрагментацию и слабый сигнал).
	enBmsReadTimeout = 5 * time.Second
)

// enbmsConn — установленное BLE-соединение с BMS EnBMS. Соединение НЕ
// закрывается между опросами: пулер держит его открытым и переустанавливает
// при обрыве.
type enbmsConn struct {
	adapter *bluetooth.Adapter
	dev     bluetooth.Device
	tx      bluetooth.DeviceCharacteristic // ff02 — запись команд
	rx      bluetooth.DeviceCharacteristic // ff01 — уведомления/ответы

	// ctx — контекст пулера: прерывает in-flight чтение при остановке сервиса,
	// чтобы shutdown не блокировался на таймауте и успел закрыть соединение.
	ctx context.Context

	mu  sync.Mutex
	buf []byte        // накопитель notify-фрагментов
	got chan struct{} // сигнал «пришли новые данные»
}

// openEnBms подключается к BMS по MAC и открывает характеристики ff01/ff02.
// Сначала — прямой Connect (устройство уже «известно» BlueZ); при «залипшем»
// состоянии bluetoothd состояние сбрасывается и попытка повторяется; если
// устройства нет в BlueZ — короткий discovery и повторный Connect.
func openEnBms(mac string, ctx context.Context) (*enbmsConn, error) {
	// Питание BLE-адаптера (и перенацеливание tinygo на реальный контроллер)
	// переиспользуем из модуля CE308 — эти хелперы не CE308-специфичны.
	_ = ce308EnsurePowered()
	a := btDefaultAdapter()
	if err := a.Enable(); err != nil {
		return nil, fmt.Errorf("включение BLE-адаптера: %w", err)
	}

	c, err := connectEnBmsBounded(mac, ctx)
	if err == nil {
		return c, nil
	}
	if isCE308ConnStuck(err) {
		ce308ClearStuck(mac)
		if c2, err2 := connectEnBmsBounded(mac, ctx); err2 == nil {
			return c2, nil
		}
	}
	if ce308DeviceKnown(mac) {
		return nil, err
	}
	if e := ensureCE308Known(mac); e != nil {
		return nil, fmt.Errorf("подключение к %s: %v; устройство не обнаружено по BLE: %w", mac, err, e)
	}
	return connectEnBmsBounded(mac, ctx)
}

// connectEnBmsBounded выполняет connectEnBms с ограничением по времени (см.
// enBmsConnectTimeout). connectEnBms вызывается без дедлайна (BlueZ), поэтому по
// таймауту/отмене мы его не ждём, но соединение не должно утечь: горутина всегда
// кладёт результат в буферизованный канал (cap=1), а фоновый «сторож» дожидается
// результата и закрывает соединение, если оно уже не нужно.
func connectEnBmsBounded(mac string, ctx context.Context) (*enbmsConn, error) {
	type res struct {
		c   *enbmsConn
		err error
	}
	ch := make(chan res, 1)
	go func() {
		c, err := connectEnBms(mac, ctx)
		ch <- res{c, err} // cap=1 — отправка не блокируется
	}()
	drain := func() {
		go func() {
			r := <-ch
			if r.c != nil {
				_ = r.c.Close()
			}
		}()
	}
	select {
	case r := <-ch:
		return r.c, r.err
	case <-ctx.Done():
		drain()
		return nil, ctx.Err()
	case <-time.After(enBmsConnectTimeout):
		drain()
		return nil, fmt.Errorf("подключение к %s: превышено %s", mac, enBmsConnectTimeout)
	}
}

// connectEnBms подключается по MAC и открывает характеристики ff02 (запись) и
// ff01 (notify). При ошибке гарантированно разрывает уже установленное соединение.
func connectEnBms(mac string, ctx context.Context) (c *enbmsConn, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	a := btDefaultAdapter()
	mac6, err := bluetooth.ParseMAC(mac)
	if err != nil {
		return nil, fmt.Errorf("неверный MAC %q: %w", mac, err)
	}
	addr := bluetooth.Address{MACAddress: bluetooth.MACAddress{MAC: mac6}}
	dev, err := a.Connect(addr, bluetooth.ConnectionParams{})
	if err != nil {
		return nil, fmt.Errorf("подключение к %s: %w", mac, err)
	}
	defer func() {
		if err != nil {
			_ = dev.Disconnect()
		}
	}()

	svcU, err := bluetooth.ParseUUID(enBmsSvcUUID)
	if err != nil {
		return nil, err
	}
	svcs, err := dev.DiscoverServices([]bluetooth.UUID{svcU})
	if err != nil {
		return nil, fmt.Errorf("поиск сервиса: %w", err)
	}
	if len(svcs) == 0 {
		return nil, errors.New("сервис 0000ff00 не найден")
	}
	wU, err := bluetooth.ParseUUID(enBmsWriteUUID)
	if err != nil {
		return nil, err
	}
	nU, err := bluetooth.ParseUUID(enBmsNotifyUUID)
	if err != nil {
		return nil, err
	}
	chars, err := svcs[0].DiscoverCharacteristics([]bluetooth.UUID{wU, nU})
	if err != nil {
		return nil, fmt.Errorf("поиск характеристик: %w", err)
	}
	if len(chars) < 2 {
		return nil, errors.New("характеристики ff01/ff02 не найдены")
	}
	var tx, rx bluetooth.DeviceCharacteristic
	for _, ch := range chars {
		switch ch.UUID() {
		case wU:
			tx = ch
		case nU:
			rx = ch
		}
	}
	// Консервативно: первый — запись, второй — notify (порядок в массиве
	// соответствует запросу wU,nU), если сопоставление по UUID не удалось.
	if tx.UUID() != wU {
		tx = chars[0]
	}
	if rx.UUID() != nU {
		rx = chars[1]
	}

	c = &enbmsConn{adapter: a, dev: dev, tx: tx, rx: rx, ctx: ctx, got: make(chan struct{}, 1)}
	if err := (&c.rx).EnableNotifications(c.onNotify); err != nil {
		return nil, fmt.Errorf("включение нотификаций: %w", err)
	}
	return c, nil
}

// onNotify накапливает пришедшие фрагменты и будит читателя.
func (c *enbmsConn) onNotify(buf []byte) {
	if len(buf) == 0 {
		return
	}
	c.mu.Lock()
	c.buf = append(c.buf, buf...)
	c.mu.Unlock()
	select {
	case c.got <- struct{}{}:
	default:
	}
}

// Close разрывает соединение (результат фиксируется для диагностики остановки).
func (c *enbmsConn) Close() error {
	if c == nil {
		return nil
	}
	return c.dev.Disconnect()
}

// reset очищает накопитель и возможную «позднюю» нотификацию перед запросом.
func (c *enbmsConn) reset() {
	c.mu.Lock()
	c.buf = nil
	c.mu.Unlock()
	select {
	case <-c.got:
	default:
	}
}

// request отправляет кадр с командой cid2 и возвращает payload первого
// полученного кадра с тем же cid2 (по длина поля LEN). Прерывается по контексту
// (errEnBmsClosed) и по таймауту (errEnBmsReadTimeout).
func (c *enbmsConn) request(cid2 byte, info []byte) ([]byte, error) {
	c.reset()
	if c.ctx != nil {
		select {
		case <-c.ctx.Done():
			return nil, errEnBmsClosed
		default:
		}
	}
	frame := buildEnBmsFrame(cid2, info)
	if _, err := c.tx.WriteWithoutResponse(frame); err != nil {
		return nil, fmt.Errorf("запись запроса 0x%02x: %w", cid2, err)
	}
	deadline := time.NewTimer(enBmsReadTimeout)
	defer deadline.Stop()
	for {
		c.mu.Lock()
		fr, rest, ok := extractEnBmsFrame(c.buf)
		if ok {
			c.buf = rest
		} else if rest == nil {
			c.buf = nil
		} else {
			c.buf = rest
		}
		c.mu.Unlock()
		if ok {
			if fr[3] != cid2 {
				continue // чужой/запоздалый кадр — ждём нужный
			}
			if fr[4] != 0 {
				// RTN — код ошибки BMS (как в RS485-ветке): разбирать payload нельзя.
				return nil, fmt.Errorf("ответ BMS: RTN=0x%02X", fr[4])
			}
			// Формат ответа: 7E 14 ADR CID2 RTN LEN(2) INFO … — LEN по смещению
			// 5, INFO с 7 (проверено по живым кадрам).
			lenid := int(fr[5])<<8 | int(fr[6])
			out := make([]byte, lenid)
			copy(out, fr[7:7+lenid])
			return out, nil
		}
		select {
		case <-c.got:
		case <-deadline.C:
			return nil, errEnBmsReadTimeout
		case <-c.ctx.Done():
			return nil, errEnBmsClosed
		}
	}
}

// readEnBmsBattery читает блок Battery (CID2 0x61) и возвращает payload.
func (c *enbmsConn) readEnBmsBattery() ([]byte, error) {
	return c.request(enBmsCID2Battery, []byte{0x00})
}

// readEnBmsBasicInfo читает блок BasicInfo (CID2 0x51): модель/активный протокол.
func (c *enbmsConn) readEnBmsBasicInfo() ([]byte, error) {
	return c.request(0x51, nil)
}

// readEnBmsState — по BLE блок TeleState (0x44) недоступен (см.
// enbmsLink.readEnBmsState); состояния приходят в хвосте Battery (0x61).
func (c *enbmsConn) readEnBmsState() ([]byte, error) {
	return nil, errEnBmsStateUnsupported
}

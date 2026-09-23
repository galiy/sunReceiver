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
	"bytes"
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"tinygo.org/x/bluetooth"
)

// BLE-транспорт счётчика Энергомера СЕ308 (СЕ208): GATT-сервис b91b0100,
// запись команд в …0105 (write without response), ответ — нотификация на …0105
// с числом фрагментов и чтение характеристик-«фрагментов» …0106…0114.
// Полное описание протокола — ../ce308/docs/PROTOCOL.md; рабочий каркас —
// ../ce308/gui/meter.go (tinygo.org/x/bluetooth, BlueZ через D-Bus).

const (
	ce308SvcUUID = "b91b0100-8bef-45e2-97c3-1cd862d914df"
	ce308TxUUID  = "b91b0105-8bef-45e2-97c3-1cd862d914df"
)

// errCE308ReadTimeout — признак таймаута ожидания ответа счётчика (транзиентный
// сбой радио): отличается от ошибки записи/транспорта, при которой соединение,
// вероятно, оборвано. Позволяет пулеру ретраить именно упавшее чтение, не рвя
// постоянное соединение (см. readCE308WithRetries).
var errCE308ReadTimeout = errors.New("нет ответа счётчика по BLE (таймаут)")

// errCE308Closed — признак, что чтение прервано намеренно из-за отмены контекста
// (остановка сервиса): пулер должен немедленно выйти и корректно закрыть BLE-соединение,
// а не дожидаться исчерпания таймаута чтения (END-команды до 45 с) и попасть под
// SIGKILL с полу-открытой связью.
var errCE308Closed = errors.New("чтение прервано (остановка сервиса)")

// isCE308ReadTimeout — true, если ошибка является таймаутом чтения.
func isCE308ReadTimeout(err error) bool {
	return errors.Is(err, errCE308ReadTimeout)
}

// isCE308Closed — true, если ошибка — намеренное прерывание чтения при остановке
// (проходит сквозь %w-обёртки buildCE308Reads/readCE308Energy).
func isCE308Closed(err error) bool {
	return errors.Is(err, errCE308Closed)
}

var ce308RxUUIDs = []string{
	"b91b0106-8bef-45e2-97c3-1cd862d914df",
	"b91b0107-8bef-45e2-97c3-1cd862d914df",
	"b91b0108-8bef-45e2-97c3-1cd862d914df",
	"b91b0109-8bef-45e2-97c3-1cd862d914df",
	"b91b010a-8bef-45e2-97c3-1cd862d914df",
	"b91b010b-8bef-45e2-97c3-1cd862d914df",
	"b91b010c-8bef-45e2-97c3-1cd862d914df",
	"b91b010d-8bef-45e2-97c3-1cd862d914df",
	"b91b010e-8bef-45e2-97c3-1cd862d914df",
	"b91b010f-8bef-45e2-97c3-1cd862d914df",
	"b91b0110-8bef-45e2-97c3-1cd862d914df",
	"b91b0111-8bef-45e2-97c3-1cd862d914df",
	"b91b0112-8bef-45e2-97c3-1cd862d914df",
	"b91b0113-8bef-45e2-97c3-1cd862d914df",
	"b91b0114-8bef-45e2-97c3-1cd862d914df",
}

// ce308Meter — установленное BLE-соединение со счётчиком и обмен кадрами
// IEC 61107-совместимыми ASCII-командами. Соединение НЕ закрывается между
// опросами: поток держит его открытым и переустанавливает при обрыве.
type ce308Meter struct {
	adapter *bluetooth.Adapter
	dev     bluetooth.Device
	tx      bluetooth.DeviceCharacteristic
	rx      []bluetooth.DeviceCharacteristic
	mtu     int

	// ctx — контекст пулера (отменяется при остановке сервиса). Позволяет
	// прервать in-flight чтение (см. Read), чтобы shutdown не блокировался на
	// долгом BLE-чтении и успел корректно закрыть соединение.
	ctx context.Context

	mu     sync.Mutex
	notify chan struct{}
	frags  int
}

// openCE308 подключается к счётчику по MAC и открывает нужные характеристики.
// Перед подключением регистрируется BlueZ-агент для ответа на запрос PIN при
// первом спаривании (см. ce308_agent_linux.go / ce308_agent_other.go).
//
// Сначала пробуем ПРЯМОЙ Connect (скан не нужен, если BlueZ уже «знает» устройство —
// оно спарено, объект /org/bluez/<hci>/dev_* существует). Только если прямой Connect
// не удался И устройства нет в BlueZ — как fallback запускаем короткий discovery и
// повторяем Connect. Discovery больше не является обязательным предусловием.
func openCE308(mac string, pin string, ctx context.Context) (*ce308Meter, error) {
	if err := registerCE308Agent(pin); err != nil {
		// Не фатально: если устройство уже спарено с хостом, агент не нужен.
		// Логируем и продолжаем — лишний вывод раз в подключение приемлем.
		logCE308("bluez agent: %v", err)
	}
	// Питание BLE-адаптера проверяем программно и при необходимости включаем
	// (BlueZ Powered). На Linux также перенацеливает tinygo на реальный
	// контроллер, если тот не hci0 (перенумерация USB-адаптера). Ошибка здесь
	// не фатальна: итоговая причина уйдёт в отчёт подключения (openCE308
	// возвращает ошибку Enable/Connect), который логируется с троттлингом.
	_ = ce308EnsurePowered()
	a := bluetooth.DefaultAdapter
	if err := a.Enable(); err != nil {
		return nil, fmt.Errorf("включение BLE-адаптера: %w", err)
	}

	// Попытка 1: прямой Connect (быстрый путь при известном/спаренном устройстве).
	m, err := connectCE308(mac, ctx)
	if err == nil {
		return m, nil
	}
	// Устройство уже известно BlueZ — повторный Connect бессмыслен (именно эта
	// ошибка и есть причина, напр. зависший радиоадаптер), возвращаем её.
	if ce308DeviceKnown(mac) {
		return nil, err
	}
	// Fallback: устройства нет в BlueZ (нет объекта) — прямой Connect не проходит.
	// Запускаем короткий discovery, чтобы BlueZ узнал устройство, и пробуем снова.
	if e := ensureCE308Known(mac); e != nil {
		return nil, fmt.Errorf("подключение к %s: %v; устройство не обнаружено по BLE: %w", mac, err, e)
	}
	return connectCE308(mac, ctx)
}

// connectCE308 выполняет подключение по MAC и открывает нужные GATT-характеристики.
// Общая часть для прямого подключения и повтора после discovery. При ошибке
// гарантированно разрывает уже установленное соединение.
func connectCE308(mac string, ctx context.Context) (m *ce308Meter, err error) {
	// Ниль-контекст отключает прерывание чтений (Done() возвращает nil-канал —
	// ветка в select не сработает), но не даёт паники на m.ctx.Done().
	if ctx == nil {
		ctx = context.Background()
	}
	a := bluetooth.DefaultAdapter
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

	svcU, err := bluetooth.ParseUUID(ce308SvcUUID)
	if err != nil {
		return nil, err
	}
	svcs, err := dev.DiscoverServices([]bluetooth.UUID{svcU})
	if err != nil {
		return nil, fmt.Errorf("поиск сервиса: %w", err)
	}
	if len(svcs) == 0 {
		return nil, errors.New("сервис b91b0100 не найден")
	}

	uuids := make([]bluetooth.UUID, 0, 1+len(ce308RxUUIDs))
	for _, s := range append([]string{ce308TxUUID}, ce308RxUUIDs...) {
		u, err := bluetooth.ParseUUID(s)
		if err != nil {
			return nil, err
		}
		uuids = append(uuids, u)
	}
	chars, err := svcs[0].DiscoverCharacteristics(uuids)
	if err != nil {
		return nil, fmt.Errorf("поиск характеристик: %w", err)
	}

	m = &ce308Meter{adapter: a, dev: dev, tx: chars[0], rx: chars[1:], mtu: 23, ctx: ctx, notify: make(chan struct{}, 1)}
	if mtu, err2 := m.tx.GetMTU(); err2 == nil && mtu > 23 {
		m.mtu = int(mtu)
	}
	if err := (&m.tx).EnableNotifications(m.onNotify); err != nil {
		return nil, fmt.Errorf("включение нотификаций: %w", err)
	}
	return m, nil
}

func (m *ce308Meter) onNotify(buf []byte) {
	if len(buf) == 0 {
		return
	}
	m.mu.Lock()
	m.frags = int(buf[0])
	m.mu.Unlock()
	select {
	case m.notify <- struct{}{}:
	default:
	}
}

// Close разрывает соединение и возвращает ошибку (для диагностики при остановке
// сервиса: неуспешный Disconnect оставляет полу-открытую связь на адаптере).
func (m *ce308Meter) Close() error {
	if m == nil {
		return nil
	}
	return m.dev.Disconnect()
}

// reset сбрасывает состояние приёма перед чтением: обнуляет число ожидаемых
// фрагментов и вычищает возможную «позднюю» нотификацию (см. Read).
func (m *ce308Meter) reset() {
	m.mu.Lock()
	m.frags = 0
	m.mu.Unlock()
	select {
	case <-m.notify:
	default:
	}
}

// Read отправляет команду и возвращает ответ строкой (без служебных байт).
// Чтение прерывается при отмене контекста (errCE308Closed), чтобы при остановке
// сервиса не ждать исчерпания таймаута (END-команды до 45 с) и успеть корректно
// закрыть соединение (см. Close).
func (m *ce308Meter) Read(cmd string) (string, error) {
	// Сброс состояния перед каждым чтением: после таймаута счётчик может задержать
	// нотификацию, которая «всплывёт» позже и подставится под следующую команду
	// (неверный frags/ответ). Дренируем канал и обнуляем счётчик фрагментов.
	m.reset()
	if m.ctx != nil {
		select {
		case <-m.ctx.Done():
			return "", errCE308Closed
		default:
		}
	}
	frame := buildCE308Frame(cmd)
	for _, pkt := range ce308Fragments(frame, m.mtu) {
		if _, err := m.tx.WriteWithoutResponse(pkt); err != nil {
			return "", fmt.Errorf("запись %q: %w", cmd, err)
		}
	}
	select {
	case <-m.notify:
	case <-time.After(ce308TimeoutFor(cmd)):
		return "", fmt.Errorf("%s: %w", cmd, errCE308ReadTimeout)
	case <-m.ctx.Done():
		return "", errCE308Closed
	}

	m.mu.Lock()
	need := m.frags + 1
	m.mu.Unlock()
	if need > len(m.rx) {
		need = len(m.rx)
	}
	var raw []byte
	for i := 0; i < need; i++ {
		buf := make([]byte, 512)
		n, err := m.rx[i].Read(buf)
		if err != nil {
			break
		}
		raw = append(raw, buf[:n]...)
	}
	out := make([]byte, 0, len(raw))
	for _, b := range raw {
		out = append(out, b&0x7F)
	}
	if i := bytes.IndexByte(out, 0x03); i >= 0 { // обрезаем по ETX
		out = out[:i+1]
	}
	return string(out), nil
}

func buildCE308Frame(cmd string) []byte {
	body := append([]byte("/?!\x01R1\x02"), []byte(cmd)...)
	body = append(body, 0x03)
	sum := 0
	for _, b := range body[4:] {
		sum += int(b)
	}
	body = append(body, byte(sum&0x7F))
	for i := range body {
		body[i] = applyCE308Parity(body[i])
	}
	return body
}

func applyCE308Parity(v byte) byte {
	v &= 0x7F
	n := 0
	for i := 0; i < 7; i++ {
		n += int(v>>uint(i)) & 1
	}
	if n&1 == 1 {
		return v | 0x80
	}
	return v
}

func ce308Fragments(frame []byte, mtu int) [][]byte {
	maxPayload := 16
	if mtu > 23 {
		maxPayload = mtu - 4
	}
	var out [][]byte
	seq := 0
	first := true
	for i := 0; i < len(frame); {
		end := i + maxPayload
		if end > len(frame) {
			end = len(frame)
		}
		chunk := frame[i:end]
		i = end
		more := i < len(frame)
		var ctrl byte
		if first {
			if more {
				ctrl = 0x00
			} else {
				ctrl = 0x80
				if mtu > 23 {
					ctrl |= 0x40
				}
			}
			first = false
		} else {
			seq = (seq + 1) & 0x7F
			ctrl = byte(seq)
			if !more {
				ctrl |= 0x80
			}
		}
		out = append(out, append([]byte{ctrl}, chunk...))
	}
	return out
}

// ce308TimeoutFor — лимит ожидания ответа по классу запроса (замеры в
// ../ce308/docs/READ-RESULTS.md): короткие ~1 c, списки ~5 c, архивы END ~26 c.
func ce308TimeoutFor(cmd string) time.Duration {
	u := strings.ToUpper(cmd)
	for _, p := range []string{"END", "EMM", "EMY", "EME", "EMD", "GETAR", "SYMON"} {
		if strings.HasPrefix(u, p) {
			return 45 * time.Second
		}
	}
	for _, p := range []string{"LST", "LSU", "LSL", "GRF", "SESON", "EXFIX"} {
		if strings.HasPrefix(u, p) {
			return 12 * time.Second
		}
	}
	return 5 * time.Second
}

// ce308Groups возвращает все значения в круглых скобках ответа.
func ce308Groups(s string) []string {
	var out []string
	for {
		a := strings.IndexByte(s, '(')
		if a < 0 {
			return out
		}
		b := strings.IndexByte(s[a:], ')')
		if b < 0 {
			return out
		}
		out = append(out, s[a+1:a+b])
		s = s[a+b+1:]
	}
}

// ce308Floats парсит группы как числа; нечисловые группы дают 0.
func ce308Floats(g []string) []float64 {
	out := make([]float64, 0, len(g))
	for _, x := range g {
		v, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
		if err != nil {
			out = append(out, 0)
			continue
		}
		out = append(out, v)
	}
	return out
}

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

//go:build linux

package main

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"
)

// Счётчик Энергомера при первом спаривании запрашивает passkey (BLE-PIN) через
// BlueZ-агента. tinygo.org/x/bluetooth сам агента не регистрирует, поэтому
// регистрируем свой org.bluez.Agent1 (capability KeyboardDisplay), который
// отвечает на запрос PIN значением из конфига (ce308.pin). После успешного
// спаривания ключи bonding сохраняются в BlueZ и агент больше не нужен.
var (
	ce308RegMu  sync.Mutex
	ce308RegPin string
)

// ce308BlueZAgent — реализация org.bluez.Agent1; всегда подтверждает/отвечает
// PIN, т.е. автоматически принимает сопряжение с настроенным счётчиком.
type ce308BlueZAgent struct {
	pin uint32
}

func (a *ce308BlueZAgent) Release() error { return nil }

func (a *ce308BlueZAgent) RequestPinCode(device dbus.ObjectPath) (string, *dbus.Error) {
	return fmt.Sprintf("%06d", a.pin), nil
}

func (a *ce308BlueZAgent) RequestPasskey(device dbus.ObjectPath) (uint32, *dbus.Error) {
	return a.pin, nil
}

func (a *ce308BlueZAgent) DisplayPasskey(device dbus.ObjectPath, passkey uint32, entered uint16) *dbus.Error {
	return nil
}

func (a *ce308BlueZAgent) RequestConfirmation(device dbus.ObjectPath, passkey uint32) (bool, *dbus.Error) {
	return true, nil
}

func (a *ce308BlueZAgent) RequestAuthorization(device dbus.ObjectPath) (bool, *dbus.Error) {
	return true, nil
}

func (a *ce308BlueZAgent) AuthorizeService(device dbus.ObjectPath, uuid string) (bool, *dbus.Error) {
	return true, nil
}

func (a *ce308BlueZAgent) Cancel() error { return nil }

// ensureCE308Known (Linux) гарантирует, что BlueZ «знает» объект устройства по
// MAC: без предварительного discovery tinygo Connect падает — объект
// /org/bluez/hci0/dev_* отсутствует, и Properties.Get даёт UnknownMethod.
// Если объект уже известен (устройство спарено ранее) — return nil. Иначе
// запускаем короткий discovery и ждём появления устройства.
func ensureCE308Known(mac string) error {
	bus, err := dbus.SystemBus()
	if err != nil {
		return fmt.Errorf("system bus: %w", err)
	}
	devPath := dbus.ObjectPath("/org/bluez/hci0/dev_" + strings.Replace(strings.ToUpper(mac), ":", "_", -1))
	known := func() bool {
		var v bool
		e := bus.Object("org.bluez", devPath).
			Call("org.freedesktop.DBus.Properties.Get", 0, "org.bluez.Device1", "Connected").Store(&v)
		return e == nil
	}
	if known() {
		return nil
	}
	adapter := bus.Object("org.bluez", dbus.ObjectPath("/org/bluez/hci0"))
	// Сбрасываем возможное зависшее discovery и запускаем новое.
	_ = adapter.Call("org.bluez.Adapter1.StopDiscovery", 0).Err
	if err := adapter.Call("org.bluez.Adapter1.StartDiscovery", 0).Err; err != nil {
		// InProgress (discovery уже запущен) — не ошибка, ждём устройство.
		logCE308("discovery: %v", err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if known() {
			_ = adapter.Call("org.bluez.Adapter1.StopDiscovery", 0).Err
			return nil
		}
		time.Sleep(time.Second)
	}
	_ = adapter.Call("org.bluez.Adapter1.StopDiscovery", 0).Err
	return fmt.Errorf("устройство %s не обнаружено по BLE (не рекламируется)", mac)
}

// registerCE308Agent регистрирует BlueZ-агента с PIN (идемпотентно) на системной
// шине и делает его агентом по умолчанию. Ошибка не фатальна: если счётчик уже
// спарен, агент не требуется. pincode может быть строкой — приводим к uint32.
func registerCE308Agent(pin string) error {
	ce308RegMu.Lock()
	defer ce308RegMu.Unlock()
	if ce308RegPin == pin && ce308RegPin != "" {
		return nil

	}
	ce308RegPin = pin
	n, err := strconv.ParseUint(pin, 10, 32)
	if err != nil || n == 0 {
		return fmt.Errorf("некорректный pin %q", pin)
	}
	conn, err := dbus.SystemBus()
	if err != nil {
		return fmt.Errorf("system bus: %w", err)
	}
	path := dbus.ObjectPath("/org/bluez/agentCE308")
	agent := &ce308BlueZAgent{pin: uint32(n)}
	if err := conn.Export(agent, path, "org.bluez.Agent1"); err != nil {
		return fmt.Errorf("export agent: %w", err)
	}
	manager := conn.Object("org.bluez", dbus.ObjectPath("/org/bluez"))
	call := manager.Call("org.bluez.AgentManager1.RegisterAgent", 0, path, "KeyboardDisplay")
	if call.Err != nil {
		return fmt.Errorf("register agent: %w", call.Err)
	}
	call = manager.Call("org.bluez.AgentManager1.RequestDefaultAgent", 0, path)
	if call.Err != nil {
		return fmt.Errorf("request default agent: %w", call.Err)
	}
	return nil
}

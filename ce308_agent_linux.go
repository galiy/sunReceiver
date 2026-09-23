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
	"sync"

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

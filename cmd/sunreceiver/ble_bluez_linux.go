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
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/godbus/dbus/v5"
)

// bluezPreStartNotify заранее включает нотификации через BlueZ с таймаутом.
//
// Зачем: tinygo.org/x/bluetooth на Linux в EnableNotifications сначала
// регистрирует signal-канал в общем D-Bus, а затем вызывает
// GattCharacteristic1.StartNotify без таймаута. Если StartNotify зависает
// (бывает при «полуживом» bluetoothd/связи), горутина остаётся внутри
// библиотеки навсегда: канал уже зарегистрирован, но не читается, и каждый
// PropertiesChanged спавнит вечный deferredDeliver — утечка памяти до OOM.
//
// Предварительный вызов с контекстом гарантирует, что к моменту tinygo-вызова
// StartNotify уже выполнен (повторный вызов идемпотентен) и не зависнет. Если
// предварительный вызов не удался/не уложился в таймаут, мы вообще не трогаем
// tinygo-подписку — значит, и утечки нет.
func bluezPreStartNotify(mac, charUUID string, timeout time.Duration) error {
	bus, err := dbus.SystemBus()
	if err != nil {
		return fmt.Errorf("system bus: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	var objects map[dbus.ObjectPath]map[string]map[string]dbus.Variant
	root := bus.Object("org.bluez", dbus.ObjectPath("/"))
	if err := root.CallWithContext(ctx, "org.freedesktop.DBus.ObjectManager.GetManagedObjects", 0).Store(&objects); err != nil {
		return fmt.Errorf("GetManagedObjects: %w", err)
	}

	devSuffix := strings.ToUpper("dev_" + strings.ReplaceAll(mac, ":", "_"))
	for path, ifaces := range objects {
		if !strings.Contains(strings.ToUpper(string(path)), devSuffix) {
			continue
		}
		props, ok := ifaces["org.bluez.GattCharacteristic1"]
		if !ok {
			continue
		}
		u, _ := props["UUID"].Value().(string)
		if !strings.EqualFold(strings.TrimSpace(u), strings.TrimSpace(charUUID)) {
			continue
		}
		obj := bus.Object("org.bluez", path)
		if err := obj.CallWithContext(ctx, "org.bluez.GattCharacteristic1.StartNotify", 0).Err; err != nil {
			return fmt.Errorf("StartNotify %s/%s: %w", mac, charUUID, err)
		}
		return nil
	}
	return fmt.Errorf("характеристика %s у %s не найдена в BlueZ", charUUID, mac)
}

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

//go:build !linux

package main

// На платформах без BlueZ регистрация агента для PIN не нужна: предполагается,
// что счётчик уже спарен с хостом (bonding сохранён в стеке ОС). Возвращает nil.
func registerCE308Agent(pin string) error {
	return nil
}

// ensureCE308Known — no-op вне Linux (нет BlueZ/discovery; BlueZ-бэкенд tinygo
// доступен только там). Возвращает nil.
func ensureCE308Known(mac string) error {
	return nil
}

// ce308DeviceKnown — вне Linux считаем устройство известным (нет discovery; ОС-стек
// Bluetooth подключается напрямую по MAC). Возвращает true.
func ce308DeviceKnown(mac string) bool {
	return true
}

// ce308EnsurePowered — no-op вне Linux: питание адаптера управляется ОС
// (Windows) или стеком Bluetooth (macOS). Возвращает nil.
func ce308EnsurePowered() error {
	return nil
}

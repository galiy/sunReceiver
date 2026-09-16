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

//go:build !windows

package main

import (
	"os"
	"os/signal"
	"sync"
	"syscall"
)

// quitOnce гарантирует идемпотентное завершение: на POSIX писатель в quit один
// (SIGINT/SIGTERM), но единая точка signalQuit сохраняет симметрию с Windows.
var quitOnce sync.Once

// signalQuit инициирует graceful-завершение один раз через канал quit.
func signalQuit(quit chan struct{}) {
	quitOnce.Do(func() { quit <- struct{}{} })
}

// runTray на POSIX (Linux/macOS) ничего не делает: приложение работает как
// обычный процесс (systemd/консоль), трея нет.
func runTray(quit chan struct{}) {}

// waitForQuit возвращает канал, закрываемый при SIGINT/SIGTERM — обычное
// graceful-завершение демона на Linux.
func waitForQuit(quit chan struct{}) <-chan struct{} {
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		signalQuit(quit)
	}()
	return quit
}

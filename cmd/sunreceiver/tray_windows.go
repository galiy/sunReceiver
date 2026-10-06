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

//go:build windows

package main

import (
	_ "embed"
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"fyne.io/systray"
)

// quitOnce гарантирует идемпотентное завершение: и пункт меню «Закрыть», и
// SIGINT/SIGTERM посылают в quit ровно один раз (иначе второй писатель остаётся
// невостребованным и считается утечкой).
var quitOnce sync.Once

// signalQuit инициирует graceful-завершение один раз через канал quit.
func signalQuit(quit chan struct{}) {
	quitOnce.Do(func() { quit <- struct{}{} })
}

//go:embed tray.ico
var trayIcon []byte

// runTray на Windows сворачивает приложение в системный трей. В меню — только
// пункт «Закрыть»: он инициирует graceful-завершение (запись в канал quit).
func runTray(quit chan struct{}) {
	go func() {
		// Неблокирующий фоновый цикл с фиксированной иконкой.
		systray.Run(onReady(quit), nil)
	}()
}

// onReady возвращает колбэк инициализации трея: заголовок, подсказка и меню.
func onReady(quit chan struct{}) func() {
	return func() {
		systray.SetIcon(trayIcon)
		systray.SetTitle("sunReceiver")
		systray.SetTooltip("sunReceiver — мониторинг солнечной станции")
		mQuit := systray.AddMenuItem("Закрыть", "Завершить sunReceiver")
		go func() {
			<-mQuit.ClickedCh
			log.Println("tray: Закрыть — завершение работы")
			systray.Quit()
			signalQuit(quit)
		}()
	}
}

// waitForQuit возвращает канал, в который пишется при закрытии из трея либо по
// Ctrl+C (SIGINT/SIGTERM в консоли — если запущены вручную).
func waitForQuit(quit chan struct{}) <-chan struct{} {
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		signalQuit(quit)
	}()
	return quit
}

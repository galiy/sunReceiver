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
	"log"
	"os"
	"path/filepath"
)

// setupLogging на Windows пишет лог в файл sunReceiver.log рядом с исполняемым
// файлом (аналогично sunReceiver.json). Приложение сворачивается в трей без
// консоли, поэтому stderr не виден — журнал нужен для диагностики.
func setupLogging() {
	path := logFilePath()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		log.Printf("logging: не удалось открыть %s: %v (лог пойдёт в stderr)", path, err)
		return
	}
	log.SetOutput(f)
}

// logFilePath — путь к файлу журнала: рядом с исполняемым файлом (как конфиг).
func logFilePath() string {
	exe, err := os.Executable()
	if err != nil {
		return "sunReceiver.log"
	}
	return filepath.Join(filepath.Dir(exe), "sunReceiver.log")
}

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
	"io"
	"log"
	"os"
	"path/filepath"
)

// logMaxSize — порог ротации журнала: при превышении sunReceiver.log
// переименовывается в sunReceiver.log.1 и переоткрывается.
const logMaxSize = 10 * 1024 * 1024

// setupLogging на Windows пишет лог в файл sunReceiver.log. Сначала пробуется
// каталог рядом с исполняемым файлом (аналогично sunReceiver.json); если он
// недоступен (например, C:\Program Files\...) — fallback на %LOCALAPPDATA%\sunReceiver,
// затем на %TEMP%. Приложение сворачивается в трей без консоли, поэтому stderr
// не виден — журнал нужен для диагностики.
func setupLogging() {
	for _, path := range candidateLogPaths() {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
		if err != nil {
			continue
		}
		log.SetOutput(&rotatingWriter{path: path, f: f})
		return
	}
	log.Printf("logging: не удалось открыть файл журнала (лог пойдёт в stderr)")
}

// candidateLogPaths — кандидаты пути к журналу в порядке приоритета:
// рядом с exe → %LOCALAPPDATA%\sunReceiver → %TEMP%.
func candidateLogPaths() []string {
	var paths []string
	if exe, err := os.Executable(); err == nil {
		paths = append(paths, filepath.Join(filepath.Dir(exe), "sunReceiver.log"))
	}
	if local := os.Getenv("LOCALAPPDATA"); local != "" {
		dir := filepath.Join(local, "sunReceiver")
		if err := os.MkdirAll(dir, 0755); err == nil {
			paths = append(paths, filepath.Join(dir, "sunReceiver.log"))
		}
	}
	if cache, err := os.UserCacheDir(); err == nil {
		dir := filepath.Join(cache, "sunReceiver")
		if err := os.MkdirAll(dir, 0755); err == nil {
			paths = append(paths, filepath.Join(dir, "sunReceiver.log"))
		}
	}
	return append(paths, filepath.Join(os.TempDir(), "sunReceiver.log"))
}

// rotatingWriter — io.Writer поверх файла журнала с ротацией по размеру:
// перед записью проверяет текущий размер и при превышении logMaxSize
// переименовывает активный файл в sunReceiver.log.1 и переоткрывает новый.
type rotatingWriter struct {
	path string
	f    *os.File
}

func (w *rotatingWriter) Write(p []byte) (int, error) {
	if fi, err := w.f.Stat(); err == nil && fi.Size() >= logMaxSize {
		w.rotate()
	}
	return w.f.Write(p)
}

func (w *rotatingWriter) rotate() {
	w.f.Close()
	os.Remove(w.path + ".1")
	os.Rename(w.path, w.path+".1")
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		// Не удалось переоткрыть — ограничиваемся записью через stderr
		// (при трее не виден, но это краевой случай).
		w.f = os.Stderr
		return
	}
	w.f = f
}

var _ io.Writer = (*rotatingWriter)(nil)
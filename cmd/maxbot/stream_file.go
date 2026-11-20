// maxbot
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
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// maxStreamFileBytes — предел размера файла, принимаемого из Redis Stream.
// Большие файлы отправлять напрямую через Bot API (см. docs/modules/notify.md).
const maxStreamFileBytes = 10 << 20 // 10 MiB

// resolveStreamFile достаёт имя и содержимое файла из события Stream.
// Допустимо ровно одно из полей: file_b64 (base64) или file_path (внутри
// filesDir). file_name обязателен и санитизируется до базового имени.
func resolveStreamFile(ev notifyEvent, filesDir string, maxBytes int) (string, []byte, error) {
	name := strings.TrimSpace(ev.FileName)
	if name == "" {
		return "", nil, fmt.Errorf("не задано file_name")
	}
	// Только базовое имя: без каталогов и переходов вверх.
	base := filepath.Base(filepath.Clean(name))
	if base == "." || base == ".." || base == string(filepath.Separator) || base == "" {
		return "", nil, fmt.Errorf("некорректное file_name %q", ev.FileName)
	}
	hasB64 := ev.FileB64 != ""
	hasPath := ev.FilePath != ""
	if hasB64 == hasPath {
		return "", nil, fmt.Errorf("нужно ровно одно из file_b64/file_path")
	}
	if hasB64 {
		data, err := base64.StdEncoding.DecodeString(ev.FileB64)
		if err != nil {
			// допустим вариант без padding
			data, err = base64.RawStdEncoding.DecodeString(ev.FileB64)
			if err != nil {
				return "", nil, fmt.Errorf("file_b64: %w", err)
			}
		}
		if maxBytes > 0 && len(data) > maxBytes {
			return "", nil, fmt.Errorf("файл %d байт больше предела %d", len(data), maxBytes)
		}
		return base, data, nil
	}
	// file_path: строго внутри filesDir (с учётом симлинков).
	if strings.TrimSpace(filesDir) == "" {
		return "", nil, fmt.Errorf("file_path запрещён: files_dir не настроен")
	}
	dir, err := filepath.Abs(filesDir)
	if err != nil {
		return "", nil, fmt.Errorf("files_dir: %w", err)
	}
	dirReal, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", nil, fmt.Errorf("files_dir: %w", err)
	}
	target, err := filepath.Abs(filepath.Clean(ev.FilePath))
	if err != nil || !filepath.IsAbs(ev.FilePath) {
		return "", nil, fmt.Errorf("file_path должен быть абсолютным")
	}
	real, err := filepath.EvalSymlinks(target)
	if err != nil {
		return "", nil, fmt.Errorf("file_path: %w", err)
	}
	if real != dirReal && !strings.HasPrefix(real, dirReal+string(filepath.Separator)) {
		return "", nil, fmt.Errorf("file_path вне files_dir")
	}
	data, err := os.ReadFile(real)
	if err != nil {
		return "", nil, fmt.Errorf("file_path: %w", err)
	}
	if maxBytes > 0 && len(data) > maxBytes {
		return "", nil, fmt.Errorf("файл %d байт больше предела %d", len(data), maxBytes)
	}
	return base, data, nil
}

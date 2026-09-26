// map-settings — локальный конфиг программы.
// Copyright (C) 2026  Aleksandr Galinskii
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// configFileName — файл локальных настроек рядом с бинарником (в .gitignore).
const configFileName = "mapsettings.json"

// fileConfig — сохраняемые настройки: адрес/режим по умолчанию и параметры
// запуска. Значения из файла применяются, если не переопределены флагами.
type fileConfig struct {
	Listen string `json:"listen,omitempty"`
	Map    string `json:"map,omitempty"`
	Unit   int    `json:"unit,omitempty"`
	User   string `json:"user,omitempty"`
	Pass   string `json:"pass,omitempty"`
	Mode   string `json:"mode,omitempty"`
	IP     string `json:"ip,omitempty"`
	Port   int    `json:"port,omitempty"`
}

// configPath — mapsettings.json рядом с исполняемым файлом; при `go run` —
// в текущем каталоге.
func configPath() string {
	exe, err := os.Executable()
	if err != nil {
		return configFileName
	}
	dir := filepath.Dir(exe)
	if _, err := os.Stat(filepath.Join(dir, configFileName)); err == nil {
		return filepath.Join(dir, configFileName)
	}
	if wd, err := os.Getwd(); err == nil {
		return filepath.Join(wd, configFileName)
	}
	return filepath.Join(dir, configFileName)
}

// loadFileConfig читает конфиг; отсутствие файла — не ошибка (nil).
func loadFileConfig(path string) *fileConfig {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var c fileConfig
	if err := json.Unmarshal(b, &c); err != nil {
		return nil
	}
	return &c
}

// saveFileConfig сохраняет конфиг (best-effort, 0644).
func saveFileConfig(path string, c *fileConfig) error {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	return os.WriteFile(path, b, 0o644)
}

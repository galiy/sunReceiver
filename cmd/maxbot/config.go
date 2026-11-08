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
	"encoding/json"
	"fmt"
	"os"
)

// config — конфигурация maxbot (maxbot.json, приватный, рядом с бинарником).
// Содержит только настройки транспорта MAX плюс адрес Redis, откуда читается
// очередь уведомлений. Никаких настроек детекции МАП здесь нет — они остались в
// sunReceiver (раздел notify), который формирует готовые тексты событий.
type config struct {
	Token  string `json:"token"`   // токен бота MAX (обязателен)
	UserID string `json:"user_id"` // адресат — user_id (личный диалог)
	ChatID string `json:"chat_id"` // адресат — chat_id (чат/канал); приоритет user_id
	// Disabled — true: бот не запускается (конфиг есть, но отправка выключена).
	Disabled bool `json:"disabled"`
	// Redis — адрес Redis sunreceiver (host:port или путь к unix-сокету),
	// откуда читается Stream уведомлений.
	Redis string `json:"redis"`
	// Consumer — имя консьюмера в consumer group Stream (по умолчанию "maxbot").
	Consumer string `json:"consumer"`
	// AllowedUserIDs — белый список user_id (строки), которым разрешено стать
	// адресатом (регистрация первого подписчика). События подписки/сообщения от
	// аккаунтов вне списка отклоняются, адресат не меняется. Если список пуст —
	// регистрация запрещена для всех (адресат задаётся только явно в конфиге).
	AllowedUserIDs []string `json:"allowed_user_ids"`
}

// loadConfig читает и валидирует maxbot.json.
func loadConfig(path string) (*config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var c config
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if c.Token == "" {
		return nil, fmt.Errorf("config %s: не задан token", path)
	}
	if c.Redis == "" {
		return nil, fmt.Errorf("config %s: не задан redis", path)
	}
	if c.Consumer == "" {
		c.Consumer = "maxbot"
	}
	return &c, nil
}

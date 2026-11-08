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
	"net/url"
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

	// DSH — чат с DeepSeek Harness (dsh) на этом же хосте. nil — раздел не
	// задан, чат выключен (уведомления работают как раньше).
	DSH *dshConfig `json:"dsh"`
}

// dshConfig — настройки прокси-чата с DeepSeek Harness.
type dshConfig struct {
	// Disabled — true: чат с harness выключен (уведомления продолжают работать).
	Disabled bool `json:"disabled"`
	// BaseURL — HTTP-адрес harness (обычно http://127.0.0.1:3080).
	BaseURL string `json:"base_url"`
	// Workspace — рабочая область агента (cwd сессии).
	Workspace string `json:"workspace"`
	// IncomingDir — каталог для файлов, полученных из MAX.
	IncomingDir string `json:"incoming_dir"`
	// Provider/DefaultModel/Effort — модель по умолчанию.
	Provider     string `json:"provider"`
	DefaultModel string `json:"default_model"`
	Effort       string `json:"effort"`
	// ModelTTLHours — окно выбранной модели (сбрасывается после простоя).
	ModelTTLHours int `json:"model_ttl_hours"`
	// HeartbeatSec — период сообщения «Агент работает».
	HeartbeatSec int `json:"heartbeat_sec"`
	// RetryAttempts — число попыток при транспортных ошибках.
	RetryAttempts int `json:"retry_attempts"`
	// TokenUnit — systemd-юнит harness для чтения launch token из journald.
	TokenUnit string `json:"token_unit"`
}

// applyDefaults заполняет необязательные поля dsh-раздела.
func (d *dshConfig) applyDefaults() {
	if d.BaseURL == "" {
		d.BaseURL = "http://127.0.0.1:3080"
	}
	if d.Workspace == "" {
		d.Workspace = "/opt/dsh-bot"
	}
	if d.IncomingDir == "" {
		d.IncomingDir = d.Workspace + "/incoming"
	}
	if d.Provider == "" {
		d.Provider = "wbllm"
	}
	if d.DefaultModel == "" {
		d.DefaultModel = "coder-medium"
	}
	if d.Effort == "" {
		d.Effort = "low"
	}
	if d.ModelTTLHours <= 0 {
		d.ModelTTLHours = 2
	}
	if d.HeartbeatSec <= 0 || d.HeartbeatSec > 180 {
		// Требование: «Агент работает» не реже раза в 3 минуты.
		d.HeartbeatSec = 180
	}
	if d.RetryAttempts <= 0 {
		d.RetryAttempts = 3
	}
	if d.TokenUnit == "" {
		d.TokenUnit = "deepseek-harness"
	}
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
	if c.DSH != nil {
		c.DSH.applyDefaults()
		if !c.DSH.Disabled {
			u, err := url.Parse(c.DSH.BaseURL)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				return nil, fmt.Errorf("config %s: dsh.base_url должен быть http(s)://host[:port]", path)
			}
		}
	}
	return &c, nil
}

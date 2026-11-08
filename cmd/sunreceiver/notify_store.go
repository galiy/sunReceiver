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

package main

import (
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// Очередь уведомлений в MAX: Redis Stream, в которую sunReceiver пишет готовые
// тексты событий (одна запись = одно событие перехода авария/норма). Читает и
// отправляет внешнее приложение cmd/maxbot; после успешной отправки в MAX оно
// подтверждает запись (XACK) — и сообщение удаляется из Stream. Пока адресата у
// бота нет, события копятся, но их немного благодаря дедупликации в sunReceiver.
const (
	// redisNotifyStreamKey — ключ Stream уведомлений в MAX.
	redisNotifyStreamKey = "sunreceiver:notify:events"
	// redisNotifyStreamMaxLen — предельная длина Stream (приблизительный MAXLEN):
	// защита от бесконечного накопления неотправленных событий.
	redisNotifyStreamMaxLen = 1000
	// redisNotifyType — идентификатор источника события в записи Stream.
	redisNotifyType = "map"
)

// enqueueNotifyEvent пишет готовый текст уведомления в Redis Stream
// (одна запись: type="map", text, ts=RFC3339). Возвращает ошибку, если запись
// не удалась — в этом случае вызывающий НЕ подтверждает доставку детектору, и
// событие повторится на следующем такте (алерт не теряется при сбое Redis).
func (s *redisStore) enqueueNotifyEvent(msg string) error {
	if s == nil || s.rdb == nil {
		return fmt.Errorf("notify enqueue: хранилище не инициализировано")
	}
	id, err := s.rdb.XAdd(s.ctx, &redis.XAddArgs{
		Stream: redisNotifyStreamKey,
		MaxLen: redisNotifyStreamMaxLen,
		Approx: true,
		Values: map[string]any{
			"type": redisNotifyType,
			"text": msg,
			"ts":   time.Now().Format(time.RFC3339),
		},
	}).Result()
	if err != nil {
		return fmt.Errorf("notify enqueue: %w", err)
	}
	if id == "" {
		return fmt.Errorf("notify enqueue: пустой stream id")
	}
	return nil
}
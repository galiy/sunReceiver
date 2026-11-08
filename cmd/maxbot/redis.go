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
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// Имя Redis Stream очереди уведомлений (пишет sunReceiver, читает maxbot).
const streamKey = "sunreceiver:notify:events"

// openRedis подключается к Redis по адресу (host:port либо путь к unix-сокету —
// если начинается с "/"). Таймауты ограничивают зависшую операцию.
func openRedis(addr string) (*redis.Client, error) {
	opts := &redis.Options{
		ReadTimeout:  3 * time.Second,
		WriteTimeout: 2 * time.Second,
		Protocol:     2,
	}
	if strings.HasPrefix(addr, "/") {
		opts.Network = "unix"
		opts.Addr = addr
	} else {
		opts.Addr = addr
	}
	rdb := redis.NewClient(opts)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		rdb.Close()
		return nil, fmt.Errorf("redis ping %s: %w", addr, err)
	}
	return rdb, nil
}

// ensureGroup создаёт consumer group, если её ещё нет (XGroupCreateMkStream
// создаёт и сам ключ Stream, чтобы не было ошибки при первом чтении).
func ensureGroup(ctx context.Context, rdb *redis.Client, group string) error {
	err := rdb.XGroupCreateMkStream(ctx, streamKey, group, "0").Err()
	if err != nil &&
		strings.Contains(err.Error(), "BUSYGROUP") {
		return nil // группа уже существует
	}
	return err
}

// notifyEvent — содержимое одной записи Stream (поля type/text/ts).
type notifyEvent struct {
	ID   string
	Type string
	Text string
	Ts   string
}

// readNew читает новые (ещё не прочитанные) записи Stream XREADGROUP.
// Возвращает пустой список, если записей нет (в т.ч. после BLOCK-таймаута).
func readNew(ctx context.Context, rdb *redis.Client, group, consumer string, block time.Duration) ([]notifyEvent, error) {
	res, err := rdb.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group:    group,
		Consumer: consumer,
		Streams:  []string{streamKey, ">"},
		Count:    10,
		Block:    block,
	}).Result()
	if err == redis.Nil {
		return nil, nil // BLOCK-таймаут — пустой опрос, не ошибка
	}
	if err != nil {
		return nil, err
	}
	return flattenStreams(res), nil
}

// readPending читает незавершённые записи (PEL) данного консьюмера: те, что были
// прочитаны, но не подтверждены (XACK). Используется, чтобы после появления
// адресата отправить накопленные события, и после рестарта — дособрать pending.
// Возвращает записи, ещё НЕ подтверждённые; успешно отправленные подтверждаются
// вызывающим через ack.
func readPending(ctx context.Context, rdb *redis.Client, group, consumer string) ([]notifyEvent, error) {
	res, err := rdb.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group:    group,
		Consumer: consumer,
		Streams:  []string{streamKey, "0"},
		Count:    100,
	}).Result()
	if err == redis.Nil {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return flattenStreams(res), nil
}

// claimStale переназначает незавершённые записи, зависшие у ДРУГИХ консьюмеров
// (например, при смене имени/краше), на текущего. MinIdle исключает перехват
// активно обрабатываемых записей. Вызывается при старте.
func claimStale(ctx context.Context, rdb *redis.Client, group, consumer string) error {
	_, _, err := rdb.XAutoClaim(ctx, &redis.XAutoClaimArgs{
		Stream:   streamKey,
		Group:    group,
		Consumer: consumer,
		MinIdle:  5 * time.Minute,
		Start:    "0-0",
		Count:    100,
	}).Result()
	if err != nil && err != redis.Nil {
		return fmt.Errorf("xautoclaim: %w", err)
	}
	return nil
}

func flattenStreams(res []redis.XStream) []notifyEvent {
	var out []notifyEvent
	for _, s := range res {
		for _, m := range s.Messages {
			ev := notifyEvent{ID: m.ID}
			if v, ok := m.Values["type"].(string); ok {
				ev.Type = v
			}
			if v, ok := m.Values["text"].(string); ok {
				ev.Text = v
			}
			if v, ok := m.Values["ts"].(string); ok {
				ev.Ts = v
			}
			out = append(out, ev)
		}
	}
	return out
}

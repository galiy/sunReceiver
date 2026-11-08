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

// maxbot — отдельное приложение: читает очередь уведомлений из Redis Stream
// (в неё пишет sunReceiver готовые тексты событий МАП) и отправляет их в
// мессенджер MAX через Bot API. После успешной отправки подтверждает запись
// (XACK) — сообщение удаляется из Redis. Также сопровождает подписку бота:
// регистрирует первого подписчика (bot_started/bot_added) и сохраняет адресат в
// собственный конфиг maxbot.json.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os/signal"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"
)

// version подставляется через -ldflags "-X main.version=..." (см. Makefile).
var version = "dev"

// bot — состояние приложения maxbot.
type bot struct {
	cfg     *config
	cfgPath string
	client  *maxClient
	rdb     *redis.Client
	group   string // имя consumer group в Stream (по умолчанию = cfg.Consumer)
	// lastSendErrAt — время последней залогированной ошибки отправки (защита от
	// лог-спама при стойком отказе MAX; доступен только из основного цикла).
	lastSendErrAt time.Time
}

// main — точка входа: конфиг (флаг -config, по умолчанию maxbot.json рядом с
// бинарником), подключение к Redis, настройка consumer group, фоновый цикл
// подписки и основной цикл чтения Stream и отправки в MAX.
func main() {
	cfgPath := flag.String("config", "maxbot.json", "путь к конфигу maxbot.json")
	printVer := flag.Bool("version", false, "печать версии и выход")
	flag.Parse()

	if *printVer {
		fmt.Printf("maxbot %s\n", version)
		return
	}

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		log.Fatalf("maxbot: %v", err)
	}
	if cfg.Disabled {
		log.Printf("maxbot: disabled=true — бот не запускается")
		return
	}

	rdb, err := openRedis(cfg.Redis)
	if err != nil {
		log.Fatalf("maxbot: %v", err)
	}
	defer rdb.Close()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	group := cfg.Consumer
	if err := ensureGroup(ctx, rdb, group); err != nil {
		log.Fatalf("maxbot: создание consumer group %q: %v", group, err)
	}
	if err := claimStale(ctx, rdb, group, cfg.Consumer); err != nil {
		log.Printf("maxbot: переназначение зависших записей не удалось: %v", err)
	}

	b := &bot{
		cfg:     cfg,
		cfgPath: *cfgPath,
		client:  newMaxClient(cfg.Token, cfg.UserID, cfg.ChatID),
		rdb:     rdb,
		group:   cfg.Consumer,
	}
	if !b.client.hasRecipient() {
		log.Printf("maxbot: адресат не задан — ожидаю первого подписчика бота (bot_started/bot_added); накопленные события отправятся после его регистрации")
	}
	go b.runSubscriber(ctx)

	log.Printf("maxbot: запущен, consumer group %q, очередь %s", group, streamKey)
	b.run(ctx)
	log.Printf("maxbot: остановлен")
}

// run — основной цикл: читает новые записи Stream, отправляет в MAX и
// подтверждает (XACK). Пока адресата нет — не отправляет и не подтверждает
// (события копятся и уходят после регистрации подписчика). Периодически
// дособирает незавершённые (pending) записи этого же консьюмера.
func (b *bot) run(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		events, err := readNew(ctx, b.rdb, b.group, b.cfg.Consumer, 5*time.Second)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			log.Printf("maxbot: чтение очереди не удалось: %v", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
			}
			continue
		}
		if len(events) > 0 {
			b.dispatchMany(ctx, events)
			continue
		}
		// Новых нет — дособираем незавершённые этого консьюмера (накопившиеся,
		// пока адресата не было, или после рестарта).
		b.dispatchPending(ctx)
	}
}

// dispatchPending читает и отправляет незавершённые (pending) записи консьюмера.
func (b *bot) dispatchPending(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	events, err := readPending(ctx, b.rdb, b.group, b.cfg.Consumer)
	if err != nil {
		log.Printf("maxbot: чтение незавершённых не удалось: %v", err)
		return
	}
	if len(events) > 0 {
		b.dispatchMany(ctx, events)
	}
}

// dispatchMany отправляет каждое событие в MAX; успешно отправленные
// подтверждаются (XACK) и удаляются из Stream (XDEL). События без адресата
// НЕ отправляются и НЕ подтверждаются — остаются в очереди до появления
// подписчика.
func (b *bot) dispatchMany(ctx context.Context, events []notifyEvent) {
	for _, ev := range events {
		if ctx.Err() != nil {
			return
		}
		b.dispatchOne(ctx, ev)
	}
}

func (b *bot) dispatchOne(ctx context.Context, ev notifyEvent) {
	// Пустое событие (нет текста) — нечего отправлять, подтверждаем и удаляем,
	// чтобы не копить мусор (даже если адресата ещё нет).
	if ev.Text == "" {
		b.ackDel(ctx, ev.ID)
		return
	}
	if !b.client.hasRecipient() {
		// Адресат ещё не зарегистрирован — молча ждём подписку; событие остаётся.
		return
	}
	sctx, cancel := context.WithTimeout(ctx, maxRequestTimeout)
	defer cancel()
	if err := b.client.send(sctx, ev.Text); err != nil {
		// Ограничиваем частоту лога, чтобы при стойком отказе MAX не спамить.
		if time.Since(b.lastSendErrAt) >= 30*time.Second {
			log.Printf("maxbot: отправка в MAX не удалась: %v (сообщение останется в очереди)", err)
			b.lastSendErrAt = time.Now()
		}
		return
	}
	b.ackDel(ctx, ev.ID)
	log.Printf("maxbot: отправлено в MAX: %s", ev.Text)
}

// ackDel подтверждает запись (XACK) и, если она действительно была подтверждена
// этим консьюмером, удаляет её из Stream (XDEL). Проверка результата исключает
// удаление записи, обрабатываемой другим консьюмером.
func (b *bot) ackDel(ctx context.Context, id string) {
	n, err := b.rdb.XAck(ctx, streamKey, b.group, id).Result()
	if err != nil {
		log.Printf("maxbot: XACK %s не удался: %v", id, err)
		return
	}
	if n > 0 {
		b.rdb.XDel(ctx, streamKey, id)
	}
}
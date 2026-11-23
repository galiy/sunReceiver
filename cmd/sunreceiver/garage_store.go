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
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/redis/go-redis/v9"
)

// Хранилище модуля «Управление гаражом»:
//   - Redis: текущее состояние модуля и реле (идемпотентность после рестарта,
//     источник для дашборда);
//   - PG: журнал переключений реле (бессрочно), график ступеньками на /garage.

const redisGarageStateKey = "sunreceiver:garage:state"

// Состояния модуля (решения оператора): 1 — выключен конфигом, 2 — ожидание
// (нет связи/нет данных счётчика), 3 — управляет.
const (
	garageStateDisabled = 1
	garageStateWaiting  = 2
	garageStateControl  = 3
)

// garagePersistState — сохраняемое в Redis состояние модуля.
type garagePersistState struct {
	ModuleState    int          `json:"module_state"`
	StateChangedAt time.Time    `json:"state_changed_at"`
	Relays         [4]bool      `json:"relays"`
	RelayChangedAt [4]time.Time `json:"relay_changed_at"`
	// Fault — авария фаз была активна на момент сохранения: после рестарта не
	// отправляем ложное «пропадание фазы» повторно и держим выдержку.
	Fault bool `json:"phase_fault"`
}

// saveGarageState пишет состояние модуля в Redis (поле state — JSON).
func (s *redisStore) saveGarageState(st garagePersistState) error {
	b, err := json.Marshal(st)
	if err != nil {
		return fmt.Errorf("marshal garage state: %w", err)
	}
	return s.rdb.HSet(s.ctx, redisGarageStateKey, "state", b).Err()
}

// loadGarageState читает состояние модуля из Redis (nil — записи нет).
func (s *redisStore) loadGarageState() (*garagePersistState, error) {
	b, err := s.rdb.HGet(s.ctx, redisGarageStateKey, "state").Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var st garagePersistState
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, fmt.Errorf("unmarshal garage state: %w", err)
	}
	return &st, nil
}

// garageRelayEvent — одна запись журнала переключений.
type garageRelayEvent struct {
	TS     time.Time `json:"ts"`
	Relay  int       `json:"relay"`
	Old    bool      `json:"old"`
	New    bool      `json:"new"`
	Reason string    `json:"reason"`
}

// ensureGarageSchema создаёт таблицу журнала переключений реле гаража.
func ensureGarageSchema(s *pgStore) error {
	_, err := s.pool.Exec(s.ctx, `
CREATE TABLE IF NOT EXISTS sunreceiver.garage_relay_events (
	relay     smallint  NOT NULL,
	ts        timestamptz NOT NULL,
	old_state boolean   NOT NULL,
	new_state boolean   NOT NULL,
	reason    text      NOT NULL DEFAULT '',
	PRIMARY KEY (relay, ts)
);
CREATE INDEX IF NOT EXISTS garage_relay_events_ts_idx ON sunreceiver.garage_relay_events (ts);`)
	return err
}

// insertGarageRelayEvents пишет пачку событий. Конфликты PK (тот же канал в ту
// же микросекунду) игнорируются — переключения идемпотентны.
func (s *pgStore) insertGarageRelayEvents(events []garageRelayEvent) error {
	if len(events) == 0 {
		return nil
	}
	// Короткий контекст: запись журнала не должна задерживать секундный цикл
	// управления при «повисшем» PG (statement_timeout может быть 15 с).
	ctx, cancel := context.WithTimeout(s.ctx, 3*time.Second)
	defer cancel()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	for _, e := range events {
		if _, err := tx.Exec(ctx, `
INSERT INTO sunreceiver.garage_relay_events (relay, ts, old_state, new_state, reason)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (relay, ts) DO NOTHING`, e.Relay, e.TS, e.Old, e.New, e.Reason); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// garageRelayStatesAt возвращает состояние каждого реле на момент t (последнее
// событие не позже t) и признак known[i] — было ли вообще событие до t. Если
// событий не было, состояние на t неизвестно и рисовать плоскую линию нельзя.
func (s *pgStore) garageRelayStatesAt(t time.Time) ([4]bool, [4]bool, error) {
	var out [4]bool
	var known [4]bool
	ctx, cancel := context.WithTimeout(s.ctx, 5*time.Second)
	defer cancel()
	rows, err := s.pool.Query(ctx, `
SELECT DISTINCT ON (relay) relay, new_state
FROM sunreceiver.garage_relay_events
WHERE ts < $1
ORDER BY relay, ts DESC`, t)
	if err != nil {
		return out, known, err
	}
	defer rows.Close()
	for rows.Next() {
		var relay int
		var st bool
		if err := rows.Scan(&relay, &st); err != nil {
			return out, known, err
		}
		if relay >= 1 && relay <= 4 {
			out[relay-1] = st
			known[relay-1] = true
		}
	}
	return out, known, rows.Err()
}

// garageJournalStart возвращает время первого события журнала (nil — журнал пуст).
func (s *pgStore) garageJournalStart() (*time.Time, error) {
	ctx, cancel := context.WithTimeout(s.ctx, 5*time.Second)
	defer cancel()
	var nt sql.NullTime
	if err := s.pool.QueryRow(ctx, `SELECT MIN(ts) FROM sunreceiver.garage_relay_events`).Scan(&nt); err != nil {
		return nil, err
	}
	if !nt.Valid {
		return nil, nil
	}
	t := nt.Time
	return &t, nil
}

// garageRelayStateDuration считает суммарное время, когда реле relay было в
// состоянии want, внутри [from, to]. Журнал разреженный: состояние на from
// берётся из последнего события до from; если событий до from не было, отсчёт
// начинается с первого события внутри интервала (состояние до него неизвестно).
func (s *pgStore) garageRelayStateDuration(relay int, want bool, from, to time.Time) (time.Duration, error) {
	if relay < 1 || relay > 4 || !from.Before(to) {
		return 0, nil
	}
	ctx, cancel := context.WithTimeout(s.ctx, 5*time.Second)
	defer cancel()

	var (
		cur   bool
		known bool
	)
	err := s.pool.QueryRow(ctx, `
SELECT new_state
FROM sunreceiver.garage_relay_events
WHERE relay = $1 AND ts < $2
ORDER BY ts DESC
LIMIT 1`, relay, from).Scan(&cur)
	switch {
	case err == nil:
		known = true
	case errors.Is(err, pgx.ErrNoRows):
	default:
		return 0, err
	}

	rows, err := s.pool.Query(ctx, `
SELECT ts, new_state
FROM sunreceiver.garage_relay_events
WHERE relay = $1 AND ts >= $2 AND ts < $3
ORDER BY ts ASC`, relay, from, to)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	curT := from
	var dur time.Duration
	for rows.Next() {
		var (
			ts time.Time
			st bool
		)
		if err := rows.Scan(&ts, &st); err != nil {
			return 0, err
		}
		if known && cur == want {
			dur += ts.Sub(curT)
		}
		cur, curT, known = st, ts, true
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if known && cur == want {
		dur += to.Sub(curT)
	}
	if dur < 0 {
		dur = 0
	}
	return dur, nil
}

// garageRelayEvents читает журнал за [from, to] по возрастанию времени.
func (s *pgStore) garageRelayEvents(from, to time.Time) ([]garageRelayEvent, error) {
	ctx, cancel := context.WithTimeout(s.ctx, 5*time.Second)
	defer cancel()
	rows, err := s.pool.Query(ctx, `
SELECT relay, ts, old_state, new_state, reason
FROM sunreceiver.garage_relay_events
WHERE ts >= $1 AND ts <= $2
ORDER BY ts ASC, relay ASC`, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []garageRelayEvent
	for rows.Next() {
		var e garageRelayEvent
		if err := rows.Scan(&e.Relay, &e.TS, &e.Old, &e.New, &e.Reason); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

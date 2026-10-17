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
	"encoding/json"
	"fmt"

	"github.com/redis/go-redis/v9"
)

// Ключ Redis модуля CE308: разовый снимок накопленной электроэнергии
// (JSON ce308EnergySnapshot). Один ключ, история по энергии не ведётся.
const redisCE308EnergyKey = "sunreceiver:ce308-energy:current"

// SaveCE308Energy сохраняет разовый снимок энергии (отдельный ключ).
func (s *redisStore) SaveCE308Energy(snap *ce308EnergySnapshot) error {
	b, err := json.Marshal(snap)
	if err != nil {
		return fmt.Errorf("marshal ce308 energy: %w", err)
	}
	return s.rdb.Set(s.ctx, redisCE308EnergyKey, b, 0).Err()
}

// CE308Energy возвращает последний снимок энергии CE308; пусто, если ещё не было.
func (s *redisStore) CE308Energy() (*ce308EnergySnapshot, error) {
	b, err := s.rdb.Get(s.ctx, redisCE308EnergyKey).Bytes()
	if err == redis.Nil {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var snap ce308EnergySnapshot
	if err := json.Unmarshal(b, &snap); err != nil {
		return nil, err
	}
	return &snap, nil
}

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
)

// SaveEnBmsCurrent пишет текущий снимок EnBMS в ряд bms26 (единый EnBMS-serial),
// т.е. старый EnBMS-пулер кормит то же хранилище, что и пулер malina26.
func (s *redisStore) SaveEnBmsCurrent(snap enbmsSnapshot) error {
	// КОСТЫЛЬ: старый EnBMS-пулер пишет в ряды bms26 (единый EnBMS-serial).
	serial := legacyEnbmsSerial()
	if serial == "" {
		return nil
	}
	b, err := json.Marshal(enbmsSnapshotToBms26(snap, serial))
	if err != nil {
		return fmt.Errorf("marshal enbms→bms26 %s: %w", snap.MAC, err)
	}
	return s.SaveBms26Current(map[string]string{serial: string(b)})
}

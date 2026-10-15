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

// ПАКЕТ СКОПИРОВАН. Оригинал (bring-up):
//   github.com/galiy/malina-26/daemons/lib/bmscontract
//   https://github.com/galiy/malina-26/blob/main/daemons/lib/bmscontract/bmscontract.go
//   локально: ../cleaning.malina/daemons/lib/bmscontract/bmscontract.go
// Снимок от cleaning.malina@9052cfb. Контракт развивается (V2/V3) — при изменении
// оригинала обновить эту копию (оффсеты/поля) и перенести изменения сюда.
// Документация контракта: cleaning.malina/docs/bms-reader.md §1.

// Package bmscontract — бинарный контракт телеметрии BMS фиксированной длины.
//
// Раскладка (все многобайтовые — big-endian, выравнивания нет), 107 байт:
//
//	0   1  cell_count      u8
//	1   1  temp_count      u8
//	2   2  flags           u16   (b0 charge_mos, b1 discharge_mos, b2 balancer)
//	4   32 cells_mv[16]    u16×16 (мВ)
//	36  16 temps_dc[8]     i16×8  (0.1 °C)
//	52  4  current_da      i32    (0.01 А; разряд «+», заряд «−»)
//	56  4  power_w         i32    (Вт)
//	60  2  soc_dp          u16    (0.1 %)
//	62  2  soh_dp          u16    (0.1 %)
//	64  4  cap_mah         u32    (0.001 А·ч)
//	68  4  remaining_mah   u32    (0.001 А·ч)
//	72  4  rated_mah       u32    (0.001 А·ч)
//	76  2  cycles          u16
//	78  2  total_cv        u16    (0.01 В)
//	80  2  port_cv         u16    (0.01 В)
//	82  4  balance_mask    u32
//	86  2  mode            u16    (младший байт — канонический код режима)
//	88  19 alarm[19]       bytes  (битфилд тревог/защит; раскладка — по типу)
//
// Контракт — docs/bms-reader.md §1. Значения, которых нет у типа, — 0.
package bmscontract

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// MaxCells / MaxTemps — фиксированные ёмкости массивов контракта.
const (
	MaxCells = 16
	MaxTemps = 8
	AlarmLen = 19
	Size     = 107
)

// Флаги `flags` (биты).
const (
	FlagChargeMOS    = 1 << 0
	FlagDischargeMOS = 1 << 1
	FlagBalancer     = 1 << 2
)

// Канонические коды `mode` (младший байт). docs/bms-reader.md §1.
const (
	ModeIdle      uint16 = 0x00 // ожидание / standby
	ModeDischarge uint16 = 0x01
	ModeCharge    uint16 = 0x02
	ModeMaintain  uint16 = 0x03
	ModeFull      uint16 = 0x04
	ModeShutdown  uint16 = 0x05
)

// Telemetry — фиксированная бинарная структура контракта BMS.
type Telemetry struct {
	CellCount    uint8
	TempCount    uint8
	Flags        uint16
	CellsMV      [MaxCells]uint16
	TempsDC      [MaxTemps]int16
	CurrentDA    int32
	PowerW       int32
	SocDP        uint16
	SohDP        uint16
	CapMAh       uint32
	RemainingMAh uint32
	RatedMAh     uint32
	Cycles       uint16
	TotalCV      uint16
	PortCV       uint16
	BalanceMask  uint32
	Mode         uint16
	Alarm        [AlarmLen]byte
}

// ErrShort — буфер короче Size.
var ErrShort = errors.New("bmscontract: короткий буфер")

// Encode сериализует структуру в 107 байт (big-endian).
func Encode(t *Telemetry) []byte {
	b := make([]byte, Size)
	b[0] = t.CellCount
	b[1] = t.TempCount
	binary.BigEndian.PutUint16(b[2:], t.Flags)
	for i := 0; i < MaxCells; i++ {
		binary.BigEndian.PutUint16(b[4+i*2:], t.CellsMV[i])
	}
	for i := 0; i < MaxTemps; i++ {
		binary.BigEndian.PutUint16(b[36+i*2:], uint16(t.TempsDC[i]))
	}
	binary.BigEndian.PutUint32(b[52:], uint32(t.CurrentDA))
	binary.BigEndian.PutUint32(b[56:], uint32(t.PowerW))
	binary.BigEndian.PutUint16(b[60:], t.SocDP)
	binary.BigEndian.PutUint16(b[62:], t.SohDP)
	binary.BigEndian.PutUint32(b[64:], t.CapMAh)
	binary.BigEndian.PutUint32(b[68:], t.RemainingMAh)
	binary.BigEndian.PutUint32(b[72:], t.RatedMAh)
	binary.BigEndian.PutUint16(b[76:], t.Cycles)
	binary.BigEndian.PutUint16(b[78:], t.TotalCV)
	binary.BigEndian.PutUint16(b[80:], t.PortCV)
	binary.BigEndian.PutUint32(b[82:], t.BalanceMask)
	binary.BigEndian.PutUint16(b[86:], t.Mode)
	copy(b[88:], t.Alarm[:])
	return b
}

// Decode разбирает 107-байтный буфер в структуру.
func Decode(b []byte) (Telemetry, error) {
	var t Telemetry
	if len(b) < Size {
		return t, fmt.Errorf("%w: %d < %d", ErrShort, len(b), Size)
	}
	t.CellCount = b[0]
	t.TempCount = b[1]
	t.Flags = binary.BigEndian.Uint16(b[2:])
	for i := 0; i < MaxCells; i++ {
		t.CellsMV[i] = binary.BigEndian.Uint16(b[4+i*2:])
	}
	for i := 0; i < MaxTemps; i++ {
		t.TempsDC[i] = int16(binary.BigEndian.Uint16(b[36+i*2:]))
	}
	t.CurrentDA = int32(binary.BigEndian.Uint32(b[52:]))
	t.PowerW = int32(binary.BigEndian.Uint32(b[56:]))
	t.SocDP = binary.BigEndian.Uint16(b[60:])
	t.SohDP = binary.BigEndian.Uint16(b[62:])
	t.CapMAh = binary.BigEndian.Uint32(b[64:])
	t.RemainingMAh = binary.BigEndian.Uint32(b[68:])
	t.RatedMAh = binary.BigEndian.Uint32(b[72:])
	t.Cycles = binary.BigEndian.Uint16(b[76:])
	t.TotalCV = binary.BigEndian.Uint16(b[78:])
	t.PortCV = binary.BigEndian.Uint16(b[80:])
	t.BalanceMask = binary.BigEndian.Uint32(b[82:])
	t.Mode = binary.BigEndian.Uint16(b[86:])
	copy(t.Alarm[:], b[88:88+AlarmLen])
	return t, nil
}

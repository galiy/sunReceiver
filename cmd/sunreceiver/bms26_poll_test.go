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
	"errors"
	"testing"
	"time"

	"github.com/galiy/sunReceiver/internal/bmscontract"
)

// fakeBms26API — источник без HTTP: список/слоты и ошибки задаются тестом.
type fakeBms26API struct {
	devs    []bms26Device
	listErr error
	raws    map[int][]byte
	rawErrs map[int]error
	listN   int
	rawN    int
}

func (f *fakeBms26API) fetchBmsDevices(context.Context) ([]bms26Device, error) {
	f.listN++
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.devs, nil
}

func (f *fakeBms26API) fetchBmsRaw(_ context.Context, id int, _ uint64) ([]byte, error) {
	f.rawN++
	if err := f.rawErrs[id]; err != nil {
		return nil, err
	}
	if b, ok := f.raws[id]; ok {
		return b, nil
	}
	return nil, nil // 404/слот потерян
}

func bms26TestPayload(t *testing.T) []byte {
	t.Helper()
	var tm bmscontract.Telemetry
	tm.CellCount = 4
	tm.TempCount = 2
	tm.CellsMV[0], tm.CellsMV[1], tm.CellsMV[2], tm.CellsMV[3] = 3350, 3360, 3345, 3370
	tm.TempsDC[0], tm.TempsDC[1] = 320, 305
	tm.CurrentDA = -1234
	tm.SocDP = 560
	tm.TotalCV = 5362
	tm.PortCV = 5364
	tm.Mode = bmscontract.ModeCharge
	return bmscontract.Encode(&tm)
}

// newTestPoller создаёт пулер с управляемыми часами и перехватом предупреждений.
func newTestPoller(t *testing.T, api bms26API, now *time.Time) (*bms26Poller, *[]string) {
	t.Helper()
	p := newBms26Poller(api, &bms26Stats{})
	p.now = func() time.Time { return *now }
	p.limiter.now = p.now
	warns := []string{}
	p.limiter.sink = func(msg string) { warns = append(warns, msg) }
	return p, &warns
}

func TestBms26DuplicateSerialDeterministicCurrent(t *testing.T) {
	payload := bms26TestPayload(t)
	api := &fakeBms26API{
		devs: []bms26Device{
			{ID: 5, Gen: 1, Serial: "DUP-1", BmsType: "AntBMS-DSP"},
			{ID: 3, Gen: 1, Serial: "DUP-1", BmsType: "AntBMS-DSP"},
		},
		raws: map[int][]byte{5: payload, 3: payload},
	}
	now := time.Unix(1_700_000_000, 0)
	p, warns := newTestPoller(t, api, &now)

	snaps := p.poll(context.Background())
	if len(snaps) != 2 {
		t.Fatalf("снимков=%d, ожидалось 2 (оба устройства публикуются)", len(snaps))
	}
	for _, sn := range snaps {
		if sn.DupCount != 2 {
			t.Errorf("DupCount=%d, ожидалось 2", sn.DupCount)
		}
	}
	v := p.stats.view()
	if v.DupSerial != 1 {
		t.Errorf("dup_serial=%d, ожидалась 1 лишняя запись", v.DupSerial)
	}
	if v.Devices != 2 {
		t.Errorf("devices=%d", v.Devices)
	}
	cur := bms26SelectCurrent(snaps)
	if len(cur) != 1 {
		t.Fatalf("current записей=%d, ожидалась 1", len(cur))
	}
	sn := cur["DUP-1"]
	if len(sn.SourceIDs) != 2 || sn.SourceIDs[0] != 3 || sn.SourceID != 3 {
		t.Fatalf("current source: SourceIDs=%v SourceID=%d, ожидались [3 5] и 3", sn.SourceIDs, sn.SourceID)
	}
	if len(*warns) != 1 {
		t.Fatalf("предупреждений=%d, ожидалось 1", len(*warns))
	}

	// Повтор в пределах cooldown не должен растить лог.
	p.poll(context.Background())
	if len(*warns) != 1 {
		t.Fatalf("повторное предупреждение о дубле без cooldown: %d", len(*warns))
	}
	// После cooldown — снова одно предупреждение.
	now = now.Add(bms26WarnCooldown + time.Second)
	p.poll(context.Background())
	if len(*warns) != 2 {
		t.Fatalf("после cooldown предупреждений=%d, ожидалось 2", len(*warns))
	}
}

func TestBms26SourceLastSeenAndStale(t *testing.T) {
	payload := bms26TestPayload(t)
	now := time.Unix(1_700_000_000, 0)
	api := &fakeBms26API{
		devs: []bms26Device{
			{ID: 1, Serial: "FRESH", LastSeen: now.Unix() - 1},
			{ID: 2, Serial: "STALE", LastSeen: now.Unix() - 30},
			{ID: 3, Serial: "UNKNOWN"}, // API без last_seen — не считаем stale
		},
		raws: map[int][]byte{1: payload, 2: payload, 3: payload},
	}
	p, warns := newTestPoller(t, api, &now)
	snaps := p.poll(context.Background())
	if len(snaps) != 3 {
		t.Fatalf("снимков=%d", len(snaps))
	}
	bySerial := map[string]bms26Snapshot{}
	for _, sn := range snaps {
		bySerial[sn.Serial] = sn
	}
	if sn := bySerial["FRESH"]; sn.Stale || sn.SourceLastSeen != now.Unix()-1 {
		t.Errorf("FRESH: stale=%v last_seen=%d", sn.Stale, sn.SourceLastSeen)
	}
	if sn := bySerial["STALE"]; !sn.Stale || sn.SourceLastSeen != now.Unix()-30 {
		t.Errorf("STALE: stale=%v last_seen=%d", sn.Stale, sn.SourceLastSeen)
	}
	if sn := bySerial["UNKNOWN"]; sn.Stale || sn.SourceLastSeen != 0 {
		t.Errorf("UNKNOWN: stale=%v last_seen=%d", sn.Stale, sn.SourceLastSeen)
	}
	if v := p.stats.view(); v.Stale != 1 || v.Devices != 3 {
		t.Errorf("stats: stale=%d devices=%d", v.Stale, v.Devices)
	}
	if len(*warns) != 1 {
		t.Errorf("предупреждений о stale=%d, ожидалось 1", len(*warns))
	}
	// Timestamp — время опроса, а не last_seen.
	if sn := bySerial["STALE"]; sn.Timestamp != now.Format(time.RFC3339) {
		t.Errorf("timestamp=%q, ожидалось время опроса %q", sn.Timestamp, now.Format(time.RFC3339))
	}
}

func TestBms26NotFoundSkippedRateLimited(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	api := &fakeBms26API{
		devs: []bms26Device{{ID: 7, Serial: "LOST"}}, // raw отсутствует → nil (404)
	}
	p, warns := newTestPoller(t, api, &now)
	for i := 0; i < 3; i++ {
		if snaps := p.poll(context.Background()); len(snaps) != 0 {
			t.Fatalf("404 не должен давать снимок, got %d", len(snaps))
		}
	}
	if v := p.stats.view(); v.NotFound != 3 {
		t.Errorf("not_found=%d, ожидалось 3", v.NotFound)
	}
	if len(*warns) != 1 {
		t.Errorf("404 предупреждений=%d, ожидалось 1 (cooldown)", len(*warns))
	}
}

func TestBms26DecodeErrorSkippedRateLimited(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	api := &fakeBms26API{
		devs: []bms26Device{{ID: 8, Serial: "BAD"}},
		raws: map[int][]byte{8: {1, 2, 3}}, // короче 107 Б
	}
	p, warns := newTestPoller(t, api, &now)
	for i := 0; i < 3; i++ {
		if snaps := p.poll(context.Background()); len(snaps) != 0 {
			t.Fatalf("битый payload не должен давать снимок")
		}
	}
	if v := p.stats.view(); v.DecodeErr != 3 {
		t.Errorf("decode_err=%d, ожидалось 3", v.DecodeErr)
	}
	if len(*warns) != 1 {
		t.Errorf("decode предупреждений=%d, ожидалось 1", len(*warns))
	}
}

func TestBms26EmptyDeviceList(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	api := &fakeBms26API{}
	p, warns := newTestPoller(t, api, &now)
	if snaps := p.poll(context.Background()); len(snaps) != 0 {
		t.Fatalf("пустой список не должен давать снимки")
	}
	if v := p.stats.view(); v.Devices != 0 || v.DupSerial != 0 || v.Stale != 0 {
		t.Errorf("stats пустого списка: %+v", v)
	}
	if len(*warns) != 0 {
		t.Errorf("пустой список не должен спамить в лог: %v", *warns)
	}
}

func TestBms26HTTPErrorBackoff(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	api := &fakeBms26API{listErr: context.DeadlineExceeded}
	p, warns := newTestPoller(t, api, &now)

	if snaps := p.poll(context.Background()); len(snaps) != 0 {
		t.Fatalf("ошибка списка не должна давать снимки")
	}
	// В пределах backoff HTTP не повторяется.
	p.poll(context.Background())
	if api.listN != 1 {
		t.Fatalf("list вызовов=%d, ожидалось 1 (backoff)", api.listN)
	}
	if v := p.stats.view(); v.HTTPErr != 1 {
		t.Errorf("http_err=%d, ожидалось 1", v.HTTPErr)
	}
	if len(*warns) != 1 {
		t.Errorf("warn=%d, ожидалось 1", len(*warns))
	}
	now = now.Add(bms26BackoffBase)
	p.poll(context.Background())
	if api.listN != 2 {
		t.Fatalf("после backoff list вызовов=%d, ожидалось 2", api.listN)
	}
}

func TestBms26RawErrorBackoff(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	api := &fakeBms26API{
		devs:    []bms26Device{{ID: 9, Serial: "ERR"}},
		rawErrs: map[int]error{9: errors.New("500 internal server error")},
	}
	p, _ := newTestPoller(t, api, &now)
	p.poll(context.Background())
	p.poll(context.Background()) // raw в backoff — не повторяется
	if api.rawN != 1 {
		t.Fatalf("raw вызовов=%d, ожидалось 1 (backoff слота)", api.rawN)
	}
	if v := p.stats.view(); v.HTTPErr != 1 {
		t.Errorf("http_err=%d, ожидалось 1", v.HTTPErr)
	}
	now = now.Add(bms26BackoffBase)
	p.poll(context.Background())
	if api.rawN != 2 {
		t.Fatalf("после backoff raw вызовов=%d, ожидалось 2", api.rawN)
	}
}

func TestBms26StableStateNoLogGrowth(t *testing.T) {
	payload := bms26TestPayload(t)
	now := time.Unix(1_700_000_000, 0)
	api := &fakeBms26API{
		devs: []bms26Device{
			{ID: 1, Serial: "A", LastSeen: now.Unix()},
			{ID: 2, Serial: "B", LastSeen: now.Unix()},
		},
		raws: map[int][]byte{1: payload, 2: payload},
	}
	p, warns := newTestPoller(t, api, &now)
	for i := 0; i < 10; i++ {
		now = now.Add(time.Second)
		// last_seen обновляем вместе с часами — источник свежий.
		api.devs[0].LastSeen = now.Unix()
		api.devs[1].LastSeen = now.Unix()
		if snaps := p.poll(context.Background()); len(snaps) != 2 {
			t.Fatalf("тик %d: снимков=%d", i, len(snaps))
		}
	}
	if len(*warns) != 0 {
		t.Fatalf("на стабильном состоянии лог вырос: %v", *warns)
	}
}

func TestBmsDeviceFromBms26SourceFlags(t *testing.T) {
	sn := bms26Snapshot{
		Serial: "DUP-1", Timestamp: time.Unix(1_700_000_000, 0).Format(time.RFC3339),
		SourceLastSeen: 1_699_999_990, Stale: true, DupCount: 2,
	}
	d := bmsDeviceFromBms26(sn)
	if !d.Stale || d.SourceLastSeen != 1_699_999_990 || d.DupSerial != 2 {
		t.Fatalf("bmsDevice не пробросил stale/dup: %+v", d)
	}
}

package main

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

func newTestRelay(lamps []relayLampCfg) (*relayController, *[]string) {
	cfg := &relaySection{
		IP:      "127.0.0.1",
		UDPPort: 6723,
		BlinkHz: 2, // полупериод 250 мс
		Lamps:   lamps,
	}
	c := newRelayController(cfg, nil)
	sent := &[]string{}
	c.sendCmd = func(s string) { *sent = append(*sent, s) }
	return c, sent
}

func boolp(b bool) *bool { return &b }

// SetLamp переключает только одну лампу; вторая сохраняет своё состояние.
func TestRelaySetLampPreservesOther(t *testing.T) {
	c, _ := newTestRelay([]relayLampCfg{{Name: "white", Relay: 1}, {Name: "red", Relay: 2}})

	if err := c.SetLampByName("white", lampOn); err != nil {
		t.Fatalf("set white: %v", err)
	}
	if c.State(0) != lampOn {
		t.Fatalf("white=%v, want on", c.State(0))
	}
	if c.State(1) != lampOff {
		t.Fatalf("red=%v, want off (сохранён)", c.State(1))
	}

	// Переключаем красную — белая должна остаться on.
	if err := c.SetLampByName("red", lampBlink); err != nil {
		t.Fatalf("set red: %v", err)
	}
	if c.State(0) != lampOn {
		t.Fatalf("white=%v, want on (не тронута)", c.State(0))
	}
	if c.State(1) != lampBlink {
		t.Fatalf("red=%v, want blink", c.State(1))
	}

	// Неизвестная лампа — ошибка, состояние не меняется.
	if err := c.SetLampByName("nope", lampOn); err == nil {
		t.Fatal("ожидали ошибку для неизвестной лампы")
	}
}

// Мигание 2 Гц: в цикле канал переключается вкл/выкл каждые 250 мс.
func TestRelayBlinkToggles(t *testing.T) {
	c, sent := newTestRelay([]relayLampCfg{{Name: "white", Relay: 1}})
	if err := c.SetLampByName("white", lampBlink); err != nil {
		t.Fatal(err)
	}
	*sent = nil

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); runRelayControl(c, ctx) }()
	// Через ~1.1 с должно быть 4+ переключений (t=0,250,500,750 мс).
	time.Sleep(1100 * time.Millisecond)
	cancel()
	<-done

	// Ожидаем чередующиеся команды на канал 1: 11 / 21 / 11 / ...
	expect := []string{"11", "21", "11", "21"}
	if len(*sent) < 4 {
		t.Fatalf("отправлено %d команд %v, want >= %v (мигание не тикает)", len(*sent), *sent, expect)
	}
	if got := (*sent)[:4]; !reflect.DeepEqual(got, expect) {
		t.Fatalf("первые 4 команды %v, want %v", got, expect)
	}
}

// steady-состояние повторно отправляется через keepalive (потеря UDP-пакета).
func TestRelaySteadyKeepalive(t *testing.T) {
	c, sent := newTestRelay([]relayLampCfg{{Name: "white", Relay: 1}})
	c.cfg.Keepalive = "100ms"
	if err := c.SetLampByName("white", lampOn); err != nil {
		t.Fatal(err)
	}
	// Первая отправка — сразу в SetLamp. Очищаем, проверяем повторы из цикла.
	*sent = nil

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); runRelayControl(c, ctx) }()
	time.Sleep(500 * time.Millisecond)
	cancel()
	<-done

	if len(*sent) < 2 {
		t.Fatalf("steady не повторяется по keepalive: %v", *sent)
	}
	for _, s := range *sent {
		if s != "11" {
			t.Fatalf("команда %q, want 11", s)
		}
	}
}

// Дефолты: порт 6723, частота 2 Гц, белая(1)/красная(2) при пустом списке ламп.
func TestRelayBuildCfgDefaults(t *testing.T) {
	cfg := buildRelayCfg(&relaySection{IP: "192.168.13.34", Disabled: boolp(false)})
	if cfg.UDPPort != 6723 {
		t.Fatalf("udp_port=%d, want 6723", cfg.UDPPort)
	}
	if cfg.BlinkHz != 2 {
		t.Fatalf("blink_hz=%v, want 2", cfg.BlinkHz)
	}
	if len(cfg.Lamps) != 2 || cfg.Lamps[0].Name != "white" || cfg.Lamps[1].Name != "red" {
		t.Fatalf("lamps=%+v, want white(1)/red(2)", cfg.Lamps)
	}
	if cfg.Lamps[0].Relay != 1 || cfg.Lamps[1].Relay != 2 {
		t.Fatalf("relay mapping=%+v, want 1 и 2", cfg.Lamps)
	}
}

// fakeSnapshotLoader — заглушка CurrentOne: снимки/ошибки по IP.
type fakeSnapshotLoader struct {
	snaps map[string]deviceSnapshot
	errs  map[string]error
}

func (f *fakeSnapshotLoader) CurrentOne(ip string) (deviceSnapshot, error) {
	if f == nil {
		return deviceSnapshot{}, errors.New("redis: nil")
	}
	if f.errs != nil {
		if err, ok := f.errs[ip]; ok {
			return deviceSnapshot{}, err
		}
	}
	if f.snaps != nil {
		if s, ok := f.snaps[ip]; ok {
			return s, nil
		}
	}
	return deviceSnapshot{}, errors.New("redis: nil")
}

func freshSnap(t time.Time, ip string, key string, v any) deviceSnapshot {
	return deviceSnapshot{
		Name:      "dev",
		IP:        ip,
		Timestamp: t.Format(time.RFC3339),
		Values:    map[string]any{key: v},
	}
}

// Последовательный алгоритм красной лампы.
func TestRedLampDecision(t *testing.T) {
	meter := &meterConfig{IP: "192.168.0.77", Name: "M"}
	now := time.Now()
	old := now.Add(-25 * time.Second)

	cases := []struct {
		name   string
		meter  *meterConfig
		loader relaySnapshotLoader
		want   lampState
	}{
		// 1. Модуль счётчика отключён → не горит (даже если счётчик «жив»).
		{"meter disabled", nil, &fakeSnapshotLoader{snaps: map[string]deviceSnapshot{meter.IP: freshSnap(now, meter.IP, "meter_active_power", -100.0)}}, lampOff},
		// 2. Счётчик недоступен: снэпшот отсутствует / старше 20 с → мигает.
		{"snapshot absent", meter, &fakeSnapshotLoader{}, lampBlink},
		{"snapshot stale", meter, &fakeSnapshotLoader{snaps: map[string]deviceSnapshot{meter.IP: freshSnap(old, meter.IP, "meter_active_power", -100.0)}}, lampBlink},
		{"unparseable ts", meter, &fakeSnapshotLoader{snaps: map[string]deviceSnapshot{meter.IP: {IP: meter.IP, Timestamp: "not-a-time", Values: map[string]any{"meter_active_power": -100.0}}}}, lampBlink},
		// 3. Мощность положительная (потребление) → не горит.
		{"positive power", meter, &fakeSnapshotLoader{snaps: map[string]deviceSnapshot{meter.IP: freshSnap(now, meter.IP, "meter_active_power", 200.0)}}, lampOff},
		// 4. Мощность отрицательная (отдача) → горит.
		{"negative power", meter, &fakeSnapshotLoader{snaps: map[string]deviceSnapshot{meter.IP: freshSnap(now, meter.IP, "meter_active_power", -300.0)}}, lampOn},
		// Нечисловое значение — считаем недоступным → мигает.
		{"non-numeric power", meter, &fakeSnapshotLoader{snaps: map[string]deviceSnapshot{meter.IP: freshSnap(now, meter.IP, "meter_active_power", "?")}}, lampBlink},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := redLampDecision(tc.loader, nil, tc.meter); got != tc.want {
				t.Fatalf("redLampDecision=%v, want %v", got, tc.want)
			}
		})
	}
}

// Последовательный алгоритм белой лампы (МАП + счётчик).
func TestWhiteLampDecision(t *testing.T) {
	const mapIP = "192.168.13.74"
	meter := &meterConfig{IP: "192.168.13.77"}
	now := time.Now()
	old := now.Add(-25 * time.Second)

	loader := func(grid float64, meterV float64, meterErr bool, staleMeter bool) relaySnapshotLoader {
		snaps := map[string]deviceSnapshot{
			mapIP: freshSnap(now, mapIP, "grid_voltage", grid),
		}
		if !meterErr {
			if staleMeter {
				snaps[meter.IP] = freshSnap(old, meter.IP, "meter_voltage", meterV)
			} else {
				snaps[meter.IP] = freshSnap(now, meter.IP, "meter_voltage", meterV)
			}
		}
		errs := map[string]error{}
		if meterErr {
			errs[meter.IP] = errors.New("redis: nil")
		}
		return &fakeSnapshotLoader{snaps: snaps, errs: errs}
	}

	cases := []struct {
		name  string
		load  relaySnapshotLoader
		mapIP string
		meter *meterConfig
		want  lampState
	}{
		// 1. МАП отключён (mapIP=="") → не горит (даже если счётчик живой).
		{"map disabled", loader(230, 230, false, false), "", meter, lampOff},
		// 1. МАП недоступен (снэпшот отсутствует/протух) → не горит.
		{"map snapshot absent", &fakeSnapshotLoader{}, mapIP, meter, lampOff},
		{"map snapshot stale", &fakeSnapshotLoader{snaps: map[string]deviceSnapshot{mapIP: freshSnap(old, mapIP, "grid_voltage", 230.0)}}, mapIP, meter, lampOff},
		// 2. Напряжение на входе МАП есть → горит.
		{"map grid present", loader(230, 0, false, false), mapIP, meter, lampOn},
		// 3. Напряжения на входе МАП нет, но у счётчика есть и он свежий → мигает.
		{"map grid absent, meter ok", loader(0, 230, false, false), mapIP, meter, lampBlink},
		// 4. Напряжения на входе МАП нет, счётчик недоступен → не горит.
		{"map grid absent, meter absent", loader(0, 0, true, false), mapIP, meter, lampOff},
		{"map grid absent, meter stale", loader(0, 230, false, true), mapIP, meter, lampOff},
		// Нечисловое напряжение МАП → считаем «нет напряжения» → смотрим счётчик (свежий → мигает).
		{"map grid non-numeric", &fakeSnapshotLoader{snaps: map[string]deviceSnapshot{
			mapIP:    {IP: mapIP, Timestamp: now.Format(time.RFC3339), Values: map[string]any{"grid_voltage": "?"}},
			meter.IP: freshSnap(now, meter.IP, "meter_voltage", 230.0),
		}}, mapIP, meter, lampBlink},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := whiteLampDecision(tc.load, nil, tc.mapIP, tc.meter); got != tc.want {
				t.Fatalf("whiteLampDecision=%v, want %v", got, tc.want)
			}
		})
	}
}

// Красная лампа переключается только при изменении состояния; белая не трогается.
func TestRedLampOnlyChangesOnTransition(t *testing.T) {
	c, sent := newTestRelay([]relayLampCfg{{Name: "white", Relay: 1}, {Name: "red", Relay: 2}})
	now := time.Now()
	loader := &fakeSnapshotLoader{snaps: map[string]deviceSnapshot{"192.168.0.77": freshSnap(now, "192.168.0.77", "meter_active_power", -100.0)}} // отдача → on
	meter := &meterConfig{IP: "192.168.0.77"}

	if err := c.SetLampByName("white", lampOn); err != nil {
		t.Fatal(err)
	}
	*sent = nil
	// Два решения подряд — одно и то же состояние on: повторная отправка быть не должна.
	_ = c.SetLampByName("red", redLampDecision(loader, c.cfg, meter))
	_ = c.SetLampByName("red", redLampDecision(loader, c.cfg, meter))
	if len(*sent) != 1 {
		t.Fatalf("команд для red=%d %v, want 1 (только первое включение)", len(*sent), *sent)
	}
	if c.State(0) != lampOn {
		t.Fatalf("white=%v, want on (не тронута)", c.State(0))
	}
	if c.State(1) != lampOn {
		t.Fatalf("red=%v, want on", c.State(1))
	}
}

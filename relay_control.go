package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"strconv"
	"sync"
	"time"
)

// Управление сетевым реле SR-201 (Lee Software, 2 канала) по UDP.
//
// Лампа = один канал (реле) SR-201. Каждая лампа может находиться в одном из
// трёх состояний: lampOff (не горит), lampOn (горит), lampBlink (мигает с
// заданной частотой). Состояния ламп независимы — любая комбинация допустима.
//
// Модуль поддерживает текущее состояние: команды отправляются на устройство по
// UDP (порт 6723, без ответа), поэтому из-за потерь/сброса реле состояние может
// «уплыть». Единственный владелец физических реле — фоновый цикл runRelayControl,
// который непрерывно приводит реле к желаемым состояниям: для мигания — переключает
// канал с частотой blink_hz, для steady (on/off) — повторно отправляет команду
// через keepalive-интервал. Другие модули проекта только задают ЖЕЛАЕМОЕ состояние
// (SetLamp / SetRelayLamp), не трогая вторую лампу.
//
// SR-201. Протокол поверх TCP 6722 / UDP 6723 (text/ASCII, без разделителей):
//
//	"00" — запрос статуса (ответ "010…", позиция = канал), только TCP;
//	"1<N>" — канал N ВКЛ; "2<N>" — канал N ВЫКЛ; "1X"/"2X" — все вкл/выкл.
//
// Команды идут по UDP, как указано в постановке — статус (TCP) для чтения
// фактического положения реле в этой итерации не используется.

const (
	// 1, 2 на SR-201: одна лампа — один канал. Запись: "1N" вкл, "2N" выкл.
	relayCmdOnPrefix  = "1"
	relayCmdOffPrefix = "2"

	relayUDPDefaultPort   = 6723
	relayDefaultBlinkHz   = 2.0
	relayDefaultKeepalive = 2 * time.Second

	// Дефолты параметров анализа состояния ламп.
	defaultStaleWindow     = 20 * time.Second
	defaultMeterPowerTag   = "meter_active_power"
	defaultMeterVoltageTag = "meter_voltage"
	defaultMapGridTag      = "grid_voltage"

	// redisRelayKey — HASH текущего (желаемого) состояния ламп: поле = имя лампы,
	// значение = "on"/"off"/"blink". Пишется при каждом изменении и при старте.
	redisRelayKey = "sunreceiver:relay"
)

// lampState — состояние лампы (канала реле).
type lampState int

const (
	lampOff lampState = iota
	lampOn
	lampBlink
)

func (s lampState) String() string {
	switch s {
	case lampOn:
		return "on"
	case lampBlink:
		return "blink"
	default:
		return "off"
	}
}

// parseLampState разбирает состояние из строки ("off"/"on"/"blink").
func parseLampState(name string) (lampState, bool) {
	switch name {
	case "off":
		return lampOff, true
	case "on":
		return lampOn, true
	case "blink":
		return lampBlink, true
	}
	return lampOff, false
}

// relayLampCfg — одна лампа: имя и номер канала (реле) SR-201, на который она
// подключена. Relay — 1-индексный номер реле.
type relayLampCfg struct {
	Name  string `json:"name"`
	Relay int    `json:"relay"`
}

// relayLampStatus — снимок состояния лампы для отдачи наружу (Redis/дашборд).
type relayLampStatus struct {
	Name  string    `json:"name"`
	Relay int       `json:"relay"`
	State lampState `json:"state"`
}

// relaySection — раздел "relay" sunReceiver.json: управление сетевым реле SR-201
// (лампы). Disabled — ОБЯЗАТЕЛЬНОЕ поле (отсутствие = ошибка конфига): false —
// контроллер ламп запускается; true — не запускается (раздел в конфиге,
// но без управления реле).
//
// Параметры анализа (окна протухания, имена тегов и пороги) вынесены в конфиг,
// чтобы управление лампами настраивалось без правки пулеров: данные по-прежнему
// берутся из Redis, а последовательность правил (логика) остаётся в коде.
type relaySection struct {
	IP        string         `json:"ip"`        // IP реле SR-201
	UDPPort   int            `json:"udp_port"`  // порт UDP команд (дефолт 6723)
	BlinkHz   float64        `json:"blink_hz"`  // частота мигания, Гц (дефолт 2)
	Keepalive string         `json:"keepalive"` // интервал повторной отправки steady-команды (duration-строка, дефолт "2s")
	Lamps     []relayLampCfg `json:"lamps"`     // список ламп; пусто = белая(1), красная(2)
	Disabled  *bool          `json:"disabled"`

	// Параметры анализа состояния (дефолты — в скобках).
	MeterStaleSec     int     `json:"meter_stale_sec"`     // окно свежести снэпшота счётчика, сек (20)
	MapStaleSec       int     `json:"map_stale_sec"`       // окно свежести снэпшота МАП, сек (20)
	MeterPowerTag     string  `json:"meter_power_tag"`     // тег активной мощности счётчика ("meter_active_power")
	MeterVoltTag      string  `json:"meter_voltage_tag"`   // тег напряжения счётчика ("meter_voltage")
	MapGridTag        string  `json:"map_grid_tag"`        // тег напряжения на входе МАП ("grid_voltage")
	VoltagePresentMin float64 `json:"voltage_present_min"` // порог «напряжение есть», В (по умолч. 0 — >0)
}

// relayController — владелец физических реле SR-201. Desired-состояния ламп
// задаются из других модулей (SetLamp/SetRelayLamp); фоновый цикл приводит их в
// исполнение по UDP. Единственная горутина (runRelayControl) пишет в socket.
type relayController struct {
	cfg   *relaySection
	store *redisStore

	mu       sync.Mutex
	desired  []lampState // желаемое состояние каждой лампы (по индексу Lamps)
	blinkOn  []bool      // фаза мигания (переключается каждый полупериод)
	lastCmd  []string    // последняя отправленная команда на канал
	lastSent time.Time   // время последней отправки
	clock    func() time.Time

	dialMu sync.Mutex
	conn   net.Conn // UDP, подключён к реле

	// sendCmd — переопределяемая отправка (для тестов); nil = реальная запись в conn.
	sendCmd func(string)
}

// package-level handle: другие модули обращаются через SetRelayLamp.
var relayCtl *relayController

// SetRelayLamp — публичный API для других модулей: переключает лампу по имени в
// заданное состояние, сохраняя текущее состояние второй лампы. Возвращает ошибку,
// если контроллер не настроен или имя лампы/состояние неизвестно.
func SetRelayLamp(relayName string, st lampState) error {
	if relayCtl == nil {
		return fmt.Errorf("relay: контроллер ламп не настроен (нет активного раздела relay)")
	}
	return relayCtl.SetLampByName(relayName, st)
}

// buildRelayCfg нормализует конфиг: дефолты порта/частоты/keepalive/ламп и
// параметров анализа состояния.
func buildRelayCfg(c *relaySection) *relaySection {
	if c == nil {
		return nil
	}
	cfg := *c
	if cfg.UDPPort == 0 {
		cfg.UDPPort = relayUDPDefaultPort
	}
	if cfg.BlinkHz <= 0 {
		cfg.BlinkHz = relayDefaultBlinkHz
	}
	if cfg.Keepalive == "" {
		cfg.Keepalive = relayDefaultKeepalive.String()
	}
	if cfg.MeterStaleSec <= 0 {
		cfg.MeterStaleSec = int(defaultStaleWindow / time.Second)
	}
	if cfg.MapStaleSec <= 0 {
		cfg.MapStaleSec = int(defaultStaleWindow / time.Second)
	}
	if cfg.MeterPowerTag == "" {
		cfg.MeterPowerTag = defaultMeterPowerTag
	}
	if cfg.MeterVoltTag == "" {
		cfg.MeterVoltTag = defaultMeterVoltageTag
	}
	if cfg.MapGridTag == "" {
		cfg.MapGridTag = defaultMapGridTag
	}
	if cfg.VoltagePresentMin <= 0 {
		cfg.VoltagePresentMin = 0 // порог «>0» (напряжение есть, когда больше нуля)
	}
	// По умолчанию — белая лампа на реле 1, красная на реле 2 (аппаратная привязка).
	if len(cfg.Lamps) == 0 {
		cfg.Lamps = []relayLampCfg{{Name: "white", Relay: 1}, {Name: "red", Relay: 2}}
	}
	return &cfg
}

// meterStale возвращает окно свежести снэпшота счётчика с учётом дефолта.
func (c *relaySection) meterStale() time.Duration {
	if c == nil || c.MeterStaleSec <= 0 {
		return defaultStaleWindow
	}
	return time.Duration(c.MeterStaleSec) * time.Second
}

// mapStale возвращает окно свежести снэпшота МАП с учётом дефолта.
func (c *relaySection) mapStale() time.Duration {
	if c == nil || c.MapStaleSec <= 0 {
		return defaultStaleWindow
	}
	return time.Duration(c.MapStaleSec) * time.Second
}

// meterPowerTag возвращает тег активной мощности счётчика.
func (c *relaySection) meterPowerTag() string {
	if c == nil || c.MeterPowerTag == "" {
		return defaultMeterPowerTag
	}
	return c.MeterPowerTag
}

// meterVoltTag возвращает тег напряжения счётчика.
func (c *relaySection) meterVoltTag() string {
	if c == nil || c.MeterVoltTag == "" {
		return defaultMeterVoltageTag
	}
	return c.MeterVoltTag
}

// mapGridTag возвращает тег напряжения на входе МАП.
func (c *relaySection) mapGridTag() string {
	if c == nil || c.MapGridTag == "" {
		return defaultMapGridTag
	}
	return c.MapGridTag
}

// voltagePresentMin возвращает порог «напряжение есть» (по умолчанию 0 = > 0).
func (c *relaySection) voltagePresentMin() float64 {
	if c == nil || c.VoltagePresentMin < 0 {
		return 0
	}
	return c.VoltagePresentMin
}

// newRelayController создаёт контроллер по нормализованному конфигу.
func newRelayController(c *relaySection, store *redisStore) *relayController {
	cfg := buildRelayCfg(c)
	if cfg == nil {
		return nil
	}
	rc := &relayController{
		cfg:      cfg,
		store:    store,
		desired:  make([]lampState, len(cfg.Lamps)),
		blinkOn:  make([]bool, len(cfg.Lamps)),
		lastCmd:  make([]string, len(cfg.Lamps)),
		lastSent: time.Time{},
		clock:    time.Now,
	}
	rc.persist()
	return rc
}

// commandFor возвращает steady-команду для состояния (для blink — пустая строка,
// мигание обрабатывает фоновый цикл). relay — 1-индексный номер канала.
func commandFor(relay int, st lampState) string {
	n := strconv.Itoa(relay)
	switch st {
	case lampOn:
		return relayCmdOnPrefix + n
	case lampOff:
		return relayCmdOffPrefix + n
	}
	return ""
}

// SetLampByName переключает лампу по имени в заданное состояние.
func (c *relayController) SetLampByName(name string, st lampState) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i, lamp := range c.cfg.Lamps {
		if lamp.Name == name {
			return c.setLampLocked(i, st)
		}
	}
	return fmt.Errorf("relay: неизвестная лампа %q (доступны: %s)", name, c.lampNamesLocked())
}

// SetLamp переключает лампу по индексу Lamps.
func (c *relayController) SetLamp(slot int, st lampState) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if slot < 0 || slot >= len(c.cfg.Lamps) {
		return fmt.Errorf("relay: некорректный индекс лампы %d (0..%d)", slot, len(c.cfg.Lamps)-1)
	}
	return c.setLampLocked(slot, st)
}

// setLampLocked задаёт желаемое состояние и, для steady, сразу отправляет команду.
// Требует удержанного c.mu.
func (c *relayController) setLampLocked(slot int, st lampState) error {
	if c.desired[slot] == st {
		return nil
	}
	prev := c.desired[slot]
	c.desired[slot] = st
	c.blinkOn[slot] = false
	cmd := commandFor(c.cfg.Lamps[slot].Relay, st)
	if cmd != "" && cmd != c.lastCmd[slot] {
		c.lastCmd[slot] = cmd
		c.lastSent = c.clock()
		c.mu.Unlock()
		c.send(cmd)
		c.mu.Lock()
	}
	c.mu.Unlock()
	// Логируем ТОЛЬКО смену желаемого состояния лампы; фоновые переключения/
	// повторы (мигание, keepalive) не пишем.
	log.Printf("relay: лампа %q → %s (было %s)", c.cfg.Lamps[slot].Name, st.String(), prev.String())
	c.mu.Lock()
	c.persistLocked()
	return nil
}

// State возвращает текущее (желаемое) состояние лампы по индексу.
func (c *relayController) State(slot int) lampState {
	c.mu.Lock()
	defer c.mu.Unlock()
	if slot < 0 || slot >= len(c.desired) {
		return lampOff
	}
	return c.desired[slot]
}

// Snapshot возвращает снимок всех ламп (для Redis/дашборда).
func (c *relayController) Snapshot() []relayLampStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]relayLampStatus, len(c.cfg.Lamps))
	for i, lamp := range c.cfg.Lamps {
		out[i] = relayLampStatus{Name: lamp.Name, Relay: lamp.Relay, State: c.desired[i]}
	}
	return out
}

func (c *relayController) lampNamesLocked() string {
	s := ""
	for i, lamp := range c.cfg.Lamps {
		if i > 0 {
			s += ", "
		}
		s += lamp.Name
	}
	return s
}

// runRelayControl — фоновый цикл приведения реле к желаемым состояниям. Тикает
// каждый полупериод мигания: мигающие каналы переключаются, steady — повторно
// отправляются через keepalive-интервал (защита от потери UDP-пакета).
// Останавливается по отмене stop.
func runRelayControl(c *relayController, stop context.Context) {
	if c == nil {
		return
	}
	blinkHz := c.cfg.BlinkHz
	if blinkHz <= 0 {
		blinkHz = relayDefaultBlinkHz
	}
	half := time.Duration(float64(time.Second) / (2 * blinkHz))
	if half < 10*time.Millisecond {
		half = 10 * time.Millisecond
	}
	keep := relayDefaultKeepalive
	if d, err := time.ParseDuration(c.cfg.Keepalive); err == nil && d > 0 {
		keep = d
	}
	// Первая итерация — сразу после старта, затем по тику.
	ticker := time.NewTicker(half)
	defer ticker.Stop()
	c.tick(c.clock(), keep)
	for {
		select {
		case <-stop.Done():
			return
		case now := <-ticker.C:
			c.tick(now, keep)
		}
	}
}

// tick приводит реле к желаемым состояниям на момент now.
func (c *relayController) tick(now time.Time, keep time.Duration) {
	c.mu.Lock()
	for i, lamp := range c.cfg.Lamps {
		st := c.desired[i]
		send := false
		var cmd string
		switch st {
		case lampBlink:
			c.blinkOn[i] = !c.blinkOn[i]
			if c.blinkOn[i] {
				cmd = commandFor(lamp.Relay, lampOn)
			} else {
				cmd = commandFor(lamp.Relay, lampOff)
			}
			send = true
		default:
			cmd = commandFor(lamp.Relay, st)
		}
		if cmd == "" {
			continue
		}
		if st != lampBlink {
			if cmd != c.lastCmd[i] {
				send = true
			} else if now.Sub(c.lastSent) >= keep {
				send = true
			}
		}
		if send {
			// lastCmd[i] меняется всеми отправками (в т.ч. миганием), но мигание
			// всегда шлёт по send=true, так что отслеживание не «сливается».
			c.lastCmd[i] = cmd
			c.lastSent = now
			c.mu.Unlock()
			c.send(cmd)
			c.mu.Lock()
		}
	}
	c.mu.Unlock()
}

// send отправляет одну команду на реле по UDP. При ошибке сокета — переподключается
// и повторяет попытку один раз (устойчивость к смене сети/ребуту реле).
func (c *relayController) send(cmd string) {
	if cmd == "" {
		return
	}
	if c.sendCmd != nil {
		c.sendCmd(cmd)
		return
	}
	for attempt := 0; attempt < 2; attempt++ {
		if c.conn == nil {
			c.dial()
		}
		if c.conn == nil {
			return
		}
		if _, err := c.conn.Write([]byte(cmd)); err != nil {
			log.Printf("relay: UDP %v: %v", c.cfg.IP, err)
			c.drop()
			continue
		}
		return
	}
}

func (c *relayController) dial() {
	c.dialMu.Lock()
	defer c.dialMu.Unlock()
	if c.conn != nil {
		return
	}
	addr := &net.UDPAddr{IP: net.ParseIP(c.cfg.IP), Port: c.cfg.UDPPort}
	conn, err := net.DialUDP("udp", nil, addr)
	if err != nil {
		log.Printf("relay: dial UDP %s:%d: %v", c.cfg.IP, c.cfg.UDPPort, err)
		return
	}
	c.conn = conn
}

func (c *relayController) drop() {
	c.dialMu.Lock()
	defer c.dialMu.Unlock()
	if c.conn != nil {
		_ = c.conn.Close()
		c.conn = nil
	}
}

// persist записывает желаемое состояние ламп в Redis (HASH sunreceiver:relay).
func (c *relayController) persist() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.persistLocked()
}

func (c *relayController) persistLocked() {
	if c.store == nil || c.store.rdb == nil || c.store.ctx == nil {
		return
	}
	snap := make([]relayLampStatus, len(c.cfg.Lamps))
	for i, lamp := range c.cfg.Lamps {
		snap[i] = relayLampStatus{Name: lamp.Name, Relay: lamp.Relay, State: c.desired[i]}
	}
	b, err := json.Marshal(snap)
	if err != nil {
		log.Printf("relay: marshal snapshot: %v", err)
		return
	}
	if err := c.store.rdb.Set(c.store.ctx, redisRelayKey, b, 0).Err(); err != nil {
		log.Printf("relay: redis set %s: %v", redisRelayKey, err)
	}
}

// ---- Контроллер красной лампы (индикатор отдачи в сеть) ----

// redLampName — имя лампы-индикатора (в конфиге по умолчанию реле 2).
const redLampName = "red"

// runRelayLampController — фоновый цикл управления красной лампой на основе
// текущего состояния счётчика (снэпшот meter_* в Redis current). Каждую секунду
// принимает решение по последовательному алгоритму; лампа переключается только
// при фактическом изменении состояния (SetLampByName — no-op при том же самом).
// Параметры (окно протухания, тег мощности, направление) берутся из конфига.
//
// Последовательный перебор (первое истинное условие решает, далее не проверяется):
//  1. Модуль счётчика отключён (meterCfg == nil)                  → лампа НЕ горит;
//  2. Счётчик недоступен (снэпшот отсутствует/старше окна)       → лампа МИГАЕТ;
//  3. Мощность сети ПОЛОЖИТЕЛЬНАЯ или НУЛЕВАЯ (потребление/0)    → лампа НЕ горит;
//  4. Мощность сети ОТРИЦАТЕЛЬНАЯ (отдача в сеть)                → лампа ГОРИТ.
func runRelayLampController(store *redisStore, meterCfg *meterConfig, c *relayController, stop context.Context) {
	if c == nil {
		return
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			c.SetLampByName(redLampName, redLampDecision(store, c.cfg, meterCfg))
		case <-stop.Done():
			return
		}
	}
}

// relaySnapshotLoader — чтение последнего снимка устройства из current-HASH по IP
// (интерфейс для тестируемости; *redisStore реализует его через CurrentOne).
type relaySnapshotLoader interface {
	CurrentOne(ip string) (deviceSnapshot, error)
}

// snapshotStale — снэпшот недоступен (отсутствует) или его время старше окна.
func snapshotStale(snap deviceSnapshot, window time.Duration) bool {
	ts, err := time.Parse(time.RFC3339, snap.Timestamp)
	if err != nil {
		return true
	}
	return time.Since(ts) > window
}

// redLampDecision вычисляет желаемое состояние красной лампы по алгоритму выше.
// Параметры окна/тега/направления — из cfg (relaySection), данные из Redis.
func redLampDecision(loader relaySnapshotLoader, cfg *relaySection, meterCfg *meterConfig) lampState {
	// 1. Модуль счётчика отключён.
	if meterCfg == nil {
		return lampOff
	}
	// 2. Счётчик недоступен: нет снэпшота или он старше окна.
	snap, err := loader.CurrentOne(meterCfg.IP)
	if err != nil {
		return lampBlink
	}
	if snapshotStale(snap, cfg.meterStale()) {
		return lampBlink
	}
	// 3./4. Знак активной мощности сети (+ потребление, 0 — нет потока, − отдача).
	p, ok := valueAsFloat(snap.Values[cfg.meterPowerTag()])
	if !ok {
		return lampBlink
	}
	if p >= 0 {
		// Мощность положительная или НУЛЕВАЯ — лампа не горит (горит только при отдаче).
		return lampOff
	}
	return lampOn
}

// ---- Контроллер белой лампы (индикатор наличия напряжения сети) ----

// whiteLampName — имя лампы-индикатора (в конфиге по умолчанию реле 1).
const whiteLampName = "white"

// runWhiteLampController — фоновый цикл управления белой лампой на основе данных
// МАП (grid_voltage) и счётчика (meter_voltage) о напряжении сети. Каждую секунду
// принимает решение по последовательному алгоритму (первое условие решает).
// Параметры (окна протухания, теги, порог) — из конфига.
//  1. МАП недоступен или отключён (mapIP == "" или снэпшот протух/отсутствует)
//     → лампа НЕ горит;
//  2. Напряжение на входе МАП есть (grid_voltage > порога)      → лампа ГОРИТ;
//  3. Напряжения на входе МАП нет, но на счётчике оно есть И счётчик доступен
//     (снэпшот свежий, meter_voltage > порога)                  → лампа МИГАЕТ;
//  4. Напряжения на входе МАП нет, и счётчик недоступен
//     (снэпшот отсутствует/протух или нет напряжения)          → лампа НЕ горит.
func runWhiteLampController(store *redisStore, mapIP string, meterCfg *meterConfig, c *relayController, stop context.Context) {
	if c == nil {
		return
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			c.SetLampByName(whiteLampName, whiteLampDecision(store, c.cfg, mapIP, meterCfg))
		case <-stop.Done():
			return
		}
	}
}

// whiteLampDecision вычисляет желаемое состояние белой лампы по алгоритму выше.
// Параметры окна/тегов/порога — из cfg (relaySection), данные из Redis.
func whiteLampDecision(loader relaySnapshotLoader, cfg *relaySection, mapIP string, meterCfg *meterConfig) lampState {
	min := cfg.voltagePresentMin()
	// 1. МАП недоступен (не настроен/отключён) — лампа не горит.
	if mapIP == "" {
		return lampOff
	}
	mapSnap, err := loader.CurrentOne(mapIP)
	if err != nil || snapshotStale(mapSnap, cfg.mapStale()) {
		return lampOff
	}
	// 2. Напряжение на входе МАП есть — лампа горит.
	gridV, ok := valueAsFloat(mapSnap.Values[cfg.mapGridTag()])
	if ok && gridV > min {
		return lampOn
	}
	// 3./4. Напряжения на входе МАП нет — смотрим счётчик.
	if meterCfg == nil {
		return lampOff
	}
	meterSnap, err := loader.CurrentOne(meterCfg.IP)
	if err != nil || snapshotStale(meterSnap, cfg.meterStale()) {
		// 4. Счётчик недоступен — лампа не горит.
		return lampOff
	}
	meterV, ok := valueAsFloat(meterSnap.Values[cfg.meterVoltTag()])
	if !ok || meterV <= min {
		return lampOff
	}
	// 3. Счётчик доступен и напряжение есть — лампа мигает.
	return lampBlink
}

// valueAsFloat безопасно приводит значение (в valuesContract это any/float64) к float64.
func valueAsFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case float32:
		return float64(x), true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	}
	return 0, false
}

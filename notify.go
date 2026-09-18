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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// Уведомления в мессенджер MAX (через Bot API). Отправляются по событиям
// мониторинга МАП Титанатор:
//   - МАП недоступен (API Малины недоступно / отдаёт устаревшее время и оно не
//     изменяется / опрос RS232/Modbus не удаётся);
//   - МАП показывает, что напряжение сети отсутствует либо ниже порога (195 В),
//     со справочным напряжением электросчётчика, если он опрашивается;
//   - события восстановления по обоим случаям.
//
// Мониторинг работает ТОЛЬКО если включён опрос МАП в целом (map.disabled != true
// и есть источник МАП: RS232/Modbus или веб-API ПАК «Малина»). Если опрос МАП
// выключен — уведомления не формируются.

// maxAPIBase — базовый URL API MAX (для ботов); токен передаётся только в
// заголовке Authorization. Переменная (не константа), чтобы тесты могли
// подставлять локальный httptest-сервер.
var maxAPIBase = "https://platform-api2.max.ru"

// Таймаут одного HTTP-запроса к API MAX (короче 5-сек клиента, чтобы цикл
// монитора не блокировался надолго).
const maxRequestTimeout = 5 * time.Second

// Пороги/окна мониторинга МАП (по умолчанию, переопределяются в разделе notify).
const (
	// defaultMapUndeclaredWindow — окно, в течение которого отсутствие валидного
	// опроса МАП считается недоступностью. Покрывает Modbus-опрос МАП (~9 с:
	// три последовательных чтения блоков) с запасом; для API — 1 с.
	defaultMapUndeclaredWindow = 20 * time.Second
	// defaultStableWindow — «стабильность» перед отправкой события: авария и
	// восстановление фиксируются только после непрерывного удержания состояния
	// в течение этого окна (защита от дребезга и дедупликация).
	defaultStableWindow = 30 * time.Second
	// defaultGridVoltageLow — порог напряжения сети МАП, ниже которого напряжение
	// считается пропавшим (низким). Значение 195 В.
	defaultGridVoltageLow = 195.0
	// maxAPITimeBehind — насколько поле timestamp ответа read_json.php?device=map
	// может «отставать» от реального времени, прежде чем считаться устаревшим.
	maxAPITimeBehind = 5 * time.Minute
	// notifyPollEvery — период чтения состояния в мониторе.
	notifyPollEvery = 5 * time.Second
	// meterFreshWindow — окно свежести снимка электросчётчика в current. Если снимок
	// старше (счётчик перестал отдавать данные — например, обесточен и не отвечает),
	// его напряжение НЕ показывается справочно в алерте: последний успешно снятый
	// снимок остаётся в Redis, но отражает устаревшее состояние, а не текущее.
	// Счётчик опрашивается раз в секунду, поэтому окно с большим запасом.
	meterFreshWindow = 30 * time.Second
)

// notifySection — раздел "notify" sunReceiver.json: настройка отправки уведомлений
// в MAX. token — обязателен; адресат задаётся user_id (личный диалог) или chat_id
// (чат/канал). Поля порогов необязательны (0 — дефолт).
type notifySection struct {
	Token             string `json:"token"`
	UserID            string `json:"user_id"`
	ChatID            string `json:"chat_id"`
	Disabled          *bool  `json:"disabled"` // true — уведомления выключены (раздел в конфиге, но без оповещений)
	StableWindowSec   int    `json:"stable_window_sec"`
	MapUndeclaredSec  int    `json:"map_undeclared_sec"`
	GridVoltageLow    float64 `json:"grid_voltage_low"`
}

// notifyCfg — глобально заполненный раздел notify (аналогично mapAPI/mppt).
var notifyCfg *notifySection

// maxClient — клиент отправки сообщений в MAX (Bot API). Однопоточный монитор,
// поэтому мьютекс не нужен; send работает с учётом лимита 2 сообщения/сек на чат.
type maxClient struct {
	token  string
	userID string
	chatID string
	hc     *http.Client
}

func newMaxClient(n *notifySection) *maxClient {
	return &maxClient{
		token:  n.Token,
		userID: n.UserID,
		chatID: n.ChatID,
		hc:     &http.Client{Timeout: maxRequestTimeout},
	}
}

// send отправляет текстовое сообщение адресату. Возвращает ошибку при HTTP != 200
// или пустом теле (201/200 с message — успех).
func (m *maxClient) send(ctx context.Context, text string) error {
	u := maxAPIBase + "/messages"
	q := url.Values{}
	if m.userID != "" {
		q.Set("user_id", m.userID)
	} else {
		q.Set("chat_id", m.chatID)
	}
	body, err := json.Marshal(map[string]any{"text": text})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u+"?"+q.Encode(), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", m.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := m.hc.Do(req)
	if err != nil {
		return fmt.Errorf("max send: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("max send: http %d: %s", resp.StatusCode, string(b))
	}
	// Подтверждение доставки — успешное тело с message (mid).
	io.Copy(io.Discard, resp.Body)
	return nil
}

// mapTrack — потокобезопасное состояние последних опросов МАП, заполняется
// poll-функциями (Modbus и веб-API) и читается монитором уведомлений.
type mapTrack struct {
	mu sync.Mutex

	source string // "modbus" | "api" — последний источник, давший опрос

	lastOK    time.Time // время последнего ВАЛИДНОГО опроса (данные получены)
	lastErr   string    // текст последней ошибки опроса (пусто, если последний ок)
	lastErrAt time.Time

	// Устаревшее время / его неизменность — признак зависшего веб-API Малины.
	apiTS       int64     // последний timestamp из ответа device=map
	apiTSChange time.Time // когда timestamp последний раз изменился

	gridHas  bool
	gridVolt float64 // напряжение сети МАП из последнего валидного снимка
}

// trackOK фиксирует успешный опрос МАП с валидными данными.
func (t *mapTrack) trackOK(source string, now time.Time, gridHas bool, gridVolt float64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.source = source
	t.lastOK = now
	t.lastErr = ""
	t.gridHas = gridHas
	t.gridVolt = gridVolt
}

// trackErr фиксирует НЕудачный опрос МАП с текстом причины.
func (t *mapTrack) trackErr(source, reason string, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.source = source
	t.lastErr = reason
	t.lastErrAt = now
}

// trackAPITS регистрирует timestamp ответа веб-API Малины (device=map). Используется
// для детекции «отдаёт сильно устаревшее время и оно не изменяется».
func (t *mapTrack) trackAPITS(ts int64, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if ts == t.apiTS {
		return // значение не менялось — apiTSChange не трогаем
	}
	t.apiTS = ts
	t.apiTSChange = now
}

// status возвращает текущее состояние МАП:
//
//	""      — МАП доступен (последние данные валидны);
//	"stale" — веб-API отдаёт устаревшее время, и оно не изменяется;
//	"down"  — МАП недоступен (нет валидных данных дольше undeclared window).
func (t *mapTrack) status(now time.Time, undeclared time.Duration) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.source == "api" && t.apiTSChange != (time.Time{}) {
		// Время из ответа отстаёт от реального и не менялось — зависший API.
		if now.Sub(t.apiTSChange) >= undeclared && now.Sub(time.Unix(t.apiTS, 0)) > maxAPITimeBehind {
			return "stale"
		}
	}
	if t.lastOK.IsZero() || now.Sub(t.lastOK) > undeclared {
		return "down"
	}
	return ""
}

// lastError возвращает текст последней ошибки (для сообщения).
func (t *mapTrack) lastError() (string, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.lastErr, t.lastErr != ""
}

// gridVoltage возвращает последнее напряжение сети МАП (и признак наличия).
func (t *mapTrack) gridVoltage() (float64, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.gridVolt, t.gridHas
}

// mapTracker — глобальный трекер состояния МАП (обновляется poll-функциями).
var mapTracker mapTrack

// ---- монитор событий ----

// mapEventKind — тип отслеживаемого события МАП.
type mapEventKind int

const (
	// eventMapDown — МАП недоступен.
	eventMapDown mapEventKind = iota
	// eventMapNoVoltage — напряжение сети МАП отсутствует/ниже порога.
	eventMapNoVoltage
)

// alertDetector отлавливает одно событие с гистерезисом (стабильность stableWindow)
// и дедупликацией: отправляет сообщение при переходе в аварию и одно при возврате
// в норму, не повторяясь, пока состояние не сменилось.
type alertDetector struct {
	kind      mapEventKind
	stable    time.Duration
	lastDown  bool // в состоянии аварии
	downSince time.Time
	lastUp    bool // в состоянии нормы (после аварии)
	upSince   time.Time
	sentDown  bool // аварийное сообщение доставлено (одно на переход)
	// pendingDown/pendingUp — «outbox» детектора: текст пограничного сообщения,
	// сформированный, но ещё НЕ доставленный. Хранится до успешной отправки и
	// повторяется каждый такт (ретрай), чтобы не потерять алерт при сбое сети/MAX.
	pendingDown string
	pendingUp   string
}

// evaluate принимает текущий признак аварии и возвращает сообщение, которое нужно
// отправить сейчас (и его направление), либо ok=false. Недоставленное ранее
// пограничное сообщение (pending) возвращается приоритетно до подтверждения.
func (d *alertDetector) evaluate(now time.Time, alarm bool, buildMsg func(alarm bool) (string, bool)) (msg string, isDown bool, ok bool) {
	// Сначала доставляем неотправленную аварию (приоритетно), затем восстановление.
	if d.pendingDown != "" {
		return d.pendingDown, true, true
	}
	if d.pendingUp != "" {
		return d.pendingUp, false, true
	}
	if alarm {
		if !d.lastDown {
			d.lastDown = true
			d.downSince = now
			d.sentDown = false
		}
		if d.lastUp {
			d.lastUp = false
			d.upSince = time.Time{}
		}
		if !d.sentDown && now.Sub(d.downSince) >= d.stable {
			if msg, b := buildMsg(true); b {
				d.pendingDown = msg
				return msg, true, true
			}
		}
		return "", false, false
	}
	// Норма.
	if !d.lastUp {
		d.lastUp = true
		d.upSince = now
	}
	if d.lastDown && now.Sub(d.upSince) >= d.stable {
		if msg, b := buildMsg(false); b {
			d.pendingUp = msg
			return msg, false, true
		}
	}
	return "", false, false
}

// confirmDispatched вызывается ПОСЛЕ успешной отправки сообщения: помечает его
// доставленным и очищает outbox детектора. Если отправка не удалась — confirm не
// вызывается, и evaluate продолжит возвращать то же сообщение на следующем такте.
// Для восстановления дополнительно закрывается авария (lastDown=false), чтобы
// следующая авария анализировалась заново.
func (d *alertDetector) confirmDispatched(isDown bool) {
	if isDown {
		d.pendingDown = ""
		d.sentDown = true
		return
	}
	d.pendingUp = ""
	d.lastDown = false
	d.downSince = time.Time{}
	d.upSince = time.Time{}
	d.sentDown = false
}

// reset возвращает детектор в исходное состояние без отправки сообщения
// (используется, когда оценка события временно невозможна, например МАП в моменте
// недоступен и событие напряжения обработает ветка недоступности).
func (d *alertDetector) reset() {
	d.lastDown = false
	d.downSince = time.Time{}
	d.lastUp = false
	d.upSince = time.Time{}
	d.sentDown = false
	d.pendingDown = ""
	d.pendingUp = ""
}

// monitorState — поведение монитора, персистентное между итерациями.
type monitorState struct {
	track       *mapTrack
	client      *maxClient
	undeclared  time.Duration
	stable      time.Duration
	gridLow     float64
	meterIP     string // IP счётчика (для справочного напряжения); "" — не опрашивается
	store       *redisStore
	down        alertDetector
	noVolt      alertDetector
	mapIP       string // devKey МАП (для чтения снимка)
	name        string // логическое имя МАП
}

func newMonitorState(store *redisStore, n *notifySection, mapIP, name string) *monitorState {
	stable := defaultStableWindow
	if n.StableWindowSec > 0 {
		stable = time.Duration(n.StableWindowSec) * time.Second
	}
	undeclared := defaultMapUndeclaredWindow
	if n.MapUndeclaredSec > 0 {
		undeclared = time.Duration(n.MapUndeclaredSec) * time.Second
	}
	gridLow := defaultGridVoltageLow
	if n.GridVoltageLow > 0 {
		gridLow = n.GridVoltageLow
	}
	return &monitorState{
		track:      &mapTracker,
		client:     newMaxClient(n),
		undeclared: undeclared,
		stable:     stable,
		gridLow:    gridLow,
		store:      store,
		mapIP:      mapIP,
		name:       name,
		down:       alertDetector{kind: eventMapDown, stable: stable},
		noVolt:     alertDetector{kind: eventMapNoVoltage, stable: stable},
	}
}

// runNotifyMonitor — цикл мониторинга МАП и отправки уведомлений в MAX. Запускается
// ТОЛЬКО когда опрос МАП включён (иначе держать монитор незачем).
func runNotifyMonitor(store *redisStore, n *notifySection, mapIP, name, meterIP string, stop context.Context) {
	if n == nil {
		return
	}
	st := newMonitorState(store, n, mapIP, name)
	st.meterIP = meterIP
	ticker := time.NewTicker(notifyPollEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			st.iterate(time.Now())
		case <-stop.Done():
			return
		}
	}
}

// iterate — один такт мониторинга: оценивает состояние МАП и отправляет сообщения.
func (m *monitorState) iterate(now time.Time) {
	statusStr := m.track.status(now, m.undeclared)
	down := statusStr != ""
	m.downReport(now, down, statusStr)
	m.voltReport(now, down)
}

// downReport — событие «МАП недоступен» + восстановление.
func (m *monitorState) downReport(now time.Time, down bool, statusStr string) {
	msg, isDown, ok := m.down.evaluate(now, down, func(alarm bool) (string, bool) {
		if alarm {
			return m.downMessage(statusStr), true
		}
		return m.downRecoverMessage(), true
	})
	if ok {
		m.dispatch(&m.down, msg, isDown)
	}
}

// downMessage строит текст уведомления о недоступности МАП, конкретизируя причину.
func (m *monitorState) downMessage(statusStr string) string {
	reason := mapDownReason(m.track, statusStr)
	return fmt.Sprintf("⚠️ МАП (%s) недоступен: %s", m.name, reason)
}

// downRecoverMessage — текст восстановления МАП.
func (m *monitorState) downRecoverMessage() string {
	return fmt.Sprintf("✅ МАП (%s) снова доступен", m.name)
}

// mapDownReason возвращает конкретизированную причину недоступности МАП.
func mapDownReason(t *mapTrack, statusStr string) string {
	switch statusStr {
	case "stale":
		return "веб-API ПАК «Малина» отдаёт сильно устаревшее время, и оно не изменяется"
	case "down":
		reason, ok := t.lastError()
		if ok {
			return reason
		}
		return "нет валидных данных от устройства"
	}
	return "нет валидных данных от устройства"
}

// voltReport — событие «напряжение сети МАП отсутствует/ниже порога» + восстановление.
func (m *monitorState) voltReport(now time.Time, estimatedDown bool) {
	// Оценка только когда МАП доступен (иначе уже обработано событием недоступности).
	if estimatedDown {
		// МАП недоступен — оценку напряжения временно откладываем: сбрасываем
		// состояние без отправки, чтобы после восстановления МАП повторное
		// падение напряжения обработалось как новая авария (а не как её
		// продолжение с уже отправленным сообщением).
		m.noVolt.reset()
		return
	}
	grid, has := m.track.gridVoltage()
	alarm := !has || grid < m.gridLow
	msg, isDown, ok := m.noVolt.evaluate(now, alarm, func(alarm bool) (string, bool) {
		if alarm {
			return m.noVoltMessage(grid, has, now), true
		}
		return m.noVoltRecoverMessage(), true
	})
	if ok {
		m.dispatch(&m.noVolt, msg, isDown)
	}
}

// Состояние счётчика для сообщения (по свежести снэпшота).
const (
	meterStateNone  = iota // счётчик не опрашивается / нет снэпшота
	meterStateFresh        // снэпшот свежий — есть актуальное напряжение
	meterStateStale        // снэпшот устарел — счётчик молчит (не отвечает)
)

// meterInfo читает справочный снэпшот электросчётчика из current.
// Возвращает напряжение, время снэпшота и его состояние:
//   - meterStateNone — счётчик не опрашивается или снэпшота нет;
//   - meterStateFresh — снэпшот свежий (< meterFreshWindow), val актуально;
//   - meterStateStale — снэпшот устарел (счётчик молчит), ts — время снэпшота.
//
// Если счётчик обесточен и перестал отдавать данные, в Redis остаётся последний
// успешный снимок со старым timestamp — он устаревший (meterStateStale), и его
// напряжение в алерте не показывается (иначе выглядело бы «напряжение есть»).
func (m *monitorState) meterInfo(now time.Time) (val float64, ts time.Time, state int) {
	if m.meterIP == "" || m.store == nil {
		return 0, time.Time{}, meterStateNone
	}
	snap, err := m.store.CurrentOne(m.meterIP)
	if err != nil {
		return 0, time.Time{}, meterStateNone
	}
	return meterInfoFromSnap(snap, now)
}

// meterInfoFromSnap классифицирует снэпшот счётчика по свежести timestamp.
// Вынесено отдельно для юнит-теста.
func meterInfoFromSnap(snap deviceSnapshot, now time.Time) (val float64, ts time.Time, state int) {
	t, e := time.Parse(time.RFC3339, snap.Timestamp)
	if e != nil {
		return 0, time.Time{}, meterStateNone
	}
	if now.Sub(t) > meterFreshWindow {
		return 0, t, meterStateStale
	}
	if v, ok := snap.Values["meter_voltage"]; ok {
		if f, ok2 := toFloat(v); ok2 {
			return f, t, meterStateFresh
		}
	}
	return 0, t, meterStateStale
}

// noVoltMessage строит текст уведомления о пропадании напряжения сети МАП.
func (m *monitorState) noVoltMessage(grid float64, has bool, now time.Time) string {
	var cur string
	if !has || grid <= 0 {
		cur = "нет напряжения"
	} else {
		cur = fmt.Sprintf("напряжение %.0f В (ниже порога %.0f В)", grid, m.gridLow)
	}
	msg := fmt.Sprintf("⚠️ МАП (%s): %s", m.name, cur)
	if m.meterIP != "" {
		val, ts, state := m.meterInfo(now)
		switch state {
		case meterStateFresh:
			msg += fmt.Sprintf(". Напряжение счётчика: %.0f В", val)
		case meterStateStale:
			// Счётчик молчит — сообщаем, что он недоступен, и время последнего
			// успешного снэпшота (когда у него ещё было напряжение).
			msg += fmt.Sprintf(". Счётчик недоступен (последний снэпшот: %s)",
				ts.Local().Format("2006-01-02 15:04:05"))
		}
	}
	return msg
}

// noVoltRecoverMessage — текст восстановления напряжения сети МАП.
func (m *monitorState) noVoltRecoverMessage() string {
	return fmt.Sprintf("✅ МАП (%s): напряжение сети в норме", m.name)
}

// send отправляет сообщение в MAX. Возвращает ошибку, если отправка не удалась.
// Подтверждение доставки — ответ 200 (тело с message/mid).
func (m *monitorState) send(msg string) error {
	ctx, cancel := context.WithTimeout(context.Background(), maxRequestTimeout)
	defer cancel()
	if err := m.client.send(ctx, msg); err != nil {
		return err
	}
	return nil
}

// dispatch отправляет сообщение в MAX; при успехе подтверждает доставку детектору
// (пограничное состояние снимается). При неудаче — ничего не подтверждаем, и на
// следующем такте evaluate повторит отправку того же сообщения (outbox в памяти):
// алерт не теряется при сбое сети/MAX.
func (m *monitorState) dispatch(d *alertDetector, msg string, isDown bool) {
	if err := m.send(msg); err != nil {
		log.Printf("notify: отправка в MAX не удалась: %v", err)
		return
	}
	d.confirmDispatched(isDown)
	log.Printf("notify: отправлено в MAX: %s", msg)
}
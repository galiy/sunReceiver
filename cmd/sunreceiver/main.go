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
	"log"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Периоды опроса устройств (секунды в конфиге, раздел "poll"). Значения по
// умолчанию; переопределяются из sunReceiver.json при загрузке (см. applyPollConfig).
var (
	mapPollInterval   = time.Second // МАП + MPPT
	meterPollInterval = time.Second // счётчик DDS238
)

const (
	// mpptEmptyTolerance — сколько подряд идущих ПУСТЫХ успешных ответов КЭС
	// (API «Малина26») терпим ДО прунинга состава MPPT из current.
	// Одиночная/короткая пустота (перезапуск, транзиентный сбой) не должна
	// сносить все MPPT-ключи дашборда; пруним только при устойчивой пустоте.
	mpptEmptyTolerance = 3
)

type targetKind int

const (
	kindSofar targetKind = iota
	kindDeyeString
	kindMAP
	kindMPPT // MPPT-контроллер (КЭС) через API «Малина26»
)

// mpptOrderBase — базовый порядок для MPPT-контроллеров (динамических, из API
// «Малина26»): они всегда размещаются на дашборде последними, после всех
// статических устройств (МАП). Order = mpptOrderBase + slot.
const mpptOrderBase = 10000

// String — короткое имя типа для логов.
func (k targetKind) String() string {
	switch k {
	case kindSofar:
		return "sofar"
	case kindDeyeString:
		return "deye"
	case kindMAP:
		return "map"
	case kindMPPT:
		return "mppt"
	default:
		return "unknown"
	}
}

// invKindName возвращает значение поля deviceSnapshot.Kind для страницы анимации:
// "deye"/"sofar" для сетевых инверторов, "kes" для MPPT-контроллеров (КЭС);
// для МАП и счётчика — пустая строка (спрайт не требуется).
func invKindName(k targetKind) string {
	switch k {
	case kindSofar:
		return "sofar"
	case kindDeyeString:
		return "deye"
	case kindMPPT:
		return "kes"
	default:
		return ""
	}
}

// invTarget — внутренняя цель пулера МАП/КЭС (сетевые инверторы больше не
// описываются этим типом: они опрашиваются через grid_poller.go).
// Slot — для kindMAP: -1 (агрегат); для kindMPPT: индекс контроллера (слот) в ответе API.
type invTarget struct {
	IP       string
	Name     string
	LoggerSN uint32
	Kind     targetKind
	Slot     int
	UID      string // для kindMPPT: UID контроллера из API «Малина26» (стабильный идентификатор)
	Order    int    // порядок устройства на дашборде (индекс в конфиге; MPPT — всегда последними)
	// Placement — размещение (для grid заполняется из API в снимке; у МАП/КЭС пусто).
	Placement string
	// InverterSN — серийник (для grid — из API; для МАП/КЭС не используется).
	InverterSN string
}

// mapSection — раздел "map" sunReceiver.json (HTTP-API «Малина26»): токен по
// паролю + бинарные контракты МАП (`/api/map/raw`) и КЭС (`/api/kes/raw`).
// host — IP или имя; port — 80/443; serial — серийник МАП (число строкой; для grid
// не требуется), по нему
// ищем «свой» МАП в `/api/map/devices`; fallback_serial — необязательный резервный
// серийник (опрос при недоступности основного, идентичность остаётся основной).
//
// Disabled — ОБЯЗАТЕЛЬНОЕ поле (отсутствие = ошибка конфига): true отключает ВСЕ
// пулеры раздела map (МАП, КЭС, BMS26) и опрос сетевых инверторов через тот же API.
type mapSection struct {
	Disabled       *bool  `json:"disabled"`
	Host           string `json:"host"`
	Port           int    `json:"port"`
	Password       string `json:"password"`
	Serial         string `json:"serial"`
	FallbackSerial string `json:"fallback_serial"`
}

// dbConfig — расположение баз данных. Задаётся в sunReceiver.json в разделе "db".
// Пароль указывается прямо в pg-DSN (sunReceiver.json — приватный конфиг, в git не
// коммитится).
type dbConfig struct {
	Redis           string `json:"redis"`             // адрес Redis: host:port либо путь к unix-сокету (напр. /run/redis/redis.sock)
	PG              string `json:"pg"`                // DSN PostgreSQL (с паролем)
	PGRestoreWindow string `json:"pg_restore_window"` // окно РЕСТАВРАЦИИ Redis из PG (duration-строка, напр. "720h"); пусто — дефолт 30 суток
}

// meterSection — конфигурация электросчётчика DDS238, заданная в sunReceiver.json
// разделом "meter" (обратная совместимость — отдельный dds238.json).
// Disabled — ОБЯЗАТЕЛЬНОЕ поле (отсутствие = ошибка конфига): false = счётчик
// опрашивается; true = все пулеры счётчика отключены, плашки/кнопка «Электроэнергия»
// на дашборде скрыты.
// Protocol — тип протокола: "tcp" (Modbus TCP, по умолчанию — прямой опрос)
// или "rtu" (Modbus RTU поверх TCP — прозрачный шлюз, напр. USR-DR164).
type meterSection struct {
	Name        string `json:"name"`
	IP          string `json:"ip"`
	Port        int    `json:"port"`
	Unit        byte   `json:"unit"`
	FirstReg    uint16 `json:"first_reg"`
	RegisterCnt uint16 `json:"register_count"`
	Protocol    string `json:"protocol"`
	Disabled    *bool  `json:"disabled"`
}

// pollSection — периоды опроса устройств в СЕКУНДАХ (обязательный раздел "poll").
// Все поля обязательны и должны быть > 0 (отсутствие = ошибка загрузки конфига).
type pollSection struct {
	Map   int `json:"map"`   // МАП + MPPT
	Meter int `json:"meter"` // счётчик DDS238
}

type configFile struct {
	Poll          *pollSection     `json:"poll"`
	Map           *mapSection      `json:"map"`
	DB            *dbConfig        `json:"db"`
	Meter         *meterSection    `json:"meter"`
	Ce308         *ce308Section    `json:"ce308"`
	Dts017        *dts017Section   `json:"dts017m"`
	Mercury       []mercurySection `json:"mercury"` // ручные снятия показаний счётчика энергосбыта «Меркурий»
	Notify        *notifySection   `json:"notify"`
	Relay         *relaySection    `json:"relay"`          // сетевое реле SR-201 (лампы), управление по UDP
	DashboardPort int              `json:"dashboard_port"` // порт веб-дашборда; обязательное поле (0 — ошибка загрузки конфига)
	// SeriesTargetPoints — цель точек/серию на стороне БЭКА (downsample); обязательное.
	SeriesTargetPoints *int `json:"series_target_points,omitempty"`
	// SeriesRedisTargetPoints — цель серверной агрегации RedisTimeSeries (≤ backend);
	// обязательное. 0 — агрегация RT отключена.
	SeriesRedisTargetPoints *int `json:"series_redis_target_points,omitempty"`
	// Необязательные учётные данные HTTP Basic для `/api/*` веб-дашборда. Если оба
	// пусты — API открыт (обратный прокси закрывает доступ снаружи сам).
	DashboardUser     string `json:"dashboard_user,omitempty"`
	DashboardPassword string `json:"dashboard_password,omitempty"`
}

// applyPollConfig проверяет и применяет периоды опроса (в секундах) из
// обязательного раздела "poll". Любое отсутствующее/нулевое/отрицательное поле —
// ошибка загрузки конфига.
func applyPollConfig(p *pollSection) error {
	if p == nil {
		return fmt.Errorf("не задан обязательный раздел poll (периоды опроса устройств, секунды)")
	}
	fields := []struct {
		name string
		val  int
	}{
		{"map", p.Map},
		{"meter", p.Meter},
	}
	for _, f := range fields {
		if f.val <= 0 {
			return fmt.Errorf("обязательное поле poll.%s должно быть > 0 (секунды)", f.name)
		}
	}
	mapPollInterval = time.Duration(p.Map) * time.Second
	meterPollInterval = time.Duration(p.Meter) * time.Second
	return nil
}

// configPath — sunReceiver.json в каталоге исполняемого файла.
func configPath() string {
	exe, err := os.Executable()
	if err != nil {
		return "sunReceiver.json"
	}
	return filepath.Join(filepath.Dir(exe), "sunReceiver.json")
}

// loadConfig читает и проверяет sunReceiver.json, возвращает список целей
// (инверторы, без отключённых), настройки БД/счётчика/МАП и порт дашборда.
// rejectLegacyConfigFields явно отклоняет удалённые поля конфига: json.Unmarshal
// молча игнорирует неизвестные ключи, поэтому «старый вид» иначе загрузился бы
// без ошибки. Возвращает первую найденную legacy-настройку.
func rejectLegacyConfigFields(b []byte) error {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(b, &top); err != nil {
		return err
	}
	// JSON-ключи в Go-структурах сопоставляются без учёта регистра, поэтому и
	// legacy-ключи ищем так же.
	hasFold := func(m map[string]json.RawMessage, name string) bool {
		for k := range m {
			if strings.EqualFold(k, name) {
				return true
			}
		}
		return false
	}
	rawFold := func(m map[string]json.RawMessage, name string) json.RawMessage {
		for k, v := range m {
			if strings.EqualFold(k, name) {
				return v
			}
		}
		return nil
	}
	check := func(raw json.RawMessage, section string, fields ...string) error {
		if raw == nil {
			return nil
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil // структура проверится обычным Unmarshal
		}
		for _, f := range fields {
			if hasFold(m, f) {
				if section == "" {
					return fmt.Errorf("поле %q удалено (рудимент)", f)
				}
				return fmt.Errorf("поле %s.%s удалено (рудимент)", section, f)
			}
		}
		return nil
	}
	if hasFold(top, "antBms") {
		return fmt.Errorf("раздел antBms удалён (единый BMS26 через map)")
	}
	if hasFold(top, "enBms") {
		return fmt.Errorf("раздел enBms удалён (единый BMS26 через map)")
	}
	if hasFold(top, "invertors") {
		return fmt.Errorf("раздел invertors удалён (сетевые инверторы опрашиваются через API malina26)")
	}
	if err := check(rawFold(top, "map"), "map", "method", "rs485", "malina", "malina26"); err != nil {
		return err
	}
	if err := check(rawFold(top, "poll"), "poll", "ce308", "antbms", "inverter"); err != nil {
		return err
	}
	if err := check(rawFold(top, "ce308"), "ce308", "final_readings_only"); err != nil {
		return err
	}
	return nil
}

func loadConfig(path string) ([]invTarget, *dbConfig, *meterSection, *mapSection, *relaySection, *ce308Config, int, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, nil, nil, nil, nil, 0, fmt.Errorf("read config %s: %w", path, err)
	}
	var cf configFile
	if err := json.Unmarshal(b, &cf); err != nil {
		return nil, nil, nil, nil, nil, nil, 0, fmt.Errorf("parse config %s: %w", path, err)
	}
	if err := rejectLegacyConfigFields(b); err != nil {
		return nil, nil, nil, nil, nil, nil, 0, fmt.Errorf("config %s: %w", path, err)
	}
	// Сетевые инверторы больше не настраиваются: они опрашиваются через API
	// malina26 (см. grid_poller.go). targets остаётся для совместимости сигнатуры.
	var targets []invTarget

	// Раздел "map" — обязательный общий флаг disabled (отключает все пулеры
	// МАП и КЭС). Параметры раздела валидируются ниже.
	if cf.Map != nil && cf.Map.Disabled == nil {
		return nil, nil, nil, nil, nil, nil, 0, fmt.Errorf("config %s: в разделе map не задано обязательное поле disabled (false/true)", path)
	}
	if cf.Map != nil && *cf.Map.Disabled {
		log.Printf("config: map disabled=true — пулеры МАП и КЭС не запускаются")
	}
	if cf.Meter != nil && cf.Meter.Disabled == nil {
		return nil, nil, nil, nil, nil, nil, 0, fmt.Errorf("config %s: в разделе meter не задано обязательное поле disabled (false/true)", path)
	}
	// Счётчик Энергомера CE308 (раздел "ce308"). Disabled обязателен; при
	// отключённом — опрос не запускается (ce308Cfg = nil), при включённом —
	// требуется mac и pin (BLE-PIN радиоинтерфейса).
	if cf.Ce308 != nil && cf.Ce308.Disabled == nil {
		return nil, nil, nil, nil, nil, nil, 0, fmt.Errorf("config %s: в разделе ce308 не задано обязательное поле disabled (false/true)", path)
	}
	ce308Cfg, err := ce308ConfigFromSection(cf.Ce308)
	if err != nil {
		return nil, nil, nil, nil, nil, nil, 0, fmt.Errorf("config %s: %w", path, err)
	}
	// Счётчик DTS017M (раздел "dts017m") — отдельные ключи/ряд Redis и таблицы PG.
	// Disabled обязателен; при активном разделе нужны name/ip (остальное — дефолты).
	if cf.Dts017 != nil && cf.Dts017.Disabled == nil {
		return nil, nil, nil, nil, nil, nil, 0, fmt.Errorf("config %s: в разделе dts017m не задано обязательное поле disabled (false/true)", path)
	}
	if err := loadDts017Config(cf.Dts017); err != nil {
		return nil, nil, nil, nil, nil, nil, 0, fmt.Errorf("config %s: %w", path, err)
	}
	// Ручные снятия показаний счётчика энергосбыта «Меркурий» (раздел "mercury").
	if err := loadMercuryConfig(cf.Mercury); err != nil {
		return nil, nil, nil, nil, nil, nil, 0, fmt.Errorf("config %s: %w", path, err)
	}
	// Сетевое реле SR-201 (лампы) — раздел "relay". Disabled обязателен: true
	// отключает контроллер ламп; false — запускает управление по UDP. IP обязателен
	// только при активном контроллере.
	if cf.Relay != nil {
		if cf.Relay.Disabled == nil {
			return nil, nil, nil, nil, nil, nil, 0, fmt.Errorf("config %s: в разделе relay не задано обязательное поле disabled (false/true)", path)
		}
		if !*cf.Relay.Disabled && cf.Relay.IP == "" {
			return nil, nil, nil, nil, nil, nil, 0, fmt.Errorf("config %s: в разделе relay не задан ip устройства relay-sr201-2l", path)
		}
	}
	// МАП (батарея/сеть), КЭС и BMS26 — раздел "map" (HTTP-API «Малина26»).
	// Способ единственный; поле method удалено.
	if cf.Map != nil && !*cf.Map.Disabled {
		if cf.Map.Host == "" || cf.Map.Password == "" {
			return nil, nil, nil, nil, nil, nil, 0, fmt.Errorf("config %s: раздел map требует host и password", path)
		}
		if strings.TrimSpace(cf.Map.Serial) != "" {
			if _, err := strconv.ParseUint(strings.TrimSpace(cf.Map.Serial), 10, 64); err != nil {
				return nil, nil, nil, nil, nil, nil, 0, fmt.Errorf("config %s: map.serial должен быть числом строкой: %w", path, err)
			}
		}
		fs := strings.TrimSpace(cf.Map.FallbackSerial)
		if fs != "" {
			if _, err := strconv.ParseUint(fs, 10, 64); err != nil {
				return nil, nil, nil, nil, nil, nil, 0, fmt.Errorf("config %s: map.fallback_serial должен быть числом строкой: %w", path, err)
			}
			if fs == strings.TrimSpace(cf.Map.Serial) {
				return nil, nil, nil, nil, nil, nil, 0, fmt.Errorf("config %s: map.fallback_serial не должен совпадать с map.serial", path)
			}
		}
		if cf.Map.Port != 0 && (cf.Map.Port < 1 || cf.Map.Port > 65535) {
			return nil, nil, nil, nil, nil, nil, 0, fmt.Errorf("config %s: map.port должен быть в диапазоне 1..65535", path)
		}
	}

	meterActive := cf.Meter != nil && cf.Meter.Disabled != nil && !*cf.Meter.Disabled
	relayActive := cf.Relay != nil && cf.Relay.Disabled != nil && !*cf.Relay.Disabled
	mapActive := cf.Map != nil && (cf.Map.Disabled == nil || !*cf.Map.Disabled)
	if !mapActive && ce308Cfg == nil && !meterActive && !relayActive && dts017Cfg == nil {
		return nil, nil, nil, nil, nil, nil, 0, fmt.Errorf("config %s: нет ни одного активного устройства", path)
	}
	// Периоды опроса устройств (секунды) — обязательный раздел "poll".
	if err := applyPollConfig(cf.Poll); err != nil {
		return nil, nil, nil, nil, nil, nil, 0, fmt.Errorf("config %s: %w", path, err)
	}
	// Порт веб-дашборда — обязательное поле dashboard_port в диапазоне 1..65535.
	if cf.DashboardPort < 1 || cf.DashboardPort > 65535 {
		return nil, nil, nil, nil, nil, nil, 0, fmt.Errorf("config %s: dashboard_port=%d вне диапазона 1..65535 (порт веб-дашборда)", path, cf.DashboardPort)
	}
	// series_target_points / series_redis_target_points — ОБЯЗАТЕЛЬНЫЕ.
	if cf.SeriesTargetPoints == nil {
		return nil, nil, nil, nil, nil, nil, 0, fmt.Errorf("config %s: не задано обязательное поле series_target_points (цель точек/серию на бэке)", path)
	}
	if cf.SeriesRedisTargetPoints == nil {
		return nil, nil, nil, nil, nil, nil, 0, fmt.Errorf("config %s: не задано обязательное поле series_redis_target_points (цель агрегации RedisTimeSeries)", path)
	}
	maxSeriesPoints = *cf.SeriesTargetPoints
	seriesRedisTargetPoints = *cf.SeriesRedisTargetPoints
	if seriesRedisTargetPoints > maxSeriesPoints && maxSeriesPoints > 0 {
		return nil, nil, nil, nil, nil, nil, 0, fmt.Errorf("config %s: series_redis_target_points=%d > series_target_points=%d (Redis-цель должна быть не больше бэкенд-цели)", path, seriesRedisTargetPoints, maxSeriesPoints)
	}
	// Учётные данные HTTP Basic для `/api/*` дашборда (необязательны). Заданы обе —
	// требовать авторизацию; иначе API открыт (доступ снаружи закрывает прокси).
	dashboardAuthUser = cf.DashboardUser
	dashboardAuthPass = cf.DashboardPassword
	// Уведомления в MAX (раздел notify): токен обязателен. Адресат (user_id или
	// chat_id) НЕ обязателен — если он пуст, бот регистрирует первого подписчика
	// автоматически (bot_started/bot_added) и дописывает его в конфиг.
	notifyCfg = cf.Notify
	if notifyCfg != nil {
		if notifyCfg.Disabled != nil && *notifyCfg.Disabled {
			log.Printf("config: notify disabled=true — уведомления в MAX выключены (раздел в конфиге, но без оповещений)")
			notifyCfg = nil
		} else if notifyCfg.Token == "" {
			return nil, nil, nil, nil, nil, nil, 0, fmt.Errorf("config %s: в разделе notify не задан token", path)
		}
	}
	return targets, cf.DB, cf.Meter, cf.Map, cf.Relay, ce308Cfg, cf.DashboardPort, nil
}

// dashboardAuthUser/dashboardAuthPass — учётные данные HTTP Basic для `/api/*`
// дашборда (из конфига dashboard_user/dashboard_password). Пустые значения —
// аутентификация не требуется. Заполняются в loadConfig.
var dashboardAuthUser, dashboardAuthPass string

// defaultDashboardAddr собирает адрес веб-дашборда из обязательного конфиг-порта.
func defaultDashboardAddr(port int) string {
	return fmt.Sprintf(":%d", port)
}

// defaultPGRestoreWindow возвращает окно реставрации Redis из PG: из раздела db
// конфига (duration-строка), иначе — дефолт 30 суток. Некорректная строка — дефолт.
func defaultPGRestoreWindow(db *dbConfig) time.Duration {
	if db != nil && db.PGRestoreWindow != "" {
		if d, err := time.ParseDuration(db.PGRestoreWindow); err == nil && d > 0 {
			return d
		}
	}
	return 30 * 24 * time.Hour
}

// defaultRedisAddr возвращает адрес Redis: из раздела db конфига (приоритет),
// иначе — дефолт 127.0.0.1:6379.
func defaultRedisAddr(db *dbConfig) string {
	if db != nil && db.Redis != "" {
		return db.Redis
	}
	return "127.0.0.1:6379"
}

// defaultPGDSN возвращает DSN PostgreSQL: из раздела db конфига (приоритет),
// иначе — дефолт localhost.
func defaultPGDSN(db *dbConfig) string {
	if db != nil && db.PG != "" {
		return db.PG
	}
	return "postgres://localhost:5432/sunreceiver?sslmode=disable"
}

// mpptPollState — межцикловое состояние пулера MPPT-контроллеров через API
// «Малина26» (сохраняется между вызовами pollAndSaveMap; доступ из одной
// горутины). Счётчик подряд идущих ПУСТЫХ успешных ответов: до порога
// mpptEmptyTolerance не вызываем PruneMPPT, чтобы одиночная пустота не снесла
// все MPPT-ключи из current.
type mpptPollState struct {
	empty int // подряд идущих пустых успешных ответов MPPT API
}

func newMPPTPollState() *mpptPollState { return &mpptPollState{} }

// shouldPrune решает, вызывать ли PruneMPPT в текущем цикле. При пустом ответе
// (empty=true) инкрементит счётчик и разрешает прунинг только после порога
// mpptEmptyTolerance; при непустом (empty=false) — сбрасывает счётчик и разрешает
// прунинг сразу.
func (s *mpptPollState) shouldPrune(empty bool) bool {
	if empty {
		s.empty++
		return s.empty >= mpptEmptyTolerance
	}
	s.empty = 0
	return true
}

// mapDevKey — постоянный ключ устройства МАП в хранилище (логический ключ
// обособленных ключей sunreceiver:map:current/:series:*; поле в PG map_averages_r —
// name). Не зависит от адреса МАП/API и его IP: МАП
// всегда одна колонка/ряд с ключом "map".
const mapDevKey = "map"

// devKey возвращает ключ устройства в хранилище (поле IP снимка): для обычных
// инверторов это IP; для МАП (kindMAP) — константа "map" (адрес не важен);
// для kindMPPT — "mppt-<UID>" (UID = серийник контроллера), без host, чтобы один и
// тот же контроллер не «переезжал» при смене адреса/хостинга ПАК. Ключ строится по
// UID (стабильный серийник), а не по индексу в массиве API: отвал контроллера с
// меньшим индексом сдвигает остальных в ответе, и индексный ключ склеил бы ряды
// двух разных аппаратов. Префикс "mppt-" сохраняется (isMPPTKey/PruneMPPT/
// PruneMPPT ищет ключи по strings.HasPrefix(ip, "mppt-")).
func devKey(t invTarget) string {
	if t.Kind == kindMPPT {
		// Ключ MPPT — только по серийнику (UID). Без UID устройства нет (пулер
		// такие пропускает); пустая строка заставит вызывающих не сохранять.
		if t.UID == "" {
			return ""
		}
		return fmt.Sprintf("mppt-%s", t.UID)
	}
	if t.Kind == kindMAP {
		return mapDevKey
	}
	return t.IP
}

// maxSeriesPoints — цель числа точек на серию на СТОРОНЕ БЭКА (downsampleSeries/
// downsampleBMSSeries). Конфиг series_target_points (обязательное).
// seriesRedisTargetPoints — цель для серверной агрегации RedisTimeSeries
// (должна быть НЕ больше backend-цели, обычно чуть меньше: тогда RT отдаёт не больше
// точек, чем ждёт backend, и бэк не перепрореживает ряд из Redis). Конфиг
// series_redis_target_points (обязательное). 0 — соответствующая агрегация off.
var (
	maxSeriesPoints         = 1000
	seriesRedisTargetPoints = 1000
)

// version — версия сборки сервиса. Переопределяется при сборке через
// -ldflags "-X main.version=<версия>"; по умолчанию "dev" (локальная сборка).
var version = "dev"

// mapSourceIdentity возвращает devKey (IP) и логическое имя МАП, по которому
// ведётся мониторинг уведомлений. Источник МАП — API «Малина26».
// При отсутствии источника — ("", ""): мониторинг не запускается.
func mapSourceIdentity() (string, string) {
	if malina26 != nil && malina26.serial != "" {
		return mapDevKey, malina26.name
	}
	return "", ""
}

// mapSourceName возвращает строковое имя активного источника МАП (для логов).
func mapSourceName() string {
	if malina26 != nil {
		return "API «Малина26»"
	}
	return "нет источника"
}

// valuesContract — универсальный контракт значений. В файл выводит общие для обеих
// марок теги (commonContractTags) плюс брендовые температуры (temperatureTags) в
// фиксированном порядке; числовые значения тегов *voltage / *current / temperature* /
// energy* / *power округляются до 1 знака после запятой.
type valuesContract map[string]any

// commonContractTags — теги, которые обе марки (Deye и Sofar) отдают в одинаковых
// единицах измерения. Только эти теги попадают в values; порядок в файле фиксирован.
var commonContractTags = []string{
	// PV входы
	"pv1_voltage", "pv1_current", "pv1_power",
	"pv2_voltage", "pv2_current", "pv2_power",
	// AC выход
	"ac_active_power",
	"ac_reactive_power",
	"grid_frequency",
	// Фазы L1/L2/L3
	"l1_voltage", "l1_current",
	"l2_voltage", "l2_current",
	"l3_voltage", "l3_current",
	// Энергия
	"energy_today",
	"energy_total",
	// МАП: батарея и сеть (для дашборда КЭС)
	"grid_voltage",
	"grid_power",
	"battery_voltage",
	"battery_current",
	"battery_power",
	// Электросчётчик DDS238: мгновенные значения (V, A, W, var, Hz, kWh)
	"meter_voltage",
	"meter_current",
	"meter_active_power",
	"meter_reactive_power",
	"meter_power_factor",
	"meter_frequency",
	"meter_import",
	"meter_export",
	"meter_total",
}

// temperatureTags — дополнительные температурные теги, которые выводятся в
// values/current ПОМИМО общих (commonContractTags). Они бренд-специфичны (разные
// имена и масштабы у Sofar/Deye/МАП), поэтому в commonContractTags не входят, но их
// полезно отдавать на дашборд. Пустые/отсутствующие пропускаются при сериализации.
var temperatureTags = []string{
	// МАП Титанатор (агрегат батарея/сеть): внешний датчик АКБ, тора, транзисторов.
	"map_temp_battery",
	"map_temp_tor",
	"map_temp_transistor",
	// Sofar K-TLX.
	"temperature_module",
	"temperature_inner",
	// Deye string.
	"temperature_radiator",
	"temperature_igbt",
}

// contractTags — полный упорядоченный список тегов, сериализуемых в values/current:
// общие теги, затем температуры.
var contractTags = append(append([]string{}, commonContractTags...), temperatureTags...)

// needsRounding — true, если тэг относится к величинам, которые округляются до
// 1 знака после запятой (напряжение, ток, мощность, энергия, температура).
// Коэффициент мощности (meter_power_factor) НЕ округляется — входит в контракт
// с точностью с датчика.
func needsRounding(tag string) bool {
	return strings.HasSuffix(tag, "voltage") ||
		strings.HasSuffix(tag, "current") ||
		strings.HasSuffix(tag, "power") ||
		strings.Contains(tag, "energy") ||
		// Энергия счётчика DDS238 (kWh) — тоже энергия, хотя «energy» в имени нет.
		tag == "meter_import" || tag == "meter_export" || tag == "meter_total" ||
		strings.Contains(tag, "temperature")
}

// needsRound2 возвращает true для частот (grid_frequency/meter_frequency) — они
// входят в контракт с точностью датчика (2 знака, как даёт логгер/счётчик). Значение
// строится умножением на ratio (напр. ×0.01), что в double даёт бинарный артефакт
// вида 50.010000000000002 — округляем до 2 знаков, сохраняя заявленную точность.
func needsRound2(tag string) bool {
	return strings.HasSuffix(tag, "frequency")
}

// round1 округляет числовое значение (для тегов, для которых needsRounding) до
// 1 знака после запятой; частотные теги (needsRound2) — до 2 знаков. Числа
// возвращает как float, прочее — как есть.
func round1(tag string, v any) any {
	mult := 10.0
	if needsRound2(tag) {
		mult = 100
	} else if !needsRounding(tag) {
		return v
	}
	switch n := v.(type) {
	case int:
		return math.Round(float64(n)*mult) / mult
	case float64:
		return math.Round(n*mult) / mult
	}
	return v
}

// MarshalJSON выводит только общие теги (commonContractTags) в фиксированном
// порядке; отсутствующие пропускаются. Значения числовых тегов округляются
// до 1 знака после запятой (round1).
func (v valuesContract) MarshalJSON() ([]byte, error) {
	if len(v) == 0 {
		return []byte("{}"), nil
	}
	var buf bytes.Buffer
	buf.WriteByte('{')
	first := true
	for _, key := range contractTags {
		raw, ok := v[key]
		if !ok {
			continue
		}
		k, err := json.Marshal(key)
		if err != nil {
			return nil, err
		}
		val, err := json.Marshal(round1(key, raw))
		if err != nil {
			return nil, err
		}
		if !first {
			buf.WriteByte(',')
		}
		first = false
		buf.Write(k)
		buf.WriteByte(':')
		buf.Write(val)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// DeviceResult — результат опроса одного инвертора. Поля для снимка
// (см. deviceSnapshot) отдельные.
type DeviceResult struct {
	OK         bool
	HasData    bool
	DeviceSN   string // серийный номер даталоггера (десятичный)
	InverterSN string // серийный номер инвертора (строка; для Sofar это ASCII-строка HW-регистров)
	ErrCode    byte   // код ошибки heartbeat Deye/Sofar (0x00 — нет ошибки), см. DeyeErrorCode
	Values     valuesContract
	Faults     []string  // аварии самого устройства (из профильных регистров; НЕ в Values)
	Time       time.Time // время актуальности данных (для MPPT API — timestamp ответа)
}

// deviceSnapshot — структура снимка, сохраняемого в Redis (HASH current и
// временной ряд series). JSON-представление — контракт для дашборда и PG.
type deviceSnapshot struct {
	Name       string `json:"name"`
	IP         string `json:"ip,omitempty"`
	Timestamp  string `json:"timestamp"`
	DeviceSN   string `json:"device_sn,omitempty"`
	InverterSN string `json:"inverter_sn,omitempty"`
	Order      int    `json:"order,omitempty"`
	// Placement — размещение инвертора (группа «Мощности инверторов»); у МАП,
	// MPPT-контроллеров (КЭС) и счётчика не заполняется.
	Placement string `json:"placement,omitempty"`
	// Kind — марка/тип устройства: "deye"|"sofar" для
	// сетевых инверторов, "kes" для MPPT-контроллеров (КЭС), "" — МАП/счётчик.
	// Заполняется пулером; используется страницей анимации для выбора спрайта.
	Kind   string         `json:"kind,omitempty"`
	Values valuesContract `json:"values"`
}

// Универсальный контракт значений (values) — в файл попадают ТОЛЬКО теги, которые
// обе марки (Deye и Sofar) отдают в одинаковых единицах: 15 общих тегов
// (commonContractTags). Единица зашита в суффикс имени: *_voltage — V, *_current — A,
// ac_*_power — W (ac_reactive_power — var), grid_frequency — Hz, energy_* — kWh.
// Числовые значения *voltage/*current/*power/energy*/temperature* округляются до
// 1 знака (round1). Бренд-специфичные диагностические поля (inverter_status, fault_*,
// bus_voltage, country и пр.) в values НЕ попадают — они не входят ни в
// commonContractTags, ни в temperatureTags и отбрасываются при сериализации снимка.
// Температуры (МАП: map_temp_*, Sofar: temperature_module/inner, Deye:
// temperature_radiator/igbt) выводятся через temperatureTags.

func runMapPoll(store *redisStore, pg *pgStore, stop context.Context) {
	ticker := time.NewTicker(mapPollInterval)
	defer ticker.Stop()
	state := newMPPTPollState()
	errState := newMapErrState() // app-ошибки по устройствам МАП/MPPT (для истории)
	for {
		select {
		case <-ticker.C:
			pollAndSaveMap(stop, store, pg, time.Now(), state, errState)
		case <-stop.Done():
			return
		}
	}
}

// mapErrState — потокобезопасное состояние «активна ли app-ошибка» по устройствам
// МАП/MPPT. saveWindowSnapshot вызывается параллельно из нескольких горутин
// (МАП/МАП-API/MPPT), поэтому доступ к карте сериализуется мьютексом: без него
// одновременная запись в map приводит к «fatal error: concurrent map writes».
type mapErrState struct {
	mu sync.Mutex
	m  map[string]bool
}

func newMapErrState() *mapErrState { return &mapErrState{m: map[string]bool{}} }

// markIfAbsent помечает устройство как «с ошибкой» и возвращает true только при
// переходе «без ошибки → с ошибкой» (для истории ошибок пишется лишь появление).
func (e *mapErrState) markIfAbsent(key string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.m[key] {
		return false
	}
	e.m[key] = true
	return true
}

// clear снимает отметку ошибки устройства (успешный опрос).
func (e *mapErrState) clear(key string) {
	e.mu.Lock()
	e.m[key] = false
	e.mu.Unlock()
}

// pollAndSaveMap опрашивает быстрые источники параллельно и пишет в Redis
// КАЖДОЕ снятое показание (Score = секунда снятия; + актуальное current) через
// saveMapSnapshot:
//   - МАП (kindMAP) — через API «Малина26» (malina26);
//   - MPPT-контроллеры (КЭС) — ДИНАМИЧЕСКИ: состав определяется фактически
//     подключёнными к ПАК «Малина» контроллерами из ответа API «Малина26», а не из
//     sunReceiver.json. Поэтому MPPT появляются/исчезают с дашборда по факту наличия
//     в ответе API; исчезнувшие удаляются и из HASH current (их строка не числится
//     «актуальной».
//
// saveWindowSnapshot складывает результат опроса в deviceSnapshot (ts = res.Time
// при наличии, иначе now) и пишет в Redis через saveMapSnapshot. Общий для
// МАП- и MPPT-веток pollAndSaveMap.
func saveWindowSnapshot(store *redisStore, pg *pgStore, t invTarget, res DeviceResult, now time.Time, errState *mapErrState) {
	key := devKey(t)
	if !res.OK || !res.HasData {
		log.Printf("%s: %s %s", t.IP, t.Kind, describeResult(res))
		if pg != nil && errState != nil && errState.markIfAbsent(key) {
			kind := "map"
			if t.Kind == kindMPPT {
				kind = "mppt"
			}
			code, desc := "no_data", "нет данных"
			if res.ErrCode != 0 {
				code = fmt.Sprintf("0x%02X", res.ErrCode)
				desc = deyeErrCodeName(res.ErrCode)
			}
			if e := pg.InsertDeviceError(key, kind, code, desc, now); e != nil {
				log.Printf("error pg %s: %v", key, e)
			}
		}
		return
	}
	if errState != nil {
		errState.clear(key)
	}
	ts := now
	if !res.Time.IsZero() {
		ts = res.Time
	}
	snap := deviceSnapshot{
		Name:       t.Name,
		IP:         devKey(t),
		Timestamp:  ts.Format(time.RFC3339),
		DeviceSN:   res.DeviceSN,
		InverterSN: res.InverterSN,
		Order:      t.Order,
		Placement:  t.Placement,
		Kind:       invKindName(t.Kind),
		Values:     res.Values,
	}
	if snap.IP == mapDevKey {
		if err := store.saveMapSnapshot(snap, ts); err != nil {
			log.Printf("redis save map: %v", err)
		}
		return
	}
	if isKesKey(snap.IP) {
		if err := store.saveKesSnapshot(snap, ts); err != nil {
			log.Printf("redis save kes %s: %v", snap.IP, err)
		}
		return
	}
	if err := store.SaveSnapshot(snap, ts); err != nil {
		log.Printf("redis save %s: %v", devKey(t), err)
	}
}

func pollAndSaveMap(ctx context.Context, store *redisStore, pg *pgStore, now time.Time, state *mpptPollState, errState *mapErrState) {
	var wg sync.WaitGroup
	activeMPPT := map[string]struct{}{}
	pruneMPPT := false // MPPT-состав чистим из current только по решению state.shouldPrune
	// МАП через API «Малина26»: цель kindMAP с ключом "map". Без serial
	// «своего» МАП этот пулер не запускается (сетевые инверторы при этом работают).
	if malina26 != nil && malina26.serial != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			t := invTarget{IP: malina26.host, Name: malina26.name, Kind: kindMAP, Slot: -1, Order: malina26MapOrder}
			saveWindowSnapshot(store, pg, t, pollMAPMalina26(ctx), now, errState)
		}()
	}
	// КЭС через API «Малина26»: все контроллеры, один пулер,
	// последовательно. Ключ устройства — mppt-<serial>.
	if malina26 != nil {
		readings, err := malina26.FetchKES(ctx)
		if err != nil {
			log.Printf("kes malina26: %v", err)
		} else {
			for _, k := range readings {
				// Отвергаем контроллеры без серийника (UID) — иначе ключ/имя пустые.
				if k.UID == "" {
					log.Printf("kes malina26: контроллер без serial (slot %d) — пропуск", k.Slot)
					continue
				}
				name := "MPPT-" + k.UID
				t := invTarget{IP: malina26.host, Name: name, Kind: kindMPPT, Slot: k.Slot, UID: k.UID, Order: mpptOrderBase + k.Slot}
				activeMPPT[devKey(t)] = struct{}{}
				wg.Add(1)
				go func(t invTarget, res DeviceResult) {
					defer wg.Done()
					saveWindowSnapshot(store, pg, t, res, now, errState)
				}(t, k.Result)
			}
			pruneMPPT = state.shouldPrune(len(readings) == 0)
		}
	}
	wg.Wait()
	// Исчезнувшие MPPT-контроллеры (не в ответе API) убираем из HASH current, чтобы
	// их строка не показывалась на дашборде как актуальная.
	if pruneMPPT {
		store.PruneMPPT(activeMPPT)
	}
}

func main() {
	log.SetFlags(log.Ltime)
	setupLogging()

	for _, a := range os.Args[1:] {
		if a == "--version" || a == "-version" {
			fmt.Printf("sunReceiver %s\n", version)
			return
		}
	}

	cfgPath := configPath()
	if _, err := os.Stat(cfgPath); os.IsNotExist(err) {
		// `go run .`: бинарник во временном каталоге go-сборки — ищем sunReceiver.json в CWD.
		cfgPath = "sunReceiver.json"
	}
	var dbCfg *dbConfig
	var meterSec *meterSection
	var mapSec *mapSection
	var relaySec *relaySection
	var ce308Cfg *ce308Config
	var dashPort int
	var err error
	_, dbCfg, meterSec, mapSec, relaySec, ce308Cfg, dashPort, err = loadConfig(cfgPath)
	if err != nil {
		log.Fatal(err)
	}

	// Адреса БД и порт дашборда берутся из sunReceiver.json (раздел db / dashboard_port).
	redisAddr := defaultRedisAddr(dbCfg)
	pgDSN := defaultPGDSN(dbCfg)
	restoreWindow := defaultPGRestoreWindow(dbCfg)
	dashboardAddr := defaultDashboardAddr(dashPort)

	log.Printf("poller started: version=%s config=%s", version, cfgPath)

	// HTTP-API «Малина26» — МАП, КЭС и BMS26 (токен + бинарные контракты).
	malina26 = loadMalina26(mapSec)
	if malina26 != nil {
		log.Printf("map malina26: опрос через %s (serial %s)", malina26.baseURL, malina26.serial)
	}
	// BMS через malina26 (единый тип ANT+EnBMS): используем общий клиент
	// (те же host/port/password и токен), отдельного раздела нет.
	bms26 = newBms26Source(malina26)
	// Конфигурация электросчётчика DDS238 — раздел "meter" sunReceiver.json
	// или файл dds238.json (обратная совместимость).
	meterCfg := loadMeterConfig(meterSec)
	if desc := describeMeterConfig(meterCfg); desc != "" {
		log.Printf("meter: %s", desc)
	} else {
		log.Printf("meter: не настроен (нет dds238.json рядом с бинарником) — опрос счётчика отключён")
	}
	// Счётчик Энергомера CE308 — раздел "ce308" sunReceiver.json (опрос по BLE).
	if desc := describeCE308Config(ce308Cfg); desc != "" {
		log.Printf("ce308: %s", desc)
	} else {
		log.Printf("ce308: не настроен (нет раздела ce308) — опрос CE308 отключён")
	}
	// Счётчик DTS017M — раздел "dts017m" (Modbus RTU/TCP, обособленное хранилище).
	if desc := describeDts017Config(dts017Cfg); desc != "" {
		log.Printf("dts017m: %s", desc)
	} else {
		log.Printf("dts017m: не настроен (нет активного раздела dts017m) — опрос отключён")
	}

	// Флаги видимости блоков дашборда, вычисленные из конфигурации:
	//   - ShowMap — МАП/MPPT включены (map.disabled != true);
	//   - ShowMeter — счётчик реально опрашивается (не disabled и не «неполный»);
	//   - ShowBMS — пулер BMS26 активен (единый ANT+EnBMS через malina26).
	//   - ShowRelay — контроллер ламп SR-201 включен (relay.disabled != true).
	dash := dashFlags{
		ShowMap:    mapSec != nil && (mapSec.Disabled == nil || !*mapSec.Disabled),
		ShowMeter:  meterCfg != nil,
		ShowBMS:    bms26 != nil,
		ShowRelay:  relaySec != nil && (relaySec.Disabled == nil || !*relaySec.Disabled),
		ShowCE308:  ce308Cfg != nil,
		ShowDTS017: dts017Cfg != nil,
	}

	// Сетевое реле SR-201 (лампы): управление по UDP, состояние поддерживает
	// фоновый цикл runRelayControl. Контроллер создаётся сразу (новый экземпляр
	// при старте переведёт реле в желаемые состояния из конфига/Redis), а ДОСТУП
	// из других модулей — через SetRelayLamp / relayCtl.
	relaySec = buildRelayCfg(relaySec)
	if relaySec != nil && relaySec.Disabled != nil && *relaySec.Disabled {
		log.Printf("relay: контроллер ламп SR-201 выключен (relay.disabled=true)")
		relaySec = nil
	}

	rdb, err := openRedis(redisAddr)
	if err != nil {
		log.Fatalf("redis: %v", err)
	}
	defer rdb.Close()
	store := &redisStore{rdb: rdb, ctx: context.Background()}

	relayCtl = newRelayController(relaySec, store)
	if relayCtl != nil {
		log.Printf("relay: управление лампами SR-201 (%s, UDP %d, blink_hz=%.1f)", relaySec.IP, relaySec.UDPPort, relaySec.BlinkHz)
	}

	// Persistent-хранилище PostgreSQL: только усреднённые 5-минутные точки,
	// пишутся фоновым процессом аккумуляции (см. accumulator.go) после накопления
	// данных за 5 минут. Реставрация Redis при пустом хранилище.
	// stopCtx — единый сигнал остановки ВСЕХ фоновых горутин: отмена ctx как
	// закрывает их циклы (select на ctx.Done()), так и прерывает ЗАВЕРШЕННЫЕ
	// (in-flight) сетевые опросы (dial/read/HTTP), чтобы graceful-shutdown не
	// ждал медленный логгер (до ~15 c на первый байт).
	stopCtx, stopCancel := context.WithCancel(context.Background())
	defer stopCancel()
	// storeCtx — контекст операций Redis/PG. Отдельный от stopCtx: циклы пулеров
	// останавливаются по stopCtx, но финальные записи при shutdown (averageBucket,
	// averageBucket) должны выполняться с ЖИВЫМ контекстом — иначе go-redis
	// вернёт "context canceled", и неполный бакет при остановке теряется (N16).
	// storeCtx отменяется после bgWg.Wait() и до закрытия пулов.
	storeCtx, storeCancel := context.WithCancel(context.Background())
	defer storeCancel()
	store.SetCtx(storeCtx)
	// bgWg — все фоновые горутины, пишущие в Redis/PG: при завершении main
	// отменяет stopCtx, ЖДЁТ их (bgWg.Wait()) и только потом defer'ы закрывают
	// пулы rdb/pg — записи при остановке (averageBucket) не гоняются
	// с закрытыми пулами.
	var bgWg sync.WaitGroup
	var pg *pgStore
	if pgDSN != "" {
		pg, err = openPG(stopCtx, pgDSN)
		if err != nil {
			log.Printf("pg: %v (persistent-хранилище отключено)", err)
		} else {
			defer pg.Close()
			log.Printf("pg: persistent-хранилище подключено (5-минутные усреднённые точки)")
			// Режим миграции средних (этап 5) — запускается с dev-ПК, выходит без
			// старта пулеров: `--migrate-avg`.
			if argPresent("--migrate-avg") {
				if merr := runMigrateAvg(pg); merr != nil {
					log.Fatalf("migrate-avg: %v", merr)
				}
				log.Printf("migrate-avg: завершено")
				return
			}
			// Если Redis пуст — восстановить в нём данные из PG. Реставрация
			// выполняется СИНХРОННО, ДО запуска runAccumulator ниже: иначе
			// backfillAccumulator мог бы прочитать наполовину восстановленный
			// Redis и перетереть полные PG-бакеты частичными (гонка restore ↔
			// backfill на пустом Redis).
			empty, cerr := store.IsEmpty()
			incomplete, ierr := store.rdb.Exists(store.ctx, redisRestoreIncompleteKey).Result()
			switch {
			case cerr != nil:
				log.Printf("redis empty-check: %v", cerr)
			case ierr != nil:
				log.Printf("redis restore-check: %v", ierr)
			case empty || incomplete > 0:
				if incomplete > 0 {
					log.Printf("pg restore: прошлая реставрация неполная — повторяю")
				}
				// IsEmpty (redis_store.go) учитывает инверторные current/ряды +
				// обособленные DDS238/МАП/КЭС. Данные ANT/EnBMS BMS лежат в
				// ОТДЕЛЬНЫХ ключах (HASH bms26:current, ряд bms26:series:*), в
				// IsEmpty не входят. Проверяем их, чтобы сложившийся BMS-«магазин»
				// не был засчитан пустым и не перетёрся реставрацией.
				bmsPresent, berr := redisBMSDataPresent(store)
				dtsPresent, derr := redisDts017DataPresent(store)
				switch {
				case berr != nil:
					log.Printf("redis empty-check (bms): %v", berr)
				case derr != nil:
					log.Printf("redis empty-check (dts017m): %v", derr)
				case incomplete == 0 && empty && (bmsPresent || dtsPresent):
					log.Printf("redis empty-check: в Redis есть данные BMS/DTS017M — реставрация не требуется")
				default:
					// SETNX-маркер: защита от повторной/одновременной реставрации
					// (два экземпляра на одном Redis). Захвативший маркер — единственный,
					// кто восстанавливает; остальные пропускают. Снимается сразу после
					// реставрации (см. TTL на случай падения).
					locked, lerr := store.rdb.SetNX(store.ctx, redisRestoreLockKey, time.Now().Unix(), restoreLockTTL).Result()
					if lerr != nil {
						log.Printf("pg restore: не удалось взять маркер: %v", lerr)
					} else if !locked {
						log.Printf("pg restore: реставрацию уже выполняет другой процесс — пропускаю")
					} else {
						complete := restoreRedisFromPG(store, pg, restoreWindow, stopCtx)
						store.rdb.Del(store.ctx, redisRestoreLockKey)
						// Маркер неполной реставрации: при сбое ряда она повторится
						// при следующем старте (см. redisRestoreIncompleteKey).
						if complete {
							store.rdb.Del(store.ctx, redisRestoreIncompleteKey)
						} else if serr := store.rdb.Set(store.ctx, redisRestoreIncompleteKey, "1", 0).Err(); serr != nil {
							log.Printf("pg restore: запись маркера неполной реставрации: %v", serr)
						}
					}
				}
			}
		}
	} else {
		log.Printf("pg: отключено (флаг -pg пустой); работаем только через Redis")
	}

	// Фоновые процессы: усреднение данных за 5 минут в PG и очистка старых
	// данных Redis (старше 2 календарных суток).
	bgWg.Add(1)
	go func() {
		defer bgWg.Done()
		runAccumulator(store, pg, stopCtx)
	}()
	bgWg.Add(1)
	go func() {
		defer bgWg.Done()
		runRedisCleanup(store, stopCtx)
	}()
	bgWg.Add(1)
	go func() {
		defer bgWg.Done()
		runAvgPartitionMaintenance(pg, stopCtx)
	}()
	// МАП и КЭС опрашиваются отдельно, 1 раз в секунду, и пишутся в Redis каждым
	// снятым показанием (см. saveMapSnapshot). Быстрый 1-сек цикл запускается
	// только если есть источник (malina26); при map.disabled=true — не стартует.
	hasMapSource := malina26 != nil
	if hasMapSource {
		bgWg.Add(1)
		go func() {
			defer bgWg.Done()
			runMapPoll(store, pg, stopCtx)
		}()
		// Сетевые инверторы через тот же API: list /api/grid/status + снимок
		// /api/grid/device?serial=, период 5 с (см. grid_poller.go).
		bgWg.Add(1)
		go func() {
			defer bgWg.Done()
			runGridPoll(store, pg, malina26, stopCtx)
		}()
		// Отдельный пулер ОШИБОК МАП (не чаще 1/30 с): сырые ячейки через
		// malina26.mapErrCells, запись появления в PG device_errors.
		if pg != nil {
			bgWg.Add(1)
			go func() {
				defer bgWg.Done()
				runMapErrorPoll(pg, stopCtx)
			}()
		}
	} else {
		log.Printf("map: опрос отключён (map.disabled=true или нет источника malina26)")
	}
	// Уведомления в MAX. Мониторинг МАП выполняется ТОЛЬКО если включён опрос МАП
	// (есть источник malina26). Если опрос МАП выключен — уведомления не
	// формируются, даже при настроенном notify.
	mapIP, mapName := mapSourceIdentity()
	if notifyCfg != nil && mapIP != "" {
		bgWg.Add(1)
		go func() {
			defer bgWg.Done()
			runNotifyMonitor(store, notifyCfg, mapIP, mapName, meterCurrentKey(meterCfg), stopCtx)
		}()
		log.Printf("notify: уведомления в MAX включены (МАП %s, источник %s)", mapName, mapSourceName())
	} else if notifyCfg != nil {
		log.Printf("notify: опрос МАП выключен или нет источника МАП — уведомления в MAX не формируются")
	}
	// BMS через malina26 (единый тип ANT+EnBMS): отдельный цикл, Redis
	// sunreceiver:bms26:* + PG bms26_averages_r, см. bms26.go.
	if bms26 != nil {
		bgWg.Add(1)
		go func() {
			defer bgWg.Done()
			runBms26Poll(store, pg, stopCtx)
		}()
	}
	// Электросчётчик DDS238 — 1 раз в секунду (мгновенные значения в Redis +
	// посуточные тарифные захваты в PG, см. runMeterPoll и meter_tariff.go).
	if meterCfg != nil {
		bgWg.Add(1)
		go func() {
			defer bgWg.Done()
			runMeterPoll(store, pg, meterCfg, stopCtx)
		}()
		// Добор пропущенных тарифных границ («ближайшее из зафиксированного»),
		// см. meter_backfill.go.
		bgWg.Add(1)
		go func() {
			defer bgWg.Done()
			runMeterBackfill(store, pg, meterCfg, stopCtx)
		}()
	}

	// Счётчик DTS017M — отдельный цикл опроса (Modbus RTU через прозрачный шлюз)
	// с периодом dts017m.poll_interval. Данные — ТОЛЬКО в собственные ключи/ряд
	// Redis и собственные таблицы PG (dts017m_averages_r, dts017m_daily_tariffs);
	// история счётчика не читается, посуточные тарифы считаются сами (см.
	// dts017_poller.go, dts017_accumulator.go, dts017_tariff.go, dts017_backfill.go).
	if dts017Cfg != nil {
		bgWg.Add(1)
		go func() {
			defer bgWg.Done()
			runDts017Poll(store, pg, dts017Cfg, stopCtx)
		}()
		bgWg.Add(1)
		go func() {
			defer bgWg.Done()
			runDts017Backfill(store, pg, dts017Cfg, stopCtx)
		}()
		bgWg.Add(1)
		go func() {
			defer bgWg.Done()
			// Стабильный ключ (dts017DeviceKey), а не cfg.Name: переименование
			// счётчика в конфиге не должно расщеплять историю PG на два потока.
			runDts017Accumulator(store, pg, dts017DeviceKey, stopCtx)
		}()
	}

	// Счётчик Энергомера CE308 — только итоговые показания: без постоянного
	// соединения, телеметрии и PG-агрегации; раз в 30 мин (или по кнопке) —
	// разовый снимок накопленной энергии. Раздел независим от DDS238 (meter).
	if ce308Cfg != nil {
		bgWg.Add(1)
		go func() {
			defer bgWg.Done()
			runCe308FinalReadings(store, pg, ce308Cfg, stopCtx)
		}()
	}

	if relayCtl != nil {
		bgWg.Add(1)
		go func() {
			defer bgWg.Done()
			runRelayControl(relayCtl, stopCtx)
		}()
		// Красная лампа — индикатор отдачи в сеть: управляется по алгоритму на
		// основе текущего состояния счётчика (см. runRelayLampController).
		bgWg.Add(1)
		go func() {
			defer bgWg.Done()
			runRelayLampController(store, meterCfg, relayCtl, stopCtx)
		}()
		// Белая лампа — индикатор наличия напряжения сети: на основе данных МАП
		// (grid_voltage) и счётчика (meter_voltage), см. runWhiteLampController.
		mapIP, _ := mapSourceIdentity()
		bgWg.Add(1)
		go func() {
			defer bgWg.Done()
			runWhiteLampController(store, mapIP, meterCfg, relayCtl, stopCtx)
		}()
	}

	bgWg.Add(1)
	go func() {
		defer bgWg.Done()
		serveDashboard(dashboardAddr, store, pg, relayCtl, stopCtx, dash, dashboardAuthUser, dashboardAuthPass)
	}()

	// Windows-сборка сворачивается в трей (меню «Закрыть»); на POSIX (Linux)
	// останавливается по SIGINT/SIGTERM. Единая точка — канал quit.
	quit := make(chan struct{}, 1)
	runTray(quit)

	<-waitForQuit(quit)
	log.Println("shutting down")
	// Отмена stopCtx: циклы фоновых горутин завершаются, a in-flight сетевые
	// опросы (dial/read/HTTP) прерываются немедленно — graceful-shutdown не
	// ждёт медленный логгер.
	stopCancel()
	// Ждём завершения фоновых горутин (их завершающие записи в Redis/PG:
	// averageBucket), ПОСЛЕ чего defer-ы закрывают пулы — гонки
	// «запись в закрытый пул» нет. storeCtx отменяем только теперь, чтобы
	// завершающие записи не упали с "context canceled".
	bgWg.Wait()
	storeCancel()
}

func describeResult(res DeviceResult) string {
	if !res.OK {
		return "error"
	}
	if res.HasData {
		return "data=OK registers"
	}
	return "heartbeat_only"
}

// deyeErrCodeName — человекочитаемое имя кода ошибки heartbeat Deye/Sofar
// (см. DeyeErrorCode в internal/solarman/frame.go).
func deyeErrCodeName(code byte) string {
	switch code {
	case 0x05:
		return "неверный Modbus-адрес устройства"
	case 0x06:
		return "SN логгера не совпадает"
	}
	return fmt.Sprintf("неизвестный код 0x%02X", code)
}

// redisRestoreLockKey — SETNX-маркер запуска реставрации Redis из PG: защищает от
// повторной реставрации, когда на один Redis смотрят два процесса (второй
// экземпляр). Захватывается на время реставрации, затем снимается.
const redisRestoreLockKey = "sunreceiver:restore:lock"

// restoreLockTTL — TTL маркера реставрации на случай падения процесса посреди
// работы (иначе «залипший» маркер навсегда заблокировал бы реставрацию).
const restoreLockTTL = 30 * time.Minute

// redisRestoreIncompleteKey — маркер НЕПОЛНОЙ реставрации: ставится, если хотя бы
// один ряд не удалось восстановить (сбой запроса PG или остановка по сигналу).
// Пока маркер есть, реставрация повторяется при следующем старте даже если Redis
// уже непуст (частично восстановленные данные не должны «замораживать» пропуск).
// Снимается после полностью успешной реставрации.
const redisRestoreIncompleteKey = "sunreceiver:restore:incomplete"

// redisBMSDataPresent — true, если в Redis есть данные BMS (bms26): HASH
// sunreceiver:bms26:current или ряд sunreceiver:bms26:series:*. Отдельно от
// IsEmpty, т.к. тот учитывает только инверторное current/series.
func redisBMSDataPresent(store *redisStore) (bool, error) {
	n, err := store.rdb.HLen(store.ctx, redisBms26CurrentKey).Result()
	if err != nil {
		return false, err
	}
	if n > 0 {
		return true, nil
	}
	keys, err := store.scanPrefixKeys(tsSeriesPrefix + "bms26" + ":*")
	if err != nil {
		return false, err
	}
	return len(keys) > 0, nil
}

// redisDts017DataPresent — true, если в Redis есть данные счётчика DTS017M
// (HASH sunreceiver:dts017m:current или ряд sunreceiver:dts017m:series:*).
// Отдельно от IsEmpty, т.к. тот учитывает только инверторное current/series:
// сложившийся ряд счётчика не должен быть засчитан пустым и перетёрт реставрацией.
func redisDts017DataPresent(store *redisStore) (bool, error) {
	n, err := store.rdb.HLen(store.ctx, redisDts017CurrentKey).Result()
	if err != nil {
		return false, err
	}
	if n > 0 {
		return true, nil
	}
	keys, err := store.scanPrefixKeys(tsSeriesPrefix + "dts017m" + ":*")
	if err != nil {
		return false, err
	}
	return len(keys) > 0, nil
}

// restoreRedisFromPG восстанавливает Redis из persistent-хранилища PostgreSQL
// за период [now-window, now], но не старше окна удержания Redis (последние 2
// календарных суток), иначе фоновая очистка сразу удалит восстановленное.
// Запускается в фоне при пустом Redis. Восстанавливаются ОБА ряда:
//   - снимки инверторов (invAverages) и обособленных (averagesAll) — SaveSnapshot в месячный
//     RT-ряда;
//   - точки BMS (pg.Bms26AveragesAll) — SaveBms26Series в ряд
//     sunreceiver:ts:bms26:*;
//   - точки DTS017M (pg.Dts017AveragesAll) — SaveDts017Series/SaveDts017Current в
//     обособленный ряд sunreceiver:ts:dts017m:*.
//
// Восстановление каждого ряда независимо: сбой одного запроса не прерывает
// остальные. Возвращает complete=false, если хотя бы один ряд восстановить не
// удалось (или пришло завершение по сигналу) — вызывающий ставит маркер неполной
// реставрации, и она повторяется при следующем старте (см. redisRestoreIncompleteKey).
//
// Замечание: штатно Redis-ряд BMS — сырые показания (samples=1), а
// восстановленный из PG участок представлен 5-минутными средними (samples>1);
// после первых новых опросов он дополняется сырыми точками.
func restoreRedisFromPG(store *redisStore, pg *pgStore, window time.Duration, stop context.Context) (complete bool) {
	end := time.Now()
	start := recentCutoff(end)
	if w := end.Add(-window); w.After(start) {
		start = w
	}
	log.Printf("pg restore: восстанавливаю Redis из PG за [%s, %s]",
		start.Format(time.RFC3339), end.Format(time.RFC3339))
	complete = true
	snaps, err := pg.averagesAll(start, end)
	if err != nil {
		log.Printf("pg restore: query: %v", err)
		complete = false
	} else {
		// Сортируем по ts ascending (запрос уже с ORDER BY ts, но сортировка
		// оставлена как страховка): в цикле ниже каждый SaveSnapshot кладёт точку в
		// HASH current[ip] — остаётся «последний из итерации». Сортируем, чтобы
		// последний HSet был самым свежим (детерминированный current[ip]).
		sort.SliceStable(snaps, func(i, j int) bool {
			ti, oki := parseTS(snaps[i].Timestamp)
			tj, okj := parseTS(snaps[j].Timestamp)
			if !oki || !okj {
				return false // битый ts не переставляем (стабильная сортировка)
			}
			return ti.Before(tj)
		})
		var restored int
		for _, snap := range snaps {
			select {
			case <-stop.Done():
				log.Printf("pg restore: остановлено по сигналу (восстановлено точек: %d)", restored)
				return false
			default:
			}
			ts, perr := time.Parse(time.RFC3339, snap.Timestamp)
			if perr != nil {
				continue
			}
			// Ключ серии RT ряда строится по ts (<prefix><YYYY-MM>).
			// Живая запись пишет по локальной зоне, а тут ts из RFC3339 обычно в UTC
			// — приводим к time.Local для того же месяца-ключа.
			ts = ts.In(time.Local)
			if snap.IP == meterDeviceKey {
				// DDS238 восстанавливаем в обособленные ключи.
				if serr := store.SaveDds238Series(snap, ts); serr != nil {
					log.Printf("pg restore: dds238 save: %v", serr)
					continue
				}
				if serr := store.SaveDds238Current(snap); serr != nil {
					log.Printf("pg restore: dds238 current: %v", serr)
				}
				restored++
				continue
			}
			if snap.IP == mapDevKey {
				// МАП восстанавливаем в обособленные ключи.
				if serr := store.saveMapSnapshot(snap, ts); serr != nil {
					log.Printf("pg restore: map save: %v", serr)
					continue
				}
				restored++
				continue
			}
			if isKesKey(snap.IP) {
				// КЭС восстанавливаем в обособленные ключи.
				if serr := store.saveKesSnapshot(snap, ts); serr != nil {
					log.Printf("pg restore: kes save: %v", serr)
					continue
				}
				restored++
				continue
			}
			if serr := store.SaveSnapshot(snap, ts); serr != nil {
				log.Printf("pg restore: save %s: %v", snap.IP, serr)
				continue
			}
			restored++
		}
		log.Printf("pg restore: завершено, восстановлено точек: %d", restored)
	}

	// Ряд BMS (единый тип ANT+EnBMS) — из pg.bms26_averages_r (5-мин средние) в
	// sunreceiver:ts:bms26:* (то же окно удержания).
	bmsPts, err := pg.Bms26AveragesAll(start, end)
	if err != nil {
		log.Printf("pg restore: bms26 query: %v", err)
		complete = false
	} else {
		var bmsRestored int
		for i := range bmsPts {
			select {
			case <-stop.Done():
				log.Printf("pg restore: остановлено по сигналу (BMS восстановлено точек: %d)", bmsRestored)
				return false
			default:
			}
			ts, perr := time.Parse(time.RFC3339, bmsPts[i].Timestamp)
			if perr != nil {
				continue
			}
			ts = ts.In(time.Local)
			if serr := store.SaveBms26Series(bmsPts[i], ts); serr != nil {
				log.Printf("pg restore: bms26 save %s: %v", bmsPts[i].Serial, serr)
				continue
			}
			bmsRestored++
		}
		log.Printf("pg restore: BMS восстановлено точек: %d", bmsRestored)
	}

	// Ряд DTS017M — из pg.dts017m_averages_r (5-мин средние) в
	// sunreceiver:ts:dts017m:* (то же окно удержания); current —
	// последняя восстановленная точка.
	dtsPts, err := pg.Dts017AveragesAll(start, end)
	if err != nil {
		log.Printf("pg restore: dts017m query: %v", err)
		complete = false
	} else {
		var dtsRestored int
		var dtsLast *dts017Snapshot
		for i := range dtsPts {
			select {
			case <-stop.Done():
				log.Printf("pg restore: остановлено по сигналу (DTS017M восстановлено точек: %d)", dtsRestored)
				return false
			default:
			}
			snap := dtsPts[i]
			ts, perr := time.Parse(time.RFC3339, snap.Timestamp)
			if perr != nil {
				continue
			}
			// Ключ месяца (dts017SeriesKey) приводим к локальной зоне, как живая запись.
			ts = ts.In(time.Local)
			if serr := store.SaveDts017Series(snap, ts); serr != nil {
				log.Printf("pg restore: dts017m save %s: %v", snap.Name, serr)
				continue
			}
			cur := snap
			dtsLast = &cur
			dtsRestored++
		}
		if dtsLast != nil {
			if serr := store.SaveDts017Current(*dtsLast); serr != nil {
				log.Printf("pg restore: dts017m current: %v", serr)
			}
		}
		log.Printf("pg restore: DTS017M восстановлено точек: %d", dtsRestored)
	}

	return complete
}

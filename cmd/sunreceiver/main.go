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

	"github.com/galiy/sunReceiver/internal/solarman"
)

// Периоды опроса устройств (секунды в конфиге, раздел "poll"). Значения по
// умолчанию; переопределяются из sunReceiver.json при загрузке (см. applyPollConfig).
var (
	pollPeriod        = 10 * time.Second // инверторы (Deye/Sofar)
	mapPollInterval   = time.Second      // МАП + MPPT
	meterPollInterval = time.Second      // счётчик DDS238
)

const (
	port = "8899"
	// inverterModbusErrPersist — сколько «неверный Modbus-адрес» (ErrCode 0x05)
	// должен держаться непрерывно, прежде чем попасть в историю ошибок (антишум).
	inverterModbusErrPersist = 10 * time.Minute
	timeout                  = 15 * time.Second
	// defaultIdleWindow — «тишина» между байтами ответа (Deye): паузы < 4 с.
	defaultIdleWindow = 4 * time.Second
	// sofarIdleWindow — Sofar LSW-3 шлёт куски с паузами до ~6.5 с — окно шире.
	sofarIdleWindow = 8 * time.Second
	// maxExchangeTotal — общий лимит приёма ОДНОГО ответа Solarman: живые ответы
	// идут до ~30 с (Sofar pacing), Deye ~8 с; «зомби»-логгер без лимита держал
	// бы Exchange часами.
	maxExchangeTotal = 60 * time.Second
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
// статических устройств из конфига (инверторы + МАП). Order = mpptOrderBase + slot.
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

// invTarget — целевой инвертор. LoggerSN — серийный номер даталоггера,
// обязателен для Deye (иначе логгер отвечает кодом 0x06 "serial number not match").
// Name — логическое имя из sunReceiver.json (например, "Deye Left").
// Slot — для kindMAP: -1 (агрегат); для kindMPPT: индекс контроллера (слот) в ответе API.
type invTarget struct {
	IP       string
	Name     string
	LoggerSN uint32
	Kind     targetKind
	Slot     int
	UID      string // для kindMPPT: UID контроллера из API «Малина26» (стабильный идентификатор)
	Order    int    // порядок устройства на дашборде (индекс в конфиге; MPPT — всегда последними)
	// Placement — размещение инвертора (группа на дашборде «Мощности инверторов»),
	// из поля placement sunReceiver.json; пустое значение приводится к «Дом».
	// У MPPT-контроллеров (КЭС) не используется — они в рамке не участвуют.
	Placement string
	// InverterSN — кэш серийного номера инвертора (для Deye/Sofar) на время жизни
	// пулера: серийник постоянен, благодаря этому HW-диапазон не перечитывается с
	// 3 ретраями на каждый опрос (см. runInverterPoll). Для остальных марок не используется.
	InverterSN string
}

// configInverter — запись инвертора (Deye/Sofar) в sunReceiver.json разделе "invertors".
// Disabled — ОБЯЗАТЕЛЬНОЕ поле (отсутствие = ошибка конфига): false = опрашивается,
// true = временно отключён (устройство в конфиге, но не опрашивается). Placement —
// размещение инвертора (необязательное; по умолчанию «Дом»): инверторы одной группы
// суммируются на дашборде в отдельной рамке «Мощности инверторов» (пара плашек
// «активная + PV» на каждое размещение).
type configInverter struct {
	IP        string `json:"ip"`
	Name      string `json:"name"`
	Type      string `json:"type"`
	LoggerSN  uint32 `json:"logger_sn"`
	Placement string `json:"placement,omitempty"`
	Disabled  *bool  `json:"disabled"`
}

// mapSection — раздел "map" sunReceiver.json (HTTP-API «Малина26»): токен по
// паролю + бинарные контракты МАП (`/api/map/raw`) и КЭС (`/api/kes/raw`).
// host — IP или имя; port — 80/443; serial — серийник МАП (число строкой), по нему
// ищем «свой» МАП в `/api/map/devices`; fallback_serial — необязательный резервный
// серийник (опрос при недоступности основного, идентичность остаётся основной).
//
// Disabled — ОБЯЗАТЕЛЬНОЕ поле (отсутствие = ошибка конфига): true отключает ВСЕ
// пулеры раздела map (МАП, КЭС, BMS26); на пулеры сетевых инверторов не влияет.
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
	Inverter int `json:"inverter"` // инверторы Deye/Sofar
	Map      int `json:"map"`      // МАП + MPPT
	Meter    int `json:"meter"`    // счётчик DDS238
}

type configFile struct {
	Invertors     []configInverter `json:"invertors"`
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
		{"inverter", p.Inverter},
		{"map", p.Map},
		{"meter", p.Meter},
	}
	for _, f := range fields {
		if f.val <= 0 {
			return fmt.Errorf("обязательное поле poll.%s должно быть > 0 (секунды)", f.name)
		}
	}
	pollPeriod = time.Duration(p.Inverter) * time.Second
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
	if err := check(rawFold(top, "map"), "map", "method", "rs485", "malina", "malina26"); err != nil {
		return err
	}
	if err := check(rawFold(top, "poll"), "poll", "ce308", "antbms"); err != nil {
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
	targets := make([]invTarget, 0, len(cf.Invertors)+1)
	nextOrder := 0                // порядок устройства на дашборде = позиция в конфиге (в порядке invertors, затем map)
	seenIP := map[string]string{} // ip -> имя устройства: запрет дублей (общий клиент/ключ Redis)

	// Инверторы (Deye/Sofar) из invertors; отключённые (disabled=true) пропускаются.
	for _, t := range cf.Invertors {
		if t.Disabled == nil {
			return nil, nil, nil, nil, nil, nil, 0, fmt.Errorf("config %s: для %s (%s) не задано обязательное поле disabled (false/true)", path, t.Name, t.IP)
		}
		if *t.Disabled {
			log.Printf("config: %s (%s) отключён (disabled=true) — не опрашивается", t.Name, t.IP)
			continue
		}
		var kind targetKind
		switch t.Type {
		case "deye":
			kind = kindDeyeString
		case "sofar":
			kind = kindSofar
		default:
			return nil, nil, nil, nil, nil, nil, 0, fmt.Errorf("config %s: неизвестный тип %q для %s", path, t.Type, t.IP)
		}
		if t.IP == "" {
			return nil, nil, nil, nil, nil, nil, 0, fmt.Errorf("config %s: пустой ip (type=%s)", path, t.Type)
		}
		if t.Name == "" {
			return nil, nil, nil, nil, nil, nil, 0, fmt.Errorf("config %s: пустое имя name для %s", path, t.IP)
		}
		// Deye/Sofar: без серийного номера логгера (logger_sn=0) логгер отвечает
		// кодом 0x06 (heartbeat_only) — данные получать невозможно. Ловим при
		// старте (fatal), а не маскируем вечным heartbeat_only с логами на poll.
		if (kind == kindDeyeString || kind == kindSofar) && t.LoggerSN == 0 {
			return nil, nil, nil, nil, nil, nil, 0, fmt.Errorf("config %s: %s (%s): не задан logger_sn — без SN логгера логгер отвечает кодом 0x06 и данные получать невозможно", path, t.Name, t.IP)
		}
		if prev, dup := seenIP[t.IP]; dup {
			return nil, nil, nil, nil, nil, nil, 0, fmt.Errorf("config %s: дублирующийся ip %s (%s и %s) — опрос одного IP двумя целями недопустим", path, t.IP, prev, t.Name)
		}
		seenIP[t.IP] = t.Name
		placement := t.Placement
		if placement == "" {
			placement = "Дом"
		}
		targets = append(targets, invTarget{IP: t.IP, Name: t.Name, LoggerSN: t.LoggerSN, Kind: kind, Slot: -1, Order: nextOrder, Placement: placement})
		nextOrder++
	}

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
		if cf.Map.Host == "" || cf.Map.Password == "" || strings.TrimSpace(cf.Map.Serial) == "" {
			return nil, nil, nil, nil, nil, nil, 0, fmt.Errorf("config %s: раздел map требует host, password и serial", path)
		}
		if _, err := strconv.ParseUint(strings.TrimSpace(cf.Map.Serial), 10, 64); err != nil {
			return nil, nil, nil, nil, nil, nil, 0, fmt.Errorf("config %s: map.serial должен быть числом строкой: %w", path, err)
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
	if len(targets) == 0 && !mapActive && ce308Cfg == nil && !meterActive && !relayActive && dts017Cfg == nil {
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

// placementOrder возвращает упорядоченный список размещений сетевых инверторов
// (Deye/Sofar) — по порядку первого появления в targets (т.е. в sunReceiver.json),
// пустое размещение приводится к «Дом». MPPT-контроллеры (КЭС) и МАП не участвуют:
// у них нет размещения (КЭС в рамке «Мощности инверторов» не считаются). Список
// задаёт порядок пар плашек (активная + PV) в рамке и передаётся на дашборд.
func placementOrder(targets []invTarget) []string {
	seen := map[string]bool{}
	var out []string
	for _, t := range targets {
		if t.Kind != kindDeyeString && t.Kind != kindSofar {
			continue
		}
		p := t.Placement
		if p == "" {
			p = "Дом"
		}
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out
}

// placeByIP возвращает размещение каждого инвертора по IP (из конфига) — надёжный
// источник для группировки анимации: у устаревшего снимка placement может не быть.
// Охватывает сетевые инверторы (Deye/Sofar) и МАП/КЭС.
func placeByIP(targets []invTarget) map[string]string {
	m := make(map[string]string, len(targets))
	for _, t := range targets {
		m[t.IP] = t.Placement
	}
	return m
}

// inverterNameByIP — имя сетевого инвертора (Deye/Sofar) по IP. В PG device_errors
// инверторы пишутся с device = IP; для дашборда показываем имя вместо адреса.
func inverterNameByIP(targets []invTarget) map[string]string {
	m := make(map[string]string)
	for _, t := range targets {
		if t.Kind != kindSofar && t.Kind != kindDeyeString {
			continue
		}
		if t.IP != "" && t.Name != "" {
			m[t.IP] = t.Name
		}
	}
	return m
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
	if malina26 != nil {
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

var targets []invTarget

var statusNames = map[uint16]string{
	0: "standby", 1: "self-checking", 2: "normal", 3: "fault", 4: "permanent",
}

var faultBits = []struct {
	Bit  uint16
	Name string
}{
	{1, "grid_over_voltage"}, {2, "grid_under_voltage"}, {4, "grid_over_frequency"},
	{8, "grid_under_frequency"}, {16, "pv_under_voltage"}, {32, "grid_low_voltage_ride_through"},
	{256, "pv_over_voltage"}, {512, "pv_current_unbalanced"}, {1024, "pv_input_mode_wrong"},
	{2048, "gfc_i_fault"}, {4096, "phase_sequence"}, {8192, "boost_over_current"},
	{16384, "ac_over_current"}, {32768, "grid_current_high"},
}

var countries = map[uint16]string{
	0: "Germany", 1: "CEI0-21 Internal", 2: "Australia", 3: "Spain RD1699",
	4: "Turkey", 5: "Denmark", 6: "Greece", 7: "Netherland", 8: "Belgium",
	9: "UK-G59", 10: "China", 11: "France", 12: "Poland", 13: "Germany BDEW",
	14: "Germany VDE0126", 15: "Italy CEI0-16", 16: "UK-G83", 17: "Greece Islands",
	18: "EU EN50438", 19: "EU EN61727", 20: "Korea", 21: "Sweden",
	22: "Europe General", 23: "CEI0-21 External", 24: "Cyprus", 25: "India",
	26: "Philippines", 27: "New Zealand",
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
	// Kind — марка/тип устройства из конфига (invTarget.Kind): "deye"|"sofar" для
	// сетевых инверторов, "kes" для MPPT-контроллеров (КЭС), "" — МАП/счётчик.
	// Заполняется пулером; используется страницей анимации для выбора спрайта.
	Kind   string         `json:"kind,omitempty"`
	Values valuesContract `json:"values"`
}

func int16val(v uint16) int {
	return int(int16(v))
}

func faultNames(mask uint16) []string {
	var names []string
	for _, fb := range faultBits {
		if mask&fb.Bit != 0 {
			names = append(names, fb.Name)
		}
	}
	if names == nil {
		names = []string{"no_error"}
	}
	return names
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

// putSofarSimple пишет 16-битный регистр в контракт: int при ratio==1, иначе float.
// Регистр трактуется как БЕЗЗНАКОВЫЙ (физические величины неотрицательны:
// напряжения, токи, мощности, частота, энергия, времена, сопротивления изоляции).
func putSofarSimple(out valuesContract, regs map[uint16]uint16, addr uint16, key string, ratio float64) {
	v, ok := regs[addr]
	if !ok {
		return
	}
	if ratio == 1 {
		out[key] = int(v)
	} else {
		out[key] = float64(v) * ratio
	}
}

// putSofarSigned пишет 16-битный регистр ЗНАКОВОГО значения (может быть
// отрицательным): реактивная мощность (var) и температуры модуля/инвертора.
func putSofarSigned(out valuesContract, regs map[uint16]uint16, addr uint16, key string, ratio float64) {
	v, ok := regs[addr]
	if !ok {
		return
	}
	if ratio == 1 {
		out[key] = int16val(v)
	} else {
		out[key] = float64(int16val(v)) * ratio
	}
}

// putSofarU32 пишет 32-битное значение (hi*65536+lo) с ratio.
func putSofarU32(out valuesContract, regs map[uint16]uint16, hiAddr, loAddr uint16, key string, ratio float64) {
	hi, ok1 := regs[hiAddr]
	lo, ok2 := regs[loAddr]
	if !ok1 || !ok2 {
		return
	}
	val := float64(hi)*65536 + float64(lo)
	if ratio == 1 {
		out[key] = val
	} else {
		out[key] = val * ratio
	}
}

// mapSofarRegisters строит значения универсального контракта из блока 0x0000-0x0027.
func mapSofarRegisters(regs map[uint16]uint16) valuesContract {
	out := valuesContract{}

	if v, ok := regs[0x0000]; ok {
		s, ok := statusNames[v]
		if !ok {
			s = fmt.Sprintf("unknown(%d)", v)
		}
		out["inverter_status"] = s
	}
	for i, name := range []string{"fault_1", "fault_2", "fault_3", "fault_4", "fault_5"} {
		if v, ok := regs[uint16(0x0001+i)]; ok {
			out[name] = faultNames(v)
		}
	}

	// PV входы (V, A, W)
	putSofarSimple(out, regs, 0x0006, "pv1_voltage", 0.1)
	putSofarSimple(out, regs, 0x0007, "pv1_current", 0.01)
	putSofarSimple(out, regs, 0x0008, "pv2_voltage", 0.1)
	putSofarSimple(out, regs, 0x0009, "pv2_current", 0.01)
	putSofarSimple(out, regs, 0x000A, "pv1_power", 10)
	putSofarSimple(out, regs, 0x000B, "pv2_power", 10)

	// AC выход: активная W, реактивная var, частота Hz
	putSofarSimple(out, regs, 0x000C, "ac_active_power", 10)
	putSofarSigned(out, regs, 0x000D, "ac_reactive_power", 10) // ×0.01 kVar → var (может быть <0)
	putSofarSimple(out, regs, 0x000E, "grid_frequency", 0.01)

	// Фазы L1/L2/L3 (V, A)
	putSofarSimple(out, regs, 0x000F, "l1_voltage", 0.1)
	putSofarSimple(out, regs, 0x0010, "l1_current", 0.01)
	putSofarSimple(out, regs, 0x0011, "l2_voltage", 0.1)
	putSofarSimple(out, regs, 0x0012, "l2_current", 0.01)
	putSofarSimple(out, regs, 0x0013, "l3_voltage", 0.1)
	putSofarSimple(out, regs, 0x0014, "l3_current", 0.01)

	// Энергия (kWh) и время
	putSofarU32(out, regs, 0x0015, 0x0016, "energy_total", 1) // 32 бит, уже kWh
	putSofarU32(out, regs, 0x0017, 0x0018, "time_total", 1)   // 32 бит, h
	putSofarSimple(out, regs, 0x0019, "energy_today", 0.01)   // ×10 Wh → kWh
	putSofarSimple(out, regs, 0x001A, "time_today", 1)        // min

	// Температуры (C), шина
	putSofarSigned(out, regs, 0x001B, "temperature_module", 1)
	putSofarSigned(out, regs, 0x001C, "temperature_inner", 1)
	putSofarSimple(out, regs, 0x001D, "bus_voltage", 0.1)

	// Диагностика Sofar
	putSofarSimple(out, regs, 0x001E, "pv1_sample_cpu_voltage", 0.1)
	putSofarSimple(out, regs, 0x001F, "pv1_sample_cpu_current", 0.01)
	putSofarSimple(out, regs, 0x0020, "countdown_time", 1)
	putSofarSimple(out, regs, 0x0021, "alert", 1)
	putSofarSimple(out, regs, 0x0022, "input_mode", 1)
	putSofarSimple(out, regs, 0x0023, "comm_board_msg", 1)
	putSofarSimple(out, regs, 0x0024, "insulation_pv1_to_ground", 1)
	putSofarSimple(out, regs, 0x0025, "insulation_pv2_to_ground", 1)
	putSofarSimple(out, regs, 0x0026, "insulation_pv_minus_to_ground", 1)

	if v, ok := regs[0x0027]; ok {
		c, ok := countries[v]
		if !ok {
			c = fmt.Sprintf("unknown(%d)", v)
		}
		out["country"] = c
	}
	return out
}

// probableSofarBlock возвращает true, если блок регистров выглядит как настоящий
// полный блок LSW-3 (0x0000-0x0027), а не сбойный кадр: логгер иногда отдаёт PDU
// с валидным CRC, но мусором (повторяющееся слово вместо регистров, частота ~220 Гц,
// пустая энергия), что даёт абсурдные пики мощности вроде ac_active_power = 220680 W.
// Валидных полных блоков 20-40 регистров; «дубль» 16 регистров (0x0010-0x001F) и
// Placeholder тоже не подходят. Физические пороги консервативные (с запасом от
// реальных значений инвертора 2.5 kVA), чтобы не отсечь аномальные, но реальные данные.
func probableSofarBlock(r map[uint16]uint16) bool {
	if len(r) < 20 {
		return false
	}

	// Частота сети: 0 или 40..80 Гц (raw ×0.01 → 4000..8000). ~220 Гц (22000) — мусор.
	if v, ok := r[0x000E]; ok {
		if hz := int16(v); hz != 0 && (hz < 4000 || hz > 8000) {
			return false
		}
	}

	// Напряжения фаз L1/L2/L3 (raw ×0.1): физически 0..270 В.
	for _, addr := range []uint16{0x000F, 0x0011, 0x0013} {
		if v, ok := r[addr]; ok {
			if volts := float64(int16(v)) * 0.1; volts < -300 || volts > 300 {
				return false
			}
		}
	}

	// PV1/PV2 напряжение (raw ×0.1): физически 0..450 В.
	for _, addr := range []uint16{0x0006, 0x0008} {
		if v, ok := r[addr]; ok {
			if volts := float64(int16(v)) * 0.1; volts < -450 || volts > 450 {
				return false
			}
		}
	}

	// Активная и реактивная мощность (raw ×10): для 2.5 kVA инвертора
	// |W| не бывает > 10 кВт. Пик 220680 Вт (raw 22068) — отсекаем.
	for _, addr := range []uint16{0x000C, 0x000D} {
		if v, ok := r[addr]; ok {
			if w := int64(int16(v)) * 10; w > 10000 || w < -10000 {
				return false
			}
		}
	}

	return true
}

// deyeAlertState — последнее залогированное значение Alert-регистров Deye по IP
// (для лога изменений и троттлинга heartbeat).
var (
	deyeAlertMu   sync.Mutex
	deyeAlertLast = map[string]string{}
)

// deyeRawAlertLog — сырой лог Alert-регистров Deye (`deye alert ...`).
// ОТКЛЮЧЁН: карта откалибрована по снятым логам (2026-10-01…05 — единственная
// ненулевая аварийная область: рег. 0x69=0x0004 при status(0x3B)=4; 0x229-0x22E
// всегда нули). Для повторной калибровки поставить true.
const deyeRawAlertLog = false

// logDeyeRawAlert пишет СЫРЫЕ Alert-регистры Deye (0x65–0x6A, 6×16 бит) в журнал
// с временем снятия — для калибровки карты аварий. По нормам апстрима Alert
// находится именно здесь (single-phase hybrid), для string-модели официальной
// карты нет. Логируем при изменении и не реже 10 минут.
func logDeyeRawAlert(ip string, status int, hexA, hexB, warns string) {
	key := hexA + "|" + hexB + "|" + warns + "|" + fmt.Sprint(status)
	deyeAlertMu.Lock()
	defer deyeAlertMu.Unlock()
	now := time.Now()
	if last, ok := deyeAlertLast[ip]; ok && last == key && now.Sub(lastDeyeAlertLog[ip]) < 10*time.Minute {
		return
	}
	deyeAlertLast[ip] = key
	lastDeyeAlertLog[ip] = now
	extra := ""
	if warns != "" {
		extra = " warns=[ " + warns + " ]"
	}
	log.Printf("deye alert %s: status(0x3B)=%d regs 0x65-0x6A=0x%s regs 0x229-0x22E=0x%s%s",
		ip, status, hexA, hexB, extra)
}

// deyeRegsHex собирает hex-строку из count регистров, начиная с addr, из карты.
// Если регистры не читались — пустая строка.
func deyeRegsHex(regs map[uint16]uint16, addr, count uint16) string {
	var b strings.Builder
	for i := uint16(0); i < count; i++ {
		v, ok := regs[addr+i]
		if !ok {
			return ""
		}
		fmt.Fprintf(&b, "%04x", v)
	}
	return b.String()
}

var lastDeyeAlertLog = map[string]time.Time{}

// deyeSensor — маппинг регистра Deye string/grid-tie инвертора
// (из kbialek/deye-inverter-mqtt, диапазоны 0x3C-0x74 и 0xC6-0xD2).
// Name — имя для raw_registers; Tag — имя универсального контракта для values.
type deyeSensor struct {
	Name   string
	Tag    string
	Ratio  float64
	Unit   string
	Signed bool
	Offset float64
	Double bool // 32-бит (2 регистра), low word first
}

// 0x58 AC reactive power Deye — ×0.01 kvar (как у Sofar ×0.01 kVar), в var → ratio 10.
var deyeRegMap = map[uint16]deyeSensor{
	0x3C: {"production_today", "energy_today", 0.1, "kWh", false, 0, false},
	0x3E: {"uptime", "uptime", 1, "min", false, 0, false},
	0x3F: {"total_production", "energy_total", 0.1, "kWh", false, 0, true},
	0x46: {"grid_l12_voltage", "grid_l12_voltage", 0.1, "V", false, 0, false},
	0x47: {"grid_l23_voltage", "grid_l23_voltage", 0.1, "V", false, 0, false},
	0x48: {"grid_l31_voltage", "grid_l31_voltage", 0.1, "V", false, 0, false},
	0x49: {"l1_voltage", "l1_voltage", 0.1, "V", false, 0, false},
	0x4A: {"l2_voltage", "l2_voltage", 0.1, "V", false, 0, false},
	0x4B: {"l3_voltage", "l3_voltage", 0.1, "V", false, 0, false},
	0x4C: {"l1_current", "l1_current", 0.1, "A", false, 0, false},
	0x4D: {"l2_current", "l2_current", 0.1, "A", false, 0, false},
	0x4E: {"l3_current", "l3_current", 0.1, "A", false, 0, false},
	0x4F: {"ac_frequency", "grid_frequency", 0.01, "Hz", false, 0, false},
	0x52: {"dc_total_power", "dc_total_power", 0.1, "W", false, 0, false},
	0x54: {"ac_apparent_power", "ac_apparent_power", 0.1, "W", false, 0, false},
	0x56: {"ac_active_power", "ac_active_power", 0.1, "W", false, 0, true},
	// 0x58 AC reactive power: ×0.1 var (raw 365 → 36.5 var, физически правдоподобно;
	// ×10 давал 3650 var — абсурд при P≈404W). Единицы совпадают с Sofar (var).
	0x58: {"ac_reactive_power", "ac_reactive_power", 0.1, "var", true, 0, false},
	0x5A: {"radiator_temperature", "temperature_radiator", 0.1, "C", false, -100, false},
	0x5B: {"igbt_temperature", "temperature_igbt", 0.1, "C", false, -100, false},
	0x6D: {"pv1_voltage", "pv1_voltage", 0.1, "V", false, 0, false},
	0x6E: {"pv1_current", "pv1_current", 0.1, "A", false, 0, false},
	0x6F: {"pv2_voltage", "pv2_voltage", 0.1, "V", false, 0, false},
	0x70: {"pv2_current", "pv2_current", 0.1, "A", false, 0, false},
	0x71: {"pv3_voltage", "pv3_voltage", 0.1, "V", false, 0, false},
	0x72: {"pv3_current", "pv3_current", 0.1, "A", false, 0, false},
	0x73: {"pv4_voltage", "pv4_voltage", 0.1, "V", false, 0, false},
	0x74: {"pv4_current", "pv4_current", 0.1, "A", false, 0, false},
	0xC6: {"load_power", "load_power", 1, "W", true, 0, true},
	0xC8: {"daily_load_consumption", "energy_load_today", 0.01, "kWh", false, 0, false},
	0xC9: {"total_load_consumption", "energy_load_total", 0.1, "kWh", false, 0, true},
	// 0xCB "Grid power" Deye НЕ маппится: тег grid_power зарезервирован контрактом
	// за устройством МАП (kindMAP) — плашки/график МАП строятся по нему. Маппинг
	// сюда из Deye-регистра давал бы ложные нули в ряду мощности сети МАП.
	0xCD: {"daily_energy_sold", "energy_sold_today", 0.01, "kWh", false, 0, false},
	0xCE: {"total_energy_sold", "energy_sold_total", 0.1, "kWh", false, 0, true},
	0xD0: {"daily_energy_bought", "energy_bought_today", 0.01, "kWh", false, 0, false},
	0xD1: {"total_energy_bought", "energy_bought_total", 0.1, "kWh", false, 0, true},
}

// deyeFaultNames — карта fault-кодов Deye (F1..F64) по номеру. Источник:
// FAULT_TABLE проекта pbix/HA-solark-PV (Sol-Ark/Deye, те же регистры): 64-битная
// карта R103–R106 = 0x67–0x6A (wordorder LSW-first), бит N-1 = F(N). Проверено на
// живом событии: пропадание сети даёт бит 0x69.2 → F35 AC_NoUtility.
var deyeFaultNames = map[int]string{
	1:  "F1: инвертирование DC (параллельный блок выключен)",
	8:  "F8: неисправность реле GFDI (защита от утечки)",
	13: "F13: смена режима сети/батареи",
	14: "F14: перегрузка по току DC",
	15: "F15: перегрузка по току AC (ПО)",
	16: "F16: неисправность GFCI",
	18: "F18: перегрузка по току AC (аппаратная)",
	20: "F20: перегрузка по току DC (аппаратная)",
	22: "F22: аварийный останов (EmergStop)",
	23: "F23: GFCI/утечка — импульсная перегрузка",
	24: "F24: нарушение изоляции DC (PV)",
	25: "F25: обратная связь DC (нет АКБ при Activate Battery)",
	26: "F26: разбаланс DC-шины",
	29: "F29: ошибка CAN в параллельной системе",
	30: "F30: неисправность главного контактора AC",
	31: "F31: сбой плавного пуска",
	34: "F34: перегрузка AC",
	35: "F35: нет сети (AC_NoUtility)",
	37: "F37: программная перегрузка DC",
	39: "F39: аппаратная перегрузка DC",
	40: "F40: перегрузка АКБ по току",
	41: "F41: останов параллельной системы",
	45: "F45: AC: низкое/высокое напряжение сети",
	46: "F46: неисправность АКБ/резервной системы",
	47: "F47: повышение частоты сети",
	48: "F48: понижение частоты сети",
	55: "F55: высокое напряжение DC/PV",
	56: "F56: низкое напряжение DC (АКБ разряжена)",
	58: "F58: ошибка связи с BMS",
	60: "F60: генератор: напряжение/частота вне допуска",
	61: "F61: ручное выключение (параллельный ведомый)",
	63: "F63: дуговая защита (AFCI)",
	64: "F64: перегрев радиатора",
}

// deyeWarnNames — карта предупреждений Deye W1..W32 (источник: страница Inversol
// «Deye error codes W1–W32 F1–F64»). Регистры предупреждений — 0x65/0x66
// (32 бита, LSW-first), бит N-1 = W(N).
var deyeWarnNames = map[int]string{
	1:  "W1: напряжение сети у порога",
	2:  "W2: неисправность вентилятора",
	3:  "W3: неправильная фазировка сети",
	4:  "W4: ошибка связи со счётчиком",
	5:  "W5: ошибка обновления прошивки",
	6:  "W6: напряжение PV у предела",
	7:  "W7: ток PV у предела",
	8:  "W8: напряжение DC-шины у предела",
	9:  "W9: ток AC у порога",
	10: "W10: высокая внутренняя температура",
	11: "W11: высокая температура радиатора",
	12: "W12: общее аппаратное предупреждение",
	13: "W13: несовпадение версии прошивки",
	14: "W14: частота сети у предела",
	15: "W15: высокое сопротивление сети",
	16: "W16: небаланс напряжений фаз",
	17: "W17: небаланс токов фаз",
	18: "W18: ограничение мощности (derating)",
	19: "W19: активно управление реактивной мощностью",
	20: "W20: активно ограничение экспорта",
	21: "W21: активен режим нулевого экспорта",
	22: "W22: активно управление DRM",
	23: "W23: инвертор управляется удалённо",
	24: "W24: предупреждение синхронизации параллельной системы",
	25: "W25: ограничение мощности параллельной системы",
	26: "W26: напряжение АКБ у предела",
	27: "W27: температура АКБ у предела",
	28: "W28: аномальные данные BMS",
	29: "W29: задержка шины связи",
	30: "W30: высокая нагрузка системы",
	31: "W31: предупреждение связи с АКБ",
	32: "W32: предупреждение",
}

// decodeDeyeWarnings декодирует предупреждения Deye из регистров 0x65/0x66
// (младшее слово — 0x65), бит N-1 = W(N). Предупреждения — не аварии, в историю
// ошибок не пишутся (только в сырой лог). Решение (2026-10-06): W1–W32 остаются
// информационными и показываются ТОЛЬКО в сыром логе (deyeRawAlertLog); запись в
// device_errors не делается (много рутинных W — шум и ложная тревожность).
func decodeDeyeWarnings(regs map[uint16]uint16) []string {
	bitmap := uint32(regs[0x65]) | uint32(regs[0x66])<<16
	if bitmap == 0 {
		return nil
	}
	var out []string
	for bit := 0; bit < 32; bit++ {
		if bitmap&(1<<uint(bit)) == 0 {
			continue
		}
		n := bit + 1
		if name, ok := deyeWarnNames[n]; ok {
			out = append(out, name)
		} else {
			out = append(out, fmt.Sprintf("W%d: предупреждение", n))
		}
	}
	return out
}

// decodeDeyeFaults декодирует аварии Deye из сырых регистров: 64-битная карта
// R103–R106 = 0x67–0x6A (первый регистр — младшее слово), бит N-1 = F(N).
// Возвращает человекочитаемые активные коды (появлений). Пусто — аварий нет.
func decodeDeyeFaults(regs map[uint16]uint16) []string {
	get := func(a uint16) uint64 { return uint64(regs[a]) }
	bitmap := get(0x67) | get(0x68)<<16 | get(0x69)<<32 | get(0x6A)<<48
	if bitmap == 0 {
		return nil
	}
	var out []string
	for bit := 0; bit < 64; bit++ {
		if bitmap&(1<<uint(bit)) == 0 {
			continue
		}
		n := bit + 1
		if name, ok := deyeFaultNames[n]; ok {
			out = append(out, name)
		} else {
			out = append(out, fmt.Sprintf("F%d: авария (нет описания)", n))
		}
	}
	return out
}

// mapDeyeRegisters строит значения универсального контракта Deye string-инвертора
// из набора регистров (по абсолютному адресу). Пишет по Tag (контрактное имя).
func mapDeyeRegisters(regs map[uint16]uint16) valuesContract {
	out := valuesContract{}
	for addr, def := range deyeRegMap {
		if !def.Double {
			v, ok := regs[addr]
			if !ok {
				continue
			}
			iv := int(int16(v))
			if def.Signed {
				out[def.Tag] = float64(iv)*def.Ratio + def.Offset
			} else {
				out[def.Tag] = float64(v)*def.Ratio + def.Offset
			}
			continue
		}
		// 32-бит, low word first (addr = low, addr+1 = high)
		lo, ok1 := regs[addr]
		hi, ok2 := regs[addr+1]
		if !ok1 || !ok2 {
			continue
		}
		val := uint32(hi)<<16 | uint32(lo)
		if def.Signed {
			out[def.Tag] = float64(int32(val))*def.Ratio + def.Offset
		} else {
			out[def.Tag] = float64(val)*def.Ratio + def.Offset
		}
	}
	// Мощность PV-входов: Deye string отдаёт только V и I на каждый вход
	// (0x6D/0x6E — PV1, 0x6F/0x70 — PV2); мощность считаем как P = V * I (W).
	if v1, ok := out["pv1_voltage"]; ok {
		if i1, ok := out["pv1_current"]; ok {
			out["pv1_power"] = v1.(float64) * i1.(float64)
		}
	}
	if v2, ok := out["pv2_voltage"]; ok {
		if i2, ok := out["pv2_current"]; ok {
			out["pv2_power"] = v2.(float64) * i2.(float64)
		}
	}
	return out
}

// clients — кэш объектов solarman.Client по ip (конфиг + счётчик порядкового
// номера кадра). Сам TCP-сокет НЕ переиспользуется: Client.Exchange открывает
// свежее соединение на каждый запрос и закрывает его после чтения ответа
// (логгеры отвечают медленно и могут оставлять «висящее» соединение).
// Опрос одного инвертора всегда идёт из одной горутины, поэтому счётчик кадра
// не гоняется между опросами.
var (
	clientsMu    sync.Mutex
	clientsByKey = map[string]*solarman.Client{}
)

func clientFor(ip string, sn uint32) *solarman.Client {
	key := ip + ":" + port
	clientsMu.Lock()
	defer clientsMu.Unlock()
	if c, ok := clientsByKey[key]; ok {
		return c
	}
	c := &solarman.Client{
		Address:    key,
		DeviceSN:   sn,
		IdleWindow: defaultIdleWindow,
		Timeout:    timeout,
		MaxTotal:   maxExchangeTotal,
	}
	clientsByKey[key] = c
	return c
}

// asciiFromRegisters собирает ASCII-строку из регистров (2 байта/регистр,
// старший байт первый — как в строках SN-областей Deye и Sofar). Не-печатные
// байты (напр. регистр длины 0x2000 у Sofar) пропускаются.
func asciiFromRegisters(vals []uint16) string {
	var b strings.Builder
	for _, v := range vals {
		hi := byte(v >> 8)
		lo := byte(v & 0xFF)
		if hi >= 0x20 && hi <= 0x7E {
			b.WriteByte(hi)
		}
		if lo >= 0x20 && lo <= 0x7E {
			b.WriteByte(lo)
		}
	}
	if b.Len() == 0 {
		return ""
	}
	return b.String()
}

// (Суффикс версий Sofar обрабатывается normalizeInverterSN — одна реализация.)

// trimSofarVersions отрезает от HW-строки Sofar хвост-версии — единая реализация
// с нормализацией серийника на выходе пулера (`normalizeInverterSN`), чтобы правило
// обрезки жило в одном месте.
func trimSofarVersions(s string) string { return normalizeInverterSN(sofarBrand.name, s) }

func pollDevice(ctx context.Context, t invTarget) DeviceResult {
	res := DeviceResult{OK: true}
	client := clientFor(t.IP, t.LoggerSN)
	// Sofar LSW-3 шлёт кадры с паузами до ~6.5 с (pacing) — окно тишины шире.
	if t.Kind == kindSofar {
		client.IdleWindow = sofarIdleWindow
	}

	var frames []solarman.Frame

	switch t.Kind {
	case kindDeyeString:
		result := map[uint16]uint16{}
		// Читаем ТОЛЬКО диапазон 0x3C–0x74 (ядро контракта). Второй диапазон
		// 0xC6–0xD2 (energy_load/sold/bought) не входит в commonContractTags и
		// отбрасывался MarshalJSON — чтение лишь замедляло опрос.
		// 0x3B — Running Status (0=standby,1=self-check,2=normal,3=FAULT): включаем
		// в блок для калибровки аварий (сопоставление Alert-битов со статусом FAULT).
		const deyeStart, deyeCount uint16 = 0x3B, 0x3A
		pdus, fr, err := client.ReadRegistersDeye(ctx, deyeStart, deyeCount, 1)
		if err != nil {
			// Сеть/логгер недоступны: это offline, а не «heartbeat без данных»
			// (раньше res.OK оставался true и устройство выглядело онлайн).
			res.OK = false
			return res
		}
		frames = append(frames, fr...)
		for _, p := range pdus {
			if p.CRC != p.CRCCalc {
				continue
			}
			for k := 0; k < len(p.Values); k++ {
				result[deyeStart+uint16(k)] = p.Values[k]
			}
		}
		if len(result) > 0 {
			// Alert-кандидаты: 1-фазный hybrid (0x65-0x6A) и 3-фазный (0x229-0x22E).
			// Значения логируем сырыми для калибровки (имена битов неизвестны).
			// Дополнительное чтение 0x229-0x22E нужно ТОЛЬКО для сырого лога
			// (калибровка аварийных битов). При выключенном логе не делаем лишнюю
			// Modbus-транзакцию на каждом опросе каждого Deye.
			if deyeRawAlertLog {
				alertB := ""
				if pdus2, _, err2 := client.ReadRegistersDeye(ctx, 0x0229, 0x0006, 1); err2 == nil {
					m2 := map[uint16]uint16{}
					for _, p := range pdus2 {
						if p.CRC != p.CRCCalc {
							continue
						}
						for k := 0; k < len(p.Values); k++ {
							m2[0x0229+uint16(k)] = p.Values[k]
						}
					}
					alertB = deyeRegsHex(m2, 0x0229, 6)
				}
				logDeyeRawAlert(t.IP, int(result[0x3B]), deyeRegsHex(result, 0x65, 6), alertB,
					strings.Join(decodeDeyeWarnings(result), ", "))
			}
			res.Faults = decodeDeyeFaults(result)
			res.Values = mapDeyeRegisters(result)
			// Ядро (ac_active_power, рег. 0x56/0x57) обязано быть: без него values после
			// фильтра commonContractTags пуст, а снимок со свежим timestamp показывает
			// инвертор «онлайн, но пустой» (не offline).
			if _, ok := res.Values["ac_active_power"]; ok {
				res.HasData = true
			}
		}
		// Серийный номер инвертора Deye — ASCII-строка в регистрах 0x0003-0x0007
		// (10 цифр; проверено на живых инверторах, напр. "##########").
		// Если серийник уже известен (кэш в runInverterPoll) — не читаем заново:
		// он постоянен, а чтение с 3 ретраями лишнее на каждый опрос.
		if t.InverterSN != "" {
			res.InverterSN = t.InverterSN
		} else {
			for attempt := 1; attempt <= 3 && res.InverterSN == ""; attempt++ {
				if ctx.Err() != nil {
					return res
				}
				pdus, _, err := client.ReadRegistersDeye(ctx, 0x0003, 0x0005, 1)
				if err != nil {
					log.Printf("%s: serial func03 read err (attempt %d): %v", t.IP, attempt, err)
					continue
				}
				for _, p := range pdus {
					if p.CRC != p.CRCCalc {
						continue
					}
					if sn := asciiFromRegisters(p.Values); sn != "" {
						res.InverterSN = sn
						break
					}
				}
			}
		}

	case kindSofar:
		// Sofar LSW-3/.76 отвечает данными ТОЛЬКО на кадр с 15-байтным datafield
		// (как Deye) и реальным SN логгера, а не на 12-байтный BuildReadFrame
		// (на него отдаёт только heartbeat с кодом 0x05). Проверено 2026-09-07:
		// на live .76 кадр 15b+SN даёт полный блок 0x0000-0x0027 (40 reg, func 03).
		pdus, fr, err := client.ReadRegistersDeye(ctx, 0x0000, 0x0028, 1)
		if err != nil {
			res.OK = false
			return res
		}
		frames = fr
		// Может прийти как полный блок (bytecount 80), так и «дубль» (16 reg)
		// и сбойные кадры — берём самую большую валидную PDU, которая выглядит
		// как НАСТОЯЩИЙ полный блок (см. probableSofarBlock).
		var bestResult map[uint16]uint16
		bestLen := 0
		for i := range pdus {
			p := pdus[i]
			if p.CRC != p.CRCCalc {
				continue
			}
			result := make(map[uint16]uint16, len(p.Values))
			for k := 0; k < len(p.Values); k++ {
				result[uint16(k)] = p.Values[k]
			}
			if !probableSofarBlock(result) {
				continue
			}
			if len(p.Values) > bestLen {
				bestLen = len(p.Values)
				bestResult = result
			}
		}
		result := bestResult
		if len(result) > 0 {
			res.HasData = true
			res.Values = mapSofarRegisters(result)
			res.Faults = decodeSofarFaults(result)
		}
		// Серийный номер инвертора Sofar — ASCII-строка HW-диапазона 0x2000-0x200D
		// (func 04; первые 2 байта = длина строки, затем ASCII на 2 байта/регистр).
		// Может быть НЕ числом (строка), напр. .76 = "SA3ES127LC1055V480V100V480".
		// Если серийник уже известен (кэш в runInverterPoll) — не читаем заново:
		// он постоянен, а чтение недоступного HW-диапазона с 3 ретраями занимает
		// до ~3 мин и на каждый опрос ломает цикл.
		if t.InverterSN != "" {
			res.InverterSN = t.InverterSN
		} else {
			for attempt := 1; attempt <= 3 && res.InverterSN == ""; attempt++ {
				if ctx.Err() != nil {
					return res
				}
				hwpdus, _, herr := client.ReadRegistersDeyeFn(ctx, 0x2000, 0x000E, 1, 0x04)
				if herr != nil {
					log.Printf("%s: serial func04 read err (attempt %d): %v", t.IP, attempt, herr)
					continue
				}
				for _, p := range hwpdus {
					if p.CRC != p.CRCCalc {
						continue
					}
					// p.Values[0] = 0x2000 — регистр ДЛИНЫ строки; явно пропускаем
					// (при длине ≥0x20 его lo-байт печатный и «подмешивался» в начало SN).
					if sn := asciiFromRegisters(p.Values[1:]); sn != "" {
						res.InverterSN = trimSofarVersions(sn)
						break
					}
				}
			}
		}

	default:
		log.Printf("%s: неизвестный kind %s — опрос пропущен", t.IP, t.Kind)
	}

	for _, f := range frames {
		// Код ошибки heartbeat (0x05 адрес устройства, 0x06 SN логгера и т.п.) —
		// в проде эти коды раньше глушились (логгировались только в probe), что
		// маскировало неверный SN/адрес вечным heartbeat_only. Выносим код в
		// DeviceResult: runInverterPoll логирует его редуцированно (с дедупликацией).
		if code, ok := solarman.DeyeErrorCode(f); ok && code != 0 {
			res.ErrCode = code
		}
		if f.DeviceSN != 0 {
			res.DeviceSN = strconv.FormatUint(uint64(f.DeviceSN), 10)
			break
		}
	}

	return res
}

// runMapPoll — отдельный 1-секундный цикл опроса МАП (kindMAP) и
// MPPT-контроллеров через API «Малина26» и записи в Redis каждого снятого
// показания (Score = секунда снятия; см. saveMapSnapshot). MPPT не
// регистрируются в конфиге вовсе (см. pollAndSaveMap).
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
	// МАП через API «Малина26»: цель kindMAP с ключом "map".
	if malina26 != nil {
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
	targets, dbCfg, meterSec, mapSec, relaySec, ce308Cfg, dashPort, err = loadConfig(cfgPath)
	if err != nil {
		log.Fatal(err)
	}

	// Адреса БД и порт дашборда берутся из sunReceiver.json (раздел db / dashboard_port).
	redisAddr := defaultRedisAddr(dbCfg)
	pgDSN := defaultPGDSN(dbCfg)
	restoreWindow := defaultPGRestoreWindow(dbCfg)
	dashboardAddr := defaultDashboardAddr(dashPort)

	log.Printf("poller started: version=%s config=%s targets=%v period=%s", version, cfgPath, targets, pollPeriod)

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
		serveDashboard(dashboardAddr, store, pg, relayCtl, stopCtx, dash, placementOrder(targets), placeByIP(targets), inverterNameByIP(targets), dashboardAuthUser, dashboardAuthPass)
	}()

	// Windows-сборка сворачивается в трей (меню «Закрыть»); на POSIX (Linux)
	// останавливается по SIGINT/SIGTERM. Единая точка — канал quit.
	quit := make(chan struct{}, 1)
	runTray(quit)

	// Каждый инвертор (Deye/Sofar) опрашивается в СВОЁМ независимом цикле с
	// периодом pollPeriod (10 с). Завис/таймаутит один текущий опрос одного
	// инвертора — остальные продолжают опрашиваться и сохранять снимки строго
	// раз в 10 секунд, влияния друг на друга нет вовсе. Снятие делается по
	// отмене stopCtx.
	for _, t := range targets {
		bgWg.Add(1)
		go func(t invTarget) {
			defer bgWg.Done()
			runInverterPoll(store, pg, t, stopCtx)
		}(t)
	}

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

// runInverterPoll — непрерывный цикл опроса ОДНОГО инвертора (Deye/Sofar) с
// периодом pollPeriod. Каждая итерация:
//   - ждёт тик time.Ticker(pollPeriod); тики коалесцируются (не накапливаются),
//     поэтому если pollDevice длиннее pollPeriod, фактический период =
//     ceil(pollDur/pollPeriod)*pollPeriod (для Sofar с pacing ~90-100 с, а не 10 с);
//   - опрашивает инвертор (pollDevice) и, если данные получены, пишет снимок в
//     Redis СРАЗУ, с фактическим временем получения.
//
// Так как у каждого инвертора свой таймер и своя горутина, зависший/таймаутящий
// инвертор никак не влияет на периодичность и запись других. Клиент solarman на
// этот IP используется только из этой горутины (инвариант «один IP — один
// опрос», см. clientsByKey). Останавливается по отмене ctx (stop): незавершённый
// опрос (pollDevice) прерывается сразу, а не до конца таймаута медленного логгера.
func runInverterPoll(store *redisStore, pg *pgStore, t invTarget, stop context.Context) {
	ticker := time.NewTicker(pollPeriod)
	defer ticker.Stop()
	var lastErrCode byte
	var lastErrAt time.Time
	lastErrCodeLog := ""            // история ошибок: код, записанный в прошлом чтении (пусто — ошибки не было)
	lastFaults := map[string]bool{} // активные аварии устройства (из регистров), для истории
	// Подавление шума «неверный Modbus-адрес» (ErrCode 0x05): пишем в историю только
	// если держится непрерывно ≥ inverterModbusErrPersist без восстановления.
	var modbusErrSince time.Time
	var modbusErrLogged bool
	for {
		select {
		case <-ticker.C:
			if stop.Err() != nil {
				return
			}
			t0 := time.Now()
			res := pollDevice(stop, t)
			// Стоп пришёл посреди опроса (ctx отменён, соединения логгеров
			// закрыты): не сохраняем частичный/устаревший снимок при остановке.
			if stop.Err() != nil {
				return
			}
			now := time.Now() // фактическое время получения данных этого инвертора
			if res.ErrCode != 0 {
				// Код ошибки heartbeat — редуцированно: при смене кода или не чаще
				// 1 раза в минуту на устройство, чтобы не спамить лог на каждые 10 с.
				if res.ErrCode != lastErrCode || now.Sub(lastErrAt) > time.Minute {
					log.Printf("%s: heartbeat код ошибки 0x%02X (%s)", t.IP, res.ErrCode, deyeErrCodeName(res.ErrCode))
					lastErrCode = res.ErrCode
					lastErrAt = now
				}
			} else {
				lastErrCode = 0
			}
			log.Printf("%s: %s (%s)", t.IP, describeResult(res), now.Sub(t0).Round(time.Millisecond))
			if !res.OK || !res.HasData {
				// heartbeat_only / no data / ошибка — снимок не сохраняем
				code, desc := "no_data", "нет данных"
				if res.ErrCode != 0 {
					code = fmt.Sprintf("0x%02X", res.ErrCode)
					desc = deyeErrCodeName(res.ErrCode)
				}
				// Правило истории: пишем только если этой ошибки НЕ было в прошлом
				// чтении (сравнение по коду), дубли на каждом чтении не создаются.
				// «Нет данных» (no_data) — не авария устройства, в историю не пишем.
				// «Неверный Modbus-адрес» (0x05) — шум: пишем только при удержании
				// ≥10 мин без восстановления.
				if res.ErrCode == 0x05 {
					if modbusErrSince.IsZero() {
						modbusErrSince = now
					}
					if pg != nil && !modbusErrLogged && now.Sub(modbusErrSince) >= inverterModbusErrPersist {
						if e := pg.InsertDeviceError(t.IP, "inverter", code, desc, now); e != nil {
							log.Printf("error pg %s: %v", t.IP, e)
						}
						modbusErrLogged = true
					}
				} else if pg != nil && code != lastErrCodeLog && code != "no_data" {
					if e := pg.InsertDeviceError(t.IP, "inverter", code, desc, now); e != nil {
						log.Printf("error pg %s: %v", t.IP, e)
					}
				}
				lastErrCodeLog = code
				continue
			}
			lastErrCodeLog = ""
			modbusErrSince = time.Time{}
			modbusErrLogged = false
			// Аварии устройства (Sofar fault-регистры и т.п.): пишем только появление.
			if pg != nil {
				cur := map[string]bool{}
				for _, f := range res.Faults {
					cur[f] = true
				}
				for f := range cur {
					if !lastFaults[f] {
						if e := pg.InsertDeviceError(t.IP, "inverter", f, f, now); e != nil {
							log.Printf("error pg %s: %v", t.IP, e)
						}
					}
				}
				lastFaults = cur
			}
			// Нормализуем аппаратный серийник (Sofar иногда отдаёт хвостовой
			// суффикс версии) — иначе один аппарат распадается на два ключа.
			res.InverterSN = normalizeInverterSN(invKindName(t.Kind), res.InverterSN)
			// Аппаратные серийные номера постоянны: если в этом цикле не удалось их
			// прочитать (инвертор выключился на закате, регистры/диапазон HW не
			// отдались), берём из предыдущего снимка — иначе номер «пропадает» в
			// таблице у оффлайн-инвертора.
			liveInverterSN := res.InverterSN // прочитан ли серийник ЖИВЬЁМ в этом цикле
			if res.InverterSN == "" || res.DeviceSN == "" {
				if b := invBrandByKind(invKindName(t.Kind)); b != nil {
					if prev, err := store.invSnapshotByIP(*b, t.IP); err == nil {
						if res.InverterSN == "" {
							res.InverterSN = normalizeInverterSN(b.name, prev.InverterSN)
						}
						if res.DeviceSN == "" {
							res.DeviceSN = prev.DeviceSN
						}
					}
				}
			}
			// Кэшируем серийник инвертора на время жизни пулера ТОЛЬКО если он прочитан
			// живьём: значение из fallback (прошлый снимок) кэшировать нельзя — при
			// замене железа на том же IP старый серийник «залипнет» и склеит устройства.
			if liveInverterSN != "" {
				t.InverterSN = liveInverterSN
			}
			snap := deviceSnapshot{
				Name:       t.Name,
				IP:         t.IP,
				Timestamp:  now.Format(time.RFC3339),
				DeviceSN:   res.DeviceSN,
				InverterSN: res.InverterSN,
				Order:      t.Order,
				Placement:  t.Placement,
				Kind:       invKindName(t.Kind),
				Values:     res.Values,
			}
			if err := store.SaveSnapshot(snap, now); err != nil {
				log.Printf("redis save %s: %v", t.IP, err)
				continue
			}
			log.Printf("saved %s: %s", t.Name, t.IP)
		case <-stop.Done():
			return
		}
	}
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
// sofarFaultBits — битовая маска аварий Sofar (регистры 0x0001–0x0005, func 03).
var sofarFaultBits = []struct {
	bit uint16
	id  string
}{
	{1, "ID01 Grid OV"}, {2, "ID02 Grid UV"}, {4, "ID03 Grid OF"}, {8, "ID04 Grid UF"},
	{16, "ID05 PV UV"}, {32, "ID06 LVRT"}, {256, "ID09 PV OV"}, {512, "ID10 PV current unbalanced"},
	{1024, "ID11"}, {2048, "ID12 GFCI"}, {4096, "ID13 phase sequence"}, {8192, "ID14 boost OC"},
	{16384, "ID15 AC OC"}, {32768, "ID16 grid current high"},
}

// decodeSofarFaults декодирует аварии Sofar из сырых регистров (0x0001–0x0005).
// Возвращает список ID активных аварий; сырые регистры в телеметрию не попадают.
func decodeSofarFaults(regs map[uint16]uint16) []string {
	var out []string
	for off := uint16(0x0001); off <= 0x0005; off++ {
		v := regs[off]
		if v == 0 {
			continue
		}
		for _, fb := range sofarFaultBits {
			if v&fb.bit != 0 {
				out = append(out, fb.id)
			}
		}
	}
	return out
}

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

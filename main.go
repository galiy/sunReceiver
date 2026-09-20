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
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/galiy/sunReceiver/modbusmap"
	"github.com/galiy/sunReceiver/solarman"
)

const (
	port       = "8899"
	pollPeriod = 10 * time.Second
	timeout    = 15 * time.Second
	// defaultIdleWindow — «тишина» между байтами ответа (Deye): паузы < 4 с.
	defaultIdleWindow = 4 * time.Second
	// sofarIdleWindow — Sofar LSW-3 шлёт куски с паузами до ~6.5 с — окно шире.
	sofarIdleWindow = 8 * time.Second
	// maxExchangeTotal — общий лимит приёма ОДНОГО ответа Solarman: живые ответы
	// идут до ~30 с (Sofar pacing), Deye ~8 с; «зомби»-логгер без лимита держал
	// бы Exchange часами.
	maxExchangeTotal = 60 * time.Second
	// mpptEmptyTolerance — сколько подряд идущих ПУСТЫХ (len(arr)==0) успешных
	// ответов read_json.php?device=mppt терпим ДО прунинга составa MPPT из current.
	// Одиночная/короткая пустота (перезапуск Малины, транзиентный сбой) не должна
	// сносить все MPPT-ключи дашборда; пруним только при устойчивой пустоте.
	mpptEmptyTolerance = 3
)

type targetKind int

const (
	kindSofar targetKind = iota
	kindDeyeString
	kindMAP
	kindMPPT // MPPT-контроллер (КЭС) через веб-API read_json.php?device=mppt ПАК «Малина»
)

// mpptOrderBase — базовый порядок для MPPT-контроллеров (динамических, из API ПАК
// «Малина»): они всегда размещаются на дашборде последними, после всех статических
// устройств из конфига (инверторы + МАП). Order = mpptOrderBase + slot.
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
// Unit — Modbus-адрес устройства для МАП (kindMAP), по умолчанию 1.
// Slot — для kindMAP: индекс MPPT-контроллера (0..15), -1 = агрегат/база батареи-сети;
//
//	для kindMPPT: индекс контроллера (слот) в ответе read_json.php?device=mppt.
type invTarget struct {
	IP       string
	Name     string
	LoggerSN uint32
	Kind     targetKind
	Unit     byte
	Slot     int
	UID      string // для kindMPPT: UID контроллера из read_json.php (стабильный идентификатор)
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

// mapRS485Section — настройка МАП Титанатор по Modbus TCP/RS485 внутри раздела "map".
// Не входит в invertors: это устройство Modbus TCP (RS485), а не инвертор.
// Disabled — ОБЯЗАТЕЛЬНОЕ поле (отсутствие = ошибка конфига): false = МАП
// опрашивается через Modbus TCP (как раньше); true = пулер по Modbus НЕ запускается,
// а все параметры МАП (батарея/сеть) берутся из веб-API ПАК «Малина»
// read_json.php?device=map.
type mapRS485Section struct {
	Name     string `json:"name"`
	IP       string `json:"ip"`
	Unit     int    `json:"unit,omitempty"`
	Disabled *bool  `json:"disabled"`
}

// dbConfig — расположение баз данных. Задаётся в sunReceiver.json в разделе "db".
// Пароль указывается прямо в pg-DSN (sunReceiver.json — приватный конфиг, в git не
// коммитится).
type dbConfig struct {
	Redis           string `json:"redis"`             // адрес Redis в формате host:port
	PG              string `json:"pg"`                // DSN PostgreSQL (с паролем)
	PGRestoreWindow string `json:"pg_restore_window"` // окно РЕСТАВРАЦИИ Redis из PG (duration-строка, напр. "720h"); пусто — дефолт 30 суток
}

// meterSection — конфигурация электросчётчика DDS238, заданная в sunReceiver.json
// разделом "meter" (обратная совместимость — отдельный dds238.json).
// Disabled — ОБЯЗАТЕЛЬНОЕ поле (отсутствие = ошибка конфига): false = счётчик
// опрашивается; true = все пулеры счётчика отключены, плашки/кнопка «Электроэнергия»
// на дашборде скрыты.
type meterSection struct {
	Name        string `json:"name"`
	IP          string `json:"ip"`
	Port        int    `json:"port"`
	Unit        byte   `json:"unit"`
	FirstReg    uint16 `json:"first_reg"`
	RegisterCnt uint16 `json:"register_count"`
	Disabled    *bool  `json:"disabled"`
}

// mapSection — раздел "map" sunReceiver.json: настройка МАП Титанатор (вложенный
// подраздел rs485 — Modbus TCP/RS485) и веб-API ПАК «Малина» для мониторинга
// MPPT-контроллеров и МАП (через read_json.php). Пароль хранится в открытом виде
// (sunReceiver.json — приватный, в git не выгружается).
// Disabled — ОБЯЗАТЕЛЬНОЕ поле (отсутствие = ошибка конфига): true отключает ВСЕ
// пулеры раздела map (МАП Modbus/веб-API, MPPT-контроллеры и BMS); на пулеры
// сетевых инверторов не влияет. BMSDisabled — ОБЯЗАТЕЛЬНОЕ поле: true отключает
// только пулер ANT BMS (плашки-батарейки на дашборде скрываются).
type mapSection struct {
	RS485       *mapRS485Section `json:"rs485"`
	BaseURL     string           `json:"base_url"`
	MPPTPath    string           `json:"mppt_path"` // путь к read_json.php?device=mppt (КЭС/MPPT-контроллеры)
	MapPath     string           `json:"map_path"`  // путь к read_json.php?device=map (МАП, батарея/сеть); пусто = выводится из mppt_path
	BMSPath     string           `json:"bms_path"`  // путь к read_bms.php (ANT BMS); пусто — BMS не опрашивается
	Login       string           `json:"login"`
	Password    string           `json:"password"`
	Disabled    *bool            `json:"disabled"`
	BMSDisabled *bool            `json:"bms_disabled"`
}

type configFile struct {
	Invertors     []configInverter `json:"invertors"`
	Map           *mapSection      `json:"map"`
	DB            *dbConfig        `json:"db"`
	Meter         *meterSection    `json:"meter"`
	Notify        *notifySection   `json:"notify"`
	Relay         *relaySection    `json:"relay"` // сетевое реле SR-201 (лампы), управление по UDP
	DashboardPort int              `json:"dashboard_port"` // порт веб-дашборда; 0 — дефолт 8080
	// Необязательные учётные данные HTTP Basic для `/api/*` веб-дашборда. Если оба
	// пусты — API открыт (обратный прокси закрывает доступ снаружи сам).
	DashboardUser     string `json:"dashboard_user,omitempty"`
	DashboardPassword string `json:"dashboard_password,omitempty"`
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
// (инверторы + МАП, без отключённых), настройки БД/счётчика/МАП-веб-API и порт дашборда.
func loadConfig(path string) ([]invTarget, *dbConfig, *meterSection, *mapSection, *relaySection, int, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, nil, nil, nil, 0, fmt.Errorf("read config %s: %w", path, err)
	}
	var cf configFile
	if err := json.Unmarshal(b, &cf); err != nil {
		return nil, nil, nil, nil, nil, 0, fmt.Errorf("parse config %s: %w", path, err)
	}
	targets := make([]invTarget, 0, len(cf.Invertors)+1)
	nextOrder := 0 // порядок устройства на дашборде = позиция в конфиге (в порядке invertors, затем map)

	// Инверторы (Deye/Sofar) из invertors; отключённые (disabled=true) пропускаются.
	for _, t := range cf.Invertors {
		if t.Disabled == nil {
			return nil, nil, nil, nil, nil, 0, fmt.Errorf("config %s: для %s (%s) не задано обязательное поле disabled (false/true)", path, t.Name, t.IP)
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
		case "map", "mppt":
			// map/mppt вынесены в отдельные разделы; записи в invertors игнорируются.
			log.Printf("config: тип %q для %s не ожидается в invertors (используйте раздел map/mppt) — пропущен", t.Type, t.IP)
			continue
		default:
			return nil, nil, nil, nil, nil, 0, fmt.Errorf("config %s: неизвестный тип %q для %s", path, t.Type, t.IP)
		}
		if t.IP == "" {
			return nil, nil, nil, nil, nil, 0, fmt.Errorf("config %s: пустой ip (type=%s)", path, t.Type)
		}
		if t.Name == "" {
			return nil, nil, nil, nil, nil, 0, fmt.Errorf("config %s: пустое имя name для %s", path, t.IP)
		}
		// Deye/Sofar: без серийного номера логгера (logger_sn=0) логгер отвечает
		// кодом 0x06 (heartbeat_only) — данные получать невозможно. Ловим при
		// старте (fatal), а не маскируем вечным heartbeat_only с логами на poll.
		if (kind == kindDeyeString || kind == kindSofar) && t.LoggerSN == 0 {
			return nil, nil, nil, nil, nil, 0, fmt.Errorf("config %s: %s (%s): не задан logger_sn — без SN логгера логгер отвечает кодом 0x06 и данные получать невозможно", path, t.Name, t.IP)
		}
		placement := t.Placement
		if placement == "" {
			placement = "Дом"
		}
		targets = append(targets, invTarget{IP: t.IP, Name: t.Name, LoggerSN: t.LoggerSN, Kind: kind, Unit: 1, Slot: -1, Order: nextOrder, Placement: placement})
		nextOrder++
	}

	// МАП (батарея/сеть) — вложенный блок "rs485" раздела "map". Disabled обязателен:
	// false — Modbus TCP (RS485), true — данные берутся из веб-API ПАК «Малина»
	// (mapAPI). При true цель в targets не добавляется (Modbus-пулер не запускается).
	// Обязательные флаги раздела map: верхнеуровневый disabled (отключает ВСЕ пулеры
	// map/mppt/bms) и bms_disabled (отключает только пулер ANT BMS).
	if cf.Map != nil {
		if cf.Map.Disabled == nil {
			return nil, nil, nil, nil, nil, 0, fmt.Errorf("config %s: в разделе map не задано обязательное поле disabled (false/true)", path)
		}
		if cf.Map.BMSDisabled == nil {
			return nil, nil, nil, nil, nil, 0, fmt.Errorf("config %s: в разделе map не задано обязательное поле bms_disabled (false/true)", path)
		}
		if *cf.Map.Disabled {
			log.Printf("config: map disabled=true — пулеры МАП, MPPT и BMS не запускаются")
		}
	}
	if cf.Meter != nil && cf.Meter.Disabled == nil {
		return nil, nil, nil, nil, nil, 0, fmt.Errorf("config %s: в разделе meter не задано обязательное поле disabled (false/true)", path)
	}
	// Сетевое реле SR-201 (лампы) — раздел "relay". Disabled обязателен: true
	// отключает контроллер ламп; false — запускает управление по UDP. IP обязателен
	// только при активном контроллере.
	if cf.Relay != nil {
		if cf.Relay.Disabled == nil {
			return nil, nil, nil, nil, nil, 0, fmt.Errorf("config %s: в разделе relay не задано обязательное поле disabled (false/true)", path)
		}
		if !*cf.Relay.Disabled && cf.Relay.IP == "" {
			return nil, nil, nil, nil, nil, 0, fmt.Errorf("config %s: в разделе relay не задан ip устройства relay-sr201-2l", path)
		}
	}
	if cf.Map != nil && cf.Map.RS485 != nil && !*cf.Map.Disabled {
		rs485 := cf.Map.RS485
		if rs485.Disabled == nil {
			return nil, nil, nil, nil, nil, 0, fmt.Errorf("config %s: в подразделе map.rs485 не задано обязательное поле disabled (false/true)", path)
		}
		if rs485.IP == "" {
			return nil, nil, nil, nil, nil, 0, fmt.Errorf("config %s: пустой ip в подразделе map.rs485", path)
		}
		name := rs485.Name
		if name == "" {
			name = "MAP (батарея/сеть)"
		}
		if *rs485.Disabled {
			log.Printf("config: МАП (%s) disabled=true — опрашивается через веб-API ПАК «Малина», а не через Modbus", name)
			mapAPI = &mapAPISource{name: name, ip: rs485.IP, order: nextOrder}
		} else {
			unit := byte(1)
			if rs485.Unit > 0 {
				unit = byte(rs485.Unit)
			}
			targets = append(targets, invTarget{IP: rs485.IP, Name: name, Kind: kindMAP, Unit: unit, Slot: -1, Order: nextOrder})
			nextOrder++
		}
	}

	if len(targets) == 0 && mapAPI == nil {
		// Если активными остались только MPPT-контроллеры из API ПАК «Малина»,
		// targets может быть пуст — это допустимо: цели собираются динамически.
		mpptOk := cf.Map != nil && cf.Map.BaseURL != "" && cf.Map.MPPTPath != "" &&
			cf.Map.Login != "" && cf.Map.Password != ""
		if !mpptOk {
			return nil, nil, nil, nil, nil, 0, fmt.Errorf("config %s: нет ни одного активного устройства", path)
		}
	}
	// Порт веб-дашборда — обязательное поле dashboard_port.
	if cf.DashboardPort == 0 {
		return nil, nil, nil, nil, nil, 0, fmt.Errorf("config %s: не задано обязательное поле dashboard_port (порт веб-дашборда)", path)
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
			return nil, nil, nil, nil, nil, 0, fmt.Errorf("config %s: в разделе notify не задан token", path)
		}
	}
	return targets, cf.DB, cf.Meter, cf.Map, cf.Relay, cf.DashboardPort, nil
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

// mpptPollState — межцикловое состояние пулера MPPT-контроллеров через веб-API
// ПАК «Малина» (сохраняется между вызовами pollAndSaveMap, которые идут раз в
// секунду из runMapPoll; доступ к нему только из одной горутины). Защищает от двух
// краевых эффектов ответов read_json.php?device=mppt:
//   - счётчик подряд идущих ПУСТЫХ (len(arr)==0) успешных ответов: до порога
//     mpptEmptyTolerance не вызываем PruneMPPT, чтобы одиночная пустота не снесла
//     все MPPT-ключи из current (аналог BMS-гварда);
//   - маппинг слот→UID: если в текущем ответе UID контроллера пуст, используем
//     последний известный UID этого слота — ключ devKey (и имя) не «дрейфует»
//     между опросами из-за пустого UID (сам UID — стабильный серийник контроллера).
type mpptPollState struct {
	slots map[int]string // слот → последний известный UID контроллера (стабильный ключ)
	empty int            // подряд идущих пустых успешных ответов MPPT API
}

func newMPPTPollState() *mpptPollState {
	return &mpptPollState{slots: map[int]string{}}
}

// resolveUID возвращает UID для слота: переданный (если непустой) — запоминает его;
// иначе — последний известный для слота (стабильный ключ при пустом UID), либо "".
func (s *mpptPollState) resolveUID(slot int, uid string) string {
	if uid != "" {
		s.slots[slot] = uid
		return uid
	}
	return s.slots[slot]
}

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

// devKey возвращает ключ устройства в хранилище (поле IP снимка): для обычных
// инверторов это IP; для kindMAP с slot и для kindMPPT — IP с суффиксом контроллера
// (например, 192.168.13.74#mppt0 / 192.168.13.60#mppt-1097), чтобы разные
// контроллеры одного гейта не сливались в одну колонку/ряд Redis и PG.
// Для kindMPPT ключ строится по UID контроллера (стабильный идентификатор), а не
// по индексу в массиве API: отвал контроллера с меньшим индексом сдвигает остальных
// в ответе, и индексный ключ склеил бы ряды двух разных аппаратов. Подстрока "#mppt"
// сохраняется (PruneMPPT ищет ключи по strings.Contains(ip, "#mppt")).
func devKey(t invTarget) string {
	if t.Kind == kindMPPT {
		if t.UID != "" {
			return fmt.Sprintf("%s#mppt-%s", t.IP, t.UID)
		}
		return fmt.Sprintf("%s#mppt%d", t.IP, t.Slot)
	}
	if t.Kind == kindMAP && t.Slot >= 0 {
		return fmt.Sprintf("%s#mppt%d", t.IP, t.Slot)
	}
	return t.IP
}

// version — версия сборки сервиса. Переопределяется при сборке через
// -ldflags "-X main.version=<версия>"; по умолчанию "dev" (локальная сборка).
var version = "dev"

// mapSourceIdentity возвращает devKey (IP) и логическое имя МАП, по которому
// ведётся мониторинг уведомлений. Источник МАП — RS232/Modbus (цель kindMAP) или
// веб-API ПАК «Малина» (mapAPI). При ни одного — ("", ""): мониторинг не запускается.
func mapSourceIdentity() (string, string) {
	for _, t := range targets {
		if t.Kind == kindMAP {
			return devKey(t), t.Name
		}
	}
	if mapAPI != nil {
		return mapAPI.ip, mapAPI.name
	}
	return "", ""
}

// mapSourceName возвращает строковое имя активного источника МАП (для логов).
func mapSourceName() string {
	for _, t := range targets {
		if t.Kind == kindMAP {
			return "RS232/Modbus"
		}
	}
	if mapAPI != nil {
		return "веб-API ПАК «Малина»"
	}
	return "нет источника"
}

// meterCfgIP возвращает IP счётчика для справочного напряжения ("" если не настроен).
func meterCfgIP(c *meterConfig) string {
	if c == nil {
		return ""
	}
	return c.IP
}

var targets []invTarget

// mppt — конфигурация доступа к веб-API ПАК «Малина» для мониторинга MPPT (КЭС)
// через read_json.php?device=mppt. Заполняется в main() из раздела "mppt"
// sunReceiver.json.
var mppt *mpptSite

// mapAPI — когда МАП опрашивается НЕ через Modbus, а через веб-API ПАК «Малина»
// (read_json.php?device=map). Заполняется в loadConfig, если в разделе "map"
// sunReceiver.json задано disabled=true. name/ip — логическое имя и ключ устройства
// (IP МАП из конфига, тот же devKey, что у МАП по Modbus). Если nil — МАП
// опрашивается через Modbus по-прежнему.
var mapAPI *mapAPISource

type mapAPISource struct {
	name  string
	ip    string
	order int
}

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

// valuesContract — универсальный контракт значений. В файл выводит ТОЛЬКО общие
// для обеих марок теги (commonContractTags) в фиксированном порядке; числовые значения
// тегов *voltage / *current / temperature* / energy* / *power округляются до 1 знака
// после запятой.
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
	// МАП modbus: батарея и сеть (для дашборда КЭС)
	"grid_voltage",
	"grid_power",
	"battery_voltage",
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

// needsRounding — true, если тэг относится к величинам, которые округляются до
// 1 знака после запятой (напряжение, ток, мощность, энергия, температура).
// Коэффициент мощности (meter_power_factor) НЕ округляется — входит в контракт
// с точностью с датчика.
func needsRounding(tag string) bool {
	return strings.HasSuffix(tag, "voltage") ||
		strings.HasSuffix(tag, "current") ||
		strings.HasSuffix(tag, "power") ||
		strings.Contains(tag, "energy") ||
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
	for _, key := range commonContractTags {
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
	Time       time.Time // время актуальности данных (для MPPT API — timestamp ответа)
}

// deviceSnapshot — структура снимка, сохраняемого в Redis (HASH current и
// временной ряд series). JSON-представление — контракт для дашборда и PG.
type deviceSnapshot struct {
	Name       string         `json:"name"`
	IP         string         `json:"ip"`
	Timestamp  string         `json:"timestamp"`
	DeviceSN   string         `json:"device_sn,omitempty"`
	InverterSN string         `json:"inverter_sn,omitempty"`
	Order      int            `json:"order,omitempty"`
	// Placement — размещение инвертора (группа «Мощности инверторов»); у МАП,
	// MPPT-контроллеров (КЭС) и счётчика не заполняется.
	Placement string `json:"placement,omitempty"`
	// Kind — марка/тип устройства из конфига (invTarget.Kind): "deye"|"sofar" для
	// сетевых инверторов, "kes" для MPPT-контроллеров (КЭС), "" — МАП/счётчик.
	// Заполняется пулером; используется страницей анимации для выбора спрайта.
	Kind     string         `json:"kind,omitempty"`
	Values   valuesContract `json:"values"`
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
// 1 знака (round1). Бренд-специфичные поля (диагностика Sofar: inverter_status,
// fault_*, температуры модуля/инвертора, bus_voltage, country; температуры Deye)
// в values НЕ попадают: они не входят в commonContractTags и отбрасываются при
// сериализации снимка — контракт хранит только общие теги.

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

// MAP clients — переиспользуемое TCP-соединение, но для МАП гейт отвечает
// медленно, и соединение живёт. Каждая цель (КЭС) опрашивается из одной горутины,
// поэтому сокет не гоняется между опросами.
var (
	mapClientsMu    sync.Mutex
	mapClientsByKey = map[string]*modbusmap.Client{}
)

// mapClientFor возвращает (и при первом обращении создаёт) клиент МАП-гейта
// для адреса ip:502 с Modbus-адресом unit.
func mapClientFor(ip string, unit byte) *modbusmap.Client {
	key := ip + ":" + modbusmap.DefaultPort
	mapClientsMu.Lock()
	defer mapClientsMu.Unlock()
	if c, ok := mapClientsByKey[key]; ok {
		return c
	}
	c := &modbusmap.Client{Address: key, Unit: unit}
	mapClientsByKey[key] = c
	return c
}

// mapMAPRegisters строит значения универсального контракта цели «КЭС» (МАП
// Титанатор — агрегат батареи/сети) из байт-ячеек МАП. Per-слотовые MPPT
// контроллеры больше не читаются через Modbus (они переехали на веб-API read_json);
// эта функция обслуживает единственный агрегат (slot = -1) из sunReceiver.json.
//
// Маппинг полей (ТЗ КЭС):
//   - l1_voltage = напряжение АКБ МАП _UAcc_med_VH/VL (0x405/0x406), (VH*256+VL)/10.
//     V_Bat самих контроллеров гейт МАП не отдаёт (проверено живьём 0x4D5=0),
//     поэтому источник — то же напряжение АКБ, что заряжают контроллеры.
//   - l1_current = ток АКБ _IAcc_med_A_u16_L/H (0x432/0x433), I[А]=(L+H*256)/16;
//     в режиме заряда (MODE 0x400==4) — со знаком «минус» (как в API-ветке).
//     Ток АКБ (а не токи отдельных MPPT-контроллеров 0x530+2*slot).
//   - ac_active_power = l1_voltage × l1_current (W).
//   - grid_frequency = 0 (частоты сети инвертор МППТ не отдаёт).
//   - pv1/pv2, ac_reactive_power, l2/l3 — в values не пишутся (нет данных).
func mapMAPRegisters(cells map[uint16]byte) valuesContract {
	out := valuesContract{}

	// Напряжение АКБ МАП: _UAcc_med_VH=0x405, _UAcc_med_VL=0x406, U=(VH*256+VL)/10.
	vh, ok1 := cells[0x405]
	vl, ok2 := cells[0x406]
	if !ok1 || !ok2 {
		return out
	}
	uAcc := float64(vh)*256 + float64(vl)
	if uAcc <= 0 {
		return out
	}
	uAcc /= 10
	out["l1_voltage"] = uAcc
	out["battery_voltage"] = uAcc

	// Ток АКБ: _IAcc_med_A_u16_L=0x432, _IAcc_med_A_u16_H=0x433,
	// I[А] = (L + H*256)/16. Это более точный ток батареи (заряд/разряд АКБ);
	// токи MPPT (0x530) — это токи контроллеров, а не ток самой АКБ.
	//
	// Знак: в режиме заряда (MODE=0x400 == 4) ток АКБ считаем ОТРИЦАТЕЛЬНЫМ —
	// согласовано с API-веткой mapMAPAPI (_Iacc знаковый, заряд отрицательный),
	// чтобы при переключении map.disabled (Modbus ↔ API) знак одного и того же
	// контрактного тега l1_current/ac_active_power не менялся и графики двух
	// источников были совместимы (раньше Modbus-ветка вела ток беззнаковым).
	mapMode := byte(0)
	if m, okM := cells[0x400]; okM {
		mapMode = m
	}
	var iAcc float64
	if l, okL := cells[0x432]; okL {
		if h, okH := cells[0x433]; okH {
			iAcc = float64(uint16(h)<<8|uint16(l)) / 16
		}
	}
	if mapMode == 4 {
		iAcc = -iAcc
	}

	out["l1_current"] = iAcc
	out["ac_active_power"] = uAcc * iAcc
	out["grid_frequency"] = 0.0

	// ---- Данные батареи и сети МАП (для дашборда КЭС) ----
	// Напряжение сети: _UNET=0x422; UNET=0 → нет сети, иначе U=(UNET+100).
	if un, ok := cells[0x422]; ok {
		if un == 0 {
			out["grid_voltage"] = 0.0
		} else {
			out["grid_voltage"] = float64(un) + 100
		}
	}
	// Мощность сети: _PNET (16 бит) = 0x59A(L),0x59B(H), Pnet=((H*256+L)/8)*100.
	// Знак по _PNET_Sign_P=0x587 (как в mapread.py) семантики дашборда:
	// 1 — положительная (потребляем из сети), 0 — отрицательная (продажа/отдача в сеть).
	if lo, okL := cells[0x59A]; okL {
		if hi, okH := cells[0x59B]; okH {
			pnet := (float64(hi)*256 + float64(lo)) / 8 * 100
			if sign, okS := cells[0x587]; okS && sign == 0 {
				pnet = -pnet
			}
			out["grid_power"] = pnet
		}
	}
	// Мощность батареи со знаком (семантика дашборда: заряд = отрицательная,
	// отдача в нагрузку/сеть = положительная). Источник — фирменная логика mapread.py:
	//   - режим заряда (MODE=0x400 == 4): P = −(I_АКБ × U_АКБ) (заряд — отрицательная),
	//     ток АКБ _IAcc_med = 0x432/0x433, I[А]=(L+H*256)/16;
	//   - иначе (генерация/подкачка/трансляция): P = +PLoad (отдача в нагрузку — положительная).
	//     Предпочитаем 16-битную PLoad_8 (0x59E/0x59F), но гейт чаще отдаёт её нулём —
	//     тогда берём 8-битную _PLoad_L (0x409, ×100).
	var batP float64
	if mode, okM := cells[0x400]; okM && mode == 4 {
		if l, okL := cells[0x432]; okL {
			if h, okH := cells[0x433]; okH {
				batP = -(float64(uint16(h)<<8|uint16(l)) / 16) * uAcc
			}
		}
	} else {
		if lo, okL := cells[0x59E]; okL {
			if hi, okH := cells[0x59F]; okH {
				if uv := uint16(hi)<<8 | uint16(lo); uv > 0 {
					batP = float64(uv) / 8 * 100
				}
			}
		}
		if batP == 0 {
			if raw, ok := cells[0x409]; ok {
				batP = float64(raw) * 100
			}
		}
	}
	out["battery_power"] = batP
	return out
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

// sofarVersionsRe — суффикс версий HW-строки Sofar: три подряд идущих токена
// «V<цифры>» в конце (аппаратная/программная/польз. платформы), напр.
// "…V480V100V480". Такие версии выводить в качестве серийного номера НЕ нужно.
var sofarVersionsRe = regexp.MustCompile(`V\d+V\d+V\d+$`)

// trimSofarVersions отрезает от HW-строки Sofar хвост-версии (если он есть),
// оставляя только серийный код инвертора. Напр. "SA3ES033KAQ846V480V100V480" →
// "SA3ES033KAQ846".
func trimSofarVersions(s string) string {
	loc := sofarVersionsRe.FindStringIndex(s)
	if loc == nil || loc[0] == 0 {
		return s
	}
	return s[:loc[0]]
}

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
		for _, r := range [][2]uint16{{0x3C, 0x39}, {0xC6, 0x0D}} {
			pdus, fr, err := client.ReadRegistersDeye(ctx, r[0], r[1], 1)
			if err != nil {
				continue
			}
			frames = append(frames, fr...)
			for _, p := range pdus {
				if p.CRC != p.CRCCalc {
					continue
				}
				for k := 0; k < len(p.Values); k++ {
					result[r[0]+uint16(k)] = p.Values[k]
				}
			}
		}
		if len(result) > 0 {
			res.Values = mapDeyeRegisters(result)
			// Ядро (ac_active_power, рег. 0x56/0x57 — только первый диапазон 0x3C–0x74)
			// обязано быть: без него values после фильтра commonContractTags пуст, а снимок
			// со свежим timestamp показывает инвертор «онлайн, но пустой» (не offline).
			// Второй диапазон (0xC6–0xD2: energy_load/sold/bought) не входит в контракт.
			if _, ok := res.Values["ac_active_power"]; ok {
				res.HasData = true
			}
		}
		// Серийный номер инвертора Deye — ASCII-строка в регистрах 0x0003-0x0007
		// (10 цифр; проверено на живых .70/.79/.91/.92/.93, напр. .70 = "2405018274").
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

	case kindMAP:
		// МАП Титанатор («КЭС») — Modbus TCP (порт 502), не Solarman-кадр.
		// Читаем блоки байт-ячеек: 0x400 (0x400..0x43F — режим/АКБ/сети),
		// токи MPPT (0x530-0x551) и мощности сети/батареи (0x580-0x5A3, т.ч.
		// 0x587 sign, 0x59A/0x59B PNET, 0x59E/0x59F PLOAD).
		mc := mapClientFor(t.IP, t.Unit)
		cells := map[uint16]byte{}
		// Блок 0x400 (0x20 слов = 0x400..0x43F) охватывает режим (0x400), мощности
		// (_PLoad_L 0x409), напряжения (_UNET 0x422, _INET 0x423, _PNET_L 0x424)
		// и ток АКБ (_IAcc_med 0x432/0x433). Отдельного чтения 0x420 нет — оно было
		// строгим подмножеством этого блока (ReadRegisters проверяет bytecount,
		// поэтому при успехе 0x422 уже в cells).
		if b, err := mc.ReadRegisters(ctx, 0x0400, 0x20); err == nil {
			for i := 0; i < len(b); i++ {
				cells[0x0400+uint16(i)] = b[i]
			}
		} else {
			log.Printf("%s: MAP: блок 0x0400: %v", t.IP, err)
		}
		if b, err := mc.ReadRegisters(ctx, 0x0530, 0x40); err == nil {
			for i := 0; i < len(b); i++ {
				cells[0x0530+uint16(i)] = b[i]
			}
		} else {
			log.Printf("%s: MAP: блок 0x0530: %v", t.IP, err)
		}
		if b, err := mc.ReadRegisters(ctx, 0x0580, 0x24); err == nil {
			for i := 0; i < len(b); i++ {
				cells[0x0580+uint16(i)] = b[i]
			}
		} else {
			log.Printf("%s: MAP: блок 0x0580: %v", t.IP, err)
		}
		res.Values = mapMAPRegisters(cells)
		if _, has := cells[0x405]; has && cells[0x406] > 0 {
			res.HasData = true
		}
		if len(res.Values) > 0 {
			res.HasData = true
			res.DeviceSN = fmt.Sprintf("map-%s", devKey(t))
		}
		// Фиксируем результат Modbus-опроса МАП для монитора уведомлений.
		now := time.Now()
		if res.HasData {
			// Напряжение сети МАП (grid_voltage): 0 = нет сети (см. mapMAPRegisters).
			grid, hasGrid := 0.0, false
			if v, ok := res.Values["grid_voltage"]; ok {
				grid, hasGrid = toFloat(v)
			}
			mapTracker.trackOK("modbus", now, hasGrid, grid)
		} else {
			mapTracker.trackErr("modbus", "опрос по RS232/Modbus не удался (нет данных от МАП)", now)
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

// pollMPPTFromArr строит DeviceResult для MPPT-контроллера (t.Slot) из УЖЕ
// полученного ответа ПАК «Малина» (arr = read_json.php?device=mppt). Вынесена из
// pollDevice, чтобы один HTTP-запрос FetchMPPTs переиспользовался для всех слотов
// за цикл, а не повторялся на каждый контроллер.
func pollMPPTFromArr(t invTarget, arr []mpptRaw) DeviceResult {
	res := DeviceResult{OK: true}
	if t.Slot < 0 || t.Slot >= len(arr) {
		res.OK = false
		log.Printf("%s: mppt api: слот %d вне диапазона (получено %d контроллеров)", t.IP, t.Slot, len(arr))
		return res
	}
	vals, ts, ok := mapMPPTAPI(arr[t.Slot])
	if !ok {
		res.OK = false
		log.Printf("%s: mppt api: слот %d не дал данных", t.IP, t.Slot)
		return res
	}
	res.OK = true
	res.HasData = true
	res.Values = vals
	if arr[t.Slot].UID != "" {
		res.DeviceSN = fmt.Sprintf("mppt-%s", arr[t.Slot].UID)
	}
	// Актуальность данных из API (для таймстампа снимка). mapMPPTAPI вернул нулевое
	// время при timestamp=0; здесь дополнительно отбрасываем устаревший (>5 мин) или
	// будущий timestamp — дрейф часов Малины не должен уводить снимок от now.
	if !ts.IsZero() {
		if d := time.Since(ts); d > 5*time.Minute || d < -5*time.Minute {
			ts = time.Time{}
		}
	}
	res.Time = ts
	return res
}

// runMapPoll — отдельный 1-секундный цикл опроса быстрых целей — МАП (kindMAP,
// Modbus TCP) и MPPT-контроллеров (веб-API ПАК «Малина») — и записи в Redis через
// SaveSnapshotWindow: в пределах каждого 10-секундного окна остаётся ровно одна
// (последняя) строка на устройство. МАП-цели исключены из 10-сек циклов инверторов
// (см. runInverterPoll); MPPT не регистрируются в конфиге вовсе (см. pollAndSaveMap).
func runMapPoll(store *redisStore, stop context.Context) {
	const pollEvery = time.Second
	ticker := time.NewTicker(pollEvery)
	defer ticker.Stop()
	state := newMPPTPollState()
	for {
		select {
		case <-ticker.C:
			pollAndSaveMap(stop, store, time.Now(), state)
		case <-stop.Done():
			return
		}
	}
}

// pollAndSaveMap опрашивает быстрые источники параллельно и пишет в Redis через
// SaveSnapshotWindow (одна строка за каждые 10 секунд + актуальное current):
//   - МАП (kindMAP, Modbus TCP) — цели из targets;
//   - MPPT-контроллеры — ДИНАМИЧЕСКИ: состав определяется фактически подключёнными
//     к ПАК «Малина» контроллерами из ответа read_json.php?device=mppt, а не из
//     sunReceiver.json. Поэтому MPPT появляются/исчезают с дашборда по факту наличия
//     в ответе API; исчезнувшие удаляются и из HASH current (их строка не числится
//     «актуальной».
//
// saveWindowSnapshot складывает результат опроса в deviceSnapshot (ts = res.Time
// при наличии, иначе now) и пишет в Redis через SaveSnapshotWindow. Общий для
// МАП- и MPPT-веток pollAndSaveMap.
func saveWindowSnapshot(store *redisStore, t invTarget, res DeviceResult, now time.Time) {
	if !res.OK || !res.HasData {
		log.Printf("%s: %s %s", t.IP, t.Kind, describeResult(res))
		return
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
	if err := store.SaveSnapshotWindow(snap, ts); err != nil {
		log.Printf("redis save %s: %v", devKey(t), err)
	}
}

func pollAndSaveMap(ctx context.Context, store *redisStore, now time.Time, state *mpptPollState) {
	var wg sync.WaitGroup
	activeMPPT := map[string]struct{}{}
	pruneMPPT := false // MPPT-состав чистим из current только по решению state.shouldPrune
	// МАП (батарея/сеть) — из targets (Modbus) или через веб-API ПАК «Малина».
	for i := range targets {
		t := targets[i]
		if t.Kind != kindMAP {
			continue
		}
		wg.Add(1)
		go func(t invTarget) {
			defer wg.Done()
			saveWindowSnapshot(store, t, pollDevice(ctx, t), now)
		}(t)
	}
	// МАП через веб-API (map.disabled=true): Modbus-пулер не запущен (цели kindMAP в
	// targets нет), параметры батареи/сети берём из read_json.php?device=map.
	if mapAPI != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			t := invTarget{IP: mapAPI.ip, Name: mapAPI.name, Kind: kindMAP, Slot: -1, Order: mapAPI.order}
			saveWindowSnapshot(store, t, pollMAPAPI(ctx), now)
		}()
	}
	// MPPT-контроллеры — по факту подключённых из API. Состав определяется
	// фактически подключёнными к ПАК «Малина» контроллерами (один HTTP-запрос
	// FetchMPPTs переиспользуется для всех слотов за цикл). Каждый контроллер
	// ответа — отдельное устройство с ключом devKey(MPPT слотом); имя MPPT-<n+1>.
	if mppt != nil {
		arr, err := mppt.FetchMPPTs(ctx)
		if err != nil {
			log.Printf("mppt api: %v", err)
			// При ошибке запроса НЕ чистим «исчезнувшие» контроллеры: одиночный
			// сбой (таймаут, перезапуск Малины) не должен вычистить все MPPT из
			// current. Чистим только когда API ответил, но контроллера нет в ответе.
		} else {
			for slot := range arr {
				slot := slot
				// UID — стабильный серийник контроллера; при пустом в ответе берём
				// последний известный для слота, чтобы ключ не «дрейфовал».
				uid := state.resolveUID(slot, arr[slot].UID)
				name := fmt.Sprintf("MPPT-%d", slot+1)
				if uid != "" {
					name = "MPPT-" + uid
				} else {
					// UID неизвестен и ни разу не встречался для слота — ключ по
					// индексу (деградация: отвал нижнего слота может склеить ряды).
					log.Printf("%s: mppt api: слот %d без UID — ключ по индексу", mppt.Host, slot)
				}
				t := invTarget{IP: mppt.Host, Name: name, Kind: kindMPPT, Slot: slot, UID: uid, Order: mpptOrderBase + slot}
				activeMPPT[devKey(t)] = struct{}{}
				wg.Add(1)
				go func(t invTarget) {
					defer wg.Done()
					saveWindowSnapshot(store, t, pollMPPTFromArr(t, arr), now)
				}(t)
			}
			// Прунинг — только по устойчивой пустоте (несколько пустых ответов подряд).
			pruneMPPT = state.shouldPrune(len(arr) == 0)
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
	var dashPort int
	var err error
	targets, dbCfg, meterSec, mapSec, relaySec, dashPort, err = loadConfig(cfgPath)
	if err != nil {
		log.Fatal(err)
	}

	// Адреса БД и порт дашборда берутся из sunReceiver.json (раздел db / dashboard_port).
	redisAddr := defaultRedisAddr(dbCfg)
	pgDSN := defaultPGDSN(dbCfg)
	restoreWindow := defaultPGRestoreWindow(dbCfg)
	dashboardAddr := defaultDashboardAddr(dashPort)

	log.Printf("poller started: version=%s config=%s targets=%v period=%s", version, cfgPath, targets, pollPeriod)

	// Конфигурация веб-API ПАК «Малина» для мониторинга MPPT (КЭС) — раздел "map"
	// sunReceiver.json (бывший malina.json).
	mppt = loadMPPTSite(mapSec)
	// ANT BMS (ANT BMS, web-API read_bms.php ПАК «Малина») — отдельный 1-сек цикл,
	// актуальное состояние в отдельном Redis-ключе (HASH sunreceiver:bms).
	bmsSite = loadBmsSite(mapSec)
	if bmsSite != nil {
		log.Printf("bms: опрос ANT BMS через %s (1 раз в секунду, ключ Redis %s)", bmsSite.url, redisBMSKey)
	}
	// Если МАП опрашивается через веб-API (map.rs485.disabled=true), обязателен доступ
	// к ПАК «Малина» (раздел "map") — иначе неоткуда взять параметры батареи/сети.
	if mapAPI != nil && mppt == nil {
		log.Fatalf("config: МАП настроен через веб-API (map.rs485.disabled=true), но раздел map неполный — нужны base_url, mppt_path, login, password")
	}
	// Конфигурация электросчётчика DDS238 — раздел "meter" sunReceiver.json
	// или файл dds238.json (обратная совместимость).
	meterCfg := loadMeterConfig(meterSec)
	if desc := describeMeterConfig(meterCfg); desc != "" {
		log.Printf("meter: %s", desc)
	} else {
		log.Printf("meter: не настроен (нет dds238.json рядом с бинарником) — опрос счётчика отключён")
	}

	// Флаги видимости блоков дашборда, вычисленные из конфигурации:
	//   - ShowMap — МАП/MPPT включены (map.disabled != true);
	//   - ShowMeter — счётчик реально опрашивается (не disabled и не «неполный»);
	//   - ShowBMS — пулер ANT BMS запущен (bms_disabled != true, заполнен bms_path);
	//   - ShowRelay — контроллер ламп SR-201 включен (relay.disabled != true).
	dash := dashFlags{
		ShowMap:    mapSec != nil && (mapSec.Disabled == nil || !*mapSec.Disabled),
		ShowMeter:  meterCfg != nil,
		ShowBMS:    bmsSite != nil,
		ShowRelay:  relaySec != nil && (relaySec.Disabled == nil || !*relaySec.Disabled),
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
	// Привязываем контекст долгих операций Redis к сигналу остановки, чтобы они
	// корректно прерывались при shutdown (N16).
	store.SetCtx(stopCtx)
	// bgWg — все фоновые горутины, пишущие в Redis/PG: при завершении main
	// отменяет stopCtx, ЖДЁТ их (bgWg.Wait()) и только потом defer'ы закрывают
	// пулы rdb/pg — записи при остановке (BMS-drain, averageBucket) не гоняются
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
			// Конвертируем старую сырую таблицу snapshots в 5-минутные средние.
			if merr := pg.MigrateLegacy(); merr != nil {
				log.Printf("pg legacy миграция: %v", merr)
			}
			// Если Redis пуст — восстановить в нём данные из PG. Реставрация
			// выполняется СИНХРОННО, ДО запуска runAccumulator ниже: иначе
			// backfillAccumulator мог бы прочитать наполовину восстановленный
			// Redis и перетереть полные PG-бакеты частичными (гонка restore ↔
			// backfill на пустом Redis).
			empty, cerr := store.IsEmpty()
			if cerr != nil {
				log.Printf("redis empty-check: %v", cerr)
			} else if empty {
				// IsEmpty (redis_store.go) учитывает только инверторное current +
				// серию инверторов. Данные ANT BMS лежат в ОТДЕЛЬНЫХ ключах
				// (HASH sunreceiver:bms, ряд sunreceiver:bms:series:*), в него не
				// входят. Проверяем их, чтобы сложившийся BMS-«магазин» не был
				// засчитан пустым и не перетёрся реставрацией.
				bmsPresent, berr := redisBMSDataPresent(store)
				switch {
				case berr != nil:
					log.Printf("redis empty-check (bms): %v", berr)
				case bmsPresent:
					log.Printf("redis empty-check: в Redis есть BMS-данные — реставрация не требуется")
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
						restoreRedisFromPG(store, pg, restoreWindow, stopCtx)
						store.rdb.Del(store.ctx, redisRestoreLockKey)
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
	// МАП («КЭС», Modbus TCP) и MPPT-контроллеры (веб-API ПАК «Малина»)
	// опрашиваются отдельно, 1 раз в секунду, и пишутся в Redis со специальной
	// логикой «одна строка за 10 с» (см. SaveSnapshotWindow). Быстрый 1-сек цикл
	// запускается только если есть хоть один источник МАП/MPPT: при map.disabled=true
	// (или отсутствии источников) горутина не стартует вовсе.
	hasMapSource := mapAPI != nil || mppt != nil
	if !hasMapSource {
		for _, t := range targets {
			if t.Kind == kindMAP {
				hasMapSource = true
				break
			}
		}
	}
	if hasMapSource {
		bgWg.Add(1)
		go func() {
			defer bgWg.Done()
			runMapPoll(store, stopCtx)
		}()
	} else {
		log.Printf("map/mppt: опрос отключён (map.disabled=true или нет источников МАП/MPPT)")
	}
	// Уведомления в MAX. Мониторинг МАП выполняется ТОЛЬКО если включён опрос МАП
	// в целом (есть источник МАП — RS232/Modbus или веб-API ПАК «Малина»). Если
	// опрос МАП выключен — уведомления не формируются, даже при настроенном notify.
	mapIP, mapName := mapSourceIdentity()
	if notifyCfg != nil && mapIP != "" {
		bgWg.Add(1)
		go func() {
			defer bgWg.Done()
			runNotifyMonitor(store, notifyCfg, mapIP, mapName, meterCfgIP(meterCfg), stopCtx)
		}()
		log.Printf("notify: уведомления в MAX включены (МАП %s, источник %s)", mapName, mapSourceName())
	} else if notifyCfg != nil {
		log.Printf("notify: опрос МАП выключен или нет источника МАП — уведомления в MAX не формируются")
	}
	// ANT BMS (read_bms.php) — 1 раз в секунду: актуальное состояние в отдельном
	// Redis-ключе (HASH sunreceiver:bms) + накопление 5-минутных усреднённых
	// точек в Redis (ряд, окно 2 суток) и PG (bms_averages), см. bms_poller.go
	// и bms_accumulator.go.
	if bmsSite != nil {
		bgWg.Add(1)
		go func() {
			defer bgWg.Done()
			runBmsPoll(store, pg, stopCtx)
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
		serveDashboard(dashboardAddr, store, pg, relayCtl, stopCtx, dash, placementOrder(targets), placeByIP(targets), dashboardAuthUser, dashboardAuthPass)
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
		if t.Kind == kindMAP || t.Kind == kindMPPT {
			continue // МАП и MPPT API опрашиваются отдельным 1-сек циклом (runMapPoll)
		}
		bgWg.Add(1)
		go func(t invTarget) {
			defer bgWg.Done()
			runInverterPoll(store, t, stopCtx)
		}(t)
	}

	<-waitForQuit(quit)
	log.Println("shutting down")
	// Отмена stopCtx: циклы фоновых горутин завершаются, a in-flight сетевые
	// опросы (dial/read/HTTP) прерываются немедленно — graceful-shutdown не
	// ждёт медленный логгер.
	stopCancel()
	// Ждём завершения фоновых горутин (их завершающие записи в Redis/PG:
	// BMS-drain, averageBucket), ПОСЛЕ чего defer'ы закрывают пулы — гонки
	// «запись в закрытый пул» нет.
	bgWg.Wait()
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
func runInverterPoll(store *redisStore, t invTarget, stop context.Context) {
	ticker := time.NewTicker(pollPeriod)
	defer ticker.Stop()
	var lastErrCode byte
	var lastErrAt time.Time
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
				continue
			}
			// Аппаратные серийные номера постоянны: если в этом цикле не удалось их
			// прочитать (инвертор выключился на закате, регистры/диапазон HW не
			// отдались), берём из предыдущего снимка — иначе номер «пропадает» в
			// таблице у оффлайн-инвертора.
			if res.InverterSN == "" || res.DeviceSN == "" {
				if prev, err := store.CurrentOne(t.IP); err == nil {
					if res.InverterSN == "" {
						res.InverterSN = prev.InverterSN
					}
					if res.DeviceSN == "" {
						res.DeviceSN = prev.DeviceSN
					}
				}
			}
			// Кэшируем серийник инвертора на время жизни пулера: в следующем опросе
			// pollDevice не будет перечитывать HW-диапазон с 3 ретраями.
			if res.InverterSN != "" {
				t.InverterSN = res.InverterSN
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
// (см. DeyeErrorCode в solarman/frame.go).
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

// redisBMSDataPresent — true, если в Redis есть данные ANT BMS: хотя бы одно поле
// в HASH sunreceiver:bms или хоть один месячный сегмент ряда
// sunreceiver:bms:series:*. Отдельно от IsEmpty, т.к. тот учитывает только
// инверторное current/series.
func redisBMSDataPresent(store *redisStore) (bool, error) {
	n, err := store.rdb.HLen(store.ctx, redisBMSKey).Result()
	if err != nil {
		return false, err
	}
	if n > 0 {
		return true, nil
	}
	keys, err := store.scanPrefixKeys(redisBMSSeriesPrefix + "*")
	if err != nil {
		return false, err
	}
	return len(keys) > 0, nil
}

// restoreRedisFromPG восстанавливает Redis из persistent-хранилища PostgreSQL
// за период [now-window, now], но не старше окна удержания Redis (последние 2
// календарных суток), иначе фоновая очистка сразу удалит восстановленное.
// Запускается в фоне при пустом Redis. Восстанавливаются ОБА ряда:
//   - снимки инверторов/МАП/счётчика (pg.Averages) — SaveSnapshot в месячный
//     ZSET ряда;
//   - 5-минутные усреднённые точки ANT BMS (pg.BMSAveragesAll) — SaveBMSSeries
//     в ряд sunreceiver:bms:series:<YYYY-MM>.
func restoreRedisFromPG(store *redisStore, pg *pgStore, window time.Duration, stop context.Context) {
	end := time.Now()
	start := recentCutoff(end)
	if w := end.Add(-window); w.After(start) {
		start = w
	}
	log.Printf("pg restore: восстанавливаю Redis из PG за [%s, %s]",
		start.Format(time.RFC3339), end.Format(time.RFC3339))
	snaps, err := pg.Averages(start, end)
	if err != nil {
		log.Printf("pg restore: query: %v", err)
		return
	}
	// Сортируем по ts ascending: pg.Averages не гарантирует порядок (без ORDER BY),
	// а в цикле ниже каждый SaveSnapshot кладёт точку в HASH current[ip] — остаётся
	// «последний из итерации». Сортируем, чтобы последний HSet был самым свежим
	// (детерминированный current[ip] после реставрации).
	sort.Slice(snaps, func(i, j int) bool {
		ti, _ := parseTS(snaps[i].Timestamp)
		tj, _ := parseTS(snaps[j].Timestamp)
		return ti.Before(tj)
	})
	var restored int
	for _, snap := range snaps {
		select {
		case <-stop.Done():
			log.Printf("pg restore: остановлено по сигналу (восстановлено точек: %d)", restored)
			return
		default:
		}
		ts, perr := time.Parse(time.RFC3339, snap.Timestamp)
		if perr != nil {
			continue
		}
		// Ключ месячного сегмента ряда строится по ts (redisSeriesKey = ts.Format("2006-01")).
		// Живая запись пишет по локальной зоне (ts = time.Now()), а тут ts из RFC3339
		// обычно в UTC — приводим к time.Local, чтобы попасть в тот же месяц-ключ.
		ts = ts.In(time.Local)
		if serr := store.SaveSnapshot(snap, ts); serr != nil {
			log.Printf("pg restore: save %s: %v", snap.IP, serr)
			continue
		}
		restored++
	}
	log.Printf("pg restore: завершено, восстановлено точек: %d", restored)

	// Ряд 5-минутных усреднённых точек ANT BMS — из pg.bms_averages в
	// sunreceiver:bms:series:<YYYY-MM> (то же окно удержания).
	bmsPts, err := pg.BMSAveragesAll(start, end)
	if err != nil {
		log.Printf("pg restore: bms query: %v", err)
		return
	}
	var bmsRestored int
	for _, p := range bmsPts {
		select {
		case <-stop.Done():
			log.Printf("pg restore: остановлено по сигналу (BMS восстановлено точек: %d)", bmsRestored)
			return
		default:
		}
		ts, perr := time.Parse(time.RFC3339, p.Ts)
		if perr != nil {
			continue
		}
		// См. выше: ключ месяца (bmsSeriesKey) приводим к локальной зоне, как живая запись.
		ts = ts.In(time.Local)
		if serr := store.SaveBMSSeries(p, ts); serr != nil {
			log.Printf("pg restore: bms save %s: %v", p.Name, serr)
			continue
		}
		bmsRestored++
	}
	log.Printf("pg restore: BMS восстановлено точек: %d", bmsRestored)
}

package main

import (
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"math"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// rangeCacheTTL — срок жизни кешированного набора снимков в loadRange. Страница
// графиков делает 4 fetch /api/series с одинаковыми from/to за цикл (60 с), и
// данные Redis обновляются каждые ~10 с, поэтому 15 с — свежее и даёт схождение
// всех 4 запросов в один реальный read из Redis/PG.
const rangeCacheTTL = 15 * time.Second

// dashboardHandler — веб-дашборд: отдаёт три HTML-страницы и JSON API.
//  - Главная страница (/) — текущие параметры: плашки, электросчётчик, сводная
//    таблица; обновляются каждую секунду из Redis.
//  - Страница графиков (/charts) — временные ряды инверторов, МАП и счётчика за
//    выбранный период (Redis полное разрешение за 2 суток + PG 5-минутные средние).
//  - Страница электроэнергии (/energy) — посуточные и помесячные тарифы счётчика
//    (потребление/отдача «День»/«Ночь») из daily_tariffs с независимыми диапазонами.
type dashboardHandler struct {
	store *redisStore
	pg    *pgStore

	// Кэш loadRange: 4 одинаковых запроса /api/series за цикл сойдутся в один
	// read из Redis/PG. Ключ — от (start, end).
	cacheMu sync.Mutex
	cache   map[string]cachedRange
}

// cachedRange — кешированный результат loadRange.
type cachedRange struct {
	at    time.Time
	snaps []deviceSnapshot
}

// currentResponse отвечает на GET /api/current.
type currentResponse struct {
	GeneratedAt string           `json:"generated_at"`
	TotalPower  float64          `json:"total_power"`
	TotalPV     float64          `json:"total_pv"`
	MapGridV    float64          `json:"map_grid_voltage"`
	MapGridP    float64          `json:"map_grid_power"`
	MapBatV     float64          `json:"map_battery_voltage"`
	MapBatP     float64          `json:"map_battery_power"`
	MapCons     float64          `json:"map_consumption"`
	// Расчётные тарифные величины счётчика за текущие календарные сутки (kWh):
	// потребление/отдача «День»/«Ночь», посчитанные из актуальных показаний
	// счётчика (Redis) и фиксированных граничных показаний (PG daily_tariffs).
	MeterImportDay   float64 `json:"meter_import_day"`
	MeterImportNight float64 `json:"meter_import_night"`
	MeterExportDay   float64 `json:"meter_export_day"`
	MeterExportNight float64 `json:"meter_export_night"`
	Devices          []deviceSnapshot `json:"devices"`
}

// seriesPoint — одна точка временного ряда: время + значение.
type seriesPoint struct {
	T string  `json:"t"`
	V float64 `json:"v"`
}

// deviceSeries — временной ряд ac_active_power одного инвертора.
type deviceSeries struct {
	Name   string        `json:"name"`
	IP     string        `json:"ip"`
	Color  string        `json:"color"`
	Points []seriesPoint `json:"points"`
}

// seriesResponse отвечает на GET /api/series.
type seriesResponse struct {
	GeneratedAt      string         `json:"generated_at"`
	From             string         `json:"from"`
	To               string         `json:"to"`
	Series           []deviceSeries `json:"series"`
	Total            []seriesPoint  `json:"total,omitempty"`
	MapGridVoltage   []seriesPoint  `json:"map_grid_voltage,omitempty"`
	MapGridPower     []seriesPoint  `json:"map_grid_power,omitempty"`
	MapBatVoltage    []seriesPoint  `json:"map_battery_voltage,omitempty"`
	MapBatPower      []seriesPoint  `json:"map_battery_power,omitempty"`
	MapCons          []seriesPoint  `json:"map_consumption,omitempty"`
	MeterVoltage     []seriesPoint  `json:"meter_voltage,omitempty"`
	MeterActivePower []seriesPoint  `json:"meter_active_power,omitempty"`
}

// meterDailyResponse отвечает на GET /api/tariffs: посуточные тарифные величины
// счётчика (потребление/отдача «День»/«Ночь»), отсортированные по дню возрастанию.
type meterDailyResponse struct {
	GeneratedAt string         `json:"generated_at"`
	From        string         `json:"from"`
	To          string         `json:"to"`
	Days        []meterDayStat `json:"days"`
}

// meterDayStat — тарифные величины одного календарного дня (kWh).
type meterDayStat struct {
	Day        string  `json:"day"` // YYYY-MM-DD
	ImportDay  float64 `json:"import_day"`
	ImportNight float64 `json:"import_night"`
	ExportDay  float64 `json:"export_day"`
	ExportNight float64 `json:"export_night"`
}

// seriesPalette — цвета линий инверторов (по индексу после сортировки по имени).
var seriesPalette = []string{
	"#ff6b6b", "#4ecdc4", "#45b7d1", "#f9ca24",
	"#a29bfe", "#fd79a8", "#00b894", "#e17055",
	"#74b9ff", "#55efc4", "#fdcb6e", "#fab1a0",
}

// snapFloat извлекает числовое значение из универсального контракта по ключу
// (обёртка над toFloat — единая реализация в accumulator.go).
func snapFloat(v valuesContract, key string) (float64, bool) {
	raw, ok := v[key]
	if !ok {
		return 0, false
	}
	return toFloat(raw)
}

// isMAPDevice возвращает true, если снимок принадлежит устройству МАП (kindMAP,
// батарея/сеть). Маркер — наличие тега battery_voltage, которого нет у инверторов
// (Deye/Sofar) и MPPT-контроллеров. Мощность МАП учитывается только на своих
// плашках и графиках, а не в сумме по инверторам.
func isMAPDevice(v valuesContract) bool {
	_, ok := v["battery_voltage"]
	return ok
}

// isMeterDevice возвращает true, если снимок принадлежит электросчётчику DDS238
// (маркер — наличие тега meter_voltage, которого нет у инверторов и МАП). Аналог
// JS-функции isMeterDevice; используется в apiCurrent для поиска актуальных
// показаний счётчика (Import/Export) при расчёте тарифов текущего дня.
func isMeterDevice(v valuesContract) bool {
	_, ok := v["meter_voltage"]
	return ok
}

// isMPPTKey возвращает true для ключа/IP устройства MPPT-контроллера (devKey вида
// host#mppt<slot>). Используется, чтобы MPPT-контроллеры сортировались и
// выводились после сетевых инверторов.
func isMPPTKey(ip string) bool {
	return strings.Contains(ip, "#mppt")
}

const dashboardPage = `<!DOCTYPE html>
<html lang="ru">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>SunReceiver</title>
<style>
:root { color-scheme: dark; }
* { box-sizing: border-box; }
body { font-family: -apple-system, "Segoe UI", Roboto, sans-serif; background:#0f1115; color:#e6e6e6; margin:0; padding:20px; overflow-x:hidden; }
h1 { font-size:22px; margin:0 0 4px; }
.sub { color:#8a93a1; margin:0 0 20px; font-size:13px; }
.top-nav { display:flex; align-items:center; justify-content:space-between; gap:12px; flex-wrap:wrap; margin-bottom:20px; }
.top-nav .ttl { margin:0; }
.top-nav .sub { margin:4px 0 0; }
.nav-btn { background:#2f6fed; color:#fff; border:none; border-radius:8px; padding:9px 16px; font-size:14px; font-weight:600; cursor:pointer; text-decoration:none; white-space:nowrap; }
.nav-btn:hover { background:#3f7bf0; }
.nav-btn.secondary { background:#252b36; border:1px solid #333b49; }
#chartbox { background:#181c24; border:1px solid #252b36; border-radius:10px; padding:16px; margin-bottom:20px; max-width:100%; }
#chartbox h2 { margin:0 0 8px; font-size:16px; }
.chart-toolbar { display:flex; align-items:center; flex-wrap:wrap; gap:10px 12px; margin-bottom:8px; font-size:13px; color:#8a93a1; }
.chart-toolbar button { background:#252b36; color:#e6e6e6; border:1px solid #333b49; border-radius:6px; padding:4px 10px; cursor:pointer; font-size:13px; }
.chart-toolbar button:hover { background:#2f3644; }
.period-panel { display:flex; align-items:center; flex-wrap:wrap; gap:10px; margin-bottom:16px; font-size:13px; color:#8a93a1; }
.period-panel button { background:#252b36; color:#e6e6e6; border:1px solid #333b49; border-radius:6px; padding:5px 12px; cursor:pointer; font-size:13px; }
.period-panel button:hover { background:#2f3644; }
.period-panel button.active { background:#2f6fed; border-color:#2f6fed; color:#fff; }
.period-panel input[type=date] { background:#181c24; color:#e6e6e6; border:1px solid #333b49; border-radius:6px; padding:4px 8px; font-size:13px; color-scheme:dark; }
.period-panel input[type=date]:focus { outline:none; border-color:#2f6fed; }
.period-panel .nav-arrow { padding:5px 10px; font-size:16px; line-height:1; }
.chart-wrap { position:relative; height:340px; }
.charts { display:flex; flex-wrap:wrap; gap:16px; margin-bottom:20px; }
.charts #chartbox { flex:1 1 46%; min-width:min(420px,100%); margin-bottom:0; }
.missing { color:#6b7280; font-style:italic; }
/* Сводная таблица параметров по инверторам */
.pivot-wrap { background:#181c24; border:1px solid #252b36; border-radius:10px; padding:14px 16px; margin:16px 0 20px; overflow-x:auto; }
.pivot-table { border-collapse:collapse; width:100%; font-size:13px; }
.pivot-table th, .pivot-table td { border:1px solid #333b49; padding:6px 10px; text-align:center; white-space:nowrap; }
.pivot-table thead th { font-size:12px; font-weight:600; color:#e6e6e6; background:#202630; }
.pivot-table .p-label { text-align:left; color:#aab3bf; width:auto; }
.pivot-table td.p-val { font-variant-numeric:tabular-nums; font-weight:600; }
.pivot-table td.p-unit { color:#8a93a1; font-weight:400; }
.pivot-table td.p-empty { color:#4a5464; }
.pivot-table tr:nth-child(even) td { background:#1b212b; }
/* Группы верхних плашек: общая рамка с заголовком и рядом плашек одинакового размера */
.group { background:#10161f; border:1px solid #2a3342; border-radius:12px; padding:14px 16px; }
.group-head { display:flex; align-items:baseline; justify-content:space-between; gap:12px; margin-bottom:10px; }
.group-title { font-size:14px; font-weight:700; color:#e6e6e6; margin-bottom:10px; }
.group-head .group-title { margin-bottom:0; }
.group-body { display:flex; flex-wrap:wrap; gap:10px; }
.groups-row { display:flex; align-items:stretch; gap:16px; margin:0 0 16px; }
.groups-row .group { flex:1 1 0; min-width:0; }
/* Единая плашка (для счётчика, инверторов и МАП) — приводятся к одному размеру */
.plate, .meter-stat { flex:1; background:linear-gradient(135deg,#1d2430,#202a3a); border:1px solid #2a3342; border-radius:10px; padding:10px 16px; min-width:130px; display:flex; flex-direction:column; gap:4px; }
.plate .lbl, .meter-stat .lbl { font-size:11px; color:#8a93a1; text-transform:uppercase; letter-spacing:.05em; white-space:nowrap; overflow:hidden; text-overflow:ellipsis; }
.plate .val, .meter-stat .val { font-size:26px; font-weight:700; line-height:1.1; font-variant-numeric:tabular-nums; }
.plate .unit, .meter-stat .unit { font-size:14px; color:#8a93a1; font-weight:400; margin-left:5px; }
.plate .sub, .meter-stat .sub, .kpi-sub { font-size:11px; color:#6b7280; }
.meter-stat .pos { color:#6fd08a; } /* положительная величина / потребление */
.meter-stat .neg { color:#ff6b6b; } /* отрицательная величина / отдача в сеть */
.meter-stat .off { color:#ff9f43; } /* нулевое/неопределённое */
.meter-ts { font-size:12px; color:#6b7280; }
.meter-note { font-size:11px; color:#6b7280; margin-top:8px; }
</style>
</head>
<body>
<div class="top-nav">
  <div>
    <h1 class="ttl">SunReceiver</h1>
    <p class="sub">Текущие параметры инверторов и электросчётчика (из Redis, обновление каждую секунду)</p>
  </div>
  <a class="nav-btn" href="/charts">Открыть графики</a>
  <a class="nav-btn" href="/energy">Электроэнергия</a>
</div>

<div class="group">
  <div class="group-head">
    <span class="group-title">Электросчётчик DDS238 &mdash; текущие параметры</span>
    <span class="meter-ts" id="meterTs">&mdash;</span>
  </div>
  <div class="group-body" id="meterStats"><span class="missing">Нет данных</span></div>
  <div class="meter-note">Мощность с отрицательным знаком &mdash; отдача в сеть (генерация); положительная &mdash; потребление.</div>
</div>

<div class="groups-row">
  <div class="group">
    <div class="group-title">Данные МАП</div>
    <div class="group-body">
      <div class="plate">
        <div class="lbl">Напряжение сети</div>
        <div class="val"><span id="kpiGridV">—</span><span class="unit">V</span></div>
        <div class="sub">МАП (батарея/сеть)</div>
      </div>
      <div class="plate">
        <div class="lbl">Мощность сети</div>
        <div class="val"><span id="kpiGridP">—</span><span class="unit">W</span></div>
        <div class="sub">МАП (батарея/сеть)</div>
      </div>
      <div class="plate">
        <div class="lbl">Напряжение батареи</div>
        <div class="val"><span id="kpiBatV">—</span><span class="unit">V</span></div>
        <div class="sub">МАП (батарея/сеть)</div>
      </div>
      <div class="plate">
        <div class="lbl">Мощность батареи</div>
        <div class="val"><span id="kpiBatP">—</span><span class="unit">W</span></div>
        <div class="sub">МАП (батарея/сеть)</div>
      </div>
      <div class="plate">
        <div class="lbl">Мощность потребления</div>
        <div class="val"><span id="kpiConsP">—</span><span class="unit">W</span></div>
        <div class="sub">Сеть + батарея</div>
      </div>
    </div>
  </div>
  <div class="group">
    <div class="group-title">Мощности инверторов</div>
    <div class="group-body">
      <div class="plate">
        <div class="lbl">Суммарная активная мощность</div>
        <div class="val"><span id="kpiTotal">—</span><span class="unit">W</span></div>
        <div class="sub" id="kpiSub">Нет данных</div>
      </div>
      <div class="plate">
        <div class="lbl">Суммарная мощность PV</div>
        <div class="val"><span id="kpiPV">—</span><span class="unit">W</span></div>
        <div class="sub" id="kpiPVSub">Нет данных</div>
      </div>
    </div>
  </div>
  <div class="group">
    <div class="group-title">Потребление/Отдача за сегодня</div>
    <div class="group-body">
      <div class="plate">
        <div class="lbl">Потребление день</div>
        <div class="val"><span id="kpiImpDay">—</span><span class="unit">kWh</span></div>
        <div class="sub">День 07:00–23:00</div>
      </div>
      <div class="plate">
        <div class="lbl">Потребление ночь</div>
        <div class="val"><span id="kpiImpNight">—</span><span class="unit">kWh</span></div>
        <div class="sub">Ночь 23:00–07:00</div>
      </div>
      <div class="plate">
        <div class="lbl">Отдача день</div>
        <div class="val"><span id="kpiExpDay">—</span><span class="unit">kWh</span></div>
        <div class="sub">День 07:00–23:00</div>
      </div>
      <div class="plate">
        <div class="lbl">Отдача ночь</div>
        <div class="val"><span id="kpiExpNight">—</span><span class="unit">kWh</span></div>
        <div class="sub">Ночь 23:00–07:00</div>
      </div>
    </div>
  </div>
</div>

<div class="period-panel">
  <button id="btnToday">Сегодня</button>
  <button id="btnYesterday">Вчера</button>
  <button id="btn7d">7 дней</button>
  <input type="date" id="datePick" title="Выбрать день">
  <button id="btnDate">За выбранный день</button>
  <button id="btnRefresh" title="Принудительно обновить графики">Обновить графики</button>
</div>

<div class="pivot-wrap" id="cards"><div class="missing">Загрузка...</div></div>

<script>
'use strict';

// ---------- Утилиты ----------
function esc(s){ return String(s).replace(/[&<>"]/g,function(c){ return {'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;'}[c]; }); }
function fmt(t){ var d=new Date(t); function p(x){return (x<10?'0':'')+x;} return d.getFullYear()+'-'+p(d.getMonth()+1)+'-'+p(d.getDate())+' '+p(d.getHours())+':'+p(d.getMinutes()); }
function fmtSec(t){ var d=new Date(t); function p(x){return (x<10?'0':'')+x;} return d.getFullYear()+'-'+p(d.getMonth()+1)+'-'+p(d.getDate())+' '+p(d.getHours())+':'+p(d.getMinutes())+':'+p(d.getSeconds()); }
function startOfToday(){ var d=new Date(); d.setHours(0,0,0,0); return d; }
function endOfToday(){ var d=new Date(); d.setHours(23,59,59,999); return d; }

// ---------- Сводная таблица параметров по инверторам ----------
// Упорядоченный список строк: [тег, подпись, единица измерения].
// Единица пишется в самую правую колонку и одинакова для всех инверторов.
var PARAMS = [
	['pv1_voltage','Напряжение PV1','V'], ['pv1_current','Ток PV1','A'], ['pv1_power','Мощность PV1','W'],
	['pv2_voltage','Напряжение PV2','V'], ['pv2_current','Ток PV2','A'], ['pv2_power','Мощность PV2','W'],
	['ac_active_power','Активная мощность','W'], ['ac_reactive_power','Реактивная мощность','var'],
	['grid_frequency','Частота сети','Hz'],
	['l1_voltage','Напряжение L1','V'], ['l1_current','Ток L1','A'], ['l1_power','Мощность L1','W'],
	['energy_today','Выработка сегодня','kWh'], ['energy_total','Выработка всего','kWh']
];
// devValue возвращает строковое значение тега инвертора или null, если его нет.
// Для вычисляемых тегов (l1_power = l1_voltage × l1_current) значение считается из
// исходных тегов, если явного тега в данных нет.
function devValue(dev, tag){
	var v = (dev && dev.values) ? dev.values[tag] : undefined;
	if(v === undefined || v === null){
		if(tag === 'l1_power' && dev && dev.values){
			var u=dev.values['l1_voltage'], i=dev.values['l1_current'];
			// Мощность = U×I. Произведение чисел с плавающей точкой даёт длинную дробную
			// часть (например 228.9×0.35 = 80.11499999…) — округляем до 1 знака, как у остальных.
			if(u!==undefined && u!==null && i!==undefined && i!==null) v=Math.round(Number(u)*Number(i)*10)/10;
		}
	}
	return (v === undefined || v === null) ? null : v;
}
var GRID_COLOR='#4ecdc4', MPPT_COLOR='#f9ca24';
function renderPivot(devices){
	if(!devices || !devices.length) return '<div class="missing">No data in Redis</div>';
	// Разбиваем колонки на две группы: сетевые инверторы (первые) и MPPT-контроллеры (последние).
	// Устройство МАП (батарея/сеть) в сводной таблице не показываем — у него нет солнечных
	// панелей; оно нужно только для верхних плашек и графиков. Маркер MPPT — ip вида host#mpptN.
	var grid=[], mpts=[];
	for(var i=0;i<devices.length;i++){
		var d=devices[i];
		if(isMAPDeviceJS(d) || isMeterDevice(d)) continue;
		if(String(d.ip||'').indexOf('#mppt')>=0) mpts.push(d); else grid.push(d);
	}
	if(!grid.length && !mpts.length) return '<div class="missing">No data in Redis</div>';
	// Единая таблица: общий первый столбец (заголовки строк и единиц), затем колонки
	// сетевых инверторов и MPPT, разделённые тонкой цветной рамкой групп.
	var h='<table class="pivot-table"><thead>';
	// Строка заголовков групп (colspan по числу колонок группы).
	h+='<tr><th class="p-label">Параметр</th>';
	if(grid.length) h+='<th colspan="'+grid.length+'" style="border:1px solid '+GRID_COLOR+';color:'+GRID_COLOR+'">Сетевые инверторы</th>';
	if(mpts.length) h+='<th colspan="'+mpts.length+'" style="border:1px solid '+MPPT_COLOR+';color:'+MPPT_COLOR+'">MPPT-контроллеры</th>';
	h+='</tr><tr><th></th>';
	for(var i=0;i<grid.length;i++) h+='<th>'+esc(grid[i].name)+'</th>';
	for(var i=0;i<mpts.length;i++) h+='<th>'+esc(mpts[i].name)+'</th>';
	h+='</tr></thead><tbody>';
	// Общие строки. Первый столбец — заголовок строки; аудитория по обеим группам.
	h+='<tr><td class="p-label">Актуально</td>'+rowCells(grid,mpts,function(d){return d.timestamp?fmtSec(d.timestamp):null;},'11px')+'</tr>';
	h+='<tr><td class="p-label">Серийный номер</td>'+rowCells(grid,mpts,function(d){return d.device_sn||null;},'11px')+'</tr>';
	for(var p=0;p<PARAMS.length;p++){
		var tag=PARAMS[p][0], label=PARAMS[p][1], unit=PARAMS[p][2];
		h+='<tr><td class="p-label">'+esc(label)+' ('+esc(unit)+')</td>'+rowCells(grid,mpts,function(d){return devValue(d,tag);})+'</tr>';
	}
	// Нижняя кромка рамок групп.
	h+='<tr><td></td>';
	for(var i=0;i<grid.length;i++) h+=edgeCell(GRID_COLOR,i===0,i===grid.length-1);
	for(var i=0;i<mpts.length;i++) h+=edgeCell(MPPT_COLOR,i===0,i===mpts.length-1);
	h+='</tr>';
	h+='</tbody></table>';
	return h;
}
// rowCells формирует ячейки строки по колонкам обеих групп. Первая колонка группы
// получает цветную левую, последняя — цветную правую рамку (вертикальные стороны).
function rowCells(grid,mpts,getter,font){
	var h='';
	for(var i=0;i<grid.length;i++) h+=cellTd(grid[i],getter,i===0,i===grid.length-1,GRID_COLOR,font);
	for(var i=0;i<mpts.length;i++) h+=cellTd(mpts[i],getter,i===0,i===mpts.length-1,MPPT_COLOR,font);
	return h;
}
function cellTd(d,getter,isFirst,isLast,color,font){
	var v=getter(d);
	var st='';
	if(isFirst) st+='border-left:1px solid '+color+';';
	if(isLast) st+='border-right:1px solid '+color+';';
	if(font) st+='font-size:'+font+';';
	var cls=v===null?'p-empty':'p-val';
	return '<td class="'+cls+'" style="'+st+'">'+(v===null?'':esc(v))+'</td>';
}
// edgeCell формирует пустую ячейку нижней кромки группы с цветной горизонтальной чертой.
function edgeCell(color,isFirst,isLast){
	var st='border-top:1px solid '+color+';';
	if(isFirst) st+='border-left:1px solid '+color+';';
	if(isLast) st+='border-right:1px solid '+color+';';
	return '<td class="p-empty" style="'+st+'"></td>';
}
// isMeterDevice возвращает true, если устройство — электросчётчик DDS238 (имеет
// теги meter_*). Используется, чтобы не выводить счётчик в сводную таблицу
// инверторов и не считать его в «инверторах онлайн».
function isMeterDevice(d){ return !!(d && d.values && d.values.meter_voltage !== undefined); }
// isMAPDeviceJS — маркер устройства МАП (батарея/сеть) в JS (аналог isMAPDevice в Go).
function isMAPDeviceJS(d){ return !!(d && d.values && d.values.battery_voltage !== undefined); }

// METER_PARAMS — параметры счётчика для плашки: [тег, подпись, единица, знаковый].
// Знаковые (активная/реактивная мощность) окрашиваются: отрицательная — отдача.
var METER_PARAMS = [
	['meter_voltage','Напряжение','V',false],
	['meter_current','Ток','A',false],
	['meter_active_power','Активная мощность','W',true],
	['meter_reactive_power','Реактивная мощность','var',true],
	['meter_power_factor','Коэффициент мощности','',false],
	['meter_frequency','Частота','Hz',false],
	['meter_import','Потребление (Import)','kWh',false],
	['meter_export','Отдача (Export)','kWh',false],
	['meter_total','Общая (Total)','kWh',false]
];
// renderMeter строит HTML статистик плашки счётчика из его снимка (или «Нет данных»).
function renderMeter(meter){
	var stats=document.getElementById('meterStats');
	if(!meter){ stats.innerHTML='<span class="missing">Нет данных</span>'; return; }
	var ts=document.getElementById('meterTs');
	ts.textContent=meter.timestamp? 'Актуально: '+fmtSec(meter.timestamp) : '—';
	var h='';
	for(var i=0;i<METER_PARAMS.length;i++){
		var t=METER_PARAMS[i][0], lbl=METER_PARAMS[i][1], unit=METER_PARAMS[i][2], signed=METER_PARAMS[i][3];
		var raw=meter.values? meter.values[t] : undefined;
		if(raw===undefined||raw===null){ h+='<div class="meter-stat"><div class="lbl">'+esc(lbl)+'</div><div class="val off">—</div></div>'; continue; }
		var n=Number(raw);
		var cls='val', txt;
		if(isFinite(n)){
			txt=n.toLocaleString('ru-RU',{maximumFractionDigits:2});
			if(signed){ cls+=' '+(n<0?' neg':' pos'); }
		}else{
			cls+=' off'; txt='—';
		}
		h+='<div class="meter-stat"><div class="lbl">'+esc(lbl)+'</div><div class="'+cls+'">'+esc(txt)+
		   (unit?' <span class="unit">'+esc(unit)+'</span>':'')+'</div></div>';
	}
	stats.innerHTML=h;
}
async function tick(){
	try{
		var r=await fetch('/api/current');
		if(!r.ok) return;
		var data=await r.json();
		// Плашка суммарной мощности
		var kpiEl=document.getElementById('kpiTotal');
		var n=Number(data.total_power);
		if(isFinite(n) && data.total_power>0){
			kpiEl.textContent=n.toLocaleString('ru-RU',{maximumFractionDigits:1});
			// Число инверторов онлайн — без устройства МАП (батарея/сеть) и счётчика.
			var invCount=0;
			for(var i=0;i<data.devices.length;i++) if(!isMAPDeviceJS(data.devices[i]) && !isMeterDevice(data.devices[i])) invCount++;
			document.getElementById('kpiSub').textContent=invCount+' инверторов онлайн';
		}else{
			kpiEl.textContent='—';
			document.getElementById('kpiSub').textContent='Нет данных';
		}
		// Плашка суммарной мощности PV
		var pvEl=document.getElementById('kpiPV');
		var pv=Number(data.total_pv);
		if(isFinite(pv) && data.total_pv>0){
			pvEl.textContent=pv.toLocaleString('ru-RU',{maximumFractionDigits:1});
		}else{
			pvEl.textContent='—';
		}
		// Плашки МАП: напряжение/мощность сети и батареи.
		setKpi('kpiGridV', data.map_grid_voltage);
		setKpi('kpiGridP', data.map_grid_power);
		setKpi('kpiBatV', data.map_battery_voltage);
		setKpi('kpiBatP', data.map_battery_power);
		setKpi('kpiConsP', data.map_consumption);
		// Плашки «Потребление/Отдача за сегодня» (kWh): считаются из актуальных
		// показаний счётчика и фиксированных граничных точек тарифов.
		setKpi2('kpiImpDay', data.meter_import_day);
		setKpi2('kpiImpNight', data.meter_import_night);
		setKpi2('kpiExpDay', data.meter_export_day);
		setKpi2('kpiExpNight', data.meter_export_night);
		// Плашка электросчётчика (вверху).
		var meter=null;
		for(var i=0;i<data.devices.length;i++) if(isMeterDevice(data.devices[i])){ meter=data.devices[i]; break; }
		renderMeter(meter);
		var cards=document.getElementById('cards');
		cards.innerHTML = renderPivot(data.devices);
	}catch(e){}
}
// setKpi заполняет плашку числом (с разделителями) или прочерком, если нет данных.
function setKpi(id, v){
	var n=Number(v);
	if(isFinite(n)){
		document.getElementById(id).textContent=n.toLocaleString('ru-RU',{maximumFractionDigits:1});
	}else{
		document.getElementById(id).textContent='—';
	}
}
// setKpi2 заполняет плашку kWh-величиной (до 2 знаков) или прочерком, если нет данных.
function setKpi2(id, v){
	var n=Number(v);
	if(isFinite(n)){
		document.getElementById(id).textContent=n.toLocaleString('ru-RU',{maximumFractionDigits:2});
	}else{
		document.getElementById(id).textContent='—';
	}
}

tick(); setInterval(tick,1000);
</script>
</body>
</html>`

// chartsPage — страница графиков: временные ряды инверторов/МАП/счётчика за
// выбранный период (Redis полное разрешение за 2 суток + PG 5-минутные средние).
const chartsPage = `<!DOCTYPE html>
<html lang="ru">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Графики — SunReceiver</title>
<script src="https://cdn.jsdelivr.net/npm/chart.js@4.4.1/dist/chart.umd.min.js"></script>
<script src="https://cdn.jsdelivr.net/npm/chartjs-adapter-date-fns@3.0.0/dist/chartjs-adapter-date-fns.bundle.min.js"></script>
<script src="https://cdn.jsdelivr.net/npm/chartjs-plugin-zoom@2.0.1/dist/chartjs-plugin-zoom.min.js"></script>
<style>
:root { color-scheme: dark; }
* { box-sizing: border-box; }
body { font-family: -apple-system, "Segoe UI", Roboto, sans-serif; background:#0f1115; color:#e6e6e6; margin:0; padding:20px; overflow-x:hidden; }
.top-nav { display:flex; align-items:center; justify-content:space-between; gap:12px; flex-wrap:wrap; margin-bottom:16px; }
.top-nav .ttl { margin:0; font-size:22px; }
.top-nav .sub { color:#8a93a1; margin:4px 0 0; font-size:13px; }
.nav-btn { background:#2f6fed; color:#fff; border:none; border-radius:8px; padding:9px 16px; font-size:14px; font-weight:600; cursor:pointer; text-decoration:none; white-space:nowrap; }
.nav-btn:hover { background:#3f7bf0; }
.nav-btn.secondary { background:#252b36; border:1px solid #333b49; }
#chartbox { background:#181c24; border:1px solid #252b36; border-radius:10px; padding:16px; margin-bottom:20px; max-width:100%; }
#chartbox h2 { margin:0 0 8px; font-size:16px; }
.chart-toolbar { display:flex; align-items:center; flex-wrap:wrap; gap:10px 12px; margin-bottom:8px; font-size:13px; color:#8a93a1; }
.chart-toolbar button { background:#252b36; color:#e6e6e6; border:1px solid #333b49; border-radius:6px; padding:4px 10px; cursor:pointer; font-size:13px; }
.chart-toolbar button:hover { background:#2f3644; }
.period-panel { display:flex; align-items:center; flex-wrap:wrap; gap:10px; margin-bottom:16px; font-size:13px; color:#8a93a1; }
.period-panel button { background:#252b36; color:#e6e6e6; border:1px solid #333b49; border-radius:6px; padding:5px 12px; cursor:pointer; font-size:13px; }
.period-panel button:hover { background:#2f3644; }
.period-panel button.active { background:#2f6fed; border-color:#2f6fed; color:#fff; }
.period-panel input[type=date] { background:#181c24; color:#e6e6e6; border:1px solid #333b49; border-radius:6px; padding:4px 8px; font-size:13px; color-scheme:dark; }
.period-panel input[type=date]:focus { outline:none; border-color:#2f6fed; }
.period-panel .nav-arrow { padding:5px 10px; font-size:16px; line-height:1; }
.chart-wrap { position:relative; height:340px; }
.charts { display:flex; flex-wrap:wrap; gap:16px; margin-bottom:20px; }
.charts #chartbox { flex:1 1 46%; min-width:min(420px,100%); margin-bottom:0; }
.missing { color:#6b7280; font-style:italic; }
</style>
</head>
<body>
<div class="top-nav">
  <div>
    <h1 class="ttl">Графики</h1>
    <p class="sub">Временные ряды за выбранный период; статистика счётчика «день/ночь» за последние дни</p>
  </div>
  <a class="nav-btn secondary" href="/">&larr; Назад</a>
</div>

<div class="period-panel">
  <button id="btnToday">Сегодня</button>
  <button id="btnYesterday">Вчера</button>
  <button id="btn7d">7 дней</button>
  <button id="btnMonth">Месяц</button>
  <button id="btnPeriodPrev" class="nav-arrow" title="Предыдущий период">&lsaquo;</button>
  <input type="date" id="datePick" title="Выбрать день">
  <button id="btnDate">За выбранный день</button>
  <span style="color:#555">С</span>
  <input type="date" id="fromPick" title="Начало периода">
  <span style="color:#555">по</span>
  <input type="date" id="toPick" title="Конец периода">
  <button id="btnRange">Показать период</button>
  <button id="btnPeriodNext" title="Следующий период">&rsaquo;</button>
  <button id="btnRefresh" title="Принудительно обновить графики">Обновить графики</button>
</div>

<div class="charts">
  <div id="chartbox">
    <h2>Напряжение сети и батареи (МАП), V + напряжение счётчика</h2>
    <div class="chart-toolbar">
      <span id="gridVChartRange"></span>
      <button id="btnGridVReset">Сброс зума</button>
      <span>Зум: колесо / drag&ndash;панорама</span>
    </div>
    <div class="chart-wrap"><canvas id="gridVChart"></canvas></div>
  </div>

  <div id="chartbox">
    <h2>Мощности сети и батареи (МАП), W + активная мощность счётчика</h2>
    <div class="chart-toolbar">
      <span id="gridPChartRange"></span>
      <button id="btnGridPReset">Сброс зума</button>
      <span>Зум: колесо / drag&ndash;панорама</span>
    </div>
    <div class="chart-wrap"><canvas id="gridPChart"></canvas></div>
  </div>

  <div id="chartbox">
    <h2>Суммарная активная мощность, W</h2>
    <div class="chart-toolbar">
      <span id="totalChartRange"></span>
      <button id="btnTotalReset">Сброс зума</button>
      <span>Зум: колесо / drag&ndash;панорама</span>
    </div>
    <div class="chart-wrap"><canvas id="totalChart"></canvas></div>
  </div>

  <div id="chartbox">
    <h2>Активная мощность по инверторам, W</h2>
    <div class="chart-toolbar">
      <span id="chartRange"></span>
      <button id="btnReset">Сброс зума</button>
      <span>Зум: колесо / drag&ndash;панорама</span>
    </div>
    <div class="chart-wrap"><canvas id="powerChart"></canvas></div>
  </div>
</div>

<script>
'use strict';

function fmt(t){ var d=new Date(t); function p(x){return (x<10?'0':'')+x;} return d.getFullYear()+'-'+p(d.getMonth()+1)+'-'+p(d.getDate())+' '+p(d.getHours())+':'+p(d.getMinutes()); }
function fmtSec(t){ var d=new Date(t); function p(x){return (x<10?'0':'')+x;} return d.getFullYear()+'-'+p(d.getMonth()+1)+'-'+p(d.getDate())+' '+p(d.getHours())+':'+p(d.getMinutes())+':'+p(d.getSeconds()); }
function startOfToday(){ var d=new Date(); d.setHours(0,0,0,0); return d; }
function endOfToday(){ var d=new Date(); d.setHours(23,59,59,999); return d; }

// ---------- Графики ----------
Chart.register(ChartZoom);

var preserveZoom=false;
var hoverPix={};
// TOOLTIP_MAX_GAP — максимальный возраст «ближайшей точки слева» (мс), который
// считается актуальным в хинте. Устройства, замолчавшие дольше этого (простои,
// исчезновение с дашборда), не должны показываться как продолжающие выдавать
// мощность из последней известной точки — их точку в хинте пропускаем.
var TOOLTIP_MAX_GAP=20*60*1000;
function drawCursorTooltip(chart){
	try{
		var px=hoverPix[chart.canvas.id];
		if(px===undefined) return;
		var xScale=chart.scales&&chart.scales.x, yScale=chart.scales&&chart.scales.y;
		if(!xScale||!yScale) return;
		if(!isFinite(px)||px<xScale.left||px>xScale.right) return;
		var t=xScale.getValueForPixel(px);
		var ctx=chart.ctx; ctx.save();
		ctx.beginPath(); ctx.moveTo(px,yScale.top); ctx.lineTo(px,yScale.bottom);
		ctx.strokeStyle='rgba(160,160,160,0.55)'; ctx.lineWidth=1; ctx.stroke();
		var labels=[], titleT=null;
		chart.data.datasets.forEach(function(ds){
			var pts=ds.data; if(!pts||!pts.length) return;
			var best=null;
			for(var i=0;i<pts.length;i++){
				var p=pts[i];
				var X=(p.x instanceof Date)? p.x.getTime() : Number(p.x);
				if(!isFinite(X)) continue;
				if(X<=t && (best===null || X>best)) best=X;
			}
			if(best===null) return;
			// Данные «неадекватно» старые относительно курсора (устройство давно
			// не присылает снимки) — не показываем его значение как текущее.
			if(t-best>TOOLTIP_MAX_GAP) return;
			var v=null;
			for(var i2=0;i2<pts.length;i2++){ var q=pts[i2]; var qX=(q.x instanceof Date)? q.x.getTime() : Number(q.x); if(qX===best){ v=q.y; break; } }
			if(v===null||v===undefined) return;
			var dsy = chart.scales && (chart.scales[ds.yAxisID||'y']) || yScale;
			if(!dsy || !isFinite(v)) return;
			labels.push({ label:ds.label||'', color:ds.borderColor||'#999', v:v,
				px:xScale.getPixelForValue(best), py:dsy.getPixelForValue(v), txt:'' });
			if(titleT===null||best>titleT) titleT=best;
		});
		if(!labels.length){ ctx.restore(); return; }
		labels.forEach(function(l){ ctx.beginPath(); ctx.arc(l.px,l.py,3.5,0,2*Math.PI); ctx.fillStyle=l.color; ctx.fill(); ctx.strokeStyle='rgba(0,0,0,0.9)'; ctx.lineWidth=1; ctx.stroke(); });
		var boxW=0, lineH=16;
		labels.forEach(function(l){ l.txt=l.label+': '+Number(l.v).toFixed(1)+' '; });
		labels.forEach(function(l){ var w=ctx.measureText(l.txt).width; if(w>boxW) boxW=w; });
		var header=titleT!==null? fmtSec(new Date(titleT)) : '';
		if(header && ctx.measureText(header).width>boxW) boxW=ctx.measureText(header).width;
		boxW+=22; var boxH=labels.length*lineH+(header?lineH:0)+8;
		var bx=px+12, by=yScale.top+4;
		if(bx+boxW>xScale.right) bx=px-boxW-12;
		if(bx<xScale.left) bx=xScale.left+2;
		ctx.fillStyle='rgba(20,20,25,0.88)'; ctx.fillRect(bx,by,boxW,boxH);
		ctx.strokeStyle='rgba(255,255,255,0.25)'; ctx.lineWidth=1; ctx.strokeRect(bx,by,boxW,boxH);
		var ty=by+(header?lineH+4:8);
		if(header){ ctx.fillStyle='#e8e8e8'; ctx.font='600 12px sans-serif'; ctx.textBaseline='top'; ctx.fillText(header,bx+11,by+6); }
		labels.forEach(function(l){ ctx.textBaseline='top'; ctx.fillStyle=l.color; ctx.font='12px sans-serif'; ctx.fillText(l.txt,bx+11,ty); ty+=lineH; });
		ctx.restore();
	}catch(err){ try{ ctx&&ctx.restore(); }catch(e){} }
}
var lastXWindow={};
var cursorTooltipPlugin={ id:'cursorTooltip', afterDraw:function(chart){
	try{
		checkZoomSync(chart);
		drawZeroGridAxis(chart);
		drawCursorTooltip(chart);
	}catch(err){}
} };
function drawZeroGridAxis(chart){
	if(!chart || (chart.canvas.id!=='gridPChart' && chart.canvas.id!=='totalChart')) return;
	var x=chart.scales&&chart.scales.x, y=chart.scales&&chart.scales.y;
	if(!x||!y) return;
	if(y.min>0 || y.max<0) return;
	var ctx=chart.ctx; ctx.save();
	var py=y.getPixelForValue(0);
	if(py<y.top||py>y.bottom){ ctx.restore(); return; }
	ctx.beginPath(); ctx.moveTo(x.left,py); ctx.lineTo(x.right,py);
	ctx.strokeStyle='rgba(0,0,0,0.85)'; ctx.lineWidth=1.2; ctx.setLineDash && ctx.setLineDash([]);
	ctx.stroke();
	ctx.restore();
}
function checkZoomSync(chart){
	if(zoomSyncing) return;
	var x=chart&&chart.scales&&chart.scales.x;
	if(!x||!isFinite(x.min)||!isFinite(x.max)||x.max<=x.min) return;
	var key=x.min.toFixed(3)+','+x.max.toFixed(3);
	if(lastXWindow[chart.canvas.id]!==undefined && lastXWindow[chart.canvas.id]!==key) syncZoomToOthers(chart);
	lastXWindow[chart.canvas.id]=key;
}
var zoomSyncing=false;
function syncZoomToOthers(fromChart){
	if(zoomSyncing) return;
	var sx=fromChart&&fromChart.scales&&fromChart.scales.x;
	if(!sx||!isFinite(sx.min)||!isFinite(sx.max)||sx.max<=sx.min) return;
	var m=sx.min, M=sx.max;
	zoomSyncing=true;
	try{
		['powerChart','totalChart','gridVChart','gridPChart'].forEach(function(id){
			var c=window[id]||Chart.getChart(id);
			if(!c || c===fromChart) return;
			try{ c.zoomScale('x', {min:m, max:M}, 'none'); }catch(e){}
		});
	}finally{ zoomSyncing=false; }
}
function renderChart(id, datasets, opts){
	var canvas=document.getElementById(id);
	var old=window[id];
	var saved={min:null,max:null};
	if(old && old.scales && old.scales.x && isFinite(old.scales.x.min) && isFinite(old.scales.x.max)){
		saved={min:old.scales.x.min, max:old.scales.x.max};
	}
	if(old){ try{ old.destroy(); }catch(e){} }
	canvas.getContext('2d');
	window[id]=new Chart(canvas,{ type:'line', data:{datasets:datasets}, options:opts, plugins:[cursorTooltipPlugin] });
	canvas.addEventListener('mouseleave',function(){ delete hoverPix[id]; try{ window[id]&&window[id].update('none'); }catch(e){} });
	if(preserveZoom && saved.min!==null && saved.max!==null){
		window[id].options.scales.x.min=saved.min; window[id].options.scales.x.max=saved.max;
		window[id].update('none');
	}
	return window[id];
}
function chartOpts(withLegend,yTitle,extra){
	var o={
		responsive:true,
		maintainAspectRatio:false,
		interaction:{ mode:'index', intersect:false },
		animation:{ duration:300 },
		onHover:function(event, elements, chart){
			if(chart && chart.canvas){
				if(event && isFinite(event.x)) hoverPix[chart.canvas.id]=event.x;
				try{ chart.update('none'); }catch(e){}
			}
		},
		plugins:{
			tooltip:{ enabled:false },
			zoom:{
				pan:{ enabled:true, mode:'x' },
				zoom:{ wheel:{ enabled:true, speed:0.1, modifierKey:'ctrl' }, pinch:{ enabled:true }, mode:'x' },
				limits:{ x:{ minRange: 60*1000 } }
			}
		},
		scales:{
			x:{ type:'time', time:{ unit:'hour', displayFormats:{ hour:'HH:mm' }, tooltipFormat:'yyyy-MM-dd HH:mm:ss' }, ticks:{ maxRotation:0, autoSkipPadding:20 } },
			y:{ beginAtZero:true, title:{ display:yTitle, text:yTitle||'' } }
		}
	};
	if(extra){
		if(extra.zoomMode){ o.plugins.zoom.zoom.mode=extra.zoomMode; o.plugins.zoom.pan.mode=extra.zoomMode; }
		if(extra.limits){ o.plugins.zoom.limits=Object.assign(o.plugins.zoom.limits, extra.limits); }
		if(extra.scales){ for(var k in extra.scales) o.scales[k]=extra.scales[k]; }
	}
	if(withLegend){ o.plugins.legend={ display:true, labels:{ boxWidth:20, padding:14 } }; }
	return o;
}

// ---------- Выбор периода ----------
var selRange={from:startOfToday(), to:endOfToday()};
var periodMode='day';
function dayFromStr(s){
	var p=String(s).split('-').map(Number);
	return new Date(p[0], p[1]-1, p[2], 0,0,0,0);
}
function toInputDate(d){ function p(x){return (x<10?'0':'')+x;} return d.getFullYear()+'-'+p(d.getMonth()+1)+'-'+p(d.getDate()); }
function endOfDay(d){
	var e=new Date(d); e.setHours(23,59,59,999); return e;
}
function startOfYesterday(){
	var d=new Date(); d.setDate(d.getDate()-1); d.setHours(0,0,0,0); return d;
}
function dayStart(d){ var r=new Date(d); r.setHours(0,0,0,0); return r; }
function addDays(d,n){ var r=new Date(d); r.setDate(r.getDate()+n); return r; }
function addMonths(d,n){ var r=new Date(d); r.setMonth(r.getMonth()+n); return r; }
function startOfMonthOf(d){ return dayStart(new Date(d.getFullYear(), d.getMonth(), 1)); }
function endOfMonthOf(d){ var f=new Date(d.getFullYear(), d.getMonth(), 1); return new Date(f.getFullYear(), f.getMonth()+1, 0, 23,59,59,999); }
var PERIOD_BTNS=['btnToday','btnYesterday','btn7d','btnMonth'];
function setActiveBtn(activeBtn){
	for(var i=0;i<PERIOD_BTNS.length;i++) document.getElementById(PERIOD_BTNS[i]).classList.remove('active');
	if(activeBtn) document.getElementById(activeBtn).classList.add('active');
}
// setPeriod(from,to,mode,activeBtn) — выставляет диапазон, режим (day/week/month/custom,
// чтобы стрелки знали шаг), синхронизирует поля ввода и перерисовывает графики.
function setPeriod(from,to,mode,activeBtn){
	selRange.from=from; selRange.to=to; periodMode=mode;
	preserveZoom=false;
	setActiveBtn(activeBtn);
	var dFrom=dayStart(from), dTo=dayStart(to);
	document.getElementById('fromPick').value=toInputDate(dFrom);
	document.getElementById('toPick').value=toInputDate(dTo);
	document.getElementById('datePick').value=toInputDate(mode==='day'?from:dFrom);
	loadAll();
}
// shiftPeriod(delta) — сдвигает текущий период назад/вперёд на его длительность.
function shiftPeriod(delta){
	var from=selRange.from, to=selRange.to;
	var newFrom, newTo;
	if(periodMode==='day'){ newFrom=addDays(dayStart(from), delta); newTo=endOfDay(newFrom); }
	else if(periodMode==='month'){ newFrom=addMonths(dayStart(from), delta); newTo=endOfMonthOf(newFrom); }
	else{ var span=to-from; newFrom=new Date(from.getTime()+delta*span); newTo=new Date(to.getTime()+delta*span); }
	setPeriod(newFrom,newTo,periodMode,null);
}
async function loadAll(){
	await Promise.all([loadTotalChart(), loadChart(), loadGridVChart(), loadGridPChart()]);
}

// Суммарный график
function buildTotalChart(data){
	var pts=(data.total||[]).map(function(p){ return {x:new Date(p.t), y:p.v}; });
	var datasets=[{ label:'Сумма', data:pts, borderColor:'#ffd166', backgroundColor:'#ffd166',
		pointRadius:0, pointHoverRadius:0, borderWidth:1.5, tension:0.35, cubicInterpolationMode:'monotone', fill:false }];
	renderChart('totalChart', datasets, chartOpts(false,'W'));
	return window.totalChart;
}
async function loadTotalChart(){
	var url='/api/series?from='+encodeURIComponent(selRange.from.toISOString())+'&to='+encodeURIComponent(selRange.to.toISOString());
	try{
		var r=await fetch(url);
		if(!r.ok) return;
		var data=await r.json();
		document.getElementById('totalChartRange').textContent='Диапазон: '+fmt(selRange.from)+' — '+fmt(selRange.to);
		buildTotalChart(data);
	}catch(e){}
}
document.getElementById('btnTotalReset').addEventListener('click',function(){ if(window.totalChart) window.totalChart.resetZoom(); });

// Активная мощность по инверторам
function buildChart(data){
	var datasets=[];
	for(var i=0;i<data.series.length;i++){
		var s=data.series[i];
		var pts=s.points.map(function(p){ return {x:new Date(p.t), y:p.v}; });
		datasets.push({ label:s.name, data:pts, borderColor:s.color, backgroundColor:s.color,
			pointRadius:0, pointHoverRadius:0, borderWidth:1.5, tension:0.35, cubicInterpolationMode:'monotone', fill:false });
	}
	renderChart('powerChart', datasets, chartOpts(true,'W'));
	return window.powerChart;
}
async function loadChart(){
	var url='/api/series?from='+encodeURIComponent(selRange.from.toISOString())+'&to='+encodeURIComponent(selRange.to.toISOString());
	try{
		var r=await fetch(url);
		if(!r.ok) return;
		var data=await r.json();
		document.getElementById('chartRange').textContent='Диапазон: '+fmt(selRange.from)+' — '+fmt(selRange.to);
		buildChart(data);
	}catch(e){}
}
document.getElementById('btnReset').addEventListener('click',function(){ if(window.powerChart) window.powerChart.resetZoom(); });

// Напряжения (МАП + счётчик). Счётчик на левой оси (белая линия).
function buildGridVChart(data){
	var grid=(data.map_grid_voltage||[]).map(function(p){ return {x:new Date(p.t), y:p.v}; });
	var bat=(data.map_battery_voltage||[]).map(function(p){ return {x:new Date(p.t), y:p.v}; });
	var meter=(data.meter_voltage||[]).map(function(p){ return {x:new Date(p.t), y:p.v}; });
	var datasets=[
		{ label:'Напряжение сети', data:grid, borderColor:'#4ecdc4', backgroundColor:'#4ecdc4',
		  pointRadius:0, pointHoverRadius:0, borderWidth:1.5, tension:0.35, cubicInterpolationMode:'monotone', fill:false },
		{ label:'Напряжение батареи', data:bat, borderColor:'#e74c3c', backgroundColor:'#e74c3c',
		  pointRadius:0, pointHoverRadius:0, borderWidth:1.5, tension:0.35, cubicInterpolationMode:'monotone', fill:false,
		  yAxisID:'y1' },
		{ label:'Напряжение счётчика', data:meter, borderColor:'#ffffff', backgroundColor:'#ffffff',
		  pointRadius:0, pointHoverRadius:0, borderWidth:1.5, tension:0.35, cubicInterpolationMode:'monotone', fill:false }
	];
	renderChart('gridVChart', datasets, chartOpts(true,'V',{
		zoomMode:'xy',
		limits:{ x:{minRange:60*1000}, y:{minRange:20}, y1:{minRange:20} },
		scales:{ y1:{ type:'linear', position:'right', beginAtZero:false, title:{display:true, text:'Напряжение батареи, V'} } }
	}));
	return window.gridVChart;
}
async function loadGridVChart(){
	var url='/api/series?from='+encodeURIComponent(selRange.from.toISOString())+'&to='+encodeURIComponent(selRange.to.toISOString());
	try{
		var r=await fetch(url);
		if(!r.ok) return;
		var data=await r.json();
		document.getElementById('gridVChartRange').textContent='Диапазон: '+fmt(selRange.from)+' — '+fmt(selRange.to);
		buildGridVChart(data);
	}catch(e){}
}
document.getElementById('btnGridVReset').addEventListener('click',function(){ if(window.gridVChart) window.gridVChart.resetZoom(); });

// Мощности (МАП + счётчик). Активная мощность счётчика белой линией на левой оси.
function buildGridPChart(data){
	var grid=(data.map_grid_power||[]).map(function(p){ return {x:new Date(p.t), y:p.v}; });
	var bat=(data.map_battery_power||[]).map(function(p){ return {x:new Date(p.t), y:p.v}; });
	var cons=(data.map_consumption||[]).map(function(p){ return {x:new Date(p.t), y:p.v}; });
	var meter=(data.meter_active_power||[]).map(function(p){ return {x:new Date(p.t), y:p.v}; });
	var datasets=[
		{ label:'Мощность сети', data:grid, borderColor:'#74b9ff', backgroundColor:'#74b9ff',
		  pointRadius:0, pointHoverRadius:0, borderWidth:1.5, tension:0.35, cubicInterpolationMode:'monotone', fill:false },
		{ label:'Мощность батареи', data:bat, borderColor:'#00b894', backgroundColor:'#00b894',
		  pointRadius:0, pointHoverRadius:0, borderWidth:1.5, tension:0.35, cubicInterpolationMode:'monotone', fill:false },
		{ label:'Мощность потребления', data:cons, borderColor:'#f39c12', backgroundColor:'#f39c12',
		  pointRadius:0, pointHoverRadius:0, borderWidth:1.5, tension:0.35, cubicInterpolationMode:'monotone', fill:false },
		{ label:'Мощность счётчика (активная)', data:meter, borderColor:'#ffffff', backgroundColor:'#ffffff',
		  pointRadius:0, pointHoverRadius:0, borderWidth:1.5, tension:0.2, cubicInterpolationMode:'monotone', fill:false }
	];
	renderChart('gridPChart', datasets, chartOpts(true,'W'));
	return window.gridPChart;
}
async function loadGridPChart(){
	var url='/api/series?from='+encodeURIComponent(selRange.from.toISOString())+'&to='+encodeURIComponent(selRange.to.toISOString());
	try{
		var r=await fetch(url);
		if(!r.ok) return;
		var data=await r.json();
		document.getElementById('gridPChartRange').textContent='Диапазон: '+fmt(selRange.from)+' — '+fmt(selRange.to);
		buildGridPChart(data);
	}catch(e){}
}
document.getElementById('btnGridPReset').addEventListener('click',function(){ if(window.gridPChart) window.gridPChart.resetZoom(); });

// ---------- Кнопки выбора периода ----------
document.getElementById('btnToday').addEventListener('click',function(){ setPeriod(startOfToday(), endOfToday(), 'day', 'btnToday'); });
document.getElementById('btnYesterday').addEventListener('click',function(){ var y=startOfYesterday(); setPeriod(y, endOfDay(y), 'day', 'btnYesterday'); });
document.getElementById('btn7d').addEventListener('click',function(){
	var to=new Date(); var from=new Date(); from.setDate(from.getDate()-7);
	setPeriod(from, to, 'week', 'btn7d');
});
document.getElementById('btnMonth').addEventListener('click',function(){
	var f=new Date(); f.setHours(0,0,0,0);
	setPeriod(startOfMonthOf(f), endOfMonthOf(f), 'month', 'btnMonth');
});
document.getElementById('btnDate').addEventListener('click',function(){
	var el=document.getElementById('datePick');
	if(!el.value) return;
	var from=dayFromStr(el.value);
	setPeriod(from, endOfDay(from), 'day', null);
});
document.getElementById('btnPeriodPrev').addEventListener('click',function(){ shiftPeriod(-1); });
document.getElementById('btnPeriodNext').addEventListener('click',function(){ shiftPeriod(1); });
document.getElementById('btnRange').addEventListener('click',function(){
	var f=document.getElementById('fromPick'), t=document.getElementById('toPick');
	if(!f.value||!t.value) return;
	var from=dayFromStr(f.value), to=dayFromStr(t.value); to.setHours(23,59,59,999);
	setPeriod(from, to, 'custom', null);
});
document.getElementById('btnRefresh').addEventListener('click',function(){
	preserveZoom=false;
	loadAll();
});

document.getElementById('fromPick').value=toInputDate(dayStart(selRange.from));
document.getElementById('toPick').value=toInputDate(dayStart(selRange.to));
document.getElementById('datePick').value=toInputDate(selRange.from);
loadAll(); setInterval(function(){ preserveZoom=true; loadAll(); },60000);
</script>
</body>
</html>`

var chartsTmpl = template.Must(template.New("charts").Parse(chartsPage))

// energyPage — страница «Электроэнергия»: посуточные и помесячные тарифы
// электросчётчика (потребление/отдача «День»/«Ночь») с независимыми
// диапазонами отображения для каждого графика.
const energyPage = `<!DOCTYPE html>
<html lang="ru">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Электроэнергия — SunReceiver</title>
<script src="https://cdn.jsdelivr.net/npm/chart.js@4.4.1/dist/chart.umd.min.js"></script>
<style>
:root { color-scheme: dark; }
* { box-sizing: border-box; }
body { font-family: -apple-system, "Segoe UI", Roboto, sans-serif; background:#0f1115; color:#e6e6e6; margin:0; padding:20px; overflow-x:hidden; }
.top-nav { display:flex; align-items:center; justify-content:space-between; gap:12px; flex-wrap:wrap; margin-bottom:20px; }
.top-nav .ttl { margin:0; font-size:22px; }
.top-nav .sub { color:#8a93a1; margin:4px 0 0; font-size:13px; }
.nav-btn { background:#2f6fed; color:#fff; border:none; border-radius:8px; padding:9px 16px; font-size:14px; font-weight:600; cursor:pointer; text-decoration:none; white-space:nowrap; }
.nav-btn.secondary { background:#252b36; border:1px solid #333b49; }
#chartbox { background:#181c24; border:1px solid #252b36; border-radius:10px; padding:16px; margin-bottom:20px; max-width:100%; }
#chartbox h2 { margin:0 0 8px; font-size:16px; }
.chart-toolbar { display:flex; align-items:center; flex-wrap:wrap; gap:10px 12px; margin-bottom:8px; font-size:13px; color:#8a93a1; }
.range-panel { display:flex; align-items:center; flex-wrap:wrap; gap:8px; margin-bottom:10px; font-size:13px; color:#8a93a1; }
.range-panel button { background:#252b36; color:#e6e6e6; border:1px solid #333b49; border-radius:6px; padding:4px 10px; cursor:pointer; font-size:13px; }
.range-panel button:hover { background:#2f3644; }
.range-panel button.active { background:#2f6fed; border-color:#2f6fed; color:#fff; }
.range-panel input[type=date] { background:#181c24; color:#e6e6e6; border:1px solid #333b49; border-radius:6px; padding:4px 8px; font-size:13px; color-scheme:dark; }
.range-panel input[type=date]:focus { outline:none; border-color:#2f6fed; }
.chart-wrap { position:relative; height:360px; }
.range-status { color:#ffa94d; font-style:italic; }
</style>
</head>
<body>
<div class="top-nav">
  <div>
    <h1 class="ttl">Электроэнергия</h1>
    <p class="sub">Счётчик DDS238: потребление/отдача по тарифам «День»/«Ночь»</p>
  </div>
  <a class="nav-btn secondary" href="/">&larr; Назад</a>
</div>

<div id="chartbox">
  <h2>Потребление / отдача по тарифу «День» и «Ночь» по дням, kWh</h2>
  <div class="range-panel" id="r1">
    <button id="d1Month">Текущий месяц</button>
    <button id="d1PrevMonth">Прошлый месяц</button>
    <button id="d1Year">Текущий год</button>
    <button id="d1Week">7 дней</button>
    <button id="d1Days30">30 дней</button>
    <span style="color:#555">С</span>
    <input type="date" id="d1From">
    <span style="color:#555">по</span>
    <input type="date" id="d1To">
    <button id="d1Apply">Показать</button>
  </div>
  <div class="chart-toolbar"><span class="range-status" id="s1"></span></div>
  <div class="chart-wrap"><canvas id="dailyTariffChart"></canvas></div>
</div>

<div id="chartbox">
  <h2>Потребление / отдача по тарифу «День» и «Ночь» по месяцам, kWh</h2>
  <div class="range-panel" id="r2">
    <button id="d2Year">Текущий год</button>
    <button id="d2PrevYear">Прошлый год</button>
    <button id="d2Month">Текущий месяц</button>
    <span style="color:#555">С</span>
    <input type="date" id="d2From">
    <span style="color:#555">по</span>
    <input type="date" id="d2To">
    <button id="d2Apply">Показать</button>
  </div>
  <div class="chart-toolbar"><span class="range-status" id="s2"></span></div>
  <div class="chart-wrap"><canvas id="monthlyTariffChart"></canvas></div>
</div>

<script>
'use strict';
var TARIFF_COLORS={ import_day:'#d0663a', import_night:'#8c5bbf', export_day:'#3fbf7f', export_night:'#2c8f6a' };

function toD(d){ function p(x){return (x<10?'0':'')+x;} return d.getFullYear()+'-'+p(d.getMonth()+1)+'-'+p(d.getDate()); }
function dayStart(d){ var r=new Date(d); r.setHours(0,0,0,0); return r; }
function parseDay(s){ var p=String(s).split('-').map(Number); return new Date(p[0],p[1]-1,p[2],0,0,0,0); }
function endOfDay(d){ var e=new Date(d); e.setHours(23,59,59,999); return e; }
function startOfMonth(){ var d=new Date(); return new Date(d.getFullYear(),d.getMonth(),1,0,0,0,0); }
function endOfMonth(){ var d=new Date(); return new Date(d.getFullYear(),d.getMonth()+1,0,23,59,59,999); }
function startOfYear(){ var d=new Date(); return new Date(d.getFullYear(),0,1,0,0,0,0); }
function endOfYear(){ var d=new Date(); return new Date(d.getFullYear(),11,31,23,59,59,999); }

function renderEnergyChart(canvasId, labels, datasets){
	var canvas=document.getElementById(canvasId);
	var old=window[canvasId]; if(old){ try{ old.destroy(); }catch(e){} }
	canvas.getContext('2d');
	window[canvasId]=new Chart(canvas,{
		type:'bar',
		data:{ labels:labels, datasets:datasets },
		options:{
			responsive:true, maintainAspectRatio:false,
			interaction:{ mode:'index', intersect:false },
			animation:{ duration:300 },
			plugins:{ legend:{ display:true, labels:{ boxWidth:16, padding:12 } } },
			scales:{ x:{ ticks:{ autoSkip:true, maxTicksLimit:24 } }, y:{ beginAtZero:true, title:{ display:true, text:'kWh' } } }
		}
	});
	return window[canvasId];
}

function dailyDatasets(days){
	return [
		{ label:'Потребление день', data:days.map(function(d){ return d.import_day; }), backgroundColor:TARIFF_COLORS.import_day },
		{ label:'Потребление ночь', data:days.map(function(d){ return d.import_night; }), backgroundColor:TARIFF_COLORS.import_night },
		{ label:'Отдача день', data:days.map(function(d){ return d.export_day; }), backgroundColor:TARIFF_COLORS.export_day },
		{ label:'Отдача ночь', data:days.map(function(d){ return d.export_night; }), backgroundColor:TARIFF_COLORS.export_night }
	];
}

function monthlyDatasets(days){
	var m={};
	days.forEach(function(d){
		var k=d.day.slice(0,7);
		if(!m[k]) m[k]={ import_day:0, import_night:0, export_day:0, export_night:0 };
		m[k].import_day+=d.import_day; m[k].import_night+=d.import_night;
		m[k].export_day+=d.export_day; m[k].export_night+=d.export_night;
	});
	var keys=Object.keys(m).sort();
	return { labels:keys, datasets:[
		{ label:'Потребление день', data:keys.map(function(k){ return m[k].import_day; }), backgroundColor:TARIFF_COLORS.import_day },
		{ label:'Потребление ночь', data:keys.map(function(k){ return m[k].import_night; }), backgroundColor:TARIFF_COLORS.import_night },
		{ label:'Отдача день', data:keys.map(function(k){ return m[k].export_day; }), backgroundColor:TARIFF_COLORS.export_day },
		{ label:'Отдача ночь', data:keys.map(function(k){ return m[k].export_night; }), backgroundColor:TARIFF_COLORS.export_night }
	] };
}

// initEnergyPanel создаёт независимо управляемый график со своим диапазоном.
// cfg: { canvasId, statusId, fromEl, toEl, applyBtn, isMonthly, presets:[{btn,range}], defaultFrom, defaultTo }
function initEnergyPanel(cfg){
	var p={ selFrom:cfg.defaultFrom(), selTo:cfg.defaultTo() };
	var presetIds=cfg.presets.map(function(pr){ return pr.btn; });
	function setRange(from,to,activeBtn){
		p.selFrom=from; p.selTo=to;
		presetIds.forEach(function(id){ document.getElementById(id).classList.remove('active'); });
		if(activeBtn) document.getElementById(activeBtn).classList.add('active');
		document.getElementById(cfg.fromEl).value=toD(dayStart(from));
		document.getElementById(cfg.toEl).value=toD(dayStart(to));
		load();
	}
	async function load(){
		var url='/api/tariffs?from='+toD(dayStart(p.selFrom))+'&to='+toD(dayStart(p.selTo));
		var r=await fetch(url); if(!r.ok) return;
		var data=await r.json();
		var days=data.days||[];
		var st=document.getElementById(cfg.statusId);
		if(!days.length){ st.textContent='Нет финализированных дней за выбранный период'; }
		else{ st.textContent='Показано дней: '+days.length+(cfg.isMonthly?' (по месяцам)':''); }
		if(cfg.isMonthly){
			var mx=monthlyDatasets(days);
			renderEnergyChart(cfg.canvasId, mx.labels, mx.datasets);
		}else{
			renderEnergyChart(cfg.canvasId, days.map(function(d){ return d.day; }), dailyDatasets(days));
		}
	}
	cfg.presets.forEach(function(pr){
		document.getElementById(pr.btn).addEventListener('click',function(){
			document.getElementById(pr.btn).blur();
			var r=pr.range(); setRange(r.from, r.to, pr.btn);
		});
	});
	document.getElementById(cfg.applyBtn).addEventListener('click',function(){
		var f=document.getElementById(cfg.fromEl).value, t=document.getElementById(cfg.toEl).value;
		if(!f||!t) return;
		setRange(parseDay(f), endOfDay(parseDay(t)), null);
	});
	// Инициализация диапазона и полей по умолчанию.
	document.getElementById(cfg.fromEl).value=toD(dayStart(p.selFrom));
	document.getElementById(cfg.toEl).value=toD(dayStart(p.selTo));
	load();
	return p;
}

// График 1 — по дням, по умолчанию текущий месяц.
initEnergyPanel({
	canvasId:'dailyTariffChart', statusId:'s1', fromEl:'d1From', toEl:'d1To', applyBtn:'d1Apply', isMonthly:false,
	presets:[
		{ btn:'d1Month', range:function(){ return { from:startOfMonth(), to:endOfMonth() }; } },
		{ btn:'d1PrevMonth', range:function(){ var d=new Date(); return { from:new Date(d.getFullYear(),d.getMonth()-1,1,0,0,0,0), to:new Date(d.getFullYear(),d.getMonth(),0,23,59,59,999) }; } },
		{ btn:'d1Year', range:function(){ return { from:startOfYear(), to:endOfYear() }; } },
		{ btn:'d1Week', range:function(){ var to=new Date(); var from=new Date(); from.setDate(from.getDate()-6); from.setHours(0,0,0,0); return { from:from, to:endOfDay(to) }; } },
		{ btn:'d1Days30', range:function(){ var to=new Date(); var from=new Date(); from.setDate(from.getDate()-29); from.setHours(0,0,0,0); return { from:from, to:endOfDay(to) }; } }
	],
	defaultFrom:startOfMonth, defaultTo:endOfMonth
});

// График 2 — по месяцам, по умолчанию текущий год.
initEnergyPanel({
	canvasId:'monthlyTariffChart', statusId:'s2', fromEl:'d2From', toEl:'d2To', applyBtn:'d2Apply', isMonthly:true,
	presets:[
		{ btn:'d2Year', range:function(){ return { from:startOfYear(), to:endOfYear() }; } },
		{ btn:'d2PrevYear', range:function(){ var y=new Date().getFullYear()-1; return { from:new Date(y,0,1,0,0,0,0), to:new Date(y,11,31,23,59,59,999) }; } },
		{ btn:'d2Month', range:function(){ return { from:startOfMonth(), to:endOfMonth() }; } }
	],
	defaultFrom:startOfYear, defaultTo:endOfYear
});
</script>
</body>
</html>`

var energyTmpl = template.Must(template.New("energy").Parse(energyPage))

func (h *dashboardHandler) charts(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = chartsTmpl.Execute(w, nil)
}

func (h *dashboardHandler) energy(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = energyTmpl.Execute(w, nil)
}

var dashboardTmpl = template.Must(template.New("dash").Parse(dashboardPage))

func (h *dashboardHandler) index(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = dashboardTmpl.Execute(w, nil)
}

// meterTariffToday вычисляет тарифные величины счётчика за текущие календарные
// сутки [00:00, now): потребление/отдачу «День» и «Ночь» (kWh). День = 07:00–23:00,
// ночь = 00:00–07:00 и 23:00–24:00. Используются актуальные показания (impNow/expNow,
// из Redis) и фиксированные границы дня (b, из pg.daily_tariffs). Границы, которые
// ещё не наступили или не захвачены, отсутствуют (nil) — расчёт строится только из
// доступных показаний; величины приводятся к ≥0 (сброс счётчика игнорируется).
func meterTariffToday(now time.Time, impNow, expNow float64, b *meterBoundaryRow) (impDay, impNight, expDay, expNight float64) {
	hour := now.In(time.Local).Hour()
	// Ночная зона [00:00, 07:00): весь прирост с начала суток — ночь.
	if hour < meterDayStartH {
		if b.Import0000 != nil {
			impNight = impNow - *b.Import0000
		}
		if b.Export0000 != nil {
			expNight = expNow - *b.Export0000
		}
		return 0, max0f(impNight), 0, max0f(expNight)
	}
	// Дневная зона [07:00, 23:00): ночь уже сформирована [00:00,07:00], день растёт от 07:00.
	if hour < meterDayEndH {
		if b.Import0000 != nil && b.Import0700 != nil {
			impNight = *b.Import0700 - *b.Import0000
		}
		if b.Export0000 != nil && b.Export0700 != nil {
			expNight = *b.Export0700 - *b.Export0000
		}
		if b.Import0700 != nil {
			impDay = impNow - *b.Import0700
		} else if b.Import0000 != nil {
			impDay = impNow - *b.Import0000
		}
		if b.Export0700 != nil {
			expDay = expNow - *b.Export0700
		} else if b.Export0000 != nil {
			expDay = expNow - *b.Export0000
		}
		return max0f(impDay), max0f(impNight), max0f(expDay), max0f(expNight)
	}
	// Ночная зона [23:00, 24:00): день полон [07:00,23:00], ночь = [00:00,07:00] + [23:00,now].
	if b.Import0700 != nil && b.Import2300 != nil {
		impDay = *b.Import2300 - *b.Import0700
	}
	if b.Export0700 != nil && b.Export2300 != nil {
		expDay = *b.Export2300 - *b.Export0700
	}
	if b.Import0000 != nil && b.Import0700 != nil {
		impNight = *b.Import0700 - *b.Import0000
	}
	if b.Export0000 != nil && b.Export0700 != nil {
		expNight = *b.Export0700 - *b.Export0000
	}
	if b.Import2300 != nil {
		impNight += impNow - *b.Import2300
	}
	if b.Export2300 != nil {
		expNight += expNow - *b.Export2300
	}
	return max0f(impDay), max0f(impNight), max0f(expDay), max0f(expNight)
}

// max0f возвращает v, если v ≥ 0, иначе 0 (отрицательные разности при сбросе счётчика).
func max0f(v float64) float64 {
	if v < 0 {
		return 0
	}
	return v
}

func (h *dashboardHandler) apiCurrent(w http.ResponseWriter, r *http.Request) {
	devices, err := h.store.Current()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	sort.SliceStable(devices, func(i, j int) bool {
		// Сначала сетевые инверторы, MPPT-контроллеры — последними, внутри — по имени.
		mi, mj := isMPPTKey(devices[i].IP), isMPPTKey(devices[j].IP)
		if mi != mj {
			return !mi
		}
		return devices[i].Name < devices[j].Name
	})
	var total float64
	var totalPV float64
	var gridV, gridP, batV, batP float64
	for _, d := range devices {
		// Мощности устройства МАП (батарея/сеть) в сумме по инверторам не участвуют:
		// они отображаются только на плашках/графиках МАП.
		if isMAPDevice(d.Values) {
			if v, ok := snapFloat(d.Values, "grid_voltage"); ok {
				gridV = v
			}
			if v, ok := snapFloat(d.Values, "grid_power"); ok {
				gridP = v
			}
			if v, ok := snapFloat(d.Values, "battery_voltage"); ok {
				batV = v
			}
			if v, ok := snapFloat(d.Values, "battery_power"); ok {
				batP = v
			}
			continue
		}
		if v, ok := snapFloat(d.Values, "ac_active_power"); ok {
			total += v
		}
		if v, ok := snapFloat(d.Values, "pv1_power"); ok {
			totalPV += v
		}
		if v, ok := snapFloat(d.Values, "pv2_power"); ok {
			totalPV += v
		}
	}
	total = math.Round(total*10) / 10
	totalPV = math.Round(totalPV*10) / 10
	gridV = math.Round(gridV*10) / 10
	gridP = math.Round(gridP*10) / 10
	batV = math.Round(batV*10) / 10
	batP = math.Round(batP*10) / 10

	// Расчёт тарифных величин сегодняшнего дня: актуальные показания счётчика
	// (Redis, device с тегами meter_*) и фиксированные граничные (PG daily_tariffs).
	now := time.Now()
	var impNow, expNow float64
	hasImp, hasExp := false, false
	for _, d := range devices {
		if isMeterDevice(d.Values) {
			if v, ok := snapFloat(d.Values, "meter_import"); ok {
				impNow, hasImp = v, true
			}
			if v, ok := snapFloat(d.Values, "meter_export"); ok {
				expNow, hasExp = v, true
			}
			break
		}
	}
	impDay, impNight, expDay, expNight := 0.0, 0.0, 0.0, 0.0
	if h.pg != nil && hasImp && hasExp {
		start, _ := dayBounds(now, time.Local)
		if b, err := h.pg.MeterBoundaryValues(start); err == nil {
			impDay, impNight, expDay, expNight = meterTariffToday(now, impNow, expNow, b)
			impDay, impNight, expDay, expNight = math.Round(impDay*100)/100, math.Round(impNight*100)/100,
				math.Round(expDay*100)/100, math.Round(expNight*100)/100
		} else {
			log.Printf("dashboard: meter tariff today: %v", err)
		}
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(currentResponse{
		GeneratedAt: time.Now().Format(time.RFC3339),
		TotalPower:  total,
		TotalPV:     totalPV,
		MapGridV:    gridV,
		MapGridP:    gridP,
		MapBatV:     batV,
		MapBatP:     batP,
		MapCons:     gridP + batP,
		MeterImportDay: impDay,
		MeterImportNight: impNight,
		MeterExportDay:   expDay,
		MeterExportNight: expNight,
		Devices: devices,
	})
}

// apiSeries отдаёт временные ряды ac_active_power по инверторам за период [from, to].
// По умолчанию (без параметров или при ошибке парсинга) — текущие календарные сутки.
// Часть периода, попадающая в последние 2 календарных суток, читается из Redis
// (полное разрешение), более старая часть — из PostgreSQL (5-минутные средние).
func (h *dashboardHandler) apiSeries(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	loc := time.Local

	from, to := dayBounds(now, loc)
	if v := r.URL.Query().Get("from"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			from = t
		}
	}
	if v := r.URL.Query().Get("to"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			to = t
		}
	}

	snaps, err := h.loadRange(from, to, now)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Группируем по имени инвертора, цвет — по индексу в отсортированном списке.
	// Снимки устройства МАП (батарея/сеть) исключаем — их мощность отображается
	// только на своих графиках МАП, а не на графике активной мощности инверторов.
	byIP := map[string]*deviceSeries{}
	nameToIP := map[string]string{}
	for _, sn := range snaps {
		// Снимки устройства МАП (батарея/сеть) и счётчика DDS238 исключаем — их
		// мощность отображается на своих графиках/плашках, а не на графике активной
		// мощности инверторов. Смотрим дальше: устройство попадает в ряд только если
		// хоть в одном снимке есть ac_active_power (у счётчика его нет).
		if isMAPDevice(sn.Values) {
			continue
		}
		v, hasPower := snapFloat(sn.Values, "ac_active_power")
		if !hasPower {
			continue
		}
		if _, ok := byIP[sn.IP]; !ok {
			byIP[sn.IP] = &deviceSeries{IP: sn.IP, Name: sn.Name}
			nameToIP[sn.Name] = sn.IP
		}
		byIP[sn.IP].Points = append(byIP[sn.IP].Points, seriesPoint{T: sn.Timestamp, V: v})
	}

	names := make([]string, 0, len(byIP))
	for _, ds := range byIP {
		names = append(names, string(ds.Name))
	}
	sort.SliceStable(names, func(i, j int) bool {
		mi, mj := isMPPTKey(nameToIP[names[i]]), isMPPTKey(nameToIP[names[j]])
		if mi != mj {
			return !mi
		}
		return names[i] < names[j]
	})

	res := seriesResponse{
		GeneratedAt: now.Format(time.RFC3339),
		From:        from.Format(time.RFC3339),
		To:          to.Format(time.RFC3339),
		Series:      make([]deviceSeries, 0, len(names)),
	}
	// name → *deviceSeries, O(1) на имя вместо O(N) вложенного обхода.
	nameToSeries := make(map[string]*deviceSeries, len(byIP))
	for _, ds := range byIP {
		nameToSeries[ds.Name] = ds
	}
	for i, n := range names {
		ds := nameToSeries[n]
		ds.Color = seriesPalette[i%len(seriesPalette)]
		res.Series = append(res.Series, *ds)
	}

	// Суммарный ряд: складываем ac_active_power всех инверторов по минутным бакетам.
	// Бакетирование нужно, т.к. инверторы опрашиваются параллельно и времена точек
	// не совпадают точно; окно в 60 с сглаживает расхождение и даёт чистый итог.
	res.Total = sumActive(snaps)

	// Ряды МАП (батарея/сеть) — метрики устройства KindMAP (единственное, отдающее
	// эти теги): напряжение/мощность сети и батареи для графиков дашборда.
	res.MapGridVoltage = singleMetricSeries(snaps, "grid_voltage")
	res.MapGridPower = singleMetricSeries(snaps, "grid_power")
	res.MapBatVoltage = singleMetricSeries(snaps, "battery_voltage")
	res.MapBatPower = singleMetricSeries(snaps, "battery_power")
	res.MapCons = sumSeries(res.MapGridPower, res.MapBatPower)

	// Ряды электросчётчика DDS238: напряжение и активная мощность для наложения
	// на графики напряжений и мощностей (белые линии счётчика). Маркер устройства —
	// наличие meter_voltage, которого нет ни у инверторов, ни у МАП/MPPT.
	res.MeterVoltage = meterSeries(snaps, "meter_voltage")
	res.MeterActivePower = meterSeries(snaps, "meter_active_power")

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(res)
}

// sumActive строит временной ряд суммарной активной мощности всех инверторов.
// Инверторы опрашиваются параллельно и присылают снимки с разным фазовым сдвигом
// относительно границ секундных интервалов, поэтому НЕЛЬЗЯ суммировать только те
// снимки, что попали в один бакет — иначе в каждом бакете часть инверторов молчит,
// и сумма проседает к нулю. Вместо этого на каждую точку берётся последнее известное
// (carry-forward) значение каждого инвертора на этот момент времени и суммируется.
// Получается кусочно-постоянный непрерывный ряд без глубоких провалов; правая точка
// совпадает с суммой последних значений (/api/current total_power).
//
// Окно актуальности carry-forward ограничено: если устройство не присылало снимков
// дольше inverterStaleWindow (напр. ушло офлайн/исчезло), его устаревшая мощность
// перестаёт учитываться в сумме — иначе отключившийся инвертор вечно «выдавал бы»
// последнее значение, и на графике суммарной мощности появлялась бы линия, будто он
// продолжает работать. Окно много больше асинхронного джиттера опроса (~8 с), поэтому
// глубокие провалы из-за фазовых сдвигов не возвращаются.
func sumActive(snaps []deviceSnapshot) []seriesPoint {
	const staleWindow = 20 * time.Minute
	type rec struct {
		ts time.Time
		v  float64
		ip string
	}
	var recs []rec
	for _, sn := range snaps {
		if isMAPDevice(sn.Values) {
			continue
		}
		v, ok := snapFloat(sn.Values, "ac_active_power")
		if !ok {
			continue
		}
		ts, err := time.Parse(time.RFC3339, sn.Timestamp)
		if err != nil {
			continue
		}
		recs = append(recs, rec{ts: ts, v: v, ip: sn.IP})
	}
	sort.Slice(recs, func(i, j int) bool { return recs[i].ts.Before(recs[j].ts) })
	current := map[string]float64{}
	lastSeen := map[string]time.Time{}
	out := make([]seriesPoint, 0, len(recs))
	for _, r := range recs {
		current[r.ip] = r.v
		lastSeen[r.ip] = r.ts
		// Исключаем устройства, замолчавшие дольше окна актуальности: их устаревшая
		// мощность больше не суммируется, пока не появится новый реальный снимок.
		var total float64
		for ip, v := range current {
			if r.ts.Sub(lastSeen[ip]) > staleWindow {
				delete(current, ip)
				delete(lastSeen, ip)
				continue
			}
			total += v
		}
		total = math.Round(total*10) / 10
		if n := len(out); n > 0 && out[n-1].T == r.ts.Format(time.RFC3339) {
			out[n-1].V = total
		} else {
			out = append(out, seriesPoint{T: r.ts.Format(time.RFC3339), V: total})
		}
	}
	return out
}

// sumSeries складывает два временных ряда поэлементно по совпадающей временной
// метке T и возвращает результирующий ряд (потребление = мощность сети + батареи).
// Ряды a и b отсортированы по времени и имеют общие точки (с одного снимка МАП);
// если в одном из рядов нет точки на временную метку другого — она пропускается.
func sumSeries(a, b []seriesPoint) []seriesPoint {
	var out []seriesPoint
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i].T == b[j].T:
			out = append(out, seriesPoint{T: a[i].T, V: math.Round((a[i].V+b[j].V)*10) / 10})
			i++
			j++
		case a[i].T < b[j].T:
			i++
		default:
			j++
		}
	}
	return out
}

// singleMetricSeries собирает временной ряд одного тега по снимкам устройства
// МАП (метрики МАП — grid_voltage, grid_power, battery_voltage, battery_power — есть
// только у kindMAP-устройства). Снимки других устройств (Deye/Sofar/MPPT) отбрасываются:
// фильтр по наличию battery_voltage исключает случайные теги с тем же именем
// (например, grid_power). Точки сортируются по времени; дубли с одинаковым временем
// схлопываются (в пределах окна SaveSnapshotWindow остаётся одна точка).
func singleMetricSeries(snaps []deviceSnapshot, key string) []seriesPoint {
	var pts []seriesPoint
	for _, sn := range snaps {
		if _, isMAP := sn.Values["battery_voltage"]; !isMAP {
			continue
		}
		v, ok := snapFloat(sn.Values, key)
		if !ok {
			continue
		}
		pts = append(pts, seriesPoint{T: sn.Timestamp, V: v})
	}
	sort.Slice(pts, func(i, j int) bool { return pts[i].T < pts[j].T })
	// Схлопываем дубли с одинаковым временем (если есть) — оставляем последний.
	if len(pts) > 1 {
		out := pts[:1]
		for i := 1; i < len(pts); i++ {
			if pts[i].T == out[len(out)-1].T {
				out[len(out)-1] = pts[i]
			} else {
				out = append(out, pts[i])
			}
		}
		pts = out
	}
	return pts
}

// meterSeries собирает временной ряд одного тега счётчика DDS238 по его снимкам
// (снимки инверторов/МАП/MPPT отбрасываются). Маркер устройства — meter_voltage.
// Точки сортируются по времени; дубли с одним временем схлопываются.
func meterSeries(snaps []deviceSnapshot, key string) []seriesPoint {
	var pts []seriesPoint
	for _, sn := range snaps {
		if _, isMeter := sn.Values["meter_voltage"]; !isMeter {
			continue
		}
		v, ok := snapFloat(sn.Values, key)
		if !ok {
			continue
		}
		pts = append(pts, seriesPoint{T: sn.Timestamp, V: v})
	}
	sort.Slice(pts, func(i, j int) bool { return pts[i].T < pts[j].T })
	if len(pts) > 1 {
		out := pts[:1]
		for i := 1; i < len(pts); i++ {
			if pts[i].T == out[len(out)-1].T {
				out[len(out)-1] = pts[i]
			} else {
				out = append(out, pts[i])
			}
		}
		pts = out
	}
	return pts
}

// loadRange возвращает снимки за период [start, end]. Точки старше окна
// последних 2 календарных суток берутся из PostgreSQL (5-минутные средние),
// точки внутри окна — из Redis (полное разрешение). Если PG отключено,
// возвращаются только данные из Redis в пределах окна удержания.
//
// Результат кешируется на rangeCacheTTL: страница графиков делает 4 fetch
// /api/series с одинаковыми from/to за цикл, и все 4 сходятся в один read.
func (h *dashboardHandler) loadRange(start, end time.Time, now time.Time) ([]deviceSnapshot, error) {
	key := fmt.Sprintf("%d|%d", start.UnixNano(), end.UnixNano())
	h.cacheMu.Lock()
	if c, ok := h.cache[key]; ok && now.Sub(c.at) < rangeCacheTTL {
		h.cacheMu.Unlock()
		return c.snaps, nil
	}
	// Чистим устаревшие записи, пока держим блокировку.
	for k, c := range h.cache {
		if now.Sub(c.at) >= rangeCacheTTL {
			delete(h.cache, k)
		}
	}
	h.cacheMu.Unlock()

	snaps, err := h.loadRangeUncached(start, end, now)
	if err != nil {
		return nil, err
	}
	h.cacheMu.Lock()
	if h.cache == nil {
		h.cache = map[string]cachedRange{}
	}
	h.cache[key] = cachedRange{at: now, snaps: snaps}
	h.cacheMu.Unlock()
	return snaps, nil
}

// loadRangeUncached — реальное чтение из PG (старая часть) и Redis (рецентная часть).
func (h *dashboardHandler) loadRangeUncached(start, end time.Time, now time.Time) ([]deviceSnapshot, error) {
	cutoff := recentCutoff(now)
	var all []deviceSnapshot
	// Старая часть периода (до cutoff) — из PostgreSQL.
	if h.pg != nil && start.Before(cutoff) {
		oldEnd := cutoff
		if end.Before(oldEnd) {
			oldEnd = end
		}
		pgSnaps, err := h.pg.Averages(start, oldEnd)
		if err != nil {
			return nil, err
		}
		all = append(all, pgSnaps...)
	}
	// Рецентная часть периода (от cutoff) — из Redis (полное разрешение ~10 с).
	// Если в Redis снимков нет (их потеряли, например при сбое Redis или пока был
	// выключен пулер, а реставрация из PG срабатывает только при полностью пустом
	// Redis) — дополняем окно 5-минутными средними из PG, чтобы график не остался
	// пустым, а показал хотя бы усреднённую историю за период.
	if end.After(cutoff) {
		rStart := start
		if rStart.Before(cutoff) {
			rStart = cutoff
		}
		redisSnaps, err := h.store.QuerySeries(rStart, end)
		if err != nil {
			return nil, err
		}
		if len(redisSnaps) == 0 && h.pg != nil {
			pgSnaps, perr := h.pg.Averages(rStart, end)
			if perr != nil {
				return nil, perr
			}
			all = append(all, pgSnaps...)
		} else {
			all = append(all, redisSnaps...)
		}
	}
	return all, nil
}

// dayBounds возвращает границы текущих календарных суток в зоне loc.
func dayBounds(now time.Time, loc *time.Location) (time.Time, time.Time) {
	y, m, d := now.In(loc).Date()
	start := time.Date(y, m, d, 0, 0, 0, 0, loc)
	end := start.AddDate(0, 0, 1).Add(-time.Nanosecond)
	return start, end
}

// serveDashboard запускает HTTP-сервер дашборда в отдельной горутине.
func serveDashboard(addr string, store *redisStore, pg *pgStore) {
	h := &dashboardHandler{store: store, pg: pg}
	mux := http.NewServeMux()
	mux.HandleFunc("/", h.index)
	mux.HandleFunc("/charts", h.charts)
	mux.HandleFunc("/energy", h.energy)
	mux.HandleFunc("/api/current", h.apiCurrent)
	mux.HandleFunc("/api/series", h.apiSeries)
	mux.HandleFunc("/api/tariffs", h.apiTariffs)
	srv := &http.Server{Addr: addr, Handler: mux}
	log.Printf("dashboard: http://%s/ (графики — http://%s/charts)", addr, addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Printf("dashboard: %v", err)
	}
}

// apiTariffs отдаёт посуточную тарифную статистику счётчика (день/ночь ×
// потребление/отдача) за запрошенный период [from, to] (YYYY-MM-DD), отсортированную
// по дате. Параметры from/to необязательны; если не заданы — берётся текущий
// календарный месяц. Только финализированные дни (полные показания на 00:00/07:00/23:00).
func (h *dashboardHandler) apiTariffs(w http.ResponseWriter, r *http.Request) {
	from, to := parseTariffRange(r.URL.Query().Get("from"), r.URL.Query().Get("to"))
	days := []meterDayStat{}
	if h.pg != nil {
		var err error
		days, err = h.pg.DailyTariffsRange(from, to)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(meterDailyResponse{
		GeneratedAt: time.Now().Format(time.RFC3339),
		From:        from.Format("2006-01-02"),
		To:          to.AddDate(0, 0, -1).Format("2006-01-02"),
		Days:        days,
	})
}

// parseTariffRange разбирает необязательные параметры from/to (YYYY-MM-DD) в диапазон
// [from, to) в локальной зоне. Пустые значения дают текущий календарный месяц.
func parseTariffRange(fromStr, toStr string) (time.Time, time.Time) {
	now := time.Now()
	loc := now.Location()
	defStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, loc)
	defEnd := defStart.AddDate(0, 1, 0)
	if t, err := time.ParseInLocation("2006-01-02", fromStr, loc); err == nil {
		defStart = t
	}
	if t, err := time.ParseInLocation("2006-01-02", toStr, loc); err == nil {
		defEnd = t.AddDate(0, 0, 1)
	}
	if defEnd.Before(defStart) {
		defEnd = defStart.AddDate(0, 1, 0)
	}
	return defStart, defEnd
}

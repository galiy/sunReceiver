package main

import (
	"encoding/json"
	"html/template"
	"log"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"
)

// dashboardHandler — веб-дашборд: отдаёт HTML-страницу и JSON API с текущими
// параметрами и временными рядами всех инверторов. Данные за последние
// 2 календарных суток берутся из Redis (полное разрешение), более старые —
// из PostgreSQL (5-минутные усреднённые точки).
type dashboardHandler struct {
	store *redisStore
	pg    *pgStore
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
	Devices     []deviceSnapshot `json:"devices"`
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
	GeneratedAt    string         `json:"generated_at"`
	From           string         `json:"from"`
	To             string         `json:"to"`
	Series         []deviceSeries `json:"series"`
	Total          []seriesPoint  `json:"total,omitempty"`
	MapGridVoltage []seriesPoint  `json:"map_grid_voltage,omitempty"`
	MapGridPower   []seriesPoint  `json:"map_grid_power,omitempty"`
	MapBatVoltage  []seriesPoint  `json:"map_battery_voltage,omitempty"`
	MapBatPower    []seriesPoint  `json:"map_battery_power,omitempty"`
	MapCons        []seriesPoint  `json:"map_consumption,omitempty"`
}

// seriesPalette — цвета линий инверторов (по индексу после сортировки по имени).
var seriesPalette = []string{
	"#ff6b6b", "#4ecdc4", "#45b7d1", "#f9ca24",
	"#a29bfe", "#fd79a8", "#00b894", "#e17055",
	"#74b9ff", "#55efc4", "#fdcb6e", "#fab1a0",
}

// snapFloat извлекает числовое значение из универсального контракта по ключу.
func snapFloat(v valuesContract, key string) (float64, bool) {
	raw, ok := v[key]
	if !ok {
		return 0, false
	}
	switch n := raw.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}

// isMAPDevice возвращает true, если снимок принадлежит устройству МАП (kindMAP,
// батарея/сеть). Маркер — наличие тега battery_voltage, которого нет у инверторов
// (Deye/Sofar) и MPPT-контроллеров. Мощность МАП учитывается только на своих
// плашках и графиках, а не в сумме по инверторам.
func isMAPDevice(v valuesContract) bool {
	_, ok := v["battery_voltage"]
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
<title>SunReceiver Dashboard</title>
<script src="https://cdn.jsdelivr.net/npm/chart.js@4.4.1/dist/chart.umd.min.js"></script>
<script src="https://cdn.jsdelivr.net/npm/chartjs-adapter-date-fns@3.0.0/dist/chartjs-adapter-date-fns.bundle.min.js"></script>
<script src="https://cdn.jsdelivr.net/npm/chartjs-plugin-zoom@2.0.1/dist/chartjs-plugin-zoom.min.js"></script>
<style>
:root { color-scheme: dark; }
* { box-sizing: border-box; }
body { font-family: -apple-system, "Segoe UI", Roboto, sans-serif; background:#0f1115; color:#e6e6e6; margin:0; padding:20px; overflow-x:hidden; }
h1 { font-size:22px; margin:0 0 4px; }
.sub { color:#8a93a1; margin:0 0 20px; font-size:13px; }
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
.kpi { display:flex; align-items:stretch; gap:16px; margin-bottom:20px; }
.kpi-plate { flex:1; background:linear-gradient(135deg,#1d2430,#202a3a); border:1px solid #2a3342; border-radius:12px; padding:18px 22px; display:flex; flex-direction:column; gap:4px; }
.kpi-label { font-size:12px; color:#8a93a1; text-transform:uppercase; letter-spacing:.06em; }
.kpi-value { font-size:52px; font-weight:700; line-height:1; font-variant-numeric:tabular-nums; }
.kpi-unit { font-size:20px; font-weight:400; color:#8a93a1; margin-left:6px; }
.kpi-sub { font-size:12px; color:#6b7280; }
/* Плашка электросчётчика (самый верх) */
.meter-plate { background:#10161f; border:1px solid #2a3342; border-radius:12px; padding:14px 18px; margin:0 0 16px; }
.meter-head { display:flex; align-items:baseline; justify-content:space-between; gap:12px; margin-bottom:10px; }
.meter-title { font-size:14px; font-weight:700; color:#e6e6e6; }
.meter-ts { font-size:12px; color:#6b7280; }
.meter-stats { display:flex; flex-wrap:wrap; gap:10px; }
.meter-stat { background:#181c24; border:1px solid #252b36; border-radius:8px; padding:8px 14px; min-width:120px; }
.meter-stat .lbl { font-size:11px; color:#8a93a1; text-transform:uppercase; letter-spacing:.04em; }
.meter-stat .val { font-size:20px; font-weight:700; font-variant-numeric:tabular-nums; }
.meter-stat .unit { font-size:12px; color:#8a93a1; font-weight:400; margin-left:4px; }
.meter-stat .pos { color:#6fd08a; } /* положительная величина / потребление */
.meter-stat .neg { color:#ff6b6b; } /* отрицательная величина / отдача в сеть */
.meter-stat .off { color:#ff9f43; } /* нулевое/неопределённое */
.meter-note { font-size:11px; color:#6b7280; margin-top:8px; }
</style>
</head>
<body>
<h1>SunReceiver</h1>
<p class="sub">Текущие параметры инверторов и электросчётчика (из Redis, обновление каждую секунду)</p>

<div class="meter-plate">
  <div class="meter-head">
    <span class="meter-title">Электросчётчик DDS238 &mdash; текущие параметры</span>
    <span class="meter-ts" id="meterTs">&mdash;</span>
  </div>
  <div class="meter-stats" id="meterStats"><span class="missing">Нет данных</span></div>
  <div class="meter-note">Мощность с отрицательным знаком &mdash; отдача в сеть (генерация); положительная &mdash; потребление.</div>
</div>

<div class="kpi">
  <div class="kpi-plate">
    <div class="kpi-label">Суммарная активная мощность</div>
    <div class="kpi-value"><span id="kpiTotal">—</span><span class="kpi-unit">W</span></div>
    <div class="kpi-sub" id="kpiSub">Нет данных</div>
  </div>
  <div class="kpi-plate">
    <div class="kpi-label">Суммарная мощность PV</div>
    <div class="kpi-value"><span id="kpiPV">—</span><span class="kpi-unit">W</span></div>
    <div class="kpi-sub" id="kpiPVSub">Нет данных</div>
  </div>
  <div class="kpi-plate">
    <div class="kpi-label">Напряжение сети</div>
    <div class="kpi-value"><span id="kpiGridV">—</span><span class="kpi-unit">V</span></div>
    <div class="kpi-sub">МАП (батарея/сеть)</div>
  </div>
  <div class="kpi-plate">
    <div class="kpi-label">Мощность сети</div>
    <div class="kpi-value"><span id="kpiGridP">—</span><span class="kpi-unit">W</span></div>
    <div class="kpi-sub">МАП (батарея/сеть)</div>
  </div>
  <div class="kpi-plate">
    <div class="kpi-label">Напряжение батареи</div>
    <div class="kpi-value"><span id="kpiBatV">—</span><span class="kpi-unit">V</span></div>
    <div class="kpi-sub">МАП (батарея/сеть)</div>
  </div>
  <div class="kpi-plate">
    <div class="kpi-label">Мощность батареи</div>
    <div class="kpi-value"><span id="kpiBatP">—</span><span class="kpi-unit">W</span></div>
    <div class="kpi-sub">МАП (батарея/сеть)</div>
  </div>
  <div class="kpi-plate">
    <div class="kpi-label">Мощность потребления</div>
    <div class="kpi-value"><span id="kpiConsP">—</span><span class="kpi-unit">W</span></div>
    <div class="kpi-sub">Сеть + батарея</div>
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

<div class="charts">
  <div id="chartbox">
    <h2>Напряжение сети и батареи (МАП), V</h2>
    <div class="chart-toolbar">
      <span id="gridVChartRange"></span>
      <button id="btnGridVReset">Сброс зума</button>
      <span>Зум: колесо / drag&ndash;панорама</span>
    </div>
    <div class="chart-wrap"><canvas id="gridVChart"></canvas></div>
  </div>

  <div id="chartbox">
    <h2>Мощности сети и батареи (МАП), W</h2>
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

// ---------- Графики ----------
Chart.register(ChartZoom);

// preserveZoom указывает, нужно ли при пересоздании графика сохранить текущую
// видимую область (зум): true при обновлении по таймеру, false при выборе
// периода/кнопке «Обновить» (в этом случае зум сбрасывается).
var preserveZoom=false;
// hoverPix — пиксель по X курсора для каждого графика (по id канваса), когда мышь над ним.
var hoverPix={};
// drawCursorTooltip — рисует вертикальную линию под курсором и подпись значений всех
// линий из ближайшей точки СЛЕВА от курсора. Оборачивается в try/catch, чтобы никогда
// не ломать отрисовку графика.
function drawCursorTooltip(chart){
	try{
		var px=hoverPix[chart.canvas.id];
		if(px===undefined) return;
		var xScale=chart.scales&&chart.scales.x, yScale=chart.scales&&chart.scales.y;
		if(!xScale||!yScale) return;
		if(!isFinite(px)||px<xScale.left||px>xScale.right) return;
		var t=xScale.getValueForPixel(px);
		var ctx=chart.ctx; ctx.save();
		// вертикальная линия под курсором
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
			var v=null;
			for(var i2=0;i2<pts.length;i2++){ var q=pts[i2]; var qX=(q.x instanceof Date)? q.x.getTime() : Number(q.x); if(qX===best){ v=q.y; break; } }
			if(v===null||v===undefined) return;
			var dsy = chart.scales && (chart.scales[ds.yAxisID||'y']) || yScale;
			if(!dsy || !isFinite(v)) return;
			labels.push({
				label:ds.label||'', color:ds.borderColor||'#999', v:v,
				px:xScale.getPixelForValue(best), py:dsy.getPixelForValue(v),
				txt:''
			});
			if(titleT===null||best>titleT) titleT=best;
		});
		if(!labels.length){ ctx.restore(); return; }
		labels.forEach(function(l){ ctx.beginPath(); ctx.arc(l.px,l.py,3.5,0,2*Math.PI); ctx.fillStyle=l.color; ctx.fill(); ctx.strokeStyle='rgba(0,0,0,0.9)'; ctx.lineWidth=1; ctx.stroke(); });
		var boxW=0, lineH=16;
		labels.forEach(function(l){ l.txt=l.label+': '+Number(l.v).toFixed(1)+' W'; });
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
// cursorTooltipPlugin — после каждой отрисовки: (1) если видимое окно времени у этого
// графика изменилось, синхронизирует его на остальных; (2) рисует хинт под курсором.
// Обёрнут в try/catch, чтобы сбой здесь не приводил к пустому графику.
var lastXWindow={};
var cursorTooltipPlugin={ id:'cursorTooltip', afterDraw:function(chart){
	try{
		checkZoomSync(chart);
		drawZeroGridAxis(chart);
		drawCursorTooltip(chart);
	}catch(err){}
} };
// drawZeroGridAxis рисует контрастную горизонтальную ось на уровне y=0 у графиков,
// где это важно (мощности сети/батареи/потребления — значения бывают и отрицательными).
function drawZeroGridAxis(chart){
	if(!chart || chart.canvas.id!=='gridPChart') return;
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
// checkZoomSync сверяет видимое окно времени (X-ось) графика с запомненным; если
// изменилось — применяет его ко всем остальным графикам. Защита zoomSyncing не даёт
// зациклиться при каскадной синхронизации.
function checkZoomSync(chart){
	if(zoomSyncing) return;
	var x=chart&&chart.scales&&chart.scales.x;
	if(!x||!isFinite(x.min)||!isFinite(x.max)||x.max<=x.min) return;
	var key=x.min.toFixed(3)+','+x.max.toFixed(3);
	if(lastXWindow[chart.canvas.id]!==undefined && lastXWindow[chart.canvas.id]!==key) syncZoomToOthers(chart);
	lastXWindow[chart.canvas.id]=key;
}
// syncZoomToOthers применяет видимую область времени (X-ось) графика fromChart ко
// всем остальным графикам через штатный метод плагина zoomScale (регистрирует зум
// во внутреннем состоянии плагина, поэтому он сохраняется). Вызывается из
// checkZoomSync при изменении окна.
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

// renderChart создаёт/пересоздаёт линейный график на канвасе id; old-график уничтожается.
// Инстанс хранится в window[id] (id = id канваса), кнопки сброса/синхронизация зума
// обращаются к нему. Начальную видимую область можно ограничить через opts (см. chartOpts).
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
// Общий диапазон для обоих графиков. По умолчанию — текущие календарные сутки,
// но можно выбрать «Вчера», «7 дней» или конкретный день через date-поле.
var selRange={from:startOfToday(), to:endOfToday()};

// dayFromStr возвращает начало локального дня по строке 'YYYY-MM-DD'.
function dayFromStr(s){
	var p=String(s).split('-').map(Number);
	return new Date(p[0], p[1]-1, p[2], 0,0,0,0);
}
function endOfDay(d){
	var e=new Date(d); e.setHours(23,59,59,999); return e;
}
function startOfYesterday(){
	var d=new Date(); d.setDate(d.getDate()-1); d.setHours(0,0,0,0); return d;
}
// selectRange устанавливает текущий период, подсвечивает активную кнопку и
// перезагружает оба графика.
function selectRange(from,to,activeBtn){
	selRange.from=from; selRange.to=to;
	preserveZoom=false;
	var btns=['btnToday','btnYesterday','btn7d'];
	for(var i=0;i<btns.length;i++) document.getElementById(btns[i]).classList.remove('active');
	if(activeBtn) document.getElementById(activeBtn).classList.add('active');
	loadAll();
}
async function loadAll(){
	await Promise.all([loadTotalChart(), loadChart(), loadGridVChart(), loadGridPChart()]);
}

// Суммарный график (одна линия)
function buildTotalChart(data){
	var pts=(data.total||[]).map(function(p){ return {x:new Date(p.t), y:p.v}; });
	var datasets=[{
		label:'Сумма',
		data:pts,
		borderColor:'#ffd166',
		backgroundColor:'#ffd166',
		pointRadius:0, pointHoverRadius:0,
		borderWidth:1.5,
		tension:0.35,
		cubicInterpolationMode:'monotone',
		fill:false
	}];
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

// График по инверторам (несколько линий)
function buildChart(data){
	var datasets=[];
	for(var i=0;i<data.series.length;i++){
		var s=data.series[i];
		var pts=s.points.map(function(p){ return {x:new Date(p.t), y:p.v}; });
		datasets.push({
			label:s.name,
			data:pts,
			borderColor:s.color,
			backgroundColor:s.color,
			pointRadius:0, pointHoverRadius:0,
			borderWidth:1.5,
			tension:0.35,
			cubicInterpolationMode:'monotone',
			fill:false
		});
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

// Графики МАП: напряжение сети и батареи (на отдельных осях), мощность сети (seriesResponse).
function buildGridVChart(data){
	var grid=(data.map_grid_voltage||[]).map(function(p){ return {x:new Date(p.t), y:p.v}; });
	var bat=(data.map_battery_voltage||[]).map(function(p){ return {x:new Date(p.t), y:p.v}; });
	var datasets=[
		{ label:'Напряжение сети', data:grid, borderColor:'#4ecdc4', backgroundColor:'#4ecdc4',
		  pointRadius:0, pointHoverRadius:0, borderWidth:1.5, tension:0.35, cubicInterpolationMode:'monotone', fill:false },
		{ label:'Напряжение батареи', data:bat, borderColor:'#e74c3c', backgroundColor:'#e74c3c',
		  pointRadius:0, pointHoverRadius:0, borderWidth:1.5, tension:0.35, cubicInterpolationMode:'monotone', fill:false,
		  yAxisID:'y1' }
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

function buildGridPChart(data){
	var grid=(data.map_grid_power||[]).map(function(p){ return {x:new Date(p.t), y:p.v}; });
	var bat=(data.map_battery_power||[]).map(function(p){ return {x:new Date(p.t), y:p.v}; });
	var cons=(data.map_consumption||[]).map(function(p){ return {x:new Date(p.t), y:p.v}; });
	var datasets=[
		{ label:'Мощность сети', data:grid, borderColor:'#74b9ff', backgroundColor:'#74b9ff',
		  pointRadius:0, pointHoverRadius:0, borderWidth:1.5, tension:0.35, cubicInterpolationMode:'monotone', fill:false },
		{ label:'Мощность батареи', data:bat, borderColor:'#00b894', backgroundColor:'#00b894',
		  pointRadius:0, pointHoverRadius:0, borderWidth:1.5, tension:0.35, cubicInterpolationMode:'monotone', fill:false },
		{ label:'Мощность потребления', data:cons, borderColor:'#f39c12', backgroundColor:'#f39c12',
		  pointRadius:0, pointHoverRadius:0, borderWidth:1.5, tension:0.35, cubicInterpolationMode:'monotone', fill:false }
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

// Кнопки выбора периода
document.getElementById('btnToday').addEventListener('click',function(){ selectRange(startOfToday(), endOfToday(), 'btnToday'); });
document.getElementById('btnYesterday').addEventListener('click',function(){ selectRange(startOfYesterday(), endOfDay(startOfYesterday()), 'btnYesterday'); });
document.getElementById('btn7d').addEventListener('click',function(){
	var to=new Date(); var from=new Date(); from.setDate(from.getDate()-7);
	selectRange(from, to, 'btn7d');
});
document.getElementById('btnDate').addEventListener('click',function(){
	var el=document.getElementById('datePick');
	if(!el.value) return;
	var from=dayFromStr(el.value);
	selectRange(from, endOfDay(from), null);
});
document.getElementById('btnRefresh').addEventListener('click',function(){
	preserveZoom=false;
	loadAll();
});

loadAll(); setInterval(function(){ preserveZoom=true; loadAll(); },60000);

tick(); setInterval(tick,1000);
</script>
</body>
</html>`

var dashboardTmpl = template.Must(template.New("dash").Parse(dashboardPage))

func (h *dashboardHandler) index(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = dashboardTmpl.Execute(w, nil)
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
		Devices:     devices,
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
		if isMAPDevice(sn.Values) {
			continue
		}
		if _, ok := byIP[sn.IP]; !ok {
			byIP[sn.IP] = &deviceSeries{IP: sn.IP, Name: sn.Name}
			nameToIP[sn.Name] = sn.IP
		}
		if v, ok := snapFloat(sn.Values, "ac_active_power"); ok {
			byIP[sn.IP].Points = append(byIP[sn.IP].Points, seriesPoint{T: sn.Timestamp, V: v})
		}
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
	for i, n := range names {
		for _, ds := range byIP {
			if ds.Name == n {
				ds.Color = seriesPalette[i%len(seriesPalette)]
				res.Series = append(res.Series, *ds)
				break
			}
		}
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
// Мощность устройства МАП (батарея/сеть) не включается — она на графиках МАП.
func sumActive(snaps []deviceSnapshot) []seriesPoint {
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
	out := make([]seriesPoint, 0, len(recs))
	for _, r := range recs {
		current[r.ip] = r.v
		var total float64
		for _, v := range current {
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

// loadRange возвращает снимки за период [start, end]. Точки старше окна
// последних 2 календарных суток берутся из PostgreSQL (5-минутные средние),
// точки внутри окна — из Redis (полное разрешение). Если PG отключено,
// возвращаются только данные из Redis в пределах окна удержания.
func (h *dashboardHandler) loadRange(start, end time.Time, now time.Time) ([]deviceSnapshot, error) {
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
	// Рецентная часть периода (от cutoff) — из Redis.
	if end.After(cutoff) {
		rStart := start
		if rStart.Before(cutoff) {
			rStart = cutoff
		}
		redisSnaps, err := h.store.QuerySeries(rStart, end)
		if err != nil {
			return nil, err
		}
		all = append(all, redisSnaps...)
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
	mux.HandleFunc("/api/current", h.apiCurrent)
	mux.HandleFunc("/api/series", h.apiSeries)
	srv := &http.Server{Addr: addr, Handler: mux}
	log.Printf("dashboard: http://%s/", addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Printf("dashboard: %v", err)
	}
}

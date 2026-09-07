package main

import (
	"encoding/json"
	"html/template"
	"log"
	"math"
	"net/http"
	"sort"
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
</style>
</head>
<body>
<h1>SunReceiver</h1>
<p class="sub">Текущие параметры инверторов (из Redis, обновление каждую секунду)</p>

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
    <h2>Напряжение сети (МАП), V</h2>
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
			if(u!==undefined && u!==null && i!==undefined && i!==null) v=Number(u)*Number(i);
		}
	}
	return (v === undefined || v === null) ? null : v;
}
function renderPivot(devices){
	if(!devices || !devices.length) return '<div class="missing">No data in Redis</div>';
	// Устройство МАП (батарея/сеть) в сводной таблице не показываем — у него нет
	// солнечных панелей; оно нужно только для верхних плашек и графиков.
	// Идентифицируем по тегу battery_voltage, которого нет у инверторов и MPPT.
	var invs=[];
	for(var i=0;i<devices.length;i++) if(!(devices[i].values && devices[i].values.battery_voltage!==undefined)) invs.push(devices[i]);
	if(!invs.length) return '<div class="missing">No data in Redis</div>';
	var h = '<table class="pivot-table"><thead><tr><th></th>';
	for(var i=0;i<invs.length;i++) h += '<th>'+esc(invs[i].name)+'</th>';
	h += '<th></th></tr></thead><tbody>';
	// Строка актуальности данных: время последнего снимка каждого инвертора.
	h += '<tr><td class="p-label">Актуально</td>';
	for(var d=0;d<invs.length;d++){
		var ts = (invs[d] && invs[d].timestamp) ? invs[d].timestamp : null;
		h += (ts===null)
			? '<td class="p-empty"></td>'
			: '<td class="p-val" style="font-size:11px">'+esc(fmtSec(ts))+'</td>';
	}
	h += '<td class="p-unit"></td></tr>';
	// Строка серийных номеров инверторов и контроллеров.
	h += '<tr><td class="p-label">Серийный номер</td>';
	for(var d=0;d<invs.length;d++){
		var sn = (invs[d] && invs[d].device_sn) ? invs[d].device_sn : null;
		h += (sn===null)
			? '<td class="p-empty"></td>'
			: '<td class="p-val" style="font-size:11px">'+esc(sn)+'</td>';
	}
	h += '<td class="p-unit"></td></tr>';
	for(var p=0;p<PARAMS.length;p++){
		var tag=PARAMS[p][0], label=PARAMS[p][1], unit=PARAMS[p][2];
		h += '<tr><td class="p-label">'+esc(label)+'</td>';
		for(var d=0;d<invs.length;d++){
			var v=devValue(invs[d], tag);
			h += (v===null)
				? '<td class="p-empty"></td>'
				: '<td class="p-val">'+esc(v)+'</td>';
		}
		h += '<td class="p-unit">'+esc(unit)+'</td></tr>';
	}
	h += '</tbody></table>';
	return h;
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
			// Число инверторов онлайн — без устройства МАП (батарея/сеть).
			var invCount=0;
			for(var i=0;i<data.devices.length;i++) if(!(data.devices[i].values && data.devices[i].values.battery_voltage!==undefined)) invCount++;
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

// renderChart создаёт/пересоздаёт линейный график на канвасе id; old-график уничтожается.
function renderChart(id, datasets, opts){
	var canvas=document.getElementById(id);
	var holder=id+'Chart';
	if(window[holder]) window[holder].destroy();
	canvas.getContext('2d');
	window[holder]=new Chart(canvas,{ type:'line', data:{datasets:datasets}, options:opts });
	return window[holder];
}
function chartOpts(withLegend,yTitle){
	var o={
		responsive:true,
		maintainAspectRatio:false,
		interaction:{ mode:'index', intersect:false },
		animation:{ duration:300 },
		plugins:{
			tooltip:{
				mode:'index', intersect:false, displayColors:true,
				callbacks:{
					title:function(items){ return items.length ? fmtSec(items[0].parsed.x) : ''; },
					label:function(item){ return item.dataset.label + ': ' + Number(item.parsed.y).toFixed(1) + ' W'; }
				}
			},
			zoom:{
				pan:{ enabled:true, mode:'x' },
				zoom:{ wheel:{ enabled:true, speed:0.1, modifierKey:'ctrl' }, pinch:{ enabled:true }, mode:'x' },
				limits:{ x:{ minRange: 60*1000 } },
				onPanComplete:onViewChange, onZoomComplete:onViewChange
			}
		},
		scales:{
			x:{ type:'time', time:{ unit:'hour', displayFormats:{ hour:'HH:mm' }, tooltipFormat:'yyyy-MM-dd HH:mm:ss' }, ticks:{ maxRotation:0, autoSkipPadding:20 } },
			y:{ beginAtZero:true, title:{ display:yTitle, text:yTitle||'' } }
		}
	};
	if(withLegend){ o.plugins.legend={ display:true, labels:{ boxWidth:20, padding:14 } }; }
	return o;
}

// ---------- Выбор периода ----------
// Общий диапазон для обоих графиков. По умолчанию — текущие календарные сутки,
// но можно выбрать «Вчера», «7 дней» или конкретный день через date-поле.
var selRange={from:startOfToday(), to:endOfToday()};
// expandedRange возвращает период с запасом по обе стороны, чтобы панорама и зум
// могли уходить за границы выбранного диапазона (например, в прошлое за начало суток).
// Запас = половина ширины видимого периода, но не меньше 12 часов с каждой стороны.
function expandedRange(){
	var from=selRange.from.getTime(), to=selRange.to.getTime();
	var pad=Math.round((to-from)/2); if(pad<12*3600*1000) pad=12*3600*1000;
	return {from:new Date(from-pad), to:new Date(to+pad)};
}
// loadedRange — фактически загруженный диапазон (для определения, когда нужна подгрузка).
var loadedRange=null;
// lastView — последняя видимая область по данным интерактивных панорам/зумов.
var lastView={min:null,max:null};
var reloading=false;
// onViewChange отслеживает смещение/сужение видимой области, расширяя lastView и проверяя,
// не нужно ли догрузить данные за новой границей (перекрытие начала суток при панораме влево).
function onViewChange(chart){
	var x=chart.scales&&chart.scales.x;
	if(!x||!isFinite(x.min)||!isFinite(x.max)) return;
	if(lastView.min===null||x.min<lastView.min) lastView.min=x.min;
	if(lastView.max===null||x.max>lastView.max) lastView.max=x.max;
	maybeReload();
}
// restoreView принудительно выставляет видимую область на всех графиках после подгрузки.
function restoreView(min,max){
	['powerChart','totalChart','gridVChart','gridPChart'].forEach(function(k){
		var c=window[k];
		if(c){ c.options.scales.x.min=min; c.options.scales.x.max=max; c.update('none'); }
	});
}
// maybeReload: если видимая область вышла за загруженный диапазон — перезагружает данные
// вокруг нового центра и возвращает графики обратно на видимую позицию.
async function maybeReload(){
	if(reloading||!lastView.min||!lastView.max) return;
	var from=new Date(lastView.min), to=new Date(lastView.max);
	var pad=(to-from)/2; if(pad<12*3600*1000) pad=12*3600*1000;
	var needL=from.getTime()-pad, needR=to.getTime()+pad;
	if(loadedRange && needL>=loadedRange.from && needR<=loadedRange.to) return;
	reloading=true;
	var view={min:lastView.min,max:lastView.max};
	selRange.from=from; selRange.to=to;
	try{ await loadAll(); }catch(e){}
	restoreView(view.min,view.max);
	reloading=false;
}

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
	loadedRange=null; lastView={min:null,max:null}; reloading=false;
	var btns=['btnToday','btnYesterday','btn7d'];
	for(var i=0;i<btns.length;i++) document.getElementById(btns[i]).classList.remove('active');
	if(activeBtn) document.getElementById(activeBtn).classList.add('active');
	loadAll();
}
async function loadAll(){
	var exp=expandedRange();
	loadedRange={from:exp.from.getTime(), to:exp.to.getTime()};
	await Promise.all([loadTotalChart(exp), loadChart(exp), loadGridVChart(exp), loadGridPChart(exp)]);
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
async function loadTotalChart(exp){
	exp = exp || expandedRange();
	var url='/api/series?from='+encodeURIComponent(exp.from.toISOString())+'&to='+encodeURIComponent(exp.to.toISOString());
	try{
		var r=await fetch(url);
		if(!r.ok) return;
		var data=await r.json();
		document.getElementById('totalChartRange').textContent='Диапазон: '+fmt(data.from)+' — '+fmt(data.to);
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
async function loadChart(exp){
	exp = exp || expandedRange();
	var url='/api/series?from='+encodeURIComponent(exp.from.toISOString())+'&to='+encodeURIComponent(exp.to.toISOString());
	try{
		var r=await fetch(url);
		if(!r.ok) return;
		var data=await r.json();
		document.getElementById('chartRange').textContent='Диапазон: '+fmt(data.from)+' — '+fmt(data.to);
		buildChart(data);
	}catch(e){}
}
document.getElementById('btnReset').addEventListener('click',function(){ if(window.powerChart) window.powerChart.resetZoom(); });

// Графики МАП: напряжение сети и мощность сети (одна линия из seriesResponse).
function buildGridVChart(data){
	var pts=(data.map_grid_voltage||[]).map(function(p){ return {x:new Date(p.t), y:p.v}; });
	var datasets=[{
		label:'Напряжение сети',
		data:pts,
		borderColor:'#4ecdc4',
		backgroundColor:'#4ecdc4',
		pointRadius:0, pointHoverRadius:0,
		borderWidth:1.5, tension:0.35,
		cubicInterpolationMode:'monotone', fill:false
	}];
	renderChart('gridVChart', datasets, chartOpts(false,'V'));
	return window.gridVChart;
}
async function loadGridVChart(exp){
	exp = exp || expandedRange();
	var url='/api/series?from='+encodeURIComponent(exp.from.toISOString())+'&to='+encodeURIComponent(exp.to.toISOString());
	try{
		var r=await fetch(url);
		if(!r.ok) return;
		var data=await r.json();
		document.getElementById('gridVChartRange').textContent='Диапазон: '+fmt(data.from)+' — '+fmt(data.to);
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
async function loadGridPChart(exp){
	exp = exp || expandedRange();
	var url='/api/series?from='+encodeURIComponent(exp.from.toISOString())+'&to='+encodeURIComponent(exp.to.toISOString());
	try{
		var r=await fetch(url);
		if(!r.ok) return;
		var data=await r.json();
		document.getElementById('gridPChartRange').textContent='Диапазон: '+fmt(data.from)+' — '+fmt(data.to);
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
	loadAll();
});

loadAll(); setInterval(loadAll,60000);

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
	sort.SliceStable(devices, func(i, j int) bool { return devices[i].Name < devices[j].Name })
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
	ipToName := map[string]string{}
	for _, sn := range snaps {
		if isMAPDevice(sn.Values) {
			continue
		}
		if _, ok := byIP[sn.IP]; !ok {
			byIP[sn.IP] = &deviceSeries{IP: sn.IP, Name: sn.Name}
			ipToName[sn.IP] = sn.Name
		}
		if v, ok := snapFloat(sn.Values, "ac_active_power"); ok {
			byIP[sn.IP].Points = append(byIP[sn.IP].Points, seriesPoint{T: sn.Timestamp, V: v})
		}
	}

	names := make([]string, 0, len(byIP))
	for _, ds := range byIP {
		names = append(names, string(ds.Name))
	}
	sort.Strings(names)

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

// sumActive агрегирует ac_active_power всех инверторов в бакеты, равные периоду
// опроса (pollPeriod), и возвращает точки суммарной мощности с дискретностью,
// соответствующей частоте опроса. Внутри одного бакета инверторы опрашиваются
// параллельными горутинами и могут дать несколько снимков, поэтому для каждого
// инвертора берётся его среднее значение по бакету, и только потом эти средние
// складываются — иначе каждое подряд-чтение завышало бы сумму в 2 и более раз.
// Последняя точка приравнивается к сумме последних известных значений по каждому
// инвертору — так правый край графика совпадает с суммарной мощностью на цифровой
// плашке (/api/current total_power).
func sumActive(snaps []deviceSnapshot) []seriesPoint {
	step := int64(pollPeriod / time.Second) // бакет = период опроса
	// bucketSum[IP][bidx] — сумма и число снимков инвертора в бакете.
	type perInv struct {
		sum, count float64
	}
	type bucket struct {
		invs  map[string]*perInv
		order []string
	}
	buckets := map[int64]*bucket{}
	var order []int64
	type last struct {
		v float64
		t time.Time
	}
	latest := map[string]last{} // последнее значение по каждому инвертору
	var latestEnd time.Time
	for _, sn := range snaps {
		// Мощность устройства МАП (батарея/сеть) в суммарный график инверторов
		// не включаем — она отображается только на графиках МАП.
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
		if prev, ok := latest[sn.IP]; !ok || ts.After(prev.t) {
			latest[sn.IP] = last{v: v, t: ts}
		}
		if ts.After(latestEnd) {
			latestEnd = ts
		}
		bidx := ts.Unix() / step
		b, ok := buckets[bidx]
		if !ok {
			b = &bucket{invs: map[string]*perInv{}}
			buckets[bidx] = b
			order = append(order, bidx)
		}
		pi, ok := b.invs[sn.IP]
		if !ok {
			pi = &perInv{}
			b.invs[sn.IP] = pi
			b.order = append(b.order, sn.IP)
		}
		pi.sum += v
		pi.count++
	}
	sort.Slice(order, func(i, j int) bool { return order[i] < order[j] })
	out := make([]seriesPoint, 0, len(order))
	for _, bidx := range order {
		b := buckets[bidx]
		var total float64
		for _, ip := range b.order {
			pi := b.invs[ip]
			if pi.count == 0 {
				continue
			}
			total += pi.sum / pi.count // среднее по снимкам инвертора в бакете
		}
		out = append(out, seriesPoint{
			T: time.Unix(bidx*step, 0).Format(time.RFC3339),
			V: math.Round(total*10) / 10,
		})
	}
	// Последняя точка = сумма последних известных значений по инверторам (как плашка).
	if len(latest) > 0 && !latestEnd.IsZero() {
		var total float64
		for _, lp := range latest {
			total += lp.v
		}
		pt := seriesPoint{T: latestEnd.Format(time.RFC3339), V: math.Round(total*10) / 10}
		if len(out) > 0 {
			out[len(out)-1] = pt
		} else {
			out = append(out, pt)
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

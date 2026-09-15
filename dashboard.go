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

// tariffCacheTTL — срок жизни кеша тарифных исходников (границ текущего дня и
// сумм финализированных дней месяца/года). Главная страница обновляется каждую
// секунду, но эти данные в PG меняются редко: границы сегодня — 3 раза в сутки,
// набор финализированных дней — раз в сутки. Поэтому 60-секундный кеш снимает
// 3 запроса к PG/сек (7200/мин) до ~1 раза в минуту без заметной задержки.
const tariffCacheTTL = 60 * time.Second

// dashboardHandler — веб-дашборд: отдаёт три HTML-страницы и JSON API.
//   - Главная страница (/) — текущие параметры: плашки, электросчётчик, сводная
//     таблица; обновляются каждую секунду из Redis.
//   - Страница графиков (/charts) — временные ряды инверторов, МАП и счётчика за
//     выбранный период (Redis полное разрешение за 2 суток + PG 5-минутные средние).
//   - Страница электроэнергии (/energy) — посуточные и помесячные тарифы счётчика
//     (потребление/отдача «День»/«Ночь») из daily_tariffs с независимыми диапазонами.
type dashboardHandler struct {
	store *redisStore
	pg    *pgStore

	// Кэш loadRange: 4 одинаковых запроса /api/series за цикл сойдутся в один
	// read из Redis/PG. Ключ — от (start, end).
	cacheMu sync.Mutex
	cache   map[string]cachedRange

	// Кэш тарифных исходников /api/current (TTL tariffCacheTTL): границы текущего
	// дня и суммы финализированных прошедших дней месяца/года. Главная страница
	// опрашивает /api/current каждую секунду, но эти данные в PG меняются редко
	// (границы — 3 раза/сутки, набор финализированных дней — раз/сутки), поэтому
	// кеш снижает фоновые запросы к PG с 3/сек до ~1/мин. Текущий день и живые
	// показания счётчика пересчитываются в каждом запросе отдельно.
	tariffMu sync.Mutex
	tariffAt time.Time
	tariffB  *meterBoundaryRow // границы текущего дня (00:00/07:00/23:00)
	tariffM  [4]float64        // месяц: [importDay, importNight, exportDay, exportNight]
	tariffY  [4]float64        // год:  [importDay, importNight, exportDay, exportNight]
}

// cachedRange — кешированный результат loadRange.
type cachedRange struct {
	at    time.Time
	snaps []deviceSnapshot
}

// currentResponse отвечает на GET /api/current.
type currentResponse struct {
	GeneratedAt string  `json:"generated_at"`
	TotalPower  float64 `json:"total_power"`
	TotalPV     float64 `json:"total_pv"`
	MapGridV    float64 `json:"map_grid_voltage"`
	MapGridP    float64 `json:"map_grid_power"`
	MapBatV     float64 `json:"map_battery_voltage"`
	MapBatP     float64 `json:"map_battery_power"`
	MapCons     float64 `json:"map_consumption"`
	// Расчётные тарифные величины счётчика за текущие календарные сутки (kWh):
	// потребление/отдача «День»/«Ночь», посчитанные из актуальных показаний
	// счётчика (Redis) и фиксированных граничных показаний (PG daily_tariffs).
	MeterImportDay   float64 `json:"meter_import_day"`
	MeterImportNight float64 `json:"meter_import_night"`
	MeterExportDay   float64 `json:"meter_export_day"`
	MeterExportNight float64 `json:"meter_export_night"`
	// То же за текущий месяц (MM.YYYY) и текущий год (YYYY): сумма финализированных
	// дней периода + незавершённый сегодняшний день.
	MeterImportDayMonth   float64          `json:"meter_import_day_month"`
	MeterImportNightMonth float64          `json:"meter_import_night_month"`
	MeterExportDayMonth   float64          `json:"meter_export_day_month"`
	MeterExportNightMonth float64          `json:"meter_export_night_month"`
	MeterImportDayYear    float64          `json:"meter_import_day_year"`
	MeterImportNightYear  float64          `json:"meter_import_night_year"`
	MeterExportDayYear    float64          `json:"meter_export_day_year"`
	MeterExportNightYear  float64          `json:"meter_export_night_year"`
	Devices               []deviceSnapshot `json:"devices"`
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
	Day         string  `json:"day"` // YYYY-MM-DD
	ImportDay   float64 `json:"import_day"`
	ImportNight float64 `json:"import_night"`
	ExportDay   float64 `json:"export_day"`
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

// mobileCommon — общие части мобильной (тачскрин) версии всех страниц:
//   - {{mcss}} — иная раскладка при ширине ≤900px (стекированные группы, 2-колоночные
//     плашки, нижняя навигация, увеличенные touch-цели, горизонтальный скролл сводной
//     таблицы с закреплённой первой колонкой);
//   - {{mjs}} — touch-управление: чипы легенды (tap — вкл/выкл, long tap — только эта
//     линия) и графики (щипок двумя пальцами — зум, горизонтальный свайп — панорама,
//     двойной тап — сброс зума, тап — значение в точке); работает только на coarse-устройствах;
//   - {{mnav}} — нижняя навигация (Главная/Графики/Электроэнергия), активный пункт
//     передаётся в Execute через .active.
const mobileCommon = `
{{define "mcss"}}
@media (max-width: 900px) {
  html { -webkit-text-size-adjust: 100%; }
  body { padding:12px 12px calc(16px + 56px + env(safe-area-inset-bottom, 0px)); }
  .top-nav { margin-bottom:12px; }
  .top-nav .ttl { font-size:19px; }
  .top-nav .sub { font-size:12px; margin-bottom:0; }
  .top-nav .nav-btn { display:none; }
  .mnav { position:fixed; left:0; right:0; bottom:0; z-index:60; display:flex; background:#151b26; border-top:1px solid #2a3342; padding-bottom:env(safe-area-inset-bottom, 0px); }
  .mnav a { flex:1 1 0; text-align:center; padding:9px 2px 10px; color:#8a93a1; font-size:12px; font-weight:600; text-decoration:none; border-top:2px solid transparent; -webkit-tap-highlight-color:transparent; }
  .mnav a.active { color:#fff; border-top-color:#2f6fed; background:rgba(47,111,237,0.10); }
  button, .lg-chip, .bms-btn { -webkit-tap-highlight-color:transparent; -webkit-user-select:none; user-select:none; -webkit-touch-callout:none; }
  .groups-row { flex-direction:column; gap:12px; margin-bottom:12px; }
  .group-top { margin-bottom:12px; }
  .group { padding:12px; }
  .group-body .plate, .group-body .meter-stat { flex:1 1 44%; min-width:44%; }
  .plate .val, .meter-stat .val { font-size:21px; }
  .bms-btn { min-width:120px; padding:12px 14px 10px; }
  .bms-batt { width:44px; height:96px; border-width:2px; }
  .bms-batt::before { top:-8px; width:20px; height:6px; }
  .bms-batt-soc { font-size:19px; }
  .bms-name { font-size:13px; }
  .pivot-wrap { margin:12px 0; padding:10px; -webkit-overflow-scrolling:touch; }
  .pivot-table { min-width:600px; font-size:12px; }
  .pivot-table th, .pivot-table td { padding:5px 8px; }
  .pivot-table th:first-child, .pivot-table td:first-child { position:sticky; left:0; z-index:2; background:#202630; box-shadow:1px 0 0 0 #333b49; }
  .pivot-table tbody tr:nth-child(even) td:first-child { background:#1b212b; }
  .charts { gap:12px; margin-bottom:12px; }
  .charts #chartbox { min-width:100%; }
  #chartbox { padding:12px; }
  #chartbox h2 { font-size:14px; }
  .chart-wrap { height:290px; }
  .period-panel { gap:8px; margin-bottom:12px; }
  .period-panel button, .range-panel button, .chart-toolbar button { min-height:40px; padding:9px 14px; font-size:13px; }
  .period-panel input[type=date], .range-panel input[type=date] { min-height:40px; padding:8px 10px; font-size:14px; }
  .lg-chips { gap:8px; }
  .lg-chip { padding:8px 14px 8px 10px; font-size:13px; }
  .lg-hint { margin-bottom:8px; }
  .kpi-row { gap:8px; }
  .kpi { flex:1 1 46%; min-width:150px; padding:10px 12px; }
  .kpi .val { font-size:22px; }
  .cards { gap:12px; }
  .card { flex:1 1 100%; min-width:0; padding:12px; }
  .trow .tname { flex:0 0 110px; width:110px; font-size:11px; }
  .tnote { font-size:10px; }
  .bms-charts { gap:12px; margin-top:16px; }
  .bms-charts-title { margin:20px 0 10px; font-size:15px; }
  .bms-charts .chart-wrap { height:250px; }
  .cell { width:34px; }
  .cell-batt { width:24px; height:48px; }
  .cell .mv { font-size:10px; }
  .foot { font-size:11px; }
}
{{end}}
{{define "mjs"}}
(function(){
'use strict';
var SR_COARSE = window.matchMedia && matchMedia('(pointer: coarse)').matches;
if(!SR_COARSE) return;
// Чипы легенды: tap — вкл/выкл, long tap (≥450 мс без движения) — только эта линия.
// Вызывается из lgKit (каждой страницы) при создании чипа.
window.srBindChip = function(b, fnToggle, fnIsolate){
  var timer=null, sx=0, sy=0, moved=false;
  b.addEventListener('touchstart', function(e){
    if(e.touches.length!==1) return;
    var t=e.touches[0]; sx=t.clientX; sy=t.clientY; moved=false;
    timer=setTimeout(function(){ timer=null; fnIsolate(); }, 450);
  }, {passive:true});
  b.addEventListener('touchmove', function(e){
    if(!timer) return;
    var t=e.touches[0];
    if(Math.abs(t.clientX-sx)>14 || Math.abs(t.clientY-sy)>14){ moved=true; clearTimeout(timer); timer=null; }
  }, {passive:true});
  b.addEventListener('touchend', function(){
    if(!timer) return;
    clearTimeout(timer); timer=null;
    if(!moved) fnToggle();
  }, {passive:true});
  b.addEventListener('touchcancel', function(){ if(timer){ clearTimeout(timer); timer=null; } }, {passive:true});
  // Синтетический click после touchend не должен дойти до десктопного обработчика чипа.
  b.addEventListener('click', function(e){ e.preventDefault(); e.stopPropagation(); }, true);
};
// Touch-управление графиком:
//   - горизонтальный свайп одним пальцем — панорама по времени (вертикальный скролл
//     страницы не блокируется: touch-action: pan-y);
//   - щипок двумя пальцами — зум/у-зум (окно зафиксировано под серединой пальцев);
//   - двойной тап — сброс зума;
//   - тап — onTap(px) (показать значение в точке).
// Плагин chartjs-plugin-zoom на touch-устройствах отключён (pan/wheel/pinch),
// чтобы его Hammer не перехватывал жесты и не мешал скроллу страницы.
window.srTouchChart = function(getChart, canvasId, minSpan, onTap){
  var canvas=document.getElementById(canvasId);
  if(!canvas) return;
  canvas.style.touchAction='pan-y';
  var st=null, lastTap={t:0, px:0};
  function tdist(e){ var a=e.touches[0], b=e.touches[1]; return Math.hypot(a.clientX-b.clientX, a.clientY-b.clientY); }
  function xwin(c){ var x=c.scales && c.scales.x; return (x && isFinite(x.min) && isFinite(x.max) && x.max>x.min) ? {min:x.min, max:x.max} : null; }
  function beginPinch(c, e){
    var w=xwin(c); if(!w || !c.chartArea){ st=null; return; }
    var rect=canvas.getBoundingClientRect();
    var midX=(e.touches[0].clientX+e.touches[1].clientX)/2;
    var fx=(midX-rect.left-c.chartArea.left)/c.chartArea.width;
    st={mode:'pinch', d0:Math.max(20,tdist(e)), min0:w.min, max0:w.max, fx:Math.max(0,Math.min(1,fx))};
  }
  function doPinch(c, e){
    if(!st || st.mode!=='pinch') return;
    var span0=st.max0-st.min0;
    var span=span0*(st.d0/Math.max(20,tdist(e)));
    span=Math.max(minSpan, Math.min(span0, span));
    var mid0=st.min0+st.fx*span0;
    try{ c.zoomScale('x', {min:mid0-st.fx*span, max:mid0-st.fx*span+span}, 'none'); }catch(err){}
  }
  canvas.addEventListener('touchstart', function(e){
    var c=getChart(); if(!c){ st=null; return; }
    if(e.touches.length===2){ beginPinch(c, e); }
    else if(e.touches.length===1){
      st={mode:'maybe', x0:e.touches[0].clientX, y0:e.touches[0].clientY, lastX:e.touches[0].clientX, t:Date.now(), decided:null};
    }
  }, {passive:true});
  canvas.addEventListener('touchmove', function(e){
    if(!st) return;
    var c=getChart(); if(!c) return;
    if(e.touches.length===2){
      if(st.mode!=='pinch') beginPinch(c, e);
      e.preventDefault();
      doPinch(c, e);
    } else if(e.touches.length===1 && st.mode==='maybe'){
      var dx=e.touches[0].clientX-st.x0, dy=e.touches[0].clientY-st.y0;
      if(st.decided===null && (Math.abs(dx)>12 || Math.abs(dy)>12)) st.decided=(Math.abs(dx)>Math.abs(dy)*1.2)?'pan':'scroll';
      if(st.decided==='pan'){
        e.preventDefault();
        var w=xwin(c);
        if(w && c.chartArea){
          var dData=-(e.touches[0].clientX-st.lastX)/c.chartArea.width*(w.max-w.min);
          try{ c.zoomScale('x', {min:w.min+dData, max:w.max+dData}, 'none'); }catch(err){}
        }
        st.lastX=e.touches[0].clientX;
      }
    }
  }, {passive:false});
  canvas.addEventListener('touchend', function(e){
    if(!st || st.mode!=='maybe' || st.decided==='pan'){ st=null; return; }
    var dt=Date.now()-st.t;
    var t=e.changedTouches[0];
    if(dt<300 && t){
      var rect=canvas.getBoundingClientRect();
      var px=t.clientX-rect.left;
      var c=getChart();
      if(c && c.chartArea && px>=c.chartArea.left && px<=c.chartArea.right){
        if(Date.now()-lastTap.t<320 && Math.abs(px-lastTap.px)<40){
          try{ c.resetZoom(); }catch(err){}
        } else if(onTap){ onTap(px); }
        lastTap={t:Date.now(), px:px};
      }
    }
    st=null;
  }, {passive:true});
  canvas.addEventListener('touchcancel', function(){ st=null; }, {passive:true});
};
// Подсказки: мышиные инструкции заменяем на тачские.
document.querySelectorAll('.lg-hint').forEach(function(s){
  s.textContent='Тап по чипу — вкл/выкл · long tap — только этот показатель · «Все» — показать все';
});
document.querySelectorAll('.chart-toolbar span').forEach(function(s){
  if(!s.id && /зум/i.test(s.textContent)) s.textContent='Щипок — зум · свайп — сдвиг · двойной тап — сброс';
});
})();
{{end}}
{{define "mnav"}}
<div class="mnav">
  <a href="/" {{if eq .active "home"}}class="active"{{end}}>Главная</a>
  <a href="/charts" {{if eq .active "charts"}}class="active"{{end}}>Графики</a>
  <a href="/energy" {{if eq .active "energy"}}class="active"{{end}}>Электроэнергия</a>
</div>
{{end}}
`

const dashboardPage = `<!DOCTYPE html>
<html lang="ru">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1, maximum-scale=1, user-scalable=no">
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
.group-top { margin-bottom:16px; }
.group-head { display:flex; align-items:baseline; justify-content:space-between; gap:12px; margin-bottom:10px; }
.group-title { font-size:14px; font-weight:700; color:#e6e6e6; margin-bottom:10px; }
.group-head .group-title { margin-bottom:0; }
.group-body { display:flex; flex-wrap:wrap; gap:10px; }
/* Рамки «Потребление/Отдача» (сегодня/месяц/год): плашки в 2 ряда × 2 колонки */
.group.tariff-grid .group-body { display:grid; grid-template-columns:1fr 1fr; grid-template-rows:1fr 1fr; gap:10px; }
.group.tariff-grid .plate { min-width:0; }
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
/* BMS (ANT батарея) — крупные кнопки-батарейки (высота ~4 см): заполнение =
   остаточный заряд (SOC). */
.bms-btn { flex:0 0 auto; display:flex; flex-direction:column; align-items:center; gap:10px; background:linear-gradient(135deg,#1d2430,#202a3a); border:1px solid #2a3342; border-radius:14px; padding:18px 22px 14px; text-decoration:none; min-width:170px; }
.bms-btn:hover { border-color:#2f6fed; }
.bms-batt { position:relative; width:56px; height:150px; border:3px solid #8a93a1; border-radius:11px; background:#10141b; }
.bms-batt::before { content:''; position:absolute; top:-9px; left:50%; transform:translateX(-50%); width:26px; height:7px; background:#8a93a1; border-radius:3px 3px 0 0; }
.bms-batt-fill { position:absolute; left:4px; right:4px; bottom:4px; border-radius:6px; transition:height .6s; }
.bms-batt-soc { position:absolute; left:0; right:0; top:50%; transform:translateY(-50%); text-align:center; font-size:26px; font-weight:700; color:#fff; text-shadow:0 1px 4px rgba(0,0,0,.9); font-variant-numeric:tabular-nums; }
.bms-name { font-size:15px; font-weight:600; color:#e6e6e6; white-space:nowrap; }
/* Мобильная версия: панель периодов на главной — безделка (графиков на главной нет). */
@media (max-width: 900px) { #mainPeriod { display:none; } }
</style>
</head>
<body>
<style>{{template "mcss"}}</style>
<div class="top-nav">
  <div>
    <h1 class="ttl">SunReceiver</h1>
    <p class="sub">Текущие параметры инверторов и электросчётчика (из Redis, обновление каждую секунду)</p>
  </div>
  <a class="nav-btn" href="/charts">Открыть графики</a>
  <a class="nav-btn" href="/energy">Электроэнергия</a>
</div>

<div class="group group-top">
  <div class="group-head">
    <span class="group-title">BMS (ANT батарея)</span>
    <span class="meter-ts">обновление раз в минуту</span>
  </div>
  <div class="group-body" id="bmsList"><span class="missing">Загрузка...</span></div>
</div>

<div class="group group-top">
  <div class="group-head">
    <span class="group-title">Электросчётчик DDS238 &mdash; текущие параметры</span>
    <span class="meter-ts" id="meterTs">&mdash;</span>
  </div>
  <div class="group-body" id="meterStats"><span class="missing">Нет данных</span></div>
  <div class="meter-note">Мощность с отрицательным знаком &mdash; отдача в сеть (генерация); положительная &mdash; потребление.</div>
</div>

<div class="groups-row">
  <div class="group tariff-grid">
    <div class="group-title">Потребление/Отдача за сегодня</div>
    <div class="group-body">
      <div class="plate">
        <div class="lbl">Потребление день</div>
        <div class="val"><span id="kpiImpDay">—</span><span class="unit">kWh</span></div>
        <div class="sub">День 07:00–23:00</div>
      </div>
      <div class="plate">
        <div class="lbl">Отдача день</div>
        <div class="val"><span id="kpiExpDay">—</span><span class="unit">kWh</span></div>
        <div class="sub">День 07:00–23:00</div>
      </div>
      <div class="plate">
        <div class="lbl">Потребление ночь</div>
        <div class="val"><span id="kpiImpNight">—</span><span class="unit">kWh</span></div>
        <div class="sub">Ночь 23:00–07:00</div>
      </div>
      <div class="plate">
        <div class="lbl">Отдача ночь</div>
        <div class="val"><span id="kpiExpNight">—</span><span class="unit">kWh</span></div>
        <div class="sub">Ночь 23:00–07:00</div>
      </div>
    </div>
  </div>
  <div class="group tariff-grid">
    <div class="group-title" id="tariffMonthTitle">Потребление/Отдача за месяц</div>
    <div class="group-body">
      <div class="plate">
        <div class="lbl">Потребление день</div>
        <div class="val"><span id="kpiImpDayM">—</span><span class="unit">kWh</span></div>
        <div class="sub">День 07:00–23:00</div>
      </div>
      <div class="plate">
        <div class="lbl">Отдача день</div>
        <div class="val"><span id="kpiExpDayM">—</span><span class="unit">kWh</span></div>
        <div class="sub">День 07:00–23:00</div>
      </div>
      <div class="plate">
        <div class="lbl">Потребление ночь</div>
        <div class="val"><span id="kpiImpNightM">—</span><span class="unit">kWh</span></div>
        <div class="sub">Ночь 23:00–07:00</div>
      </div>
      <div class="plate">
        <div class="lbl">Отдача ночь</div>
        <div class="val"><span id="kpiExpNightM">—</span><span class="unit">kWh</span></div>
        <div class="sub">Ночь 23:00–07:00</div>
      </div>
    </div>
  </div>
  <div class="group tariff-grid">
    <div class="group-title" id="tariffYearTitle">Потребление/Отдача за год</div>
    <div class="group-body">
      <div class="plate">
        <div class="lbl">Потребление день</div>
        <div class="val"><span id="kpiImpDayY">—</span><span class="unit">kWh</span></div>
        <div class="sub">День 07:00–23:00</div>
      </div>
      <div class="plate">
        <div class="lbl">Отдача день</div>
        <div class="val"><span id="kpiExpDayY">—</span><span class="unit">kWh</span></div>
        <div class="sub">День 07:00–23:00</div>
      </div>
      <div class="plate">
        <div class="lbl">Потребление ночь</div>
        <div class="val"><span id="kpiImpNightY">—</span><span class="unit">kWh</span></div>
        <div class="sub">Ночь 23:00–07:00</div>
      </div>
      <div class="plate">
        <div class="lbl">Отдача ночь</div>
        <div class="val"><span id="kpiExpNightY">—</span><span class="unit">kWh</span></div>
        <div class="sub">Ночь 23:00–07:00</div>
      </div>
    </div>
  </div>
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
</div>

<div class="period-panel" id="mainPeriod">
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
	h+='<tr><td class="p-label">Серийный номер инвертора</td>'+rowCells(grid,mpts,function(d){return d.inverter_sn||null;},'11px')+'</tr>';
	h+='<tr><td class="p-label">Серийный номер логгера</td>'+rowCells(grid,mpts,function(d){return d.device_sn||null;},'11px')+'</tr>';
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
		// Плашки «Потребление/Отдача за месяц/год» (kWh).
		setKpi2('kpiImpDayM', data.meter_import_day_month);
		setKpi2('kpiImpNightM', data.meter_import_night_month);
		setKpi2('kpiExpDayM', data.meter_export_day_month);
		setKpi2('kpiExpNightM', data.meter_export_night_month);
		setKpi2('kpiImpDayY', data.meter_import_day_year);
		setKpi2('kpiImpNightY', data.meter_import_night_year);
		setKpi2('kpiExpDayY', data.meter_export_day_year);
		setKpi2('kpiExpNightY', data.meter_export_night_year);
		// Заголовки рамок: «Потребление/Отдача за MM.YYYY» и «... за YYYY год».
		var now=new Date();
		function p2(x){ return (x<10?'0':'')+x; }
		document.getElementById('tariffMonthTitle').textContent='Потребление/Отдача за '+p2(now.getMonth()+1)+'.'+now.getFullYear();
		document.getElementById('tariffYearTitle').textContent='Потребление/Отдача за '+now.getFullYear()+' год';
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

// ---------- BMS (ANT батарея) ----------
// Кнопки-батарейки: заполнение = остаточный заряд (SOC), клик — страница
// деталей батареи (/bms/<name>). Обновление раз в минуту (отдельно от
// 1-секундного tick главной страницы).
function bmsSocColor(soc){ return soc<20?'#ff6b6b':(soc<50?'#ff9f43':(soc<80?'#f9ca24':'#00b894')); }
async function tickBMS(){
  try{
    var r=await fetch('/api/bms');
    if(!r.ok) return;
    var data=await r.json();
    var list=data.bms||[];
    var el=document.getElementById('bmsList');
    if(!list.length){ el.innerHTML='<span class="missing">BMS не найдены (или опрос отключён)</span>'; return; }
    var h='';
    for(var i=0;i<list.length;i++){
      var d=list[i];
      var soc=Math.max(0,Math.min(100,Number(d.soc)||0));
      h+='<a class="bms-btn" href="/bms/'+encodeURIComponent(d.deviceName)+'" title="Порт: '+esc(d.port)+'">'
        +'<div class="bms-batt">'
        +'<div class="bms-batt-fill" style="height:'+Math.max(4,soc)+'%;background:'+bmsSocColor(soc)+'"></div>'
        +'<span class="bms-batt-soc">'+soc+'%</span>'
        +'</div>'
        +'<div class="bms-name">'+esc(d.deviceName)+'</div>'
        +'</a>';
    }
    el.innerHTML=h;
  }catch(e){}
}
tick(); setInterval(tick,1000);
tickBMS(); setInterval(tickBMS,60000);
</script>
{{template "mnav" .}}
<script>{{template "mjs"}}</script>
</body>
</html>`

// chartsPage — страница графиков: временные ряды инверторов/МАП/счётчика за
// выбранный период (Redis полное разрешение за 2 суток + PG 5-минутные средние).
const chartsPage = `<!DOCTYPE html>
<html lang="ru">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1, maximum-scale=1, user-scalable=no">
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
.lg-chips { display:flex; flex-wrap:wrap; gap:6px; margin:2px 0 10px; }
.lg-chip { display:inline-flex; align-items:center; gap:6px; background:#202630; border:1px solid #333b49; border-radius:14px; padding:3px 11px 3px 8px; font-size:12px; color:#e6e6e6; cursor:pointer; line-height:1.4; }
.lg-chip:hover { border-color:#2f6fed; }
.lg-chip i { width:10px; height:10px; border-radius:3px; display:inline-block; flex:0 0 auto; }
.lg-chip.off { opacity:.4; }
.lg-chip.off i { background:#5a6472 !important; }
.lg-chip.lg-all { background:#2f6fed; border-color:#2f6fed; }
.lg-hint { font-size:11px; color:#6b7280; margin:0 0 10px; }
.missing { color:#6b7280; font-style:italic; }
</style>
</head>
<body>
<style>{{template "mcss"}}</style>
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

<div class="lg-hint">ЛКМ по чипу — вкл/выкл линию · двойной ЛКМ — только эта линия · «Все» — показать все</div>
<div class="charts">
  <div id="chartbox">
    <h2>Напряжение сети и батареи (МАП), V + напряжение счётчика</h2>
    <div class="chart-toolbar">
      <span id="gridVChartRange"></span>
      <button id="btnGridVReset">Сброс зума</button>
      <span>Зум: колесо / drag&ndash;панорама</span>
    </div>
    <div class="lg-chips" id="gridVChartLg"></div>
    <div class="chart-wrap"><canvas id="gridVChart"></canvas></div>
  </div>

  <div id="chartbox">
    <h2>Мощности сети и батареи (МАП), W + активная мощность счётчика</h2>
    <div class="chart-toolbar">
      <span id="gridPChartRange"></span>
      <button id="btnGridPReset">Сброс зума</button>
      <span>Зум: колесо / drag&ndash;панорама</span>
    </div>
    <div class="lg-chips" id="gridPChartLg"></div>
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
    <div class="lg-chips" id="powerChartLg"></div>
    <div class="chart-wrap"><canvas id="powerChart"></canvas></div>
  </div>
</div>

<script>
'use strict';
var SR_COARSE = window.matchMedia ? matchMedia('(pointer: coarse)').matches : false;

function fmt(t){ var d=new Date(t); function p(x){return (x<10?'0':'')+x;} return d.getFullYear()+'-'+p(d.getMonth()+1)+'-'+p(d.getDate())+' '+p(d.getHours())+':'+p(d.getMinutes()); }
function fmtSec(t){ var d=new Date(t); function p(x){return (x<10?'0':'')+x;} return d.getFullYear()+'-'+p(d.getMonth()+1)+'-'+p(d.getDate())+' '+p(d.getHours())+':'+p(d.getMinutes())+':'+p(d.getSeconds()); }
function startOfToday(){ var d=new Date(); d.setHours(0,0,0,0); return d; }
function endOfToday(){ var d=new Date(); d.setHours(23,59,59,999); return d; }

// ---------- Графики ----------
Chart.register(ChartZoom);

var preserveZoom=false;
var hoverPix={};
// hiddenSets[chartId] — метки датасетов, которые пользователь скрыл кликом по
// легенде. При пересоздании графика (обновление по таймеру) выбор восстанавливается,
// чтобы отключённые линии не «восставали» сами по себе.
var hiddenSets={};
function captureHidden(id, oldChart){
	if(!oldChart || !oldChart.data || !oldChart.data.datasets) return;
	var hidden=[];
	oldChart.data.datasets.forEach(function(ds,i){ if(!oldChart.isDatasetVisible(i)) hidden.push(ds.label); });
	hiddenSets[id]=hidden;
}
// applyHidden скрывает в новом графике те датасеты, что были скрыты ранее.
// Возвращает true, если применил (тогда нужен update).
function applyHidden(id, chart){
	var hidden=hiddenSets[id]||[];
	if(!hidden.length || !chart || !chart.data || !chart.data.datasets) return false;
	chart.data.datasets.forEach(function(ds,i){ if(hidden.indexOf(ds.label)>=0) chart.setDatasetVisibility(i,false); });
	return true;
}
// lgKit — HTML-легенда-чипы вместо встроенной легенды Chart.js. ЛКМ по чипу —
// вкл/выкл эту линию (toggle); двойной ЛКМ — показать только эту линию; «Все» —
// показать все. Чистый DOM: без модификаторов (на Mac Ctrl/Cmd-клик перехватывается
// браузером) и без приватного API Chart.js. Состояние чипов синхронизировано с
// видимостью датасетов (работает с captureHidden/applyHidden).
function lgKit(canvasId, chipsId){
	var canvas=document.getElementById(canvasId), row=document.getElementById(chipsId);
	function swatch(ds){ return ds.borderColor || ds.backgroundColor || '#888'; }
	function sync(chart){
		var chips=row.querySelectorAll('.lg-chip:not(.lg-all)');
		chart.data.datasets.forEach(function(ds,i){ if(chips[i]) chips[i].classList.toggle('off', !chart.isDatasetVisible(i)); });
	}
	function isolate(chart,i){ chart.data.datasets.forEach(function(ds,j){ chart.setDatasetVisibility(j, j===i); }); chart.update(); sync(chart); }
	function toggle(chart,i){ chart.setDatasetVisibility(i, !chart.isDatasetVisible(i)); chart.update(); sync(chart); }
	function showAll(chart){ chart.data.datasets.forEach(function(ds,j){ chart.setDatasetVisibility(j, true); }); chart.update(); sync(chart); }
	function build(chart){
		row.innerHTML='';
		if(!chart.data.datasets || !chart.data.datasets.length) return;
		chart.data.datasets.forEach(function(ds,i){
			var b=document.createElement('button'); b.type='button'; b.className='lg-chip';
			b.title='ЛКМ — вкл/выкл · двойной ЛКМ — только эта линия';
			var sw=document.createElement('i'); sw.style.background=swatch(ds);
			b.appendChild(sw); b.appendChild(document.createTextNode(ds.label));
			if(window.srBindChip){
				srBindChip(b, function(){ toggle(chart,i); }, function(){ isolate(chart,i); });
			}else{
				var clickTimer=null;
				b.addEventListener('click', function(){
					if(clickTimer){ clearTimeout(clickTimer); clickTimer=null; isolate(chart,i); }
					else { clickTimer=setTimeout(function(){ clickTimer=null; toggle(chart,i); }, 250); }
				});
			}
			row.appendChild(b);
		});
		var all=document.createElement('button'); all.type='button'; all.className='lg-chip lg-all';
		all.textContent='Все'; all.title='Показать все линии';
		all.addEventListener('click', function(){ showAll(chart); });
		row.appendChild(all);
		sync(chart);
	}
	return { build: build };
}
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
	if(old){ captureHidden(id, old); try{ old.destroy(); }catch(e){} }
	canvas.getContext('2d');
	window[id]=new Chart(canvas,{ type:'line', data:{datasets:datasets}, options:opts, plugins:[cursorTooltipPlugin] });
	canvas.addEventListener('mouseleave',function(){ delete hoverPix[id]; try{ window[id]&&window[id].update('none'); }catch(e){} });
	var needUpdate=false;
	if(preserveZoom && saved.min!==null && saved.max!==null){
		window[id].options.scales.x.min=saved.min; window[id].options.scales.x.max=saved.max;
		needUpdate=true;
	}
	if(applyHidden(id, window[id])) needUpdate=true;
	if(needUpdate){ window[id].update('none'); }
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
				pan:{ enabled:!SR_COARSE, mode:'x' },
				zoom:{ wheel:{ enabled:!SR_COARSE, speed:0.1, modifierKey:'ctrl' }, pinch:{ enabled:!SR_COARSE }, mode:'x' },
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
	if(withLegend){ o.plugins.legend={ display:false }; }
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
	lgKit('powerChart','powerChartLg').build(window.powerChart);
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
	lgKit('gridVChart','gridVChartLg').build(window.gridVChart);
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
	lgKit('gridPChart','gridPChartLg').build(window.gridPChart);
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
// Мобильная версия: touch-жесты по графикам (щипок — зум, свайп — панорама,
// двойной тап — сброс зума, тап — значение в точке через cursorTooltip).
if(SR_COARSE){
	['powerChart','totalChart','gridVChart','gridPChart'].forEach(function(id){
		srTouchChart(function(){ return window[id]; }, id, 60*1000, function(px){
			hoverPix[id]=px;
			try{ if(window[id]) window[id].update('none'); }catch(e){}
		});
	});
}
</script>
{{template "mnav" .}}
<script>{{template "mjs"}}</script>
</body>
</html>`

var chartsTmpl = template.Must(template.New("charts").Parse(mobileCommon + chartsPage))

// energyPage — страница «Электроэнергия»: посуточные и помесячные тарифы
// электросчётчика (потребление/отдача «День»/«Ночь») с независимыми
// диапазонами отображения для каждого графика.
const energyPage = `<!DOCTYPE html>
<html lang="ru">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1, maximum-scale=1, user-scalable=no">
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
.lg-chips { display:flex; flex-wrap:wrap; gap:6px; margin:2px 0 10px; }
.lg-chip { display:inline-flex; align-items:center; gap:6px; background:#202630; border:1px solid #333b49; border-radius:14px; padding:3px 11px 3px 8px; font-size:12px; color:#e6e6e6; cursor:pointer; line-height:1.4; }
.lg-chip:hover { border-color:#2f6fed; }
.lg-chip i { width:10px; height:10px; border-radius:3px; display:inline-block; flex:0 0 auto; }
.lg-chip.off { opacity:.4; }
.lg-chip.off i { background:#5a6472 !important; }
.lg-chip.lg-all { background:#2f6fed; border-color:#2f6fed; }
.lg-hint { font-size:11px; color:#6b7280; margin:0 0 10px; }
</style>
</head>
<body>
<style>{{template "mcss"}}</style>
<div class="top-nav">
  <div>
    <h1 class="ttl">Электроэнергия</h1>
    <p class="sub">Счётчик DDS238: потребление/отдача по тарифам «День»/«Ночь»</p>
  </div>
  <a class="nav-btn secondary" href="/">&larr; Назад</a>
</div>

<div class="lg-hint">ЛКМ по чипу — вкл/выкл · двойной ЛКМ — только этот показатель · «Все» — показать все</div>
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
  <div class="lg-chips" id="dailyTariffChartLg"></div>
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
  <div class="lg-chips" id="monthlyTariffChartLg"></div>
  <div class="chart-wrap"><canvas id="monthlyTariffChart"></canvas></div>
</div>

<script>
'use strict';
var SR_COARSE = window.matchMedia ? matchMedia('(pointer: coarse)').matches : false;
var TARIFF_COLORS={ import_day:'#d0663a', import_night:'#8c5bbf', export_day:'#3fbf7f', export_night:'#2c8f6a' };
// hiddenSets[chartId] — метки скрытых пользователем столбцов (клик по легенде).
// Восстанавливаются при пересоздании графика (смена диапазона/пресета).
var hiddenSets={};
function captureHidden(id, oldChart){
	if(!oldChart || !oldChart.data || !oldChart.data.datasets) return;
	var hidden=[];
	oldChart.data.datasets.forEach(function(ds,i){ if(!oldChart.isDatasetVisible(i)) hidden.push(ds.label); });
	hiddenSets[id]=hidden;
}
function applyHidden(id, chart){
	var hidden=hiddenSets[id]||[];
	if(!hidden.length || !chart || !chart.data || !chart.data.datasets) return false;
	chart.data.datasets.forEach(function(ds,i){ if(hidden.indexOf(ds.label)>=0) chart.setDatasetVisibility(i,false); });
	return true;
}
// lgKit — HTML-легенда-чипы вместо встроенной легенды Chart.js. ЛКМ по чипу —
// вкл/выкл этот показатель (toggle); двойной ЛКМ — показать только его; «Все» —
// показать все. Чистый DOM: без модификаторов и приватного API Chart.js.
function lgKit(canvasId, chipsId){
	var canvas=document.getElementById(canvasId), row=document.getElementById(chipsId);
	function swatch(ds){ return ds.borderColor || ds.backgroundColor || '#888'; }
	function sync(chart){
		var chips=row.querySelectorAll('.lg-chip:not(.lg-all)');
		chart.data.datasets.forEach(function(ds,i){ if(chips[i]) chips[i].classList.toggle('off', !chart.isDatasetVisible(i)); });
	}
	function isolate(chart,i){ chart.data.datasets.forEach(function(ds,j){ chart.setDatasetVisibility(j, j===i); }); chart.update(); sync(chart); }
	function toggle(chart,i){ chart.setDatasetVisibility(i, !chart.isDatasetVisible(i)); chart.update(); sync(chart); }
	function showAll(chart){ chart.data.datasets.forEach(function(ds,j){ chart.setDatasetVisibility(j, true); }); chart.update(); sync(chart); }
	function build(chart){
		row.innerHTML='';
		if(!chart.data.datasets || !chart.data.datasets.length) return;
		chart.data.datasets.forEach(function(ds,i){
			var b=document.createElement('button'); b.type='button'; b.className='lg-chip';
			b.title='ЛКМ — вкл/выкл · двойной ЛКМ — только этот показатель';
			var sw=document.createElement('i'); sw.style.background=swatch(ds);
			b.appendChild(sw); b.appendChild(document.createTextNode(ds.label));
			if(window.srBindChip){
				srBindChip(b, function(){ toggle(chart,i); }, function(){ isolate(chart,i); });
			}else{
				var clickTimer=null;
				b.addEventListener('click', function(){
					if(clickTimer){ clearTimeout(clickTimer); clickTimer=null; isolate(chart,i); }
					else { clickTimer=setTimeout(function(){ clickTimer=null; toggle(chart,i); }, 250); }
				});
			}
			row.appendChild(b);
		});
		var all=document.createElement('button'); all.type='button'; all.className='lg-chip lg-all';
		all.textContent='Все'; all.title='Показать все показатели';
		all.addEventListener('click', function(){ showAll(chart); });
		row.appendChild(all);
		sync(chart);
	}
	return { build: build };
}

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
	var old=window[canvasId]; if(old){ captureHidden(canvasId, old); try{ old.destroy(); }catch(e){} }
	canvas.getContext('2d');
	window[canvasId]=new Chart(canvas,{
		type:'bar',
		data:{ labels:labels, datasets:datasets },
		options:{
			responsive:true, maintainAspectRatio:false,
			interaction:{ mode:'index', intersect:false },
			animation:{ duration:300 },
			plugins:{ legend:{ display:false } },
			scales:{ x:{ ticks:{ autoSkip:true, maxTicksLimit:24 } }, y:{ beginAtZero:true, title:{ display:true, text:'kWh' } } }
		}
	});
	if(applyHidden(canvasId, window[canvasId])) window[canvasId].update('none');
	lgKit(canvasId, canvasId+'Lg').build(window[canvasId]);
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
{{template "mnav" .}}
<script>{{template "mjs"}}</script>
</body>
</html>`

var energyTmpl = template.Must(template.New("energy").Parse(mobileCommon + energyPage))

// bmsDetailPage — страница деталей ANT BMS (/bms/<name>): актуальные параметры
// одной батареи из HASH sunreceiver:bms (обновление раз в секунду). Имя берётся
// из URL; данные — /api/bms/<name>. Ячейки: самая высокая — красная, самая
// низкая — синяя, остальные — зелёные; рядом с заголовком — разброс (mV).
const bmsDetailPage = `<!DOCTYPE html>
<html lang="ru">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1, maximum-scale=1, user-scalable=no">
<title>BMS — SunReceiver</title>
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
.nav-btn.secondary { background:#252b36; border:1px solid #333b49; color:#e6e6e6; }
.kpi-row { display:flex; flex-wrap:wrap; gap:10px; margin-bottom:16px; }
.kpi { flex:1 1 150px; min-width:140px; background:linear-gradient(135deg,#1d2430,#202a3a); border:1px solid #2a3342; border-radius:10px; padding:12px 16px; }
.kpi .lbl { font-size:11px; color:#8a93a1; text-transform:uppercase; letter-spacing:.05em; }
.kpi .val { font-size:26px; font-weight:700; line-height:1.15; font-variant-numeric:tabular-nums; margin-top:4px; }
.kpi .unit { font-size:14px; color:#8a93a1; font-weight:400; margin-left:5px; }
.kpi .sub { font-size:11px; color:#6b7280; margin-top:2px; }
.kpi .pos { color:#6fd08a; } .kpi .neg { color:#ff9f43; }
.kpi .socval { display:flex; align-items:center; gap:12px; }
.mini-batt { position:relative; width:22px; height:40px; border:2px solid #8a93a1; border-radius:5px; background:#10141b; flex:0 0 auto; }
.mini-batt::before { content:''; position:absolute; top:-6px; left:50%; transform:translateX(-50%); width:10px; height:4px; background:#8a93a1; border-radius:2px 2px 0 0; }
.mini-batt-fill { position:absolute; left:2px; right:auto; top:2px; bottom:2px; border-radius:2px; }
.cards { display:flex; flex-wrap:wrap; gap:16px; }
.card { background:#181c24; border:1px solid #252b36; border-radius:10px; padding:16px; flex:1 1 440px; min-width:min(440px,100%); }
.card h2 { margin:0 0 12px; font-size:15px; font-weight:600; }
.card h2 .delta { color:#ff9f43; font-size:13px; font-weight:600; margin-left:10px; }
.legend { display:flex; flex-wrap:wrap; gap:14px; font-size:11px; color:#8a93a1; margin:0 0 12px; }
.legend span { display:inline-flex; align-items:center; gap:5px; }
.legend i { width:10px; height:10px; border-radius:3px; display:inline-block; }
.cells { display:flex; flex-wrap:wrap; gap:12px; }
.cell { display:flex; flex-direction:column; align-items:center; gap:4px; }
.cell-batt { position:relative; width:28px; height:56px; border:2px solid #8a93a1; border-radius:6px; background:#10141b; }
.cell-batt::before { content:''; position:absolute; top:-6px; left:50%; transform:translateX(-50%); width:11px; height:4px; background:#8a93a1; border-radius:2px 2px 0 0; }
.cell-fill { position:absolute; left:2px; right:2px; bottom:2px; border-radius:3px; transition:height .4s; }
.cell.mv { font-size:11px; font-variant-numeric:tabular-nums; color:#e6e6e6; }
.cell.idx { font-size:10px; color:#6b7280; }
.cell.max .cell-batt { border-color:#ff6b6b; }
.cell.min .cell-batt { border-color:#4dabf7; }
.temps { display:flex; flex-direction:column; gap:7px; }
.trow { display:flex; align-items:center; gap:10px; font-size:13px; }
.trow .tname { width:26px; color:#8a93a1; font-size:12px; }
.tbar { flex:1; height:10px; background:#10141b; border-radius:5px; overflow:hidden; }
.tbar-fill { height:100%; border-radius:5px; transition:width .4s; }
.trow .tval { width:52px; text-align:right; font-variant-numeric:tabular-nums; }
.trow .tname { flex:0 0 150px; width:150px; }
.tnote { color:#6b7280; font-size:11px; margin:10px 0 0; line-height:1.4; }
.bms-charts { display:flex; flex-wrap:wrap; gap:16px; margin-top:24px; }
.bms-charts-title { margin:28px 0 12px; font-size:16px; }
.bms-charts .card { margin-bottom:0; }
.bms-charts .chart-wrap { position:relative; height:300px; }
.period-panel { display:flex; align-items:center; flex-wrap:wrap; gap:10px; margin-bottom:16px; font-size:13px; color:#8a93a1; }
.period-panel button { background:#252b36; color:#e6e6e6; border:1px solid #333b49; border-radius:6px; padding:5px 12px; cursor:pointer; font-size:13px; }
.period-panel button:hover { background:#2f3644; }
.period-panel button.active { background:#2f6fed; border-color:#2f6fed; color:#fff; }
.period-panel input[type=date] { background:#181c24; color:#e6e6e6; border:1px solid #333b49; border-radius:6px; padding:4px 8px; font-size:13px; color-scheme:dark; }
.period-panel input[type=date]:focus { outline:none; border-color:#2f6fed; }
.period-panel .nav-arrow { padding:5px 10px; font-size:16px; line-height:1; }
.chart-toolbar { display:flex; align-items:center; flex-wrap:wrap; gap:10px 12px; margin:0 0 8px; font-size:13px; color:#8a93a1; }
.chart-toolbar button { background:#252b36; color:#e6e6e6; border:1px solid #333b49; border-radius:6px; padding:4px 10px; cursor:pointer; font-size:13px; }
.chart-toolbar button:hover { background:#2f3644; }
.mos-row { display:flex; gap:10px; flex-wrap:wrap; margin-top:4px; }
.mos { padding:7px 14px; border-radius:18px; font-size:13px; font-weight:600; border:1px solid #333b49; background:#181c24; color:#6b7280; }
.mos.on { background:#123524; border-color:#00b894; color:#00b894; }
.foot { color:#6b7280; font-size:12px; margin-top:16px; }
.lg-chips { display:flex; flex-wrap:wrap; gap:6px; margin:2px 0 10px; }
.lg-chip { display:inline-flex; align-items:center; gap:6px; background:#202630; border:1px solid #333b49; border-radius:14px; padding:3px 11px 3px 8px; font-size:12px; color:#e6e6e6; cursor:pointer; line-height:1.4; }
.lg-chip:hover { border-color:#2f6fed; }
.lg-chip i { width:10px; height:10px; border-radius:3px; display:inline-block; flex:0 0 auto; }
.lg-chip.off { opacity:.4; }
.lg-chip.off i { background:#5a6472 !important; }
.lg-chip.lg-all { background:#2f6fed; border-color:#2f6fed; }
.lg-hint { font-size:11px; color:#6b7280; margin:0 0 10px; }
.missing { color:#6b7280; font-style:italic; }
</style>
</head>
<body>
<style>{{template "mcss"}}</style>
<div class="top-nav">
  <div>
    <h1 class="ttl" id="bmsTitle">ANT BMS</h1>
    <p class="sub" id="bmsSub">Загрузка...</p>
  </div>
  <a class="nav-btn secondary" href="/">&larr; На главную</a>
</div>

<div class="kpi-row" id="kpiRow"><div class="missing">Загрузка...</div></div>

<div class="cards">
  <div class="card">
    <h2>Напряжения ячеек, V <span class="delta" id="cellDelta"></span></h2>
    <div class="legend">
      <span><i style="background:#ff6b6b"></i>максимальное</span>
      <span><i style="background:#4dabf7"></i>минимальное</span>
      <span><i style="background:#00b894"></i>остальные</span>
    </div>
    <div class="cells" id="cellsGrid"></div>
  </div>
  <div class="card">
    <h2>Температуры, &deg;C</h2>
    <div class="temps" id="tempsWrap"></div>
    <p class="tnote">T1–T6 — NTC-датчики температуры. Производитель не публикует точное соответствие каналов и мест; подписи реконструированы по даташиту AFE (2 внешних датчика + датчик платы), мануалу ANT (до 4 внешних датчиков) и параметрам защит (температура батареи / силовых ключей). В нашей установке все каналы показывают одинаковую температуру (батарея и плата в одном корпусе).</p>
    <h2 style="margin-top:20px">Мощностные ключи</h2>
    <div class="mos-row" id="mosWrap"></div>
  </div>
</div>

<h2 class="bms-charts-title">Графики (5-минутные средние, Redis)</h2>
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
<div class="lg-hint">ЛКМ по чипу — вкл/выкл линию · двойной ЛКМ — только эта линия · «Все» — показать все</div>
<div class="charts bms-charts">
  <div class="card">
    <h2>Заряд (SOC), %</h2>
    <div class="chart-toolbar"><span id="bmsCapChartRange"></span><button id="btnBmsCapReset">Сброс зума</button><span>Зум: колесо / drag&ndash;панорама</span></div>
    <div class="chart-wrap"><canvas id="bmsCapChart"></canvas></div>
  </div>
  <div class="card">
    <h2>Напряжение пакета, V</h2>
    <div class="chart-toolbar"><span id="bmsVoltChartRange"></span><button id="btnBmsVoltReset">Сброс зума</button><span>Зум: колесо / drag&ndash;панорама</span></div>
    <div class="chart-wrap"><canvas id="bmsVoltChart"></canvas></div>
  </div>
  <div class="card">
    <h2>Ток, A</h2>
    <div class="chart-toolbar"><span id="bmsCurChartRange"></span><button id="btnBmsCurReset">Сброс зума</button><span>Зум: колесо / drag&ndash;панорама</span></div>
    <div class="chart-wrap"><canvas id="bmsCurChart"></canvas></div>
  </div>
  <div class="card">
    <h2>Мощность, W</h2>
    <div class="chart-toolbar"><span id="bmsPwrChartRange"></span><button id="btnBmsPwrReset">Сброс зума</button><span>Зум: колесо / drag&ndash;панорама</span></div>
    <div class="chart-wrap"><canvas id="bmsPwrChart"></canvas></div>
  </div>
  <div class="card">
    <h2>Напряжения ячеек, V</h2>
    <div class="chart-toolbar"><span id="bmsCellsChartRange"></span><button id="btnBmsCellsReset">Сброс зума</button><span>Зум: колесо / drag&ndash;панорама</span></div>
    <div class="lg-chips" id="bmsCellsChartLg"></div>
    <div class="chart-wrap"><canvas id="bmsCellsChart"></canvas></div>
  </div>
  <div class="card">
    <h2>Температуры T1–T4 (батарея, силовые ключи, плата), &deg;C</h2>
    <div class="chart-toolbar"><span id="bmsTempChartRange"></span><button id="btnBmsTempReset">Сброс зума</button><span>Зум: колесо / drag&ndash;панорама</span></div>
    <div class="lg-chips" id="bmsTempChartLg"></div>
    <div class="chart-wrap"><canvas id="bmsTempChart"></canvas></div>
  </div>
</div>

<p class="foot" id="bmsFoot"></p>

<script>
'use strict';
var SR_COARSE = window.matchMedia ? matchMedia('(pointer: coarse)').matches : false;
function esc(s){ return String(s).replace(/[&<>"]/g,function(c){ return {'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;'}[c]; }); }
function fmtNum(n,digits){ return isFinite(n)? n.toLocaleString('ru-RU',{maximumFractionDigits:digits}) : '—'; }

var NAME = decodeURIComponent(location.pathname.replace(/^\/bms\//,''));

// Цвет заполнения ячейки: max — красная, min — синяя, остальные — зелёные.
var CELL_COLOR={ max:'#ff6b6b', min:'#4dabf7', normal:'#00b894' };
// Заполнение батарейки ячейки по напряжению (шкала LiFePO4 3.00–3.65 В).
function cellFillPct(v){ var p=(v-3.00)/(3.65-3.00)*100; return Math.max(4,Math.min(100,p)); }
// Индексы max/min из кадра BMS — 1-based (ячейка №1 = 1), а i в цикле — 0-based.
function cellColor(d,i){ if(i+1===d.max_cell_idx) return 'max'; if(i+1===d.min_cell_idx) return 'min'; return 'normal'; }

function renderKPIs(d){
  var soc=Math.max(0,Math.min(100,Number(d.soc)||0));
  var socCol= soc<20?'#ff6b6b':(soc<50?'#ff9f43':(soc<80?'#f9ca24':'#00b894'));
  var cells=(d.cells_v||[]).slice(0,d.cell_count);
  var packV=0; for(var i=0;i<cells.length;i++) packV+=cells[i];
  var iCls= d.current_a>=0? 'pos':'neg';
  var pCls= d.power_w>=0? 'pos':'neg';
  var h='';
  h+='<div class="kpi"><div class="lbl">Заряд (SOC)</div><div class="val socval">'
    +'<span class="mini-batt"><span class="mini-batt-fill" style="width:'+soc+'%;background:'+socCol+';display:block"></span></span>'
    +'<span>'+soc+'<span class="unit">%</span></span></div>'
    +'<div class="sub">остаток '+fmtNum(d.remaining_ah,1)+' А·ч из '+fmtNum(d.capacity_ah,0)+' А·ч</div></div>';
  h+='<div class="kpi"><div class="lbl">Напряжение пакета</div><div class="val">'+fmtNum(packV,2)+'<span class="unit">V</span></div>'
    +'<div class="sub">'+d.cell_count+' ячеек, сред. '+fmtNum(d.avg_cell_v,3)+' В/яч</div></div>';
  h+='<div class="kpi"><div class="lbl">Ток</div><div class="val '+iCls+'">'+fmtNum(d.current_a,1)+'<span class="unit">A</span></div></div>';
  h+='<div class="kpi"><div class="lbl">Мощность</div><div class="val '+pCls+'">'+fmtNum(d.power_w,1)+'<span class="unit">W</span></div></div>';
  h+='<div class="kpi"><div class="lbl">Ёмкость</div><div class="val">'+fmtNum(d.capacity_ah,0)+'<span class="unit">А·ч</span></div></div>';
  document.getElementById('kpiRow').innerHTML=h;
}

function renderCells(d){
  var cells=(d.cells_v||[]).slice(0,d.cell_count);
  var h='';
  for(var i=0;i<cells.length;i++){
    var c=cellColor(d,i);
    h+='<div class="cell '+c+'">'
      +'<div class="cell-batt"><div class="cell-fill" style="height:'+cellFillPct(cells[i])+'%;background:'+CELL_COLOR[c]+'"></div></div>'
      +'<div class="mv">'+cells[i].toFixed(3)+'</div>'
      +'<div class="idx">'+(i+1)+'</div>'
      +'</div>';
  }
  document.getElementById('cellsGrid').innerHTML=h;
  var delta=(d.max_cell_v-d.min_cell_v);
  document.getElementById('cellDelta').textContent='разброс: '+delta.toFixed(3)+' В (макс '+d.max_cell_v.toFixed(3)+' / мин '+d.min_cell_v.toFixed(3)+' В)';
}

// Подписи каналов T1–T6 (реконструкция — см. docs/antbms/antbms-protocol-status-frame.md).
var TEMP_NAMES=['Батарея 1','Батарея 2','Силовая плата','Плата управления','Резерв','Резерв'];
function renderTemps(d){
  var t=d.temperatures_c||[];
  var h='';
  for(var i=0;i<t.length;i++){
    var v=Number(t[i]);
    var w=Math.max(2,Math.min(100,v/60*100));
    var col= v<15?'#4dabf7':(v<=40?'#00b894':(v<=55?'#ff9f43':'#ff6b6b'));
    var n=TEMP_NAMES[i]||('датчик '+(i+1));
    h+='<div class="trow"><span class="tname">T'+(i+1)+' · '+n+'</span>'
      +'<span class="tbar"><span class="tbar-fill" style="width:'+w+'%;background:'+col+';display:block"></span></span>'
      +'<span class="tval">'+fmtNum(v,0)+' &deg;C</span></div>';
  }
  document.getElementById('tempsWrap').innerHTML=h;
}

function renderMos(d){
  function pill(label,on){ return '<span class="mos'+(on?' on':'')+'">'+label+': '+(on?'ВКЛ':'ВЫКЛ')+'</span>'; }
  document.getElementById('mosWrap').innerHTML=pill('Заряд',d.charge_mos===1)+pill('Разряд',d.discharge_mos===1)+pill('Балансировка',d.balancer===1);
}

async function load(){
  if(!NAME){ document.getElementById('bmsTitle').textContent='BMS не выбрана'; return; }
  try{
    var r=await fetch('/api/bms/'+encodeURIComponent(NAME));
    if(r.status===404){
      document.getElementById('bmsTitle').textContent='BMS не найдена';
      document.getElementById('bmsSub').textContent='Устройство отсутствует в Redis (опрос отключён или батарея отключена)';
      return;
    }
    if(!r.ok) return;
    var d=await r.json();
    document.title=d.deviceName+' — SunReceiver';
    document.getElementById('bmsTitle').textContent=d.deviceName;
    document.getElementById('bmsSub').textContent='ANT BMS · порт '+d.port+' · актуально: '+d.time;
    renderKPIs(d); renderCells(d); renderTemps(d); renderMos(d);
    document.getElementById('bmsFoot').textContent='Порт: '+d.port+' · счётчик кадров: '+d.frames+' · обновляется каждую секунду';
  }catch(e){}
}
// ---------- Графики (5-минутные средние из Redis) ----------
Chart.register(ChartZoom);
var CHART_COLORS=['#4ecdc4','#ff6b6b','#4dabf7','#ffd166','#00b894','#a29bfe','#ff9f43','#e84393','#55efc4','#fd79a8','#74b9ff','#ffeaa7','#dfe6e9','#fab1a0','#81ecec','#6c5ce7'];
var BMS_CHART_IDS=['bmsCapChart','bmsVoltChart','bmsCurChart','bmsPwrChart','bmsCellsChart','bmsTempChart'];
var BMS_RESET_BTN={ bmsCapChart:'btnBmsCapReset', bmsVoltChart:'btnBmsVoltReset', bmsCurChart:'btnBmsCurReset', bmsPwrChart:'btnBmsPwrReset', bmsCellsChart:'btnBmsCellsReset', bmsTempChart:'btnBmsTempReset' };
function mkBmsDs(label,color,data){ return { label:label, data:data, borderColor:color, backgroundColor:color, pointRadius:0, pointHoverRadius:0, borderWidth:1.5, tension:0.35, cubicInterpolationMode:'monotone', fill:false }; }
function packVolt(p){ var s=0, c=p.cells_v||[]; for(var i=0;i<c.length;i++) s+=c[i]; return s; }
function fmtDate(t){ var d=new Date(t); function p(x){return (x<10?'0':'')+x;} return d.getFullYear()+'-'+p(d.getMonth()+1)+'-'+p(d.getDate())+' '+p(d.getHours())+':'+p(d.getMinutes()); }
function setBmsRangeLabels(){
  var txt='Диапазон: '+fmtDate(selRange.from)+' — '+fmtDate(selRange.to);
  BMS_CHART_IDS.forEach(function(id){ var el=document.getElementById(id+'Range'); if(el) el.textContent=txt; });
}

// ---------- Выбор периода (общий для всех графиков BMS) ----------
var preserveZoom=false;
var selRange={from:startOfToday(), to:endOfToday()};
var periodMode='day';
function startOfToday(){ var d=new Date(); d.setHours(0,0,0,0); return d; }
function endOfToday(){ var d=new Date(); d.setHours(23,59,59,999); return d; }
function startOfYesterday(){ var d=new Date(); d.setDate(d.getDate()-1); d.setHours(0,0,0,0); return d; }
function dayFromStr(s){ var p=String(s).split('-').map(Number); return new Date(p[0], p[1]-1, p[2], 0,0,0,0); }
function toInputDate(d){ function p(x){return (x<10?'0':'')+x;} return d.getFullYear()+'-'+p(d.getMonth()+1)+'-'+p(d.getDate()); }
function endOfDay(d){ var e=new Date(d); e.setHours(23,59,59,999); return e; }
function dayStart(d){ var r=new Date(d); r.setHours(0,0,0,0); return r; }
function addDays(d,n){ var r=new Date(d); r.setDate(r.getDate()+n); return r; }
function addMonths(d,n){ var r=new Date(d); r.setMonth(r.getMonth()+n); return r; }
function startOfMonthOf(d){ return dayStart(new Date(d.getFullYear(), d.getMonth(), 1)); }
function endOfMonthOf(d){ var f=new Date(d.getFullYear(), d.getMonth(), 1); return new Date(f.getFullYear(), f.getMonth()+1, 0, 23,59,59,999); }
var PERIOD_BTNS=['btnToday','btnYesterday','btn7d','btnMonth'];
function setActiveBtn(activeBtn){ for(var i=0;i<PERIOD_BTNS.length;i++) document.getElementById(PERIOD_BTNS[i]).classList.remove('active'); if(activeBtn) document.getElementById(activeBtn).classList.add('active'); }
// setPeriod — выставляет диапазон, режим (day/week/month/custom), синхронизирует поля
// и перерисовывает все графики BMS. Сбрасывает зум (новое окно = полный период).
function setPeriod(from,to,mode,activeBtn){
  selRange.from=from; selRange.to=to; periodMode=mode;
  preserveZoom=false;
  setActiveBtn(activeBtn);
  var dFrom=dayStart(from), dTo=dayStart(to);
  document.getElementById('fromPick').value=toInputDate(dFrom);
  document.getElementById('toPick').value=toInputDate(dTo);
  document.getElementById('datePick').value=toInputDate(mode==='day'?from:dFrom);
  loadBmsCharts();
}
function shiftPeriod(delta){
  var from=selRange.from, to=selRange.to;
  var newFrom, newTo;
  if(periodMode==='day'){ newFrom=addDays(dayStart(from), delta); newTo=endOfDay(newFrom); }
  else if(periodMode==='month'){ newFrom=addMonths(dayStart(from), delta); newTo=endOfMonthOf(newFrom); }
  else{ var span=to-from; newFrom=new Date(from.getTime()+delta*span); newTo=new Date(to.getTime()+delta*span); }
  setPeriod(newFrom,newTo,periodMode,null);
}

// ---------- Синхронизация зума между графиками BMS (по X) ----------
var lastXWindow={};
var zoomSyncing=false;
var bmsRebuilding=false;
function checkBmsZoomSync(chart){
  if(zoomSyncing || bmsRebuilding) return;
  var x=chart&&chart.scales&&chart.scales.x;
  if(!x||!isFinite(x.min)||!isFinite(x.max)||x.max<=x.min) return;
  var key=x.min.toFixed(3)+','+x.max.toFixed(3);
  if(lastXWindow[chart.canvas.id]!==undefined && lastXWindow[chart.canvas.id]!==key) syncBmsZoomToOthers(chart);
  lastXWindow[chart.canvas.id]=key;
}
function syncBmsZoomToOthers(fromChart){
  if(zoomSyncing) return;
  var sx=fromChart&&fromChart.scales&&fromChart.scales.x;
  if(!sx||!isFinite(sx.min)||!isFinite(sx.max)||sx.max<=sx.min) return;
  var m=sx.min, M=sx.max;
  zoomSyncing=true;
  try{
    BMS_CHART_IDS.forEach(function(id){
      var c=BMS_CHARTS[id];
      if(!c || c===fromChart) return;
      try{ c.zoomScale('x',{min:m,max:M},'none'); }catch(e){}
    });
  }finally{ zoomSyncing=false; }
}
var bmsZoomSyncPlugin={ id:'bmsZoomSync', afterDraw:function(chart){ try{ checkBmsZoomSync(chart); }catch(e){} } };
// lgKit — HTML-легенда-чипы вместо встроенной легенды Chart.js. ЛКМ по чипу —
// вкл/выкл эту линию (toggle); двойной ЛКМ — показать только эту линию; «Все» —
// показать все. Чистый DOM: без модификаторов и приватного API Chart.js.
function lgKit(canvasId, chipsId){
	var canvas=document.getElementById(canvasId), row=document.getElementById(chipsId);
	function swatch(ds){ return ds.borderColor || ds.backgroundColor || '#888'; }
	function sync(chart){
		var chips=row.querySelectorAll('.lg-chip:not(.lg-all)');
		chart.data.datasets.forEach(function(ds,i){ if(chips[i]) chips[i].classList.toggle('off', !chart.isDatasetVisible(i)); });
	}
	function isolate(chart,i){ chart.data.datasets.forEach(function(ds,j){ chart.setDatasetVisibility(j, j===i); }); chart.update(); sync(chart); }
	function toggle(chart,i){ chart.setDatasetVisibility(i, !chart.isDatasetVisible(i)); chart.update(); sync(chart); }
	function showAll(chart){ chart.data.datasets.forEach(function(ds,j){ chart.setDatasetVisibility(j, true); }); chart.update(); sync(chart); }
	function build(chart){
		row.innerHTML='';
		if(!chart.data.datasets || !chart.data.datasets.length) return;
		chart.data.datasets.forEach(function(ds,i){
			var b=document.createElement('button'); b.type='button'; b.className='lg-chip';
			b.title='ЛКМ — вкл/выкл · двойной ЛКМ — только эта линия';
			var sw=document.createElement('i'); sw.style.background=swatch(ds);
			b.appendChild(sw); b.appendChild(document.createTextNode(ds.label));
			if(window.srBindChip){
				srBindChip(b, function(){ toggle(chart,i); }, function(){ isolate(chart,i); });
			}else{
				var clickTimer=null;
				b.addEventListener('click', function(){
					if(clickTimer){ clearTimeout(clickTimer); clickTimer=null; isolate(chart,i); }
					else { clickTimer=setTimeout(function(){ clickTimer=null; toggle(chart,i); }, 250); }
				});
			}
			row.appendChild(b);
		});
		var all=document.createElement('button'); all.type='button'; all.className='lg-chip lg-all';
		all.textContent='Все'; all.title='Показать все линии';
		all.addEventListener('click', function(){ showAll(chart); });
		row.appendChild(all);
		sync(chart);
	}
	return { build: build };
}

// bmsRender — линейный график; zero=true — симметричная ось с нулём посередине.
// Инстансы хранятся в BMS_CHARTS (НЕ в window[id] — там элемент canvas с этим
// id: window.bmsCapChart отдаёт канвас, а не Chart, и destroy() по нему падает).
// Zум по X (колесо с Ctrl / pinch / drag-панорама) синхронизируется между всеми
// графиками страницы (bmsZoomSyncPlugin). Выбор линий через легенду и зум
// сохраняются при обновлении по таймеру (bmsCaptureState/bmsApplyHidden/bmsSavedZoom).
var BMS_CHARTS={};
var bmsHiddenSets={};
var bmsSavedZoom={};
// bmsCaptureState запоминает скрытые пользователем линии и текущий зум каждого
// графика до его уничтожения (вызывается до destroyBmsCharts).
function bmsCaptureState(){
  Object.keys(BMS_CHARTS).forEach(function(id){
    var c=BMS_CHARTS[id];
    if(!c) return;
    if(c.data && c.data.datasets){
      var hidden=[];
      c.data.datasets.forEach(function(ds,i){ if(!c.isDatasetVisible(i)) hidden.push(ds.label); });
      bmsHiddenSets[id]=hidden;
    }
    if(c.scales && c.scales.x && isFinite(c.scales.x.min) && isFinite(c.scales.x.max)){
      bmsSavedZoom[id]={min:c.scales.x.min, max:c.scales.x.max};
    }
  });
}
// bmsApplyHidden скрывает в новом графике ранее скрытые линии. Возвращает true,
// если применил (тогда нужен update).
function bmsApplyHidden(id, chart){
  var hidden=bmsHiddenSets[id]||[];
  if(!hidden.length || !chart || !chart.data || !chart.data.datasets) return false;
  chart.data.datasets.forEach(function(ds,i){ if(hidden.indexOf(ds.label)>=0) chart.setDatasetVisibility(i,false); });
  return true;
}
function bmsRender(id, datasets, yTitle, legend, zero){
  var saved=bmsSavedZoom[id]||{min:null,max:null};
  var y={ beginAtZero:false, title:{ display:!!yTitle, text:yTitle||'' } };
  if(zero){
    var m=0;
    datasets.forEach(function(ds){ (ds.data||[]).forEach(function(p){ if(isFinite(p.y)){ var a=Math.abs(p.y); if(a>m)m=a; } }); });
    m=m>0?m:1; y.min=-m; y.max=m;
  }
  BMS_CHARTS[id]=new Chart(document.getElementById(id),{
    type:'line', data:{datasets:datasets},
    plugins:[bmsZoomSyncPlugin],
    options:{
      responsive:true, maintainAspectRatio:false,
      interaction:{ mode:'index', intersect:false },
      animation:{ duration:200 },
      plugins:{
        legend: { display:false },
        zoom:{
          pan:{ enabled:!SR_COARSE, mode:'x' },
          zoom:{ wheel:{ enabled:!SR_COARSE, speed:0.1, modifierKey:'ctrl' }, pinch:{ enabled:!SR_COARSE }, mode:'x' },
          limits:{ x:{ minRange: 5*60*1000 } }
        }
      },
      scales:{
        x:{ type:'time', time:{ unit:'hour', displayFormats:{ hour:'HH:mm' } }, ticks:{ maxRotation:0, autoSkipPadding:16 } },
        y:y
      }
    }
  });
  var needUpdate=false;
  if(preserveZoom && saved.min!==null && saved.max!==null){
    BMS_CHARTS[id].options.scales.x.min=saved.min; BMS_CHARTS[id].options.scales.x.max=saved.max;
    needUpdate=true;
  }
  if(bmsApplyHidden(id, BMS_CHARTS[id])) needUpdate=true;
  if(needUpdate){ BMS_CHARTS[id].update('none'); }
  if(legend){ lgKit(id, id+'Lg').build(BMS_CHARTS[id]); }
}
function buildBmsCharts(points){
  if(!points||!points.length) return;
  // 1. Заряд (SOC)
  bmsRender('bmsCapChart',[mkBmsDs('SOC','#4ecdc4',points.map(function(p){ return {x:new Date(p.ts), y:p.soc}; }))],'%',false,false);
  // 2. Напряжение пакета (сумма ячеек)
  bmsRender('bmsVoltChart',[mkBmsDs('Пакет','#ffd166',points.map(function(p){ return {x:new Date(p.ts), y:packVolt(p)}; }))],'V',false,false);
  // 3. Ток (± ось посередине)
  bmsRender('bmsCurChart',[mkBmsDs('Ток','#4dabf7',points.map(function(p){ return {x:new Date(p.ts), y:p.current_a}; }))],'A',false,true);
  // 4. Мощность (± ось посередине)
  bmsRender('bmsPwrChart',[mkBmsDs('Мощность','#ff6b6b',points.map(function(p){ return {x:new Date(p.ts), y:p.power_w}; }))],'W',false,true);
  // 5. Напряжения всех ячеек на одном графике
  var ncells=0; points.forEach(function(p){ var l=(p.cells_v||[]).length; if(l>ncells)ncells=l; });
  var cds=[];
  for(var i=0;i<ncells;i++){
    (function(i){
      cds.push(mkBmsDs('Ячейка '+(i+1),CHART_COLORS[i%CHART_COLORS.length],points.map(function(p){
        var c=p.cells_v||[]; return {x:new Date(p.ts), y:(i<c.length? c[i] : null)};
      })));
    })(i);
  }
  bmsRender('bmsCellsChart',cds,'V',true,false);
  // 6. Температуры: батарея (T1/T2), силовые ключи (T3), плата (T4)
  var tnames=['T1 · Батарея 1','T2 · Батарея 2','T3 · Силовая плата','T4 · Плата управления'];
  var tcols=['#00b894','#55efc4','#ff9f43','#a29bfe'];
  var tds=[];
  for(var t=0;t<4;t++){
    (function(t){
      tds.push(mkBmsDs(tnames[t],tcols[t],points.map(function(p){
        var arr=p.temperatures_c||[]; return {x:new Date(p.ts), y:(t<arr.length? arr[t] : null)};
      })));
    })(t);
  }
  bmsRender('bmsTempChart',tds,'°C',true,false);
}
// Обновление раз в минуту: старые Chart-инстансы уничтожаются и строятся заново
// (число датасетов — напр. ячеек — может меняться). Скрытые линии и зум
// запоминаются заранее (bmsCaptureState) и восстанавливаются в bmsRender.
function destroyBmsCharts(){
  Object.keys(BMS_CHARTS).forEach(function(id){
    BMS_CHARTS[id].destroy();
    delete BMS_CHARTS[id];
  });
}
async function loadBmsCharts(){
  try{
    var from=selRange.from, to=selRange.to;
    var url='/api/bms/'+encodeURIComponent(NAME)+'/series?from='+encodeURIComponent(from.toISOString())+'&to='+encodeURIComponent(to.toISOString());
    var r=await fetch(url);
    if(!r.ok) return;
    var data=await r.json();
    bmsCaptureState();
    destroyBmsCharts();
    bmsRebuilding=true;
    try{
      lastXWindow={};
      buildBmsCharts(data.points||[]);
    }finally{ bmsRebuilding=false; }
    setBmsRangeLabels();
  }catch(e){}
}

// Кнопки периода BMS.
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
document.getElementById('btnRefresh').addEventListener('click',function(){ preserveZoom=false; loadBmsCharts(); });
// Кнопки «Сброс зума» по каждому графику BMS.
for(var _b=0;_b<BMS_CHART_IDS.length;_b++){
  (function(id){
    var btn=document.getElementById(BMS_RESET_BTN[id]);
    if(btn) btn.addEventListener('click',function(){ var c=BMS_CHARTS[id]; if(c) try{ c.resetZoom(); }catch(e){} });
  })(BMS_CHART_IDS[_b]);
}
// Инициализация полей периода и первичная загрузка; дальше — раз в минуту (зум
// и выбор линий сохраняются), параметры батареи — каждую секунду.
document.getElementById('fromPick').value=toInputDate(dayStart(selRange.from));
document.getElementById('toPick').value=toInputDate(dayStart(selRange.to));
document.getElementById('datePick').value=toInputDate(selRange.from);
setActiveBtn('btnToday');
loadBmsCharts();
setInterval(function(){ preserveZoom=true; loadBmsCharts(); },60000);

// Мобильная версия: touch-жесты по графикам BMS (щипок — зум, свайп — панорама,
// двойной тап — сброс зума, тап — стандартный tooltip Chart.js в точке).
if(SR_COARSE){
  BMS_CHART_IDS.forEach(function(id){
    srTouchChart(function(){ return BMS_CHARTS[id]; }, id, 5*60*1000, function(px){
      var c=BMS_CHARTS[id]; if(!c || !c.chartArea) return;
      try{
        var els=c.getElementsAtEventForMode({x:px, y:c.chartArea.top+10}, 'index', {intersect:false}, true);
        if(els.length){
          c.tooltip.setActiveElements(els.map(function(el){ return {datasetIndex:el.datasetIndex, index:el.index}; }), true);
          c.update();
        }
      }catch(e){}
    });
  });
}

load(); setInterval(load,1000);
</script>
{{template "mnav" .}}
<script>{{template "mjs"}}</script>
</body>
</html>`

var bmsDetailTmpl = template.Must(template.New("bmsdetail").Parse(mobileCommon + bmsDetailPage))

func (h *dashboardHandler) charts(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = chartsTmpl.Execute(w, map[string]any{"active": "charts"})
}

func (h *dashboardHandler) energy(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = energyTmpl.Execute(w, map[string]any{"active": "energy"})
}

// apiBMS отдаёт актуальное состояние всех ANT BMS (HASH sunreceiver:bms,
// пулер bms_poller.go) для батареек на главной странице (обновление раз в минуту).
func (h *dashboardHandler) apiBMS(w http.ResponseWriter, r *http.Request) {
	m, err := h.store.BMSCurrent()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	devs := make([]bmsDevice, 0, len(m))
	for _, raw := range m {
		var d bmsDevice
		if err := json.Unmarshal([]byte(raw), &d); err != nil {
			continue
		}
		devs = append(devs, d)
	}
	sort.Slice(devs, func(i, j int) bool { return devs[i].DeviceName < devs[j].DeviceName })
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"generated_at": time.Now().Format(time.RFC3339),
		"bms":          devs,
	})
}

// apiBMSOne отдаёт актуальное состояние одной ANT BMS по deviceName
// (/api/bms/<name>) для страницы деталей (обновление раз в секунду).
// /api/bms/<name>/series — временной ряд 5-минутных усреднённых точек
// (см. apiBMSSeries).
func (h *dashboardHandler) apiBMSOne(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/api/bms/")
	if name == "" {
		http.Error(w, "не указано имя BMS", http.StatusBadRequest)
		return
	}
	if strings.HasSuffix(name, "/series") {
		h.apiBMSSeries(w, r, strings.TrimSuffix(name, "/series"))
		return
	}
	raw, err := h.store.BMSOne(name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if raw == "" {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "BMS не найдена"})
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(raw))
}

// apiBMSSeries отдаёт 5-минутные усреднённые точки BMS (/api/bms/<name>/series)
// за период [from, to] (RFC3339; по умолч. — последние 24 часа) из Redis-ряда
// (окно удержания 2 календарных суток). Точки — bmsSeriesPoint: ts +
// усреднённые параметры (см. bms_accumulator.go).
func (h *dashboardHandler) apiBMSSeries(w http.ResponseWriter, r *http.Request, name string) {
	now := time.Now()
	to := now
	if s := r.URL.Query().Get("to"); s != "" {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			to = t
		}
	}
	from := to.Add(-24 * time.Hour)
	if s := r.URL.Query().Get("from"); s != "" {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			from = t
		}
	}
	pts, err := h.store.QueryBMSSeries(name, from, to)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"name":   name,
		"from":   from.Format(time.RFC3339),
		"to":     to.Format(time.RFC3339),
		"points": pts,
	})
}

// bmsDetail — страница деталей ANT BMS (/bms/<name>): все текущие параметры
// выбранной батареи, обновление раз в секунду. JS берёт имя из URL и
// опрашивает /api/bms/<name>.
func (h *dashboardHandler) bmsDetail(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = bmsDetailTmpl.Execute(w, map[string]any{"active": "home"})
}

var dashboardTmpl = template.Must(template.New("dash").Parse(mobileCommon + dashboardPage))

func (h *dashboardHandler) index(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = dashboardTmpl.Execute(w, map[string]any{"active": "home"})
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

// addTariffs складывает тарифные величины посуточных финализированных дней периода
// (days) с тарифами текущего незавершённого дня (tDay/tNight/eDay/eNight). Возвращает
// итоговые значения периода (kWh): потребление/отдачу «День»/«Ночь».
func addTariffs(days []meterDayStat, tDay, tNight, eDay, eNight float64) (float64, float64, float64, float64) {
	var d, n, ed, en float64
	for _, st := range days {
		d += st.ImportDay
		n += st.ImportNight
		ed += st.ExportDay
		en += st.ExportNight
	}
	return d + tDay, n + tNight, ed + eDay, en + eNight
}

// loadTariffData возвращает кешированные исходники тарифов для /api/current:
// границы текущего дня (b), сумму финализированных прошедших дней текущего
// месяца (m: [importDay, importNight, exportDay, exportNight]) и года (y). Текущий
// день сюда НЕ входит (он считается отдельно на каждом запросе из живых
// показаний счётчика). Результат кешируется на tariffCacheTTL — данные в PG
// меняются редко, а /api/current опрашивается каждую секунду.
func (h *dashboardHandler) loadTariffData(now time.Time) (*meterBoundaryRow, [4]float64, [4]float64) {
	h.tariffMu.Lock()
	defer h.tariffMu.Unlock()
	if !h.tariffAt.IsZero() && now.Sub(h.tariffAt) < tariffCacheTTL {
		return h.tariffB, h.tariffM, h.tariffY
	}
	var b *meterBoundaryRow
	var m, y [4]float64
	if h.pg != nil {
		loc := time.Local
		start, _ := dayBounds(now, loc)
		if bb, err := h.pg.MeterBoundaryValues(start); err == nil {
			b = bb
		} else {
			log.Printf("dashboard: meter tariff today: %v", err)
		}
		if b != nil {
			yy, mo, _ := now.In(loc).Date()
			monthStart := time.Date(yy, mo, 1, 0, 0, 0, 0, loc)
			monthEnd := monthStart.AddDate(0, 1, 0)
			yearStart := time.Date(yy, 1, 1, 0, 0, 0, 0, loc)
			yearEnd := yearStart.AddDate(1, 0, 0)
			if days, err := h.pg.DailyTariffsRange(monthStart, monthEnd); err == nil {
				m[0], m[1], m[2], m[3] = addTariffs(days, 0, 0, 0, 0)
			} else {
				log.Printf("dashboard: meter tariff month: %v", err)
			}
			if days, err := h.pg.DailyTariffsRange(yearStart, yearEnd); err == nil {
				y[0], y[1], y[2], y[3] = addTariffs(days, 0, 0, 0, 0)
			} else {
				log.Printf("dashboard: meter tariff year: %v", err)
			}
		}
	}
	h.tariffAt = now
	h.tariffB = b
	h.tariffM = m
	h.tariffY = y
	return h.tariffB, h.tariffM, h.tariffY
}

func (h *dashboardHandler) apiCurrent(w http.ResponseWriter, r *http.Request) {
	devices, err := h.store.Current()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	sort.SliceStable(devices, func(i, j int) bool {
		// MPPT-контроллеры — всегда последними; остальные — в порядке конфигурации
		// (поле order снимка = позиция устройства в sunReceiver.json: инверторы в
		// порядке invertors, затем МАП). При равных order (напр. старые снимки без order)
		// — стабильно по имени.
		mi, mj := isMPPTKey(devices[i].IP), isMPPTKey(devices[j].IP)
		if mi != mj {
			return !mi
		}
		oi, oj := devices[i].Order, devices[j].Order
		if oi != oj {
			return oi < oj
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
	impDayM, impNightM, expDayM, expNightM := 0.0, 0.0, 0.0, 0.0
	impDayY, impNightY, expDayY, expNightY := 0.0, 0.0, 0.0, 0.0
	if hasImp && hasExp && h.pg != nil {
		// Исходники (границы дня и суммы финализированных прошедших дней месяца/года)
		// берём из TTL-кеша; сегодняшний день и живые показания счётчика (impNow/expNow)
		// пересчитываются на каждом запросе — так плашки остаются актуальными без лишних
		// запросов к PG (данные прошедших дней в PG неизменны).
		b, mSum, ySum := h.loadTariffData(now)
		if b != nil {
			impDay, impNight, expDay, expNight = meterTariffToday(now, impNow, expNow, b)
		}
		impDayM, impNightM, expDayM, expNightM = mSum[0]+impDay, mSum[1]+impNight, mSum[2]+expDay, mSum[3]+expNight
		impDayY, impNightY, expDayY, expNightY = ySum[0]+impDay, ySum[1]+impNight, ySum[2]+expDay, ySum[3]+expNight
	}
	impDay, impNight, expDay, expNight = math.Round(impDay*100)/100, math.Round(impNight*100)/100,
		math.Round(expDay*100)/100, math.Round(expNight*100)/100
	impDayM, impNightM, expDayM, expNightM = math.Round(impDayM*100)/100, math.Round(impNightM*100)/100,
		math.Round(expDayM*100)/100, math.Round(expNightM*100)/100
	impDayY, impNightY, expDayY, expNightY = math.Round(impDayY*100)/100, math.Round(impNightY*100)/100,
		math.Round(expDayY*100)/100, math.Round(expNightY*100)/100

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(currentResponse{
		GeneratedAt:           time.Now().Format(time.RFC3339),
		TotalPower:            total,
		TotalPV:               totalPV,
		MapGridV:              gridV,
		MapGridP:              gridP,
		MapBatV:               batV,
		MapBatP:               batP,
		MapCons:               gridP + batP,
		MeterImportDay:        impDay,
		MeterImportNight:      impNight,
		MeterExportDay:        expDay,
		MeterExportNight:      expNight,
		MeterImportDayMonth:   impDayM,
		MeterImportNightMonth: impNightM,
		MeterExportDayMonth:   expDayM,
		MeterExportNightMonth: expNightM,
		MeterImportDayYear:    impDayY,
		MeterImportNightYear:  impNightY,
		MeterExportDayYear:    expDayY,
		MeterExportNightYear:  expNightY,
		Devices:               devices,
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
	mux.HandleFunc("/bms/", h.bmsDetail)
	mux.HandleFunc("/api/current", h.apiCurrent)
	mux.HandleFunc("/api/series", h.apiSeries)
	mux.HandleFunc("/api/tariffs", h.apiTariffs)
	mux.HandleFunc("/api/bms", h.apiBMS)
	mux.HandleFunc("/api/bms/", h.apiBMSOne)
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

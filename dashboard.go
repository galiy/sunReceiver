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
	GeneratedAt string         `json:"generated_at"`
	From        string         `json:"from"`
	To          string         `json:"to"`
	Series      []deviceSeries `json:"series"`
	Total       []seriesPoint  `json:"total,omitempty"`
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
.cards { display:grid; grid-template-columns:repeat(auto-fit,minmax(min(220px,100%),1fr)); gap:12px; width:100%; }
.card { background:#181c24; border:1px solid #252b36; border-radius:10px; padding:14px 16px; min-width:0; max-width:100%; overflow:hidden; }
.card h2 { margin:0 0 12px; font-size:16px; display:flex; justify-content:space-between; align-items:center; gap:8px; flex-wrap:wrap; }
.card .badge { font-size:12px; color:#0f1115; background:#3fb950; padding:2px 8px; border-radius:20px; }
.card .ts { font-size:12px; color:#8a93a1; font-weight:normal; }
.card .groups { display:grid; grid-template-columns:repeat(auto-fit,minmax(min(140px,100%),1fr)); gap:12px; }
.group h3 { margin:0 0 6px; font-size:12px; color:#8a93a1; text-transform:uppercase; letter-spacing:.05em; }
.rows { display:flex; flex-direction:column; gap:5px; }
.row { display:grid; grid-template-columns:minmax(0,1fr) max-content; gap:0 10px; align-items:baseline; font-size:13px; line-height:1.3; }
.row .k { color:#aab3bf; min-width:0; }
.row .v { font-variant-numeric:tabular-nums; font-weight:600; text-align:right; justify-self:end; }
.row .u { color:#8a93a1; font-weight:400; font-size:12px; margin-left:2px; }
.missing { color:#6b7280; font-style:italic; }
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
<p class="sub">Текущие параметры инверторов (из Redis, обновление каждые 5&nbsp;с)</p>

<div class="kpi">
  <div class="kpi-plate">
    <div class="kpi-label">Суммарная активная мощность</div>
    <div class="kpi-value"><span id="kpiTotal">—</span><span class="kpi-unit">W</span></div>
    <div class="kpi-sub" id="kpiSub">Нет данных</div>
  </div>
</div>

<div class="period-panel">
  <button id="btnToday">Сегодня</button>
  <button id="btnYesterday">Вчера</button>
  <button id="btn7d">7 дней</button>
  <input type="date" id="datePick" title="Выбрать день">
  <button id="btnDate">За выбранный день</button>
</div>

<div class="charts">
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

<div class="cards" id="cards"><div class="missing">Загрузка...</div></div>

<script>
'use strict';

// ---------- Утилиты ----------
function esc(s){ return String(s).replace(/[&<>"]/g,function(c){ return {'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;'}[c]; }); }
function fmt(t){ var d=new Date(t); function p(x){return (x<10?'0':'')+x;} return d.getFullYear()+'-'+p(d.getMonth()+1)+'-'+p(d.getDate())+' '+p(d.getHours())+':'+p(d.getMinutes()); }
function fmtSec(t){ var d=new Date(t); function p(x){return (x<10?'0':'')+x;} return d.getFullYear()+'-'+p(d.getMonth()+1)+'-'+p(d.getDate())+' '+p(d.getHours())+':'+p(d.getMinutes())+':'+p(d.getSeconds()); }
function startOfToday(){ var d=new Date(); d.setHours(0,0,0,0); return d; }
function endOfToday(){ var d=new Date(); d.setHours(23,59,59,999); return d; }

// ---------- Таблички текущих параметров ----------
// Каждый тег: смысловая подпись по-русски + единица измерения.
var sel = {
	pv1_voltage:['Напряжение PV1','V'],pv1_current:['Ток PV1','A'],pv1_power:['Мощность PV1','W'],
	pv2_voltage:['Напряжение PV2','V'],pv2_current:['Ток PV2','A'],pv2_power:['Мощность PV2','W'],
	ac_active_power:['Активная мощность','W'],ac_reactive_power:['Реактивная мощность','var'],
	grid_frequency:['Частота сети','Hz'],
	l1_voltage:['Напряжение L1','V'],l1_current:['Ток L1','A'],
	l2_voltage:['Напряжение L2','V'],l2_current:['Ток L2','A'],
	l3_voltage:['Напряжение L3','V'],l3_current:['Ток L3','A'],
	energy_today:['Выработка сегодня','kWh'],energy_total:['Выработка всего','kWh']
};
function render(dev){
	var g={};
	var keys=Object.keys(dev.values||{});
	for(var i=0;i<keys.length;i++){
		var k=keys[i], v=dev.values[k];
		if(!sel[k]) continue;
		var gk = (k.indexOf('ac_')===0 || k==='grid_frequency') ? 'AC' : ((k.indexOf('pv')===0)?'PV':'Energy');
		if(!g[gk]) g[gk]=[];
		g[gk].push({lab:sel[k][0], val:v, uni:sel[k][1]});
	}
	var order=['PV','AC','Energy'];
	var rows='';
	for(var j=0;j<order.length;j++){
		var gk=order[j];
		if(!g[gk]) continue;
		var gTitle = gk==='PV' ? 'Входы PV' : (gk==='AC' ? 'Выход AC' : 'Энергия');
		rows += '<div class="group"><h3>' + gTitle + '</h3><div class="rows">';
		for(var m=0;m<g[gk].length;m++){
			var it=g[gk][m];
			rows += '<div class="row"><span class="k">' + esc(it.lab) + '</span><span class="v">' + esc(it.val) + ' <span class="u">' + esc(it.uni) + '</span></span></div>';
		}
		rows += '</div></div>';
	}
	return rows;
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
			document.getElementById('kpiSub').textContent=data.devices.length+' инверторов онлайн';
		}else{
			kpiEl.textContent='—';
			document.getElementById('kpiSub').textContent='Нет данных';
		}
		var cards=document.getElementById('cards');
		cards.innerHTML='';
		if(!data.devices.length){ cards.innerHTML='<div class="missing">No data in Redis</div>'; return; }
		for(var i=0;i<data.devices.length;i++){
			var d=data.devices[i];
			var el=document.createElement('div'); el.className='card';
			var ts=(d.timestamp||'').replace('T',' ').substring(0,19);
			var ok=d.values && Object.keys(d.values).length>0;
			var body=ok?render(d):'<div class="missing">No data</div>';
			el.innerHTML='<h2><span>'+esc(d.name)+'</span><span>'+(ok?'<span class="badge">online</span>':'')+'<span class="ts">'+esc(ts)+'</span></span></h2>'+body;
			cards.appendChild(el);
		}
	}catch(e){}
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
				limits:{ x:{ minRange: 60*1000 } }
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
	var btns=['btnToday','btnYesterday','btn7d'];
	for(var i=0;i<btns.length;i++) document.getElementById(btns[i]).classList.remove('active');
	if(activeBtn) document.getElementById(activeBtn).classList.add('active');
	loadAll();
}
async function loadAll(){
	await Promise.all([loadTotalChart(), loadChart()]);
}

// Суммарный график (одна линия)
function buildTotalChart(data){
	var pts=(data.total||[]).map(function(p){ return {x:new Date(p.t), y:p.v}; });
	var datasets=[{
		label:'Сумма',
		data:pts,
		borderColor:'#ffd166',
		backgroundColor:'#ffd166',
		pointRadius:2,
		pointHoverRadius:4,
		borderWidth:2,
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
			pointRadius:2,
			pointHoverRadius:4,
			borderWidth:2,
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
		document.getElementById('chartRange').textContent='Диапазон: '+fmt(data.from)+' — '+fmt(data.to);
		buildChart(data);
	}catch(e){}
}
document.getElementById('btnReset').addEventListener('click',function(){ if(window.powerChart) window.powerChart.resetZoom(); });

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

loadAll(); setInterval(loadAll,60000);

tick(); setInterval(tick,5000);
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
	for _, d := range devices {
		if v, ok := snapFloat(d.Values, "ac_active_power"); ok {
			total += v
		}
	}
	total = math.Round(total*10) / 10
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(currentResponse{
		GeneratedAt: time.Now().Format(time.RFC3339),
		TotalPower:  total,
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
	byIP := map[string]*deviceSeries{}
	ipToName := map[string]string{}
	for _, sn := range snaps {
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

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(res)
}

// sumActive агрегирует ac_active_power всех инверторов в бакеты, равные периоду
// опроса (pollPeriod), и возвращает точки суммарной мощности с дискретностью,
// соответствующей частоте опроса. Последняя точка приравнивается к сумме
// последних известных значений по каждому инвертору — так правый край графика
// совпадает с суммарной мощностью на цифровой плашке (/api/current total_power).
func sumActive(snaps []deviceSnapshot) []seriesPoint {
	step := int64(pollPeriod / time.Second) // бакет = период опроса
	type agg struct{ sum, count float64 }
	buckets := map[int64]*agg{}
	var order []int64
	type last struct{ v float64; t time.Time }
	latest := map[string]last{} // последнее значение по каждому инвертору
	var latestEnd time.Time
	for _, sn := range snaps {
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
		if _, ok := buckets[bidx]; !ok {
			buckets[bidx] = &agg{}
			order = append(order, bidx)
		}
		buckets[bidx].sum += v
		buckets[bidx].count++
	}
	sort.Slice(order, func(i, j int) bool { return order[i] < order[j] })
	out := make([]seriesPoint, 0, len(order))
	for _, bidx := range order {
		a := buckets[bidx]
		if a.count == 0 {
			continue
		}
		bt := time.Unix(bidx*step, 0)
		out = append(out, seriesPoint{
			T: bt.Format(time.RFC3339),
			V: math.Round(a.sum*10) / 10,
		})
	}
	// Последняя точка = сумма последних известных значений по инверторам (как плашка).
	if len(latest) > 0 && !latestEnd.IsZero() {
		var total float64
		for _, lp := range latest {
			total += lp.v
		}
		if len(out) > 0 {
			out[len(out)-1] = seriesPoint{
				T: latestEnd.Format(time.RFC3339),
				V: math.Round(total*10) / 10,
			}
		} else {
			out = append(out, seriesPoint{
				T: latestEnd.Format(time.RFC3339),
				V: math.Round(total*10) / 10,
			})
		}
	}
	return out
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
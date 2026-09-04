package main

import (
	"encoding/json"
	"html/template"
	"log"
	"net/http"
	"sort"
	"time"
)

// dashboardHandler — веб-дашборд: отдаёт HTML-страницу и JSON API с текущими
// параметрами и временными рядами всех инверторов, читаемых из Redis.
type dashboardHandler struct {
	store *redisStore
}

// currentResponse отвечает на GET /api/current.
type currentResponse struct {
	GeneratedAt string           `json:"generated_at"`
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
body { font-family: -apple-system, "Segoe UI", Roboto, sans-serif; background:#0f1115; color:#e6e6e6; margin:0; padding:20px; }
h1 { font-size:22px; margin:0 0 4px; }
.sub { color:#8a93a1; margin:0 0 20px; font-size:13px; }
#chartbox { background:#181c24; border:1px solid #252b36; border-radius:10px; padding:16px; margin-bottom:20px; }
#chartbox h2 { margin:0 0 8px; font-size:16px; }
.chart-toolbar { display:flex; align-items:center; gap:12px; margin-bottom:8px; font-size:13px; color:#8a93a1; }
.chart-toolbar button { background:#252b36; color:#e6e6e6; border:1px solid #333b49; border-radius:6px; padding:4px 10px; cursor:pointer; font-size:13px; }
.chart-toolbar button:hover { background:#2f3644; }
.chart-wrap { position:relative; height:340px; }
.cards { display:grid; grid-template-columns:repeat(auto-fill,minmax(300px,1fr)); gap:16px; }
.card { background:#181c24; border:1px solid #252b36; border-radius:10px; padding:16px; }
.card h2 { margin:0 0 12px; font-size:16px; display:flex; justify-content:space-between; align-items:center; }
.card .badge { font-size:12px; color:#0f1115; background:#3fb950; padding:2px 8px; border-radius:20px; }
.card .ts { font-size:12px; color:#8a93a1; font-weight:normal; }
.groups { display:flex; flex-direction:column; gap:12px; }
.group h3 { margin:0 0 6px; font-size:12px; color:#8a93a1; text-transform:uppercase; letter-spacing:.05em; }
.rows { display:flex; flex-direction:column; gap:4px; }
.row { display:flex; justify-content:space-between; font-size:14px; }
.row .k { color:#aab3bf; }
.row .v { font-variant-numeric:tabular-nums; font-weight:600; }
.row .u { color:#8a93a1; font-weight:400; font-size:12px; }
.missing { color:#6b7280; font-style:italic; }
</style>
</head>
<body>
<h1>SunReceiver</h1>
<p class="sub">Текущие параметры инверторов (из Redis, обновление каждые 5&nbsp;с)</p>

<div id="chartbox">
  <h2>Ac active power, W</h2>
  <div class="chart-toolbar">
    <span id="chartRange"></span>
    <button id="btnToday">Сегодня</button>
    <button id="btnReset">Сброс зума</button>
    <span>Зум: колесо / drag&ndash;панорама</span>
  </div>
  <div class="chart-wrap"><canvas id="powerChart"></canvas></div>
</div>

<div class="cards" id="cards"><div class="missing">Загрузка...</div></div>

<script>
'use strict';

// ---------- Утилиты ----------
function esc(s){ return String(s).replace(/[&<>"]/g,function(c){ return {'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;'}[c]; }); }
function fmt(t){ var d=new Date(t); function p(x){return (x<10?'0':'')+x;} return d.getFullYear()+'-'+p(d.getMonth()+1)+'-'+p(d.getDate())+' '+p(d.getHours())+':'+p(d.getMinutes()); }
function startOfToday(){ var d=new Date(); d.setHours(0,0,0,0); return d; }
function endOfToday(){ var d=new Date(); d.setHours(23,59,59,999); return d; }

// ---------- Табличка текущих параметров ----------
var sel = {
	pv1_voltage:['PV1','V'],pv1_current:['PV1 I','A'],pv1_power:['PV1 P','W'],
	pv2_voltage:['PV2','V'],pv2_current:['PV2 I','A'],pv2_power:['PV2 P','W'],
	ac_active_power:['Akt','W'],ac_reactive_power:['Reakt','var'],
	grid_frequency:['Freq','Hz'],
	l1_voltage:['L1','V'],l1_current:['L1 I','A'],
	l2_voltage:['L2','V'],l2_current:['L2 I','A'],
	l3_voltage:['L3','V'],l3_current:['L3 I','A'],
	energy_today:['Today','kWh'],energy_total:['Total','kWh']
};
function render(dev){
	var g={};
	var keys=Object.keys(dev.values||{});
	for(var i=0;i<keys.length;i++){
		var k=keys[i], v=dev.values[k];
		if(!sel[k]) continue;
		var gk = (k.indexOf('ac_')===0 || k==='grid_frequency') ? 'AC' : ((k.indexOf('pv')===0)?'PV':'Energy');
		if(!g[gk]) g[gk]=[];
		g[gk].push(sel[k][0] + ':' + v + ' ' + sel[k][1]);
	}
	var order=['PV','AC','Energy'];
	var rows='';
	for(var j=0;j<order.length;j++){
		var gk=order[j];
		if(!g[gk]) continue;
		rows += '<div class="group"><h3>' + gk + '</h3><div class="rows">';
		for(var m=0;m<g[gk].length;m++){
			var parts=g[gk][m].split(':');
			rows += '<div class="row"><span class="k">' + esc(parts[0]) + '</span><span class="v">' + esc(parts[1]) + '</span></div>';
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

// ---------- График ac_active_power ----------
Chart.register(ChartZoom);
var ctx=document.getElementById('powerChart').getContext('2d');
var powerChart=null;
var range={from:null,to:null};

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
			pointRadius:0,
			borderWidth:2,
			tension:0.2,
			fill:false
		});
	}
	powerChart=new Chart(ctx,{
		type:'line',
		data:{datasets:datasets},
		options:{
			responsive:true,
			maintainAspectRatio:false,
			interaction:{ mode:'index', intersect:false },
			animation:{ duration:300 },
			plugins:{
				legend:{ display:true, labels:{ boxWidth:20, padding:14 } },
				tooltip:{
					mode:'index',
					intersect:false,
					displayColors:true,
					callbacks:{
						title:function(items){ return items.length ? fmt(items[0].parsed.x) : ''; },
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
				x:{ type:'time', time:{ unit:'hour', displayFormats:{ hour:'HH:mm' }, tooltipFormat:'yyyy-MM-dd HH:mm' }, ticks:{ maxRotation:0, autoSkipPadding:20 } },
				y:{ beginAtZero:true, title:{ display:true, text:'W' } }
			}
		}
	});
}

async function loadChart(){
	if(!range.from){ range.from=startOfToday(); range.to=endOfToday(); }
	var fromIso=range.from.toISOString();
	var toIso=range.to.toISOString();
	var url='/api/series?from='+encodeURIComponent(fromIso)+'&to='+encodeURIComponent(toIso);
	try{
		var r=await fetch(url);
		if(!r.ok) return;
		var data=await r.json();
		document.getElementById('chartRange').textContent='Диапазон: '+fmt(data.from)+' — '+fmt(data.to);
		buildChart(data);
	}catch(e){}
}
document.getElementById('btnToday').addEventListener('click',function(){
	range={from:startOfToday(),to:endOfToday()};
	loadChart();
});
document.getElementById('btnReset').addEventListener('click',function(){
	if(powerChart) powerChart.resetZoom();
});
loadChart();
setInterval(loadChart,60000);

tick(); setInterval(tick,5000);
</script>
</body>
</html>`

var dashboardTmpl = template.Must(template.New("dash").Parse(dashboardPage))

func (h *dashboardHandler) index(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = dashboardTmpl.Execute(w, nil)
}

func (h *dashboardHandler) apiCurrent(w http.ResponseWriter, r *http.Request) {
	devices, err := h.store.Current()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	sort.SliceStable(devices, func(i, j int) bool { return devices[i].Name < devices[j].Name })
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(currentResponse{
		GeneratedAt: time.Now().Format(time.RFC3339),
		Devices:     devices,
	})
}

// apiSeries отдаёт временные ряды ac_active_power по инверторам за период [from, to].
// По умолчанию (без параметров или при ошибке парсинга) — текущие календарные сутки.
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

	snaps, err := h.store.QuerySeries(from, to)
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

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(res)
}

// dayBounds возвращает границы текущих календарных суток в зоне loc.
func dayBounds(now time.Time, loc *time.Location) (time.Time, time.Time) {
	y, m, d := now.In(loc).Date()
	start := time.Date(y, m, d, 0, 0, 0, 0, loc)
	end := start.AddDate(0, 0, 1).Add(-time.Nanosecond)
	return start, end
}

// serveDashboard запускает HTTP-сервер дашборда в отдельной горутине.
func serveDashboard(addr string, store *redisStore) {
	h := &dashboardHandler{store: store}
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
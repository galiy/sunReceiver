'use strict';
var SR_COARSE = ('ontouchstart' in window) || navigator.maxTouchPoints > 0 || (window.matchMedia && matchMedia('(pointer: coarse)').matches);

function fmt(t){ var d=new Date(t); function p(x){return (x<10?'0':'')+x;} return d.getFullYear()+'-'+p(d.getMonth()+1)+'-'+p(d.getDate())+' '+p(d.getHours())+':'+p(d.getMinutes()); }
function fmtSec(t){ var d=new Date(t); function p(x){return (x<10?'0':'')+x;} return d.getFullYear()+'-'+p(d.getMonth()+1)+'-'+p(d.getDate())+' '+p(d.getHours())+':'+p(d.getMinutes())+':'+p(d.getSeconds()); }
function startOfToday(){ var d=new Date(); d.setHours(0,0,0,0); return d; }
function endOfToday(){ var d=new Date(); d.setHours(23,59,59,999); return d; }

// ---------- Графики ----------
Chart.register(ChartZoom);

var preserveZoom=false;
var userZoomed=false; // true — пользователь зумнул/сдвинул (нестандартное окно); false — стандартный вид (правый край догоняет now)
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
		if(SR_COARSE || window.__srTouched) return; // touch: хинт со значениями отключён
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
		chart.data.datasets.forEach(function(ds,i){
			// Видимость: скрытые кликом по легенде линии не участвуют в хинте
			// (иначе их значения и «призрачные» точки видны на пустом поле).
			if(!chart.isDatasetVisible||!chart.isDatasetVisible(i)) return;
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
		['powerChart','totalChart','gridVChart','gridPChart','tempChart'].forEach(function(id){
			var c=window[id]||Chart.getChart(id);
			if(!c || c===fromChart) return;
			try{ c.zoomScale('x', {min:m, max:M}, 'none'); }catch(e){}
		});
	}finally{ zoomSyncing=false; }
}
// ---------- Перезагрузка данных после зума/сдвига ----------
// Окно X общее для всех графиков страницы. После завершения зума/панорамы
// (колесо, drag, pinch, свайп, двойной тап) данные удаляются и загружаются
// заново с бэкенда под новое окно; поля «С/по» обновляются.
var winReloadTimer=null, winReloadSrc=null;
function srWindowChanged(chartId, isResetFromGesture){
	if(winReloadTimer) clearTimeout(winReloadTimer);
	winReloadSrc=chartId;
	winReloadTimer=setTimeout(function(){
		winReloadTimer=null;
		var isReset = isResetFromGesture;
		var id=winReloadSrc; winReloadSrc=null;
		var c=window[id]||Chart.getChart(id);
		if(!c||!c.scales||!c.scales.x) return;
		var x=c.scales.x;
		if(!isFinite(x.min)||!isFinite(x.max)||x.max<=x.min) return;
		var from=new Date(x.min), to=new Date(x.max);
		if(Math.round(from.getTime())===Math.round(selRange.from.getTime()) && Math.round(to.getTime())===Math.round(selRange.to.getTime())) return;
		selRange.from=from; selRange.to=to; periodMode='custom';
		document.getElementById('fromPick').value=toInputDateTime(from);
		document.getElementById('toPick').value=toInputDateTime(to);
		document.getElementById('datePick').value=toInputDate(from);
		setActiveBtn(null);
		// Сброс зума (двойной тап) возвращает окно к стандартному виду: не
		// фиксировать его (preserveZoom) и не помечать как нестандартное — правый
		// край должен снова догонять now. От обычного зума отличается маркером.
		if(isReset){ preserveZoom=false; userZoomed=false; }
		else { preserveZoom=true; userZoomed=true; }
		loadAll();
	},300);
}
function renderChart(id, datasets, opts){
	var canvas=document.getElementById(id);
	var old=window[id];
	var saved={min:null,max:null};
	if(old && old.scales && old.scales.x && isFinite(old.scales.x.min) && isFinite(old.scales.x.max)){
		saved={min:old.scales.x.min, max:old.scales.x.max};
	}
	// После завершения зума/панорамы — перезагрузка данных под новое окно.
	if(opts && opts.plugins && opts.plugins.zoom){
		opts.plugins.zoom.zoom.onZoomComplete=function(){ srWindowChanged(id); };
		if(opts.plugins.zoom.pan) opts.plugins.zoom.pan.onPanComplete=function(){ srWindowChanged(id); };
	}
	if(old){ captureHidden(id, old); try{ old.destroy(); }catch(e){} }
	// При сохранении окна чарт создаём сразу С ЭТИМ окном: первый draw на полном
	// диапазоне через afterDraw(checkZoomSync) «раскачал бы» остальные чарты
	// транзиторным диапазоном — после обновления график «прыгал
	// обратно» на полный период.
	if(preserveZoom && saved.min!==null && saved.max!==null){
		opts.scales.x.min=saved.min; opts.scales.x.max=saved.max;
	}
	canvas.getContext('2d');
	window[id]=new Chart(canvas,{ type:'line', data:{datasets:datasets}, options:opts, plugins:[cursorTooltipPlugin] });
	if(!canvas.__srMLBound){ canvas.__srMLBound=true; canvas.addEventListener('mouseleave',function(){ delete hoverPix[id]; try{ window[id]&&window[id].update('none'); }catch(e){} }); }
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
		// На touch onHover отключён: Chart.js вызывает его на каждый touchmove
		// (каждый палец), и chart.update() + перерисовка хинта со значениями
		// мешали pinch-зуму. __srTouched — страховка, если SR_COARSE не сработал.
		onHover:function(event, elements, chart){
			if(SR_COARSE || window.__srTouched) return;
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
function toInputDateTime(d){ function p(x){return (x<10?'0':'')+x;} return d.getFullYear()+'-'+p(d.getMonth()+1)+'-'+p(d.getDate())+'T'+p(d.getHours())+':'+p(d.getMinutes()); }
function dtFromStr(s){ var d=String(s).split('T'), p=d[0].split('-').map(Number), t=(d[1]||'0:0').split(':').map(Number); return new Date(p[0],p[1]-1,p[2],t[0]||0,t[1]||0,0,0); }
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
	userZoomed=false; // пресет/поля «С/по» — возврат к стандартному виду (правый край догоняет now)
	setActiveBtn(activeBtn);
	var dFrom=dayStart(from);
	document.getElementById('fromPick').value=toInputDateTime(from);
	document.getElementById('toPick').value=toInputDateTime(to);
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
// loadAll загружает данные ОДНИМ запросом /api/series и строит по ним все
// графики страницы: ряды одни и те же (from/to общие), поэтому отдельные fetch
// на каждый график лишь дублировали чтение Redis/PG.
async function loadAll(){
	if(window.srRefresh && !window.srRefresh.isEnabled()) return;
	var url='/api/series?from='+encodeURIComponent(selRange.from.toISOString())+'&to='+encodeURIComponent(selRange.to.toISOString());
	var data;
	try{
		var r=await fetch(url);
		if(!r.ok) return;
		data=await r.json();
	}catch(e){ return; }
	var range='Диапазон: '+fmt(selRange.from)+' — '+fmt(selRange.to);
	document.getElementById('totalChartRange').textContent=range;
	document.getElementById('chartRange').textContent=range;
	document.getElementById('gridVChartRange').textContent=range;
	document.getElementById('gridPChartRange').textContent=range;
	document.getElementById('tempChartRange').textContent=range;
	buildTotalChart(data);
	buildChart(data);
	buildGridVChart(data);
	buildGridPChart(data);
	buildTempChart(data);
}

// Суммарный график
function buildTotalChart(data){
	var pts=(data.total||[]).map(function(p){ return {x:new Date(p.t), y:p.v}; });
	var datasets=[{ label:'Сумма', data:pts, borderColor:'#428bca', backgroundColor:'#428bca',
		pointRadius:0, pointHoverRadius:0, borderWidth:1.5, tension:0.35, cubicInterpolationMode:'monotone', fill:false }];
	renderChart('totalChart', datasets, chartOpts(false,'W'));
	return window.totalChart;
}
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

// Температуры всех устройств (инверторы Deye/Sofar + МАП), °C. По одной линии на
// датчик («Имя — Корпус/Транзисторы», «МАП — Батарея/Тор/Транзисторы»). Данные —
// только из Redis (в PG температуры не усредняются), за старый период линий нет.
function buildTempChart(data){
	var datasets=[];
	var temps=data.temps||[];
	for(var i=0;i<temps.length;i++){
		var s=temps[i];
		var pts=s.points.map(function(p){ return {x:new Date(p.t), y:p.v}; });
		datasets.push({ label:s.name, data:pts, borderColor:s.color, backgroundColor:s.color,
			pointRadius:0, pointHoverRadius:0, borderWidth:1.5, tension:0.35, cubicInterpolationMode:'monotone', fill:false });
	}
	renderChart('tempChart', datasets, chartOpts(true,'°C',{
		scales:{ y:{ beginAtZero:false, title:{display:true, text:'Температура, °C'} } }
	}));
	lgKit('tempChart','tempChartLg').build(window.tempChart);
	return window.tempChart;
}

// Напряжения (МАП + счётчик). Счётчик на левой оси (белая линия).
function buildGridVChart(data){
	var grid=(data.map_grid_voltage||[]).map(function(p){ return {x:new Date(p.t), y:p.v}; });
	var bat=(data.map_battery_voltage||[]).map(function(p){ return {x:new Date(p.t), y:p.v}; });
	var meter=(data.meter_voltage||[]).map(function(p){ return {x:new Date(p.t), y:p.v}; });
	var ce1=(data.ce308_l1_voltage||[]).map(function(p){ return {x:new Date(p.t), y:p.v}; });
	var ce2=(data.ce308_l2_voltage||[]).map(function(p){ return {x:new Date(p.t), y:p.v}; });
	var ce3=(data.ce308_l3_voltage||[]).map(function(p){ return {x:new Date(p.t), y:p.v}; });
	var datasets=[
		{ label:'Напряжение сети', data:grid, borderColor:'#428bca', backgroundColor:'#428bca',
		  pointRadius:0, pointHoverRadius:0, borderWidth:1.5, tension:0.35, cubicInterpolationMode:'monotone', fill:false },
		{ label:'Напряжение батареи', data:bat, borderColor:'#d9534f', backgroundColor:'#d9534f',
		  pointRadius:0, pointHoverRadius:0, borderWidth:1.5, tension:0.35, cubicInterpolationMode:'monotone', fill:false,
		  yAxisID:'y1' },
		{ label:'Напряжение счётчика', data:meter, borderColor:'#6b7785', backgroundColor:'#6b7785',
		  pointRadius:0, pointHoverRadius:0, borderWidth:1.5, tension:0.35, cubicInterpolationMode:'monotone', fill:false }
	];
	// Фазные напряжения CE308 (опрос по BLE) — на левую ось (как напряжение сети).
	function addIfPts(dataset){ if(dataset.data.length) datasets.push(dataset); }
	addIfPts({ label:'CE308 L1', data:ce1, borderColor:'#20c997', backgroundColor:'#20c997',
	  pointRadius:0, pointHoverRadius:0, borderWidth:1.5, tension:0.35, cubicInterpolationMode:'monotone', fill:false });
	addIfPts({ label:'CE308 L2', data:ce2, borderColor:'#e8590c', backgroundColor:'#e8590c',
	  pointRadius:0, pointHoverRadius:0, borderWidth:1.5, tension:0.35, cubicInterpolationMode:'monotone', fill:false });
	addIfPts({ label:'CE308 L3', data:ce3, borderColor:'#7048e8', backgroundColor:'#7048e8',
	  pointRadius:0, pointHoverRadius:0, borderWidth:1.5, tension:0.35, cubicInterpolationMode:'monotone', fill:false });
	renderChart('gridVChart', datasets, chartOpts(true,'V',{
		scales:{ y1:{ type:'linear', position:'right', beginAtZero:false, title:{display:true, text:'Напряжение батареи, V'} } }
	}));
	lgKit('gridVChart','gridVChartLg').build(window.gridVChart);
	return window.gridVChart;
}

// Мощности (МАП + счётчик), знаки — по правилу «Мощности дома» (как на странице
// анимации Дома): потребление +, выработка/отдача в сеть −. Мощность батареи
// инвертирована (−battery_power: отдача/разряд = минус, заряд = плюс), как в
// анимации Дома («батарея→МАП»). Активная мощность счётчика — белой линией.
function buildGridPChart(data){
	var grid=(data.map_grid_power||[]).map(function(p){ return {x:new Date(p.t), y:p.v}; });
	// Знак «Мощности батареи» — как в анимации Дома (−battery_power).
	var bat=(data.map_battery_power||[]).map(function(p){ return {x:new Date(p.t), y:-p.v}; });
	// «Мощность дома» — по формуле анимации Дома: Σac(инверторы Дома)+grid+battery.
	var house=(data.house_power||[]).map(function(p){ return {x:new Date(p.t), y:p.v}; });
	// Вклад Дома в формулу: суммарная активная мощность сетевых инверторов Дома.
	var houseAc=(data.house_inverter_power||[]).map(function(p){ return {x:new Date(p.t), y:p.v}; });
	var meter=(data.meter_active_power||[]).map(function(p){ return {x:new Date(p.t), y:p.v}; });
	var ce308=(data.ce308_active_power||[]).map(function(p){ return {x:new Date(p.t), y:p.v}; });
	var datasets=[
		{ label:'Мощность сети', data:grid, borderColor:'#428bca', backgroundColor:'#428bca',
		  pointRadius:0, pointHoverRadius:0, borderWidth:1.5, tension:0.35, cubicInterpolationMode:'monotone', fill:false },
		{ label:'Мощность батареи', data:bat, borderColor:'#37b24d', backgroundColor:'#37b24d',
		  pointRadius:0, pointHoverRadius:0, borderWidth:1.5, tension:0.35, cubicInterpolationMode:'monotone', fill:false },
		{ label:'Инверторы Дома (Σ)', data:houseAc, borderColor:'#9463b8', backgroundColor:'#9463b8',
		  pointRadius:0, pointHoverRadius:0, borderWidth:1.5, tension:0.35, cubicInterpolationMode:'monotone', fill:false },
		{ label:'Мощность дома', data:house, borderColor:'#f08c00', backgroundColor:'#f08c00',
		  pointRadius:0, pointHoverRadius:0, borderWidth:1.5, tension:0.35, cubicInterpolationMode:'monotone', fill:false },
		{ label:'Мощность счётчика (активная)', data:meter, borderColor:'#6b7785', backgroundColor:'#6b7785',
		  pointRadius:0, pointHoverRadius:0, borderWidth:1.5, tension:0.2, cubicInterpolationMode:'monotone', fill:false }
	];
	// Суммарная активная мощность CE308 (опрос по BLE). Линия добавляется только
	// при наличии точек (счётчик мог быть не настроен/не опрошен).
	if(ce308.length) datasets.push({ label:'CE308 (активная, Σ)', data:ce308, borderColor:'#e64980', backgroundColor:'#e64980',
	  pointRadius:0, pointHoverRadius:0, borderWidth:1.5, tension:0.35, cubicInterpolationMode:'monotone', fill:false });
	renderChart('gridPChart', datasets, chartOpts(true,'W'));
	lgKit('gridPChart','gridPChartLg').build(window.gridPChart);
	return window.gridPChart;
}

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
	setPeriod(dtFromStr(f.value), dtFromStr(t.value), 'custom', null);
});
document.getElementById('btnRefresh').addEventListener('click',function(){
	preserveZoom=false;
	userZoomed=false; // принудительное обновление — возврат к стандартному виду
	loadAll();
});

document.getElementById('fromPick').value=toInputDateTime(selRange.from);
document.getElementById('toPick').value=toInputDateTime(selRange.to);
document.getElementById('datePick').value=toInputDate(selRange.from);
// Периодическое обновление графиков (раз в минуту) управляется глобальным
// выключателем обновления: enable — немедленный опрос + интервал, disable — остановка.
var chartsTimer=null;
function chartsStart(){ if(chartsTimer) return; loadAll(); chartsTimer=setInterval(function(){ preserveZoom=userZoomed; loadAll(); },60000); }
function chartsStop(){ if(chartsTimer){ clearInterval(chartsTimer); chartsTimer=null; } }
if(window.srRefresh){ window.srRefresh.register(chartsStart, chartsStop); }
chartsStart();
// Мобильная версия: touch-жесты по графикам (щипок — зум по X, свайп —
// панорама, двойной тап — сброс зума; хинт со значениями на мобильной
// отключён — он мешал зуму).
if(SR_COARSE){
	['powerChart','totalChart','gridVChart','gridPChart','tempChart'].forEach(function(id){
		srTouchChart(function(){ return window[id]; }, id, 60*1000, null, function(isReset){ srWindowChanged(id, isReset); });
	});
}

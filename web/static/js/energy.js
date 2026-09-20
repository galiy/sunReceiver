'use strict';
var SR_COARSE = ('ontouchstart' in window) || navigator.maxTouchPoints > 0 || (window.matchMedia && matchMedia('(pointer: coarse)').matches);
var TARIFF_COLORS={ import_day:'#d9534f', import_night:'#9463b8', export_day:'#37b24d', export_night:'#2c8f6a' };
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

function toInputDateTime(d){ function p(x){return (x<10?'0':'')+x;} return d.getFullYear()+'-'+p(d.getMonth()+1)+'-'+p(d.getDate())+'T'+p(d.getHours())+':'+p(d.getMinutes()); }
function dayStart(d){ var r=new Date(d); r.setHours(0,0,0,0); return r; }
function dtFromStr(s){ var d=String(s).split('T'), p=d[0].split('-').map(Number), t=(d[1]||'0:0').split(':').map(Number); return new Date(p[0],p[1]-1,p[2],t[0]||0,t[1]||0,0,0); }
function endOfDay(d){ var e=new Date(d); e.setHours(23,59,59,999); return e; }
function startOfMonth(){ var d=new Date(); return new Date(d.getFullYear(),d.getMonth(),1,0,0,0,0); }
function endOfMonth(){ var d=new Date(); return new Date(d.getFullYear(),d.getMonth()+1,0,23,59,59,999); }
function startOfYear(){ var d=new Date(); return new Date(d.getFullYear(),0,1,0,0,0,0); }
function endOfYear(){ var d=new Date(); return new Date(d.getFullYear(),11,31,23,59,59,999); }

// energyOpts — опции бар-графика на time-шкале (как chartOpts на /charts, но для
// тарифов). Time-шкала (а не category) — чтобы зум/сдвиг работали за пределы
// загруженных данных: окно X — время, а после жеста данные перезагружаются под
// новое окно (energyWindowChanged). Зум/сдвиг только по X.
function energyOpts(unit, minRange){
	return {
		responsive:true, maintainAspectRatio:false,
		interaction:{ mode:'index', intersect:false },
		animation:{ duration:300 },
		plugins:{
			legend:{ display:false },
			// На touch встроенный tooltip отключён (хинт по тапу); __srTouched —
			// страховка, если SR_COARSE на устройстве не сработал.
			tooltip:{ enabled:!(SR_COARSE || window.__srTouched) },
			zoom:{
				pan:{ enabled:!SR_COARSE, mode:'x' },
				zoom:{ wheel:{ enabled:!SR_COARSE, speed:0.1, modifierKey:'ctrl' }, pinch:{ enabled:!SR_COARSE }, mode:'x' },
				limits:{ x:{ minRange:minRange } }
			}
		},
		scales:{
			x:{ type:'time', time:{ unit:unit, displayFormats:{ day:'dd.MM', week:'dd.MM', month:'MM.yy', year:'yyyy' } }, ticks:{ maxRotation:0, autoSkip:true, maxTicksLimit:24 } },
			y:{ beginAtZero:true, title:{ display:true, text:'kWh' } }
		}
	};
}
// renderEnergyChart — бар-график на time-шкале. Как renderChart на /charts:
// сохранённое окно (old.scales.x.min/max) вшивается ДО и ПОСЛЕ new Chart(), когда
// preserveZoom=true (перезагрузка под новое окно после зума/сдвига), иначе чарт
// «прыгнул бы» на полный диапазон данных.
function renderEnergyChart(canvasId, datasets, opts){
	var canvas=document.getElementById(canvasId);
	var old=window[canvasId];
	var saved={min:null,max:null};
	if(old&&old.scales&&old.scales.x&&isFinite(old.scales.x.min)&&isFinite(old.scales.x.max)){ saved={min:old.scales.x.min, max:old.scales.x.max}; }
	if(opts&&opts.plugins&&opts.plugins.zoom){
		opts.plugins.zoom.zoom.onZoomComplete=function(){ energyWindowChanged(canvasId); };
		if(opts.plugins.zoom.pan) opts.plugins.zoom.pan.onPanComplete=function(){ energyWindowChanged(canvasId); };
	}
	if(old){ captureHidden(canvasId, old); try{ old.destroy(); }catch(e){} }
	var pan=ENERGY_PANELS[canvasId];
	if(pan&&pan.p.preserveZoom&&saved.min!==null&&saved.max!==null){ opts.scales.x.min=saved.min; opts.scales.x.max=saved.max; }
	canvas.getContext('2d');
	window[canvasId]=new Chart(canvas,{ type:'bar', data:{datasets:datasets}, options:opts });
	var need=false;
	if(pan&&pan.p.preserveZoom&&saved.min!==null&&saved.max!==null){ window[canvasId].options.scales.x.min=saved.min; window[canvasId].options.scales.x.max=saved.max; need=true; }
	if(applyHidden(canvasId, window[canvasId])) need=true;
	if(need){ window[canvasId].update('none'); }
	lgKit(canvasId, canvasId+'Lg').build(window[canvasId]);
	return window[canvasId];
}

// dailyDatasets — 4 бар-датасета (потребление/отдача × день/ночь) на time-шкале:
// точка {x: полдень дня, y: значение}.
function dailyDatasets(days){
	function mk(key){ return days.map(function(d){ return { x:new Date(d.day+'T12:00:00'), y:d[key] }; }); }
	return [
		{ label:'Потребление день', data:mk('import_day'), backgroundColor:TARIFF_COLORS.import_day },
		{ label:'Потребление ночь', data:mk('import_night'), backgroundColor:TARIFF_COLORS.import_night },
		{ label:'Отдача день', data:mk('export_day'), backgroundColor:TARIFF_COLORS.export_day },
		{ label:'Отдача ночь', data:mk('export_night'), backgroundColor:TARIFF_COLORS.export_night }
	];
}
// monthlyDatasets — те же 4 датасета, но дни сгруппированы по месяцам (сумма);
// x — 1-е число месяца.
function monthlyDatasets(days){
	var m={};
	days.forEach(function(d){
		var k=d.day.slice(0,7);
		if(!m[k]) m[k]={ import_day:0, import_night:0, export_day:0, export_night:0 };
		m[k].import_day+=d.import_day; m[k].import_night+=d.import_night;
		m[k].export_day+=d.export_day; m[k].export_night+=d.export_night;
	});
	var keys=Object.keys(m).sort();
	function mk(key){ return keys.map(function(k){ var q=k.split('-'); return { x:new Date(+q[0], +q[1]-1, 1), y:m[k][key] }; }); }
	return [
		{ label:'Потребление день', data:mk('import_day'), backgroundColor:TARIFF_COLORS.import_day },
		{ label:'Потребление ночь', data:mk('import_night'), backgroundColor:TARIFF_COLORS.import_night },
		{ label:'Отдача день', data:mk('export_day'), backgroundColor:TARIFF_COLORS.export_day },
		{ label:'Отдача ночь', data:mk('export_night'), backgroundColor:TARIFF_COLORS.export_night }
	];
}

// initEnergyPanel создаёт независимо управляемый график со своим диапазоном.
// Каждый график (по дням / по месяцам) — полностью самостоятелен: свой период,
// свой зум/сдвиг и СВОЙ запрос /api/tariffs под своё окно (без синхронизации).
// cfg: { canvasId, statusId, fromEl, toEl, applyBtn, isMonthly, presets:[{btn,range}], defaultFrom, defaultTo }
var ENERGY_PANELS={};
var energyReloadTimer={};
function initEnergyPanel(cfg){
	var p={
		selFrom:cfg.defaultFrom(), selTo:cfg.defaultTo(),
		preserveZoom:false,
		fromEl:cfg.fromEl, toEl:cfg.toEl,
		presetIds:cfg.presets.map(function(pr){ return pr.btn; })
	};
	function setRange(from,to,activeBtn){
		p.selFrom=from; p.selTo=to; p.preserveZoom=false;
		p.presetIds.forEach(function(id){ document.getElementById(id).classList.remove('active'); });
		if(activeBtn) document.getElementById(activeBtn).classList.add('active');
		document.getElementById(p.fromEl).value=toInputDateTime(from);
		document.getElementById(p.toEl).value=toInputDateTime(to);
		return load();
	}
	async function load(){
		var url='/api/tariffs?from='+encodeURIComponent(p.selFrom.toISOString())+'&to='+encodeURIComponent(p.selTo.toISOString());
		var r=await fetch(url); if(!r.ok) return;
		var data=await r.json();
		var days=data.days||[];
		var st=document.getElementById(cfg.statusId);
		if(!days.length){ st.textContent='Нет финализированных дней за выбранный период'; }
		else{ st.textContent='Показано дней: '+days.length+(cfg.isMonthly?' (по месяцам)':''); }
		var ds=cfg.isMonthly?monthlyDatasets(days):dailyDatasets(days);
		renderEnergyChart(cfg.canvasId, ds, energyOpts(cfg.isMonthly?'month':'day', cfg.isMonthly?90*86400000:3*86400000));
	}
	p.load=load; p.setRange=setRange;
	ENERGY_PANELS[cfg.canvasId]={ p:p };
	cfg.presets.forEach(function(pr){
		document.getElementById(pr.btn).addEventListener('click',function(){
			document.getElementById(pr.btn).blur();
			var r=pr.range(); setRange(r.from, r.to, pr.btn);
		});
	});
	document.getElementById(cfg.applyBtn).addEventListener('click',function(){
		var f=document.getElementById(p.fromEl).value, t=document.getElementById(p.toEl).value;
		if(!f||!t) return;
		setRange(dtFromStr(f), dtFromStr(t), null);
	});
	// Инициализация диапазона и полей по умолчанию.
	document.getElementById(p.fromEl).value=toInputDateTime(p.selFrom);
	document.getElementById(p.toEl).value=toInputDateTime(p.selTo);
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
// Перезагрузка данных после зума/сдвига (каждый график независимо). Time-шкала:
// окно X — время; по завершении жеста (onZoomComplete/onPanComplete на десктопе,
// onGestureComplete на touch) 300ms-debounce → данные удаляются и грузятся
// заново под новое окно [x.min, x.max], окно сохраняется (preserveZoom). Каждый
// график запрашивает СВОЙ период /api/tariffs (день/месяц — независимы). Страж
// «окно == текущее» пропускает повторную перезагрузку (в т.ч. от resetZoom()).
function energyWindowChanged(canvasId){
	if(energyReloadTimer[canvasId]) clearTimeout(energyReloadTimer[canvasId]);
	energyReloadTimer[canvasId]=setTimeout(function(){
		delete energyReloadTimer[canvasId];
		var c=window[canvasId]; if(!c||!c.scales||!c.scales.x) return;
		var x=c.scales.x; if(!isFinite(x.min)||!isFinite(x.max)||x.max<=x.min) return;
		var from=new Date(x.min), to=new Date(x.max);
		var pan=ENERGY_PANELS[canvasId]; if(!pan) return;
		if(Math.round(from.getTime())===Math.round(pan.p.selFrom.getTime()) && Math.round(to.getTime())===Math.round(pan.p.selTo.getTime())) return;
		pan.p.selFrom=from; pan.p.selTo=to; pan.p.preserveZoom=true;
		document.getElementById(pan.p.fromEl).value=toInputDateTime(from);
		document.getElementById(pan.p.toEl).value=toInputDateTime(to);
		pan.p.presetIds.forEach(function(id){ document.getElementById(id).classList.remove('active'); });
		pan.p.load();
	},300);
}
// Мобильная версия: touch-жесты по тарифным графикам (щипок — зум по X,
// свайп — панорама, двойной тап — сброс; на touch плагин zoom отключён, жесты —
// srTouchChart). По завершении жеста — перезагрузка под новое окно.
if(SR_COARSE){
	['dailyTariffChart','monthlyTariffChart'].forEach(function(id){
		srTouchChart(function(){ return window[id]; }, id, 3*86400000, null, function(){ energyWindowChanged(id); });
	});
}

'use strict';
var SR_COARSE = ('ontouchstart' in window) || navigator.maxTouchPoints > 0 || (window.matchMedia && matchMedia('(pointer: coarse)').matches);
function esc(s){ return String(s).replace(/[&<>"]/g,function(c){ return {'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;'}[c]; }); }
function fmtNum(n,digits){ return isFinite(n)? n.toLocaleString('ru-RU',{maximumFractionDigits:digits}) : '—'; }

var NAME = decodeURIComponent(location.pathname.replace(/^\/bms\//,''));

// Цвет заполнения ячейки: max — красная, min — синяя, остальные — зелёные.
var CELL_COLOR={ max:'#d9534f', min:'#428bca', normal:'#37b24d' };
// Заполнение батарейки ячейки по напряжению (шкала LiFePO4 3.00–3.65 В).
function cellFillPct(v){ var p=(v-3.00)/(3.65-3.00)*100; return Math.max(4,Math.min(100,p)); }
// Индексы max/min из кадра BMS — 1-based (ячейка №1 = 1), а i в цикле — 0-based.
function cellColor(d,i){ if(i+1===d.max_cell_idx) return 'max'; if(i+1===d.min_cell_idx) return 'min'; return 'normal'; }

function renderKPIs(d){
  var soc=Math.max(0,Math.min(100,Number(d.soc)||0));
  var socCol= soc<20?'#d9534f':(soc<50?'#f08c00':(soc<80?'#f0ad4e':'#37b24d'));
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
    var col= v<15?'#428bca':(v<=40?'#37b24d':(v<=55?'#f08c00':'#d9534f'));
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
var CHART_COLORS=['#428bca','#5cb85c','#f0ad4e','#d9534f','#5bc0de','#9463b8','#7f8fa6','#17a2b8','#c3b91c','#e91e63','#6d9ee8','#f7b32b','#4c9f70','#9c6bcf','#4db6ac','#8d6e63'];
var BMS_CHART_IDS=['bmsCapChart','bmsVoltChart','bmsCurChart','bmsPwrChart','bmsCellsChart','bmsSpreadChart','bmsTempChart'];
function mkBmsDs(label,color,data){ return { label:label, data:data, borderColor:color, backgroundColor:color, pointRadius:0, pointHoverRadius:0, borderWidth:1.5, tension:0.35, cubicInterpolationMode:'monotone', fill:false }; }
function packVolt(p){ var s=0, c=p.cells_v||[]; for(var i=0;i<c.length;i++) s+=c[i]; return s; }
function fmtDate(t){ var d=new Date(t); function p(x){return (x<10?'0':'')+x;} return d.getFullYear()+'-'+p(d.getMonth()+1)+'-'+p(d.getDate())+' '+p(d.getHours())+':'+p(d.getMinutes()); }
function setBmsRangeLabels(){
  var txt='Диапазон: '+fmtDate(selRange.from)+' — '+fmtDate(selRange.to);
  BMS_CHART_IDS.forEach(function(id){ var el=document.getElementById(id+'Range'); if(el) el.textContent=txt; });
}

// ---------- Выбор периода (общий для всех графиков BMS) ----------
var preserveZoom=false;
var userZoomed=false; // true — зум/сдвиг (нестандартное окно); false — стандартный вид
var selRange={from:startOfToday(), to:endOfToday()};
var periodMode='day';
function startOfToday(){ var d=new Date(); d.setHours(0,0,0,0); return d; }
function endOfToday(){ var d=new Date(); d.setHours(23,59,59,999); return d; }
function startOfYesterday(){ var d=new Date(); d.setDate(d.getDate()-1); d.setHours(0,0,0,0); return d; }
function dayFromStr(s){ var p=String(s).split('-').map(Number); return new Date(p[0], p[1]-1, p[2], 0,0,0,0); }
function toInputDate(d){ function p(x){return (x<10?'0':'')+x;} return d.getFullYear()+'-'+p(d.getMonth()+1)+'-'+p(d.getDate()); }
function toInputDateTime(d){ function p(x){return (x<10?'0':'')+x;} return d.getFullYear()+'-'+p(d.getMonth()+1)+'-'+p(d.getDate())+'T'+p(d.getHours())+':'+p(d.getMinutes()); }
function dtFromStr(s){ var d=String(s).split('T'), p=d[0].split('-').map(Number), t=(d[1]||'0:0').split(':').map(Number); return new Date(p[0],p[1]-1,p[2],t[0]||0,t[1]||0,0,0); }
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
  userZoomed=false; // пресет/поля «С/по» — возврат к стандартному виду
  setActiveBtn(activeBtn);
  var dFrom=dayStart(from);
  document.getElementById('fromPick').value=toInputDateTime(from);
  document.getElementById('toPick').value=toInputDateTime(to);
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
// Вертикальная тёмная чёрточка по курсору на графиках BMS (только десктоп).
// Позиция хранится как значение по оси X — поэтому линия рисуется сразу на
// ВСЕХ графиках страницы, когда курсор находится над одним из них.
var bmsCursor={ active:false, value:null };
function drawBmsCursor(chart){
  try{
    if(SR_COARSE || window.__srTouched) return;
    if(!bmsCursor.active || !isFinite(bmsCursor.value)) return;
    var x=chart.scales&&chart.scales.x, y=chart.scales&&chart.scales.y;
    if(!x||!y) return;
    var px=x.getPixelForValue(bmsCursor.value);
    if(!isFinite(px)||px<x.left||px>x.right) return;
    var ctx=chart.ctx; ctx.save();
    ctx.beginPath(); ctx.moveTo(px,y.top); ctx.lineTo(px,y.bottom);
    ctx.strokeStyle='rgba(15,18,22,0.75)'; ctx.lineWidth=1; ctx.stroke();
    ctx.restore();
  }catch(e){}
}
function bmsUpdateAllCharts(){
  BMS_CHART_IDS.forEach(function(id){
    try{ BMS_CHARTS[id]&&BMS_CHARTS[id].update('none'); }catch(e){}
  });
}
var bmsZoomSyncPlugin={ id:'bmsZoomSync', afterDraw:function(chart){ try{ checkBmsZoomSync(chart); drawBmsCursor(chart); }catch(e){} } };
// ---------- Перезагрузка данных после зума/сдвига ----------
// Окно X общее для всех графиков BMS. После завершения зума/панорамы данные
// удаляются и загружаются заново с бэкенда под новое окно; поля «С/по»
// обновляются.
var bmsWinReloadTimer=null, bmsWinReloadSrc=null;
function bmsWindowChanged(chartId, isResetFromGesture){
  if(bmsWinReloadTimer) clearTimeout(bmsWinReloadTimer);
  bmsWinReloadSrc=chartId;
  bmsWinReloadTimer=setTimeout(function(){
    bmsWinReloadTimer=null;
    var isReset = isResetFromGesture;
    var id=bmsWinReloadSrc; bmsWinReloadSrc=null;
    var c=BMS_CHARTS[id];
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
    // Сброс зума (двойной тап) — окно возвращается к стандартному виду:
    // не фиксировать и не помечать как нестандартное (правый край догоняет now).
    if(isReset){ preserveZoom=false; userZoomed=false; }
    else { preserveZoom=true; userZoomed=true; }
    loadBmsCharts();
  },300);
}
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
  var bmsOpts={
    responsive:true, maintainAspectRatio:false,
    interaction:{ mode:'index', intersect:false },
    onHover:function(event,elements,chart){
      if(SR_COARSE || window.__srTouched) return;
      if(chart && chart.canvas && chart.scales && chart.scales.x){
        var v=chart.scales.x.getValueForPixel(event.x);
        if(isFinite(v)){
          bmsCursor.active=true; bmsCursor.value=v;
          bmsUpdateAllCharts(); return;
        }
      }
      // Не удалось определить значение — прячем линию на всех графиках.
      if(bmsCursor.active){ bmsCursor.active=false; bmsUpdateAllCharts(); }
    },
    animation:{ duration:200 },
    plugins:{
      legend: { display:false },
      zoom:{
        pan:{ enabled:!SR_COARSE, mode:'x', onPanComplete:function(){ bmsWindowChanged(id); } },
        zoom:{ wheel:{ enabled:!SR_COARSE, speed:0.1, modifierKey:'ctrl' }, pinch:{ enabled:!SR_COARSE }, mode:'x', onZoomComplete:function(){ bmsWindowChanged(id); } },
        limits:{ x:{ minRange: 5*60*1000 } }
      }
    },
    scales:{
      x:{ type:'time', time:{ unit:'hour', displayFormats:{ hour:'HH:mm' } }, ticks:{ maxRotation:0, autoSkipPadding:16 } },
      y:y
    }
  };
  // При сохранении окна чарт создаём сразу С ЭТИМ окном (как на странице
  // графиков): первый draw на полном диапазоне транзитно «раскачивает»
  // остальные графики страницы через afterDraw-синхронизацию.
  if(preserveZoom && saved.min!==null && saved.max!==null){
    bmsOpts.scales.x.min=saved.min; bmsOpts.scales.x.max=saved.max;
  }
  BMS_CHARTS[id]=new Chart(document.getElementById(id),{
    type:'line', data:{datasets:datasets},
    plugins:[bmsZoomSyncPlugin],
    options:bmsOpts
  });
  // Сброс чёрточки курсора при уходе мыши с графика (линия гаснет на всех).
  var bmsCanvasEl=document.getElementById(id);
  if(!bmsCanvasEl.__srMLBound){ bmsCanvasEl.__srMLBound=true; bmsCanvasEl.addEventListener('mouseleave',function(){ if(bmsCursor.active){ bmsCursor.active=false; bmsUpdateAllCharts(); } }); }
  // На touch встроенный tooltip Chart.js отключён (показывается по тапу — хинт).
  // __srTouched — страховка, если SR_COARSE на устройстве не сработал.
  if(SR_COARSE || window.__srTouched){ BMS_CHARTS[id].options.plugins.tooltip.enabled=false; }
  var needUpdate=false;
  if(bmsApplyHidden(id, BMS_CHARTS[id])) needUpdate=true;
  if(needUpdate){ BMS_CHARTS[id].update('none'); }
  if(legend){ lgKit(id, id+'Lg').build(BMS_CHARTS[id]); }
}
function buildBmsCharts(points){
  if(!points||!points.length) return;
  // 1. Заряд (SOC)
  bmsRender('bmsCapChart',[mkBmsDs('SOC','#428bca',points.map(function(p){ return {x:new Date(p.ts), y:p.soc}; }))],'%',false,false);
  // 2. Напряжение пакета (сумма ячеек)
  bmsRender('bmsVoltChart',[mkBmsDs('Пакет','#f0ad4e',points.map(function(p){ return {x:new Date(p.ts), y:packVolt(p)}; }))],'V',false,false);
  // 3. Ток (± ось посередине)
  bmsRender('bmsCurChart',[mkBmsDs('Ток','#5bc0de',points.map(function(p){ return {x:new Date(p.ts), y:p.current_a}; }))],'A',false,true);
  // 4. Мощность (± ось посередине)
  bmsRender('bmsPwrChart',[mkBmsDs('Мощность','#d9534f',points.map(function(p){ return {x:new Date(p.ts), y:p.power_w}; }))],'W',false,true);
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
  // 6. Разброс ячеек (max-min напряжения по снимку)
  bmsRender('bmsSpreadChart',[mkBmsDs('Разброс','#9463b8',points.map(function(p){
    var c=p.cells_v||[], mx=null, mn=null;
    for(var i=0;i<c.length;i++){ var v=c[i]; if(!isFinite(v)) continue; if(mx===null||v>mx)mx=v; if(mn===null||v<mn)mn=v; }
    return {x:new Date(p.ts), y:(mx!==null && mn!==null? mx-mn : null)};
  }))],'V',false,false);
  // 7. Температуры: батарея (T1/T2), силовые ключи (T3), плата (T4)
  var tnames=['T1 · Батарея 1','T2 · Батарея 2','T3 · Силовая плата','T4 · Плата управления'];
  var tcols=['#37b24d','#5cb85c','#f08c00','#9463b8'];
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
  setPeriod(dtFromStr(f.value), dtFromStr(t.value), 'custom', null);
});
document.getElementById('btnRefresh').addEventListener('click',function(){ preserveZoom=false; userZoomed=false; loadBmsCharts(); });
// Инициализация полей периода и первичная загрузка; дальше — раз в минуту (зум
// и выбор линий сохраняются), параметры батареи — каждую секунду.
document.getElementById('fromPick').value=toInputDateTime(selRange.from);
document.getElementById('toPick').value=toInputDateTime(selRange.to);
document.getElementById('datePick').value=toInputDate(selRange.from);
setActiveBtn('btnToday');
loadBmsCharts();
setInterval(function(){ preserveZoom=userZoomed; loadBmsCharts(); },60000);

// Мобильная версия: touch-жесты по графикам BMS (щипок — зум по X, свайп —
// панорама, двойной тап — сброс зума; tooltip на тап отключён — мешал зуму).
if(SR_COARSE){
  BMS_CHART_IDS.forEach(function(id){
    srTouchChart(function(){ return BMS_CHARTS[id]; }, id, 5*60*1000, null, function(isReset){ bmsWindowChanged(id, isReset); });
  });
}

load(); setInterval(load,1000);

// ---------- Изменение порядка графиков BMS (реактивная сетка 2 колонки) ----------
// Порядок карточек в .bms-charts можно менять стрелками вверх/вниз/влево/вправо.
// Макет — flex-wrap с 2 колонками (flex:1 1 46%), значит DOM-порядок == порядок
// row-major: чётный индекс — левая колонка, нечётный — правая; COLS=2.
// Вверх/вниз — сдвиг на COLS (±2, та же колонка), влево/вправо — на ±1 (соседняя
// колонка того же ряда). Порядок сохраняется в localStorage (bmsChartOrder) и
// восстанавливается при загрузке. Инстансы Chart.js привязаны к canvas по id,
// поэтому достаточно двигать DOM-карточки.
var BMS_ORDER_COLS=2;
function bmsRefreshOrderBtns(){
  var cards=document.querySelectorAll('.bms-charts > .card');
  for(var i=0;i<cards.length;i++){
    var up=cards[i].querySelector('.ord-up'), down=cards[i].querySelector('.ord-down');
    var left=cards[i].querySelector('.ord-left'), right=cards[i].querySelector('.ord-right');
    if(up) up.disabled=(i-BMS_ORDER_COLS<0);
    if(down) down.disabled=(i+BMS_ORDER_COLS>=cards.length);
    if(left) left.disabled=(i%BMS_ORDER_COLS!==1);
    if(right) right.disabled=(i%BMS_ORDER_COLS!==0 || i+1>=cards.length);
  }
}
function bmsSaveOrder(){
  var ids=[];
  document.querySelectorAll('.bms-charts > .card').forEach(function(c){
    var cv=c.querySelector('.chart-wrap canvas');
    if(cv) ids.push(cv.id);
  });
  try{ localStorage.setItem('bmsChartOrder', JSON.stringify(ids)); }catch(e){}
}
function bmsLoadOrder(){
  var ids=null;
  try{ ids=JSON.parse(localStorage.getItem('bmsChartOrder')||'null'); }catch(e){}
  if(!Array.isArray(ids) || !ids.length) return;
  var container=document.querySelector('.bms-charts');
  if(!container) return;
  var byId={};
  container.querySelectorAll('.card').forEach(function(c){
    var cv=c.querySelector('.chart-wrap canvas');
    if(cv) byId[cv.id]=c;
  });
  ids.forEach(function(id){ var c=byId[id]; if(c) container.appendChild(c); });
  container.querySelectorAll('.card').forEach(function(c){ container.appendChild(c); });
}
function bmsMoveCard(btn,dir){
  var card=btn.closest('.card');
  if(!card) return;
  var cards=card.parentNode.querySelectorAll('.card');
  var idx=Array.prototype.indexOf.call(cards,card);
  var target=-1;
  if(dir==='up') target=(idx-BMS_ORDER_COLS>=0)? idx-BMS_ORDER_COLS : -1;
  else if(dir==='down') target=(idx+BMS_ORDER_COLS<cards.length)? idx+BMS_ORDER_COLS : -1;
  else if(dir==='left') target=(idx%BMS_ORDER_COLS===1)? idx-1 : -1;
  else if(dir==='right') target=(idx%BMS_ORDER_COLS===0 && idx+1<cards.length)? idx+1 : -1;
  if(target<0) return;
  var ref=cards[target];
  if(target>idx) card.parentNode.insertBefore(card, ref.nextSibling);
  else card.parentNode.insertBefore(card, ref);
  bmsSaveOrder();
  bmsRefreshOrderBtns();
}
document.querySelectorAll('.bms-charts .ord-up').forEach(function(b){ b.addEventListener('click',function(){ bmsMoveCard(this,'up'); }); });
document.querySelectorAll('.bms-charts .ord-down').forEach(function(b){ b.addEventListener('click',function(){ bmsMoveCard(this,'down'); }); });
document.querySelectorAll('.bms-charts .ord-left').forEach(function(b){ b.addEventListener('click',function(){ bmsMoveCard(this,'left'); }); });
document.querySelectorAll('.bms-charts .ord-right').forEach(function(b){ b.addEventListener('click',function(){ bmsMoveCard(this,'right'); }); });
bmsLoadOrder();
bmsRefreshOrderBtns();

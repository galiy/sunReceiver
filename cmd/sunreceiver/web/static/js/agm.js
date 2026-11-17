'use strict';
// Страница AGM (свинцово-кислотный АКБ). Источник — собственный модуль AGM:
// PZEM-017 (напряжение, ток, мощность, накопленная энергия) и 4×BK300
// (напряжения батарей). PZEM отдаёт ток/мощность по модулю; знак выставляет
// бэкенд по напряжению МАП в тот же момент: выше напряжения сборки → заряд (−),
// иначе разряд (+). Управление, зум, связь графиков, хинты и легенда — как на
// странице BMS.
var SR_COARSE = ('ontouchstart' in window) || navigator.maxTouchPoints > 0 || (window.matchMedia && matchMedia('(pointer: coarse)').matches);
function fmtDate(t){ var d=new Date(t); function p(x){return (x<10?'0':'')+x;} return d.getFullYear()+'-'+p(d.getMonth()+1)+'-'+p(d.getDate())+' '+p(d.getHours())+':'+p(d.getMinutes()); }
function fmtNum(n,digits){ return isFinite(n)? n.toLocaleString('ru-RU',{maximumFractionDigits:digits}) : '—'; }
function agmSocColor(soc){ return soc<20?'#d9534f':(soc<50?'#f08c00':(soc<80?'#f0ad4e':'#37b24d')); }

// Плашки текущих значений: объективные данные PZEM-017 + напряжения 4×BK300.
// SOC — по таблице AGM от напряжения сборки PZEM (оценка остаётся грубой, т.к.
// таблица для напряжения покоя).
function renderAgmKPIs(d){
  var soc=Math.max(0,Math.min(100,Number(d.soc)||0));
  var iCls= d.current_a>=0? 'pos':'neg';
  var pCls= d.power_w>=0? 'pos':'neg';
  var objective=(d.source==='agm');
  var h='';
  h+='<div class="kpi"><div class="lbl">Заряд (SOC)</div><div class="val socval">'
    +'<span class="mini-batt"><span class="mini-batt-fill" style="width:'+soc+'%;background:'+agmSocColor(soc)+';display:block"></span></span>'
    +'<span>'+soc+'<span class="unit">%</span></span></div>'
    +'<div class="sub">остаток '+fmtNum(d.remaining_ah,1)+' А·ч / '+fmtNum(d.capacity_ah,0)+' А·ч</div></div>';
  h+='<div class="kpi"><div class="lbl">Напряжение сборки</div><div class="val">'+fmtNum(d.voltage,2)+'<span class="unit">V</span></div>'
    +'<div class="sub">'+(objective?'PZEM-017':'с шины МАП')+'</div></div>';
  if(d.map_voltage_v!==null && d.map_voltage_v!==undefined){
    h+='<div class="kpi"><div class="lbl">Напряжение МАП</div><div class="val">'+fmtNum(d.map_voltage_v,2)+'<span class="unit">V</span></div>'
      +'<div class="sub">справочно</div></div>';
  }
  h+='<div class="kpi"><div class="lbl">Ток</div><div class="val '+iCls+'">'+fmtNum(d.current_a,1)+'<span class="unit">A</span></div>'
    +'<div class="sub">заряд &minus; / разряд +</div></div>';
  h+='<div class="kpi"><div class="lbl">Мощность</div><div class="val '+pCls+'">'+fmtNum(d.power_w,1)+'<span class="unit">W</span></div>'
    +'<div class="sub">заряд &minus; / разряд +</div></div>';
  h+='<div class="kpi"><div class="lbl">Энергия</div><div class="val">'+(objective?fmtNum(d.energy_wh,0)+'<span class="unit">Вт·ч</span>':'—')+'</div>'
    +'<div class="sub">'+(objective?'накопленная (PZEM-017)':'—')+'</div></div>';
  if(d.batteries_sum_v!==null && d.batteries_sum_v!==undefined){
    h+='<div class="kpi"><div class="lbl">Сумма отдельных батарей</div><div class="val">'+fmtNum(d.batteries_sum_v,2)+'<span class="unit">V</span></div>'
      +'<div class="sub">4×BK300</div></div>';
  }
  var bv=d.batteries_v||[];
  if(bv.length===4){
    var mn=Math.min.apply(null,bv), mx=Math.max.apply(null,bv);
    var parts=[];
    for(var i=0;i<4;i++) parts.push((i+1)+': '+fmtNum(bv[i],2));
    h+='<div class="kpi"><div class="lbl">Батареи</div><div class="val">'+fmtNum(mn,2)+'–'+fmtNum(mx,2)+'<span class="unit">V</span></div>'
      +'<div class="sub">'+parts.join(' · ')+'</div></div>';
  }else if(objective){
    h+='<div class="kpi"><div class="lbl">Батареи</div><div class="val">—</div><div class="sub">нет данных BK300</div></div>';
  }else{
    h+='<div class="kpi"><div class="lbl">Ёмкость</div><div class="val">'+fmtNum(d.capacity_ah,0)+'<span class="unit">А·ч</span></div></div>';
  }
  document.getElementById('agmKpiRow').innerHTML=h;
  var foot=document.getElementById('agmFoot');
  if(foot) foot.textContent=(objective?'PZEM-017 + 4×BK300':'расчёт МАП − Σ BMS')+' · обновляется каждую секунду';
}
async function loadAgmCurrent(){
  if(window.srRefresh && !window.srRefresh.isEnabled()) return;
  try{
    var r=await fetch('/api/agm');
    if(!r.ok) return;
    var d=await r.json();
    renderAgmKPIs(d);
  }catch(e){}
}

Chart.register(ChartZoom);
// Позиционер хинта (боковой) — как на странице BMS/«Графики».
function agmTooltipPos(elements, eventPosition){
  var x=(eventPosition&&isFinite(eventPosition.x))?eventPosition.x:0;
  var y=(eventPosition&&isFinite(eventPosition.y))?eventPosition.y:0;
  var xAlign='left';
  var area=this&&this.chart&&this.chart.chartArea;
  if(area&&x>=(area.left+area.right)/2) xAlign='right';
  return { x:x, y:y, xAlign:xAlign, yAlign:'center' };
}
(function(){
  try{
    var tp=Chart.registry&&Chart.registry.plugins&&Chart.registry.plugins.get('tooltip');
    if(tp&&tp.positioners) tp.positioners.agmSide=agmTooltipPos;
    else if(Chart.Tooltip&&Chart.Tooltip.positioners) Chart.Tooltip.positioners.agmSide=agmTooltipPos;
  }catch(e){}
})();

var CHART_COLORS=['#428bca','#5cb85c','#f0ad4e','#d9534f','#5bc0de','#9463b8','#7f8fa6','#17a2b8'];
var AGM_CHART_IDS=['agmVoltChart','agmBatChart','agmCurChart','agmPwrChart'];
function mkAgmDs(label,color,data){ return { label:label, data:data, borderColor:color, backgroundColor:color, pointRadius:0, pointHoverRadius:0, borderWidth:1.5, tension:0.35, cubicInterpolationMode:'monotone', fill:false, spanGaps:false }; }
function setAgmRangeLabels(){
  var txt='Диапазон: '+fmtDate(selRange.from)+' — '+fmtDate(selRange.to);
  AGM_CHART_IDS.forEach(function(id){ var el=document.getElementById(id+'Range'); if(el) el.textContent=txt; });
}

// ---------- Выбор периода (общий для всех графиков) ----------
var preserveZoom=false;
var userZoomed=false;
var selRange=(function(){ var to=new Date(); return {from:new Date(to.getTime()-24*60*60*1000), to:to}; })();
var periodMode='24h';
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
var PERIOD_BTNS=['btn24h','btnToday','btnYesterday','btn7d','btnMonth'];
function setActiveBtn(activeBtn){ for(var i=0;i<PERIOD_BTNS.length;i++) document.getElementById(PERIOD_BTNS[i]).classList.remove('active'); if(activeBtn) document.getElementById(activeBtn).classList.add('active'); }
function setPeriod(from,to,mode,activeBtn){
  selRange.from=from; selRange.to=to; periodMode=mode;
  preserveZoom=false;
  userZoomed=false;
  setActiveBtn(activeBtn);
  var dFrom=dayStart(from);
  document.getElementById('fromPick').value=toInputDateTime(from);
  document.getElementById('toPick').value=toInputDateTime(to);
  document.getElementById('datePick').value=toInputDate(mode==='day'?from:dFrom);
  loadAgmCharts();
}
function shiftPeriod(delta){
  var from=selRange.from, to=selRange.to;
  var newFrom, newTo;
  if(periodMode==='day'){ newFrom=addDays(dayStart(from), delta); newTo=endOfDay(newFrom); }
  else if(periodMode==='month'){ newFrom=addMonths(dayStart(from), delta); newTo=endOfMonthOf(newFrom); }
  else{ var span=to-from; newFrom=new Date(from.getTime()+delta*span); newTo=new Date(to.getTime()+delta*span); }
  setPeriod(newFrom,newTo,periodMode,null);
}

// ---------- Синхронизация зума, курсор и хинты (как на странице BMS) ----------
var lastXWindow={};
var zoomSyncing=false;
var agmRebuilding=false;
function checkAgmZoomSync(chart){
  if(zoomSyncing || agmRebuilding) return;
  var x=chart&&chart.scales&&chart.scales.x;
  if(!x||!isFinite(x.min)||!isFinite(x.max)||x.max<=x.min) return;
  var key=x.min.toFixed(3)+','+x.max.toFixed(3);
  if(lastXWindow[chart.canvas.id]!==undefined && lastXWindow[chart.canvas.id]!==key) syncAgmZoomToOthers(chart);
  lastXWindow[chart.canvas.id]=key;
}
function syncAgmZoomToOthers(fromChart){
  if(zoomSyncing) return;
  var sx=fromChart&&fromChart.scales&&fromChart.scales.x;
  if(!sx||!isFinite(sx.min)||!isFinite(sx.max)||sx.max<=sx.min) return;
  var m=sx.min, M=sx.max;
  zoomSyncing=true;
  try{
    AGM_CHART_IDS.forEach(function(id){
      var c=AGM_CHARTS[id];
      if(!c || c===fromChart) return;
      try{ c.zoomScale('x',{min:m,max:M},'none'); }catch(e){}
    });
  }finally{ zoomSyncing=false; }
}
var agmCursor={ active:false, value:null };
var AGM_HOVER_HINT_DELAY=300;
var agmHoverTimer=null, agmHoverPx=null;
function agmHideHintsNow(){
  if(agmCursor.active){ agmCursor.active=false; agmSyncTooltips(null); agmUpdateAllCharts(); }
}
function agmClearHover(){
  if(agmHoverTimer){ clearTimeout(agmHoverTimer); agmHoverTimer=null; }
  agmHoverPx=null;
  agmHideHintsNow();
}
function agmShowHint(v){
  agmHoverTimer=null;
  agmCursor.active=true; agmCursor.value=v;
  agmSyncTooltips(v);
  agmUpdateAllCharts();
}
function drawAgmCursor(chart){
  try{
    if(SR_COARSE || window.__srTouched) return;
    if(!agmCursor.active || !isFinite(agmCursor.value)) return;
    var x=chart.scales&&chart.scales.x, y=chart.scales&&chart.scales.y;
    if(!x||!y) return;
    var px=x.getPixelForValue(agmCursor.value);
    if(!isFinite(px)||px<x.left||px>x.right) return;
    var ctx=chart.ctx; ctx.save();
    ctx.beginPath(); ctx.moveTo(px,y.top); ctx.lineTo(px,y.bottom);
    ctx.strokeStyle='rgba(15,18,22,0.75)'; ctx.lineWidth=1; ctx.stroke();
    ctx.restore();
  }catch(e){}
}
function agmUpdateAllCharts(){
  AGM_CHART_IDS.forEach(function(id){ try{ AGM_CHARTS[id]&&AGM_CHARTS[id].update('none'); }catch(e){} });
}
function agmNearestIndex(chart, t){
  try{
    var ds=(chart.data&&chart.data.datasets)||[];
    if(!ds.length) return -1;
    var arr=ds[0].data||[];
    if(!arr.length) return -1;
    var hi=arr.length-1;
    if(t<=new Date(arr[0].x).getTime()) return 0;
    if(t>=new Date(arr[hi].x).getTime()) return hi;
    var lo=0;
    while(lo<=hi){
      var mid=(lo+hi)>>1, xm=new Date(arr[mid].x).getTime();
      if(xm===t) return mid;
      if(xm<t) lo=mid+1; else hi=mid-1;
    }
    var a=new Date(arr[hi].x).getTime(), b=new Date(arr[lo].x).getTime();
    return (t-a)<=(b-t)? hi : lo;
  }catch(e){ return -1; }
}
function agmSyncTooltips(t){
  AGM_CHART_IDS.forEach(function(id){
    var c=AGM_CHARTS[id];
    if(!c||!c.scales||!c.scales.x||!c.tooltip) return;
    try{
      if(t===null||!isFinite(t)){ c.tooltip.setActiveElements([],{x:0,y:0}); return; }
      var idx=agmNearestIndex(c,t);
      if(idx<0){ c.tooltip.setActiveElements([],{x:0,y:0}); return; }
      var els=[], py=null;
      c.data.datasets.forEach(function(ds,i){
        if(!c.isDatasetVisible(i)) return;
        var p=ds.data[idx];
        if(p && p.y!==null && p.y!==undefined){
          els.push({datasetIndex:i, index:idx});
          if(py===null){ var v=c.scales.y.getPixelForValue(p.y); if(isFinite(v)) py=v; }
        }
      });
      var px=c.scales.x.getPixelForValue(t);
      c.tooltip.setActiveElements(els,{x:px, y:(py!==null?py:c.chartArea.top)});
    }catch(e){}
  });
}
var agmZoomSyncPlugin={ id:'agmZoomSync', afterDraw:function(chart){ try{ checkAgmZoomSync(chart); drawAgmCursor(chart); }catch(e){} } };

// ---------- Перезагрузка данных после зума/сдвига ----------
var agmWinReloadTimer=null, agmWinReloadSrc=null;
function agmWindowChanged(chartId, isResetFromGesture){
  if(agmWinReloadTimer) clearTimeout(agmWinReloadTimer);
  agmWinReloadSrc=chartId;
  agmWinReloadTimer=setTimeout(function(){
    agmWinReloadTimer=null;
    var isReset = isResetFromGesture;
    var id=agmWinReloadSrc; agmWinReloadSrc=null;
    var c=AGM_CHARTS[id];
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
    if(isReset){ preserveZoom=false; userZoomed=false; }
    else { preserveZoom=true; userZoomed=true; }
    loadAgmCharts();
  },300);
}

// lgKit — HTML-легенда-чипы (как на странице BMS): ЛКМ — вкл/выкл линию,
// двойной ЛКМ — только эта линия, «Все» — показать все.
function agmLgKit(canvasId, chipsId){
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

var AGM_CHARTS={};
var agmHiddenSets={};
var agmSavedZoom={};
function agmCaptureState(){
  Object.keys(AGM_CHARTS).forEach(function(id){
    var c=AGM_CHARTS[id];
    if(!c) return;
    if(c.data && c.data.datasets){
      var hidden=[];
      // Индексы, а не подписи: i18n переводит ds.label, и после обновления
      // русская подпись не совпала бы с переведённой.
      c.data.datasets.forEach(function(ds,i){ if(!c.isDatasetVisible(i)) hidden.push(i); });
      agmHiddenSets[id]=hidden;
    }
    if(c.scales && c.scales.x && isFinite(c.scales.x.min) && isFinite(c.scales.x.max)){
      agmSavedZoom[id]={min:c.scales.x.min, max:c.scales.x.max};
    }
  });
}
function agmApplyHidden(id, chart){
  var hidden=agmHiddenSets[id]||[];
  if(!hidden.length || !chart || !chart.data || !chart.data.datasets) return false;
  hidden.forEach(function(i){ if(chart.data.datasets[i]) chart.setDatasetVisibility(i,false); });
  return true;
}
// agmRender — линейный график; zero=true — симметричная ось Y относительно нуля
// (ток/мощность); legend=true — HTML-легенда с чипами (для 4 батарей).
function agmRender(id, datasets, yTitle, legend, zero){
  var saved=agmSavedZoom[id]||{min:null,max:null};
  var y={ beginAtZero:false, title:{ display:!!yTitle, text:yTitle||'' } };
  if(zero){
    var m=0;
    datasets.forEach(function(ds){ (ds.data||[]).forEach(function(p){ if(isFinite(p.y)){ var a=Math.abs(p.y); if(a>m)m=a; } }); });
    m=m>0?m:1; y.min=-m; y.max=m;
  }
  var opts={
    responsive:true, maintainAspectRatio:false,
    interaction:{ mode:'index', intersect:false },
    onHover:function(event,elements,chart){
      if(SR_COARSE || window.__srTouched) return;
      if(chart && chart.canvas && chart.scales && chart.scales.x && event && isFinite(event.x)){
        var xScale=chart.scales.x;
        if(event.x<xScale.left || event.x>xScale.right){ agmClearHover(); return; }
        var v=xScale.getValueForPixel(event.x);
        if(isFinite(v)){
          var moved=(agmHoverPx===null || Math.abs(event.x-agmHoverPx)>0.5);
          agmHoverPx=event.x;
          if(moved){
            if(agmCursor.active){ agmCursor.active=false; agmSyncTooltips(null); agmUpdateAllCharts(); }
            if(agmHoverTimer) clearTimeout(agmHoverTimer);
            agmHoverTimer=setTimeout(function(){ agmShowHint(v); }, AGM_HOVER_HINT_DELAY);
          }else if(!agmCursor.active && !agmHoverTimer){
            agmHoverTimer=setTimeout(function(){ agmShowHint(v); }, AGM_HOVER_HINT_DELAY);
          }
          return;
        }
      }
      agmClearHover();
    },
    animation:{ duration:200 },
    plugins:{
      legend: { display:false },
      tooltip:{ position:'agmSide', caretPadding:30, backgroundColor:'rgba(255,255,255,1)', titleColor:'#111', bodyColor:'#111', borderColor:'rgba(0,0,0,0.35)', borderWidth:1 },
      zoom:{
        pan:{ enabled:!SR_COARSE, mode:'x', onPanComplete:function(){ agmWindowChanged(id); } },
        zoom:{ wheel:{ enabled:!SR_COARSE, speed:0.1, modifierKey:'ctrl' }, pinch:{ enabled:!SR_COARSE }, mode:'x', onZoomComplete:function(){ agmWindowChanged(id); } },
        limits:{ x:{ minRange: 5*60*1000 } }
      }
    },
    scales:{
      x:{ type:'time', time:{ displayFormats:{ millisecond:'HH:mm:ss.SSS', second:'HH:mm:ss', minute:'HH:mm', hour:'HH:mm', day:'dd.MM.yyyy', week:'dd.MM.yyyy', month:'MM.yyyy', quarter:'MM.yyyy', year:'yyyy' }, tooltipFormat:'dd.MM.yyyy HH:mm:ss' }, ticks:{ maxRotation:0, autoSkipPadding:16 } },
      y:y
    }
  };
  if(preserveZoom && saved.min!==null && saved.max!==null){
    opts.scales.x.min=saved.min; opts.scales.x.max=saved.max;
  }
  AGM_CHARTS[id]=new Chart(document.getElementById(id),{
    type:'line', data:{datasets:datasets},
    plugins:[agmZoomSyncPlugin],
    options:opts
  });
  srCtrlDragZoom(AGM_CHARTS[id], function(f,t){ setPeriod(f,t,'custom',null); });
  var el=document.getElementById(id);
  if(!el.__srMLBound){ el.__srMLBound=true; el.addEventListener('mouseleave',function(){ agmClearHover(); }); }
  if(SR_COARSE || window.__srTouched){ AGM_CHARTS[id].options.plugins.tooltip.enabled=false; }
  if(agmApplyHidden(id, AGM_CHARTS[id])) AGM_CHARTS[id].update('none');
  if(legend){ agmLgKit(id, id+'Lg').build(AGM_CHARTS[id]); }
}
function buildAgmCharts(points){
  points=points||[];
  function yVal(p, field){ return p['break']? null : p[field]; }
  agmRender('agmVoltChart',[
    mkAgmDs('Напряжение сборки','#f0ad4e',points.map(function(p){ return {x:new Date(p.ts), y:yVal(p,'voltage_v')}; })),
    mkAgmDs('Напряжение МАП','#5cb85c',points.map(function(p){ return {x:new Date(p.ts), y:(p['break']||p.map_voltage_v===null||p.map_voltage_v===undefined)? null : p.map_voltage_v}; })),
    mkAgmDs('Сумма отдельных батарей','#428bca',points.map(function(p){ return {x:new Date(p.ts), y:(p['break']||p.batteries_sum_v===null||p.batteries_sum_v===undefined)? null : p.batteries_sum_v}; }))
  ],'V',true,false);
  var bds=[];
  for(var i=0;i<4;i++){
    (function(i){
      bds.push(mkAgmDs('Батарея '+(i+1),CHART_COLORS[i%CHART_COLORS.length],points.map(function(p){
        var y=(p['break']||!p.batteries_v||i>=p.batteries_v.length)? null : p.batteries_v[i];
        return {x:new Date(p.ts), y:y};
      })));
    })(i);
  }
  agmRender('agmBatChart',bds,'V',true,false);
  agmRender('agmCurChart',[mkAgmDs('Ток','#5bc0de',points.map(function(p){ return {x:new Date(p.ts), y:yVal(p,'current_a')}; }))],'A',false,true);
  agmRender('agmPwrChart',[mkAgmDs('Мощность','#d9534f',points.map(function(p){ return {x:new Date(p.ts), y:yVal(p,'power_w')}; }))],'W',false,true);
}
function destroyAgmCharts(){
  Object.keys(AGM_CHARTS).forEach(function(id){ AGM_CHARTS[id].destroy(); delete AGM_CHARTS[id]; });
}
function catchUp24hRange(){
  if(periodMode!=='24h' || userZoomed) return;
  var to=new Date();
  selRange.from=new Date(to.getTime()-24*60*60*1000);
  selRange.to=to;
  var f=document.getElementById('fromPick'), t=document.getElementById('toPick');
  if(f) f.value=toInputDateTime(selRange.from);
  if(t) t.value=toInputDateTime(selRange.to);
}
async function loadAgmCharts(){
  if(window.srRefresh && !window.srRefresh.isEnabled()) return;
  catchUp24hRange();
  try{
    var from=selRange.from, to=selRange.to;
    var url='/api/agm/series?from='+encodeURIComponent(from.toISOString())+'&to='+encodeURIComponent(to.toISOString());
    var r=await fetch(url);
    if(!r.ok) return;
    var data=await r.json();
    agmCaptureState();
    destroyAgmCharts();
    agmRebuilding=true;
    try{
      lastXWindow={};
      buildAgmCharts(data.points||[]);
    }finally{ agmRebuilding=false; }
    setAgmRangeLabels();
  }catch(e){}
}

// Кнопки периода.
document.getElementById('btn24h').addEventListener('click',function(){
  var to=new Date(); var from=new Date(to.getTime()-24*60*60*1000);
  setPeriod(from, to, '24h', 'btn24h');
});
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
document.getElementById('btnRefresh').addEventListener('click',function(){ preserveZoom=false; userZoomed=false; loadAgmCharts(); });

document.getElementById('fromPick').value=toInputDateTime(selRange.from);
document.getElementById('toPick').value=toInputDateTime(selRange.to);
document.getElementById('datePick').value=toInputDate(selRange.from);
setActiveBtn('btn24h');

// Периодическое обновление графиков (раз в минуту) и плашек (раз в секунду).
var agmChartsTimer=null;
function agmChartsStart(){ if(agmChartsTimer) return; loadAgmCharts(); agmChartsTimer=setInterval(function(){ preserveZoom=userZoomed; loadAgmCharts(); },60000); }
function agmChartsStop(){ if(agmChartsTimer){ clearInterval(agmChartsTimer); agmChartsTimer=null; } }
var agmParamTimer=null;
function agmParamStart(){ if(agmParamTimer) return; loadAgmCurrent(); agmParamTimer=setInterval(loadAgmCurrent,1000); }
function agmParamStop(){ if(agmParamTimer){ clearInterval(agmParamTimer); agmParamTimer=null; } }
if(window.srRefresh){
  window.srRefresh.register(agmChartsStart, agmChartsStop);
  window.srRefresh.register(agmParamStart, agmParamStop);
}
agmChartsStart();
agmParamStart();

// Мобильные жесты (щипок/свайп/двойной тап) — как на странице BMS.
if(SR_COARSE){
  AGM_CHART_IDS.forEach(function(id){
    srTouchChart(function(){ return AGM_CHARTS[id]; }, id, 5*60*1000, null, function(isReset){ agmWindowChanged(id, isReset); });
  });
}

// ---------- Порядок графиков (сетка 2 колонки), как на странице BMS ----------
var AGM_ORDER_COLS=2;
function agmRefreshOrderBtns(){
  var cards=document.querySelectorAll('.bms-charts > .card');
  for(var i=0;i<cards.length;i++){
    var up=cards[i].querySelector('.ord-up'), down=cards[i].querySelector('.ord-down');
    var left=cards[i].querySelector('.ord-left'), right=cards[i].querySelector('.ord-right');
    if(up) up.disabled=(i-AGM_ORDER_COLS<0);
    if(down) down.disabled=(i+AGM_ORDER_COLS>=cards.length);
    if(left) left.disabled=(i%AGM_ORDER_COLS!==1);
    if(right) right.disabled=(i%AGM_ORDER_COLS!==0 || i+1>=cards.length);
  }
}
function agmSaveOrder(){
  var ids=[];
  document.querySelectorAll('.bms-charts > .card').forEach(function(c){
    var cv=c.querySelector('.chart-wrap canvas');
    if(cv) ids.push(cv.id);
  });
  try{ localStorage.setItem('agmChartOrder', JSON.stringify(ids)); }catch(e){}
}
function agmLoadOrder(){
  var ids=null;
  try{ ids=JSON.parse(localStorage.getItem('agmChartOrder')||'null'); }catch(e){}
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
function agmMoveCard(btn,dir){
  var card=btn.closest('.card');
  if(!card) return;
  var cards=card.parentNode.querySelectorAll('.card');
  var idx=Array.prototype.indexOf.call(cards,card);
  var target=-1;
  if(dir==='up') target=(idx-AGM_ORDER_COLS>=0)? idx-AGM_ORDER_COLS : -1;
  else if(dir==='down') target=(idx+AGM_ORDER_COLS<cards.length)? idx+AGM_ORDER_COLS : -1;
  else if(dir==='left') target=(idx%AGM_ORDER_COLS===1)? idx-1 : -1;
  else if(dir==='right') target=(idx%AGM_ORDER_COLS===0 && idx+1<cards.length)? idx+1 : -1;
  if(target<0) return;
  var ref=cards[target];
  if(target>idx) card.parentNode.insertBefore(card, ref.nextSibling);
  else card.parentNode.insertBefore(card, ref);
  agmSaveOrder();
  agmRefreshOrderBtns();
}
document.querySelectorAll('.bms-charts .ord-up').forEach(function(b){ b.addEventListener('click',function(){ agmMoveCard(this,'up'); }); });
document.querySelectorAll('.bms-charts .ord-down').forEach(function(b){ b.addEventListener('click',function(){ agmMoveCard(this,'down'); }); });
document.querySelectorAll('.bms-charts .ord-left').forEach(function(b){ b.addEventListener('click',function(){ agmMoveCard(this,'left'); }); });
document.querySelectorAll('.bms-charts .ord-right').forEach(function(b){ b.addEventListener('click',function(){ agmMoveCard(this,'right'); }); });
agmLoadOrder();
agmRefreshOrderBtns();

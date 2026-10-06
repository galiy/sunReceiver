'use strict';
// Страница AGM (свинцово-кислотный АКБ). Отдельного датчика нет: ток и мощность —
// это МАП (battery) минус сумма трёх BMS (ANT×2 + EnBMS), расчёт на бэкенде
// (/api/agm/series). Управление, зум, связь графиков и хинты — как на странице BMS.
var SR_COARSE = ('ontouchstart' in window) || navigator.maxTouchPoints > 0 || (window.matchMedia && matchMedia('(pointer: coarse)').matches);
function fmtDate(t){ var d=new Date(t); function p(x){return (x<10?'0':'')+x;} return d.getFullYear()+'-'+p(d.getMonth()+1)+'-'+p(d.getDate())+' '+p(d.getHours())+':'+p(d.getMinutes()); }
function fmtNum(n,digits){ return isFinite(n)? n.toLocaleString('ru-RU',{maximumFractionDigits:digits}) : '—'; }
function agmSocColor(soc){ return soc<20?'#d9534f':(soc<50?'#f08c00':(soc<80?'#f0ad4e':'#37b24d')); }

// Плашки текущих значений (как на странице BMS): заряд, напряжение батареи (с
// шины МАП), ток, мощность, ёмкость. Ток/мощность — с бэкенда (/api/agm) по той
// же формуле, что и графики; SOC — по таблице AGM от напряжения.
function renderAgmKPIs(d){
  var soc=Math.max(0,Math.min(100,Number(d.soc)||0));
  var iCls= d.current_a>=0? 'pos':'neg';
  var pCls= d.power_w>=0? 'pos':'neg';
  var h='';
  h+='<div class="kpi"><div class="lbl">Заряд (SOC)</div><div class="val socval">'
    +'<span class="mini-batt"><span class="mini-batt-fill" style="width:'+soc+'%;background:'+agmSocColor(soc)+';display:block"></span></span>'
    +'<span>'+soc+'<span class="unit">%</span></span></div>'
    +'<div class="sub">остаток '+fmtNum(d.remaining_ah,1)+' А·ч из '+fmtNum(d.capacity_ah,0)+' А·ч</div></div>';
  h+='<div class="kpi"><div class="lbl">Напряжение батареи</div><div class="val">'+fmtNum(d.voltage,2)+'<span class="unit">V</span></div>'
    +'<div class="sub">с шины МАП</div></div>';
  h+='<div class="kpi"><div class="lbl">Ток</div><div class="val '+iCls+'">'+fmtNum(d.current_a,1)+'<span class="unit">A</span></div></div>';
  h+='<div class="kpi"><div class="lbl">Мощность</div><div class="val '+pCls+'">'+fmtNum(d.power_w,1)+'<span class="unit">W</span></div></div>';
  h+='<div class="kpi"><div class="lbl">Ёмкость</div><div class="val">'+fmtNum(d.capacity_ah,0)+'<span class="unit">А·ч</span></div></div>';
  document.getElementById('agmKpiRow').innerHTML=h;
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
// Позиционер хинта (боковой) — как на странице BMS, регистрируем под именем 'agmSide'.
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

var AGM_CHART_IDS=['agmVoltChart','agmCurChart','agmPwrChart'];
function mkDs(label,color,data){ return { label:label, data:data, borderColor:color, backgroundColor:color, pointRadius:0, pointHoverRadius:0, borderWidth:1.5, tension:0.35, cubicInterpolationMode:'monotone', fill:false, spanGaps:false }; }
function setAgmRangeLabels(){
  var txt='Диапазон: '+fmtDate(selRange.from)+' — '+fmtDate(selRange.to);
  AGM_CHART_IDS.forEach(function(id){ var el=document.getElementById(id+'Range'); if(el) el.textContent=txt; });
}

// ---------- Выбор периода (общий для обоих графиков) ----------
var preserveZoom=false;
var userZoomed=false;
// Диапазон по умолчанию при входе на страницу — «24 часа»: скользящее окно
// (текущий момент минус 24 часа … текущий момент), а не «Сегодня».
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

// ---------- Синхронизация зума и курсора (как на странице BMS) ----------
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

var AGM_CHARTS={};
// agmRender — линейный график. zero=true — симметричная ось Y относительно нуля
// (ток, мощность); zero=false — авто-масштаб по данным (напряжение).
function agmRender(id, datasets, yTitle, zero){
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
      if(chart && chart.canvas && chart.scales && chart.scales.x){
        var v=chart.scales.x.getValueForPixel(event.x);
        if(isFinite(v)){
          agmCursor.active=true; agmCursor.value=v;
          agmSyncTooltips(v);
          agmUpdateAllCharts(); return;
        }
      }
      if(agmCursor.active){ agmCursor.active=false; agmSyncTooltips(null); agmUpdateAllCharts(); }
    },
    animation:{ duration:200 },
    plugins:{
      legend: { display:false },
      tooltip:{ position:'agmSide', caretPadding:30 },
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
  AGM_CHARTS[id]=new Chart(document.getElementById(id),{ type:'line', data:{datasets:datasets}, plugins:[agmZoomSyncPlugin], options:opts });
  var el=document.getElementById(id);
  if(!el.__srMLBound){ el.__srMLBound=true; el.addEventListener('mouseleave',function(){ if(agmCursor.active){ agmCursor.active=false; agmSyncTooltips(null); agmUpdateAllCharts(); } }); }
  if(SR_COARSE || window.__srTouched){ AGM_CHARTS[id].options.plugins.tooltip.enabled=false; }
}
function buildAgmCharts(points, voltage){
  // Точки с break=true — разрыв линии: y=null, spanGaps:false.
  function ds(src,label,color,field){ return mkDs(label,color,src.map(function(p){ return {x:new Date(p.ts), y:(p['break']? null : p[field])}; })); }
  // Напряжение — только по данным МАП (разрывы BMS на него не влияют).
  agmRender('agmVoltChart',[ds(voltage||[],'Напряжение','#f0ad4e','voltage_v')],'V',false);
  agmRender('agmCurChart',[ds(points,'Ток','#5bc0de','current_a')],'A',true);
  agmRender('agmPwrChart',[ds(points,'Мощность','#d9534f','power_w')],'W',true);
}
function destroyAgmCharts(){
  Object.keys(AGM_CHARTS).forEach(function(id){ AGM_CHARTS[id].destroy(); delete AGM_CHARTS[id]; });
}
// catchUp24hRange — скользящее окно режима «24 часа»: при стандартном виде (без
// пользовательского зума) на каждом обновлении (таймер, кнопка «Обновить графики»)
// правый край переставляется на текущий момент, начало — ровно на 24 часа раньше.
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
    destroyAgmCharts();
    agmRebuilding=true;
    try{
      lastXWindow={};
      buildAgmCharts(data.points||[], data.voltage||[]);
    }finally{ agmRebuilding=false; }
    setAgmRangeLabels();
    var foot=document.getElementById('agmFoot');
    if(foot) foot.textContent='AGM = батарея МАП − Σ (ANT×2 + EnBMS) · обновляется раз в минуту';
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
setActiveBtn('btn24h'); // по умолчанию активен диапазон «24 часа»

// Периодическое обновление графиков (раз в минуту) через глобальный выключатель.
var agmChartsTimer=null;
function agmChartsStart(){ if(agmChartsTimer) return; loadAgmCharts(); agmChartsTimer=setInterval(function(){ preserveZoom=userZoomed; loadAgmCharts(); },60000); }
function agmChartsStop(){ if(agmChartsTimer){ clearInterval(agmChartsTimer); agmChartsTimer=null; } }
// Плашки текущих значений — раз в секунду (как параметры на странице BMS).
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

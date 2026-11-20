'use strict';
// Страница «Управление гаражом»: состояние модуля и реле, входные данные
// мониторинга, график состояний реле (журнал PG, ступеньки).
(function(){
  var RELAYS=[
    {n:1, label:'Реле 1 — инверторы Deye Right/Left', on:'инверторы отключены от сети', off:'инверторы подключены к сети'},
    {n:2, label:'Реле 2 — подача напряжения', on:'подача отключена', off:'подача разрешена'},
    {n:3, label:'Реле 3 — стабилизатор / прямая', on:'прямая подача', off:'через стабилизатор'},
    {n:4, label:'Реле 4 — управление', on:'авто (по реле 3)', off:'ручное'}
  ];
  var COLORS=['#e2574c','#d9a521','#2f9e44','#3b82f6'];
  var STATE_LABELS={1:'Выключен',2:'Ожидание восстановления',3:'Управляет'};
  var charts=[];
  var period={from:null,to:null,mode:'24h'};
  // Общий курсор: вертикальная линия через все полосы + один хинт на 4 реле.
  var hover={t:null};
  var lastSeries=[];
  var hintEl=null;

  if(window.Chart && !window.__garageCursorPlugin){
    window.__garageCursorPlugin={
      id:'garageCursor',
      afterDatasetsDraw:function(chart){
        if(hover.t==null) return;
        var sc=chart.scales&&chart.scales.x, ca=chart.chartArea;
        if(!sc||!ca) return;
        var x=sc.getPixelForValue(hover.t);
        if(!isFinite(x)||x<ca.left||x>ca.right) return;
        var ctx=chart.ctx;
        ctx.save();
        ctx.beginPath();
        ctx.moveTo(x,ca.top);
        ctx.lineTo(x,ca.bottom);
        ctx.strokeStyle='rgba(33,37,41,0.45)';
        ctx.lineWidth=1;
        ctx.stroke();
        ctx.restore();
      }
    };
    window.Chart.register(window.__garageCursorPlugin);
  }

  // Ступеньки реле рисуем сами: горизонтальные участки в 2 раза жирнее базовой
  // линии, вертикальные переходы — в 2 раза тоньше (Chart.js одним стилем не умеет).
  if(window.Chart && !window.__garageStepPlugin){
    window.__garageStepPlugin={
      id:'garageSteps',
      afterDatasetsDraw:function(chart){
        var pts=chart.$garagePts, col=chart.$garageColor;
        var sc=chart.scales&&chart.scales.y, sx=chart.scales&&chart.scales.x;
        if(!pts||!col||!sc||!sx||pts.length<2) return;
        var ctx=chart.ctx;
        ctx.save();
        ctx.strokeStyle=col;
        ctx.lineCap='butt';
        for(var i=1;i<pts.length;i++){
          var p=pts[i-1], q=pts[i];
          var yp=sc.getPixelForValue(p.y), yq=sc.getPixelForValue(q.y);
          var xp=sx.getPixelForValue(p.x), xq=sx.getPixelForValue(q.x);
          if(!isFinite(xp)||!isFinite(xq)||!isFinite(yp)||!isFinite(yq)) continue;
          // Горизонталь — 2× толще (3.2px).
          ctx.lineWidth=3.2;
          ctx.beginPath();
          if(p.y===q.y){
            ctx.moveTo(xp,yp);
            ctx.lineTo(xq,yq);
          }else{
            ctx.moveTo(xp,yp);
            ctx.lineTo(xq,yp);
          }
          ctx.stroke();
          // Вертикальная ступенька — 2× тоньше (0.8px).
          if(p.y!==q.y){
            ctx.lineWidth=0.8;
            ctx.beginPath();
            ctx.moveTo(xq,yp);
            ctx.lineTo(xq,yq);
            ctx.stroke();
          }
        }
        ctx.restore();
      }
    };
    window.Chart.register(window.__garageStepPlugin);
  }

  function el(id){ return document.getElementById(id); }
  function esc(s){ return String(s==null?'':s).replace(/[&<>"']/g,function(c){return {'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c];}); }
  function fmtTs(ts){ if(!ts||ts.indexOf('0001')===0) return '—'; var d=new Date(ts); return isNaN(d.getTime())?'—':d.toLocaleString(); }
  function fmtAge(sec){ if(sec==null||!isFinite(sec)) return '—'; if(sec<0) sec=0; return sec.toFixed(0)+' s'; }
  function fmtV(v){ return (v==null||!isFinite(v))?'—':Number(v).toFixed(1); }

  function renderState(s){
    var box=el('garageState');
    if(!s || s.enabled===false){
      box.innerHTML='<span class="missing">Модуль выключен в конфиге</span>';
      return;
    }
    var st=s.module_state||2;
    var cls=st===3?'green':(st===1?'grey':'red');
    var html='<div class="garage-state"><span class="garage-dot '+cls+'"></span><b>'+esc(STATE_LABELS[st]||('Состояние '+st))+'</b></div>';
    if(s.phase_fault) html+='<div class="garage-warn">Авария фаз — подача отключена</div>';
    if(!s.device_up) html+='<div class="garage-warn">Нет связи с реле</div>';
    else if(!s.host_ready) html+='<div class="garage-warn">Хост реле не готов к приёму кадров</div>';
    html+='<div class="garage-note">Изменение состояния: '+esc(fmtTs(s.state_changed_at))+'</div>';
    if(s.last_error) html+='<div class="garage-note">Ошибка: '+esc(s.last_error)+'</div>';
    box.innerHTML=html;
  }

  function renderRelays(s){
    var box=el('garageRelays');
    if(s && s.enabled===false){
      box.innerHTML='<span class="missing">Состояние реле не читается (модуль выключен в конфиге)</span>';
      return;
    }
    var relays=(s&&s.relays)||[false,false,false,false];
    var at=(s&&s.relay_changed_at)||[];
    var html='<table class="garage-table"><tbody>';
    for(var i=0;i<RELAYS.length;i++){
      var r=RELAYS[i], on=!!relays[i];
      html+='<tr><td>'+esc(r.label)+'</td>'+
        '<td><span class="garage-badge '+(on?'on':'off')+'">'+(on?'Вкл':'Выкл')+'</span></td>'+
        '<td class="garage-sub">'+esc(on?r.on:r.off)+'</td>'+
        '<td class="garage-sub">'+esc(fmtTs(at[i]))+'</td></tr>';
    }
    html+='</tbody></table>';
    box.innerHTML=html;
  }

  function renderInputs(s){
    var box=el('garageInputs');
    if(!s||s.enabled===false){ box.innerHTML='<span class="missing">Модуль выключен в конфиге</span>'; return; }
    var ph=s.phases||[0,0,0];
    var html='<div class="garage-section-title">Счётчик DTS017M — фазные напряжения</div>';
    if(s.dts017_enabled===false){
      html+='<span class="missing">Счётчик DTS017M выключен — правила фаз и реле 3 не применяются</span>';
    }else{
      html+='<table class="garage-table garage-phases"><tbody>'+
        '<tr><td>Фаза A</td><td>'+fmtV(ph[0])+' В</td></tr>'+
        '<tr><td>Фаза B</td><td>'+fmtV(ph[1])+' В</td></tr>'+
        '<tr><td>Фаза C</td><td>'+fmtV(ph[2])+' В</td></tr>'+
        '<tr><td>Свежесть данных счётчика</td><td>'+esc(fmtAge(s.meter_age_sec))+(s.meter_fresh?' (актуально)':' (устарело)')+'</td></tr>'+
        '</tbody></table>';
    }
    html+='<div class="garage-section-title">Инверторы «Гараж»</div>';
    var invs=s.inverters||[];
    if(!invs.length){
      html+='<span class="missing">Нет данных инверторов группы «Гараж»</span>';
    }else{
      html+='<table class="garage-table"><thead><tr><th>Инвертор</th><th>PV1, В</th><th>PV2, В</th><th>Свежесть</th></tr></thead><tbody>';
      for(var i=0;i<invs.length;i++){
        var it=invs[i];
        html+='<tr><td>'+esc(it.name||it.serial)+'</td><td>'+fmtV(it.pv1_voltage)+'</td><td>'+fmtV(it.pv2_voltage)+'</td><td>'+esc(fmtAge(it.age_sec))+(it.fresh?' (актуально)':' (устарело)')+'</td></tr>';
      }
      html+='</tbody></table>';
    }
    box.innerHTML=html;
    el('garageUpdated').textContent='Обновлено: '+fmtTs(s.updated_at);
  }

  async function loadState(){
    if(window.srRefresh && !window.srRefresh.isEnabled()) return;
    try{
      var r=await fetch('/api/garage',{cache:'no-store'});
      if(!r.ok) return;
      var s=await r.json();
      renderState(s); renderRelays(s); renderInputs(s);
    }catch(e){}
  }

  // Один график на реле: своя ось X и своя полоса, чтобы состояния не
  // накладывались друг на друга. Диапазон X у всех одинаковый.
  // Если журнал не знает состояние на начало периода (событий до from не было),
  // плоскую линию от from не рисуем — ряд начинается с первого события.
  function stepPoints(events, initialOn, known, ri, from, to, journalStartMs, currentOn){
    var pts=[];
    var last=initialOn?1:0;
    var started=!!known;
    if(started) pts.push({x:from,y:last});
    for(var i=0;i<events.length;i++){
      var e=events[i];
      if(e.relay!==ri+1) continue;
      var t=new Date(e.ts).getTime();
      if(t<from||t>to) continue;
      started=true;
      pts.push({x:t,y:e.new?1:0});
      last=e.new?1:0;
    }
    if(!started){
      // Событий реле нет: состояние известно только с начала журнала. Рисуем
      // ровную линию от начала журнала (не от начала периода), уровень — текущий.
      if(journalStartMs!=null && journalStartMs<=to){
        var sx=Math.max(from,journalStartMs);
        pts.push({x:sx,y:currentOn?1:0});
        last=currentOn?1:0;
        started=true;
      }
    }
    if(!started) return pts; // журнала нет вовсе
    pts.push({x:to,y:last});
    return pts;
  }

  function buildCharts(data){
    if(!window.Chart) return;
    var from=period.from.getTime(), to=period.to.getTime();
    var events=(data&&data.events)||[];
    var initial=(data&&data.initial)||[false,false,false,false];
    var known=(data&&data.initial_known)||[false,false,false,false];
    var journalStart=(data&&data.journal_start)?new Date(data.journal_start).getTime():null;
    var current=(data&&data.current)||[false,false,false,false];
    for(var i=0;i<charts.length;i++){ if(charts[i]) charts[i].destroy(); }
    charts=[];
    for(var ri=0;ri<4;ri++){
      var ctx=el('garageChart'+(ri+1));
      if(!ctx) continue;
      var pts=stepPoints(events,initial[ri],known[ri],ri,from,to,journalStart,!!current[ri]);
      lastSeries[ri]=pts;
      var ch=new Chart(ctx,{
        type:'line',
        data:{datasets:[{
          label:'Реле '+(ri+1),
          data:pts,
          borderColor:COLORS[ri],
          backgroundColor:COLORS[ri],
          stepped:'after',
          borderWidth:0,
          pointRadius:0,
          fill:false
        }]},
        options:{
          responsive:true, maintainAspectRatio:false, animation:false,
          interaction:{mode:'nearest',intersect:false},
          scales:{
            // Одинаковая высота и сетка у всех полос: подписи времени только под
            // нижней, у остальных ось есть, но подписи пустые (место сохраняется).
            // Линейная ось в миллисекундах: подписи форматируем сами, не
            // завися от date-adapter (иначе возможны «сырые» unix-числа).
            x:{type:'linear',
               grid:{display:true},
               ticks:{maxTicksLimit:12,autoSkip:true,
                      callback:function(value){
                        if(ri!==3) return '';
                        var d=new Date(Number(value));
                        if(isNaN(d.getTime())) return '';
                        var two=function(n){ return n<10?'0'+n:''+n; };
                        if(period.mode==='24h'||period.mode==='today'||period.mode==='yesterday'){
                          return two(d.getHours())+':'+two(d.getMinutes());
                        }
                        return two(d.getDate())+'.'+two(d.getMonth()+1)+' '+two(d.getHours())+':'+two(d.getMinutes());
                      }}},
            y:{min:-0.1,max:1.1,ticks:{stepSize:1,callback:function(v){return v===1?'ON':(v===0?'OFF':'');}}}
          },
          plugins:{legend:{display:false},tooltip:{enabled:false}}
        }
      });
      ch.$garagePts=pts;
      ch.$garageColor=COLORS[ri];
      ch.update('none'); // первый рендер был до установки данных плагина
      charts.push(ch);
    }
    installHover();
  }

  // Общий курсор: слушатели вешаются ОДИН раз на контейнер (канвасы при
  // пересборке графиков переиспользуются — иначе слушатели копились бы).
  function installHover(){
    var box=document.querySelector('.garage-charts');
    if(!box||box.__garageHover) return;
    box.__garageHover=true;
    box.addEventListener('mousemove',function(ev){
      var target=ev.target;
      if(!target||target.tagName!=='CANVAS') return;
      var ch=null;
      for(var i=0;i<charts.length;i++){ if(charts[i]&&charts[i].canvas===target){ ch=charts[i]; break; } }
      if(!ch||!ch.scales||!ch.scales.x) return;
      var rect=target.getBoundingClientRect();
      var t=ch.scales.x.getValueForPixel(ev.clientX-rect.left);
      if(t==null||!isFinite(t)) return;
      hover.t=t;
      for(var j=0;j<charts.length;j++){ if(charts[j]) charts[j].update('none'); }
      showHint(ev.clientX,ev.clientY,t);
    });
    box.addEventListener('mouseleave',hideHint);
  }

  function valueAt(pts,t){
    var v=0;
    if(!pts) return v;
    for(var i=0;i<pts.length;i++){
      if(pts[i].x<=t) v=pts[i].y; else break;
    }
    return v;
  }

  function ensureHint(){
    if(hintEl) return hintEl;
    var box=document.querySelector('.garage-charts');
    if(!box) return null;
    hintEl=document.createElement('div');
    hintEl.className='garage-hint';
    hintEl.style.display='none';
    box.appendChild(hintEl);
    return hintEl;
  }

  function showHint(clientX,clientY,t){
    var h=ensureHint();
    if(!h) return;
    var ts=new Date(t);
    var html='<div class="garage-hint-ts">'+esc(ts.toLocaleString())+'</div>';
    for(var i=0;i<4;i++){
      var on=valueAt(lastSeries[i],t)===1;
      html+='<div class="garage-hint-row"><span class="garage-hint-dot" style="background:'+COLORS[i]+'"></span>'+
        'Реле '+(i+1)+': '+(on?'Вкл':'Выкл')+'</div>';
    }
    h.innerHTML=html;
    var box=document.querySelector('.garage-charts');
    var r=box.getBoundingClientRect();
    var x=clientX-r.left+16, y=clientY-r.top+16;
    h.style.display='block';
    if(x+h.offsetWidth>r.width-4) x=clientX-r.left-h.offsetWidth-16;
    if(y+h.offsetHeight>r.height-4) y=clientY-r.top-h.offsetHeight-16;
    h.style.left=Math.max(2,x)+'px';
    h.style.top=Math.max(2,y)+'px';
  }

  function hideHint(){
    hover.t=null;
    if(hintEl) hintEl.style.display='none';
    for(var i=0;i<charts.length;i++){ if(charts[i]) charts[i].update('none'); }
  }

  // Диапазон относительного периода пересчитывается в момент запроса, чтобы
  // правый край графика сдвигался при обновлении раз в минуту.
  function rangeFor(mode){
    var now=new Date();
    if(mode==='24h') return {from:new Date(now.getTime()-24*3600*1000),to:now};
    if(mode==='today') return {from:startOfToday(),to:now};
    if(mode==='yesterday'){ var s0=startOfToday(); return {from:new Date(s0.getTime()-86400000),to:s0}; }
    if(mode==='7d') return {from:new Date(now.getTime()-7*86400000),to:now};
    if(mode==='30d') return {from:new Date(now.getTime()-30*86400000),to:now};
    return {from:new Date(2020,0,1),to:now};
  }

  async function loadChart(){
    if(window.srRefresh && !window.srRefresh.isEnabled()) return;
    var rg=rangeFor(period.mode);
    period.from=rg.from; period.to=rg.to;
    try{
      var url='/api/garage/events?from='+encodeURIComponent(period.from.toISOString())+'&to='+encodeURIComponent(period.to.toISOString());
      var r=await fetch(url,{cache:'no-store'});
      if(!r.ok) return;
      var data=await r.json();
      buildCharts(data);
    }catch(e){}
  }

  function setPeriod(mode,activeId,silent){
    period.mode=mode;
    var rg=rangeFor(mode);
    period.from=rg.from; period.to=rg.to;
    var ids=['gBtn24h','gBtnToday','gBtnYesterday','gBtn7d','gBtn30d','gBtnAll'];
    for(var i=0;i<ids.length;i++){ var b=el(ids[i]); if(b) b.classList.toggle('active',ids[i]===activeId); }
    if(!silent) loadChart();
  }
  function startOfToday(){ var d=new Date(); d.setHours(0,0,0,0); return d; }

  el('gBtn24h').addEventListener('click',function(){ setPeriod('24h','gBtn24h'); });
  el('gBtnToday').addEventListener('click',function(){ setPeriod('today','gBtnToday'); });
  el('gBtnYesterday').addEventListener('click',function(){ setPeriod('yesterday','gBtnYesterday'); });
  el('gBtn7d').addEventListener('click',function(){ setPeriod('7d','gBtn7d'); });
  el('gBtn30d').addEventListener('click',function(){ setPeriod('30d','gBtn30d'); });
  el('gBtnAll').addEventListener('click',function(){ setPeriod('all','gBtnAll'); });

  var stateTimer=null, chartTimer=null;
  function startState(){ if(stateTimer) return; loadState(); stateTimer=setInterval(loadState,1000); }
  function stopState(){ if(stateTimer){ clearInterval(stateTimer); stateTimer=null; } }
  function startChart(){ if(chartTimer) return; loadChart(); chartTimer=setInterval(loadChart,60000); }
  function stopChart(){ if(chartTimer){ clearInterval(chartTimer); chartTimer=null; } }
  if(window.srRefresh){ window.srRefresh.register(startState,stopState); window.srRefresh.register(startChart,stopChart); }

  setPeriod('24h','gBtn24h',true); // без запроса: первый запрос делает startChart
  startState();
  startChart();
})();

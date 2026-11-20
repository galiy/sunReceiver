'use strict';
// Страница «Управление гаражом»: состояние модуля и реле, входные данные
// мониторинга, общий график состояний 4 реле (стиль — как на странице BMS).
(function(){
  var RELAYS=[
    {n:1, label:'Реле 1 — инверторы Deye Right/Left', on:'инверторы отключены от сети', off:'инверторы подключены к сети'},
    {n:2, label:'Реле 2 — подача напряжения', on:'подача отключена', off:'подача разрешена'},
    {n:3, label:'Реле 3 — стабилизатор / прямая', on:'прямая подача', off:'через стабилизатор'},
    {n:4, label:'Реле 4 — управление', on:'авто (по реле 3)', off:'ручное'}
  ];
  var COLORS=['#e2574c','#d9a521','#2f9e44','#3b82f6'];
  var STATE_LABELS={1:'Выключен',2:'Ожидание восстановления',3:'Управляет'};

  // common.js держит SR_COARSE внутри IIFE — для страницы объявляем свою копию.
  var SR_COARSE = ('ontouchstart' in window) || navigator.maxTouchPoints > 0 ||
    (window.matchMedia && matchMedia('(pointer: coarse)').matches);
  function tr(s){ return (window.SR_i18n && window.SR_i18n.t) ? window.SR_i18n.t(s) : s; }

  var chart=null;
  var hidden={};
  var hover={t:null};
  var savedZoom=null;       // {min,max} при пользовательском зуме
  var userZoomed=false;
  var activeBtn='gBtn24h';
  var relativeMode='24h'; // '24h' | 'today' | 'yesterday' | '7d' | 'month' | null
  var navMode=null;        // 'day' | 'month' | 'span' — шаг для ‹/›
  var selRange={from:null,to:null};

  if(window.Chart && !window.__garageCursorPlugin){
    window.__garageCursorPlugin={
      id:'garageCursor',
      afterDatasetsDraw:function(c){
        if(hover.t==null) return;
        var sc=c.scales&&c.scales.x, ca=c.chartArea;
        if(!sc||!ca) return;
        var x=sc.getPixelForValue(hover.t);
        if(!isFinite(x)||x<ca.left||x>ca.right) return;
        var ctx=c.ctx;
        ctx.save();
        ctx.beginPath();
        ctx.moveTo(x,ca.top);
        ctx.lineTo(x,ca.bottom);
        ctx.strokeStyle='rgba(33,37,41,0.35)';
        ctx.lineWidth=1;
        ctx.stroke();
        ctx.restore();
      }
    };
    window.Chart.register(window.__garageCursorPlugin);
  }

  function el(id){ return document.getElementById(id); }
  function esc(s){ return String(s==null?'':s).replace(/[&<>"']/g,function(c){return {'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c];}); }
  function fmtTs(ts){ if(!ts||ts.indexOf('0001')===0) return '—'; var d=new Date(ts); return isNaN(d.getTime())?'—':d.toLocaleString(); }
  function fmtAge(sec){ if(sec==null||!isFinite(sec)) return '—'; if(sec<0) sec=0; return sec.toFixed(0)+' s'; }
  function fmtV(v){ return (v==null||!isFinite(v))?'—':Number(v).toFixed(1); }

  // ---------------- Дашборд: состояние модуля / реле / входные данные ------
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

  // ---------------- График: 4 линии на одной оси ----------------------------
  // Значение линии = состояние (0/1) + (номер реле - 1) * 2 — линии не
  // накладываются. Ряды выравниваются по общим временным точкам, чтобы
  // tooltip в режиме index показывал все 4 реле.
  function relayEvents(events, ri){
    var out=[];
    for(var i=0;i<events.length;i++){
      var e=events[i];
      if(e.relay===ri+1){
        out.push({t:new Date(e.ts).getTime(), on:!!e.new});
      }
    }
    out.sort(function(a,b){ return a.t-b.t; });
    return out;
  }

  function stateAt(ev, initialOn, known, t){
    var st=initialOn?1:0;
    if(!known){
      if(!ev.length || ev[0].t>t) return null;
      st=ev[0].on?1:0;
      for(var i=1;i<ev.length && ev[i].t<=t;i++) st=ev[i].on?1:0;
      return st;
    }
    for(var j=0;j<ev.length && ev[j].t<=t;j++) st=ev[j].on?1:0;
    return st;
  }

  function buildSeries(data, from, to, live){
    var events=(data&&data.events)||[];
    var initial=(data&&data.initial)||[false,false,false,false];
    var known=(data&&data.initial_known)||[false,false,false,false];
    var current=(data&&data.current)||[false,false,false,false];
    var journalStart=(data&&data.journal_start)?new Date(data.journal_start).getTime():null;
    var evs=[], starts=[], flat=[];
    var tillNow=!!live;
    for(var ri=0;ri<4;ri++){
      var ev=relayEvents(events,ri);
      evs.push(ev);
      var st=null, fl=false;
      if(known[ri]) st=from;
      else if(ev.length && ev[0].t<=to) st=Math.max(from,ev[0].t);
      else if(tillNow && journalStart!=null && journalStart<=to){
        // Окно доходит до «сейчас», событий у реле нет вовсе → состояние
        // постоянно с начала журнала и равно текущему: плоская линия.
        st=Math.max(from,journalStart); fl=true;
      }
      starts.push(st); flat.push(fl);
    }
    // Общие временные точки: старт каждого ряда, его события и правый край.
    var times={};
    for(var i=0;i<4;i++){
      if(starts[i]==null) continue;
      times[starts[i]]=1;
      for(var k=0;k<evs[i].length;k++){
        if(evs[i][k].t>=starts[i] && evs[i][k].t<=to) times[evs[i][k].t]=1;
      }
    }
    times[to]=1;
    var xs=Object.keys(times).map(Number).sort(function(a,b){ return a-b; });
    var datasets=[];
    // Все ряды строятся по ОДНОМУ массиву xs (до старта — null/разрыв),
    // иначе tooltip mode:'index' берёт точки с разными индексами.
    for(var r=0;r<4;r++){
      var off=r*2, pts=[];
      for(var x=0;x<xs.length;x++){
        var y=null;
        if(starts[r]!=null && xs[x]>=starts[r]){
          if(flat[r]){
            y=(current[r]?1:0)+off;
          }else{
            var stt=stateAt(evs[r],initial[r],known[r],xs[x]);
            if(stt==null && evs[r].length && evs[r][0].t===xs[x]) stt=evs[r][0].on?1:0;
            if(stt!=null) y=stt+off;
          }
        }
        pts.push({x:xs[x],y:y});
      }
      datasets.push({
        label:'Реле '+(r+1),
        data:pts,
        borderColor:COLORS[r],
        backgroundColor:COLORS[r],
        stepped:'after',
        borderWidth:2,
        pointRadius:0,
        fill:false,
        hidden:!!hidden[r]
      });
    }
    return datasets;
  }

  function tooltipLabel(ctx){
    var v=ctx.parsed.y;
    if(v==null || !isFinite(v)) return null; // разрыв до первого события
    var relay=Math.floor(v/2)+1;
    var on=(v%2)>=1;
    return tr('Реле')+' '+relay+': '+tr(on?'Вкл':'Выкл');
  }

  function buildLegend(){
    var box=el('garageChartLg');
    if(!box) return;
    box.innerHTML='';
    for(var i=0;i<4;i++){
      var b=document.createElement('button');
      b.type='button';
      b.className='lg-chip'+(hidden[i]?' off':'');
      b.innerHTML='<i style="background:'+COLORS[i]+'"></i>'+tr('Реле')+' '+(i+1);
      (function(idx,btn){
        btn.addEventListener('click',function(){
          hidden[idx]=!hidden[idx];
          btn.classList.toggle('off',hidden[idx]);
          if(chart) chart.setDatasetVisibility(idx,!hidden[idx]);
          if(chart) chart.update('none');
        });
      })(i,b);
      box.appendChild(b);
    }
  }

  function buildChart(data){
    if(!window.Chart) return;
    captureZoom();
    var from=selRange.from.getTime(), to=selRange.to.getTime();
    // «Живое» окно — только те пресеты, чей конец = сейчас (не «Вчера»).
    var live=!userZoomed && relativeMode!==null && relativeMode!=='yesterday';
    var datasets=buildSeries(data,from,to,live);
    if(chart){ chart.destroy(); chart=null; }
    var opts={
      responsive:true, maintainAspectRatio:false, animation:false,
      interaction:{mode:'index',intersect:false},
      onHover:function(ev,els,c){
        if(!c.scales||!c.scales.x||ev.x<c.scales.x.left||ev.x>c.scales.x.right){ hideCursor(); return; }
        hover.t=c.scales.x.getValueForPixel(ev.x);
        c.update('none');
      },
      plugins:{
        legend:{display:false},
        tooltip:{
          callbacks:{
            title:function(items){ return items.length?new Date(items[0].parsed.x).toLocaleString():''; },
            label:tooltipLabel
          }
        },
        zoom:{
          pan:{enabled:!SR_COARSE, mode:'x', onPanComplete:function(){ userZoomed=true; }},
          zoom:{wheel:{enabled:!SR_COARSE, speed:0.1, modifierKey:'ctrl'}, pinch:{enabled:!SR_COARSE}, mode:'x',
                onZoomComplete:function(){ userZoomed=true; }},
          limits:{x:{minRange:60000}}
        }
      },
      scales:{
        x:{type:'time',
           time:{displayFormats:{minute:'HH:mm',hour:'HH:mm',day:'dd.MM.yyyy'},tooltipFormat:'dd.MM.yyyy HH:mm:ss'},
           ticks:{maxRotation:0,autoSkipPadding:16}},
        y:{min:-0.6,max:7.6,
           title:{display:true,text:tr('Реле (низ — OFF, верх — ON)')},
           ticks:{stepSize:1,callback:function(v){ return (v%2===0)?String(v/2+1):''; }},
           grid:{color:function(c){ return (c.tick&&c.tick.value%2===0)?'rgba(0,0,0,0.18)':'rgba(0,0,0,0.06)'; }}}
      }
    };
    if(userZoomed && savedZoom && isFinite(savedZoom.min) && isFinite(savedZoom.max)){
      opts.scales.x.min=savedZoom.min;
      opts.scales.x.max=savedZoom.max;
    }
    chart=new Chart(el('garageChart'),{type:'line',data:{datasets:datasets},options:opts});
    srCtrlDragZoom(chart, function(f,t){ setPeriod(f,t,null,null); });
    buildLegend();
    var canvas=el('garageChart');
    if(canvas && !canvas.__garageHover){
      canvas.__garageHover=true;
      canvas.addEventListener('mouseleave',hideCursor);
    }
    setRangeLabel();
  }

  function captureZoom(){
    if(!chart||!chart.scales||!chart.scales.x) return;
    var sc=chart.scales.x;
    if(isFinite(sc.min)&&isFinite(sc.max)) savedZoom={min:sc.min,max:sc.max};
  }

  function hideCursor(){ hover.t=null; if(chart) chart.update('none'); }

  function setRangeLabel(err){
    var box=el('garageChartRange');
    if(!box) return;
    if(err){ box.textContent=tr('Ошибка загрузки данных'); return; }
    if(!selRange.from||!selRange.to) return;
    box.textContent=selRange.from.toLocaleString()+' — '+selRange.to.toLocaleString();
  }

  // ---------------- Периоды (как на BMS) -----------------------------------
  function startOfToday(){ var d=new Date(); d.setHours(0,0,0,0); return d; }
  function endOfDay(d){ var x=new Date(d); x.setHours(23,59,59,999); return x; }
  function startOfMonthOf(d){ return new Date(d.getFullYear(),d.getMonth(),1,0,0,0,0); }
  function endOfMonthOf(d){ return new Date(d.getFullYear(),d.getMonth()+1,0,23,59,59,999); }
  function toInputDateTime(d){ var p=function(n){return (n<10?'0':'')+n;}; return d.getFullYear()+'-'+p(d.getMonth()+1)+'-'+p(d.getDate())+'T'+p(d.getHours())+':'+p(d.getMinutes()); }
  function toInputDate(d){ var p=function(n){return (n<10?'0':'')+n;}; return d.getFullYear()+'-'+p(d.getMonth()+1)+'-'+p(d.getDate()); }
  function dayFromStr(s){ var p=s.split('-'); return new Date(+p[0],+p[1]-1,+p[2],0,0,0,0); }
  function dtFromStr(s){ return new Date(s); }

  function setPeriod(from,to,btn,mode,nav){ // btn: id кнопки; mode: скользящий токен; nav: шаг ‹/›
    captureZoom();
    selRange.from=from; selRange.to=to;
    activeBtn=btn||null;
    relativeMode=mode||null;
    navMode=nav||null;
    userZoomed=false; savedZoom=null;
    var ids=['gBtn24h','gBtnToday','gBtnYesterday','gBtn7d','gBtnMonth'];
    for(var i=0;i<ids.length;i++){ var b=el(ids[i]); if(b) b.classList.toggle('active',ids[i]===activeBtn); }
    var f=el('gFromPick'), t=el('gToPick'), dp=el('gDatePick');
    if(f) f.value=toInputDateTime(from);
    if(t) t.value=toInputDateTime(to);
    if(dp) dp.value=toInputDate(from);
    loadChart();
  }

  function shiftPeriod(dir){
    var from,to;
    if(navMode==='day'){
      var d=new Date(selRange.from); d.setDate(d.getDate()+dir);
      from=new Date(d.getFullYear(),d.getMonth(),d.getDate(),0,0,0,0);
      to=endOfDay(from);
    }else if(navMode==='month'){
      var m=new Date(selRange.from.getFullYear(),selRange.from.getMonth()+dir,1,0,0,0,0);
      from=startOfMonthOf(m); to=endOfMonthOf(m);
    }else{
      var span=selRange.to.getTime()-selRange.from.getTime();
      from=new Date(selRange.from.getTime()+dir*span);
      to=new Date(selRange.to.getTime()+dir*span);
    }
    var now=new Date();
    if(to>now) to=now;
    if(from>=to) return;
    // Скользящий режим сбрасываем, шаг навигации сохраняем.
    setPeriod(from,to,null,null,navMode);
  }

  function relativeRange(){
    var now=new Date();
    if(relativeMode==='24h') return {from:new Date(now.getTime()-24*3600*1000),to:now};
    if(relativeMode==='today') return {from:startOfToday(),to:now};
    if(relativeMode==='yesterday'){ var s0=startOfToday(); var y=new Date(s0.getTime()-86400000); return {from:y,to:new Date(s0.getTime()-1)}; }
    if(relativeMode==='7d') return {from:new Date(now.getTime()-7*86400000),to:now};
    if(relativeMode==='month'){ var s2=startOfMonthOf(now); return {from:s2,to:now}; }
    return null;
  }

  async function loadChart(){
    if(window.srRefresh && !window.srRefresh.isEnabled()) return;
    if(!userZoomed){
      var rr=relativeRange();
      if(rr){ selRange=rr; }
    }
    if(!selRange.from||!selRange.to) return;
    try{
      var url='/api/garage/events?from='+encodeURIComponent(selRange.from.toISOString())+'&to='+encodeURIComponent(selRange.to.toISOString());
      var r=await fetch(url,{cache:'no-store'});
      if(!r.ok){ setRangeLabel(true); return; }
      buildChart(await r.json());
    }catch(e){ setRangeLabel(true); }
  }

  el('gBtn24h').addEventListener('click',function(){ var n=new Date(); setPeriod(new Date(n.getTime()-24*3600*1000),n,'gBtn24h','24h','span'); });
  el('gBtnToday').addEventListener('click',function(){ setPeriod(startOfToday(),new Date(),'gBtnToday','today','day'); });
  el('gBtnYesterday').addEventListener('click',function(){ var s=startOfToday(); var y=new Date(s.getTime()-86400000); setPeriod(y,endOfDay(y),'gBtnYesterday','yesterday','day'); });
  el('gBtn7d').addEventListener('click',function(){ var n=new Date(); setPeriod(new Date(n.getTime()-7*86400000),n,'gBtn7d','7d','span'); });
  el('gBtnMonth').addEventListener('click',function(){ var n=new Date(); setPeriod(startOfMonthOf(n),n,'gBtnMonth','month','month'); });
  el('gBtnDate').addEventListener('click',function(){ var v=el('gDatePick').value; if(!v) return; var d=dayFromStr(v); setPeriod(d,endOfDay(d),null); });
  el('gBtnRange').addEventListener('click',function(){ var f=el('gFromPick').value,t=el('gToPick').value; if(!f||!t) return; setPeriod(dtFromStr(f),dtFromStr(t),null); });
  el('gBtnPeriodPrev').addEventListener('click',function(){ shiftPeriod(-1); });
  el('gBtnPeriodNext').addEventListener('click',function(){ shiftPeriod(1); });
  el('gBtnRefresh').addEventListener('click',function(){ userZoomed=false; savedZoom=null; loadChart(); });

  // ---------------- Таймеры -----------------------------------------------
  var stateTimer=null, chartTimer=null;
  function startState(){ if(stateTimer) return; loadState(); stateTimer=setInterval(loadState,1000); }
  function stopState(){ if(stateTimer){ clearInterval(stateTimer); stateTimer=null; } }
  function startChart(){ if(chartTimer) return; loadChart(); chartTimer=setInterval(loadChart,60000); }
  function stopChart(){ if(chartTimer){ clearInterval(chartTimer); chartTimer=null; } }
  if(window.srRefresh){ window.srRefresh.register(startState,stopState); window.srRefresh.register(startChart,stopChart); }

  // Первичный период — 24 часа.
  (function init(){
    var n=new Date();
    selRange.from=new Date(n.getTime()-24*3600*1000);
    selRange.to=n;
    activeBtn='gBtn24h'; relativeMode='24h';
    var b=el('gBtn24h'); if(b) b.classList.add('active');
    var f=el('gFromPick'),t=el('gToPick'),dp=el('gDatePick');
    if(f) f.value=toInputDateTime(selRange.from);
    if(t) t.value=toInputDateTime(selRange.to);
    if(dp) dp.value=toInputDate(selRange.from);
  })();
  startState();
  startChart();
})();

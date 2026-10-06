(function(){
'use strict';

// ---------- Глобальный выключатель обновления данных ----------
// Чекбокс «Отключить обновление» в правом верхнем углу каждой страницы.
// Включён — опрос API и обновление данных по таймеру полностью останавливаются;
// выключен — все таймерные события срабатывают немедленно по одному разу, далее —
// обычное обновление по таймеру. Каждый периодический поллер страницы
// регистрирует hook (enable/disable): srRefresh вызывает disable при выключении
// и enable при включении (enable обязан один раз сработать сразу — «однократное
// срабатывание»). gate()/isEnabled() страхуют точечные запросы (кнопки/периоды),
// чтобы при выключенном обновлении данные вообще не опрашивались. Состояние не
// сохраняется: после загрузки/перезагрузки страницы выключатель всегда выключен
// (= обновление включено).
window.srRefresh = (function(){
  var enabled=true, hooks=[], checkbox=null;
  function runEnable(h){ try{ h.enable(); }catch(e){} }
  function runDisable(h){ try{ h.disable(); }catch(e){} }
  function refreshUI(){
    if(!checkbox) return;
    checkbox.checked = !enabled; // checked ⇒ обновление отключено
    var lbl = checkbox.closest ? checkbox.closest('label') : null;
    if(lbl) lbl.classList.toggle('off', !enabled);
  }
  function set(v){
    v=!!v;
    if(v===enabled) return;
    enabled=v;
    for(var i=0;i<hooks.length;i++){ if(v) runEnable(hooks[i]); else runDisable(hooks[i]); }
    refreshUI();
  }
  function bind(){
    checkbox=document.getElementById('refreshToggle');
    if(!checkbox) return;
    checkbox.addEventListener('change', function(){ set(!checkbox.checked); });
    refreshUI();
  }
  if(document.readyState==='loading'){ document.addEventListener('DOMContentLoaded', bind); }
  else { bind(); }
  return {
    // Обновление разрешено?
    isEnabled: function(){ return enabled; },
    // Выполнить fn только при включённом обновлении; иначе — no-op. Возвращает
    // true, если fn сработала (для кнопок, которые при отключённом обновлении
    // должны оставаться «молчаливыми»).
    gate: function(fn){ if(enabled){ try{ return !!fn(); }catch(e){} } return false; },
    // Зарегистрировать периодический поллер. enable вызывается при включении
    // обновления, disable — при выключении. Ни один не вызывается при регистрации:
    // стартовый запуск делает сама страница (аналог «однократного срабатывания»).
    register: function(enable, disable){ hooks.push({enable:enable, disable:disable}); },
    set: set
  };
})();

// Факт реального касания — глушит хинт со значениями (onHover) даже если
// SR_COARSE не сработал (надёжнее, чем pointer:coarse один).
document.addEventListener('touchstart', function(){ window.__srTouched = true; }, {passive:true, once:true});
var SR_COARSE = ('ontouchstart' in window) || (navigator.maxTouchPoints > 0)
  || (window.matchMedia && matchMedia('(pointer: coarse)').matches);
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
//   - щипок двумя пальцами — зум по X (окно зафиксировано под серединой пальцев);
//   - двойной тап — сброс зума;
//   - тап — onTap(px), если задан. На страницах графиков хинт со значениями на
//     мобильной версии отключён (onHover не вызывает chart.update на каждый
//     touchmove — он мешал зуму), поэтому onTap не передаётся.
// Плагин chartjs-plugin-zoom на touch-устройствах отключён (pan/wheel/pinch),
// чтобы его Hammer не перехватывал жесты и не мешал скроллу страницы.
window.srTouchChart = function(getChart, canvasId, minSpan, onTap, onGestureComplete){
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
    if(e.touches.length>=2){
      // preventDefault на двухпальцевом жесте — браузер не перехватывает его
      // под нативные жесты (зум/скролл страницы).
      e.preventDefault();
      beginPinch(c, e);
    } else if(e.touches.length===1){
      st={mode:'maybe', x0:e.touches[0].clientX, y0:e.touches[0].clientY, lastX:e.touches[0].clientX, t:Date.now(), decided:null};
    }
  }, {passive:false});
  canvas.addEventListener('touchmove', function(e){
    var c=getChart(); if(!c) return;
    if(e.touches.length>=2){
      // Двухпальцевый жест — щипок, даже если первый touchstart потерян.
      if(!st || st.mode!=='pinch') beginPinch(c, e);
      e.preventDefault();
      doPinch(c, e);
      return;
    }
    if(!st) return;
    if(e.touches.length===1 && st.mode==='maybe'){
      var dx=e.touches[0].clientX-st.x0, dy=e.touches[0].clientY-st.y0;
      if(st.decided===null && (Math.abs(dx)>12 || Math.abs(dy)>12)) st.decided=(Math.abs(dx)>Math.abs(dy)*1.2)?'pan':'scroll';
      if(st.decided==='pan'){
        e.preventDefault();
        var w=xwin(c);
        if(w && c.chartArea){
          var dData=-(e.touches[0].clientX-st.lastX)/c.chartArea.width*(w.max-w.min);
          var dMin=w.min+dData, dMax=w.max+dData;
          try{ c.zoomScale('x', {min:dMin, max:dMax}, 'none'); }catch(err){}
        }
        st.lastX=e.touches[0].clientX;
      }
    }
  }, {passive:false});
  canvas.addEventListener('touchend', function(e){
    // Палец отпущен во время щипка: не роняем жест — продолжаем панораму
    // оставшимся пальцем.
    if(st && st.mode==='pinch' && e.touches.length===1){
      var t0=e.touches[0];
      st={mode:'maybe', x0:t0.clientX, y0:t0.clientY, lastX:t0.clientX, t:Date.now(), decided:'pan'};
      return;
    }
    if(!st || st.mode!=='maybe' || st.decided==='pan'){
      // Жест завершён: панорама (swipe) или щипок (pinch) — не сброс.
      var done=(st && (st.mode==='pinch' || st.decided==='pan'));
      st=null;
      if(done && onGestureComplete) onGestureComplete(false);
      return;
    }
    var dt=Date.now()-st.t;
    var t=e.changedTouches[0];
    if(dt<300 && t){
      var rect=canvas.getBoundingClientRect();
      var px=t.clientX-rect.left;
      var c=getChart();
      if(c && c.chartArea && px>=c.chartArea.left && px<=c.chartArea.right){
        if(Date.now()-lastTap.t<320 && Math.abs(px-lastTap.px)<40){
          try{ c.resetZoom(); }catch(err){}
          // Двойной тап = сброс зума: onGestureComplete(true) — окно возвращается
          // к стандартному виду, правый край должен снова догонять now.
          if(onGestureComplete) onGestureComplete(true);
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

// Расшифровки кодов ошибок/алармов (англ. технический код → русская фраза).
// Сама локализация ru/en/zh выполняется словарём i18n.js (ключ = русская фраза).
var SR_ALARM_RU = {
  'Charge: Overvoltage protection':'Перезаряд: защита по напряжению',
  'Charge: Over current protection':'Заряд: защита по току',
  'Charge: Total overpressure':'Заряд: общее перенапряжение',
  'Charge: Battery overtemperature':'Заряд: перегрев батареи',
  'Charge: Power overtemperature':'Заряд: перегрев силовой части',
  'Charge: Abnormal current':'Заряд: аномальный ток',
  'Charge: Balanced line dropped':'Заряд: обрыв балансировочной линии',
  'Charge: Motherboard overtemperature':'Заряд: перегрев платы управления',
  'Charge: Discharge tube abnormality':'Заряд: неисправность разрядного ключа',
  'Discharge: Over-discharge protection':'Разряд: защита от глубокого разряда',
  'Discharge: Over current protection':'Разряд: защита по току',
  'Discharge: Total undervoltage':'Разряд: общее пониженное напряжение',
  'Discharge: Battery overtemperature':'Разряд: перегрев батареи',
  'Discharge: Power overtemperature':'Разряд: перегрев силовой части',
  'Discharge: Abnormal current':'Разряд: аномальный ток',
  'Discharge: Balanced line dropped':'Разряд: обрыв балансировочной линии',
  'Discharge: Motherboard overtemperature':'Разряд: перегрев платы управления',
  'Discharge: Short circuit protection':'Разряд: защита от короткого замыкания',
  'Discharge: Discharge tube abnormality':'Разряд: неисправность разрядного ключа',
  'Discharge: Start exception':'Разряд: ошибка запуска',
  'Balance: Balance overtemperature':'Балансировка: перегрев',
  'Balance: Motherboard overtemperature':'Балансировка: перегрев платы управления',
  'comm':'Нет связи с устройством',
  'no_data':'Нет данных от устройства',
  'неверный Modbus-адрес устройства':'Неверный Modbus-адрес устройства',
  'SN логгера не совпадает':'Серийный номер логгера не совпадает',
  'ID01 Grid OV':'Сеть: превышение напряжения','ID02 Grid UV':'Сеть: пониженное напряжение',
  'ID03 Grid OF':'Сеть: повышение частоты','ID04 Grid UF':'Сеть: понижение частоты',
  'ID05 PV UV':'PV: пониженное напряжение','ID06 LVRT':'LVRT (просадка сети)',
  'ID09 PV OV':'PV: превышение напряжения','ID10 PV current unbalanced':'PV: разбаланс тока',
  'ID11':'ID11','ID12 GFCI':'GFCI (утечка на землю)','ID13 phase sequence':'Чередование фаз',
  'ID14 boost OC':'Boost: перегрузка по току','ID15 AC OC':'AC: перегрузка по току','ID16 grid current high':'Сеть: высокий ток'
};

function SR_descOf(code, msg){
  if(code && SR_ALARM_RU[code]) return SR_ALARM_RU[code];
  if(code==='неизвестный' ) return msg||code;
  if(msg && /[\u0400-\u04FF]/.test(msg)) return msg;
  if(code && /^0x[0-9A-Fa-f]+$/.test(code)) return 'Неизвестный код ' + code;
  if(msg) return msg;
  return code || '';
}

// srCtrlDragZoom — универсальное управление зумом «Ctrl + выделение отрезка».
// При зажатом Ctrl (или Cmd на macOS) ЛКМ на графике рисуется прямоугольник
// выделения по оси X; по отпусканию вызывается cb(fromDate, toDate) — обычно
// страница выставляет это окно как полный период всех своих графиков.
// Работает поверх chartjs-plugin-zoom: capture-обработчик mousedown на document
// гасит propagation, чтобы панорамирование плагина не мешало выделению.
// Обработчик навешивается на canvas ОДИН раз (при повторных renderChart колбэк
// просто заменяется).
function srCtrlDragZoom(chart, cb){
  if(!chart || !chart.canvas || typeof document==='undefined') return;
  var canvas=chart.canvas;
  // График пересоздаётся на том же canvas при каждой перезагрузке — храним
  // ТЕКУЩИЙ объект chart и колбэк на canvas, а сами обработчики навешиваем один раз.
  canvas.__srCtrlChart=chart;
  canvas.__srCtrlCb=cb;
  if(canvas.__srCtrlBound) return;
  canvas.__srCtrlBound=true;

  // Стиль выделения вставляем один раз.
  if(!document.getElementById('srXselStyle')){
    var st=document.createElement('style'); st.id='srXselStyle';
    st.textContent='.sr-xsel{position:absolute;display:none;pointer-events:none;z-index:5;background:rgba(66,139,202,0.18);border:1px solid rgba(66,139,202,0.85);box-sizing:border-box}';
    (document.head||document.documentElement).appendChild(st);
  }

  var drag=null, box=null;
  function cur(){ return canvas.__srCtrlChart; }
  function area(){ var ch=cur(); return (ch&&ch.chartArea) || {left:0,right:(ch?ch.width:canvas.width),top:0,bottom:(ch?ch.height:canvas.height)}; }
  function rel(e){
    var ch=cur();
    if(ch && window.Chart && Chart.helpers && Chart.helpers.getRelativePosition){
      return Chart.helpers.getRelativePosition(e, ch);
    }
    var r=canvas.getBoundingClientRect();
    return { x:e.clientX-r.left, y:e.clientY-r.top };
  }
  function ensureBox(){
    var parent=canvas.parentNode; if(!parent) return null;
    if(getComputedStyle(parent).position==='static') parent.style.position='relative';
    if(!box||box.parentNode!==parent){ box=document.createElement('div'); box.className='sr-xsel'; parent.appendChild(box); }
    return box;
  }
  function xclamp(x){ var a=area(); return Math.max(a.left, Math.min(a.right, x)); }
  function paint(){
    if(!drag||!box) return;
    var a=area();
    var x0=xclamp(Math.min(drag.x0,drag.x1)), x1=xclamp(Math.max(drag.x0,drag.x1));
    box.style.left=(canvas.offsetLeft+x0)+'px';
    box.style.top=(canvas.offsetTop+a.top)+'px';
    box.style.width=Math.max(0,x1-x0)+'px';
    box.style.height=(a.bottom-a.top)+'px';
    box.style.display='block';
  }
  function stop(e){ e.stopPropagation(); e.preventDefault(); }
  function onDown(e){
    if(!e.ctrlKey && !e.metaKey) return;
    if(e.button!==0 || e.target!==canvas) return;
    stop(e);
    ensureBox();
    var r=rel(e); drag={x0:r.x, x1:r.x};
    canvas.style.cursor='crosshair';
    paint();
    document.addEventListener('mousemove', onMove, true);
    document.addEventListener('mouseup', onUp, true);
  }
  function onMove(e){ if(!drag) return; stop(e); drag.x1=rel(e).x; paint(); }
  function onUp(e){
    if(!drag) return;
    stop(e);
    document.removeEventListener('mousemove', onMove, true);
    document.removeEventListener('mouseup', onUp, true);
    var x0=xclamp(Math.min(drag.x0,drag.x1)), x1=xclamp(Math.max(drag.x0,drag.x1));
    drag=null; if(box) box.style.display='none'; canvas.style.cursor='';
    if(x1-x0 < 8) return; // слишком короткое выделение — не зум
    var ch=cur(); var sxx=ch && ch.scales && ch.scales.x; if(!sxx) return;
    var v0=sxx.getValueForPixel(x0), v1=sxx.getValueForPixel(x1);
    if(!isFinite(v0)||!isFinite(v1)) return;
    var from=new Date(Math.min(v0,v1)), to=new Date(Math.max(v0,v1));
    if(to-from < 60000) return; // минимум 1 минута (как limits зума)
    if(typeof canvas.__srCtrlCb==='function') canvas.__srCtrlCb(from, to);
  }
  document.addEventListener('mousedown', onDown, true);
  // На случай, если зум-плагин/SDK слушает pointer-события: гасим их при Ctrl,
  // чтобы выделение не превращалось в панорамирование.
  document.addEventListener('pointerdown', function(e){
    if((e.ctrlKey||e.metaKey) && e.button===0 && e.target===canvas){ e.stopPropagation(); }
  }, true);
}

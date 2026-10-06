'use strict';
// Страница истории ошибок всех устройств (/errors): таблица с фильтрами
// (тип, устройство, период) и пагинацией. Данные — /api/errors (PG device_errors),
// сервер отдаёт последние сверху (ORDER BY ts DESC) с limit/offset и total.
function pad(x){ return (x<10?'0':'')+x; }
function toLocalInput(d){ return d.getFullYear()+'-'+pad(d.getMonth()+1)+'-'+pad(d.getDate())+'T'+pad(d.getHours())+':'+pad(d.getMinutes()); }
function fmtTs(s){ var d=new Date(s); if(isNaN(d)) return s; return d.getFullYear()+'-'+pad(d.getMonth()+1)+'-'+pad(d.getDate())+' '+pad(d.getHours())+':'+pad(d.getMinutes())+':'+pad(d.getSeconds()); }
function esc(s){ return String(s).replace(/[&<>"]/g,function(c){ return {'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;'}[c]; }); }

var KIND_LABEL={inverter:'Инвертор',map:'МАП',mppt:'MPPT',meter:'Счётчик',ce308:'CE308',antbms:'ANT BMS',enbms:'EnBMS'};

var errOffset=0;
var errLimit=100;
var errTotal=0;

async function loadErrors(resetOffset){
  if(resetOffset) errOffset=0;
  var kind=document.getElementById('errKind').value;
  var device=document.getElementById('errDevice').value.trim();
  var from=document.getElementById('errFrom').value;
  var to=document.getElementById('errTo').value;
  errLimit=parseInt(document.getElementById('errLimit').value,10)||100;
  var q=['limit='+errLimit,'offset='+errOffset];
  if(from) q.push('from='+encodeURIComponent(new Date(from).toISOString()));
  if(to) q.push('to='+encodeURIComponent(new Date(to).toISOString()));
  if(kind) q.push('kind='+encodeURIComponent(kind));
  if(device) q.push('device='+encodeURIComponent(device));
  var body=document.getElementById('errBody');
  try{
    var r=await fetch('/api/errors?'+q.join('&'));
    if(!r.ok){ body.innerHTML='<span class="missing">ошибка запроса</span>'; return; }
    var d=await r.json();
    var rows=d.errors||[];
    errTotal=d.total||0;
    updatePager();
    if(!rows.length){ body.innerHTML='<span class="missing">Нет ошибок за период</span>'; return; }
    var h='<table class="pivot-table"><thead><tr><th>Время</th><th>Тип</th><th>Устройство</th><th>Код</th><th>Описание</th></tr></thead><tbody>';
    for(var i=0;i<rows.length;i++){
      var x=rows[i];
      // «Описание» = локализуемая часть по коду + детальный текст (msg) через « · ».
      // Оба столбца локализуются; если описание совпадает с кодом (msg == code) —
      // в «Описании» ставим «—», чтобы не дублировать.
      var desc=SR_descOf(x.code, '');
      if(!desc) desc=x.msg||x.code;
      if(x.msg && x.msg!==desc) desc=desc+' · '+x.msg;
      if(desc===x.code) desc='—'; // не дублировать «Код»
      h+='<tr><td>'+esc(fmtTs(x.ts))+'</td><td>'+esc(KIND_LABEL[x.kind]||x.kind)+'</td><td>'+esc(x.device)+'</td><td>'+esc(x.code)+'</td><td>'+esc(desc)+'</td></tr>';
    }
    h+='</tbody></table>';
    body.innerHTML=h;
  }catch(e){ body.innerHTML='<span class="missing">ошибка запроса</span>'; }
}

function updatePager(){
  var pages=errTotal>0?Math.ceil(errTotal/errLimit):1;
  var page=Math.floor(errOffset/errLimit)+1;
  document.getElementById('errPageInfo').textContent='Стр. '+page+' из '+pages+' ('+errTotal+')';
  document.getElementById('errPrev').disabled=errOffset<=0;
  document.getElementById('errNext').disabled=(errOffset+errLimit)>=errTotal;
}

(function(){
  var to=new Date(), from=new Date(); from.setDate(from.getDate()-7);
  document.getElementById('errFrom').value=toLocalInput(from);
  document.getElementById('errTo').value=toLocalInput(to);
  document.getElementById('errApply').addEventListener('click',function(){ loadErrors(true); });
  document.getElementById('errLimit').addEventListener('change',function(){ loadErrors(true); });
  document.getElementById('errPrev').addEventListener('click',function(){ if(errOffset>0){ errOffset=Math.max(0,errOffset-errLimit); loadErrors(false); } });
  document.getElementById('errNext').addEventListener('click',function(){ if((errOffset+errLimit)<errTotal){ errOffset+=errLimit; loadErrors(false); } });
  loadErrors(true);
})();

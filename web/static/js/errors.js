'use strict';
// Страница истории ошибок всех устройств (/errors): таблица с фильтрами
// (тип, устройство, период). Данные — /api/errors (PG device_errors).
function pad(x){ return (x<10?'0':'')+x; }
function toLocalInput(d){ return d.getFullYear()+'-'+pad(d.getMonth()+1)+'-'+pad(d.getDate())+'T'+pad(d.getHours())+':'+pad(d.getMinutes()); }
function fmtTs(s){ var d=new Date(s); if(isNaN(d)) return s; return d.getFullYear()+'-'+pad(d.getMonth()+1)+'-'+pad(d.getDate())+' '+pad(d.getHours())+':'+pad(d.getMinutes())+':'+pad(d.getSeconds()); }
function esc(s){ return String(s).replace(/[&<>"]/g,function(c){ return {'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;'}[c]; }); }

var KIND_LABEL={inverter:'Инвертор',map:'МАП',mppt:'MPPT',meter:'Счётчик',ce308:'CE308',antbms:'ANT BMS',enbms:'EnBMS'};

async function loadErrors(){
  var kind=document.getElementById('errKind').value;
  var device=document.getElementById('errDevice').value.trim();
  var from=document.getElementById('errFrom').value;
  var to=document.getElementById('errTo').value;
  var q=[];
  if(from) q.push('from='+encodeURIComponent(new Date(from).toISOString()));
  if(to) q.push('to='+encodeURIComponent(new Date(to).toISOString()));
  if(kind) q.push('kind='+encodeURIComponent(kind));
  if(device) q.push('device='+encodeURIComponent(device));
  var body=document.getElementById('errBody');
  try{
    var r=await fetch('/api/errors'+(q.length?('?'+q.join('&')):''));
    if(!r.ok){ body.innerHTML='<span class="missing">ошибка запроса</span>'; return; }
    var d=await r.json();
    var rows=d.errors||[];
    if(!rows.length){ body.innerHTML='<span class="missing">Нет ошибок за период</span>'; return; }
    var h='<table class="pivot-table"><thead><tr><th>Время</th><th>Тип</th><th>Устройство</th><th>Код</th><th>Описание</th></tr></thead><tbody>';
    for(var i=0;i<rows.length;i++){
      var x=rows[i];
      var desc=SR_descOf(x.code, x.msg);
      if(x.msg && desc!==x.msg) desc=desc+' · '+x.msg;
      h+='<tr><td>'+esc(fmtTs(x.ts))+'</td><td>'+esc(KIND_LABEL[x.kind]||x.kind)+'</td><td>'+esc(x.device)+'</td><td>'+esc(x.code)+'</td><td>'+esc(desc)+'</td></tr>';
    }
    h+='</tbody></table>';
    body.innerHTML=h;
  }catch(e){ body.innerHTML='<span class="missing">ошибка запроса</span>'; }
}

(function(){
  var to=new Date(), from=new Date(); from.setDate(from.getDate()-7);
  document.getElementById('errFrom').value=toLocalInput(from);
  document.getElementById('errTo').value=toLocalInput(to);
  document.getElementById('errApply').addEventListener('click', loadErrors);
  loadErrors();
})();

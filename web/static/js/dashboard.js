'use strict';

// Флаги видимости блоков (из sunReceiver.json): скрытые рамки/плашки не рендерятся
// сервером, поэтому и обновления соответствующих элементов пропускаем.
var showMap = document.body.dataset.showMap === '1';
var showMeter = document.body.dataset.showMeter === '1';
var showBMS = document.body.dataset.showBms === '1';
var showCE308 = document.body.dataset.showCe308 === '1';

// ---------- Утилиты ----------
function esc(s){ return String(s).replace(/[&<>"]/g,function(c){ return {'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;'}[c]; }); }
function fmt(t){ var d=new Date(t); function p(x){return (x<10?'0':'')+x;} return d.getFullYear()+'-'+p(d.getMonth()+1)+'-'+p(d.getDate())+' '+p(d.getHours())+':'+p(d.getMinutes()); }
function fmtSec(t){ var d=new Date(t); function p(x){return (x<10?'0':'')+x;} return d.getFullYear()+'-'+p(d.getMonth()+1)+'-'+p(d.getDate())+' '+p(d.getHours())+':'+p(d.getMinutes())+':'+p(d.getSeconds()); }
function startOfToday(){ var d=new Date(); d.setHours(0,0,0,0); return d; }
function endOfToday(){ var d=new Date(); d.setHours(23,59,59,999); return d; }

// ---------- Сводная таблица параметров по инверторам ----------
// Упорядоченный список строк: [тег, подпись, единица измерения].
// Единица пишется в самую правую колонку и одинакова для всех инверторов.
var PARAMS = [
	['pv1_voltage','Напряжение PV1','V'], ['pv1_current','Ток PV1','A'], ['pv1_power','Мощность PV1','W'],
	['pv2_voltage','Напряжение PV2','V'], ['pv2_current','Ток PV2','A'], ['pv2_power','Мощность PV2','W'],
	['grid_frequency','Частота сети','Hz'],
	['l1_voltage','Напряжение L1','V'], ['l1_current','Ток L1','A'], ['l1_power','Мощность L1','W'],
	['energy_today','Выработка сегодня','kWh'], ['energy_total','Выработка всего','kWh']
];
// devValue возвращает строковое значение тега инвертора или null, если его нет.
// Для вычисляемых тегов (l1_power = l1_voltage × l1_current) значение считается из
// исходных тегов, если явного тега в данных нет.
function devValue(dev, tag){
	var v = (dev && dev.values) ? dev.values[tag] : undefined;
	if(v === undefined || v === null){
		if(tag === 'l1_power' && dev && dev.values){
			var u=dev.values['l1_voltage'], i=dev.values['l1_current'];
			// Мощность = U×I. Произведение чисел с плавающей точкой даёт длинную дробную
			// часть (например 228.9×0.35 = 80.11499999…) — округляем до 1 знака, как у остальных.
			if(u!==undefined && u!==null && i!==undefined && i!==null) v=Math.round(Number(u)*Number(i)*10)/10;
		}
	}
	return (v === undefined || v === null) ? null : v;
}
// devTempValue возвращает температуру инвертора по подписи («Корпус»/«Транзисторы»)
// в °C с учётом марки (как inverterTemps на сервере): у Deye — temperature_radiator
// (Корпус) и temperature_igbt (Транзисторы); у Sofar — temperature_inner (Корпус) и
// temperature_module (Транзисторы). Отсутствующий датчик у Deye маппится сентелом
// −100 (raw 0 → −100), поэтому значения ≤ −100 пропускаются.
function devTempValue(dev, which){
	var tags;
	if((dev&&dev.kind)==='deye') tags={'Корпус':'temperature_radiator','Транзисторы':'temperature_igbt'};
	else if((dev&&dev.kind)==='sofar') tags={'Корпус':'temperature_inner','Транзисторы':'temperature_module'};
	else return null;
	var v = (dev && dev.values) ? dev.values[tags[which]] : undefined;
	return (v===undefined || v===null || Number(v)<=-100) ? null : Number(v);
}
var GRID_COLOR='#428bca', MPPT_COLOR='#5cb85c';
function renderPivot(devices){
	if(!devices || !devices.length) return '<div class="missing">No data in Redis</div>';
	// Разбиваем колонки на две группы: сетевые инверторы (первые) и MPPT-контроллеры (последние).
	// Устройство МАП (батарея/сеть) в сводной таблице не показываем — у него нет солнечных
	// панелей; оно нужно только для верхних плашек и графиков. Маркер MPPT — ip вида host#mpptN.
	var grid=[], mpts=[];
	for(var i=0;i<devices.length;i++){
		var d=devices[i];
		if(isMAPDeviceJS(d) || isMeterDevice(d)) continue;
		if(String(d.ip||'').indexOf('#mppt')>=0) mpts.push(d); else grid.push(d);
	}
	if(!grid.length && !mpts.length) return '<div class="missing">No data in Redis</div>';
	// Единая таблица: общий первый столбец (заголовки строк и единиц), затем колонки
	// сетевых инверторов и MPPT, разделённые тонкой цветной рамкой групп.
	var h='<table class="pivot-table"><thead>';
	// Строка заголовков групп (colspan по числу колонок группы).
	h+='<tr><th class="p-label">Параметр</th>';
	if(grid.length) h+='<th colspan="'+grid.length+'" style="border:1px solid '+GRID_COLOR+';color:'+GRID_COLOR+'">Сетевые инверторы</th>';
	if(mpts.length) h+='<th colspan="'+mpts.length+'" style="border:1px solid '+MPPT_COLOR+';color:'+MPPT_COLOR+'">MPPT-контроллеры</th>';
	h+='</tr><tr><th></th>';
	for(var i=0;i<grid.length;i++) h+='<th>'+esc(grid[i].name)+'</th>';
	for(var i=0;i<mpts.length;i++) h+='<th>'+esc(mpts[i].name)+'</th>';
	h+='</tr></thead><tbody>';
	// Общие строки. Первый столбец — заголовок строки; аудитория по обеим группам.
	// «Актуально» у offline-устройства (снимок старше STALE_MS) — красное;
	// время недоступности — второй строкой через <br> (не растягивает колонки).
	h+='<tr><td class="p-label">Актуально</td>'+rowCells(grid,mpts,function(d){
		if(!d.timestamp) return null;
		return isStaleDev(d) ? fmtSec(d.timestamp)+'<br>'+agoStr(new Date(d.timestamp).getTime()) : fmtSec(d.timestamp);
	},'11px',function(d){ return isStaleDev(d)?'stale-time':''; },true)+'</tr>';
	h+='<tr><td class="p-label">Серийный номер инвертора</td>'+rowCells(grid,mpts,function(d){return d.inverter_sn||null;},'11px')+'</tr>';
	h+='<tr><td class="p-label">Серийный номер логгера</td>'+rowCells(grid,mpts,function(d){return d.device_sn||null;},'11px')+'</tr>';
	// Данные ниже серийных номеров у offline-устройства не выводятся — значения
	// устарели (ночь/авария); серийные номера постоянны, они остаются.
	// Мощности — сразу после серийных номеров; значения — ярко-светло-зелёные (tr.p-power).
	h+='<tr class="p-power"><td class="p-label">Активная мощность (W)</td>'+rowCells(grid,mpts,function(d){return isStaleDev(d)?null:devValue(d,'ac_active_power');})+'</tr>';
	h+='<tr class="p-power"><td class="p-label">Реактивная мощность (var)</td>'+rowCells(grid,mpts,function(d){return isStaleDev(d)?null:devValue(d,'ac_reactive_power');})+'</tr>';
	for(var p=0;p<PARAMS.length;p++){
		var tag=PARAMS[p][0], label=PARAMS[p][1], unit=PARAMS[p][2];
		// Температуры инвертора (Корпус/Транзисторы) — перед строками «Выработка ...».
		if(tag==='energy_today'){
			h+='<tr><td class="p-label">t корпус (°C)</td>'+rowCells(grid,mpts,function(d){return isStaleDev(d)?null:devTempValue(d,'Корпус');})+'</tr>';
			h+='<tr><td class="p-label">t транзисторы (°C)</td>'+rowCells(grid,mpts,function(d){return isStaleDev(d)?null:devTempValue(d,'Транзисторы');})+'</tr>';
		}
		// Накопительная выработка у offline-инвертора — последнее зарегистрированное
		// значение (сколько выработал за день/всего до остановки) — оставляем,
		// но серым (td.stale-keep).
		var keepStale=(tag==='energy_today'||tag==='energy_total');
		h+='<tr><td class="p-label">'+esc(label)+' ('+esc(unit)+')</td>'+rowCells(grid,mpts,function(d){return (!keepStale && isStaleDev(d))?null:devValue(d,tag);},null,keepStale?function(d){return isStaleDev(d)?'stale-keep':'';}:null)+'</tr>';
	}
	// Нижняя кромка рамок групп.
	h+='<tr><td></td>';
	for(var i=0;i<grid.length;i++) h+=edgeCell(GRID_COLOR,i===0,i===grid.length-1);
	for(var i=0;i<mpts.length;i++) h+=edgeCell(MPPT_COLOR,i===0,i===mpts.length-1);
	h+='</tr>';
	h+='</tbody></table>';
	return h;
}
// rowCells формирует ячейки строки по колонкам обеих групп. Первая колонка группы
// получает цветную левую, последняя — цветную правую рамку (вертикальные стороны).
function rowCells(grid,mpts,getter,font,clsFn,raw){
	var h='';
	for(var i=0;i<grid.length;i++) h+=cellTd(grid[i],getter,i===0,i===grid.length-1,GRID_COLOR,font,clsFn,raw);
	for(var i=0;i<mpts.length;i++) h+=cellTd(mpts[i],getter,i===0,i===mpts.length-1,MPPT_COLOR,font,clsFn,raw);
	return h;
}
function cellTd(d,getter,isFirst,isLast,color,font,clsFn,raw){
	var v=getter(d);
	var st='';
	if(isFirst) st+='border-left:1px solid '+color+';';
	if(isLast) st+='border-right:1px solid '+color+';';
	if(font) st+='font-size:'+font+';';
	var cls=v===null?'p-empty':'p-val';
	if(clsFn){ var extra=clsFn(d); if(extra) cls+=' '+extra; }
	// raw — getter возвращает доверенный HTML (напр. <br>); иначе экранируем.
	var txt=v===null?'':(raw?v:esc(v));
	return '<td class="'+cls+'" style="'+st+'">'+txt+'</td>';
}
// edgeCell формирует пустую ячейку нижней кромки группы с цветной горизонтальной чертой.
function edgeCell(color,isFirst,isLast){
	var st='border-top:1px solid '+color+';';
	if(isFirst) st+='border-left:1px solid '+color+';';
	if(isLast) st+='border-right:1px solid '+color+';';
	return '<td class="p-empty" style="'+st+'"></td>';
}
// isMeterDevice возвращает true, если устройство — электросчётчик DDS238 (имеет
// теги meter_*). Используется, чтобы не выводить счётчик в сводную таблицу
// инверторов и не считать его в «инверторах онлайн».
function isMeterDevice(d){ return !!(d && d.values && d.values.meter_voltage !== undefined); }
// isMAPDeviceJS — маркер устройства МАП (батарея/сеть) в JS (аналог isMAPDevice в Go).
function isMAPDeviceJS(d){ return !!(d && d.values && d.values.battery_voltage !== undefined); }
// STALE_MS — окно «молчания» устройства: если последний снимок старше 20 минут
// (то же окно, что TOOLTIP_MAX_GAP на графиках), устройство считаем оффлайн —
// напр. ночью, когда инверторы выключены и перестали присылать данные.
// Таблица при этом показывает последнее известное значение, но приглушает его
// (td.stale), «Актуально» краснеет с возрастом, «инверторов онлайн» и суммарная
// мощность (/api/current) offline-устройства не включают.
var STALE_MS=20*60*1000;
function isStaleDev(d){
	var t=(d && d.timestamp) ? new Date(d.timestamp).getTime() : NaN;
	return !isFinite(t) || (Date.now()-t)>STALE_MS;
}
function agoStr(t){
	var m=Math.floor((Date.now()-t)/60000);
	if(m<1) return 'меньше минуты';
	if(m<60) return m+' мин';
	var h=Math.floor(m/60);
	if(h<24) return h+' ч '+(m%60)+' мин';
	return Math.floor(h/24)+' д '+(h%24)+' ч';
}

// MTR_STALE_MS — окно «молчания» электросчётчика DDS238. Если последний снэпшот
// счётчика старше этого окна (счётчик обесточен/не отвечает), оперативные плашки
// (напряжение/ток/мощность/частота/коэффициент) выводят «—», а накопленные
// (потребление/отдача/тотал) продолжают показывать последнее значение. Отдельно
// от STALE_MS (20 мин для инверторов): счётчик опрашивается раз в секунду, и уже
// 20 с молчания означают, что данных нет. Отличается от «просто нулей»: ноль —
// это реальное значение счётчика, «—» — данных оперативных нет.
var MTR_STALE_MS=20*1000;
// CE308_STALE_MS — окно «молчания» электросчётчика CE308: опрашивается раз в ~2 с,
// поэтому уже 20 с без обновления снимка означают, что данные зависли (счётчик не
// отвечает / радиоканал потерян). При устаревании метка «Актуально» окрашивается
// в красный и рядом показывается возраст снимка (аналогично offline-инверторам).
var CE308_STALE_MS=20*1000;
// METER_PARAMS — параметры счётчика для плашки: [тег, подпись, единица, знаковый,
// накопленный]. Знаковые (активная/реактивная мощность) окрашиваются: отрицательная —
// отдача. Последние три (накопленные) продолжают отображаться даже при устаревшем
// снэпшоте; оперативные при устаревании выводят «—».
var METER_PARAMS = [
	['meter_voltage','Напряжение','V',false,false],
	['meter_current','Ток','A',false,false],
	['meter_active_power','Активная мощность','W',true,false],
	['meter_reactive_power','Реактивная мощность','var',true,false],
	['meter_power_factor','Коэффициент мощности','',false,false],
	['meter_frequency','Частота','Hz',false,false],
	['meter_import','Потребление (Import)','kWh',false,true],
	['meter_export','Отдача (Export)','kWh',false,true],
	['meter_total','Общая (Total)','kWh',false,true]
];
// renderMeter строит HTML статистик плашки счётчика из его снимка (или «Нет данных»).
// При устаревшем снэпшоте (> MTR_STALE_MS) оперативные значения выводятся как «—»,
// накопленные (import/export/total) продолжают показываться.
function renderMeter(meter){
	var stats=document.getElementById('meterStats');
	if(!stats) return;
	if(!meter){ stats.innerHTML='<span class="missing">Нет данных</span>'; return; }
	var ts=document.getElementById('meterTs');
	if(ts) ts.textContent=meter.timestamp? 'Актуально: '+fmtSec(meter.timestamp) : '—';
	// Свежесть снэпшота счётчика (0.02 окна — только для оперативных плашек).
	var t=(meter.timestamp)? new Date(meter.timestamp).getTime() : NaN;
	var stale=!isFinite(t) || (Date.now()-t)>MTR_STALE_MS;
	var h='';
	for(var i=0;i<METER_PARAMS.length;i++){
		var t=METER_PARAMS[i][0], lbl=METER_PARAMS[i][1], unit=METER_PARAMS[i][2], signed=METER_PARAMS[i][3], cumulative=METER_PARAMS[i][4];
		var raw=meter.values? meter.values[t] : undefined;
		// Оперативное значение при устаревшем снэпшоте данных не имеем — «—».
		if(stale && !cumulative){ h+='<div class="meter-stat"><div class="lbl">'+esc(lbl)+'</div><div class="val off">—</div></div>'; continue; }
		if(raw===undefined||raw===null){ h+='<div class="meter-stat"><div class="lbl">'+esc(lbl)+'</div><div class="val off">—</div></div>'; continue; }
		var n=Number(raw);
		var cls='val', txt;
		if(isFinite(n)){
			txt=n.toLocaleString('ru-RU',{maximumFractionDigits:2});
			// Инверсия цвета мощностей: положительная (потребление) — красная (neg),
			// отрицательная (отдача в сеть) — зелёная (pos).
			if(signed){ cls+=' '+(n<0?' pos':' neg'); }
		}else{
			cls+=' off'; txt='—';
		}
		h+='<div class="meter-stat"><div class="lbl">'+esc(lbl)+'</div><div class="'+cls+'">'+esc(txt)+
		   (unit?' <span class="unit">'+esc(unit)+'</span>':'')+'</div></div>';
	}
	stats.innerHTML=h;
}
// invPlatesSig — подпись набора размещений, по которой решаем, нужно ли пересобирать
// плашки рамки «Мощности инверторов» (при смене набора). Значения обновляются в tick
// точечно по id, без пересоздания DOM каждую секунду.
var invPlatesSig='';
function invPlural(n){ var m10=n%10, m100=n%100; if(m10===1 && m100!==11) return 'инвертор'; if(m10>=2 && m10<=4 && (m100<10||m100>=20)) return 'инвертора'; return 'инверторов'; }
// renderInvPlates перестраивает плашки рамки «Мощности инверторов» по набору
// размещений из /api/current: одна пара плашек (активная + PV) на каждое размещение,
// в порядке, заданном sunReceiver.json. Пересобираем только при изменении набора
// (число или имена размещений) — сами значения потом обновляются в tick.
function renderInvPlates(placements){
	var names=(placements||[]).map(function(p){return p.name;});
	var sig=names.join('\u0000');
	if(sig===invPlatesSig) return;
	invPlatesSig=sig;
	var box=document.getElementById('invPowerGroup');
	if(!box) return;
	var h='';
	for(var i=0;i<names.length;i++){
		// Под «Суммарной активной мощностью» первого размещения — строка
		// «N инверторов онлайн» (информация о доступности устройств).
		var sub=(i===0)?'<div class="sub" id="invPlatesSub">Нет данных</div>':'';
		h+='<div class="plate">'
		 +'<div class="lbl">Суммарная активная<br>мощность ('+esc(names[i])+')</div>'
		 +'<div class="val"><span id="kpiPlaceP'+i+'">—</span><span class="unit">W</span></div>'+sub
		 +'</div>';
		h+='<div class="plate">'
		 +'<div class="lbl">Суммарная мощность<br>PV ('+esc(names[i])+')</div>'
		 +'<div class="val"><span id="kpiPlacePV'+i+'">—</span><span class="unit">W</span></div>'
		 +'</div>';
	}
	box.innerHTML=h;
}

// ---------- Электросчётчик CE308 (текущие данные + показания) ----------
// Данные — из Redis через /api/ce308/current и /api/ce308/energy, обновление раз в секунду
// (вместе с tick спойлера «Детальные данные»). Текущий снимок приходит map имя→снимок,
// показания энергии — один объект (см. ce308.go). Знаки мощности: положительная —
// потребление, отрицательная — отдача в сеть.
function ce308Num(v){ return (v===undefined || v===null || !isFinite(Number(v))) ? null : Number(v); }
// ce308Fmt — 2 знака + разделители разрядов (для показаний энергии, левый столбец).
function ce308Fmt(v){ var n=ce308Num(v); return n===null ? '—' : n.toLocaleString('ru-RU',{minimumFractionDigits:2, maximumFractionDigits:2}); }
function setCe308Cell(id, v, signed){
	var el=document.getElementById(id);
	if(!el) return;
	var n=ce308Num(v);
	if(n===null){ el.textContent='—'; el.className='ce-num'; return; }
	// 2 знака после запятой + разделители разрядов. Знаковые (мощности): инверсия —
	// положительное (потребление) — красное, отрицательное (отдача) — зелёное.
	el.textContent=n.toLocaleString('ru-RU',{minimumFractionDigits:2, maximumFractionDigits:2});
	if(signed){ el.className='ce-num '+(n<0 ? 'ce-pos' : 'ce-neg'); }
	else{ el.className='ce-num'; }
}
function renderCE308Current(cur){
	var ts=document.getElementById('ce308CurTs');
	var name=Object.keys(cur||{})[0];
	var snap=name ? cur[name] : null;
	if(ts){
		if(snap && snap.timestamp){
			ts.textContent='Актуально: '+fmtSec(snap.timestamp);
			var t=new Date(snap.timestamp).getTime();
			// Свежесть снимка: при зависании (> CE308_STALE_MS) метка краснеет и
			// рядом показывается возраст — «данные заморожены» видно сразу.
			var stale=!isFinite(t) || (Date.now()-t)>CE308_STALE_MS;
			ts.classList.toggle('stale-time', stale);
			ts.title=stale ? ('Данные не обновляются: '+(Math.round((Date.now()-t)/1000))+' с назад') : '';
		}else{
			ts.textContent='Актуально: —';
			ts.classList.remove('stale-time');
			ts.title='';
		}
	}
	var v=(snap && snap.values) ? snap.values : null;
	var v1=ce308Num(v&&v.ce308_l1_voltage), v2=ce308Num(v&&v.ce308_l2_voltage), v3=ce308Num(v&&v.ce308_l3_voltage);
	setCe308Cell('ce308V1', v1); setCe308Cell('ce308V2', v2); setCe308Cell('ce308V3', v3);
	var i1=ce308Num(v&&v.ce308_l1_current), i2=ce308Num(v&&v.ce308_l2_current), i3=ce308Num(v&&v.ce308_l3_current);
	setCe308Cell('ce308I1', i1); setCe308Cell('ce308I2', i2); setCe308Cell('ce308I3', i3);
	setCe308Cell('ce308Isum', (i1!==null&&i2!==null&&i3!==null) ? (i1+i2+i3) : null);
	var p1=ce308Num(v&&v.ce308_l1_active_power), p2=ce308Num(v&&v.ce308_l2_active_power), p3=ce308Num(v&&v.ce308_l3_active_power);
	setCe308Cell('ce308P1', p1, true); setCe308Cell('ce308P2', p2, true); setCe308Cell('ce308P3', p3, true);
	// Сумма по фазам — с учётом знака (L1+L2+L3), а не Σ от датчика (дам появляться
	// взаимоисключающие фазы: одна отдаёт в сеть, другая потребляет).
	setCe308Cell('ce308Psum', (p1!==null&&p2!==null&&p3!==null) ? (p1+p2+p3) : null, true);
	var q1=ce308Num(v&&v.ce308_l1_reactive_power), q2=ce308Num(v&&v.ce308_l2_reactive_power), q3=ce308Num(v&&v.ce308_l3_reactive_power);
	setCe308Cell('ce308Q1', q1, true); setCe308Cell('ce308Q2', q2, true); setCe308Cell('ce308Q3', q3, true);
	setCe308Cell('ce308Qsum', (q1!==null&&q2!==null&&q3!==null) ? (q1+q2+q3) : null, true);
}
function renderCE308Energy(snap){
	var ts=document.getElementById('ce308EnTs');
	if(ts) ts.textContent = (snap && snap.timestamp) ? ('Актуально: '+fmtSec(snap.timestamp)) : 'Актуально: —';
	var s=snap||{};
	// Значения в ячейках карточек (День/Ночь × А+/А− и R+/R−) — левые подписи-надписи
	// заданы в HTML (.ce308-lbl), здесь только числа (целочисленный формат + «,—»).
	var set=function(id, v){ var el=document.getElementById(id); if(el) el.textContent=ce308Fmt(v); };
	set('ce308AtcDay', s.active_consumption_day);  set('ce308AtcNight', s.active_consumption_night);
	set('ce308AtdDay', s.active_delivery_day);     set('ce308AtdNight', s.active_delivery_night);
	set('ce308RtcDay', s.reactive_consumption_day); set('ce308RtcNight', s.reactive_consumption_night);
	set('ce308RtdDay', s.reactive_delivery_day);    set('ce308RtdNight', s.reactive_delivery_night);
	// Итоги по каждому виду энергии (потребление/отдача за день+ночь).
	set('ce308AtcTotal', s.active_consumption_total); set('ce308AtdTotal', s.active_delivery_total);
	set('ce308RtcTotal', s.reactive_consumption_total); set('ce308RtdTotal', s.reactive_delivery_total);
}
// Ограничение частоты ручного снимка энергии: кнопка «Обновить» не чаще раза в
// 5 минут (совпадает с бэкендом, POST /api/ce308/energy → 429 при частом нажатии).
var ce308RefreshAt=0, ce308LockTimer=null;
function ce308RefreshReserve(){
	ce308RefreshAt=Date.now();
	var btn=document.getElementById('ce308Refresh');
	if(btn){ btn.disabled=true; btn.textContent='5 мин'; }
	if(ce308LockTimer) clearTimeout(ce308LockTimer);
	ce308LockTimer=setTimeout(function(){
		if((Date.now()-ce308RefreshAt)>=5*60*1000){ // защита от сдвига таймера
			var b=document.getElementById('ce308Refresh');
			if(b){ b.disabled=false; b.textContent='Обновить'; }
		}
	}, 5*60*1000);
}
// tickCE308 — опрос текущих данных и показаний CE308 (вызывается из tick раз в секунду).
// Кнопка «Обновить» шлёт POST /api/ce308/energy — сигнал пулеру снять свежий снимок энергии.
async function tickCE308(){
	if(!showCE308 || (window.srRefresh && !window.srRefresh.isEnabled())) return;
	try{
		var r=await fetch('/api/ce308/current');
		if(r.ok) renderCE308Current(await r.json());
		var re=await fetch('/api/ce308/energy');
		if(re.ok) renderCE308Energy(await re.json());
	}catch(e){}
}
(function(){
	var btn=document.getElementById('ce308Refresh');
	if(!btn) return;
	btn.addEventListener('click', function(){
		if(window.srRefresh && !window.srRefresh.isEnabled()) return;
		// Фронтовая защита: не чаще раза в 5 минут (не дёргаем API при активном локе).
		if((Date.now()-ce308RefreshAt) < 5*60*1000) return;
		ce308RefreshReserve();
		fetch('/api/ce308/energy',{method:'POST'}).catch(function(){}).finally(function(){
			// Снимок уже зарезервирован на 5 мин — кнопка остаётся заблокированной.
		});
	});
})();

async function tick(){
	if(window.srRefresh && !window.srRefresh.isEnabled()) return;
	try{
		// Электросчётчик CE308 (текущие данные + показания) — опрашивается тем же
		// ежесекундным циклом спойлера «Детальные данные» (свои /api/ce308/*).
		tickCE308();
		var r=await fetch('/api/current');
		if(!r.ok) return;
		var data=await r.json();
		// Плашки «Мощности инверторов»: одна пара (активная + PV) на каждое размещение.
		var placements=data.placements||[];
		renderInvPlates(placements);
		// Число инверторов онлайн — без устройства МАП (батарея/сеть), счётчика,
		// MPPT-контроллеров (КЭС) и offline-устройств (снимок старше STALE_MS, напр.
		// ночью). Отображается под первой плашкой:
		var invCount=0;
		for(var i=0;i<data.devices.length;i++) if(!isMAPDeviceJS(data.devices[i]) && !isMeterDevice(data.devices[i]) && String(data.devices[i].ip||'').indexOf('#mppt')<0 && !isStaleDev(data.devices[i])) invCount++;
		for(var i=0;i<placements.length;i++){
			setKpi('kpiPlaceP'+i, placements[i].power);
			setKpi('kpiPlacePV'+i, placements[i].pv);
		}
		var subEl=document.getElementById('invPlatesSub');
		if(subEl) subEl.textContent=invCount ? (invCount+' '+invPlural(invCount)+' онлайн') : 'Нет данных';
		// Плашки МАП: напряжение/мощность сети и батареи.
		if(showMap){
			setKpi('kpiGridV', data.map_grid_voltage);
			setKpi('kpiGridP', data.map_grid_power);
			setKpi('kpiBatV', data.map_battery_voltage);
			setKpi('kpiBatP', data.map_battery_power);
			setKpi('kpiConsP', data.map_consumption);
		}
		// Плашки «Потребление/Отдача за сегодня» (kWh): считаются из актуальных
		// показаний счётчика и фиксированных граничных точек тарифов.
		if(showMeter){
			setKpi2('kpiImpDay', data.meter_import_day);
			setKpi2('kpiImpNight', data.meter_import_night);
			setKpi2('kpiExpDay', data.meter_export_day);
			setKpi2('kpiExpNight', data.meter_export_night);
			// Плашки «Потребление/Отдача за месяц/год» (kWh).
			setKpi2('kpiImpDayM', data.meter_import_day_month);
			setKpi2('kpiImpNightM', data.meter_import_night_month);
			setKpi2('kpiExpDayM', data.meter_export_day_month);
			setKpi2('kpiExpNightM', data.meter_export_night_month);
			setKpi2('kpiImpDayY', data.meter_import_day_year);
			setKpi2('kpiImpNightY', data.meter_import_night_year);
			setKpi2('kpiExpDayY', data.meter_export_day_year);
			setKpi2('kpiExpNightY', data.meter_export_night_year);
			// Заголовки рамок: «Потребление/Отдача за MM.YYYY» и «... за YYYY год».
			var now=new Date();
			function p2(x){ return (x<10?'0':'')+x; }
			document.getElementById('tariffMonthTitle').textContent='Потребление/Отдача за '+p2(now.getMonth()+1)+'.'+now.getFullYear();
			document.getElementById('tariffYearTitle').textContent='Потребление/Отдача за '+now.getFullYear()+' год';
			// Плашка электросчётчика (вверху).
			var meter=null;
			for(var i=0;i<data.devices.length;i++) if(isMeterDevice(data.devices[i])){ meter=data.devices[i]; break; }
			renderMeter(meter);
		}
		var cards=document.getElementById('cards');
		cards.innerHTML = renderPivot(data.devices);
	}catch(e){}
}
// setKpi заполняет плашку числом (с разделителями) или прочерком, если нет данных.
function setKpi(id, v){
	var el=document.getElementById(id);
	if(!el) return;
	var n=Number(v);
	if(isFinite(n)){
		el.textContent=n.toLocaleString('ru-RU',{maximumFractionDigits:1});
	}else{
		el.textContent='—';
	}
}
// setKpi2 заполняет плашку kWh-величиной (до 2 знаков) или прочерком, если нет данных.
function setKpi2(id, v){
	var el=document.getElementById(id);
	if(!el) return;
	var n=Number(v);
	if(isFinite(n)){
		el.textContent=n.toLocaleString('ru-RU',{maximumFractionDigits:2});
	}else{
		el.textContent='—';
	}
}

// ---------- BMS (ANT батарея) ----------
// Кнопки-батарейки: заполнение = остаточный заряд (SOC), клик — страница
// деталей батареи (/bms/<name>). Обновление раз в минуту (отдельно от
// 1-секундного tick главной страницы).
function bmsSocColor(soc){ return soc<20?'#ff6b6b':(soc<50?'#ff9f43':(soc<80?'#f9ca24':'#00b894')); }
async function tickBMS(){
  if(!showBMS || (window.srRefresh && !window.srRefresh.isEnabled())) return;
  try{
    var r=await fetch('/api/bms');
    if(!r.ok) return;
    var data=await r.json();
    var list=data.bms||[];
    var el=document.getElementById('bmsList');
    if(!el) return;
    if(!list.length){ el.innerHTML='<span class="missing">BMS не найдены (или опрос отключён)</span>'; return; }
    var h='';
    for(var i=0;i<list.length;i++){
      var d=list[i];
      var soc=Math.max(0,Math.min(100,Number(d.soc)||0));
      h+='<a class="bms-btn" href="/bms/'+encodeURIComponent(d.key||d.deviceName)+'" title="Порт: '+esc(d.port)+'">'
        +'<div class="bms-batt">'
        +'<div class="bms-batt-fill" style="width:'+Math.max(4,soc)+'%;background:'+bmsSocColor(soc)+'"></div>'
        +'<span class="bms-batt-soc">'+soc+'%</span>'
        +'</div>'
        +'<div class="bms-name">'+esc(d.deviceName)+'</div>'
        +'</a>';
    }
    el.innerHTML=h;
  }catch(e){}
}
// Периодический опрос BMS управляется глобальным выключателем обновления:
// enable — немедленный опрос + интервал, disable — остановка. Стартовый вызов
// bmsStart() выполняет первый опрос сразу (обновление при загрузке включено).
var bmsTimer=null;
function bmsStart(){ if(bmsTimer || !showBMS) return; tickBMS(); bmsTimer=setInterval(tickBMS,60000); }
function bmsStop(){ if(bmsTimer){ clearInterval(bmsTimer); bmsTimer=null; } }
if(window.srRefresh){ window.srRefresh.register(bmsStart, bmsStop); }
bmsStart();

// ---------- Спойлеры и опрос данных ----------
// Спойлеры (Дом / Гараж / Детальные данные): открытость храним на клиенте
// (localStorage) и восстанавливаем при загрузке. Ключ — data-spoil элемента.
// Помимо видимости, состояние спойлера управляет опросом API: замкнутый спойлер НЕ
// опрашивает свои данные (не дёргает API); при открытии выполняется один немедленный
// опрос, далее — по собственному графику (интервалу), и только пока спойлер открыт.
// Единый менеджер на window.srSpoilers, чтобы им пользовались и dashboard.js
// (спойлер «Детальные данные» → /api/current), и animation.js (спойлеры «Дом»/«Гараж»
// → /api/animation). BMS-группа вне спойлеров, поэтому опрашивается всегда.
var srSpoilers=(function(){
  var listeners={}; // key -> [fn(open)] — независимые наблюдатели (обычно один на скрипт)
  var polls={};     // key -> {fn, interval, timer, running}
  function isOpen(key){
    var el=document.querySelector('.spoiler[data-spoil="'+key+'"]');
    return !!(el && el.classList.contains('open'));
  }
  // Применить состояние спойлера к его поллеру: открыт → немедленный опрос + интервал,
  // закрыт → остановить. При выключенном обновлении (srRefresh) опрос не запускается,
  // уже запущенный — останавливается.
  function apply(key){
    var p=polls[key];
    if(!p) return;
    var open=isOpen(key);
    var ok=open && (window.srRefresh ? window.srRefresh.isEnabled() : true);
    if(ok && !p.running){ p.running=true; p.fn(); p.timer=setInterval(p.fn,p.interval); }
    else if(!ok && p.running){ p.running=false; clearInterval(p.timer); p.timer=null; }
  }
  // Применить все поллеры (при переключении глобального выключателя обновления).
  function applyAll(){
    for(var k in polls) apply(k);
  }
  function toggle(key){
    var open=isOpen(key);
    var ls=listeners[key]||[];
    for(var i=0;i<ls.length;i++) ls[i](open);
    apply(key);
  }
  // Выключатель обновления: при включении — перезапустить все поллеры (немедленный
  // опрос открытых спойлеров), при выключении — остановить.
  if(window.srRefresh) window.srRefresh.register(applyAll, applyAll);
  return {
    isOpen:isOpen,
    // Подписать поллер на спойлер: вызывается немедленно (если спойлер уже открыт)
    // и далее по interval, только пока спойлер открыт.
    poll:function(key, fn, interval){ polls[key]={fn:fn, interval:interval, timer:null, running:false}; apply(key); },
    listen:function(key, fn){ (listeners[key]=listeners[key]||[]).push(fn); },
    toggle:toggle
  };
})();
window.srSpoilers=srSpoilers;

// Спойлер «Детальные данные» → /api/current (сводная таблица, МАП, счётчик, тарифы).
// Спойлер закрыт — опрос не выполняется; открыт — один раз сразу и далее раз в секунду.
srSpoilers.poll('details', tick, 1000);

function initSpoilers(){
  var heads=document.querySelectorAll('.spoiler-head');
  for(var i=0;i<heads.length;i++){
    (function(head){
      var spoiler=head.parentElement;
      var key=spoiler.getAttribute('data-spoil');
      if(!key) return;
      if(localStorage.getItem('sunr.spoil.'+key)==='1'){
        spoiler.classList.add('open');
        srSpoilers.toggle(key); // восстановленный открытым спойлер сразу опрашивается
      }
      head.addEventListener('click', function(){
        spoiler.classList.toggle('open');
        localStorage.setItem('sunr.spoil.'+key, spoiler.classList.contains('open')?'1':'0');
        srSpoilers.toggle(key);
      });
    })(heads[i]);
  }
}
initSpoilers();

'use strict';

// Флаги видимости блоков (из sunReceiver.json): скрытые рамки/плашки не рендерятся
// сервером, поэтому и обновления соответствующих элементов пропускаем.
var showMap = document.body.dataset.showMap === '1';
var showMeter = document.body.dataset.showMeter === '1';
var showBMS = document.body.dataset.showBMS === '1';

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
			if(signed){ cls+=' '+(n<0?' neg':' pos'); }
		}else{
			cls+=' off'; txt='—';
		}
		h+='<div class="meter-stat"><div class="lbl">'+esc(lbl)+'</div><div class="'+cls+'">'+esc(txt)+
		   (unit?' <span class="unit">'+esc(unit)+'</span>':'')+'</div></div>';
	}
	stats.innerHTML=h;
}
async function tick(){
	try{
		var r=await fetch('/api/current');
		if(!r.ok) return;
		var data=await r.json();
		// Плашка суммарной мощности (Дом)
		var kpiEl=document.getElementById('kpiTotalHome');
		var n=Number(data.total_power_home);
		if(isFinite(n) && data.total_power_home>0){
			kpiEl.textContent=n.toLocaleString('ru-RU',{maximumFractionDigits:1});
			// Число инверторов онлайн — без устройства МАП (батарея/сеть), счётчика
			// и offline-устройств (последний снимок старше STALE_MS, напр. ночью).
			var invCount=0;
			for(var i=0;i<data.devices.length;i++) if(!isMAPDeviceJS(data.devices[i]) && !isMeterDevice(data.devices[i]) && !isStaleDev(data.devices[i])) invCount++;
			function invPlural(n){ var m10=n%10, m100=n%100; if(m10===1 && m100!==11) return 'инвертор'; if(m10>=2 && m10<=4 && (m100<10||m100>=20)) return 'инвертора'; return 'инверторов'; }
			document.getElementById('kpiSub').textContent=invCount+' '+invPlural(invCount)+' онлайн';
		}else{
			kpiEl.textContent='—';
			document.getElementById('kpiSub').textContent='Нет данных';
		}
		// Плашка суммарной мощности PV (Дом)
		setKpi('kpiPVHome', data.total_pv_home);
		// Плашки «Гараж» показываем только если в таблице инверторов есть хотя бы один
		// из инверторов "Deye Left"/"Deye Right"; иначе скрываем их целиком.
		var showGarage = data.show_garage;
		var gp1=document.getElementById('kpiGaragePlate1'), gp2=document.getElementById('kpiGaragePlate2');
		if(gp1) gp1.style.display = showGarage ? '' : 'none';
		if(gp2) gp2.style.display = showGarage ? '' : 'none';
		if(showGarage){
			setKpi('kpiTotalGarage', data.total_power_garage);
			setKpi('kpiPVGarage', data.total_pv_garage);
		}
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
  if(!showBMS) return;
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
        +'<div class="bms-batt-fill" style="height:'+Math.max(4,soc)+'%;background:'+bmsSocColor(soc)+'"></div>'
        +'<span class="bms-batt-soc">'+soc+'%</span>'
        +'</div>'
        +'<div class="bms-name">'+esc(d.deviceName)+'</div>'
        +'</a>';
    }
    el.innerHTML=h;
  }catch(e){}
}
tick(); setInterval(tick,1000);
tickBMS(); setInterval(tickBMS,60000);

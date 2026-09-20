(function(){
'use strict';

// Анимация схем Дом/Гараж: связи с бегущим током и подписью мощности (1/с).
// Используется на главной странице внутри спойлеров «Дом»/«Гараж» (ранее — отдельная
// страница /animation). Обёрнуто в IIFE, чтобы не конфликтовать с dashboard.js
// (там тоже есть глобальная tick); здесь tick/loop/BUILT — локальные.
// «Внутренняя сеть» и «линия батареи» — проводники-шины без подписи: это не
// устройства, а узлы-соединители из UML (нужны только для замысла формул).
// Ширина шины определяется числом подключённых устройств, толщина — как у связи.
// Все связи — ломаные под прямыми углами со скруглениями на углах; огоньки бегут
// вдоль пути, цвет — по направлению (из сети красный, выработка/в сеть зелёный),
// мощность обновляется раз в секунду.

// ---------- Спрайты (PNG на прозрачном фоне) ----------
var SPR = {
  grid:    {file:'power-line-pylon', w:64, h:135},
  meter:   {file:'dds238-meter',     w:70, h:126},
  map:     {file:'map-converter',    w:120,h:51},
  house:   {file:'country-house',    w:120,h:86},
  garage:  {file:'garage',           w:110,h:78},
  panel:   {file:'solar-panel',      w:96, h:63},
  deye:    {file:'deye-inverter',    w:72, h:78},
  sofar:   {file:'sofar-inverter',   w:72, h:78},
  kes:     {file:'kes-dominator',    w:66, h:74},
  battery: {file:'battery-lifepo4',  w:104,h:64}
};
function sprKey(kind){ return (kind==='deye'||kind==='sofar'||kind==='kes') ? kind : (kind||'grid'); }
function sprH(key){ var s=SPR[sprKey(key)]||SPR.grid; return s.h; }

var GREEN='#37b24d', RED='#f03e3e';
var NS='http://www.w3.org/2000/svg';

// ---------- Путь с закруглениями в углах ----------
function pathWithRounds(pts, r){
  r = r===undefined?12:r;
  var d='M '+pts[0][0]+' '+pts[0][1];
  for(var i=1;i<pts.length-1;i++){
    var p=pts[i], a=pts[i-1], n=pts[i+1];
    var v1=[p[0]-a[0], p[1]-a[1]];
    var v2=[n[0]-p[0], n[1]-p[1]];
    var l1=Math.sqrt(v1[0]*v1[0]+v1[1]*v1[1])||1;
    var l2=Math.sqrt(v2[0]*v2[0]+v2[1]*v2[1])||1;
    var rr=Math.min(r, l1/2, l2/2);
    var s=[p[0]-v1[0]/l1*rr, p[1]-v1[1]/l1*rr];
    var e=[p[0]+v2[0]/l2*rr, p[1]+v2[1]/l2*rr];
    d+=' L '+s[0]+' '+s[1];
    d+=' Q '+p[0]+' '+p[1]+' '+e[0]+' '+e[1];
  }
  var last=pts[pts.length-1];
  d+=' L '+last[0]+' '+last[1];
  return d;
}

// ---------- Спрайт-узел ----------
// raise — поднять спрайт вверх на N px (точка подключения магистрали тогда
// приходится в нижнюю часть стены, а не в крышу, напр. у Дома/Гаража).
function spriteNode(svg, key, cx, cy, label, raise){
  raise=raise||0;
  var g=document.createElementNS(NS,'g');
  var spec=SPR[sprKey(key)]||SPR.grid;
  var img=document.createElementNS(NS,'image');
  img.setAttribute('href','/static/img/animation/'+spec.file+'.png?v='+CACHE_BUST);
  img.setAttribute('width',spec.w);
  img.setAttribute('height',spec.h);
  img.setAttribute('x',cx-spec.w/2);
  img.setAttribute('y',cy-raise-spec.h/2);
  img.setAttribute('class','anim-sprite');
  g.appendChild(img);
  if(key==='meter'){
    // Накладка накопленных показаний на ЖК счётчика: потребление и отдача за всё
    // время (значения обновляются tick-ом).
    var box=document.createElementNS(NS,'rect');
    box.setAttribute('x',cx-spec.w/2+spec.w*0.05);
    box.setAttribute('y',cy-raise-spec.h*0.20);
    box.setAttribute('width',spec.w*0.90);
    box.setAttribute('height',spec.h*0.42);
    box.setAttribute('rx',4); box.setAttribute('fill','#eef7f1'); box.setAttribute('class','anim-meter-box');
    g.appendChild(box);
    var d=document.createElementNS(NS,'text');
    d.setAttribute('x',cx+spec.w*0.38); d.setAttribute('y',cy-raise-spec.h*0.02); d.setAttribute('text-anchor','end');
    d.setAttribute('class','anim-meter-read'); d.textContent='Приход —';
    g.appendChild(d);
    var n=document.createElementNS(NS,'text');
    n.setAttribute('x',cx+spec.w*0.38); n.setAttribute('y',cy-raise+spec.h*0.16); n.setAttribute('text-anchor','end');
    n.setAttribute('class','anim-meter-read'); n.textContent='Отдача —';
    g.appendChild(n);
    meterReadEls.push({day:d, night:n});
  }
  if(label){
    var t=document.createElementNS(NS,'text');
    t.setAttribute('x',cx); t.setAttribute('y',cy-raise+spec.h/2+14);
    t.setAttribute('text-anchor','middle');
    t.setAttribute('class','anim-name');
    t.textContent=label;
    g.appendChild(t);
  }
  svg.appendChild(g);
}

// ---------- Шина: горизонтальный проводник без подписи ----------
function drawBus(svg, x1, x2, y){
  var l=document.createElementNS(NS,'line');
  l.setAttribute('x1',x1); l.setAttribute('y1',y);
  l.setAttribute('x2',x2); l.setAttribute('y2',y);
  l.setAttribute('class','anim-bus-wire');
  svg.appendChild(l);
}

// ---------- Связь ----------
function makeEdge(svg, opts){
  var path=document.createElementNS(NS,'path');
  path.setAttribute('d', pathWithRounds(opts.pts));
  path.setAttribute('fill','none');
  path.setAttribute('class','anim-wire');
  svg.appendChild(path);
  var total=path.getTotalLength();

  var txt=null;
  if(opts.label){
    txt=document.createElementNS(NS,'text');
    txt.setAttribute('x',opts.label.x);
    txt.setAttribute('y',opts.label.y);
    txt.setAttribute('text-anchor',opts.label.anchor||'middle');
    txt.setAttribute('class','anim-power');
    txt.textContent='—';
    svg.appendChild(txt);
  }

  var dotG=document.createElementNS(NS,'g');
  var dots=[], N=3;
  for(var i=0;i<N;i++){
    var c=document.createElementNS(NS,'circle');
    c.setAttribute('r',4);
    c.setAttribute('fill',GREEN);
    dotG.appendChild(c);
    dots.push({el:c, phase:i/N});
  }
  svg.appendChild(dotG);

  return {path:path, total:total, dots:dots, dotG:dotG, txt:txt,
    rule:opts.rule, getValue:opts.getValue, value:0, active:false, toEnd:true};
}

function updateEdge(e){
  if(e.stale){ // молчащее устройство: не показываем мощность и огоньки
    e.active=false;
    if(e.txt) e.txt.style.visibility='hidden';
    return;
  }
  if(e.txt) e.txt.style.visibility='';
  var v=e.value, rule=e.rule;
  var isGreen = (rule.greenSign>0) ? (v>0) : (v<0);
  e.toEnd = rule.greenDir==='toEnd' ? isGreen : !isGreen;
  e.active = Math.abs(v)>0.05;
  var col=isGreen?GREEN:RED;
  for(var i=0;i<e.dots.length;i++) e.dots[i].el.setAttribute('fill',col);
  if(e.txt){
    // Цвет мощности: при движении огоньков — как у них, при 0 — нейтральный.
    e.txt.style.fill = e.active ? col : '#2b3238';
    e.txt.textContent=fmtPower(v);
  }
}

function animateEdge(e, t){
  var g=e.dotG;
  if(!e.active){ g.style.display='none'; return; }
  g.style.display='';
  var speed=0.18;
  for(var i=0;i<e.dots.length;i++){
    var ph=(e.dots[i].phase + t*speed) % 1;
    var f=e.toEnd ? ph : (1-ph);
    var pt=e.path.getPointAtLength(f*e.total);
    e.dots[i].el.setAttribute('cx', pt.x);
    e.dots[i].el.setAttribute('cy', pt.y);
  }
}

// ---------- Сборка ----------
function buildScheme(container, nodes, edges){
  var svg=document.getElementById(container);
  svg.innerHTML='';
  var readings=[]; meterReadEls=readings;
  var objs=[];
  for(var i=0;i<edges.length;i++){
    if(edges[i].bus){ drawBus(svg, edges[i].x1, edges[i].x2, edges[i].y); continue; }
    objs.push(makeEdge(svg, edges[i]));
  }
  for(var j=0;j<nodes.length;j++){ var n=nodes[j]; spriteNode(svg, n.key, n.cx, n.cy, n.label, n.raise); }
  return {svg:svg, edges:objs, meterReads:readings};
}

var BUILT={house:null, garage:null};
var meterReadEls=[]; // элементы накладки показаний день/ночь на счётчике (схема Дома)

function centers(center, n, step){
  var out=[];
  if(n<=0) return out;
  var start=center - step*(n-1)/2;
  for(var i=0;i<n;i++) out.push(start+i*step);
  return out;
}

// Равномерная раскладка n точек внутри [left,right] (влезает в канву).
function spread(left, right, n){
  if(n<=0) return [];
  if(n===1) return [Math.round((left+right)/2)];
  var step=(right-left)/(n-1), out=[];
  for(var i=0;i<n;i++) out.push(left+i*step);
  return out;
}

// ---------- Схема Дома ----------
// Магистраль сверху (сеть слева → счётчик → МАП → дом справа); от МАП вниз две
// ветви: левая — «Внутренняя сеть» (шина) → инверторы → панели; правая — батарея
// (шина) → КЭС → панели.
function layoutHouse(data){
  var invs=data.inverters||[];
  var kes=data.kes||[];
  var n=invs.length, k=kes.length;

  var MAI=140, BUSY=300, INVY=420, PANY=555;
  var BATTY=230, KESY=430, KPANY=565;
  var gridX=90, meterX=260, mapX=470, houseX=790;
  // Батарея: без КЭС — левее, под Домом; с КЭС — справа (ветвь КЭС под ней).
  var battX = k>0 ? 920 : 790;
  // Порты подключения к МАП снизу: слева — ветвь инверторов, справа — батарея.
  // Разные x, чтобы линии не накладывались друг на друга.
  var mapBotY=MAI+26;             // низ спрайта МАП
  var mapPortL=mapX-30, mapPortR=mapX+30;

  // Ветвь инверторов — левый блок; ветвь КЭС — правый блок (под батареей).
  var invXs=spread(140, 830, n);
  var kesXs=centers(battX, k, 140);

  var nodes=[
    {key:'grid', cx:gridX, cy:MAI, label:'Сеть'},
    {key:'meter',cx:meterX,cy:MAI, label:'Счётчик'},
    {key:'map',  cx:mapX,  cy:MAI, label:'МАП'},
    {key:'house',cx:houseX,cy:MAI, label:'Дом', raise:26},
    {key:'battery',cx:battX,cy:BATTY, label:'Батарея'}
  ];

  var edges=[
    // Магистраль (горизонтальная, на уровне MAI).
    {pts:[[gridX,MAI],[meterX,MAI]], rule:{greenSign:-1,greenDir:'toStart'},
     label:{x:(gridX+meterX)/2, y:MAI-12},
     getValue:function(d){return d.meter_active_power;}},
    {pts:[[meterX,MAI],[mapX,MAI]], rule:{greenSign:-1,greenDir:'toStart'},
     label:{x:(meterX+mapX)/2, y:MAI-12},
     getValue:function(d){return d.map_grid_power;}},
{pts:[[mapX,MAI],[houseX,MAI]], rule:{greenSign:-1,greenDir:'toStart'},
      label:{x:(mapX+houseX)/2, y:MAI-12},
      getValue:function(d){return d.house_power;}}
  ];

  // Ветвь «Внутренняя сеть» → инверторы → панели (слева).
  if(n>0){
    var x1=invXs[0], x2=invXs[n-1];
    // МАП (левый порт) → вниз к шине («Внутренняя сеть») — прямая вертикаль, без
    // промежуточного горизонтального изгиба (иначе два близких поворота сливаются
    // в S-образную кривую). При выдаче (Σac>0) энергия идёт вверх к МАП.
    edges.push({pts:[[mapPortL,mapBotY],[mapPortL,BUSY]],
      rule:{greenSign:1,greenDir:'toStart'},
      label:{x:mapPortL-14, y:(mapBotY+BUSY)/2},
      getValue:function(d){ var s=0; for(var i=0;i<d.inverters.length;i++) s+=d.inverters[i].ac; return s; }});
    // Шина («Внутренняя сеть») — проводник без подписи, ширина по числу устройств.
    edges.push({bus:true, x1:x1, x2:x2, y:BUSY});
    for(var i=0;i<n;i++){
      var ix=invXs[i], inv=invs[i];
      nodes.push({key:inv.kind,cx:ix,cy:INVY,label:inv.name});
      nodes.push({key:'panel',cx:ix,cy:PANY,label:''});
      // инвертор → шина (вверх)
      edges.push({pts:[[ix,INVY-sprH(inv.kind)/2],[ix,BUSY]], rule:{greenSign:1,greenDir:'toEnd'},
        label:{x:ix+38,y:(INVY-39+BUSY)/2}, stale:inv.stale,
        getValue:(function(idx){return function(d){return d.inverters[idx].ac;};})(i)});
      // панель → инвертор (вверх)
      var pvTop=INVY+sprH(inv.kind)/2, pvBot=PANY-31;
      // выработка: от панели (низ) вверх к инвертору (toStart)
      edges.push({pts:[[ix,pvTop],[ix,pvBot]], rule:{greenSign:1,greenDir:'toStart'},
        label:{x:ix+38,y:(pvTop+pvBot)/2}, stale:inv.stale,
        getValue:(function(idx){return function(d){return d.inverters[idx].pv;};})(i)});
    }
  }

  // МАП (правый порт) → батарея: связь всегда (по UML map -- batt, battery_power),
  // даже если КЭС нет (иначе батарея остаётся ни с чем не связанной). Знак здесь
  // «наоборот» (→ −battery_power) и направление развёрнуто (greenDir toStart), а
  // цвет сохранён: заряд (—) красный, отдача (+) зелёный — как на дисплее МАП.
  edges.push({pts:[[mapPortR,mapBotY],[mapPortR,BATTY]], rule:{greenSign:-1,greenDir:'toStart'},
    label:{x:mapPortR-14, y:(mapBotY+BATTY)/2},
    getValue:function(d){return -d.map_battery_power;}});
  edges.push({pts:[[mapPortR,BATTY],[battX,BATTY]], rule:{greenSign:-1,greenDir:'toStart'},
    label:{x:(mapPortR+battX)/2, y:BATTY-12},
    getValue:function(d){return -d.map_battery_power;}});

  // Ветвь батарея → КЭС → панели (справа) — только при наличии КЭС.
  if(k>0){
    // батарея → КЭС (через горизонтальную шину на уровне KESY-40).
    var kx1=kesXs[0], kx2=kesXs[k-1];
    edges.push({pts:[[battX,BATTY+sprH('battery')/2],[battX,KESY-40]],
      rule:{greenSign:1,greenDir:'toEnd'},
      label:{x:battX-14, y:(BATTY+30+KESY-40)/2},
      getValue:function(d){ var s=0; for(var i=0;i<d.kes.length;i++) s+=d.kes[i].ac; return s; }});
    edges.push({bus:true, x1:kx1, x2:kx2, y:KESY-40});
    for(var j=0;j<k;j++){
      var kx=kesXs[j], kes=kes[j];
      nodes.push({key:'kes',cx:kx,cy:KESY,label:kes.name});
      nodes.push({key:'panel',cx:kx,cy:KPANY,label:''});
      edges.push({pts:[[kx,KESY-sprH('kes')/2],[kx,KESY-40]], rule:{greenSign:1,greenDir:'toEnd'},
        label:{x:kx+38,y:(KESY-37+KESY-40)/2}, stale:kes.stale,
        getValue:(function(idx){return function(d){return d.kes[idx].ac;};})(j)});
      var kPvTop=KESY+sprH('kes')/2, kPvBot=KPANY-31;
      // выработка: от панели (низ) вверх к КЭС (toStart)
      edges.push({pts:[[kx,kPvTop],[kx,kPvBot]], rule:{greenSign:1,greenDir:'toStart'},
        label:{x:kx+38,y:(kPvTop+kPvBot)/2}, stale:kes.stale,
        getValue:(function(idx){return function(d){return d.kes[idx].pv;};})(j)});
    }
  }

  return {nodes:nodes, edges:edges, height:KPANY+80};
}

// ---------- Схема Гаража ----------
// Магистраль сверху (сеть слева → «Внутренняя сеть» → гараж справа); от шины вниз
// ветвь инверторов → панели.
function layoutGarage(data){
  var invs=data.inverters||[];
  var n=invs.length;
  var MAI=150, BUSY=320, INVY=430, PANY=560;
  var gridX=110, innerX=470, garageX=810;
  var invXs=spread(140, 780, n);

  var nodes=[
    {key:'grid', cx:gridX, cy:MAI, label:'Сеть'},
    {key:'garage',cx:garageX,cy:MAI,label:'Гараж', raise:24}
  ];

  var edges=[
    // Сеть → шина (магистраль). При выдаче (Σac>0) энергия идёт в сеть (влево),
    // при потреблении — из сети (вправо).
    {pts:[[gridX,MAI],[innerX,MAI]], rule:{greenSign:1,greenDir:'toStart'},
     label:{x:(gridX+innerX)/2, y:MAI-12},
     getValue:function(d){ var s=0; for(var i=0;i<d.inverters.length;i++) s+=d.inverters[i].ac; return s; }},
    // Шина → гараж.
    {pts:[[innerX,MAI],[garageX,MAI]], rule:{greenSign:-1,greenDir:'toStart'},
     label:{x:(innerX+garageX)/2, y:MAI-12},
     getValue:function(d){return d.garage_power;}}
  ];

  if(n>0){
    var x1=invXs[0], x2=invXs[n-1];
    // Шина → вниз к горизонтальной шине инверторов — прямая вертикаль. При выдаче
    // (Σac>0) энергия идёт от инверторов вверх к магистрали (toStart).
    edges.push({pts:[[innerX,MAI],[innerX,BUSY]],
      rule:{greenSign:1,greenDir:'toStart'},
      label:{x:innerX-14, y:(MAI+BUSY)/2},
      getValue:function(d){ var s=0; for(var i=0;i<d.inverters.length;i++) s+=d.inverters[i].ac; return s; }});
    edges.push({bus:true, x1:x1, x2:x2, y:BUSY});
    for(var i=0;i<n;i++){
      var ix=invXs[i], inv=invs[i];
      nodes.push({key:inv.kind,cx:ix,cy:INVY,label:inv.name});
      nodes.push({key:'panel',cx:ix,cy:PANY,label:''});
      edges.push({pts:[[ix,INVY-sprH(inv.kind)/2],[ix,BUSY]], rule:{greenSign:1,greenDir:'toEnd'},
        label:{x:ix+38,y:(INVY-39+BUSY)/2}, stale:inv.stale,
        getValue:(function(idx){return function(d){return d.inverters[idx].ac;};})(i)});
      var pvTop=INVY+sprH(inv.kind)/2, pvBot=PANY-31;
      // выработка: от панели (низ) вверх к инвертору (toStart)
      edges.push({pts:[[ix,pvTop],[ix,pvBot]], rule:{greenSign:1,greenDir:'toStart'},
        label:{x:ix+38,y:(pvTop+pvBot)/2}, stale:inv.stale,
        getValue:(function(idx){return function(d){return d.inverters[idx].pv;};})(i)});
    }
  }

  return {nodes:nodes, edges:edges, height:PANY+80};
}

// ---------- Пересборка схемы при изменении набора устройств ----------
function ensureScheme(which, layoutData, container){
  var sig=layoutData.nodes.map(function(n){return n.label;}).join('|')+'#'+layoutData.height;
  if(BUILT[which] && BUILT[which].sig===sig) return BUILT[which].obj;
  var obj=buildScheme(container, layoutData.nodes, layoutData.edges);
  BUILT[which]={sig:sig, obj:obj};
  var svg=obj.svg;
  svg.setAttribute('viewBox','0 0 1000 '+layoutData.height);
  svg.setAttribute('preserveAspectRatio','xMidYMid meet');
  return obj;
}

function refreshEdges(obj, data){
  for(var i=0;i<obj.edges.length;i++){
    var e=obj.edges[i];
    e.value=e.getValue(data);
    updateEdge(e);
  }
  // Показания счётчика (есть только в схеме Дома: meterReads на объекте).
  var reads=obj.meterReads||[];
  for(var k=0;k<reads.length;k++){
    reads[k].day.textContent='Приход '+fmtKWh(data.meter_import_total);
    reads[k].night.textContent='Отдача '+fmtKWh(data.meter_export_total);
  }
}

function fmtPower(v){
  var sign=(v>0?'+':(v<0?'-':''));
  var a=Math.abs(v);
  if(a>=1000){
    var kw=Math.round(a/100)/10; // 1 знак после запятой, кВт
    return sign+kw+' кВт';
  }
  return (a===0?'0':sign)+Math.round(a)+' Вт';
}

function fmtKWh(v){
  v=Math.round((v||0)*10)/10;
  var neg=v<0, s=(''+Math.abs(v)).split('.'), int=s[0], dec=s[1]||'';
  var out='';
  while(int.length>3){ out=' '+int.slice(-3)+out; int=int.slice(0,-3); }
  out=int+out+(dec?'.'+dec:'');
  return (neg?'-':'')+out+' кВт·ч';
}

function fmtSec(t){
  var d=new Date(t);
  function p(x){return (x<10?'0':'')+x;}
  return d.getFullYear()+'-'+p(d.getMonth()+1)+'-'+p(d.getDate())+' '+p(d.getHours())+':'+p(d.getMinutes())+':'+p(d.getSeconds());
}

async function tick(){
  var r=await fetch('/api/animation');
  if(!r.ok) return;
  var data=await r.json();
  var ts=data.generated_at?('Актуально: '+fmtSec(data.generated_at)):'—';
  var lh=layoutHouse(data.house);
  var oh=ensureScheme('house', lh, 'svgHouse');
  refreshEdges(oh, data.house);
  var tsH=document.getElementById('animHouseTs'); if(tsH) tsH.textContent=ts;
  var lg=layoutGarage(data.garage);
  var og=ensureScheme('garage', lg, 'svgGarage');
  refreshEdges(og, data.garage);
  var tsG=document.getElementById('animGarageTs'); if(tsG) tsG.textContent=ts;
}

var animStart=performance.now();
function loop(now){
  var t=(now-animStart)/1000;
  if(BUILT.house) for(var i=0;i<BUILT.house.obj.edges.length;i++) animateEdge(BUILT.house.obj.edges[i], t);
  if(BUILT.garage) for(var j=0;j<BUILT.garage.obj.edges.length;j++) animateEdge(BUILT.garage.obj.edges[j], t);
  requestAnimationFrame(loop);
}
requestAnimationFrame(loop);

tick(); setInterval(tick, 1000);

})();
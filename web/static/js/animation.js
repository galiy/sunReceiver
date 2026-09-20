'use strict';

// ---------- Спрайты (PNG на прозрачном фоне, скопированы из animation/) ----------
// Ключ — тип узла; размеры подобраны под макет (сохраняют пропорции исходника).
var SPR = {
  grid:    {file:'power-line-pylon', w:64, h:135},
  meter:   {file:'dds238-meter',     w:52, h:116},
  map:     {file:'map-converter',    w:120,h:51},
  house:   {file:'country-house',    w:120,h:86},
  garage:  {file:'garage',           w:110,h:78},
  panel:   {file:'solar-panel',      w:96, h:63},
  deye:    {file:'deye-inverter',    w:72, h:78},
  sofar:   {file:'sofar-inverter',   w:72, h:78},
  kes:     {file:'kes-dominator',    w:66, h:74},
  battery: {file:'battery-lifepo4',  w:104,h:64}
};
function sprKey(kind){ return (kind==='deye'||kind==='sofar') ? kind : (kind==='kes'?'kes':kind) || 'grid'; }

// ---------- Цвета огоньков ----------
var GREEN = '#37b24d';
var RED   = '#f03e3e';
var WIRE  = '#3a4752';

// ---------- Утилиты ----------
function createSvg(container){
  var svg = document.getElementById(container);
  svg.setAttribute('width','100%');
  return svg;
}
function imageNode(svg, key, cx, cy, label){
  var g = document.createElementNS('http://www.w3.org/2000/svg','g');
  if(key==='bus'){
    // «Внутренняя сеть» — условный узел-соединитель (нет спрайта): светлый
    // прямоугольник с меткой.
    var r=document.createElementNS('http://www.w3.org/2000/svg','rect');
    r.setAttribute('x',cx-52); r.setAttribute('y',cy-26);
    r.setAttribute('width',104); r.setAttribute('height',52);
    r.setAttribute('rx',8);
    r.setAttribute('class','anim-bus');
    g.appendChild(r);
  } else {
    var spec = SPR[sprKey(key)] || SPR.grid;
    var img = document.createElementNS('http://www.w3.org/2000/svg','image');
    img.setAttribute('href','/static/img/animation/'+spec.file+'.png?v='+CACHE_BUST);
    img.setAttribute('width',spec.w);
    img.setAttribute('height',spec.h);
    img.setAttribute('x',cx-spec.w/2);
    img.setAttribute('y',cy-spec.h);
    img.setAttribute('class','anim-sprite');
    g.appendChild(img);
  }
  if(label){
    var t=document.createElementNS('http://www.w3.org/2000/svg','text');
    t.setAttribute('x',cx);
    t.setAttribute('y',cy+16);
    t.setAttribute('text-anchor','middle');
    t.setAttribute('class','anim-name');
    t.textContent=label;
    g.appendChild(t);
  }
  svg.appendChild(g);
  return g;
}

// ---------- Связь (линия + бегущие огоньки + подпись мощности) ----------
function makeEdge(svg, ax, ay, bx, by, direction){
  // direction: {greenSign:+1|-1, greenDir:'toA'|'toB'} — правило цвета/направления.
  var line = document.createElementNS('http://www.w3.org/2000/svg','line');
  line.setAttribute('x1',ax); line.setAttribute('y1',ay);
  line.setAttribute('x2',bx); line.setAttribute('y2',by);
  line.setAttribute('class','anim-wire');
  svg.appendChild(line);

  // Подпись мощности — в середине линии, перпендикулярный сдвиг.
  var mx=(ax+bx)/2, my=(ay+by)/2;
  var dx=bx-ax, dy=by-ay;
  var len=Math.sqrt(dx*dx+dy*dy)||1;
  // Перпендикуляр (нормированный).
  var px=-dy/len, py=dx/len;
  var off=18; // сдвиг подписи в сторону от линии
  var lx=mx+px*off, ly=my+py*off;
  var txt=document.createElementNS('http://www.w3.org/2000/svg','text');
  txt.setAttribute('x',lx); txt.setAttribute('y',ly-4);
  txt.setAttribute('text-anchor','middle');
  txt.setAttribute('class','anim-power');
  txt.textContent='—';
  svg.appendChild(txt);

  // Группа огоньков: несколько точек, бегущих по линии.
  var dots=[];
  var N=3;
  var dotG=document.createElementNS('http://www.w3.org/2000/svg','g');
  for(var i=0;i<N;i++){
    var c=document.createElementNS('http://www.w3.org/2000/svg','circle');
    c.setAttribute('r',4);
    c.setAttribute('fill',GREEN);
    dotG.appendChild(c);
    dots.push({el:c, phase:i/N});
  }
  svg.appendChild(dotG);

  return {
    ax:ax, ay:ay, bx:bx, by:by, direction:direction,
    txt:txt, dots:dots, dotG:dotG,
    value:0, active:false
  };
}

// Обновляет геометрию/цвет/активность огоньков по текущему значению.
function updateEdge(e){
  var v=e.value;
  var dir=e.direction;
  var isGreen;
  if(dir.greenSign>0){ isGreen = v>0; }
  else { isGreen = v<0; }
  // Направление движения огоньков: к зелёному концу (генерация) или обратно.
  var toGreen = (dir.greenDir==='toA');
  var flowsToGreen = isGreen ? toGreen : !toGreen;
  // Физический поток направлен «к зелёному» когда зелёный, иначе «от зелёного».
  // flowsToGreen — огоньки бегут в сторону greenDir; при красном — против.
  e.flowToGreen = flowsToGreen;
  e.active = Math.abs(v) > 0.05;
  var col = isGreen ? GREEN : RED;
  for(var i=0;i<e.dots.length;i++){ e.dots[i].el.setAttribute('fill',col); }
  // Подпись: значение со знаком, W.
  e.txt.textContent = (Math.round(v*10)/10 === 0 ? '0' : (v>0?'+':'')+(Math.round(v*10)/10)) + ' W';
}

// Кадр анимации: двигает огоньки по сегменту в зависимости от направления.
function animateEdge(e, t){
  var g=e.dotG;
  if(!e.active){ g.style.display='none'; return; }
  g.style.display='';
  var toGreen;
  if(e.flowToGreen) toGreen=true; else toGreen=false;
  var ax=e.ax, ay=e.ay, bx=e.bx, by=e.by;
  var sx, sy, ex, ey;
  if(toGreen){ sx=ax; sy=ay; ex=bx; ey=by; } else { sx=bx; sy=by; ex=ax; ey=ay; }
  var speed=0.35; // доля пути в секунду
  for(var i=0;i<e.dots.length;i++){
    var ph=(e.dots[i].phase + t*speed) % 1;
    var x=sx+(ex-sx)*ph, y=sy+(ey-sy)*ph;
    var c=e.dots[i].el;
    c.setAttribute('cx',x); c.setAttribute('cy',y);
  }
}

// ---------- Построение схемы ----------
// nodes: [{key,cx,cy,label}], edges: [{a:{x,y},b:{x,y},rule,getValue}]
function buildScheme(container, nodes, edges, getValueFor){
  var svg = createSvg(container);
  svg.innerHTML='';
  var edgeObjs=[];
  for(var i=0;i<edges.length;i++){
    var ed=edges[i];
    var obj=makeEdge(svg, ed.a.x, ed.a.y, ed.b.x, ed.b.y, ed.rule);
    obj.getValue=ed.getValue;
    edgeObjs.push(obj);
  }
  // Узлы поверх проводов.
  for(var j=0;j<nodes.length;j++){
    var n=nodes[j];
    imageNode(svg, n.key, n.cx, n.cy, n.label);
  }
  return {svg:svg, edges:edgeObjs};
}

// ---------- Данные ----------
var LAST={ house:{ts:'—', data:null, sig:''}, garage:{ts:'—', data:null, sig:''} };
var BUILT={ house:null, garage:null };

// Серия из n центров, симметрично вокруг center, шагом step.
function centersAround(center, n, step){
  var out=[];
  if(n<=0) return out;
  var start=center - step*(n-1)/2;
  for(var i=0;i<n;i++) out.push(start + i*step);
  return out;
}

// Настраивает layout Дома (пересборка только при смене числа устройств).
function layoutHouse(data){
  var invs=data.inverters||[];
  var kes=data.kes||[];
  var n=invs.length, k=kes.length;

  var row1y=130, row2y=360, row3y=560, row4y=720;
  var grid={x:120,y:row1y};
  var meter={x:360,y:row1y};
  var map={x:620,y:row1y};
  var house={x:920,y:row1y};
  var inner={x:400,y:row2y};
  var battery={x:790,y:row2y};

  var invXs=centersAround(inner.x, n, 170);
  var kesXs=centersAround(battery.x, k, 150);

  var nodes=[
    {key:'grid',cx:grid.x,cy:grid.y,label:'Сеть'},
    {key:'meter',cx:meter.x,cy:meter.y,label:'Счётчик'},
    {key:'map',cx:map.x,cy:map.y,label:'МАП'},
    {key:'house',cx:house.x,cy:house.y,label:'Дом'},
    {key:'bus',cx:inner.x,cy:inner.y,label:'Внутренняя сеть'},
    {key:'battery',cx:battery.x,cy:battery.y,label:'Батарея'}
  ];
  var edges=[
    {a:{x:grid.x,y:row1y}, b:{x:meter.x,y:row1y}, rule:{greenSign:-1,greenDir:'toA'}, getValue:function(d){return d.meter_active_power;}},
    {a:{x:meter.x,y:row1y}, b:{x:map.x,y:row1y}, rule:{greenSign:-1,greenDir:'toA'}, getValue:function(d){return d.map_grid_power;}},
    {a:{x:map.x,y:row1y}, b:{x:house.x,y:row1y}, rule:{greenSign:-1,greenDir:'toA'}, getValue:function(d){return d.house_power;}},
    {a:{x:map.x,y:row1y}, b:{x:inner.x,y:row2y}, rule:{greenSign:1,greenDir:'toA'}, getValue:function(d){
        var s=0; for(var i=0;i<d.inverters.length;i++) s+=d.inverters[i].ac; return s;}},
    {a:{x:map.x,y:row1y}, b:{x:battery.x,y:row2y}, rule:{greenSign:1,greenDir:'toA'}, getValue:function(d){return d.map_battery_power;}}
  ];
  for(var i=0;i<n;i++){
    var ix=invXs[i], inv=invs[i];
    nodes.push({key:inv.kind,cx:ix,cy:row3y,label:inv.name});
    nodes.push({key:'panel',cx:ix,cy:row4y,label:''});
    // панель → инвертор
    edges.push({a:{x:ix,y:row4y}, b:{x:ix,y:row3y}, rule:{greenSign:1,greenDir:'toB'}, getValue:(function(idx){return function(d){return d.inverters[idx].pv;};})(i)});
    // инвертор → внутр. сеть (ac>0 выдача → к сети, b)
    edges.push({a:{x:ix,y:row3y}, b:{x:inner.x,y:row2y}, rule:{greenSign:1,greenDir:'toB'}, getValue:(function(idx){return function(d){return d.inverters[idx].ac;};})(i)});
  }
  for(var j=0;j<k;j++){
    var kx=kesXs[j], kes=kes[j];
    nodes.push({key:'kes',cx:kx,cy:row3y,label:kes.name});
    nodes.push({key:'panel',cx:kx,cy:row4y,label:''});
    edges.push({a:{x:kx,y:row4y}, b:{x:kx,y:row3y}, rule:{greenSign:1,greenDir:'toB'}, getValue:(function(idx){return function(d){return d.kes[idx].pv;};})(j)});
    edges.push({a:{x:kx,y:row3y}, b:{x:battery.x,y:row2y}, rule:{greenSign:1,greenDir:'toB'}, getValue:(function(idx){return function(d){return d.kes[idx].ac;};})(j)});
  }
  // высота канвы
  var h = row4y + 90;
  return {nodes:nodes, edges:edges, height:h};
}

function layoutGarage(data){
  var invs=data.inverters||[];
  var n=invs.length;
  var row1y=140, row2y=380, row3y=540;
  var grid={x:160,y:row1y};
  var inner={x:500,y:row1y};
  var garage={x:850,y:row1y};
  var invXs=centersAround(inner.x, n, 170);
  var nodes=[
    {key:'grid',cx:grid.x,cy:grid.y,label:'Сеть'},
    {key:'bus',cx:inner.x,cy:inner.y,label:'Внутренняя сеть'},
    {key:'garage',cx:garage.x,cy:garage.y,label:'Гараж'}
  ];
  var edges=[
    {a:{x:grid.x,y:row1y}, b:{x:inner.x,y:row1y}, rule:{greenSign:1,greenDir:'toA'}, getValue:function(d){
        var s=0; for(var i=0;i<d.inverters.length;i++) s+=d.inverters[i].ac; return s;}},
    {a:{x:inner.x,y:row1y}, b:{x:garage.x,y:row1y}, rule:{greenSign:-1,greenDir:'toA'}, getValue:function(d){return d.garage_power;}}
  ];
  for(var i=0;i<n;i++){
    var ix=invXs[i], inv=invs[i];
    nodes.push({key:inv.kind,cx:ix,cy:row2y,label:inv.name});
    nodes.push({key:'panel',cx:ix,cy:row3y,label:''});
    edges.push({a:{x:ix,y:row3y}, b:{x:ix,y:row2y}, rule:{greenSign:1,greenDir:'toB'}, getValue:(function(idx){return function(d){return d.inverters[idx].pv;};})(i)});
    edges.push({a:{x:ix,y:row2y}, b:{x:inner.x,y:row1y}, rule:{greenSign:1,greenDir:'toA'}, getValue:(function(idx){return function(d){return d.inverters[idx].ac;};})(i)});
  }
  return {nodes:nodes, edges:edges, height:row3y+90};
}

// Собирает/пересобирает схему только при изменении набора устройств.
function ensureScheme(which, layoutData, container){
  var sig = layoutData.nodes.map(function(n){return n.label;}).join('|');
  var built = BUILT[which];
  if(built && built.sig===sig && layoutData.height===built.height){
    return built.obj;
  }
  var obj = buildScheme(container, layoutData.nodes, layoutData.edges, null);
  BUILT[which]={sig:sig, height:layoutData.height, obj:obj};
  var svg=obj.svg;
  svg.setAttribute('viewBox','0 0 1040 '+layoutData.height);
  svg.setAttribute('preserveAspectRatio','xMidYMid meet');
  return obj;
}

// Обновляет значения связей из свежих данных.
function refreshEdges(obj, data){
  for(var i=0;i<obj.edges.length;i++){
    var e=obj.edges[i];
    var v=e.getValue(data);
    e.value = v;
    updateEdge(e);
  }
}

async function tick(){
  var r=await fetch('/api/animation');
  if(!r.ok) return;
  var data=await r.json();
  var ts=data.generated_at? ('Актуально: '+fmtSec(data.generated_at)):'—';
  // Дом
  var lh=layoutHouse(data.house);
  var oh=ensureScheme('house', lh, 'svgHouse');
  refreshEdges(oh, data.house);
  document.getElementById('animHouseTs').textContent=ts;
  // Гараж
  var lg=layoutGarage(data.garage);
  var og=ensureScheme('garage', lg, 'svgGarage');
  refreshEdges(og, data.garage);
  document.getElementById('animGarageTs').textContent=ts;
}

function fmtSec(t){
  var d=new Date(t);
  function p(x){return (x<10?'0':'')+x;}
  return d.getFullYear()+'-'+p(d.getMonth()+1)+'-'+p(d.getDate())+' '+p(d.getHours())+':'+p(d.getMinutes())+':'+p(d.getSeconds());
}

// Цикл анимации: общая rAF-петля двигает огоньки по всем связям.
var animStart = performance.now();
function loop(now){
  var t=(now-animStart)/1000;
  if(BUILT.house) for(var i=0;i<BUILT.house.obj.edges.length;i++) animateEdge(BUILT.house.obj.edges[i], t);
  if(BUILT.garage) for(var j=0;j<BUILT.garage.obj.edges.length;j++) animateEdge(BUILT.garage.obj.edges[j], t);
  requestAnimationFrame(loop);
}
requestAnimationFrame(loop);

tick(); setInterval(tick, 1000);
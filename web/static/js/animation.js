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
  meter:   {file:'dds238-meter',     w:150,h:126}, // растянут в ширину (PNG 756×800)
  ce308:   {file:'ce308-meter',      w:64, h:76},
  map:     {file:'map-converter',    w:120,h:51},
  house:   {file:'country-house',    w:120,h:86},
  garage:  {file:'garage',           w:110,h:78},
  panel:   {file:'solar-panel',      w:96, h:63},
  deye:    {file:'deye-inverter',    w:72, h:78},
  sofar:   {file:'sofar-inverter',   w:72, h:78},
  kes:     {file:'kes-dominator',    w:66, h:74},
  // visX0/visX1 — доля ширины PNG, занятая видимыми пикселями (по alpha-каналу).
  // У батареи спрайт содержит широкое прозрачное поле справа (контент только
  // x=37..432 из 620), поэтому метку температуры нужно ставить к видимому краю,
  // а не к краю бокса.
  battery: {file:'battery-lifepo4',  w:104,h:64, visX0:0.060, visX1:0.697}
};
function sprKey(kind){ return (kind==='deye'||kind==='sofar'||kind==='kes') ? kind : (kind||'grid'); }
function sprH(key){ var s=SPR[sprKey(key)]||SPR.grid; return s.h; }

var GREEN='#37b24d', RED='#f03e3e';
var NS='http://www.w3.org/2000/svg';

// Количество и скорость огоньков зависят от мощности: чем больше мощность, тем
// их больше (вплотную) и тем быстрее они бегут. Диапазон: 100 Вт — минимум
// (редкие огоньки, еле ползут), 20 кВт — максимум (вплотную, очень быстро).
// Шаг между огоньками фиксирован при данной мощности и НЕ зависит от длины
// линии: на более длинной линии просто больше огоньков.
var P_MIN=100, P_MAX=20000;     // Вт
var SP_MIN=9,  SP_MAX=64;       // px между огоньками: 9 (20 кВт, вплотную) .. 64 (100 Вт, редко)
var DOTS_MIN=2;
var MAX_DOTS=60;                // пул кружков на одну связь (запас для длинных линий)
function pwLerp(v){ // 0..1 по мощности, |v|<=100 → 0, |v|>=20000 → 1
  var t=(Math.abs(v)-P_MIN)/(P_MAX-P_MIN);
  return t<0?0:(t>1?1:t);
}
function speedFor(t){ // px/с: 6 (100 Вт) .. 260 (20 кВт) — скорость уменьшена в 2 раза
  return 6 + (260-6)*t;
}
function spacingFor(t){ // px между огоньками (одинаково для любой длины линии)
  return SP_MAX + (SP_MIN-SP_MAX)*t;
}

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
// Счётчик (DDS238) использует красивый PNG-корпус (монтажные планки, винты,
// марка DDS238), растянутый по ширине; дисплей-накладка с показаниями ложится
// на нижнюю часть белого корпуса и по ширине равна самому корпусу, поэтому
// никогда не вылезает за него.
// Доля белого корпуса в спрайте dds238-meter (измерена по пикселям 756×800
// после растяжения PNG в 1.5 раза): корпус — 0.80 ширины, по центру.
// Дисплей-накладка — 70% от прежней ширины (152px → 106px) при новой ширине
// спрайта 150px: доля 0.71.
// Масштаб схемы: 1 см ≈ 16px в viewBox.
var CM=16;
var METER={bodyXFrac:0.80, dispXFrac:0.582};
function spriteNode(svg, key, cx, cy, label, raise){
  raise=raise||0;
  // Узел-«сеть» (Сеть дома / Сеть гаража): кружок с той же толщиной контура,
  // что у дорожек тока; подпись под кружком. Это узел-соединитель, не устройство.
  if(key==='net'){
    var ng=document.createElementNS(NS,'g');
    var nc=document.createElementNS(NS,'circle');
    nc.setAttribute('cx',cx); nc.setAttribute('cy',cy); nc.setAttribute('r',15);
    nc.setAttribute('class','anim-net-node');
    ng.appendChild(nc);
    if(label){
      var nt=document.createElementNS(NS,'text');
      nt.setAttribute('x',cx); nt.setAttribute('y',cy+15+14);
      nt.setAttribute('text-anchor','middle');
      nt.setAttribute('class','anim-name');
      nt.textContent=label;
      ng.appendChild(nt);
    }
    svg.appendChild(ng);
    return {img:nc, meterReads:null};
  }
  var g=document.createElementNS(NS,'g');
  var spec=SPR[sprKey(key)]||SPR.grid;
  var wScale=spec.wScale||1, effW=spec.w*wScale; // эффективная ширина (растяжение в ширину)
  var img=document.createElementNS(NS,'image');
  img.setAttribute('href','/static/img/animation/'+spec.file+'.png?v='+CACHE_BUST);
  img.setAttribute('width',effW);
  img.setAttribute('height',spec.h);
  img.setAttribute('x',cx-effW/2);
  img.setAttribute('y',cy-raise-spec.h/2);
  img.setAttribute('class','anim-sprite');
  g.appendChild(img);
  var meterReads=null; // ссылки на текстовые узлы дисплея счётчика (возврат наружу)
  if(key==='meter'){
    // Накладка показаний на белый корпус (под маркой DDS238). Ширина — 70% от
    // прежней ширины накладки (dispXFrac), выровнена по центру cx.
    var dispW=effW*METER.dispXFrac;
    var top=cy-raise+spec.h*0.06-2*CM+0.2*CM, lh=spec.h*0.30-0.3*CM; // верх и высота накладки (+2 мм вниз, −3 мм высота)
    var box=document.createElementNS(NS,'rect');
    box.setAttribute('x',cx-dispW/2); box.setAttribute('y',top);
    box.setAttribute('width',dispW); box.setAttribute('height',lh);
    box.setAttribute('rx',5); box.setAttribute('fill','#eef7f1');
    box.setAttribute('class','anim-meter-box');
    g.appendChild(box);
    var lx=cx+dispW/2-8;
    var d=document.createElementNS(NS,'text');
    d.setAttribute('x',lx); d.setAttribute('y',top+lh*0.44); d.setAttribute('text-anchor','end');
    d.setAttribute('class','anim-meter-read anim-meter-import'); d.textContent='—';
    g.appendChild(d);
    var n=document.createElementNS(NS,'text');
    n.setAttribute('x',lx); n.setAttribute('y',top+lh*0.82); n.setAttribute('text-anchor','end');
    n.setAttribute('class','anim-meter-read anim-meter-export'); n.textContent='—';
    g.appendChild(n);
    meterReads=[{day:d, night:n}];
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
  return {img:img, meterReads:meterReads};
}

// ---------- Шина: широкая медная полоса с болтами в точках присоединения ----------
// x1/x2 — крайние x полосы, bolts — массив x-координат мест присоединения (болты).
function drawBus(svg, x1, x2, y, bolts){
  var grp=document.createElementNS(NS,'g');
  // Полоса шины (медь), чуть шире обычного провода.
  var bar=document.createElementNS(NS,'rect');
  var bh=9;
  bar.setAttribute('x',x1); bar.setAttribute('y',y-bh/2);
  bar.setAttribute('width',x2-x1); bar.setAttribute('height',bh);
  bar.setAttribute('rx',2);
  bar.setAttribute('class','anim-bus');
  grp.appendChild(bar);
  // Зажим-болты в местах присоединения (по одному на каждый отвод).
  bolts=bolts||[];
  for(var i=0;i<bolts.length;i++){
    var b=document.createElementNS(NS,'circle');
    b.setAttribute('cx',bolts[i]); b.setAttribute('cy',y);
    b.setAttribute('r',6);
    b.setAttribute('class','anim-bus-bolt');
    grp.appendChild(b);
  }
  svg.appendChild(grp);
}

// ---------- Связь (rail) ----------
// Rail — «полая» линия: две тонкие параллельные кромки вокруг гладкой оси.
// Кромки строятся сэмплированием оси (trace) и смещением каждой точки по нормали
// на ±gap: так они повторяют скругления углов (концентричны) и остаются строго
// параллельными, а на стыках/пересечениях просто накладываются целыми линиями
// (без «вырезания» белым штрихом, как при двойном штрихе). Огоньки бегут по оси.
var RAIL_GAP=6;   // расстояние от оси до кромки
var RAIL_W=1.5;   // толщина кромки
function makeEdge(svg, opts, skipRail){
  // Геометрическая ось — невидимая, по ней считаем длину и ведём огоньки.
  var trace=document.createElementNS(NS,'path');
  trace.setAttribute('d', pathWithRounds(opts.pts));
  trace.setAttribute('fill','none');
  trace.setAttribute('class','anim-wire-trace');
  svg.appendChild(trace);
  var total=trace.getTotalLength();
  // Кромки рисуются НЕ здесь, а единой SVG-маской в buildScheme (skipRail=true),
  // чтобы на Т-стыках все связи объединялись в один силуэт труб без пересечений.

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
  var dots=[], MAX=MAX_DOTS;
  for(var i=0;i<MAX;i++){
    var c=document.createElementNS(NS,'circle');
    c.setAttribute('r',4);
    c.setAttribute('fill',GREEN);
    dotG.appendChild(c);
    dots.push({el:c});
  }
  svg.appendChild(dotG);

  return {path:trace, trace:trace, total:total, dots:dots, dotG:dotG, txt:txt,
    rule:opts.rule, getValue:opts.getValue, value:0, active:false, toEnd:true,
    pos:0,
    spacing:SP_MAX, speed:0, tSpacing:SP_MAX, tSpeed:12};
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
  // labelSign: дополнительный множитель ТОЛЬКО для подписи значения (цвет/направление/
  // активность считаются по сырому value). Для веток выработки (панelи→инверторы/КЭС→МАП)
  // он равен −1: правило «потребление +, выработка/отдача −» при сохранении верных
  // цвета (зелёный=выработка/в сеть) и направления потока.
  var ls=(rule.labelSign||1);
  e.toEnd = rule.greenDir==='toEnd' ? isGreen : !isGreen;
  e.active = Math.abs(v)>0.5; // ниже 0.5 Вт — связь неактивна (не рисуем «+0 Вт» с огоньками)
  var t=pwLerp(v);                 // 0 (100 Вт) .. 1 (20 кВт)
  e.tSpacing=spacingFor(t);        // цель для плавного перехода (px, не зависит от длины)
  e.tSpeed=speedFor(t);            // px/с
  var col=isGreen?GREEN:RED;
  for(var i=0;i<e.dots.length;i++) e.dots[i].el.setAttribute('fill',col);
  if(e.txt){
    // При нулевой мощности подпись гасим полностью.
    if(v===0){ e.txt.style.visibility='hidden'; return; }
    e.txt.style.visibility='';
    // Цвет мощности: при движении огоньков — как у них, при 0 — нейтральный.
    e.txt.style.fill = e.active ? col : '#2b3238';
    e.txt.textContent=fmtPower(v*ls);
  }
}

function animateEdge(e, dt){
  var g=e.dotG;
  if(!e.active){ g.style.display='none'; return; }
  g.style.display='';
  var total=e.total;
  if(!(total>0) || !isFinite(total)){ g.style.display='none'; return; } // защита от NaN-геометрии
  // Ограничиваем dt (после фона/троттлинга вкладки) и защищаемся от не-конечных значений.
  if(!isFinite(dt) || dt<0) dt=0; else if(dt>0.1) dt=0.1;
  // Плавный переход к целевым значениям (показательная аппроксимация к цели).
  // k — в 1/с: постоянная времени не зависит от частоты кадров (при 60 Гц даёт
  // ту же скорость сглаживания, что и прежний шаг 0.08/кадр).
  var k=5;
  var a=1-Math.exp(-dt*k);
  var ts=(isFinite(e.tSpacing) && e.tSpacing>0)?e.tSpacing:SP_MAX;
  var tsd=(isFinite(e.tSpeed) && e.tSpeed>=0)?e.tSpeed:0;
  var sp=(isFinite(e.spacing) && e.spacing>0)?e.spacing:ts;
  var sd=isFinite(e.speed)?e.speed:tsd;
  sp += (ts-sp)*a;
  sd += (tsd-sd)*a;
  e.spacing=sp; e.speed=sd;
  // Позицию храним в диапазоне [0,total), чтобы не росла бесконечно (теряется точность).
  e.pos=((e.pos + sd*dt*(e.toEnd?1:-1))%total+total)%total;
  // Число огоньков следует из текущего шага и длины линии: при фиксированном
  // шаге более длинная линия даёт больше огоньков (меняются по одному, без рывка).
  var n=Math.max(DOTS_MIN, Math.min(MAX_DOTS, Math.round(total/sp)));
  // Распределяем огоньки РАВНОМЕРНО по кольцу шагом total/n (а не i*sp): иначе
  // из-за округления n*sp != total и последний огонёк наматывается вплотную к
  // первому, образуя слипшуюся пару. Шаг total/n ≈ sp, что сохраняет и плотность
  // (зависит от мощности), и «больше огоньков на длинной линии».
  var step=n>0 ? total/n : sp;
  for(var i=0;i<e.dots.length;i++){
    var el=e.dots[i].el;
    if(i>=n){ el.style.display='none'; continue; }
    el.style.display='';
    var s=((e.pos + i*step)%total+total)%total;
    if(!isFinite(s)) continue;
    var pt=e.path.getPointAtLength(s);
    el.setAttribute('cx', pt.x);
    el.setAttribute('cy', pt.y);
  }
}

// ---------- Сборка ----------
// Все связи рисуются ЕДИНЫМ «силуэтом труб» через SVG-маску. Маска строится так:
//  1) БЕЛЫМ рисуются наружные контуры всех труб (широкий штрих по каждой оси);
//  2) поверх ЧЁРНЫМ — внутренняя полость каждой трубы (узкий штрих по той же оси).
// В итоге тёмным остаются только кромки шириной RAIL_W, причём на стыках/поворотах
// все трубы объединяются в единый силуэт: ветвь корректно примыкает к магистрали,
// а границы магистрали прерываются ровно там, где входит ветвь. Огоньки и подписи
// рисуются поверх (поверх маскируемого тёмного слоя) в makeEdge.
function buildScheme(container, nodes, edges, height, width){
  var svg=document.getElementById(container);
  svg.innerHTML='';
  var readings=[];
  var sprites=[]; // спрайты со «stale» из данных: затемняются в refreshEdges
  var tempUpds=[]; // обновители температур (renderNodeTemps)
  var objs=[];

  // --- Сбор осей всех связей (не шины) ---
  var railD=[];
  for(var i=0;i<edges.length;i++){
    if(edges[i].bus) continue;
    railD.push(pathWithRounds(edges[i].pts));
  }

  // --- Маска: наружный белый контур + чёрная внутренняя полость ---
  var maskId=container+'_rail';
  var mask=document.createElementNS(NS,'mask');
  mask.setAttribute('id', maskId);
  mask.setAttribute('maskUnits','userSpaceOnUse');
  mask.setAttribute('x','0'); mask.setAttribute('y','0');
  mask.setAttribute('width', String(width)); mask.setAttribute('height', String(height));
  var outer=document.createElementNS(NS,'g');
  outer.setAttribute('fill','none'); outer.setAttribute('stroke','#ffffff');
  outer.setAttribute('stroke-width', 2*RAIL_GAP+RAIL_W);
  outer.setAttribute('stroke-linejoin','round'); outer.setAttribute('stroke-linecap','butt');
  var inner=document.createElementNS(NS,'g');
  inner.setAttribute('fill','none'); inner.setAttribute('stroke','#000000');
  inner.setAttribute('stroke-width', 2*RAIL_GAP-RAIL_W);
  inner.setAttribute('stroke-linejoin','round'); inner.setAttribute('stroke-linecap','butt');
  for(var rd=0;rd<railD.length;rd++){
    var p1=document.createElementNS(NS,'path'); p1.setAttribute('d', railD[rd]); outer.appendChild(p1);
    var p2=document.createElementNS(NS,'path'); p2.setAttribute('d', railD[rd]); inner.appendChild(p2);
  }
  mask.appendChild(outer); mask.appendChild(inner);
  var defs=document.createElementNS(NS,'defs'); defs.appendChild(mask);
  svg.appendChild(defs);

  // --- Тёмный слой труб, ограниченный маской (видно только кромки) ---
  var railLayer=document.createElementNS(NS,'rect');
  railLayer.setAttribute('x','0'); railLayer.setAttribute('y','0');
  railLayer.setAttribute('width', String(width)); railLayer.setAttribute('height', String(height));
  railLayer.setAttribute('fill','#3a4752');
  railLayer.setAttribute('mask','url(#'+maskId+')');
  svg.appendChild(railLayer);
  // Слой подписей мощности, чтобы был ПОД связями на стыках? Оставим поверх в makeEdge.

  // --- Связи: оси/огоньки/подписи поверх слоя труб ---
  for(var e=0;e<edges.length;e++){
    if(edges[e].bus){ drawBus(svg, edges[e].x1, edges[e].x2, edges[e].y, edges[e].bolts); continue; }
    objs.push(makeEdge(svg, edges[e], true));
  }

  // --- Узлы (спрайты) поверх всего ---
  for(var j=0;j<nodes.length;j++){
    var n=nodes[j];
    var spr=spriteNode(svg, n.key, n.cx, n.cy, n.label, n.raise);
    if(spr.meterReads) readings.push(spr.meterReads[0]);
    if(n.staleOf) sprites.push({img:spr.img, staleOf:n.staleOf});
    if(n.temps && n.temps.length) tempUpds=tempUpds.concat(renderNodeTemps(svg, n));
  }
  return {svg:svg, edges:objs, meterReads:readings, sprites:sprites, tempUpds:tempUpds};
}

var BUILT={house:null, garage:null};

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

// inverterTempsSpec возвращает temps-спеку (Корпус/Транзисторы) для инвертора
// с индексом idx в массиве d.inverters: значения читаются из data.inverters[idx].temps.
// stale — предикат «инвертор молчит»: при нём температуры гасятся (не показываем).
function inverterTempsSpec(idx){
  var stale=function(d){ return !!(d.inverters[idx]&&d.inverters[idx].stale); };
  return [
    {label:'Корпус', get:function(d){ return tempValue((d.inverters[idx]&&d.inverters[idx].temps)||[], 'Корпус'); }, stale:stale},
    {label:'Транзисторы', get:function(d){ return tempValue((d.inverters[idx]&&d.inverters[idx].temps)||[], 'Транзисторы'); }, stale:stale}
  ];
}

// ---------- Схема Дома ----------
// Магистраль сверху (сеть слева → счётчик → МАП → дом справа); от МАП вниз две
// ветви: левая — «Внутренняя сеть» (шина) → инверторы → панели; правая — батарея
// (шина) → КЭС → панели.
function layoutHouse(data){
  var invs=data.inverters||[];
  var kes=data.kes||[];
  var n=invs.length, k=kes.length;

  var MAI=140, BUSY=310, INVY=430, PANY=560;
  var BATTY=230, KESY=450, KPANY=580;
  var gridX=80, meterX=270, mapX=500, nodeX=700, houseX=880;
  // Порт МАП вниз-влево — ветвь батареи; батарея/КЭС слева, инверторы — справа
  // (под узлом «Сеть дома»), чтобы труба узла не пересекала батарейную ветку.
  var mapBotY=MAI+26;
  var mapPortL=mapX-30;
  var battX=180;

  var invXs=spread(320, 660, n);
  var kesXs=centers(battX, k, 140);

  var nodes=[
    {key:'grid', cx:gridX, cy:MAI, label:'Сеть'},
    {key:'meter',cx:meterX,cy:MAI, label:'Счётчик'},
    {key:'map',  cx:mapX,  cy:MAI, label:'МАП',
      tempPos:'above',
      temps:[
        {label:'Тор', get:function(d){ return tempValue(d.map_temps, 'Тор'); }},
        {label:'Транзисторы', get:function(d){ return tempValue(d.map_temps, 'Транзисторы'); }}
      ]},
    // «Сеть дома» — узел-соединитель справа от МАП: МАП, шина инверторов, Дом.
    {key:'net',  cx:nodeX, cy:MAI, label:'Сеть дома'},
    {key:'house',cx:houseX,cy:MAI, label:'Дом', raise:26},
    {key:'battery',cx:battX,cy:BATTY, label:'Батарея',
      tempPos:'right',
      temps:[{label:'Батарея', get:function(d){ return (d.battery_temp===undefined?null:d.battery_temp); }}]}
  ];

  var edges=[
    // Магистраль (горизонтальная, на уровне MAI).
    {pts:[[gridX,MAI],[meterX,MAI]], rule:{greenSign:-1,greenDir:'toStart'},
     label:{x:(gridX+meterX)/2, y:MAI-12},
     getValue:function(d){return d.meter_active_power;}},
    {pts:[[meterX,MAI],[mapX,MAI]], rule:{greenSign:-1,greenDir:'toStart'},
     label:{x:(meterX+mapX)/2, y:MAI-12},
     getValue:function(d){return d.map_grid_power;}},
    // МАП → «Сеть дома» (вклад сети и батареи) и «Сеть дома» → Дом (потребление дома).
    {pts:[[mapX,MAI],[nodeX,MAI]], rule:{greenSign:-1,greenDir:'toStart'},
     label:{x:(mapX+nodeX)/2, y:MAI-12},
     getValue:function(d){return d.map_grid_power + d.map_battery_power;}},
    {pts:[[nodeX,MAI],[houseX,MAI]], rule:{greenSign:-1,greenDir:'toStart'},
     label:{x:(nodeX+houseX)/2, y:MAI-12},
     getValue:function(d){return d.house_power;}}
  ];

  // Ветвь «Сеть дома» → шина инверторов → инверторы → панели (справа).
  if(n>0){
    // «Сеть дома» → вниз к шине инверторов. При выдаче (Σac>0) энергия идёт вверх к узлу.
    edges.push({pts:[[nodeX,MAI],[nodeX,BUSY]],
      rule:{greenSign:1,greenDir:'toStart',labelSign:-1},
      label:{x:nodeX-14, y:(MAI+BUSY)/2},
      getValue:function(d){ var s=0; for(var i=0;i<d.inverters.length;i++) s+=d.inverters[i].ac; return s; }});
    // Шина инверторов — медная с болтами в точках присоединения инверторов.
    edges.push({bus:true, x1:invXs[0], x2:nodeX, y:BUSY, bolts:invXs});
    for(var i=0;i<n;i++){
      var ix=invXs[i], inv=invs[i];
      nodes.push({key:inv.kind,cx:ix,cy:INVY,label:inv.name,
        staleOf:(function(idx){return function(d){return d.inverters[idx].stale;};})(i),
        temps:inverterTempsSpec(i), tempPos:'right'});
      nodes.push({key:'panel',cx:ix,cy:PANY,label:''});
      // инвертор → шина (вверх)
      edges.push({pts:[[ix,INVY-sprH(inv.kind)/2],[ix,BUSY]], rule:{greenSign:1,greenDir:'toEnd',labelSign:-1},
        label:{x:ix+38,y:(INVY-39+BUSY)/2}, stale:inv.stale,
        getValue:(function(idx){return function(d){return d.inverters[idx].ac;};})(i)});
      // панель → инвертор (вверх)
      var pvTop=INVY+sprH(inv.kind)/2, pvBot=PANY-31;
      // выработка: от панели (низ) вверх к инвертору (toStart)
      edges.push({pts:[[ix,pvTop],[ix,pvBot]], rule:{greenSign:1,greenDir:'toStart',labelSign:-1},
        label:{x:ix+38,y:(pvTop+pvBot)/2}, stale:inv.stale,
        getValue:(function(idx){return function(d){return d.inverters[idx].pv;};})(i)});
    }
  }

  // МАП (левый порт) ↓ вниз → батарея (слева) — одна связь с поворотом 90°.
  // Знак «наоборот» (−battery_power): заряд красный, отдача зелёная.
  edges.push({pts:[[mapPortL,mapBotY],[mapPortL,BATTY],[battX,BATTY]],
    rule:{greenSign:-1,greenDir:'toStart'},
    label:{x:(mapPortL+battX)/2+55, y:BATTY-12},
    getValue:function(d){return -d.map_battery_power;}});

  // Ветвь батарея → КЭС → панели (под батареей) — только при наличии КЭС.
  if(k>0){
    var kx1=kesXs[0], kx2=kesXs[k-1];
    edges.push({pts:[[battX,BATTY+sprH('battery')/2],[battX,KESY-40]],
      rule:{greenSign:1,greenDir:'toEnd',labelSign:-1},
      label:{x:battX-14, y:(BATTY+30+KESY-40)/2},
      getValue:function(d){ var s=0; for(var i=0;i<d.kes.length;i++) s+=d.kes[i].ac; return s; }});
    edges.push({bus:true, x1:kx1, x2:kx2, y:KESY-40, bolts:kesXs});
    for(var j=0;j<k;j++){
      var kx=kesXs[j], kes=kes[j];
      nodes.push({key:'kes',cx:kx,cy:KESY,label:kes.name,
        staleOf:(function(idx){return function(d){return d.kes[idx].stale;};})(j)});
      nodes.push({key:'panel',cx:kx,cy:KPANY,label:''});
      edges.push({pts:[[kx,KESY-sprH('kes')/2],[kx,KESY-40]], rule:{greenSign:1,greenDir:'toEnd',labelSign:-1},
        label:{x:kx+38,y:(KESY-37+KESY-40)/2}, stale:kes.stale,
        getValue:(function(idx){return function(d){return d.kes[idx].ac;};})(j)});
      var kPvTop=KESY+sprH('kes')/2, kPvBot=KPANY-31;
      // выработка: от панели (низ) вверх к КЭС (toStart)
      edges.push({pts:[[kx,kPvTop],[kx,kPvBot]], rule:{greenSign:1,greenDir:'toStart',labelSign:-1},
        label:{x:kx+38,y:(kPvTop+kPvBot)/2}, stale:kes.stale,
        getValue:(function(idx){return function(d){return d.kes[idx].pv;};})(j)});
    }
  }

  return {nodes:nodes, edges:edges, height:KPANY+80, width:houseX+120};
}

// ---------- Схема Гаража ----------
// Магистраль сверху: Сеть → Счётчик CE308(развилка) → Гараж. От точки (стыка труб)
// вправо от счётчика — вниз ветвь инверторов → панели; счётчик стоит левее стыка,
// чтобы не находиться над ним. Мощность счётчик↔сеть — по данным CE308; мощность
// в гараж (справа от развилки) = P(CE308) − Σac(инверторы гаража).
function layoutGarage(data){
  var invs=data.inverters||[];
  var n=invs.length;
  var MAI=150, BUSY=330, INVY=440, PANY=570;
  var gridX=110, meterX=330, nodeX=580, garageX=820;
  var invXs=spread(140, 540, n);

  var nodes=[
    {key:'grid',  cx:gridX,  cy:MAI, label:'Сеть'},
    {key:'ce308', cx:meterX, cy:MAI, label:'Счётчик'},
    // «Сеть гаража» — узел-соединитель справа от CE308: CE308, шина инверторов, Гараж.
    {key:'net',   cx:nodeX,  cy:MAI, label:'Сеть гаража'},
    {key:'garage',cx:garageX,cy:MAI,label:'Гараж', raise:24}
  ];

  var edges=[
    // Сеть → счётчик CE308. Потребление из сети (CE308>0) — красный, отдача — зелёный.
    {pts:[[gridX,MAI],[meterX,MAI]], rule:{greenSign:-1,greenDir:'toStart'},
     label:{x:(gridX+meterX)/2, y:MAI-12},
     getValue:function(d){return d.ce308_power;}},
    // Счётчик → «Сеть гаража» (показания CE308) и «Сеть гаража» → Гараж (остаток).
    {pts:[[meterX,MAI],[nodeX,MAI]], rule:{greenSign:-1,greenDir:'toStart'},
     label:{x:(meterX+nodeX)/2, y:MAI-12},
     getValue:function(d){return d.ce308_power;}},
    {pts:[[nodeX,MAI],[garageX,MAI]], rule:{greenSign:-1,greenDir:'toStart'},
     label:{x:(nodeX+garageX)/2, y:MAI-12},
     getValue:function(d){return d.garage_power;}}
  ];

  if(n>0){
    // «Сеть гаража» → вниз к шине инверторов. При выдаче (Σac>0) энергия идёт вверх к узлу.
    edges.push({pts:[[nodeX,MAI],[nodeX,BUSY]],
      rule:{greenSign:1,greenDir:'toStart',labelSign:-1},
      label:{x:nodeX-14, y:(MAI+BUSY)/2},
      getValue:function(d){ var s=0; for(var i=0;i<d.inverters.length;i++) s+=d.inverters[i].ac; return s; }});
    edges.push({bus:true, x1:invXs[0], x2:nodeX, y:BUSY, bolts:invXs});
    for(var i=0;i<n;i++){
      var ix=invXs[i], inv=invs[i];
      nodes.push({key:inv.kind,cx:ix,cy:INVY,label:inv.name,
        staleOf:(function(idx){return function(d){return d.inverters[idx].stale;};})(i),
        temps:inverterTempsSpec(i), tempPos:'right'});
      nodes.push({key:'panel',cx:ix,cy:PANY,label:''});
      edges.push({pts:[[ix,INVY-sprH(inv.kind)/2],[ix,BUSY]], rule:{greenSign:1,greenDir:'toEnd',labelSign:-1},
        label:{x:ix+38,y:(INVY-39+BUSY)/2}, stale:inv.stale,
        getValue:(function(idx){return function(d){return d.inverters[idx].ac;};})(i)});
      var pvTop=INVY+sprH(inv.kind)/2, pvBot=PANY-31;
      // выработка: от панели (низ) вверх к инвертору (toStart)
      edges.push({pts:[[ix,pvTop],[ix,pvBot]], rule:{greenSign:1,greenDir:'toStart',labelSign:-1},
        label:{x:ix+38,y:(pvTop+pvBot)/2}, stale:inv.stale,
        getValue:(function(idx){return function(d){return d.inverters[idx].pv;};})(i)});
    }
  }

  return {nodes:nodes, edges:edges, height:PANY+80, width:garageX+120};
}

// ---------- Пересборка схемы при изменении набора устройств ----------
function ensureScheme(which, layoutData, container){
  var sig=layoutData.nodes.map(function(n){return n.label;}).join('|')+'#'+layoutData.height+'x'+layoutData.width;
  if(BUILT[which] && BUILT[which].sig===sig) return BUILT[which].obj;
  var obj=buildScheme(container, layoutData.nodes, layoutData.edges, layoutData.height, layoutData.width);
  BUILT[which]={sig:sig, obj:obj};
  var svg=obj.svg;
  svg.setAttribute('viewBox','0 0 '+layoutData.width+' '+layoutData.height);
  svg.setAttribute('preserveAspectRatio','xMidYMid meet');
  return obj;
}

function refreshEdges(obj, data){
  for(var i=0;i<obj.edges.length;i++){
    var e=obj.edges[i];
    e.value=e.getValue(data);
    updateEdge(e);
  }
  // Спрайты stale-устройств (инверторы/КЭС): приглушаем, когда устройство молчит.
  var sprites=obj.sprites||[];
  for(var s=0;s<sprites.length;s++){
    var stale=!!sprites[s].staleOf(data);
    sprites[s].img.setAttribute('class', stale?'anim-sprite anim-sprite-stale':'anim-sprite');
  }
  // Показания счётчика (есть только в схеме Дома: meterReads на объекте).
  var reads=obj.meterReads||[];
  for(var k=0;k<reads.length;k++){
    reads[k].day.textContent=fmtKWh(data.meter_import_total);
    reads[k].night.textContent=fmtKWh(data.meter_export_total);
  }
  // Температуры (МАП-панель, инверторы, батарея): обновляем из данных схемы.
  var tupds=obj.tempUpds||[];
  for(var tu=0;tu<tupds.length;tu++) tupds[tu](data);
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

// ---------- Температуры на схеме ----------
// Значения температур (°C) приходят из /api/animation: для МАП — панель над
// спрайтом (подписи «Тор»/«Транзисторы» видны), для инверторов и батареи —
// значения справа от спрайта без подписей, но с подсказкой при наведении.
// Подсказка — собственный HTML-div (не <title>): нативный <title> в SVG браузер
// показывает с задержкой ~1-2 с, которой нельзя управлять; свой div появляется
// мгновенно по mouseenter.
function fmtTemp(v){
  if(v===null || v===undefined || !isFinite(v)) return null; // нет датчика → не рисуем
  return Math.round(v)+'°C';
}
// tempValue извлекает значение температуры из списка []{label,value} по подписи.
function tempValue(arr, label){
  if(!arr) return null;
  for(var i=0;i<arr.length;i++) if(arr[i].label===label) return arr[i].value;
  return null;
}
// Быстрая подсказка: один общий div на страницу, позиционируется по курсору
// (position:fixed), поэтому масштаб/панорама SVG на неё не влияют.
var ANIM_TIP=null;
function animTipEl(){
  if(ANIM_TIP) return ANIM_TIP;
  var d=document.createElement('div');
  d.className='anim-tip';
  document.body.appendChild(d);
  ANIM_TIP=d;
  return d;
}
function moveAnimTip(ev){
  var d=ANIM_TIP; if(!d) return;
  var w=d.offsetWidth, h=d.offsetHeight;
  var x=ev.clientX+12, y=ev.clientY+14;
  if(x+w>window.innerWidth-4) x=ev.clientX-w-12;   // не вылезать за правый край
  if(y+h>window.innerHeight-4) y=ev.clientY-h-14;  // ... и за нижний
  d.style.left=x+'px'; d.style.top=y+'px';
}
function bindTempTip(el, label){
  el.addEventListener('mouseenter', function(ev){
    var d=animTipEl(); d.textContent=label; d.style.display='block'; moveAnimTip(ev);
  });
  el.addEventListener('mousemove', moveAnimTip);
  el.addEventListener('mouseleave', function(){ if(ANIM_TIP) ANIM_TIP.style.display='none'; });
}
// renderNodeTemps рисует температуры узла схемы (node.temps — [{label,get(data)}]).
//   "above" — панель над спрайтом (подпись + значение, для МАП);
//   "right" — значения справа от спрайта (без видимых подписей, подсказка при
//             наведении; для инверторов и батареи).
// Возвращает массив функций-обновителей, вызываемых с данными схемы в refreshEdges.
// Отсутствующее значение (нет датчика) прячет элемент целиком, а не рисует «—».
// ВАЖНО: число выводится в <tspan>, а не через textContent, иначе при обновлении
// затираются дочерние элементы (подсказка перестаёт работать).
var TEMP_ROW_H=16, TEMP_PAD_V=5, TEMP_PAD_H=8, TEMP_VAL_W=40;
function renderNodeTemps(svg, n){
  var upds=[];
  var key=n.key, cx=n.cx, cy=n.cy, raise=n.raise||0;
  var spec=SPR[sprKey(key)]||SPR.grid;
  var effW=spec.w*(spec.wScale||1);
  var g=document.createElementNS(NS,'g');
  if(n.tempPos==='above'){
    // Ширина панели — под самую длинную надпись + место под значение:
    // maxLabelPx + gap + valPx + 2*pad, но не уже минимума.
    var rows=n.temps.length;
    var hgt=rows*TEMP_ROW_H+TEMP_PAD_V*2;
    var labelMax=0;
    for(var jw=0;jw<rows;jw++){
      var lp=(''+n.temps[jw].label).length*6.2; // грубая ширина подписи (10px шрифт)
      if(lp>labelMax) labelMax=lp;
    }
    // Немного уже минимума (подписи стали мельче) — панель не выглядит громоздкой.
    var w=Math.max(labelMax+8+TEMP_VAL_W+TEMP_PAD_H*2, 92);
    var x=cx-w/2, y=(cy-raise-spec.h/2-6)-hgt;
    var box=document.createElementNS(NS,'rect');
    box.setAttribute('x',x); box.setAttribute('y',y);
    box.setAttribute('width',w); box.setAttribute('height',hgt);
    box.setAttribute('rx',5);
    box.setAttribute('class','anim-temp-panel');
    g.appendChild(box);
    for(var j=0;j<rows;j++){(function(t){
      var ly=y+TEMP_PAD_V+j*TEMP_ROW_H+TEMP_ROW_H*0.72;
      var lab=document.createElementNS(NS,'text');
      lab.setAttribute('x',x+TEMP_PAD_H); lab.setAttribute('y',ly);
      lab.setAttribute('class','anim-temp-label'); lab.textContent=t.label;
      g.appendChild(lab);
      var val=document.createElementNS(NS,'text');
      val.setAttribute('x',x+w-TEMP_PAD_H); val.setAttribute('y',ly);
      val.setAttribute('text-anchor','end');
      val.setAttribute('class','anim-temp-val');
      var tsp=document.createElementNS(NS,'tspan'); tsp.textContent='';
      val.appendChild(tsp);
      g.appendChild(val);
      upds.push(function(d){ var s=(t.stale&&t.stale(d))?null:fmtTemp(t.get(d)); if(!s){val.style.visibility='hidden';} else {val.style.visibility=''; tsp.textContent=s;} });
    })(n.temps[j]);}
  } else { // "right"
    var gap=5;
    // Границы видимого контента внутри спрайта (visX0/visX1); по умолчанию — весь
    // бокс. Так метка ставится к самому спрайту, а не к его прозрачным полям
    // (у батареи справа ~30% прозрачного поля).
    var visX0=(spec.visX0===undefined?0:spec.visX0);
    var visX1=(spec.visX1===undefined?1:spec.visX1);
    var contentL=cx-effW/2+effW*visX0;
    var contentR=cx-effW/2+effW*visX1;
    var rightX=contentR+gap;
    // Если справа места нет (батарея — правый крайний элемент схемы, viewBox 1000),
    // значение ставим слева от спрайта (rightX вылезает за правое поле). Батарея
    // с ветвью КЭС стоит у правого края (battX≈920), поэтому температура батареи
    // отображается слева — вплотную к спрайту.
    var onLeft = (rightX+22) > 996;
    var rx = onLeft ? contentL-gap : rightX;
    var anchor = onLeft ? 'end' : 'start';
    var off=(n.temps.length-1)*7.5;
    for(var m=0;m<n.temps.length;m++){(function(t, idx){
      var vy=cy-off+idx*15;
      var val=document.createElementNS(NS,'text');
      val.setAttribute('x',rx); val.setAttribute('y',vy);
      val.setAttribute('text-anchor',anchor);
      val.setAttribute('class','anim-temp-side');
      // Мгновенная подсказка (см. bindTempTip); нативный <title> не используем.
      bindTempTip(val, t.label);
      var tsp=document.createElementNS(NS,'tspan'); tsp.textContent='';
      val.appendChild(tsp);
      g.appendChild(val);
      upds.push(function(d){ var s=(t.stale&&t.stale(d))?null:fmtTemp(t.get(d)); if(!s){val.style.visibility='hidden';} else {val.style.visibility=''; tsp.textContent=s;} });
    })(n.temps[m], m);}
  }
  svg.appendChild(g);
  return upds;
}

function fmtSec(t){
  var d=new Date(t);
  function p(x){return (x<10?'0':'')+x;}
  return d.getFullYear()+'-'+p(d.getMonth()+1)+'-'+p(d.getDate())+' '+p(d.getHours())+':'+p(d.getMinutes())+':'+p(d.getSeconds());
}

async function tick(){
  if(window.srRefresh && !window.srRefresh.isEnabled()) return;
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

// ---------- Опрос /api/animation по видимости спойлеров Дом/Гараж ----------
// Один запрос /api/animation отдаёт обе схемы (и Дом, и Гараж), и единый tick()
// пересобирает обе. Поэтому поллер включаем, когда открыт хотя бы один из двух
// спойлеров (не дублируем запрос на каждый спойлер); при открытии — немедленный
// опрос + интервал 1 с; при закрытии последнего открытого — останавливаем, чтобы
// закрытый спойлер не дёргал API.
function spoilerOpen(key){ return !!(window.srSpoilers && window.srSpoilers.isOpen(key)); }
function animAnyOpen(){ return spoilerOpen('house') || spoilerOpen('garage'); }
var ANIM_POLL_MS=1000, animRunning=false, animTimer=null;
function animStart(){
  // При выключенном обновлении опрос не запускается (даже если спойлер открыт).
  if(animRunning || (window.srRefresh && !window.srRefresh.isEnabled())) return;
  animRunning=true;
  tick(); animTimer=setInterval(tick, ANIM_POLL_MS);
}
function animStop(){
  if(!animRunning) return;
  animRunning=false;
  clearInterval(animTimer); animTimer=null;
}
function animSync(){ if(animAnyOpen()) animStart(); else animStop(); }
if(window.srSpoilers){
  window.srSpoilers.listen('house', animSync);
  window.srSpoilers.listen('garage', animSync);
}
// Выключатель обновления: при включении — перезапуск по видимости спойлеров
// (немедленный опрос), при выключении — остановка опроса /api/animation.
if(window.srRefresh){ window.srRefresh.register(animSync, animStop); }
animSync(); // старт/стоп по текущему состоянию спойлеров при загрузке

var lastNow=performance.now();
function loop(now){
  var dt=Math.min(0.1, (now-lastNow)/1000); lastNow=now; // с, без рывка после фона
  // При выключенном обновлении огоньки замораживаются (dt=0 — анимация не движется,
  // но цикл продолжает работать и размораживается сразу после включения).
  if(window.srRefresh && !window.srRefresh.isEnabled()) dt=0;
  // Анимируем только видимые (открытые) схемы: закрытый спойлер не тратит кадры.
  if(spoilerOpen('house') && BUILT.house) for(var i=0;i<BUILT.house.obj.edges.length;i++) animateEdge(BUILT.house.obj.edges[i], dt);
  if(spoilerOpen('garage') && BUILT.garage) for(var j=0;j<BUILT.garage.obj.edges.length;j++) animateEdge(BUILT.garage.obj.edges[j], dt);
  requestAnimationFrame(loop);
}
requestAnimationFrame(loop);

// ---------- Панорамирование и зум анимированных схем (pinch/колесо/перетаскивание) ----------
// Работает на контейнере .anim-scheme: трансформирует сам SVG (transform: translate+scale),
// не трогая viewBox, поэтому огоньки/подписи масштабируются вместе со схемой. Масштаб
// ограничен [1, 6] (меньше 1 — схема как была, бессмысленно уменьшать), панорама
// ограничена рамками увеличенной области, чтобы не увести схему за пределы экрана.
// PINCH_MIN — минимальная база стартового «разлёта» пальцев: если пальцы пришли в
// одну точку, относительный прирост иначе прыгает на максимум.
var PINCH_MIN=60;
function attachSchemePanZoom(scheme){
  if(!scheme || scheme.__panzoom) return;
  scheme.__panzoom=true;
  var svg=scheme.querySelector('.anim-svg');
  if(!svg) return;
  var min=1, max=6, scale=min, tx=0, ty=0;

  // Смещение SVG относительно контейнера (независимо от трансформа). У SVG-элемента
  // offsetLeft/offsetTop не стандартизированы (возвращают undefined на Android),
  // поэтому берём разность getBoundingClientRect(): sr.left-cr.left — позиция SVG
  // внутри scheme (scheme имеет position:relative). При translate+scale локальная
  // точка u (в своём боксе) отображается в контейнере как  S.left + tx + u*scale.
  var svgOff=function(){
    var sr=svg.getBoundingClientRect(), cr=scheme.getBoundingClientRect();
    return {left:sr.left-cr.left, top:sr.top-cr.top};
  };
  var box=function(){ return scheme.getBoundingClientRect(); }; // контейнер в клиентских

  function apply(){
    svg.style.transform='translate('+tx.toFixed(2)+'px,'+ty.toFixed(2)+'px) scale('+scale.toFixed(3)+')';
  }
  function clampPan(){
    var b=box(), w=b.width, h=b.height;
    var sw=w*scale, sh=h*scale;   // экранная площадь содержимого ≈ контейнер*scale
    var pad=w*0.05;
    tx=Math.max(w-sw-pad, Math.min(pad, tx));
    ty=Math.max(h-sh-pad, Math.min(pad, ty));
  }
  // ---------- Жесты (тач-контроллер через Pointer Events) ----------
  // Pointer Events покрывают тач и перо. Мус-события игнорируются, чтобы на
  // десктопе колесо/клики не перехватывались (зум/панорама только на сенсорных).
  // Держим карту активных указателей (pointerId → позиция), поэтому мультитач-пинч
  // работает. setPointerCapture НЕ используем: он на некоторых Android ломает
  // второй палец. touch-action:none (см. CSS) запрещает браузеру забирать жест.
  var pts={};                             // pointerId -> {x,y} (clientX/Y)
  var gs=null;                            // состояние жеста
  function ptsList(){ return Object.keys(pts).map(function(k){return pts[k];}); }
  function activeCount(){ return Object.keys(pts).length; }
  function mid(){ // центр всех активных указателей в координатах контейнера
    var ls=ptsList(), n=ls.length, r=box(), sx=0, sy=0;
    if(n===0) return null;
    for(var i=0;i<n;i++){ sx+=ls[i].x; sy+=ls[i].y; }
    return {x:sx/n-r.left, y:sy/n-r.top};
  }
  function dist(){ // расстояние между первыми двумя указателями
    var ls=ptsList(), n=ls.length;
    if(n<2) return 0;
    return Math.hypot(ls[0].x-ls[1].x, ls[0].y-ls[1].y);
  }
  scheme.addEventListener('pointerdown', function(e){
    if(e.pointerType==='mouse') return; // на десктопе зум/панорама отключены
    if(e.target.closest('a,button,input')) return;
    pts[e.pointerId]={x:e.clientX, y:e.clientY};
    var n=activeCount();
    if(n===2){
      // Пинч: фиксируем стартовое расстояние. Если пальцы пришли почти в одну
      // точку (startDist ≈ 0), относительный прирост d/startDist сразу огромный и
      // масштаб «прыгает» на максимум. Задаём минимальную базу разлёта, чтобы зум
      // нарастал плавно от исходного масштаба.
      var sd=dist();
      gs={mode:'pinch', startDist:Math.max(PINCH_MIN, sd), startScale:scale, startTx:tx, startTy:ty,
          startMid:mid()};
    } else if(n===1){
      gs={mode:'pan', startX:e.clientX, startY:e.clientY, startTx:tx, startTy:ty};
    }
    if(e.cancelable && e.pointerType!=='mouse') e.preventDefault();
  });
  scheme.addEventListener('pointermove', function(e){
    if(e.pointerType==='mouse') return;
    if(!pts[e.pointerId]) return;
    pts[e.pointerId]={x:e.clientX, y:e.clientY};
    var n=activeCount();
    if(gs){
      if(gs.mode==='pinch' && n>=2){
        var d=dist(), m=mid(), o=svgOff();
        var ratio=d/(gs.startDist||1);
        // Целевой масштаб от разведения пальцев (база PINCH_MIN уже защищает от
        // скачка при почти нулевом старте). Реальный масштаб плавно стремится к
        // цели, поэтому жёсткого клэмпа прироста нет — иначе зум «застревает» на
        // малом значении и движение пальцев не ощущается.
        var target=Math.max(min,Math.min(max, gs.startScale*ratio));
        scale += (target-scale)*0.25;
        var ux=(gs.startMid.x-o.left-gs.startTx)/gs.startScale;
        var uy=(gs.startMid.y-o.top-gs.startTy)/gs.startScale;
        tx=m.x-o.left-ux*scale;
        ty=m.y-o.top-uy*scale;
        clampPan(); apply();
      } else if(gs.mode==='pan' && n===1){
        var p=ptsList()[0];
        tx=gs.startTx+(p.x-gs.startX);
        ty=gs.startTy+(p.y-gs.startY);
        if(scale<=1.001){ tx=0; ty=0; }
        clampPan(); apply();
      }
    }
    if(e.cancelable && e.pointerType!=='mouse') e.preventDefault();
  });
  function endPointer(e){
    if(!pts[e.pointerId]) return;
    delete pts[e.pointerId];
    var n=activeCount();
    if(n===0){ gs=null; }
    else if(n===1){ // остался один палец — продолжаем панораму от него
      var p=ptsList()[0];
      gs={mode:'pan', startX:p.x, startY:p.y, startTx:tx, startTy:ty};
    }
    if(n<2 && gs && gs.mode==='pinch'){ gs=null; }
  }
  scheme.addEventListener('pointerup', endPointer);
  scheme.addEventListener('pointercancel', endPointer);

  return {
    reset:function(){ scale=min; tx=0; ty=0; apply(); },
    isActive:function(){ return scale>min || tx!==0 || ty!==0; }
  };
}

document.querySelectorAll('.anim-scheme').forEach(attachSchemePanZoom);

})();
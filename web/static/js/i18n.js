'use strict';
// Трёхъязычный дашборд (ru/en/zh). Перевод на клиенте: словарь «русская фраза →
// перевод»; замена выполняется по ГРАНИЦАМ кириллических слов (не по подстрокам),
// поэтому слова не ломаются. Русский — исходный (identity). Язык: ?lang= > localStorage > 'en'.
(function(){
  var LANGS=[['ru','Русский'],['zh','中文'],['en','English']];
  var LANG='en';
  try{
    var m=/[?&]lang=(ru|en|zh)\b/.exec(location.search);
    if(m){ LANG=m[1]; try{ localStorage.setItem('lang',LANG); }catch(e){} }
    else { var s=localStorage.getItem('lang'); if(s==='ru'||s==='en'||s==='zh') LANG=s; }
  }catch(e){}

  var DICT={
    en:{
      // Навигация/шапка
      'Главная':'Home','Графики':'Charts','Электроэнергия':'Energy',
      'Текущие параметры инверторов и электросчётчика (из Redis, обновление каждую секунду)':'Current inverter and meter parameters (from Redis, updated every second)',
      'обновление раз в минуту':'updates once a minute','обновляется раз в минуту':'updates once a minute',
      'обновляется каждую секунду':'updates every second',
      'Загрузка...':'Loading...','Нет данных':'No data',
      'BMS (батареи)':'BMS (batteries)',
      'Дом':'House','Дома':'House','Гараж':'Garage','Детальные данные':'Details',
      'выработка / в сеть':'generation / to grid','из сети (потребление)':'from grid (consumption)',
      'Мощность на связях — в ваттах (W), знак показывает направление.':'Link power in watts (W); the sign shows direction.',
      'Отключить обновление':'Disable refresh','Выключить опрос API и обновление данных по таймеру':'Disable API polling and timed refresh',
      // Период/кнопки
      'Сегодня':'Today','Вчера':'Yesterday','7 дней':'7 days','Месяц':'Month','За выбранный день':'For selected day','Обновить графики':'Refresh charts','Показать период':'Show period',
      'Выбрать день':'Pick a day','Начало периода':'Period start','Конец периода':'Period end','Принудительно обновить графики':'Force refresh charts',
      'Предыдущий период':'Previous period','Следующий период':'Next period','Обновить':'Refresh',
      'С':'from','по':'to','Показать':'Show','Скрыть':'Hide','30 дней':'30 days','Все':'All',
      // CE308
      'Электросчётчик CE308 — текущие данные':'Meter CE308 — current data','Электросчётчик CE308 — показания':'Meter CE308 — readings',
      'Актуально:':'As of:','Параметр':'Parameter','Напряжение (V)':'Voltage (V)','Ток (A)':'Current (A)',
      'Активная мощность (W)':'Active power (W)','Реактивная мощность (var)':'Reactive power (var)',
      'Отрицательное значение — отдача в сеть (генерация); положительное — потребление.':'Negative value — export to grid (generation); positive — consumption.',
      'Активная энергия':'Active energy','Реактивная энергия':'Reactive energy','потребление':'consumption','отдача':'export',
      'День':'Day','Ночь':'Night','Всего (А+):':'Total (A+):','Всего (А−):':'Total (A−):','Всего (R+):':'Total (R+):','Всего (R−):':'Total (R−):',
      'А+':'A+','А−':'A−',
      // DDS238/тарифы
      'Электросчётчик DDS238 — текущие параметры':'Meter DDS238 — current parameters',
      'Мощность с отрицательным знаком — отдача в сеть (генерация); положительная — потребление.':'Negative power — export to grid (generation); positive — consumption.',
      'Потребление/Отдача за сегодня':'Consumption/Export today','Потребление/Отдача за месяц':'Consumption/Export this month','Потребление/Отдача за год':'Consumption/Export this year',
      'Потребление':'Consumption','Отдача':'Export','Потребление день':'Consumption day','Отдача день':'Export day','Потребление ночь':'Consumption night','Отдача ночь':'Export night',
      'День 07:00–23:00':'Day 07:00–23:00','Ночь 23:00–07:00':'Night 23:00–07:00','Текущий месяц':'Current month','Прошлый месяц':'Previous month','Текущий год':'Current year','Прошлый год':'Previous year',
      // МАП/index
      'МАП':'MAP','Данные МАП':'MAP data','Напряжение сети':'Grid voltage','Мощность сети':'Grid power','Напряжение батареи':'Battery voltage','Мощность батареи':'Battery power','Мощность дома':'House power',
      'МАП (батарея/сеть)':'MAP (battery/grid)','Инверторы + сеть + батарея':'Inverters + grid + battery','Мощности инверторов':'Inverter powers',
      'с шины МАП':'from the MAP bus','Батарея':'Battery','батарея':'battery','сеть':'grid','Сеть':'Grid',
      // Графики (общие)
      'Напряжения ячеек':'Cell voltages','Напряжения ячеек, V':'Cell voltages, V','Температуры':'Temperatures','Температуры T1–T4 (батарея, силовые ключи, плата), °C':'Temperatures T1–T4 (battery, power switches, board), °C',
      'Разброс ячеек (max−min), V':'Cell spread (max−min), V','Мощностные ключи':'Power switches','Заряд (SOC)':'Charge (SOC)','Напряжение пакета':'Pack voltage',
      'Ток':'Current','Мощность':'Power','Ёмкость':'Capacity','Напряжение батареи, V':'Battery voltage, V','Ток, A':'Current, A','Мощность, W':'Power, W',
      'сред.':'avg','В/яч':'V/cell','А·ч':'Ah','максимальное':'max','минимальное':'min','остальные':'others','разброс':'spread','макс':'max','мин':'min','Датчик':'Sensor','Ячейка':'Cell','ячеек':'cells','ячейка':'cell',
      'Диапазон:':'Range:','Зум: Ctrl+колесо / drag–панорама':'Zoom: Ctrl+wheel / drag to pan',
      'ЛКМ по чипу — вкл/выкл линию · двойной ЛКМ — только эта линия · «Все» — показать все':'Click a chip to toggle a line · double-click for this line only · "All" shows all',
      'ЛКМ по чипу — вкл/выкл · двойной ЛКМ — только этот показатель · «Все» — показать все':'Click a chip to toggle · double-click for this metric only · "All" shows all',
      'остаток':'remaining','из':'of',
      // Note BMS
      'T1–T6 — NTC-датчики температуры. Производитель не публикует точное соответствие каналов и мест; подписи реконструированы по даташиту AFE (2 внешних датчика + датчик платы), мануалу ANT (до 4 внешних датчиков) и параметрам защит (температура батареи / силовых ключей). В нашей установке все каналы показывают одинаковую температуру (батарея и плата в одном корпусе).':'T1–T6 are NTC temperature sensors. The manufacturer does not publish the exact channel/location mapping; labels are reconstructed from the AFE datasheet (2 external sensors + board sensor), the ANT manual (up to 4 external sensors) and protection settings (battery / power-switch temperature). In our installation all channels show the same temperature (battery and board in one enclosure).',
      // Страница «Графики»
      'Временные ряды за выбранный период; статистика счётчика «день/ночь» за последние дни':'Time series for the selected period; meter day/night statistics for recent days',
      'Напряжения сети и батареи (МАП), V + напряжение счётчика/CE308':'Grid and battery (MAP) voltages, V + meter/CE308 voltage',
      'Напряжение счётчика':'Meter voltage',
      'Мощности сети и батареи (МАП), W + активная мощность счётчика/CE308':'Grid and battery (MAP) power, W + meter/CE308 active power',
      'Инверторы Дома (Σ)':'House inverters (Σ)','Мощность счётчика (активная)':'Meter power (active)',
      'Суммарная активная мощность, W':'Total active power, W','Активная мощность по инверторам, W':'Active power by inverter, W',
      'Левый':'Left','Правый':'Right','Средний':'Middle','Корпус':'Housing','Транзисторы':'Transistors','Тор':'Toroid',
      // Страница «Энергия»
      'Счётчик DDS238: потребление/отдача по тарифам «День»/«Ночь»':'Meter DDS238: consumption/export by Day/Night tariffs',
      'Потребление / Отдача по тарифу «День» и «Ночь» по дням, kWh':'Consumption / Export by Day and Night tariffs, by day, kWh',
      'Потребление / Отдача по тарифу «День» и «Ночь» по месяцам, kWh':'Consumption / Export by Day and Night tariffs, by month, kWh',
      'Нет финализированных дней за выбранный период':'No finalized days for the selected period',
      'Показано дней:':'Days shown:','по месяцам':'by months','Потребление / Отдача':'Consumption / Export',
      // AGM
      'Свинцово-кислотный АКБ':'Lead-acid (AGM) battery',
      'AGM = батарея МАП − Σ (ANT×2 + EnBMS) · обновляется раз в минуту':'AGM = MAP battery − Σ (ANT×2 + EnBMS) · updates once a minute',
      'Графики (расчёт: МАП − Σ BMS)':'Charts (calc: MAP − Σ BMS)',
      'Графики (Redis — сырые показания, PG — 5-мин средние)':'Charts (Redis — raw, PG — 5-min averages)',
      'Знак':'Sign','Балансировка':'Balancing'
    },
    zh:{
      'Главная':'主页','Графики':'图表','Электроэнергия':'电能',
      'Текущие параметры инверторов и электросчётчика (из Redis, обновление каждую секунду)':'逆变器与电表实时参数（来自 Redis，每秒更新）',
      'обновление раз в минуту':'每分钟更新一次','обновляется раз в минуту':'每分钟更新一次','обновляется каждую секунду':'每秒更新',
      'Загрузка...':'加载中...','Нет данных':'无数据','BMS (батареи)':'BMS（电池）',
      'Дом':'住宅','Дома':'住宅','Гараж':'车库','Детальные данные':'详细数据',
      'выработка / в сеть':'发电 / 上网','из сети (потребление)':'来自电网（用电）',
      'Мощность на связях — в ваттах (W), знак показывает направление.':'连线功率单位为瓦（W），符号表示方向。',
      'Отключить обновление':'停止刷新','Выключить опрос API и обновление данных по таймеру':'关闭 API 轮询与定时刷新',
      'Сегодня':'今天','Вчера':'昨天','7 дней':'7 天','Месяц':'月','За выбранный день':'指定日期','Обновить графики':'刷新图表','Показать период':'显示区间',
      'Выбрать день':'选择日期','Начало периода':'开始','Конец периода':'结束','Принудительно обновить графики':'强制刷新图表',
      'Предыдущий период':'上一区间','Следующий период':'下一区间','Обновить':'刷新',
      'С':'从','по':'至','Показать':'显示','Скрыть':'隐藏','30 дней':'30 天','Все':'全部',
      'Электросчётчик CE308 — текущие данные':'电表 CE308 — 当前数据','Электросчётчик CE308 — показания':'电表 CE308 — 读数',
      'Актуально:':'更新于：','Параметр':'参数','Напряжение (V)':'电压 (V)','Ток (A)':'电流 (A)','Активная мощность (W)':'有功功率 (W)','Реактивная мощность (var)':'无功功率 (var)',
      'Отрицательное значение — отдача в сеть (генерация); положительное — потребление.':'负值表示向电网输出（发电）；正值表示用电。',
      'Активная энергия':'有功电能','Реактивная энергия':'无功电能','потребление':'用电','отдача':'上网',
      'День':'白天','Ночь':'夜间','Всего (А+):':'总计 (A+)：','Всего (А−):':'总计 (A−)：','Всего (R+):':'总计 (R+)：','Всего (R−):':'总计 (R−)：',
      'А+':'A+','А−':'A−',
      'Электросчётчик DDS238 — текущие параметры':'电表 DDS238 — 当前参数',
      'Мощность с отрицательным знаком — отдача в сеть (генерация); положительная — потребление.':'负功率表示向电网输出（发电）；正功率表示用电。',
      'Потребление/Отдача за сегодня':'今日用电/上网','Потребление/Отдача за месяц':'本月用电/上网','Потребление/Отдача за год':'本年用电/上网',
      'Потребление':'用电','Отдача':'上网','Потребление день':'白天用电','Отдача день':'白天上网','Потребление ночь':'夜间用电','Отдача ночь':'夜间上网',
      'День 07:00–23:00':'白天 07:00–23:00','Ночь 23:00–07:00':'夜间 23:00–07:00','Текущий месяц':'本月','Прошлый месяц':'上月','Текущий год':'本年','Прошлый год':'去年',
      'МАП':'MAP','Данные МАП':'MAP 数据','Напряжение сети':'电网电压','Мощность сети':'电网功率','Напряжение батареи':'电池电压','Мощность батареи':'电池功率','Мощность дома':'住宅功率',
      'МАП (батарея/сеть)':'MAP（电池/电网）','Инверторы + сеть + батарея':'逆变器 + 电网 + 电池','Мощности инверторов':'逆变器功率',
      'с шины МАП':'来自 MAP 总线','Батарея':'电池','батарея':'电池','сеть':'电网','Сеть':'电网',
      'Напряжения ячеек':'电芯电压','Напряжения ячеек, V':'电芯电压, V','Температуры':'温度','Температуры T1–T4 (батарея, силовые ключи, плата), °C':'温度 T1–T4（电池、功率开关、板卡），°C',
      'Разброс ячеек (max−min), V':'电芯压差 (max−min), V','Мощностные ключи':'功率开关','Заряд (SOC)':'电量 (SOC)','Напряжение пакета':'电池组电压',
      'Ток':'电流','Мощность':'功率','Ёмкость':'容量','Напряжение батареи, V':'电池电压, V','Ток, A':'电流, A','Мощность, W':'功率, W',
      'сред.':'均','В/яч':'V/芯','А·ч':'Ah','максимальное':'最大','минимальное':'最小','остальные':'其他','разброс':'压差','макс':'最大','мин':'最小','Датчик':'传感器','Ячейка':'电芯','ячеек':'电芯','ячейка':'电芯',
      'Диапазон:':'区间：','Зум: Ctrl+колесо / drag–панорама':'缩放：Ctrl+滚轮 / 拖动平移',
      'ЛКМ по чипу — вкл/выкл линию · двойной ЛКМ — только эта линия · «Все» — показать все':'点击标签切换线条 · 双击仅显示该线 · “全部”显示全部',
      'ЛКМ по чипу — вкл/выкл · двойной ЛКМ — только этот показатель · «Все» — показать все':'点击标签切换 · 双击仅显示该指标 · “全部”显示全部',
      'остаток':'剩余','из':'共',
      'T1–T6 — NTC-датчики температуры. Производитель не публикует точное соответствие каналов и мест; подписи реконструированы по даташиту AFE (2 внешних датчика + датчик платы), мануалу ANT (до 4 внешних датчиков) и параметрам защит (температура батареи / силовых ключей). В нашей установке все каналы показывают одинаковую температуру (батарея и плата в одном корпусе).':'T1–T6 为 NTC 温度传感器。厂家未公布通道与位置的精确对应；标签根据 AFE 数据手册（2 个外部传感器 + 板载传感器）、ANT 手册（最多 4 个外部传感器）与保护参数（电池/功率开关温度）重建。本安装中各通道温度相同（电池与板卡同一外壳）。',
      'Временные ряды за выбранный период; статистика счётчика «день/ночь» за последние дни':'所选区间的时间序列；近期电表“白天/夜间”统计',
      'Напряжения сети и батареи (МАП), V + напряжение счётчика/CE308':'电网与电池（MAP）电压, V + 电表/CE308 电压',
      'Напряжение счётчика':'电表电压',
      'Мощности сети и батареи (МАП), W + активная мощность счётчика/CE308':'电网与电池（MAP）功率, W + 电表/CE308 有功功率',
      'Инверторы Дома (Σ)':'住宅逆变器 (Σ)','Мощность счётчика (активная)':'电表功率（有功）',
      'Суммарная активная мощность, W':'总有功功率, W','Активная мощность по инверторам, W':'各逆变器有功功率, W',
      'Левый':'左','Правый':'右','Средний':'中','Корпус':'机壳','Транзисторы':'晶体管','Тор':'环形',
      'Счётчик DDS238: потребление/отдача по тарифам «День»/«Ночь»':'电表 DDS238：按“白天/夜间”费率用电/上网',
      'Потребление / Отдача по тарифу «День» и «Ночь» по дням, kWh':'按“白天/夜间”费率每日用电/上网, kWh',
      'Потребление / Отдача по тарифу «День» и «Ночь» по месяцам, kWh':'按“白天/夜间”费率每月用电/上网, kWh',
      'Нет финализированных дней за выбранный период':'所选区间没有已结算的日期',
      'Показано дней:':'显示天数：','по месяцам':'按月份','Потребление / Отдача':'用电 / 上网',
      'Свинцово-кислотный АКБ':'铅酸电池',
      'AGM = батарея МАП − Σ (ANT×2 + EnBMS) · обновляется раз в минуту':'AGM = MAP 电池 − Σ (ANT×2 + EnBMS) · 每分钟更新',
      'Графики (расчёт: МАП − Σ BMS)':'图表（计算：MAP − Σ BMS）',
      'Графики (Redis — сырые показания, PG — 5-мин средние)':'图表（Redis—原始值，PG—5分钟均值）',
      'Знак':'符号','Балансировка':'均衡'
    }
  };

  // Дополнительные фразы/словоформы (после основной таблицы; перекрывают дубли).
  Object.assign(DICT.en,{
    'расчёт: МАП − Σ BMS (ANT×2 + EnBMS) · разряд положительный':'calc: MAP − Σ BMS (ANT×2 + EnBMS) · discharge positive',
    'разряд положительный':'discharge positive','актуально':'as of','актуально:':'as of:','В':'V','и':'and','активная':'active',
    'тарифу':'tariff','дням':'days','по тарифу':'by tariff','по дням':'by days','по месяцам':'by months','расчёт':'calc',
    'Напряжение сети и батареи (МАП), V + напряжение счётчика/CE308':'Grid and battery (MAP) voltage, V + meter/CE308 voltage',
    'счётчика':'meter','счётчика/CE308':'meter/CE308','напряжение счётчика':'meter voltage'
  });
  Object.assign(DICT.zh,{
    'расчёт: МАП − Σ BMS (ANT×2 + EnBMS) · разряд положительный':'计算：MAP − Σ BMS (ANT×2 + EnBMS) · 放电为正',
    'разряд положительный':'放电为正','актуально':'更新于','актуально:':'更新于：','В':'V','и':'和','активная':'有功',
    'тарифу':'费率','дням':'日','по тарифу':'按费率','по дням':'按日','по месяцам':'按月','расчёт':'计算',
    'Напряжение сети и батареи (МАП), V + напряжение счётчика/CE308':'电网与电池（MAP）电压, V + 电表/CE308 电压',
    'счётчика':'电表','счётчика/CE308':'电表/CE308','напряжение счётчика':'电表电压'
  });

  Object.assign(DICT.en,{
    'Батарея 1':'Battery 1','Батарея 2':'Battery 2','Силовая плата':'Power board','Плата управления':'Control board','Резерв':'Reserve',
    'Заряд':'Charge','Разряд':'Discharge','Балансировка':'Balancing','ВКЛ':'ON','ВЫКЛ':'OFF',
    'Счётчик':'Meter','Инвертор':'Inverter','инверторов':'inverters','онлайн':'online','Солнце':'Solar','порт':'port','Порт':'Port','Порт:':'Port:','кадров':'frames'
  });
  Object.assign(DICT.zh,{
    'Батарея 1':'电池 1','Батарея 2':'电池 2','Силовая плата':'功率板','Плата управления':'控制板','Резерв':'备用',
    'Заряд':'充电','Разряд':'放电','Балансировка':'均衡','ВКЛ':'开','ВЫКЛ':'关',
    'Счётчик':'电表','Инвертор':'逆变器','инверторов':'逆变器','онлайн':'在线','Солнце':'太阳能','порт':'端口','Порт':'端口','Порт:':'端口：','кадров':'帧'
  });


  Object.assign(DICT.en,{
    'MPPT-контроллеры':'MPPT controllers','Сетевые инверторы':'Grid inverters','BMS не найдены (или опрос отключён)':'No BMS found (or polling disabled)',
    'Активная мощность':'Active power','Актуально':'As of','Серийный номер инвертора':'Inverter serial number','Серийный номер логгера':'Logger serial number',
    'Выработка всего':'Total generation','Выработка сегодня':'Generation today','год':'year','д':'d','ч':'h','мин':'min','с назад':'ago',
    'Данные не обновляются':'Data not updating','Общая':'Total','Коэффициент мощности':'Power factor','Частота':'Frequency','счётчик кадров':'frame counter','Свинцово-кислотный АКБ (AGM) — графики тока и мощности':'Lead-acid (AGM) battery — current and power charts',
    'Напряжение':'Voltage','Реактивная':'Reactive','Реактивная мощность':'Reactive power','тока':'current','мощности':'power',
    'инвертора':'inverters','инверторов':'inverters','Суммарная':'Total','Коэффициент':'Coefficient','5 мин':'5 min'
  });
  Object.assign(DICT.zh,{
    'MPPT-контроллеры':'MPPT 控制器','Сетевые инверторы':'并网逆变器','BMS не найдены (или опрос отключён)':'未找到 BMS（或轮询已关闭）',
    'Активная мощность':'有功功率','Актуально':'更新于','Серийный номер инвертора':'逆变器序列号','Серийный номер логгера':'记录器序列号',
    'Выработка всего':'总发电量','Выработка сегодня':'今日发电量','год':'年','д':'天','ч':'时','мин':'分','с назад':'前',
    'Данные не обновляются':'数据未更新','Общая':'总计','Коэффициент мощности':'功率因数','Частота':'频率','счётчик кадров':'帧计数','Свинцово-кислотный АКБ (AGM) — графики тока и мощности':'铅酸电池 (AGM) — 电流与功率图表',
    'Напряжение':'电压','Реактивная':'无功','Реактивная мощность':'无功功率','тока':'电流','мощности':'功率',
    'инвертора':'逆变器','инверторов':'逆变器','Суммарная':'合计','Коэффициент':'系数','5 мин':'5 分钟'
  });
  var ORIG=new WeakMap(), ATTR_ORIG=new WeakMap(), LAST=new WeakMap();
  function esc(s){ return s.replace(/[.*+?^${}()|[\]\\]/g,'\\$&'); }
  var comp={};
  function compiled(){
    if(comp[LANG]) return comp[LANG];
    var d=DICT[LANG]||{}, keys=Object.keys(d).sort(function(a,b){return b.length-a.length;});
    // Границы кириллических слов БЕЗ lookbehind (совместимость): захватываем
    // предыдущий символ ((^|не-кириллица)) и используем lookahead.
    comp[LANG]=keys.map(function(k){
      var re=new RegExp('(^|[^\\u0400-\\u04FF])('+esc(k)+')(?![\\u0400-\\u04FF])','gi');
      var v=d[k];
      return { re:re, v:v };
    });
    return comp[LANG];
  }
  function t(s){
    if(s==null||s==='') return s;
    if(LANG==='ru') return s;
    var d=DICT[LANG]; if(!d) return s;
    if(Object.prototype.hasOwnProperty.call(d,s)) return d[s];
    if(!/[\u0400-\u04FF]/.test(s)) return s;
    var res=s, list=compiled();
    for(var i=0;i<list.length;i++){
      var it=list[i];
      res=res.replace(it.re, function(m,p1){ return p1 + it.v; });
    }
    return res;
  }
  function translateTextNode(node){
    var cur=node.nodeValue; if(cur==null) return;
    var last=LAST.get(node);
    if(last!=null && cur!==last){ ORIG.delete(node); } // содержимое изменил код страницы
    if(!ORIG.has(node)){
      if(!/[\u0400-\u04FF]/.test(cur)){ LAST.set(node,cur); return; }
      ORIG.set(node,cur);
    }
    var src=ORIG.get(node), out=(LANG==='ru')?src:t(src);
    if(node.nodeValue!==out) node.nodeValue=out;
    LAST.set(node,out);
  }
  function translateAttr(el,attr){
    if(!el.getAttribute) return; var cur=el.getAttribute(attr); if(cur==null) return;
    var map=ATTR_ORIG.get(el); if(!map){ map={}; ATTR_ORIG.set(el,map); }
    if(!(attr in map)){ if(!/[\u0400-\u04FF]/.test(cur)) return; map[attr]=cur; }
    var src=map[attr], out=(LANG==='ru')?src:t(src);
    if(el.getAttribute(attr)!==out) el.setAttribute(attr,out);
  }
  function trAttrs(el){ if(!el||el.nodeType!==1) return; ['title','placeholder','aria-label'].forEach(function(a){ if(el.hasAttribute&&el.hasAttribute(a)) translateAttr(el,a); }); }
  function walk(root){
    if(!root) return;
    if(root.nodeType===3){ translateTextNode(root); return; }
    if(root.nodeType!==1) return;
    if(root.tagName==='SCRIPT'||root.tagName==='STYLE') return;
    trAttrs(root);
    var w=document.createTreeWalker(root, NodeFilter.SHOW_TEXT|NodeFilter.SHOW_ELEMENT, null), n;
    while((n=w.nextNode())){ if(n.nodeType===3) translateTextNode(n); else trAttrs(n); }
  }
  function translateCharts(){
    try{
      var Ch=window.Chart; if(!Ch) return;
      var inst=Ch.instances||(Ch.registry&&Ch.registry.instances&&Ch.registry.instances.items)||{};
      var list=[]; if(inst&&typeof inst.forEach==='function'){ inst.forEach(function(c){ list.push(c); }); } else { for(var k in inst){ if(inst[k]&&inst[k].data) list.push(inst[k]); } }
      list.forEach(function(c){
        if(!c||!c.data||!c.data.datasets) return;
        var changed=false;
        c.data.datasets.forEach(function(ds){
          if(ds&&typeof ds.label==='string'){
            if(!('_ruLabel' in ds)){ if(!/[\u0400-\u04FF]/.test(ds.label)) return; ds._ruLabel=ds.label; }
            var out=(LANG==='ru')?ds._ruLabel:t(ds._ruLabel);
            if(ds.label!==out){ ds.label=out; changed=true; }
          }
        });
        if(changed){ try{ c.update('none'); }catch(e){} }
      });
    }catch(e){}
  }
  function apply(){
    document.documentElement.setAttribute('lang',LANG);
    try{
      if(!('__ruTitle' in window)) window.__ruTitle=document.title;
      document.title=(LANG==='ru')?window.__ruTitle:t(window.__ruTitle);
    }catch(e){}
    walk(document.body||document.documentElement);
    translateCharts();
    var sel=document.getElementById('langSelect'); if(sel) sel.value=LANG;
  }
  var obs=null;
  function startObserver(){
    if(obs) return;
    obs=new MutationObserver(function(muts){
      if(LANG==='ru') return;
      for(var i=0;i<muts.length;i++){
        var m=muts[i];
        if(m.type==='characterData'){ translateTextNode(m.target); continue; }
        if(m.addedNodes){ for(var j=0;j<m.addedNodes.length;j++){ var nd=m.addedNodes[j]; if(nd.nodeType===1) walk(nd); else if(nd.nodeType===3) translateTextNode(nd); } }
        if(m.type==='attributes'&&m.target&&m.target.nodeType===1) trAttrs(m.target);
      }
    });
    obs.observe(document.documentElement,{childList:true,subtree:true,characterData:true,attributes:true,attributeFilter:['title','placeholder','aria-label']});
  }
  function buildSwitcher(){
    var nav=document.querySelector('.top-nav')||document.body;
    var wrap=document.createElement('div'); wrap.className='lang-switch';
    var sel=document.createElement('select'); sel.id='langSelect'; sel.title='Language / Язык / 语言';
    LANGS.forEach(function(p){ var o=document.createElement('option'); o.value=p[0]; o.textContent=p[1]; sel.appendChild(o); });
    sel.value=LANG;
    sel.addEventListener('change',function(){ LANG=sel.value; try{ localStorage.setItem('lang',LANG); }catch(e){} comp={}; apply(); });
    wrap.appendChild(sel); nav.appendChild(wrap);
  }
  window.SR_i18n={ get lang(){return LANG;}, t:t, apply:apply, set:function(l){ LANG=l; try{localStorage.setItem('lang',l);}catch(e){} comp={}; apply(); } };
  function init(){ buildSwitcher(); apply(); startObserver(); setInterval(translateCharts,3000); }
  if(document.readyState==='loading') document.addEventListener('DOMContentLoaded',init); else init();
})();

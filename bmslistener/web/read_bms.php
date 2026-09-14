<?php
// read_bms.php — ANT BMS (батарея, пассивный слушатель).
// Источник данных — C-демон bmslistener (/usr/sbin/bmslistener,
// bmslistener.service): пассивно слушает USB-serial адаптеры, на которых
// ANT BMS вещают 140-байтные live-кадры (19200 8N1), и публикует коллекцию
// активных адаптеров в System V shared memory (ключ 2018, 32 КБ) в формате
// {"updated":<epoch>,"devices":[{...},...]} + терминатор "#EOF"
// (конвенция экосистемы mapd/mpptd).
//
// Новый отдельный скрипт (аналог read_json.php, который НЕ модифицируется):
// читает shm 2018 целиком и отдаёт готовый JSON. Доступ — та же
// Basic-авторизация, что и на остальном веб-интерфейсе ПАК «Малина».

$shm=shmop_open(2018,"a",0,0);
if ($shm===false) { echo "{\"updated\":0,\"devices\":[]}"; die(); }
$str_json=shmop_read($shm,0,32768);
shmop_close($shm);
$pos=strpos($str_json,"#EOF");
if ($pos===false || $pos<=0) { echo "{\"updated\":0,\"devices\":[]}"; die(); }
echo substr($str_json,0,$pos);
die();
?>

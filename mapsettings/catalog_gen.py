#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""Генератор каталога ячеек МАП (SVEN POWER MANAGER II).

Модуль map-settings: читает документ ``protocol_MAP_cells_2026_07_15.doc``
(единственный источник истины), парсит описание ячеек RAM/EEPROM и формирует
машиночитаемый каталог ``catalog.json``.

Запуск (из корня репозитория):

    python3 mapsettings/catalog_gen.py

Запуск с явными путями:

    python3 mapsettings/catalog_gen.py \
        --doc  docs/map/map/protocol_MAP_cells_2026_07_15.doc \
        --txt  /tmp/kilo/mapdoc/protocol_MAP_cells_2026_07_15.txt \
        --html /tmp/kilo/mapdoc/html/protocol_MAP_cells_2026_07_15.html \
        --out  mapsettings/catalog.json

Допущения и правила парсинга
----------------------------
1. Источник — только ``protocol_MAP_cells_2026_07_15.doc``. Текстовый и HTML
   варианты являются производными этого документа. Если производные файлы
   отсутствуют, скрипт сам вызывает ``libreoffice --headless --convert-to``
   (txt:Text и html) во временный каталог. Результат детерминирован при
   одинаковой версии LibreOffice.
2. Определением ячейки считается строка, начинающаяся (после отступов) с
   ``_`` (или ``fl_``), в "голове" которой встречается последовательность
   ``ИМЯ=0xАДРЕС``. Голова — это начало строки до первого описания; несколько
   ячеек в одной голове разделены запятыми/пробелами. Хвост строки после
   последней ячейки считается описанием.
3. Адреса могут содержать кириллическую ``х`` — она нормализуется в ``x``.
   Диапазон ``0x588-0x589`` означает 2 байта (width=2); более длинные
   диапазоны (3-4 байта) для соблюдения схемы (width только 1 или 2)
   приводятся к width=2. Для width=2 обязательно поле ``order``:
   ``"lh"`` — младший байт по младшему адресу, ``"hl"`` — старший по младшему.
   Пары LOW/HIGH (суффиксы ``_L``/``_VL`` и ``_H``/``_VH``, в т.ч. с индексом
   ``[n]``) на СОСЕДНИХ адресах сливаются в один параметр с width=2;
   на несоседних — остаются двумя параметрами width=1.
4. Зелёный цвет в исходном .doc (атрибут ``color="#00b050"`` в HTML)
   помечает дополнительные ячейки модели Титанатор. Ячейка считается
   Titanator-only, если её СОБСТВЕННОЕ имя в определении выделено зелёным.
   Дополнительно учитываются явные пометки «пункт меню Титанатор»,
   «для модели Титанатор» и т.п. Ячейки с пометкой «отсутствует в модели
   TITANATOR» относятся только к Dominator. Все прочие — общие для
   Titanator и Dominator (модели Pro/Hybrid/Dominator совместимы).
5. ``access=rw`` — ячейка-настройка (можно менять), ``ro`` — измерение/
   состояние/служебное. EEPROM по умолчанию ``rw`` (кроме адресов 0x00-0x04),
   RAM по умолчанию ``ro`` (кроме командных/управляющих ячеек).
6. ``scale``/``offset``/``unit`` извлекаются из формул вида
   ``Uакб(В) = (UAcc_med_VH*256 + UAcc_med_VL)/10``: делитель в конце даёт
   scale=1/K, множитель — scale=K, свободный ``+N``/``-N`` — offset.
   Нелинейные формулы (например ``6250/TFNET``) дают scale=1.0 (дефолт).
   ``name`` — короткое русское имя (<=80 символов) из описания без формул.
   ``desc`` — исходный русский текст описания (хвост строки определения).
7. ``enum`` строится по строкам ``СИМВОЛ =значение - описание``; ``bits`` —
   по строкам ``БитN ... - описание``. Если в блоке есть битовые поля,
   enum для этого блока не строится.
8. Каждая ячейка получает ровно одну группу из фиксированного набора.
   Для ячеек внутри меню ЖКИ (EEPROM 0x138+) группа определяется пунктом
   меню; для остальных — эвристикой по имени/описанию.
"""

import argparse
import json
import os
import re
import shutil
import subprocess
import sys
import tempfile

# --------------------------------------------------------------------------
# Константы
# --------------------------------------------------------------------------

TITLE = "SVEN POWER MANAGER II — ячейки МАП"
SOURCE_NAME = "protocol_MAP_cells_2026_07_15.doc"

MODELS_BOTH = ["titanator", "dominator"]
MODELS_TITANATOR = ["titanator"]
MODELS_DOMINATOR = ["dominator"]

# Ячейки, для которых явная текстовая пометка «пункт меню Титанатор»
# не означает Titanator-only (присутствуют во всех моделях).
MODES_OVERRIDE = {
    "_Language": MODELS_BOTH,
    "_LCD_NetDizel": MODELS_BOTH,
}

# Осмысленные русские имена для ячеек, у которых в документе нет короткого
# описания (только перечисления, формулы, диапазоны адресов, многоточия).
# Ключ — имя ячейки; для слитых пар достаточно имени того байта, который
# остаётся в каталоге (младший адрес).
NAME_OVERRIDE = {
    "_SerialNum2": "Зарезервированный байт серийного номера",
    "_No3FazeMotor": "Коррекция синусоид 3 фаз для асинхронных моторов",
    "_DigitInFunc1": "Назначение цифрового входа 1",
    "_DigitInFunc2": "Назначение цифрового входа 2",
    "_Grid_52Hz": "Управление сетевым солнечным инвертором",
    "_T_CHARGE_NEED_ECO": "Задержка перехода на сеть в ЭКО режиме",
    "_T_CHARGE_UmaxECO_diz": "Максимальное время дозаряда от генератора/MPPT",
    "_RSErrJobM": "Ошибки (перегрузки) с автовозвратом в рабочий режим",
    "_RSErrJob": "Ошибки (перегрузки) с автовозвратом в рабочий режим",
    "_Status_RELEdop": "Состояние управления доп. реле",
    "_ReleIzbNum": "Очередность избытка мощности для реле",
    "_LCD_Rele3_Funk": "Функция реле 3",
    "_LCD_Rele3_L1": "Порог 1 реле 3",
    "_LCD_Rele3_L2": "Порог 2 реле 3",
    "_LCD_Rele3_Gist": "Гистерезис и инверсия реле 3",
    "_LCD_ReleII_Funk": "Функция реле II входа сети",
    "_LCD_ReleII_L1": "Порог 1 реле II входа сети",
    "_LCD_ReleII_L2": "Порог 2 реле II входа сети",
    "_LCD_ReleII_Gist": "Гистерезис и инверсия реле II входа сети",
    "_LCD_DevAdr": "Адрес МАП (ModBus)",
    "_LCD_RS_Protocol": "Протокол RS1 (USB)",
    "_LCD_AddDevice": "Устройства на доп. плате RS485",
    "_LCD_RSBAUD485": "Скорость RS3 (RS485)",
    "_LCD_RS485_Num": "Количество устройств RS3 по ModBus",
    "_LCD_RasberyON": "Малина (Raspberry) включена",
    "_UakbCell_L[0]": "Напряжение банок BMS",
    "_UakbCell_H[0]": "Напряжение банок BMS",
    "_UakbCell_L[31]": "Напряжение банок BMS",
    "_UakbCell_H[31]": "Напряжение банок BMS",
    "_TakbCell[0]": "Температура банок BMS",
    "_TakbCell[31]": "Температура банок BMS",
    "_Q_Cell[0]": "Ток разряда банок BMS",
    "_Q_Cell[31]": "Ток разряда банок BMS",
    "_I_Akb_MPPT_L[0]": "Ток заряда параллельных MPPT",
    "_I_Akb_MPPT_H[0]": "Ток заряда параллельных MPPT",
    "_I_Akb_MPPT_L[15]": "Ток заряда параллельных MPPT",
    "_I_Akb_MPPT_H[15]": "Ток заряда параллельных MPPT",
    "_Takb_MPPT[0]": "Температура параллельных MPPT",
    "_Takb_MPPT[15]": "Температура параллельных MPPT",
    "_BMS": "Состояние BMS (сухие контакты)",
    "_PNET_Sign_P": "Знак мощности сети",
    "_PNET_8": "Текущая мощность сети (киловатты ×8)",
    "_PLoad_8": "Текущая мощность нагрузки (киловатты ×8)",
    "_UAcc_Optim_VL": "Оптимальное среднее напряжение АКБ",
    "_UAccMinNetGen_S": "Реальное напряжение ЭКО с коррекцией",
    "_LCD_UAccChMax_S_Temp": "Напряжение окончания заряда с коррекцией (×10)",
    "_LCD_UAccChBUF_S_Temp": "Напряжение поддержания заряда с коррекцией (×10)",
    "_M_POWhourNET_L": "Статистика потребления от сети",
    "_M_POWhourNET_HH": "Старший байт статистики потребления от сети",
    "_M_POWhourMAP_L": "Статистика потребления от АКБ",
    "_M_POWhourMAP_HH": "Старший байт статистики потребления от АКБ",
    "_M_POWhourMAPCharge_L": "Статистика потребления от сети на заряд",
    "_M_POWhourMAPCharge_HH": "Старший байт статистики потребления на заряд",
    "_M_POWhourNET_sign": "Знаковый счётчик энергии от сети",
    "_T_AccDisch_L": "Обратный отсчёт работы на низком напряжении АКБ (устаревшая)",
    "_T_AccDisch": "Обратный отсчёт работы на низком напряжении АКБ",
}

# Регулярное выражение заведомо «пустого» описания.
PLACEHOLDER_DESC_RE = re.compile(r"^[\s.,…·\-–—]*$")

GROUP_GENERATION = "Выход / инвертор"
GROUP_BMS_MPPT = "Реле и внешние устройства"
GROUP_NET = "Сеть"
GROUP_CHARGE = "Заряд АКБ (настройки)"
GROUP_RELE = "Реле и внешние устройства"
GROUP_OPTIONS = "ЖКИ и меню (настройки)"

# Диапазоны строк (1-based, по *.txt) для пунктов меню ЖКИ.
MENU_RANGES = [
    (1008, 1076, GROUP_GENERATION),   # Меню "Генерация МАП"
    (1077, 1122, GROUP_BMS_MPPT),     # Меню "Б/Диз.Генер./BMS MPPT"
    (1123, 1175, GROUP_NET),          # Меню "Сеть/ЭнергЭконом"
    (1176, 1252, GROUP_CHARGE),       # Меню "Параметры АКБ При Заряде"
    (1253, 1355, GROUP_RELE),         # Меню "Дополнит. РЕЛЕ"
    (1356, 1417, GROUP_OPTIONS),      # Меню "Другие Опции"
]

# Явные группы для ячеек вне меню ЖКИ (RAM и начальный EEPROM).
EXPLICIT_GROUP = {
    # --- Состояние МАП / флаги / режим ---
    "_MODE": "Состояние МАП",
    "_StatusCh": "Состояние МАП",
    "_put_eeprom": "Состояние МАП",
    "_StateUAcc": "Состояние МАП",
    "_F_AccOver": "Ошибки и предупреждения",
    "_F_NETOver": "Ошибки и предупреждения",
    "_flagUnet2": "Сеть",
    "_flag_NETON_ECO": "Сеть",
    "_Pmax_On": "Сеть",
    "_PmaxGenDisch": "Сеть",
    "_Status_RELEdop": "Реле и внешние устройства",
    # --- Сеть ---
    "_UNET": "Сеть",
    "_INET": "Сеть",
    "_INET_16_4": "Сеть",
    "_PNET_L": "Сеть",
    "_PNET_H": "Сеть",
    "_PNET_8": "Сеть",
    "_PNET_Sign_P": "Сеть",
    "_TFNET": "Сеть",
    "_TFNET_Limit": "Сеть",
    "_UNET_Limit": "Сеть",
    "_ThFMAP": "Выход / инвертор",
    "_ThFMAPSync": "Выход / инвертор",
    "_UOUTmed": "Выход / инвертор",
    "_LCD_UNET2Up": "Сеть",
    "_LCD_UNET2Down": "Сеть",
    "_UAccMinNetGen_S": "Сеть",
    "_TFT_UNET2Up": "Сеть",
    "_TFT_UNET2Down": "Сеть",
    "_NetUpECO_net2_off": "Сеть",
    # --- Батарея / BMS ---
    "_UAcc_med_VH": "Батарея (АКБ)",
    "_UAcc_med_VL": "Батарея (АКБ)",
    "_IAcc_med_A_2": "Батарея (АКБ)",
    "_IAcc_med_A_u16_L": "Батарея (АКБ)",
    "_IAcc_med_A_u16_H": "Батарея (АКБ)",
    "_UAcc_Optim_VL": "Батарея (АКБ)",
    "_UAcc_Optim_VH": "Батарея (АКБ)",
    "_UAcc_med_VS_100L": "Батарея (АКБ)",
    "_UAcc_med_VS_100H": "Батарея (АКБ)",
    "_I_Akb_MAP_faz1L": "Батарея (АКБ)",
    "_I_Akb_MAP_faz1H": "Батарея (АКБ)",
    "_I_Akb_MAP_faz2L": "Батарея (АКБ)",
    "_I_Akb_MAP_faz2H": "Батарея (АКБ)",
    "_I_Akb_MAP_faz3L": "Батарея (АКБ)",
    "_I_Akb_MAP_faz3H": "Батарея (АКБ)",
    "_E_LCD_UAccChMax": "Батарея (АКБ)",
    "_E_LCD_UAccChBUF": "Батарея (АКБ)",
    "_LCD_UAccChMax_S_Temp": "Батарея (АКБ)",
    "_LCD_UAccChBUF_S_Temp": "Батарея (АКБ)",
    "_fl_UAccChBUF_24h": "Батарея (АКБ)",
    "fl_UAccChBUF_24h": "Батарея (АКБ)",
    "_LCD_UAccChMax": "Заряд АКБ (настройки)",
    "_LCD_UAccChBUF": "Заряд АКБ (настройки)",
    "_LCD_UAccChStart": "Заряд АКБ (настройки)",
    "_LCD_UAccMin": "Заряд АКБ (настройки)",
    "_UAccAfterDisChage": "Заряд АКБ (настройки)",
    "_FrozenUAccAfterDisChage": "Заряд АКБ (настройки)",
    "_BMS": "Батарея (АКБ)",
    "_BMS_Num": "Батарея (АКБ)",
    "_TempTopToBMS": "Реле и внешние устройства",
    "_LCD_BMSFunk": "Реле и внешние устройства",
    "_LCD_MPPTNum": "Реле и внешние устройства",
    "_MPPT_toCh": "Реле и внешние устройства",
    "_UakbCell_L": "Батарея (АКБ)",
    "_UakbCell_H": "Батарея (АКБ)",
    "_Q_Cell": "Батарея (АКБ)",
    "_I_Akb_MPPT_L": "Батарея (АКБ)",
    "_I_Akb_MPPT_H": "Батарея (АКБ)",
    "_SoC": "Батарея (АКБ)",
    "_SoH": "Батарея (АКБ)",
    "_MASK_DEV_ON": "Реле и внешние устройства",
    # --- Нагрузка / мощность ---
    "_PLoad_L": "Нагрузка и мощность",
    "_PLoad_H": "Нагрузка и мощность",
    "_PLoad_8": "Нагрузка и мощность",
    "_PowAccNom": "Нагрузка и мощность",
    "_LCD_NetMaxPow": "Сеть",
    "_LCD_NetMaxPow_dop": "Сеть",
    # --- Температуры ---
    "_Temp_Grad0": "Температуры",
    "_Temp_Grad1": "Температуры",
    "_Temp_Grad2": "Температуры",
    "_Temp_off": "Температуры",
    "_TakbCell": "Температуры",
    "_Takb_MPPT": "Температуры",
    "_KorrTempAkb": "Температуры",
    "_detTforDopRele": "Реле и внешние устройства",
    # --- Ошибки / сброс ---
    "_RSErrSis": "Ошибки и предупреждения",
    "_RSErrJobM": "Ошибки и предупреждения",
    "_RSErrJob": "Ошибки и предупреждения",
    "_RSErrDop": "Ошибки и предупреждения",
    "_RSWarning": "Ошибки и предупреждения",
    "_RCON_img": "Ошибки и предупреждения",
    "_RCON_img_L": "Ошибки и предупреждения",
    "_RCON_img_H": "Ошибки и предупреждения",
    # --- Энергия / счётчики ---
    "_M_POWhourNET_L": "Энергия и счётчики",
    "_M_POWhourNET_H": "Энергия и счётчики",
    "_M_POWhourNET_HH": "Энергия и счётчики",
    "_M_POWhourMAP_L": "Энергия и счётчики",
    "_M_POWhourMAP_H": "Энергия и счётчики",
    "_M_POWhourMAP_HH": "Энергия и счётчики",
    "_M_POWhourMAPCharge_L": "Энергия и счётчики",
    "_M_POWhourMAPCharge_H": "Энергия и счётчики",
    "_M_POWhourMAPCharge_HH": "Энергия и счётчики",
    "_M_POWhourNET_sign": "Энергия и счётчики",
    "_T_Nominal": "Энергия и счётчики",
    "_T_Overload": "Энергия и счётчики",
    "_TOff_Overload": "Энергия и счётчики",
    "_TdizProfilact_day_cnt": "Энергия и счётчики",
    "_TchEco_Ch": "Энергия и счётчики",
    # --- Время и часы ---
    "_WATCH": "Время и часы",
    "_WATCH2": "Время и часы",
    "_TimeCyr_MINUT": "Время и часы",
    "_LCD_TimeCyr": "Время и часы",
    "_LCD_TimeEcoStart": "Время и часы",
    "_LCD_TimeEcoEnd": "Время и часы",
    "_T_NetOn": "Время и часы",
    "_T_Charge": "Время и часы",
    "_T_AccDisch_L": "Время и часы",
    "_T_AccDisch": "Время и часы",
    "_T_CHARGE_NEED_ECO": "Время и часы",
    "_T_ACCDISCH": "Время и часы",
    "_T_AccDisch_ExtH": "Время и часы",
    "_T_CHARGE": "Время и часы",
    "_T_CHARGE_Umax": "Время и часы",
    "_T_CHARGE_UmaxECO_diz": "Время и часы",
    "_T_CHARGE_NEED_ECO": "Время и часы",
    "_T_ButCh_r": "Время и часы",
    "_T_Diz_prof": "Время и часы",
    "_T_MaxLiIonBMS": "Время и часы",
    "_TdizProfilact_day": "Время и часы",
    "_TchEco_14day": "Время и часы",
    "_TCh_MPPT_GRID52": "Время и часы",
    "_T_NET_ON": "Время и часы",
    "_T_NOMINAL": "Время и часы",
    # --- Заряд АКБ (настройки) ---
    "_TFT_CIChargeSOC95": "Заряд АКБ (настройки)",
    "_TFT_CIChStartMPPT": "Заряд АКБ (настройки)",
    "_TFT_CIChEndMPPT": "Заряд АКБ (настройки)",
    "_CIChargeAbsorb": "Заряд АКБ (настройки)",
    "_del_UAccChBUF_24h": "Заряд АКБ (настройки)",
    "_DelUChargeEnd": "Заряд АКБ (настройки)",
    "_LCD_CAcc": "Заряд АКБ (настройки)",
    "_LCD_CAcc_dop": "Заряд АКБ (настройки)",
    "_LCD_CIChargeStart": "Заряд АКБ (настройки)",
    "_LCD_CIChargeEnd": "Заряд АКБ (настройки)",
    "_LCD_ChargeAlg": "Заряд АКБ (настройки)",
    "_LCD_AccType": "Заряд АКБ (настройки)",
    "_LCD_UAccChMax_dop": "Заряд АКБ (настройки)",
    "_LCD_UAccChBUF_dop": "Заряд АКБ (настройки)",
    "_LCD_UAccChStart_dop": "Заряд АКБ (настройки)",
    "_LCD_UAccMin_dop": "Заряд АКБ (настройки)",
    "_LCD_UAccMinNetGen_dop": "Заряд АКБ (настройки)",
    "_TFT_SOC_DisCharge": "Заряд АКБ (настройки)",
    "_TFT_SOC_StartCharge": "Заряд АКБ (настройки)",
    "_TFT_SOC_DizStart": "Заряд АКБ (настройки)",
    "_LCD_UAccDizStart": "Реле и внешние устройства",
    "_LCD_UAccDizStart_dop": "Реле и внешние устройства",
    # --- Реле / внешние устройства ---
    "_AVR_On": "Реле и внешние устройства",
    "_Grid_52Hz": "Реле и внешние устройства",
    "_DigitInFunc1": "Реле и внешние устройства",
    "_DigitInFunc2": "Реле и внешние устройства",
    "_ReleNetNum": "Реле и внешние устройства",
    "_ReleIzbNum": "Реле и внешние устройства",
    "_PNET_Prodag_MAX": "Реле и внешние устройства",
    "_CAP_OnOff": "Реле и внешние устройства",
    "_P_ModemVdd": "Реле и внешние устройства",
    "_RSBAUD_Rasb_Load": "Реле и внешние устройства",
    "_NUMCULLER": "Реле и внешние устройства",
    "_NUM_Overload": "Реле и внешние устройства",
    # --- Сервис / идентификация ---
    "_Device": "Сервис и идентификация",
    "_VerPow": "Сервис и идентификация",
    "_VerPO": "Сервис и идентификация",
    "_DevOpt": "Сервис и идентификация",
    "_RAM_END_L": "Сервис и идентификация",
    "_RAM_END_H": "Сервис и идентификация",
    "_Language": "Сервис и идентификация",
    "_VerPlatPic": "Сервис и идентификация",
    "_VerPlatNet": "Сервис и идентификация",
    "_VerPlatPowDop": "Сервис и идентификация",
    "_VerTest": "Сервис и идентификация",
    "_SerialNum0": "Сервис и идентификация",
    "_SerialNum1": "Сервис и идентификация",
    "_SerialNum2": "Сервис и идентификация",
    "_SerialNum3": "Сервис и идентификация",
    "_SyncDiz_Plat": "Сервис и идентификация",
    "_No3FazeMotor": "Сервис и идентификация",
    "_fUAcc_Korr": "Сервис и идентификация",
    "_POW_Korr": "Сервис и идентификация",
    "_I_Chage_Korr": "Сервис и идентификация",
    "_POW": "Сервис и идентификация",
    "_UACC": "Сервис и идентификация",
    # --- ЖКИ / меню ---
    "_LCD_TypeSin": "Выход / инвертор",
    "_LCD_MAP_iFaze": "Выход / инвертор",
    "_LCD_UMAP220_NEED": "Выход / инвертор",
    "_LCD_NetUpLoad": "Сеть",
    "_LCD_NetUpECO": "Сеть",
    "_LCD_SensLoad": "Нагрузка и мощность",
    "_LCD_MAP_Sync": "Выход / инвертор",
    "_LCD_NetDizel": "Сеть",
    "_LCD_Net2": "Сеть",
    "_LCD_DizelMaxPow": "Сеть",
    "_LCD_SlaveMAPNum": "Выход / инвертор",
    "_LCD_NETAlg": "Сеть",
    "_LCD_UNETUp": "Сеть",
    "_LCD_UNETDown": "Сеть",
    "_LCD_PercentECOMinGen": "Сеть",
    "_LCD_Vers": "ЖКИ и меню (настройки)",
    "_LCD_LCDType": "ЖКИ и меню (настройки)",
    "_LCD_Sound": "ЖКИ и меню (настройки)",
    "_LCD_SignalNetOff": "ЖКИ и меню (настройки)",
    "_LCD_RSBAUD": "ЖКИ и меню (настройки)",
    "_LCD_T_MAXCharge": "Заряд АКБ (настройки)",
    "_LCD_RasberyON": "ЖКИ и меню (настройки)",
    "_LCD_DevAdr": "ЖКИ и меню (настройки)",
    "_LCD_RS_Protocol": "ЖКИ и меню (настройки)",
    "_LCD_AddDevice": "ЖКИ и меню (настройки)",
    "_LCD_RSBAUD485": "ЖКИ и меню (настройки)",
    "_LCD_RS485_Num": "ЖКИ и меню (настройки)",
    "_LCD_Rele1_Funk": "Реле и внешние устройства",
    "_LCD_Rele1_U_T_min": "Реле и внешние устройства",
    "_LCD_Rele1_U_T_maxInv": "Реле и внешние устройства",
    "_LCD_Rele1_GistInv": "Реле и внешние устройства",
    "_LCD_Rele2_Funk": "Реле и внешние устройства",
    "_LCD_Rele2_U_T_min": "Реле и внешние устройства",
    "_LCD_Rele2_U_T_maxInv": "Реле и внешние устройства",
    "_LCD_Rele2_GistInv": "Реле и внешние устройства",
    "_LCD_Rele3_Funk": "Реле и внешние устройства",
    "_LCD_Rele3_L1": "Реле и внешние устройства",
    "_LCD_Rele3_L2": "Реле и внешние устройства",
    "_LCD_Rele3_Gist": "Реле и внешние устройства",
    "_LCD_ReleII_Funk": "Реле и внешние устройства",
    "_LCD_ReleII_L1": "Реле и внешние устройства",
    "_LCD_ReleII_L2": "Реле и внешние устройства",
    "_LCD_ReleII_Gist": "Реле и внешние устройства",
    "_minTarifAnCh": "Сеть",
    "_minTarifCh": "Сеть",
}

# Управляющие (доступные на запись) ячейки RAM.
RW_RAM = {
    "_put_eeprom",
    "_Status_RELEdop",
    "_MPPT_toCh",
    "_RCON_img",
    "_RCON_img_L",
    "_RCON_img_H",
}

# Единицы измерения, встречающиеся в формулах документа.
UNIT_MAP = {
    "В": "В",
    "А": "А",
    "A": "А",
    "Вт": "Вт",
    "Гц": "Гц",
    "град": "°C",
    "°C": "°C",
    "мин": "мин",
    "с": "с",
    "А·ч": "А·ч",
    "А.ч": "А·ч",
    "%": "%",
    "кВт.ч": "кВт·ч",
    "кВт·ч": "кВт·ч",
}

# --------------------------------------------------------------------------
# Разбор HTML: зелёный цвет = дополнительные ячейки Титанатор
# --------------------------------------------------------------------------


class _ParagraphParser:
    """Собирает абзацы HTML и флаги зелёного цвета по символам."""

    def __init__(self):
        from html.parser import HTMLParser

        class _Impl(HTMLParser):
            def __init__(self):
                super().__init__(convert_charrefs=True)
                self.paras = []
                self.cur = []
                self.green = []
                self.stack = []
                self.depth = 0

            def handle_starttag(self, tag, attrs):
                a = dict(attrs)
                if tag == "p":
                    self.depth += 1
                    self.cur = []
                    self.green = []
                if tag == "font":
                    self.stack.append(a.get("color"))
                if tag == "br":
                    self.cur.append(" ")
                    self.green.append(False)

            def handle_endtag(self, tag):
                if tag == "p" and self.depth > 0:
                    self.depth -= 1
                    self.paras.append(("".join(self.cur), list(self.green)))
                    self.cur = []
                    self.green = []
                if tag == "font" and self.stack:
                    self.stack.pop()

            def handle_data(self, data):
                color = None
                for c in reversed(self.stack):
                    if c:
                        color = c.lower()
                        break
                is_green = color == "#00b050"
                for ch in data:
                    self.cur.append(ch)
                    self.green.append(is_green)

        self._impl = _Impl()

    def feed(self, html_text):
        self._impl.feed(html_text)
        return self._impl.paras


TOKEN_RE = re.compile(
    r"(_?\s?[A-Za-z][A-Za-z0-9_]*(?:\s*\[\s*\d+\s*\])?)\s*=\s*=?\s*0?[xX]?\s*([0-9A-Fa-f]{1,4})"
)
GAP_RE = re.compile(r"^[\s,]*$")


def _norm_hex(line):
    return line.replace("х", "x").replace("Х", "X")


def extract_def_tokens(line, green_flags=None):
    """Возвращает список определений ячеек в "голове" строки.

    Каждый элемент: (raw_name, addr, name_start, name_end, token_end).
    Дополнительные ячейки принимаются только если текст между ними состоит
    из запятых/пробелов (это отсекает ссылки на ячейки внутри описаний).
    """
    t = _norm_hex(line)
    out = []
    prev_end = None
    for m in TOKEN_RE.finditer(t):
        raw = re.sub(r"\s+", "", m.group(1))
        if not (raw.startswith("_") or raw.startswith("fl_")):
            prev_end = m.end()
            continue
        if prev_end is not None and not GAP_RE.match(t[prev_end:m.start()]):
            break
        addr = int(m.group(2), 16)
        out.append((raw, addr, m.start(1), m.end(1), m.end()))
        prev_end = m.end()
    return out


def green_names_from_html(html_path):
    """Имена ячеек, для которых есть ТОЛЬКО зелёное определение.

    Возвращает множество ``green_only``: имя встречается в зелёном
    определении и ни разу — в незелёном. Это исключает общие ячейки,
    у которых наряду с обычным есть отдельное (зелёное) описание
    варианта Титанатор (например ``_LCD_Rele1_Funk``).
    """
    parser = _ParagraphParser()
    with open(html_path, encoding="utf-8", errors="replace") as fh:
        paragraphs = parser.feed(fh.read())
    green = set()
    nongreen = set()
    for text, flags in paragraphs:
        for raw, _addr, s, e, _tend in extract_def_tokens(text, flags):
            if e > s:
                frac = sum(flags[s:e]) / float(e - s)
                (green if frac >= 0.5 else nongreen).add(raw)
    return green - nongreen


# --------------------------------------------------------------------------
# Конвертация .doc при отсутствии производных файлов
# --------------------------------------------------------------------------


def _libreoffice_convert(doc_path, kind, outdir):
    """kind: 'txt' или 'html'. Возвращает путь к результату или None."""
    filt = "txt:Text" if kind == "txt" else "html"
    exe = shutil.which("libreoffice") or shutil.which("soffice")
    if not exe:
        return None
    try:
        subprocess.run(
            [exe, "--headless", "--convert-to", filt, "--outdir", outdir, doc_path],
            check=True,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
        )
    except Exception:
        return None
    base = os.path.splitext(os.path.basename(doc_path))[0]
    candidate = os.path.join(outdir, base + ("." + kind if kind == "txt" else ".html"))
    return candidate if os.path.exists(candidate) else None


def resolve_sources(doc_path, txt_path, html_path):
    """Возвращает (txt_path, html_path), при необходимости конвертируя .doc."""
    tmpdir = None
    if not (txt_path and os.path.exists(txt_path)) or not (
        html_path and os.path.exists(html_path)
    ):
        if not os.path.exists(doc_path):
            raise SystemExit("Не найден исходный документ: %s" % doc_path)
        tmpdir = tempfile.mkdtemp(prefix="mapsettings-")
    if not (txt_path and os.path.exists(txt_path)):
        txt_path = _libreoffice_convert(doc_path, "txt", tmpdir)
        if not txt_path:
            raise SystemExit("Не удалось сконвертировать .doc в txt (нужен libreoffice).")
    if not (html_path and os.path.exists(html_path)):
        html_path = _libreoffice_convert(doc_path, "html", tmpdir)
        if not html_path:
            raise SystemExit("Не удалось сконвертировать .doc в html (нужен libreoffice).")
    return txt_path, html_path


# --------------------------------------------------------------------------
# Разбор текста
# --------------------------------------------------------------------------

ZERO = "0"


def parse_txt(txt_path, green_names):
    """Возвращает список записей ячеек (ещё с дублями)."""
    with open(txt_path, encoding="utf-8", errors="replace") as fh:
        lines = fh.read().split("\n")

    records = []
    block = []          # строки после определения
    pending = []        # определения текущего блока: (raw, addr, tail, line_no)
    menu_group = None

    def flush():
        if not pending:
            return
        block_text = "\n".join(block)
        for raw, addr, tail, line_no in pending:
            records.append(
                build_record(raw, addr, tail, block_text, line_no, menu_group, green_names)
            )
        del pending[:]
        del block[:]

    for idx, raw_line in enumerate(lines):
        line_no = idx + 1
        stripped = raw_line.strip()
        group = menu_group_for(line_no)
        if not stripped:
            if pending:
                block.append("")
            continue
        if stripped.startswith("_") or stripped.startswith("fl_"):
            tokens = extract_def_tokens(raw_line)
            if tokens:
                tail = raw_line[tokens[-1][4]:]
                # Группы последовательных строк-определений без собственного
                # описания (например, массив ячеек 0x44D-0x455) делят один
                # общий блок описания, идущий ниже.
                merge = (
                    bool(pending)
                    and not any(b.strip() for b in block)
                    and (not clean_tail(tail) or not clean_tail(pending[-1][2]))
                )
                if not merge:
                    flush()
                menu_group = group
                for raw, addr, _s, _e, _te in tokens:
                    pending.append((raw, addr, tail, line_no))
                continue
        # заголовки меню/секций меняют контекст
        if stripped.startswith("Меню "):
            flush()
            menu_group = group
        if pending:
            block.append(raw_line)
    flush()
    return records


def menu_group_for(line_no):
    for lo, hi, grp in MENU_RANGES:
        if lo <= line_no <= hi:
            return grp
    return None


NAME_SEP_RE = re.compile(r"\s[–—-]\s")
CYR_RE = re.compile(r"[\u0400-\u04FF]")
RANGE_TAIL_RE = re.compile(r"^[-–—]?\s*0?[xX][0-9A-Fa-f]{1,4}\b")
BRACE_RE = re.compile(r"^\s*\{[^{}]*\}")


def clean_tail(tail):
    """Убирает ведущие значения по умолчанию и хвост диапазона адресов."""
    t = tail.strip()
    while True:
        before = t
        t = BRACE_RE.sub("", t, count=1).strip()
        t = RANGE_TAIL_RE.sub("", t).strip()
        t = re.sub(r"^[\s,;:\-–—]+", "", t).strip()
        t = re.sub(r"[\s,]+$", "", t).strip()
        if t == before:
            break
    return t


def find_top_sep(tail):
    """Индекс разделителя " - " на верхнем уровне (вне скобок), или -1."""
    depth = 0
    for m in NAME_SEP_RE.finditer(tail):
        depth = tail.count("(", 0, m.start()) - tail.count(")", 0, m.start())
        if depth <= 0:
            return m
    return None


def split_name_desc(tail):
    tail = clean_tail(tail)
    m = find_top_sep(tail)
    if m:
        name = tail[: m.start()].strip()
        desc = tail[m.end():].strip()
    else:
        name = tail
        desc = tail
    return name, desc


def clean_name(text, fallback):
    s = re.sub(r"\s+", " ", text).strip()
    s = re.sub(r"^[\s,;:\-–—]+", "", s)
    s = re.split(r"\s*\(", s)[0]
    s = re.split(
        r"\s+(?:используется|используются|аналогичн\w*|зависит|зада[её]тся|"
        r"по умолчанию|По умолчанию)\b",
        s,
    )[0]
    s = re.split(r"\.\s", s)[0]
    s = re.sub(r"\s+т\.\s*[ед]\.?\s*$", "", s)
    s = s.strip().rstrip(".,;:").strip()
    if not CYR_RE.search(s):
        s = fallback
    if s:
        s = s[0].upper() + s[1:]
    if len(s) > 80:
        s = s[:80].rsplit(" ", 1)[0]
    return s


def fallback_name(cell, group):
    return "Ячейка %s" % cell.replace("_", " ").strip()


def first_russian_line(block_text):
    for line in block_text.split("\n"):
        s = line.strip()
        if not s or not CYR_RE.search(s):
            continue
        if BIT_RE.match(s) or ENUM_RE.match(s):
            continue
        return re.sub(r"\s+", " ", s)
    return ""


ENUM_RE = re.compile(
    r"^\s*([A-Za-z_][A-Za-z0-9_]*)\s*=\s*(0[xX][0-9A-Fa-f]+|\d+)\s*[,.]?\s*(?:[-–—]\s*)?(.*)$"
)
# Перечисления без символа: "0- Трансляция+заряд.".
NUM_ENUM_RE = re.compile(r"^\s*(\d+)\s*[-–—]\s+(.+)$")
BIT_RE = re.compile(
    r"^\s*(?:Бит|бит|BIT|bit)\s*(\d+)\s*[:.]?\s*"
    r"(?:([A-Za-z_][A-Za-z0-9_]*)\s*=\s*(0[xX][0-9A-Fa-f]+|\d+)\s*)?"
    r"(?:[-–—:]\s*)?(.*)$"
)
FORMULA_RE = re.compile(r"([A-Za-zА-Яа-я_][\wА-Яа-я]*)\s*\(([^)]{1,12})\)\s*=\s*(.+)")


def build_record(raw, addr, tail, block_text, line_no, menu_group, green_names):
    name_ru_raw, desc = split_name_desc(tail)
    desc = re.sub(r"\s+", " ", desc).strip()
    if not CYR_RE.search(desc):
        fallback = first_russian_line(block_text)
        if fallback:
            desc = fallback
    full_text = (tail + " " + block_text).lower()

    kind = "ram" if addr >= 0x400 else "eeprom"
    # width=2 только для (а) явного диапазона адресов в самом определении и
    # (б) слитых пар LOW/HIGH на соседних адресах (см. merge_pairs()).
    width = 1
    order = None
    tail_norm = _norm_hex(tail)
    end = None
    # диапазон вида "0x588-0x589" внутри определения
    rng = re.search(r"0?[xX][0-9A-Fa-f]{0,4}\s*[-–—]\s*0?[xX]([0-9A-Fa-f]{1,4})", tail_norm)
    if rng:
        end = int(rng.group(1), 16)
    else:
        # диапазон вида "=0x718-0x719": конец диапазона ушёл в хвост строки
        m = re.match(r"^\s*[-–—]\s*0?[xX]\s*([0-9A-Fa-f]{1,4})\b", tail_norm)
        if m:
            end = int(m.group(1), 16)
    if end is not None and end >= addr:
        width = 2
        order = "lh"  # порядок байт по умолчанию (младший по младшему адресу)

    if kind == "ram":
        access = "rw" if raw in RW_RAM else "ro"
    else:
        if addr <= 0x04 or raw in ("_UAccAfterDisChage", "_LCD_Vers"):
            access = "ro"
        else:
            access = "rw"

    unit, scale, offset = derive_formula(raw, block_text)
    if not unit and "%" in desc:
        unit = "%"
    enum, bits = derive_enum_bits(raw, block_text)

    modes = derive_modes(raw, tail, block_text, green_names)

    group = EXPLICIT_GROUP.get(raw)
    if group is None and menu_group:
        group = menu_group
    if group is None:
        group = classify_group(raw, full_text, addr)

    name_fallback = fallback_name(raw, group)
    if desc and not re.match(r"^\s*0?[xX]", desc) and CYR_RE.search(desc):
        name_fallback = clean_name(desc, name_fallback)
    name_ru = clean_name(name_ru_raw, name_fallback)
    if raw in NAME_OVERRIDE:
        name_ru = NAME_OVERRIDE[raw]

    # «нет пригодного описания» — для отчёта и для производных имён
    usable_desc = bool(desc) and bool(CYR_RE.search(desc)) and not PLACEHOLDER_DESC_RE.match(desc)
    nodesc = not usable_desc

    mn = mx = None
    if enum:
        nums = sorted(int(k) for k in enum.keys() if re.fullmatch(r"-?\d+", k))
        if nums:
            mn, mx = nums[0], nums[-1]

    rec = {
        "key": None,
        "cell": raw,
        "addr": addr,
        "width": width,
        "kind": kind,
        "access": access,
        "name": name_ru,
        "group": group,
        "modes": modes,
        "unit": unit,
        "scale": float(scale),
        "offset": float(offset),
        "min": mn,
        "max": mx,
        "desc": desc,
        "order": order,
    }
    if enum:
        rec["enum"] = enum
    if bits:
        rec["bits"] = bits
    rec["_line"] = line_no
    rec["_nodesc"] = nodesc
    rec["_derived"] = (raw in NAME_OVERRIDE) or nodesc
    return rec


LOW_SUF_RE = re.compile(r"(_L|_VL)(\[\d+\])?$")
HIGH_SUF_RE = re.compile(r"(_H|_VH)(\[\d+\])?$")


def pair_base(cell):
    """('low'|'high', base) для ячейки-байта пары, иначе (None, None).

    base сохраняет индекс массива: ``_I_Akb_MPPT_L[0]`` -> ``_I_Akb_MPPT[0]``.
    """
    m = LOW_SUF_RE.search(cell)
    if m:
        return "low", cell[: m.start()] + (m.group(2) or "")
    m = HIGH_SUF_RE.search(cell)
    if m:
        return "high", cell[: m.start()] + (m.group(2) or "")
    return None, None


def merge_pairs(merged):
    """Сливает LOW/HIGH-пары на соседних адресах в один 16-битный параметр.

    Возвращает список кортежей (removed_cell, kept_cell, addr, order).
    """
    lows = {}
    highs = {}
    for key, rec in merged.items():
        kind, base = pair_base(rec["cell"])
        if kind == "low":
            lows.setdefault(base, []).append(key)
        elif kind == "high":
            highs.setdefault(base, []).append(key)

    report = []
    for base, hkeys in highs.items():
        for hkey in hkeys:
            hi = merged.get(hkey)
            if hi is None:
                continue
            lkey = None
            for cand in lows.get(base, []):
                lo = merged.get(cand)
                if lo is not None and abs(lo["addr"] - hi["addr"]) == 1:
                    lkey = cand
                    break
            if lkey is None:
                continue
            lo = merged[lkey]
            if lo["addr"] < hi["addr"]:
                keep, rem, ordv = lo, hi, "lh"
            else:
                keep, rem, ordv = hi, lo, "hl"
            keep["width"] = 2
            keep["order"] = ordv
            # scale/offset берём из формулы объединённого значения
            for fld, default in (("scale", 1.0), ("offset", 0.0), ("unit", "")):
                if keep[fld] == default and rem[fld] != default:
                    keep[fld] = rem[fld]
            if rem.get("_nodesc") is not None:
                keep["_nodesc"] = bool(keep.get("_nodesc")) and bool(rem.get("_nodesc"))
            keep["_derived"] = bool(keep.get("_derived")) or bool(rem.get("_derived"))
            if keep["cell"] in NAME_OVERRIDE:
                keep["name"] = NAME_OVERRIDE[keep["cell"]]
            elif rem["cell"] in NAME_OVERRIDE:
                keep["name"] = NAME_OVERRIDE[rem["cell"]]
            report.append((rem["cell"], keep["cell"], keep["addr"], ordv))
            del merged[(rem["cell"], rem["addr"])]
    return report


def self_check(params):
    problems = []
    seen = set()
    for p in params:
        if not CYR_RE.search(p["name"]):
            problems.append("имя без кириллицы: %s (%r)" % (p["cell"], p["name"]))
        if p["width"] == 2 and p.get("order") not in ("lh", "hl"):
            problems.append("width=2 без order: %s" % p["cell"])
        if p["key"] in seen:
            problems.append("дубликат key: %s" % p["key"])
        seen.add(p["key"])
    if problems:
        raise SystemExit(
            "САМОПРОВЕРКА НЕ ПРОЙДЕНА (%d ошибок):\n  %s"
            % (len(problems), "\n  ".join(problems))
        )


def derive_modes(raw, tail, block_text, green_only):
    """Определяет модели применимости ячейки.

    Приоритет: явное «отсутствует в модели TITANATOR» (Dominator-only),
    затем собственное зелёное определение (Titanator-only), затем явные
    пометки «пункт меню Титанатор» / «только для модели Титанатор» на
    строках, не являющихся перечислениями значений или битовыми полями.
    """
    if raw in MODES_OVERRIDE:
        return list(MODES_OVERRIDE[raw])
    if re.search(r"отсутствует\s+в\s+модели\s+titanator", (tail + " " + block_text).lower()):
        return list(MODELS_DOMINATOR)
    if raw in green_only:
        return list(MODELS_TITANATOR)
    tit = False
    other = False
    for line in (tail + "\n" + block_text).split("\n"):
        if not line.strip():
            continue
        if BIT_RE.match(line) or ENUM_RE.match(line):
            continue
        if re.match(r"^\s*\d+\s*[-–—.)]", line):
            continue
        low = line.lower()
        if re.search(r"(также\s+)?пункт\s+меню\s+(в\s+)?титанатор", low):
            tit = True
        if re.search(r"только\s+для\s+модели\s+титанатор", low):
            tit = True
        if ("dominator" in low) or ("hybrid" in low) or re.search(r"\bpro\b", low):
            other = True
    if tit and not other:
        return list(MODELS_TITANATOR)
    return list(MODELS_BOTH)


SCALE_TAIL_DIV_RE = re.compile(r"/\s*([0-9]+(?:[.,][0-9]+)?)\s*[.)\];]*\s*$")
SCALE_TAIL_MUL_RE = re.compile(r"\*\s*([0-9]+(?:[.,][0-9]+)?)\s*[.)\];]*\s*$")
OFFSET_TAIL_RE = re.compile(r"([+\-–—])\s*(\d+(?:[.,]\d+)?)\s*[.)\];]*\s*$")


def derive_formula(raw, block_text):
    """Извлекает unit/scale/offset из формул блока для данной ячейки.

    Берётся первая строка с формулой ``X(unit) = выражение``, в которой
    встречается имя ячейки (без ведущего подчёркивания).
    """
    base = raw.lstrip("_")
    variants = [base]
    stripped = re.sub(r"_?[V]?[LH]$", "", base)
    if stripped and stripped != base:
        variants.append(stripped)
    unit = ""
    scale = 1.0
    offset = 0.0
    for line in block_text.split("\n"):
        if not any(v and v in line for v in variants):
            continue
        fm = FORMULA_RE.search(line)
        if not fm:
            continue
        u = fm.group(2).strip()
        rhs = fm.group(3).strip()
        # отрезаем пояснение после формулы ("... – общее потребление ...",
        # "... , где K=1 ..."), но не трогаем " – 50" (это знак в формуле).
        rhs = re.split(r"\s[–—-]\s(?=[А-Яа-яA-Za-z])", rhs)[0].strip()
        rhs = re.split(r",\s", rhs)[0].strip()
        if u in UNIT_MAP and not unit:
            unit = UNIT_MAP[u]
        # нелинейные формулы (деление на саму ячейку) пропускаем
        if re.search(r"/\s*\(?\s*" + re.escape(base) + r"\b", rhs):
            continue
        m = SCALE_TAIL_DIV_RE.search(rhs)
        if m:
            val = float(m.group(1).replace(",", "."))
            if val:
                scale = 1.0 / val
        else:
            m = SCALE_TAIL_MUL_RE.search(rhs)
            if m:
                try:
                    scale = float(m.group(1).replace(",", "."))
                except ValueError:
                    pass
        om = OFFSET_TAIL_RE.search(rhs)
        if om and ("*" not in rhs[om.start():]) and ("/" not in rhs[om.start():]):
            val = float(om.group(2).replace(",", "."))
            offset = val if om.group(1) == "+" else -val
        if unit or scale != 1.0 or offset:
            break
    return unit, scale, offset


def clean_enum_desc(s):
    s = re.sub(r"\s+", " ", s).strip()
    s = re.sub(r"^[-–—:;,]+\s*", "", s)
    return s


def derive_enum_bits(raw, block_text):
    enum = {}
    bits = []
    for line in block_text.split("\n"):
        if not line.strip():
            continue
        bm = BIT_RE.match(line)
        if bm:
            bit = int(bm.group(1))
            desc = bm.group(4) or ""
            desc = clean_enum_desc(desc)
            if desc:
                bits.append({"bit": bit, "name": desc})
            continue
        em = ENUM_RE.match(line)
        if em:
            symbol = em.group(1)
            value = int(em.group(2), 0)
            desc = clean_enum_desc(em.group(3))
            # отсекаем алгебраические строки вида "UNET =0 – нет сети иначе ..."
            if symbol.lower() == raw.lstrip("_").lower():
                continue
            if "=" in desc or "иначе" in desc.lower():
                continue
            if desc and len(desc) > 3:
                enum.setdefault(str(value), desc)
            continue
        nm = NUM_ENUM_RE.match(line)
        if nm:
            desc = clean_enum_desc(nm.group(2))
            if "иначе" in desc.lower():
                continue
            if desc and len(desc) > 3:
                enum.setdefault(nm.group(1), desc)
    # если есть биты — enum не нужен
    if bits:
        enum = {}
    # убираем enum-дубли (одинаковые значения)
    if enum:
        enum = {k: enum[k] for k in sorted(enum, key=lambda x: int(x))}
    return enum, bits


# --------------------------------------------------------------------------
# Классификация групп для ячеек вне явного словаря
# --------------------------------------------------------------------------


def classify_group(raw, text, addr):
    n = raw.lower()
    t = text
    if n.startswith("_rserr") or n.startswith("_rswarning") or n.startswith("_f_acc") or n.startswith("_f_net"):
        return "Ошибки и предупреждения"
    if "температур" in t or n.startswith("_temp") or n.startswith("_takb"):
        return "Температуры"
    if n.startswith("_m_powhour") or "квт.ч" in t or "счетчик" in t or "счётчик" in t:
        return "Энергия и счётчики"
    if n.startswith(("_unet", "_inet", "_tfnet", "_thfmap", "_pnet")):
        return "Сеть"
    if n.startswith(("_uacc", "_iacc", "_uakbcell", "_q_cell", "_i_akb", "_soc", "_soh", "_bms")) or n.startswith("_e_lcd_uacc"):
        return "Батарея (АКБ)"
    if n.startswith("_pl") or "мощност" in t or "нагрузк" in t:
        return "Нагрузка и мощность"
    if n.startswith(("_uout", "_thfmap")) or "синус" in t or "генераци" in t or "инвертор" in t:
        return "Выход / инвертор"
    if "заряд" in t:
        return "Заряд АКБ (настройки)"
    if "реле" in t or "rs485" in t or "modbus" in t or "mppt" in t or "bms" in t or "usb" in t or "rs232" in t:
        return "Реле и внешние устройства"
    if "жкi" in t or "жк" in t or "меню" in t or "подсветк" in t or "звук" in t:
        return "ЖКИ и меню (настройки)"
    if n in ("_mode", "_statusch"):
        return "Состояние МАП"
    if addr >= 0x100:
        return "Режимы и пороги (настройки)"
    return "Прочее"


# --------------------------------------------------------------------------
# Ключи и итоговый JSON
# --------------------------------------------------------------------------


def make_key(raw, addr, used):
    base = raw.lstrip("_")
    base = base.replace("[", "_").replace("]", "")
    base = re.sub(r"[^A-Za-z0-9]+", "_", base).strip("_").lower()
    base = re.sub(r"_+", "_", base) or "cell"
    key = base
    if key in used:
        key = "%s_%d" % (base, addr)
        i = 2
        while key in used:
            key = "%s_%d_%d" % (base, addr, i)
            i += 1
    used.add(key)
    return key


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    script_dir = os.path.dirname(os.path.abspath(__file__))
    repo_root = os.path.dirname(script_dir)
    parser.add_argument(
        "--doc",
        default=os.path.join(repo_root, "docs", "map", "map", SOURCE_NAME),
    )
    parser.add_argument("--txt", default="/tmp/kilo/mapdoc/protocol_MAP_cells_2026_07_15.txt")
    parser.add_argument(
        "--html",
        default="/tmp/kilo/mapdoc/html/protocol_MAP_cells_2026_07_15.html",
    )
    parser.add_argument("--out", default=os.path.join(script_dir, "catalog.json"))
    args = parser.parse_args(argv)

    txt_path, html_path = resolve_sources(args.doc, args.txt, args.html)
    green_names = green_names_from_html(html_path)
    records = parse_txt(txt_path, green_names)

    # Слияние дублей по (cell, addr): приоритет — зелёный/более полный.
    merged = {}
    order = []
    for rec in records:
        k = (rec["cell"], rec["addr"])
        if k not in merged:
            merged[k] = rec
            order.append(k)
        else:
            cur = merged[k]
            union = [m for m in MODELS_BOTH if m in cur["modes"] or m in rec["modes"]]
            cur["modes"] = union or list(rec["modes"])
            if len(rec.get("enum", {})) > len(cur.get("enum", {})):
                cur["enum"] = rec["enum"]
            if len(rec.get("bits", [])) > len(cur.get("bits", [])):
                cur["bits"] = rec["bits"]
            if len(rec["desc"]) > len(cur["desc"]):
                cur["desc"] = rec["desc"]
                if len(rec["name"]) > len(cur["name"]):
                    cur["name"] = rec["name"]

    merged_pairs = merge_pairs(merged)
    merged_pairs.sort(key=lambda t: t[2])

    used = set()
    params = []
    derived = []
    remaining = [k for k in order if k in merged]
    for k in remaining:
        rec = merged[k]
        rec["key"] = make_key(rec["cell"], rec["addr"], used)
        if rec.get("_derived"):
            derived.append(rec["cell"])
        # порядок полей строго по схеме
        out = {
            "key": rec["key"],
            "cell": rec["cell"],
            "addr": rec["addr"],
            "width": rec["width"],
            "kind": rec["kind"],
            "access": rec["access"],
            "name": rec["name"],
            "group": rec["group"],
            "modes": rec["modes"],
            "unit": rec["unit"],
            "scale": rec["scale"],
            "offset": rec["offset"],
            "min": rec["min"],
            "max": rec["max"],
            "desc": rec["desc"],
        }
        if rec["width"] == 2:
            out["order"] = rec["order"]
        if "enum" in rec:
            out["enum"] = rec["enum"]
        if "bits" in rec:
            out["bits"] = rec["bits"]
        params.append(out)

    params.sort(key=lambda p: (p["addr"], p["cell"]))
    self_check(params)
    catalog = {"source": SOURCE_NAME, "title": TITLE, "params": params}

    with open(args.out, "w", encoding="utf-8") as fh:
        json.dump(catalog, fh, ensure_ascii=False, indent=2)
        fh.write("\n")

    print_summary(catalog, args.out, green_names, merged_pairs, derived)
    return 0


def print_summary(catalog, out_path, green_names, merged_pairs, derived):
    params = catalog["params"]
    total = len(params)
    ram = sum(1 for p in params if p["kind"] == "ram")
    eeprom = total - ram
    rw = sum(1 for p in params if p["access"] == "rw")
    ro = total - rw
    tit_only = sum(1 for p in params if p["modes"] == MODELS_TITANATOR)
    width2 = [p for p in params if p["width"] == 2]
    noncyr = [p for p in params if not CYR_RE.search(p["name"])]
    print("Каталог записан: %s" % out_path)
    print("Всего ячеек: %d" % total)
    print("  RAM: %d, EEPROM: %d" % (ram, eeprom))
    print("  rw: %d, ro: %d" % (rw, ro))
    print("  Titanator-only (зелёные): %d" % tit_only)
    print("  width==2: %d (order lh/hl)" % len(width2))
    print("  не-кириллических имён: %d" % len(noncyr))
    print("Слитые пары LOW/HIGH (%d):" % len(merged_pairs))
    for rem, keep, addr, ordv in merged_pairs:
        print("  %s + %s -> %s @0x%X order=%s" % (rem, keep, keep, addr, ordv))
    print("Имя получено не из описания документа (%d):" % len(derived))
    print("  " + ", ".join(derived))
    by_group = {}
    for p in params:
        by_group[p["group"]] = by_group.get(p["group"], 0) + 1
    print("Группы:")
    for g in sorted(by_group):
        print("  %-32s %d" % (g, by_group[g]))


if __name__ == "__main__":
    sys.exit(main())

#!/usr/bin/env python3
# ЭТАП 1: новые партиционированные и «разложенные по колонкам» таблицы для
# 5-минутных средних (*_averages). Запуск с dev-ПК прямо в БД. Идемпотентно.
#
#   - Имена: <исходная>_r (суффикс "R").
#   - Партиционирование: PARTITION BY RANGE (ts), помесячно; PK (ключ, ts).
#   - Значения jsonb -> типизированные колонки double precision; строковые
#     метаданные (base/source/...) -> text.
#   - Вложенные массивы bms26 (cells_v/temperatures_c/alarm_bytes) -> отдельные
#     дочерние таблицы <parent>_cells / _temps / _alarm, тем же партиционированием.
#   - Функция ensure_month_partition (помесячное создание партиции); партиции
#     создаёт ПРИЛОЖЕНИЕ (старт + каждые 12ч, горизонт) — BEFORE-триггер не годится
#     (PG routing до триггеров). Здесь предсоздаются месяцы -2..+24.
#
# Старые таблицы НЕ трогаются. Данные пока пишутся/лежат в старых.
import json
import os
import urllib.parse

import pg8000.dbapi

HERE = os.path.dirname(os.path.abspath(__file__))
CFG = os.path.join(HERE, "..", "sunReceiver.json")

DP = "double precision"

# spec: table -> (key_col, meta_cols[list], metric_cols[list])
# key_col = логический ключ устройства; meta = текстовые информационные колонки.
TABLES = {
    "deye_averages": ("serial", ["name", "device_sn", "ip"], [
        "ac_active_power", "ac_reactive_power", "energy_today", "energy_total",
        "grid_frequency", "l1_current", "l1_voltage", "l2_current", "l2_voltage",
        "l3_current", "l3_voltage", "pv1_current", "pv1_power", "pv1_voltage",
        "pv2_current", "pv2_power", "pv2_voltage"]),
    "sofar_averages": ("serial", ["name", "device_sn", "ip"], [
        "ac_active_power", "ac_reactive_power", "energy_today", "energy_total",
        "grid_frequency", "l1_current", "l1_voltage", "l2_current", "l2_voltage",
        "l3_current", "l3_voltage", "pv1_current", "pv1_power", "pv1_voltage",
        "pv2_current", "pv2_power", "pv2_voltage"]),
    "dds238_averages": ("name", ["device_sn"], [
        "meter_active_power", "meter_current", "meter_export", "meter_frequency",
        "meter_import", "meter_power_factor", "meter_reactive_power", "meter_total",
        "meter_voltage"]),
    "map_averages": ("name", ["device_sn"], [
        "battery_current", "battery_power", "battery_voltage", "grid_frequency",
        "grid_power", "grid_voltage", "l1_voltage"]),
    "kes_averages": ("name", ["device_sn"], [
        "ac_active_power", "energy_today", "l1_current", "l1_voltage",
        "pv1_current", "pv1_power", "pv1_voltage"]),
    "ce308_averages": ("name", [], [
        "ce308_l1_current", "ce308_l1_voltage", "ce308_l2_current", "ce308_l2_voltage",
        "ce308_l3_current", "ce308_l3_voltage", "ce308_active_power", "ce308_reactive_power",
        "ce308_l1_active_power", "ce308_l2_active_power", "ce308_l3_active_power",
        "ce308_l1_reactive_power", "ce308_l2_reactive_power", "ce308_l3_reactive_power"]),
    # bms26: скалярные числа -> колонки, текстовые метаданные -> колонки,
    # массивы (cells_v/temperatures_c/alarm_bytes) -> дочерние таблицы CHILD.
    "bms26_averages": ("name", ["base", "serial", "bms_type", "device_name"], [
        "soc", "soh", "mode", "flags", "cycles", "power_w", "rated_ah", "current_a",
        "avg_cell_v", "cell_count", "max_cell_v", "min_cell_v", "temp_count",
        "capacity_ah", "balance_mask", "cell_warn_mask", "cell_prot_mask",
        "temp_warn_mask", "temp_prot_mask", "max_cell_idx", "min_cell_idx",
        "remaining_ah", "port_voltage_v", "total_voltage_v"]),
    "dts017m_averages": ("name", ["device_sn"], [
        "dts017_active_power", "dts017_active_power_a", "dts017_active_power_b",
        "dts017_active_power_c", "dts017_apparent_power", "dts017_apparent_power_a",
        "dts017_apparent_power_b", "dts017_apparent_power_c", "dts017_current_a",
        "dts017_current_b", "dts017_current_c", "dts017_export", "dts017_export_reactive",
        "dts017_frequency_a", "dts017_frequency_b", "dts017_frequency_c", "dts017_import",
        "dts017_import_reactive", "dts017_power_factor", "dts017_power_factor_a",
        "dts017_power_factor_b", "dts017_power_factor_c", "dts017_reactive_power",
        "dts017_reactive_power_a", "dts017_reactive_power_b", "dts017_reactive_power_c",
        "dts017_reactive_total", "dts017_total", "dts017_voltage_a", "dts017_voltage_ab",
        "dts017_voltage_b", "dts017_voltage_bc", "dts017_voltage_c", "dts017_voltage_ca"]),
}

# Дочерние таблицы для массивов bms26: (суффикс, имя значения, тип значения)
CHILDREN = {
    "bms26_averages": [("cells", "v", DP), ("temps", "v", DP), ("alarm", "v", "integer")],
}


def pg_connect(dsn):
    u = urllib.parse.urlparse(dsn)
    return pg8000.dbapi.connect(
        user=u.username, password=u.password, host=u.hostname,
        port=u.port or 5432, database=(u.path or "/sunreceiver").lstrip("/"))


def ddl_parent(t, key, meta, metrics):
    cols = [f"\t{key} text NOT NULL", "\tts timestamptz NOT NULL"]
    cols += [f"\t{c} text NOT NULL DEFAULT ''" for c in meta]
    cols += [f"\t{c} {DP}" for c in metrics]
    cols.append(f"\tPRIMARY KEY ({key}, ts)")
    return f"CREATE TABLE IF NOT EXISTS sunreceiver.{t}_r (\n" + ",\n".join(cols) + \
           "\n) PARTITION BY RANGE (ts);"


def ddl_child(parent, suffix, valname, valtype):
    return (f"CREATE TABLE IF NOT EXISTS sunreceiver.{parent}_r_{suffix} (\n"
            f"\tname text NOT NULL,\n\tts timestamptz NOT NULL,\n\tidx integer NOT NULL,\n"
            f"\t{valname} {valtype},\n\tPRIMARY KEY (name, ts, idx)\n) PARTITION BY RANGE (ts);")


def main():
    with open(os.path.abspath(CFG)) as f:
        dsn = json.load(f)["db"]["pg"]
    conn = pg_connect(dsn)
    cur = conn.cursor()

    # 0) Функция авто-создания помесячной партиции. Партиции создаёт ПРИЛОЖЕНИЕ
    #    (при старте и каждые 12 ч, этап 2) — BEFORE-триггер на parent не годится:
    #    PG выполняет tuple routing до fired BEFORE-ROW-триггеров, поэтому партипия
    #    должна существовать заранее. Здесь же предсоздаём горизонт на 24 мес.
    cur.execute(r"""
CREATE OR REPLACE FUNCTION sunreceiver.ensure_month_partition(p_parent regclass, p_ts timestamptz)
RETURNS void LANGUAGE plpgsql AS $$
DECLARE
  v_from timestamptz := date_trunc('month', p_ts);
  v_to   timestamptz := v_from + interval '1 month';
  v_name text := format('%s_p%s', p_parent::text, to_char(v_from, 'YYYYMM'));
BEGIN
  IF to_regclass(v_name) IS NULL THEN
    EXECUTE format('CREATE TABLE %s PARTITION OF %s FOR VALUES FROM (%L) TO (%L)',
                   v_name, p_parent, v_from, v_to);
  END IF;
END $$;
""")

    created = []
    parents = list(TABLES.items())
    for t, (key, meta, metrics) in parents:
        cur.execute(ddl_parent(t, key, meta, metrics))
        # индекс по ts (для диапазонных запросов по времени по всем устройствам).
        cur.execute(f"CREATE INDEX IF NOT EXISTS {t}_r_ts_idx ON sunreceiver.{t}_r (ts);")
        # на случай ранней версии скрипта с триггерами — убираем их.
        cur.execute(f"DROP TRIGGER IF EXISTS {t}_r_ensure_part ON sunreceiver.{t}_r;")
        created.append(f"{t}_r")
        for suffix, vname, vtype in CHILDREN.get(t, []):
            child = f"{t}_r_{suffix}"
            cur.execute(ddl_child(t, suffix, vname, vtype))
            cur.execute(f"CREATE INDEX IF NOT EXISTS {child}_ts_idx ON sunreceiver.{child} (ts);")
            cur.execute(f"DROP TRIGGER IF EXISTS {child}_ensure_part ON sunreceiver.{child};")
            created.append(child)

    # Предсоздаём партиции: текущий месяц и 24 вперёд (запас; дальше горизонт
    # поддерживает приложение).
    for tbl in created:
        for off in range(-2, 25):
            cur.execute("SELECT sunreceiver.ensure_month_partition(%s::regclass, "
                        "(date_trunc('month', now()) + (%s || ' month')::interval))",
                        (f"sunreceiver.{tbl}", str(off)))
    conn.commit()
    cur.close()
    conn.close()
    print("Создано таблиц:", len(created))
    for c in created:
        print("  ", c)
    print("done")


if __name__ == "__main__":
    main()

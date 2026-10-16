#!/usr/bin/env python3
# Зачистка рудиментов device_sn/name по согласованным требованиям
# (docs/requirements/db-rudiment-cleanup.md). Запуск с dev-ПК. Идемпотентно.
#  1) dds238_averages: device_sn 'oracle-import' -> '' (реального серийника нет).
#  2) map_averages:    device_sn 'map'          -> '<serial из конфига>' (серийник МАП из конфига).
#  3) kes_averages:    name MPPT-1/2/3 -> MPPT-1097/1750/1751,
#                      device_sn -> mppt-1097/1750/1751 (3 ключа истории).
# NB: живой снимок map device_sn ставит код (malina26), не этот скрипт.
import json
import os
import urllib.parse

import pg8000.dbapi

HERE = os.path.dirname(os.path.abspath(__file__))
CFG = os.path.join(HERE, "..", "sunReceiver.json")


def pg_connect(dsn):
    u = urllib.parse.urlparse(dsn)
    return pg8000.dbapi.connect(
        user=u.username, password=u.password, host=u.hostname,
        port=u.port or 5432, database=(u.path or "/sunreceiver").lstrip("/"))


def main():
    with open(os.path.abspath(CFG)) as f:
        cfg = json.load(f)
    dsn = cfg["db"]["pg"]
    map_serial = str(cfg.get("map", {}).get("malina26", {}).get("serial", "")).strip()
    conn = pg_connect(dsn)
    cur = conn.cursor()

    cur.execute("UPDATE sunreceiver.dds238_averages SET device_sn='' WHERE device_sn='oracle-import'")
    print("1) dds238 oracle-import -> '':", cur.rowcount)

    if map_serial:
        cur.execute("UPDATE sunreceiver.map_averages SET device_sn=%s WHERE device_sn='map'", (map_serial,))
        print(f"2) map 'map' -> '{map_serial}':", cur.rowcount)
    else:
        print("2) map: серийник МАП не найден в конфиге — пропуск")

    cur.execute("""
        UPDATE sunreceiver.kes_averages
        SET name = CASE name
                     WHEN 'MPPT-1' THEN 'MPPT-1097'
                     WHEN 'MPPT-2' THEN 'MPPT-1750'
                     WHEN 'MPPT-3' THEN 'MPPT-1751'
                   END,
            device_sn = CASE name
                     WHEN 'MPPT-1' THEN 'mppt-1097'
                     WHEN 'MPPT-2' THEN 'mppt-1750'
                     WHEN 'MPPT-3' THEN 'mppt-1751'
                   END
        WHERE name IN ('MPPT-1','MPPT-2','MPPT-3')""")
    print("3) kes MPPT-1/2/3 -> MPPT-1097/1750/1751:", cur.rowcount)

    conn.commit()
    cur.close()
    conn.close()
    print("done")


if __name__ == "__main__":
    main()

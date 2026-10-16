#!/usr/bin/env python3
# Удаление рудиментарной колонки ip из обособленных таблиц DDS238 и МАП
# (PK переезжает с (ip,ts) на (name,ts)). Выполняется с этого (dev) ПК напрямую
# в БД, ПОСЛЕ правки кода и ДО деплоя (иначе INSERT без ip упрётся в NOT NULL).
# Идемпотентно.
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


def has_column(cur, table, col):
    cur.execute("""SELECT 1 FROM information_schema.columns
                   WHERE table_schema='sunreceiver' AND table_name=%s AND column_name=%s""",
                (table, col))
    return cur.fetchone() is not None


def drop_ip(cur, table):
    if not has_column(cur, table, "ip"):
        print(f"{table}: колонки ip нет — пропуск")
        return
    cur.execute(f"ALTER TABLE sunreceiver.{table} DROP CONSTRAINT IF EXISTS {table}_pkey")
    cur.execute(f"ALTER TABLE sunreceiver.{table} DROP COLUMN ip")
    cur.execute(f"ALTER TABLE sunreceiver.{table} ADD PRIMARY KEY (name, ts)")
    print(f"{table}: колонка ip удалена, PK (name, ts)")


def main():
    with open(os.path.abspath(CFG)) as f:
        dsn = json.load(f)["db"]["pg"]
    conn = pg_connect(dsn)
    cur = conn.cursor()
    for t in ("dds238_averages", "map_averages"):
        drop_ip(cur, t)
    conn.commit()
    cur.close()
    conn.close()
    print("done")


if __name__ == "__main__":
    main()

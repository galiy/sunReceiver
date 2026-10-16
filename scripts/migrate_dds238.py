#!/usr/bin/env python3
# Миграция данных DDS238 в обособленные таблицы/ключи (этап 2).
#
# Переносит:
#   PG:  averages(ip='dds238')      -> dds238_averages
#        daily_tariffs              -> dds238_daily_tariffs
#   Redis: sunreceiver:current[dds238]      -> sunreceiver:dds238:current
#          sunreceiver:series:* (IP=dds238) -> sunreceiver:dds238:series:*
#
# Правило проекта: миграции — отдельным Python-скриптом, запуск с ЭТОГО (dev) ПК
# напрямую в БД. Скрипт идемпотентен (ON CONFLICT / без перезаписи существующего).
#
# Адреса БД берутся из приватного sunReceiver.json (db.redis, db.pg).
import json
import os
import sys
import urllib.parse

import pg8000.dbapi
import redis

HERE = os.path.dirname(os.path.abspath(__file__))
CFG = os.path.join(HERE, "..", "sunReceiver.json")
DEVKEY = "dds238"


def load_db():
    with open(os.path.abspath(CFG)) as f:
        d = json.load(f)["db"]
    host, port = d["redis"].split(":")
    return (host, int(port)), d["pg"]


def pg_connect(dsn):
    u = urllib.parse.urlparse(dsn)
    return pg8000.dbapi.connect(
        user=u.username, password=u.password, host=u.hostname,
        port=u.port or 5432, database=(u.path or "/sunreceiver").lstrip("/"))


def migrate_pg(cur):
    cur.execute("""
        INSERT INTO sunreceiver.dds238_averages (ip, name, ts, device_sn, values)
        SELECT ip, name, ts, device_sn, values
        FROM sunreceiver.averages WHERE ip = %s
        ON CONFLICT (ip, ts) DO NOTHING""", (DEVKEY,))
    print("PG dds238_averages: перенесено строк:", cur.rowcount)

    cur.execute("""
        INSERT INTO sunreceiver.dds238_daily_tariffs
        SELECT * FROM sunreceiver.daily_tariffs
        ON CONFLICT (day) DO NOTHING""")
    print("PG dds238_daily_tariffs: перенесено строк:", cur.rowcount)


def migrate_redis(r):
    # current-поле dds238 из общего HASH
    val = r.hget("sunreceiver:current", DEVKEY)
    if val is not None:
        moved = r.hsetnx("sunreceiver:dds238:current", DEVKEY, val)
        print("Redis current: поле dds238 ->", "перенесено" if moved else "уже есть")

    # точки DDS238 из общего месячного ряда
    months = [k.decode() for k in r.scan_iter(match="sunreceiver:series:*")]
    total = 0
    for key in months:
        month = key.rsplit(":", 1)[-1]
        dst = "sunreceiver:dds238:series:" + month
        vals = r.zrange(key, 0, -1, withscores=True)
        for member, score in vals:
            try:
                snap = json.loads(member)
            except Exception:
                continue
            if snap.get("ip") != DEVKEY:
                continue
            r.zadd(dst, {member: score})
            total += 1
    print("Redis series: точек DDS238 перенесено:", total)


def main():
    (rhost, rport), dsn = load_db()
    r = redis.Redis(host=rhost, port=rport, decode_responses=False)
    conn = pg_connect(dsn)
    cur = conn.cursor()
    migrate_pg(cur)
    conn.commit()
    cur.close()
    conn.close()
    migrate_redis(r)
    print("done")


if __name__ == "__main__":
    sys.exit(main())

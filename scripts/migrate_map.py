#!/usr/bin/env python3
# Миграция данных МАП в обособленные таблицу/ключи (по образцу dds238).
#   PG:    averages(ip='map') -> map_averages
#   Redis: sunreceiver:current[map]      -> sunreceiver:map:current
#          sunreceiver:series:* (ip=map) -> sunreceiver:map:series:*
# Правило проекта: Python-скрипт, запуск с этого (dev) ПК напрямую в БД. Идемпотентно.
import json
import os
import urllib.parse

import pg8000.dbapi
import redis

HERE = os.path.dirname(os.path.abspath(__file__))
CFG = os.path.join(HERE, "..", "sunReceiver.json")
DEVKEY = "map"


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


def main():
    (rhost, rport), dsn = load_db()
    r = redis.Redis(host=rhost, port=rport, decode_responses=False)
    conn = pg_connect(dsn)
    cur = conn.cursor()

    cur.execute("""
        INSERT INTO sunreceiver.map_averages (ip, name, ts, device_sn, values)
        SELECT ip, name, ts, device_sn, values
        FROM sunreceiver.averages WHERE ip = %s
        ON CONFLICT (ip, ts) DO NOTHING""", (DEVKEY,))
    print("PG map_averages: перенесено строк:", cur.rowcount)
    conn.commit()
    cur.close()
    conn.close()

    val = r.hget("sunreceiver:current", DEVKEY)
    if val is not None:
        moved = r.hsetnx("sunreceiver:map:current", DEVKEY, val)
        print("Redis current: поле map ->", "перенесено" if moved else "уже есть")

    total = 0
    for key in [k.decode() for k in r.scan_iter(match="sunreceiver:series:*")]:
        month = key.rsplit(":", 1)[-1]
        dst = "sunreceiver:map:series:" + month
        for member, score in r.zrange(key, 0, -1, withscores=True):
            try:
                snap = json.loads(member)
            except Exception:
                continue
            if snap.get("ip") != DEVKEY:
                continue
            r.zadd(dst, {member: score})
            total += 1
    print("Redis series: точек МАП перенесено:", total)
    print("done")


if __name__ == "__main__":
    main()

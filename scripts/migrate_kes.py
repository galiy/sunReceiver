#!/usr/bin/env python3
# Миграция данных КЭС (MPPT) в обособленные таблицу/ключи.
#   PG:    averages(ip LIKE 'mppt-%')      -> kes_averages (без ip)
#   Redis: sunreceiver:current[mppt-*]      -> sunreceiver:kes:current
#          sunreceiver:series:* (ip mppt-*) -> sunreceiver:kes:series:*
# Правило проекта: Python-скрипт, запуск с этого (dev) ПК. Идемпотентно.
import json
import os
import urllib.parse

import pg8000.dbapi
import redis

HERE = os.path.dirname(os.path.abspath(__file__))
CFG = os.path.join(HERE, "..", "sunReceiver.json")


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
        INSERT INTO sunreceiver.kes_averages (name, ts, device_sn, values)
        SELECT name, ts, device_sn, values FROM sunreceiver.averages
        WHERE ip LIKE 'mppt-%%'
        ON CONFLICT (name, ts) DO NOTHING""")
    print("PG kes_averages: перенесено строк:", cur.rowcount)
    conn.commit()
    cur.close()
    conn.close()

    moved = 0
    for field, val in r.hscan_iter("sunreceiver:current"):
        fk = field.decode()
        if fk.startswith("mppt-"):
            r.hsetnx("sunreceiver:kes:current", fk, val)
            moved += 1
    print("Redis current: полей КЭС перенесено:", moved)

    total = 0
    for key in [k.decode() for k in r.scan_iter(match="sunreceiver:series:*")]:
        month = key.rsplit(":", 1)[-1]
        dst = "sunreceiver:kes:series:" + month
        for member, score in r.zrange(key, 0, -1, withscores=True):
            try:
                snap = json.loads(member)
            except Exception:
                continue
            if str(snap.get("ip", "")).startswith("mppt-"):
                r.zadd(dst, {member: score})
                total += 1
    print("Redis series: точек КЭС перенесено:", total)
    print("done")


if __name__ == "__main__":
    main()

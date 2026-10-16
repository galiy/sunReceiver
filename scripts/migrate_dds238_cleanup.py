#!/usr/bin/env python3
# Этап 3: удаление СТАРЫХ данных DDS238 после миграции.
#
# Убирает:
#   Redis: поле dds238 из общего sunreceiver:current;
#          точки IP=dds238 из общего ряда sunreceiver:series:*;
#   PG:    строки ip='dds238' из averages;
#          таблицу daily_tariffs (старая, больше не используется).
#
# Правило проекта: запуск с ЭТОГО (dev) ПК напрямую в БД. Идемпотентно.
import json
import os
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


def clean_pg(cur):
    cur.execute("DELETE FROM sunreceiver.averages WHERE ip = %s", (DEVKEY,))
    print("PG averages ip=dds238 удалено строк:", cur.rowcount)
    cur.execute("DROP TABLE IF EXISTS sunreceiver.daily_tariffs")
    print("PG daily_tariffs: удалена")


def clean_redis(r):
    removed = r.hdel("sunreceiver:current", DEVKEY)
    print("Redis current: поле dds238 удалено:", removed)

    total = 0
    for key in [k.decode() for k in r.scan_iter(match="sunreceiver:series:*")]:
        vals = r.zrange(key, 0, -1)
        stale = []
        for member in vals:
            try:
                snap = json.loads(member)
            except Exception:
                continue
            if snap.get("ip") == DEVKEY:
                stale.append(member)
        if stale:
            r.zrem(key, *stale)
            total += len(stale)
    print("Redis series: точек DDS238 удалено:", total)


def main():
    (rhost, rport), dsn = load_db()
    r = redis.Redis(host=rhost, port=rport, decode_responses=False)
    conn = pg_connect(dsn)
    cur = conn.cursor()
    clean_pg(cur)
    conn.commit()
    cur.close()
    conn.close()
    clean_redis(r)
    print("done")


if __name__ == "__main__":
    main()

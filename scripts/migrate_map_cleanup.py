#!/usr/bin/env python3
# Удаление СТАРЫХ данных МАП после миграции.
#   Redis: поле map из sunreceiver:current; точки ip=map из sunreceiver:series:*;
#   PG:    строки ip='map' из averages.
# Правило проекта: запуск с этого (dev) ПК напрямую в БД. Идемпотентно.
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
    cur.execute("DELETE FROM sunreceiver.averages WHERE ip = %s", (DEVKEY,))
    print("PG averages ip=map удалено строк:", cur.rowcount)
    conn.commit()
    cur.close()
    conn.close()

    removed = r.hdel("sunreceiver:current", DEVKEY)
    print("Redis current: поле map удалено:", removed)
    total = 0
    for key in [k.decode() for k in r.scan_iter(match="sunreceiver:series:*")]:
        stale = []
        for member in r.zrange(key, 0, -1):
            try:
                snap = json.loads(member)
            except Exception:
                continue
            if snap.get("ip") == DEVKEY:
                stale.append(member)
        if stale:
            r.zrem(key, *stale)
            total += len(stale)
    print("Redis series: точек МАП удалено:", total)
    print("done")


if __name__ == "__main__":
    main()

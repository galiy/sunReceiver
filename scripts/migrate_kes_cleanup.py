#!/usr/bin/env python3
# Удаление СТАРЫХ данных КЭС после миграции.
#   Redis: поля mppt-* из sunreceiver:current; точки ip=mppt-* из sunreceiver:series:*;
#   PG:    строки ip LIKE 'mppt-%' из averages.
# Правило проекта: запуск с этого (dev) ПК. Идемпотентно.
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
    cur.execute("DELETE FROM sunreceiver.averages WHERE ip LIKE 'mppt-%%'")
    print("PG averages ip=mppt-* удалено строк:", cur.rowcount)
    conn.commit()
    cur.close()
    conn.close()

    stale = [f.decode() for f, _ in r.hscan_iter("sunreceiver:current") if f.decode().startswith("mppt-")]
    if stale:
        r.hdel("sunreceiver:current", *stale)
    print("Redis current: полей КЭС удалено:", len(stale))

    total = 0
    for key in [k.decode() for k in r.scan_iter(match="sunreceiver:series:*")]:
        rem = []
        for member in r.zrange(key, 0, -1):
            try:
                snap = json.loads(member)
            except Exception:
                continue
            if str(snap.get("ip", "")).startswith("mppt-"):
                rem.append(member)
        if rem:
            r.zrem(key, *rem)
            total += len(rem)
    print("Redis series: точек КЭС удалено:", total)
    print("done")


if __name__ == "__main__":
    main()

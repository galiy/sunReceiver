#!/usr/bin/env python3
# Правка после миграции ZSET->RT: у КЭС миграция создала серии с device="MPPT-<n>",
# а живой писатель использует device="mppt-<n>". Приводим к одному виду (mppt-<n>):
# слияние точек и удаление исходной серии. Идемпотентно.
import json
import os

import redis

HERE = os.path.dirname(os.path.abspath(__file__))
CFG = os.path.join(HERE, "..", "sunReceiver.json")
TS_PREFIX = "sunreceiver:ts:"


def main():
    with open(os.path.abspath(CFG)) as f:
        host, port = json.load(f)["db"]["redis"].split(":")
    r = redis.Redis(host=host, port=int(port), decode_responses=True)
    keys = [k for k in r.scan_iter(match=TS_PREFIX + "kes:*")]
    moved = merged = 0
    for src in keys:
        parts = src.split(":")  # sunreceiver ts kes <device> <tag>
        if len(parts) < 5:
            continue
        device = parts[3]
        if not device.startswith("MPPT-"):
            continue
        newdev = "mppt-" + device[len("MPPT-"):]
        parts[3] = newdev
        dst = ":".join(parts)
        if r.exists(dst):
            # слияние: переносим точки в существующую серию
            pts = r.execute_command("TS.RANGE", src, "-", "+")
            if pts:
                pipe = r.pipeline(transaction=False)
                for ts, val in pts:
                    pipe.execute_command("TS.ADD", dst, int(ts), val, "ON_DUPLICATE", "LAST")
                pipe.execute()
            r.delete(src)
            merged += 1
        else:
            r.rename(src, dst)
            try:
                r.execute_command("TS.ALTER", dst, "LABELS", "device", newdev)
            except redis.exceptions.ResponseError:
                pass
            moved += 1
    print("renamed:", moved, "merged:", merged)
    print("done")


if __name__ == "__main__":
    main()

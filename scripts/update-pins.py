#!/usr/bin/env python3
import json
import sys
from pathlib import Path


def update(versions, record):
    asset = record["asset"]
    plat = record["platform"]
    entry = versions.get("assets", {}).get(asset)
    if entry is None:
        raise SystemExit("update-pins: no asset %s" % asset)
    tree = entry.get("platforms", {}).get(plat)
    if tree is None:
        raise SystemExit("update-pins: %s has no tree pin for %s" % (asset, plat))
    tree["treeSha256"] = record["treeSha256"]
    tree["rawBytes"] = record["rawBytes"]
    tree["zstdBytes"] = record["zstdBytes"]
    binary = record.get("binarySha256", "")
    if binary:
        fetch = entry.get("fetch", {}).get(plat)
        if fetch is None or "sha256" not in fetch:
            raise SystemExit("update-pins: %s has no binary pin for %s" % (asset, plat))
        fetch["sha256"] = binary


def main():
    if len(sys.argv) != 3:
        raise SystemExit("usage: update-pins.py versions.json pins-dir")
    path = Path(sys.argv[1])
    pins = sorted(Path(sys.argv[2]).glob("*.json"))
    old = path.read_text()
    versions = json.loads(old)
    if json.dumps(versions, indent=2) + "\n" != old:
        raise SystemExit("update-pins: %s is not in json.dumps indent=2 form" % path)
    for pin in pins:
        update(versions, json.loads(pin.read_text()))
    text = json.dumps(versions, indent=2) + "\n"
    if text != old:
        path.write_text(text)
        print("stage-assets: updated pins in %s" % path, file=sys.stderr)
    else:
        print("stage-assets: pins unchanged in %s" % path, file=sys.stderr)


if __name__ == "__main__":
    main()

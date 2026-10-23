#!/usr/bin/env python3
import json
import re
import sys
from pathlib import Path


def asset_block(text, asset):
    assets = text.find('\n  "assets": {\n')
    if assets < 0:
        raise SystemExit("update-pins: no assets section")
    start = text.find('\n    "%s": {\n' % asset, assets)
    if start < 0:
        raise SystemExit("update-pins: no asset %s" % asset)
    start += 1
    nxt = text.find('\n    "', start + 1)
    end_assets = text.find('\n  },\n  "kcpCompat"', start + 1)
    ends = [x for x in (nxt, end_assets) if x >= 0]
    end = min(ends) + 1 if ends else len(text)
    return start, end


def update_block(block, asset, record):
    plat = record["platform"]
    tree = re.compile(
        r'^(\s*"%s": \{"treeSha256": ")[0-9a-f]{64}(", "rawBytes": )\d+(, "zstdBytes": )\d+(\},?)$'
        % re.escape(plat),
        re.MULTILINE,
    )
    block, count = tree.subn(
        lambda m: m.group(1) + record["treeSha256"] + m.group(2) + str(record["rawBytes"]) + m.group(3) + str(record["zstdBytes"]) + m.group(4),
        block,
    )
    if count != 1:
        raise SystemExit("update-pins: %s %s tree pin matched %d lines" % (asset, plat, count))
    binary = record.get("binarySha256", "")
    if binary:
        fetch = re.compile(
            r'^(\s*"%s": \{[^\n]*?"sha256": ")[0-9a-f]{64}("[^\n]*)$' % re.escape(plat),
            re.MULTILINE,
        )
        block, count = fetch.subn(lambda m: m.group(1) + binary + m.group(2), block)
        if count != 1:
            raise SystemExit("update-pins: %s %s binary pin matched %d lines" % (asset, plat, count))
    return block


def main():
    if len(sys.argv) != 3:
        raise SystemExit("usage: update-pins.py versions.json pins-dir")
    path = Path(sys.argv[1])
    pins = sorted(Path(sys.argv[2]).glob("*.json"))
    old = path.read_text()
    text = old
    for pin in pins:
        record = json.loads(pin.read_text())
        asset = record["asset"]
        start, end = asset_block(text, asset)
        text = text[:start] + update_block(text[start:end], asset, record) + text[end:]
    if text != old:
        path.write_text(text)
        print("stage-assets: updated pins in %s" % path, file=sys.stderr)
    else:
        print("stage-assets: pins unchanged in %s" % path, file=sys.stderr)


if __name__ == "__main__":
    main()

#!/usr/bin/env python3
"""Compare repeated Go benchmark output as medians, ranges and percent changes."""
import argparse
import json
from pathlib import Path
import re
import statistics


def read(path):
    rows = {}
    for line in path.read_text().splitlines():
        if not line.startswith("Benchmark"):
            continue
        fields = line.split()
        if len(fields) < 4 or not fields[1].isdigit():
            continue
        name = re.sub(r"-\d+$", "", fields[0])
        for index in range(2, len(fields) - 1, 2):
            try:
                value = float(fields[index])
            except ValueError:
                continue
            rows.setdefault(name, {}).setdefault(fields[index + 1], []).append(value)
    return rows


def summary(values):
    return {"median": statistics.median(values), "min": min(values),
            "max": max(values), "samples": len(values)}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("before", type=Path)
    parser.add_argument("after", type=Path)
    parser.add_argument("--output", type=Path)
    args = parser.parse_args()
    before, after = read(args.before), read(args.after)
    if not before or before.keys() != after.keys():
        raise SystemExit("Benchmark names do not match; do not compare different workloads")
    result = {}
    for name, metrics in before.items():
        result[name] = {}
        for unit, values in metrics.items():
            old, new = summary(values), summary(after[name][unit])
            result[name][unit] = {"before": old, "after": new,
                                  "change_percent": None if old["median"] == 0 else (new["median"] / old["median"] - 1) * 100}
    text = json.dumps(result, ensure_ascii=False, indent=2) + "\n"
    if args.output:
        args.output.write_text(text)
    else:
        print(text, end="")


if __name__ == "__main__":
    main()

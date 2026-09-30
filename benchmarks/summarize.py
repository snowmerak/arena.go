"""Summarize native Go arena benchmark output using only the standard library."""

import argparse
import json
import re
import statistics
from collections import defaultdict
from pathlib import Path


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("input", type=Path)
    parser.add_argument("--benchmark", choices=("Lifecycle", "PoolChurn"), default="Lifecycle")
    args = parser.parse_args()
    groups = defaultdict(list)
    pattern = re.compile(rf"Benchmark{args.benchmark}/N(\d+)/(\w+)-\d+\s+(\d+)\s+(.+)")
    content = args.input.read_text(encoding="utf-8-sig")
    if "PASS" not in content or "FAIL" in content:
        raise SystemExit("Input must be a completed, passing benchmark run")
    for line in content.splitlines():
        match = pattern.fullmatch(line.strip())
        if not match:
            continue
        count, mode, iterations, measurements = match.groups()
        fields = measurements.split()
        metrics = {fields[i + 1]: float(fields[i]) for i in range(0, len(fields), 2)}
        metrics["iterations"] = int(iterations)
        groups[int(count), mode].append(metrics)
    if not groups:
        raise SystemExit(f"No {args.benchmark} benchmark samples found")

    rows = []
    for (count, mode), samples in groups.items():
        medians = {key: statistics.median(sample[key] for sample in samples) for key in samples[0]}
        rows.append({
            "objects": count,
            "mode": mode,
            "samples": len(samples),
            "median": medians,
            "min_ns": min(sample["ns/op"] for sample in samples),
            "max_ns": max(sample["ns/op"] for sample in samples),
        })
    output = args.input.with_suffix(".summary.json")
    output.write_text(json.dumps(rows, indent=2) + "\n", encoding="utf-8")

    if args.benchmark == "PoolChurn":
        print("| Live slots | Mode | Samples | Median ns | Min–max ns | Bytes/op | Allocs/op |")
        print("| ---: | --- | ---: | ---: | ---: | ---: | ---: |")
        for row in rows:
            m = row["median"]
            print(f"| {row['objects']:,} | {row['mode']} | {row['samples']} | "
                  f"{m['ns/op']:.2f} | {row['min_ns']:.2f}–{row['max_ns']:.2f} | "
                  f"{m['B/op']:,.0f} | {m['allocs/op']:,.0f} |")
        print(f"\nSummary: {output}")
        return

    print("| Objects | Mode | Samples | Median ms | Min–max ms | Allocated MiB | Allocs | Retained MiB | GCs |")
    print("| ---: | --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |")
    for row in rows:
        m = row["median"]
        print(f"| {row['objects']:,} | {row['mode']} | {row['samples']} | "
              f"{m['ns/op'] / 1e6:.3f} | {row['min_ns'] / 1e6:.3f}–{row['max_ns'] / 1e6:.3f} | "
              f"{m['B/op'] / 2**20:.3f} | {m['allocs/op']:,.0f} | "
              f"{m['retained-heap-B'] / 2**20:.3f} | {m['GCs/op']:.1f} |")
    print(f"\nSummary: {output}")


if __name__ == "__main__":
    main()

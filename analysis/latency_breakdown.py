#!/usr/bin/env python3
"""Reproduce paper Fig. 3 (LLM/Harness/Tool latency share) and Fig. 4 (per-call
latency CDF) from ARIES run directories.

Data sources per task (see analysis plan for details on why these were chosen):
  - run-result.json:            harness.duration (ns) -> Total_time
  - harness/telemetry/*.jsonl:  raw OpenClaw transcript
        toolResult messages carry message.details.durationMs -> Tool_time
        assistant messages have no explicit duration -> LLM_time is derived
        from the gap to the previous event's timestamp

Harness_time is the residual: Total_time - LLM_time - Tool_time.

Caveats (see plan/README): LLM duration is derived, never a native field;
bridge/tool-calls.jsonl is intentionally not used (noisy, no shared ID with
telemetry); the OpenClaw telemetry format is internal/unversioned, so this
script defensively skips events missing expected fields instead of raising.
"""
from __future__ import annotations

import argparse
import json
import sys
from dataclasses import dataclass, field
from datetime import datetime, timezone
from pathlib import Path

NS_PER_MS = 1_000_000


def parse_ts(value: str) -> float:
    """Parse an ISO-8601 timestamp (as emitted by OpenClaw) to epoch ms."""
    dt = datetime.fromisoformat(value.replace("Z", "+00:00"))
    return dt.astimezone(timezone.utc).timestamp() * 1000.0


@dataclass
class TaskBreakdown:
    run_id: str
    task_id: str
    harness_type: str
    total_ms: float
    llm_ms: float
    tool_ms: float
    llm_call_durations_ms: list[float] = field(default_factory=list)
    tool_call_durations_ms: list[float] = field(default_factory=list)

    @property
    def harness_ms(self) -> float:
        return max(0.0, self.total_ms - self.llm_ms - self.tool_ms)


def load_run_result(run_dir: Path) -> dict | None:
    path = run_dir / "run-result.json"
    if not path.exists():
        return None
    with path.open() as f:
        return json.load(f)


def find_openclaw_telemetry_jsonl(task_dir: Path) -> Path | None:
    telemetry_dir = task_dir / "harness" / "telemetry"
    if not telemetry_dir.is_dir():
        return None
    candidates = [
        p
        for p in telemetry_dir.glob("*.jsonl")
        if "trajectory" not in p.name and p.name != "sessions.jsonl"
    ]
    if not candidates:
        return None
    # Pick the largest file in case more than one session was captured.
    return max(candidates, key=lambda p: p.stat().st_size)


def harness_type_for_task(task_dir: Path) -> str:
    if (task_dir / "harness" / "openclaw.json").exists():
        return "openclaw"
    if (task_dir / "harness" / "config.yaml").exists():
        return "hermes"
    return "hermes" if (task_dir / "harness").is_dir() else "unknown"


def compute_breakdown(run_dir: Path, task_result: dict) -> TaskBreakdown | None:
    task_id = task_result.get("task_id")
    harness = task_result.get("harness", {})
    if harness.get("status") != "succeeded":
        return None
    total_ns = harness.get("duration")
    if not isinstance(total_ns, (int, float)):
        return None
    total_ms = total_ns / NS_PER_MS

    task_dir = run_dir / task_id
    harness_type = harness_type_for_task(task_dir)

    if harness_type == "hermes":
        # Hermes writes harness/telemetry/sessions.jsonl, but every message in
        # it is timestamped at final-flush time (a whole task's ~24 messages
        # span well under a second), not at real per-event execution time.
        # There is no other source of per-call LLM/tool timing for Hermes
        # (no gateway.log equivalent), so a per-call breakdown would be
        # fabricated, not measured. Skip rather than report misleading zeros.
        print(
            f"  skip {run_dir.name}/{task_id}: Hermes telemetry has no usable "
            "per-call timestamps (all messages flushed within the same instant)",
            file=sys.stderr,
        )
        return None

    telemetry_path = find_openclaw_telemetry_jsonl(task_dir)
    if telemetry_path is None:
        print(f"  skip {run_dir.name}/{task_id}: no telemetry jsonl found", file=sys.stderr)
        return None

    events: list[tuple[float, dict]] = []
    with telemetry_path.open() as f:
        for line in f:
            line = line.strip()
            if not line:
                continue
            try:
                record = json.loads(line)
            except json.JSONDecodeError:
                continue
            if record.get("type") != "message":
                continue
            ts_raw = record.get("timestamp")
            if not ts_raw:
                continue
            try:
                ts = parse_ts(ts_raw)
            except (ValueError, TypeError):
                continue
            events.append((ts, record["message"]))
    events.sort(key=lambda e: e[0])

    tool_ms_total = 0.0
    tool_durations: list[float] = []
    llm_ms_total = 0.0
    llm_durations: list[float] = []
    prev_ts: float | None = None

    for ts, message in events:
        role = message.get("role")
        if role == "toolResult":
            duration = message.get("details", {}).get("durationMs")
            if isinstance(duration, (int, float)) and duration >= 0:
                tool_ms_total += duration
                tool_durations.append(float(duration))
            prev_ts = ts
        elif role == "assistant":
            if prev_ts is not None:
                delta = ts - prev_ts
                if delta > 0:
                    llm_ms_total += delta
                    llm_durations.append(delta)
            prev_ts = ts
        elif role == "user":
            prev_ts = ts

    return TaskBreakdown(
        run_id=run_dir.name,
        task_id=task_id,
        harness_type=harness_type,
        total_ms=total_ms,
        llm_ms=llm_ms_total,
        tool_ms=tool_ms_total,
        llm_call_durations_ms=llm_durations,
        tool_call_durations_ms=tool_durations,
    )


def collect_breakdowns(run_dirs: list[Path]) -> list[TaskBreakdown]:
    breakdowns: list[TaskBreakdown] = []
    for run_dir in run_dirs:
        result = load_run_result(run_dir)
        if result is None:
            continue
        for task_result in result.get("tasks", []):
            breakdown = compute_breakdown(run_dir, task_result)
            if breakdown is not None:
                breakdowns.append(breakdown)
    return breakdowns


@dataclass
class TaskTotal:
    harness_type: str
    task_id: str
    total_ms: float


def collect_totals(run_dirs: list[Path]) -> list[TaskTotal]:
    """Total task duration per succeeded harness run, independent of whether a
    per-call LLM/Harness/Tool breakdown could be computed (Hermes has no
    usable per-call timestamps, see compute_breakdown, but its total task
    duration from run-result.json is still a real measurement)."""
    totals: list[TaskTotal] = []
    for run_dir in run_dirs:
        result = load_run_result(run_dir)
        if result is None:
            continue
        for task_result in result.get("tasks", []):
            task_id = task_result.get("task_id")
            harness = task_result.get("harness", {})
            if harness.get("status") != "succeeded":
                continue
            total_ns = harness.get("duration")
            if not isinstance(total_ns, (int, float)):
                continue
            harness_type = harness_type_for_task(run_dir / task_id)
            totals.append(TaskTotal(harness_type, task_id, total_ns / NS_PER_MS))
    return totals


def print_summary(breakdowns: list[TaskBreakdown]) -> None:
    header = f"{'harness':<10} {'task':<28} {'total_s':>8} {'llm%':>6} {'harness%':>9} {'tool%':>6}"
    print(header)
    print("-" * len(header))
    for b in breakdowns:
        total_s = b.total_ms / 1000
        llm_pct = 100 * b.llm_ms / b.total_ms if b.total_ms else 0
        harness_pct = 100 * b.harness_ms / b.total_ms if b.total_ms else 0
        tool_pct = 100 * b.tool_ms / b.total_ms if b.total_ms else 0
        print(f"{b.harness_type:<10} {b.task_id:<28} {total_s:>8.1f} {llm_pct:>5.0f}% {harness_pct:>8.0f}% {tool_pct:>5.0f}%")


def plot_stacked_bars(breakdowns: list[TaskBreakdown], out_path: Path) -> None:
    import matplotlib

    matplotlib.use("Agg")
    import matplotlib.pyplot as plt

    by_harness: dict[str, list[TaskBreakdown]] = {}
    for b in breakdowns:
        by_harness.setdefault(b.harness_type, []).append(b)

    harnesses = sorted(by_harness)
    llm_pct, harness_pct, tool_pct = [], [], []
    for h in harnesses:
        tasks = by_harness[h]
        llm_pct.append(sum(100 * t.llm_ms / t.total_ms for t in tasks if t.total_ms) / len(tasks))
        harness_pct.append(sum(100 * t.harness_ms / t.total_ms for t in tasks if t.total_ms) / len(tasks))
        tool_pct.append(sum(100 * t.tool_ms / t.total_ms for t in tasks if t.total_ms) / len(tasks))

    fig, ax = plt.subplots(figsize=(4 + len(harnesses), 5))
    x = range(len(harnesses))
    ax.bar(x, llm_pct, label="LLM", color="#4C72B0")
    ax.bar(x, harness_pct, bottom=llm_pct, label="Harness", color="#55A868")
    bottom2 = [a + b for a, b in zip(llm_pct, harness_pct)]
    ax.bar(x, tool_pct, bottom=bottom2, label="Tool", color="#DD8452")
    ax.set_xticks(list(x))
    ax.set_xticklabels(harnesses)
    ax.set_ylabel("Share (%)")
    ax.set_ylim(0, 100)
    ax.set_title(f"End-to-end latency breakdown ({sum(len(v) for v in by_harness.values())} tasks)")
    ax.legend()
    fig.tight_layout()
    fig.savefig(out_path, dpi=150)
    print(f"wrote {out_path}")


def plot_cdf(breakdowns: list[TaskBreakdown], out_path: Path) -> None:
    import matplotlib

    matplotlib.use("Agg")
    import matplotlib.pyplot as plt

    llm_all = [d for b in breakdowns for d in b.llm_call_durations_ms if d > 0]
    tool_all = [d for b in breakdowns for d in b.tool_call_durations_ms if d > 0]

    fig, ax = plt.subplots(figsize=(6, 5))
    for label, data, color in (("LLM", llm_all, "#4C72B0"), ("Tool", tool_all, "#DD8452")):
        if not data:
            continue
        data_sorted = sorted(data)
        n = len(data_sorted)
        y = [i / n for i in range(1, n + 1)]
        ax.plot(data_sorted, y, label=f"{label} (n={n})", color=color)
    ax.set_xscale("log")
    ax.set_xlabel("Latency, ms")
    ax.set_ylabel("CDF")
    ax.set_title("Per-call latency: LLM vs Tool")
    ax.legend()
    ax.grid(True, which="both", alpha=0.3)
    fig.tight_layout()
    fig.savefig(out_path, dpi=150)
    print(f"wrote {out_path}")


def plot_total_duration_bars(totals: list[TaskTotal], out_path: Path) -> None:
    """Total task duration by harness (seconds). Unlike plot_stacked_bars this
    does not break duration down into LLM/Harness/Tool shares, so it is the
    only OpenClaw-vs-Hermes comparison available (Hermes has no per-call
    breakdown, see compute_breakdown)."""
    import matplotlib

    matplotlib.use("Agg")
    import matplotlib.pyplot as plt

    by_harness: dict[str, list[float]] = {}
    for t in totals:
        by_harness.setdefault(t.harness_type, []).append(t.total_ms / 1000)

    harnesses = sorted(by_harness)
    means = [sum(by_harness[h]) / len(by_harness[h]) for h in harnesses]
    counts = [len(by_harness[h]) for h in harnesses]

    fig, ax = plt.subplots(figsize=(4 + len(harnesses), 5))
    ax.bar(harnesses, means, color="#4C72B0")
    for i, (m, n) in enumerate(zip(means, counts)):
        ax.text(i, m, f" n={n}", va="bottom", ha="center")
    ax.set_ylabel("Mean total task duration (s)")
    ax.set_title("Total task duration by harness (no LLM/Harness/Tool split)")
    fig.tight_layout()
    fig.savefig(out_path, dpi=150)
    print(f"wrote {out_path}")


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "run_dirs",
        nargs="*",
        help="Run directories to analyze (default: all of runs/)",
    )
    parser.add_argument(
        "--runs-root",
        default=Path(__file__).resolve().parent.parent / "runs",
        type=Path,
        help="Root directory to scan when no run_dirs are given",
    )
    parser.add_argument(
        "--out-dir",
        default=Path(__file__).resolve().parent / "out",
        type=Path,
    )
    args = parser.parse_args()

    if args.run_dirs:
        run_dirs = [Path(d) for d in args.run_dirs]
    else:
        run_dirs = sorted(p for p in args.runs_root.iterdir() if p.is_dir())

    if not run_dirs:
        print("No run directories found.", file=sys.stderr)
        sys.exit(1)

    breakdowns = collect_breakdowns(run_dirs)
    if not breakdowns:
        print("No successful tasks with usable telemetry found.", file=sys.stderr)
        sys.exit(1)

    print_summary(breakdowns)

    args.out_dir.mkdir(parents=True, exist_ok=True)
    plot_stacked_bars(breakdowns, args.out_dir / "latency_breakdown.png")
    plot_cdf(breakdowns, args.out_dir / "latency_cdf.png")

    totals = collect_totals(run_dirs)
    if totals:
        plot_total_duration_bars(totals, args.out_dir / "total_duration_by_harness.png")


if __name__ == "__main__":
    main()

#!/usr/bin/env python3
"""Render the README charts from docs/results/packsim.json.

    python3 docs/charts.py             # SVGs from the committed results
    python3 docs/charts.py --refresh   # re-run cmd/packsim first and rewrite the JSON

Every number comes from cmd/packsim output: a static allocation calculation
from the planner's memory estimates. No model is loaded and no GPU utilization
is measured. The script uses only the Python standard library and writes a
light and a dark variant of each chart to docs/images/.
"""

import argparse
import json
import pathlib
import re
import subprocess
import sys
from xml.sax.saxutils import escape

ROOT = pathlib.Path(__file__).resolve().parent.parent
RESULTS = ROOT / "docs" / "results" / "packsim.json"
IMAGES = ROOT / "docs" / "images"

FLEET_CMD = ["go", "run", "./cmd/packsim"]
EXPLAIN_MODEL = "llama-3.1-8b"
EXPLAIN_CMD = ["go", "run", "./cmd/packsim", "-explain", EXPLAIN_MODEL]


# --------------------------------------------------------------------------
# Parsing packsim output
# --------------------------------------------------------------------------

def run(cmd):
    return subprocess.run(cmd, cwd=ROOT, check=True, capture_output=True, text=True).stdout


def columns(line):
    """packsim prints tabwriter tables padded with at least two spaces."""
    return re.split(r"\s{2,}", line.strip())


def table(lines, header_prefix):
    start = next(i for i, l in enumerate(lines) if l.startswith(header_prefix))
    rows = []
    for line in lines[start + 1:]:
        if not line.strip() or line.startswith("("):
            break
        rows.append(columns(line))
    return rows


def pct(s):
    return None if s == "-" else float(s.rstrip("%"))


def parse_fleet(out):
    lines = out.splitlines()
    gpu = re.search(r"GPU (\S+): (\d+) GiB, (\d+) compute slices, (\d+) memory slices", out)
    wl = re.search(r"max-model-len=(\d+) max-num-seqs=(\d+) gpu-memory-utilization=([\d.]+)", out)
    workloads = [{
        "name": r[0], "replicas": int(r[1]), "variant": r[2],
        "weights_gib": float(r[3]), "kv_cache_gib": float(r[4]), "overhead_gib": float(r[5]),
        "estimate_gib": float(r[6]), "mig_profile": r[7], "replicas_per_gpu": int(r[8]),
    } for r in table(lines, "WORKLOAD")]
    allocations = [{
        "allocation": r[0], "gpus": int(r[1]), "replicas": int(r[2]),
        "estimate_over_gpu_memory_pct": pct(r[3]),
        "mig_memory_over_gpu_memory_pct": pct(r[4]),
        "compute_slices_in_use": None if r[5] == "-" else r[5],
    } for r in table(lines, "ALLOCATION")]
    layout = []
    for line in lines[lines.index("MIG layout:") + 1:]:
        m = re.match(r"\s+gpu(\d+)\s+(.*)", line)
        if not m:
            continue
        instances = []
        for part in m.group(2).split(", "):
            placed, replica = part.split(" ")
            profile, start = placed.split("@")
            instances.append({"profile": profile, "start_memory_slice": int(start), "replica": replica})
        layout.append({"gpu": int(m.group(1)), "instances": instances})
    return {
        "gpu": {"name": gpu.group(1), "memory_gib": int(gpu.group(2)),
                "compute_slices": int(gpu.group(3)), "memory_slices": int(gpu.group(4))},
        "serving_shape": {"max_model_len": int(wl.group(1)), "max_num_seqs": int(wl.group(2)),
                          "gpu_memory_utilization": float(wl.group(3))},
        "workloads": workloads,
        "allocations": allocations,
        "mig_layout": layout,
    }


def parse_explain(out):
    rows = table(out.splitlines(), "VARIANT")
    return {
        "model": EXPLAIN_MODEL,
        "variants": [{
            "variant": r[0], "weights_gib": float(r[1]), "kv_cache_gib": float(r[2]),
            "overhead_gib": float(r[3]), "estimate_gib": float(r[4]), "smallest_profile": r[5],
            "profile_usable_gib": None if r[6] == "-" else float(r[6]),
            "replicas_per_gpu": None if r[7] == "-" else int(r[7]),
        } for r in rows],
    }


def refresh():
    data = {
        "description": ("Static allocation calculation by cmd/packsim from the planner's memory "
                        "estimates (weights + KV cache for max-num-seqs sequences at max-model-len + "
                        "fixed 1.5 GiB runtime overhead). No model was loaded and no GPU utilization "
                        "was measured. Memory values are GiB."),
        "commands": {"fleet": " ".join(FLEET_CMD), "explain": " ".join(EXPLAIN_CMD)},
        "fleet": parse_fleet(run(FLEET_CMD)),
        "explain": parse_explain(run(EXPLAIN_CMD)),
    }
    RESULTS.parent.mkdir(parents=True, exist_ok=True)
    RESULTS.write_text(json.dumps(data, indent=2) + "\n")
    print(f"wrote {RESULTS.relative_to(ROOT)}")


# --------------------------------------------------------------------------
# SVG rendering
# --------------------------------------------------------------------------

THEMES = {
    "light": {
        "surface": "#fcfcfb", "border": "#e1e0d9", "ink": "#0b0b0b", "ink2": "#52514e",
        "muted": "#898781", "grid": "#e1e0d9", "axis": "#c3c2b7", "context": "#b9b7ae",
        "series": ["#2a78d6", "#eb6834", "#1baf7a"],
    },
    "dark": {
        "surface": "#1a1a19", "border": "#2c2c2a", "ink": "#f0efec", "ink2": "#c3c2b7",
        "muted": "#898781", "grid": "#2c2c2a", "axis": "#383835", "context": "#5d5c57",
        "series": ["#3987e5", "#d95926", "#199e70"],
    },
}
FONT = "system-ui, -apple-system, 'Segoe UI', Helvetica, Arial, sans-serif"
WIDTH = 720


def text(x, y, s, fill, size=13, weight=400, anchor="start", halo=None, more=()):
    """A text run; `more` holds (string, fill, weight) spans that flow on after it.
    `halo` paints a surface-colored outline so lines behind the label stay out of it."""
    attrs = ""
    if halo:
        attrs = f' stroke="{halo}" stroke-width="5" stroke-linejoin="round" paint-order="stroke"'
    spans = "".join(f'<tspan fill="{f}" font-weight="{w}">{escape(m)}</tspan>' for m, f, w in more)
    return (f'<text x="{x:.1f}" y="{y:.1f}" fill="{fill}" font-size="{size}" '
            f'font-weight="{weight}" text-anchor="{anchor}"{attrs}>{escape(s)}{spans}</text>')


def bar(x, y, w, h, fill, rounded_end, tooltip):
    """A horizontal bar segment; the data end gets a 4px radius, the baseline stays square."""
    title = f"<title>{escape(tooltip)}</title>"
    if not rounded_end or w <= 4:
        return f'<rect x="{x:.1f}" y="{y:.1f}" width="{w:.1f}" height="{h}" fill="{fill}">{title}</rect>'
    r = 4
    d = (f"M{x:.1f},{y:.1f} H{x + w - r:.1f} Q{x + w:.1f},{y:.1f} {x + w:.1f},{y + r:.1f} "
         f"V{y + h - r:.1f} Q{x + w:.1f},{y + h:.1f} {x + w - r:.1f},{y + h:.1f} H{x:.1f} Z")
    return f'<path d="{d}" fill="{fill}">{title}</path>'


def frame(height, t, body, label):
    return (f'<svg xmlns="http://www.w3.org/2000/svg" width="{WIDTH}" height="{height}" '
            f'viewBox="0 0 {WIDTH} {height}" role="img" aria-label="{escape(label)}" '
            f'font-family="{FONT}">\n'
            f'<rect x="0.5" y="0.5" width="{WIDTH - 1}" height="{height - 1}" rx="8" '
            f'fill="{t["surface"]}" stroke="{t["border"]}"/>\n' + "\n".join(body) + "\n</svg>\n")


def x_axis(body, t, x0, x1, top, bottom, vmax, step, unit):
    scale = (x1 - x0) / vmax
    v = 0
    while v <= vmax + 1e-9:
        x = x0 + v * scale
        body.append(f'<line x1="{x:.1f}" y1="{top}" x2="{x:.1f}" y2="{bottom}" '
                    f'stroke="{t["axis"] if v == 0 else t["grid"]}" stroke-width="1"/>')
        body.append(text(x, bottom + 16, f"{v:g}", t["muted"], 11, anchor="middle"))
        v += step
    body.append(text(x1, bottom + 32, unit, t["muted"], 11, anchor="end"))
    return scale


def gpus_chart(data, t):
    fleet = data["fleet"]
    gpu = fleet["gpu"]["name"]
    replicas = fleet["allocations"][0]["replicas"]
    rows = [
        ("One replica per whole GPU", fleet["allocations"][0], t["context"]),
        ("MIG-packed (first-fit decreasing)", fleet["allocations"][1], t["series"][0]),
    ]
    x0, x1 = 24, 600
    vmax = max(a["gpus"] for _, a, _ in rows)
    vmax = (vmax + 4) // 5 * 5
    body = [
        text(24, 34, f"{gpu} GPUs needed for the {replicas}-replica sample fleet", t["ink"], 15, 600),
        text(24, 54, "Static allocation from the planner's memory estimates (cmd/packsim), "
                     "not measured GPU utilization", t["ink2"], 12),
    ]
    top, pitch, bh = 78, 54, 22
    bottom = top + pitch * len(rows) + 4
    scale = x_axis(body, t, x0, x1, top, bottom, vmax, 5, "GPUs")
    for i, (label, a, color) in enumerate(rows):
        y = top + i * pitch + 22
        w = a["gpus"] * scale
        body.append(text(x0, y - 7, label, t["ink2"], 13, halo=t["surface"], more=[
            (f"  ·  planner estimates fill {a['estimate_over_gpu_memory_pct']}% of their memory",
             t["muted"], 400)]))
        tip = (f"{label}: {a['gpus']} GPUs for {a['replicas']} replicas; planner estimates "
               f"use {a['estimate_over_gpu_memory_pct']}% of their memory")
        body.append(bar(x0, y, w, bh, color, True, tip))
        body.append(text(x0 + w + 8, y + 16, f"{a['gpus']} GPUs", t["ink"], 14, 600))
    alt = (f"{rows[0][1]['gpus']} GPUs with one replica per whole GPU versus "
           f"{rows[1][1]['gpus']} GPUs when the same {replicas} replicas are packed onto MIG instances")
    return frame(bottom + 44, t, body, alt)


def memory_chart(data, t):
    ex = data["explain"]
    shape = data["fleet"]["serving_shape"]
    variants = ex["variants"]
    parts = [("weights_gib", "Weights"), ("kv_cache_gib", "KV cache"), ("overhead_gib", "Runtime overhead")]
    x0, x1 = 104, 620
    vmax = 40
    body = [
        text(24, 34, "Llama-3.1-8B: per-replica memory estimate by checkpoint", t["ink"], 15, 600),
        text(24, 54, f"max-model-len {shape['max_model_len']}, max-num-seqs {shape['max_num_seqs']}, "
                     f"{data['fleet']['gpu']['name']}  ·  static calculation, nothing was loaded",
             t["ink2"], 12),
    ]
    # Legend: swatch beside ink-colored text.
    lx = 24
    for (key, name), color in zip(parts, t["series"]):
        body.append(f'<rect x="{lx}" y="70" width="12" height="12" rx="2" fill="{color}"/>')
        body.append(text(lx + 18, 80, name, t["ink2"], 12))
        lx += 18 + 7 * len(name) + 22
    top, pitch, bh = 112, 36, 18
    bottom = top + pitch * len(variants)
    scale = x_axis(body, t, x0, x1, top, bottom, vmax, 10, "GiB")

    # Layers, bottom to top: bars, capacity lines, value labels (with a surface halo).
    marks, labels = [], []
    for i, v in enumerate(variants):
        y = top + i * pitch + (pitch - bh) / 2
        labels.append(text(x0 - 12, y + 13, v["variant"], t["ink2"], 13, anchor="end"))
        x = x0
        for j, ((key, name), color) in enumerate(zip(parts, t["series"])):
            w = v[key] * scale
            last = j == len(parts) - 1
            seg = w - (0 if last else 2)  # 2px surface gap between stacked segments
            marks.append(bar(x, y, seg, bh, color, last, f"{v['variant']} {name}: {v[key]:.2f} GiB"))
            x += w
        labels.append(text(x + 8, y + 13, f"{v['estimate_gib']:.2f} GiB", t["ink"], 13, 600,
                           halo=t["surface"], more=[(f"  \u2192 {v['smallest_profile']}", t["muted"], 400)]))

    # Usable capacity (90% of the profile's memory) of the profiles the variants land on,
    # drawn over the bars so a bar that exceeds it is visibly crossed.
    lines = []
    caps = sorted({(v["profile_usable_gib"], v["smallest_profile"]) for v in variants
                   if v["profile_usable_gib"] is not None})
    for usable, profile in caps:
        x = x0 + usable * scale
        lines.append(f'<line x1="{x:.1f}" y1="{top - 8}" x2="{x:.1f}" y2="{bottom}" '
                     f'stroke="{t["ink2"]}" stroke-width="1.5"/>')
        labels.append(text(x, top - 13, f"{profile} usable: {usable:.2f} GiB", t["ink2"], 11, 600, "middle"))
    body += marks + lines + labels
    alt = "; ".join(f"{v['variant']} {v['estimate_gib']:.2f} GiB on {v['smallest_profile']}" for v in variants)
    return frame(bottom + 44, t, body, "Llama-3.1-8B memory estimate per checkpoint: " + alt)


def render():
    data = json.loads(RESULTS.read_text())
    IMAGES.mkdir(parents=True, exist_ok=True)
    for name, fn in (("packsim-gpus", gpus_chart), ("llama-8b-memory", memory_chart)):
        for mode, theme in THEMES.items():
            path = IMAGES / f"{name}-{mode}.svg"
            path.write_text(fn(data, theme))
            print(f"wrote {path.relative_to(ROOT)}")


def main():
    p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument("--refresh", action="store_true", help="re-run cmd/packsim and rewrite the results JSON")
    args = p.parse_args()
    if args.refresh:
        refresh()
    if not RESULTS.exists():
        sys.exit(f"{RESULTS} is missing; run with --refresh")
    render()


if __name__ == "__main__":
    main()

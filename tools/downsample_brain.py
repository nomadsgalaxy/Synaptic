"""
Downsample the source brain point cloud into 4 quality tiers for Synaptic.

Usage:
    python downsample_brain.py path/to/Brain_pc_coldhot_ed.ply

Source mesh:
    "Brain Point Cloud" by Terrie Simmons-Ehrhardt
    https://skfb.ly/oQsLq
    Licensed under CC-BY 4.0 (http://creativecommons.org/licenses/by/4.0/)

Outputs (written to ../assets/ relative to this script):
    brain_low.ply    ~30k  vertices  (Raspberry Pi tier)
    brain_med.ply   ~120k  vertices  (default)
    brain_high.ply  ~400k  vertices  (discrete GPU)
    brain_ultra.ply  full  vertices  (gaming rig / show off)
"""

import argparse
import sys
from pathlib import Path
import numpy as np

TIERS = {
    "low":   30_000,
    "med":   120_000,
    "high":  400_000,
    "ultra": None,  # full
}

VERTEX_DTYPE = np.dtype([
    ("x", "<f4"), ("y", "<f4"), ("z", "<f4"),
    ("r", "u1"), ("g", "u1"), ("b", "u1"), ("a", "u1"),
])

def read_ply(path: Path):
    with path.open("rb") as f:
        header_lines = []
        while True:
            line = f.readline()
            header_lines.append(line)
            if line.strip() == b"end_header":
                break
        header_text = b"".join(header_lines).decode("ascii", errors="replace")
        n_vertices = None
        for line in header_text.splitlines():
            if line.startswith("element vertex"):
                n_vertices = int(line.split()[-1])
                break
        if n_vertices is None:
            raise ValueError("no vertex count in PLY header")
        verts = np.fromfile(f, dtype=VERTEX_DTYPE, count=n_vertices)
        if verts.shape[0] != n_vertices:
            raise ValueError(f"expected {n_vertices} vertices, got {verts.shape[0]}")
    return verts

def write_ply(path: Path, verts: np.ndarray):
    n = verts.shape[0]
    header = (
        "ply\n"
        "format binary_little_endian 1.0\n"
        "comment Synaptic downsample of Brain_pc_coldhot_ed.ply\n"
        "comment Source: T. Simmons-Ehrhardt, https://skfb.ly/oQsLq, CC-BY 4.0\n"
        f"element vertex {n}\n"
        "property float x\n"
        "property float y\n"
        "property float z\n"
        "property uchar red\n"
        "property uchar green\n"
        "property uchar blue\n"
        "property uchar alpha\n"
        "element face 0\n"
        "property list uchar int vertex_indices\n"
        "end_header\n"
    ).encode("ascii")
    with path.open("wb") as f:
        f.write(header)
        verts.tofile(f)

def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("source", type=Path, help="Path to Brain_pc_coldhot_ed.ply")
    ap.add_argument("--out", type=Path, default=None,
                    help="Output directory (default: ../assets relative to this script)")
    ap.add_argument("--seed", type=int, default=42, help="RNG seed for reproducible sampling")
    args = ap.parse_args()

    if not args.source.is_file():
        sys.exit(f"source not found: {args.source}")

    out_dir = args.out or (Path(__file__).resolve().parent.parent / "assets")
    out_dir.mkdir(parents=True, exist_ok=True)

    print(f"Reading {args.source} ...", flush=True)
    verts = read_ply(args.source)
    n_total = verts.shape[0]
    print(f"  {n_total:,} vertices loaded", flush=True)

    rng = np.random.default_rng(seed=args.seed)
    for tier_name, target in TIERS.items():
        out = out_dir / f"brain_{tier_name}.ply"
        if target is None or target >= n_total:
            sample = verts
        else:
            idx = rng.choice(n_total, size=target, replace=False)
            idx.sort()
            sample = verts[idx]
        write_ply(out, sample)
        size_mb = out.stat().st_size / 1024 / 1024
        print(f"  {tier_name:5s}  {sample.shape[0]:>8,} vertices  {size_mb:5.1f} MB  -> {out.name}")

    print("Done.")

if __name__ == "__main__":
    main()

"""
Sample the Allen atlas down to a colored overlay point cloud the dashboard
can render as a calibration reference. Each high-level region gets a
distinct color; the user drags rotation/scale/translation sliders in the
dashboard until this overlay lines up with the Simmons-Ehrhardt mesh.

Output: assets/allen_overlay.ply (binary little-endian, ~25k points)

The output is in Allen RAS+ world coordinates (mm). The dashboard's
calibration transform converts these to PLY scaffold space.
"""
from __future__ import annotations
import csv
from pathlib import Path
from collections import Counter
import nibabel as nib
import numpy as np

ROOT = Path("/sessions/funny-peaceful-meitner/mnt/Synaptic Disorder")
ALLEN_DIR = Path("/sessions/funny-peaceful-meitner/mnt/outputs/allen")
OUT = ROOT / "assets" / "allen_overlay.ply"
TARGET_POINTS = 25_000

ANCHORS = {
    "frontal_lobe":     [12113],
    "parietal_lobe":    [12131],
    "temporal_lobe":    [12139],
    "visual_cortex":    [12148],
    "cerebellum":       [10656],
    "hippocampus":      [10294],
    "amygdala":         [10361],
    "corpus_callosum":  [10561],
    "brain_stem":       [10648, 10661],
    "thalamus":         [10390],
}

REGION_ORDER = [
    "cerebellum", "brain_stem", "corpus_callosum",
    "hippocampus", "amygdala", "thalamus",
    "visual_cortex", "parietal_lobe", "temporal_lobe", "frontal_lobe",
]

REGION_COLORS = {
    "frontal_lobe":     (90, 170, 255),
    "parietal_lobe":    (140, 230, 140),
    "temporal_lobe":    (245, 220, 120),
    "visual_cortex":    (240, 130, 110),
    "cerebellum":       (190, 140, 230),
    "brain_stem":       (90, 200, 180),
    "hippocampus":      (240, 130, 200),
    "amygdala":         (240, 170, 90),
    "corpus_callosum":  (240, 240, 240),
    "thalamus":         (180, 160, 130),
    "_unmapped":        (90,  90,  90),
}

def load_ontology(p):
    d = {}
    with p.open() as f:
        for row in csv.DictReader(f):
            sid = int(row["id"])
            d[sid] = {
                "name": row["name"].lower(),
                "path": [int(x) for x in row["structure_id_path"].strip("/").split("/") if x],
            }
    return d

def build_label_to_region(ontology, present_labels):
    out = {}
    for sid in present_labels:
        info = ontology.get(sid)
        if not info: continue
        path_set = set(info["path"]) | {sid}
        for region in REGION_ORDER:
            if path_set & set(ANCHORS[region]):
                out[sid] = region
                break
    return out

def write_ply_xyzrgb(path, xyz, rgb):
    n = xyz.shape[0]
    header = (
        "ply\n"
        "format binary_little_endian 1.0\n"
        "comment Allen atlas overlay sampled from annotation_full.nii.gz\n"
        "comment Allen Human Reference Atlas - 3D, 2020 (RRID:SCR_017764, CC BY 4.0)\n"
        "comment Ding et al. 2020; ontology Ding et al. 2016 DOI 10.1002/cne.24080\n"
        f"element vertex {n}\n"
        "property float x\nproperty float y\nproperty float z\n"
        "property uchar red\nproperty uchar green\nproperty uchar blue\nproperty uchar alpha\n"
        "element face 0\n"
        "property list uchar int vertex_indices\n"
        "end_header\n"
    ).encode("ascii")
    dtype = np.dtype([("x","<f4"),("y","<f4"),("z","<f4"),
                      ("r","u1"),("g","u1"),("b","u1"),("a","u1")])
    buf = np.zeros(n, dtype=dtype)
    buf["x"] = xyz[:,0]; buf["y"] = xyz[:,1]; buf["z"] = xyz[:,2]
    buf["r"] = rgb[:,0]; buf["g"] = rgb[:,1]; buf["b"] = rgb[:,2]
    buf["a"] = 255
    with path.open("wb") as f:
        f.write(header)
        buf.tofile(f)

def main():
    print("Loading Allen annotation_full and ontology...", flush=True)
    img = nib.load(ALLEN_DIR / "annotation_full.nii.gz")
    data = img.get_fdata().astype(np.int32)
    affine = img.affine
    ontology = load_ontology(ALLEN_DIR / "voxel_count.csv")

    print("Mirroring hemispheres to fill any unlabeled voxels...", flush=True)
    mirrored = np.flip(data, axis=0)
    data = np.where(data > 0, data, mirrored)

    present = set(int(l) for l in np.unique(data) if l > 0)
    label_to_region = build_label_to_region(ontology, present)
    print(f"  {len(label_to_region)} of {len(present)} labels rolled up to a region")

    print("Selecting labeled voxels...", flush=True)
    ijk = np.argwhere(data > 0)
    print(f"  {len(ijk):,} labeled voxels total")

    rng = np.random.default_rng(42)
    if len(ijk) > TARGET_POINTS:
        idx = rng.choice(len(ijk), TARGET_POINTS, replace=False)
        ijk = ijk[idx]

    labels = data[ijk[:,0], ijk[:,1], ijk[:,2]]
    region_keys = [label_to_region.get(int(l), "_unmapped") for l in labels]
    rgb = np.array([REGION_COLORS[r] for r in region_keys], dtype=np.uint8)

    homog = np.column_stack([ijk.astype(np.float32), np.ones(len(ijk), dtype=np.float32)])
    world = (affine @ homog.T).T[:, :3].astype(np.float32)

    OUT.parent.mkdir(parents=True, exist_ok=True)
    write_ply_xyzrgb(OUT, world, rgb)
    sz = OUT.stat().st_size / 1024
    print(f"\nWrote {OUT}  ({sz:.0f} KB)")
    counts = Counter(region_keys)
    for k, n in counts.most_common():
        print(f"  {k:18s}  {n:>6,} pts  color={REGION_COLORS[k]}")

if __name__ == "__main__":
    main()

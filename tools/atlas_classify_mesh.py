"""
Use the Allen Human Reference Atlas to label each point in our brain point
cloud with an anatomical region.

Workflow:
  1. Load Simmons-Ehrhardt PLY (medium tier — ~120k points).
  2. Load Allen NIfTI annotation volume (141 structure labels in scanner coords).
  3. Load Allen ontology (voxel_count.csv) to map every detailed structure ID
     to one of our 14 high-level region keys via the structure_id_path hierarchy.
  4. Estimate the transform from PLY native axes to Allen RAS+ world coords
     (axis swap + bounding-box align). Two hemisphere orientations are tried;
     the one producing the most coherent left/right structure wins.
  5. For each PLY point, transform to Allen voxel space, sample the label,
     map up the hierarchy to a region key.
  6. Emit assets/labeled_points.json with everything the dashboard needs:
       - points:  Float32 positions in unit-cube coords (matches existing mesh)
       - regions: Uint8 region indices (parallel array)
       - region_index: { region_key: index }
       - region_centroids: { region_key: [x,y,z] } in unit-cube coords
       - region_point_indices: { region_key: [int, ...] }   (memory placement)

Citations baked into output:
  Simmons-Ehrhardt brain mesh — CC BY 4.0, https://skfb.ly/oQsLq
  Allen Human Reference Atlas – 3D, 2020 (RRID:SCR_017764) — CC BY 4.0
    Ding et al. (2020); ontology Ding et al. (2016) DOI 10.1002/cne.24080
"""

from __future__ import annotations
import csv
import json
import sys
from pathlib import Path
from collections import defaultdict, Counter

import numpy as np
import nibabel as nib

ROOT = Path("/sessions/funny-peaceful-meitner/mnt/Synaptic Disorder")
ALLEN_DIR = Path("/sessions/funny-peaceful-meitner/mnt/outputs/allen")
SOURCE_PLY = ROOT / "assets" / "brain_med.ply"
OUT = ROOT / "assets" / "labeled_points.json"

# Our 14 region keys, ordered. The dashboard already uses these names.
REGION_KEYS = [
    "prefrontal_cortex", "frontal_lobe", "broca_area", "wernicke_area",
    "visual_cortex", "temporal_lobe_left", "temporal_lobe_right",
    "hippocampus", "parietal_lobe", "motor_cortex",
    "cerebellum", "amygdala", "corpus_callosum", "brain_stem",
]
REGION_INDEX = {k: i for i, k in enumerate(REGION_KEYS)}
UNMAPPED = 255  # Reserved label for unclassified points.

# Anchor acronyms / search terms in the Allen ontology. The classifier walks
# the structure_id_path of each label. The first anchor whose ID appears in
# the path wins. Order matters — more specific anchors come first.
#
# (We classify L/R temporal lobes by point X coordinate after assignment.)
ANCHOR_RULES = [
    # Brainstem comes first to catch mesencephalon / pons / medulla
    ("brain_stem",        ["mesencephalon", "pons", "medulla", "brainstem"]),
    ("cerebellum",        ["cerebellum"]),
    ("hippocampus",       ["hippocampal formation", "hippocampus"]),
    ("amygdala",          ["amygdaloid complex", "amygdala"]),
    ("corpus_callosum",   ["corpus callosum"]),
    # Specific cortical areas before the general lobe rules
    ("broca_area",        ["inferior frontal gyrus, opercular",
                           "inferior frontal gyrus, triangular"]),
    ("wernicke_area",     ["posterior superior temporal",
                           "supramarginal gyrus", "angular gyrus"]),
    ("motor_cortex",      ["precentral gyrus", "primary motor",
                           "supplementary motor"]),
    ("visual_cortex",     ["occipital lobe", "striate", "calcarine",
                           "lingual gyrus", "cuneus"]),
    ("prefrontal_cortex", ["superior frontal gyrus", "middle frontal gyrus",
                           "frontal pole"]),
    ("parietal_lobe",     ["parietal lobe", "postcentral", "precuneus",
                           "supraparietal", "supramarginal", "angular"]),
    # Temporal lobe is split L/R later by X coordinate
    ("temporal_lobe_left",["temporal lobe", "temporal gyrus", "fusiform"]),
    # Frontal lobe is the catch-all for remaining frontal cortex
    ("frontal_lobe",      ["frontal lobe", "frontal gyrus", "frontal cortex",
                           "cingulate gyrus, rostral", "paracingulate"]),
]

VERTEX_DTYPE = np.dtype([
    ("x", "<f4"), ("y", "<f4"), ("z", "<f4"),
    ("r", "u1"), ("g", "u1"), ("b", "u1"), ("a", "u1"),
])

def load_ply_xyz(path: Path) -> np.ndarray:
    """Return Nx3 float32 of vertex positions from binary PLY."""
    with path.open("rb") as f:
        header = []
        while True:
            line = f.readline()
            header.append(line)
            if line.strip() == b"end_header":
                break
        n = None
        for h in header:
            s = h.decode("ascii", errors="replace")
            if s.startswith("element vertex"):
                n = int(s.split()[-1])
                break
        verts = np.fromfile(f, dtype=VERTEX_DTYPE, count=n)
    return np.column_stack([verts["x"], verts["y"], verts["z"]]).astype(np.float32)

def load_ontology(csv_path: Path) -> dict:
    """id -> {acronym, name, parent, path: [int]}"""
    d = {}
    with csv_path.open() as f:
        for row in csv.DictReader(f):
            sid = int(row["id"])
            d[sid] = {
                "acronym": row["acronym"],
                "name": row["name"].lower(),
                "parent": int(float(row["parent_structure_id"])) if row["parent_structure_id"] else None,
                "path": [int(x) for x in row["structure_id_path"].strip("/").split("/") if x],
            }
    return d

def find_anchor_ids(ontology: dict, search_terms: list) -> set:
    """Return all structure IDs whose name contains any of the search terms."""
    out = set()
    for sid, info in ontology.items():
        for term in search_terms:
            if term.lower() in info["name"]:
                out.add(sid)
                break
    return out

def build_label_to_region(ontology: dict, present_labels: set) -> dict:
    """For each present label, decide which region key it belongs to."""
    # Pre-compute anchor ID sets for each rule
    rule_anchors = []
    for region_key, terms in ANCHOR_RULES:
        anchor_ids = find_anchor_ids(ontology, terms)
        rule_anchors.append((region_key, anchor_ids))

    label_to_region = {}
    for sid in present_labels:
        info = ontology.get(sid)
        if not info:
            continue
        path_set = set(info["path"])
        # Walk the rules in order; first anchor present in the label's path wins
        chosen = None
        for region_key, anchors in rule_anchors:
            if path_set & anchors:
                chosen = region_key
                break
        if chosen:
            label_to_region[sid] = chosen
    return label_to_region

def estimate_transform(ply_pts: np.ndarray, atlas_world_min: np.ndarray,
                       atlas_world_max: np.ndarray):
    """Return a 4x4 matrix mapping PLY native -> Allen RAS world.

    PLY native axes (empirically determined earlier):
        +X = posterior, +Y = inferior, +Z = lateral
    Allen world (RAS+):
        +X = right, +Y = anterior, +Z = superior

    So the axis re-mapping is:
        Allen X = ±PLY Z          (pick sign by trying both, keep the one
                                   whose left/right hemisphere assignment
                                   produces a coherent split — see caller)
        Allen Y = -PLY X
        Allen Z = -PLY Y
    Then scale + translate so the post-rotation PLY bbox matches the atlas.
    """
    R = np.array([
        [ 0,  0,  1],   # Allen X  ←   +PLY Z
        [-1,  0,  0],   # Allen Y  ←   -PLY X
        [ 0, -1,  0],   # Allen Z  ←   -PLY Y
    ], dtype=np.float64)
    rotated = ply_pts @ R.T
    rmin = rotated.min(axis=0)
    rmax = rotated.max(axis=0)

    atlas_span = atlas_world_max - atlas_world_min
    ply_span = rmax - rmin
    scale = atlas_span / np.where(ply_span > 1e-6, ply_span, 1.0)
    # Use a single isotropic scale (avoid distorting the brain). Pick the
    # smallest so the brain definitely fits inside the atlas bounds.
    s = float(scale.min())
    translation = atlas_world_min - rmin * s

    M = np.eye(4)
    M[:3, :3] = R * s
    M[:3, 3] = translation
    return M

def main():
    if not ALLEN_DIR.exists():
        sys.exit(f"Allen data not found at {ALLEN_DIR}. Download annotation.nii.gz "
                 f"and voxel_count.csv from the Allen archive first.")

    print("Loading Allen annotation volume…", flush=True)
    img = nib.load(ALLEN_DIR / "annotation.nii.gz")
    data = img.get_fdata().astype(np.int32)
    affine = img.affine
    inv_affine = np.linalg.inv(affine)
    print(f"  shape={data.shape}  voxel size={img.header.get_zooms()}mm")
    present = set(int(l) for l in np.unique(data) if l > 0)
    print(f"  {len(present)} structure labels present")

    print("Loading Allen ontology (voxel_count.csv)…", flush=True)
    ontology = load_ontology(ALLEN_DIR / "voxel_count.csv")
    print(f"  {len(ontology)} ontology entries")

    print("Mapping atlas labels → region keys…", flush=True)
    label_to_region = build_label_to_region(ontology, present)
    counts = Counter(label_to_region.values())
    unmapped = present - set(label_to_region.keys())
    print(f"  mapped: {len(label_to_region)} / {len(present)}  (unmapped {len(unmapped)})")
    for k in REGION_KEYS:
        print(f"    {k:22s}  {counts.get(k, 0):>4d} labels")
    if unmapped:
        sample = list(unmapped)[:8]
        print(f"  unmapped sample: {sample}  (e.g. {ontology.get(sample[0], {}).get('name')!r})")

    print("Loading brain mesh (medium tier)…", flush=True)
    ply = load_ply_xyz(SOURCE_PLY)
    print(f"  {ply.shape[0]:,} points  ply bbox min={ply.min(0)} max={ply.max(0)}")

    # Compute Allen brain bbox in world coords (where labels > 0)
    print("Computing Allen brain bbox in world coords…", flush=True)
    ijk_brain = np.argwhere(data > 0)
    sample_ijk = ijk_brain[np.random.default_rng(0).choice(len(ijk_brain),
                                                            size=min(80_000, len(ijk_brain)),
                                                            replace=False)]
    sample_world = (affine @ np.column_stack([sample_ijk, np.ones(len(sample_ijk))]).T).T[:, :3]
    atlas_min = sample_world.min(0)
    atlas_max = sample_world.max(0)
    print(f"  atlas world bbox: min={atlas_min} max={atlas_max}")

    # Transform each PLY point into Allen world coords
    print("Transforming PLY → Allen world coords…", flush=True)
    M = estimate_transform(ply, atlas_min, atlas_max)
    ply_world = (M[:3, :3] @ ply.T).T + M[:3, 3]
    print(f"  ply→world bbox: min={ply_world.min(0)} max={ply_world.max(0)}")

    # World → voxel indices
    print("Sampling labels for each PLY point…", flush=True)
    ones = np.ones((ply_world.shape[0], 1))
    homog = np.column_stack([ply_world, ones])
    ijk_f = (inv_affine @ homog.T).T[:, :3]
    ijk = np.round(ijk_f).astype(np.int64)
    sx, sy, sz = data.shape
    inside = ((ijk[:, 0] >= 0) & (ijk[:, 0] < sx) &
              (ijk[:, 1] >= 0) & (ijk[:, 1] < sy) &
              (ijk[:, 2] >= 0) & (ijk[:, 2] < sz))
    labels = np.zeros(ply.shape[0], dtype=np.int32)
    labels[inside] = data[ijk[inside, 0], ijk[inside, 1], ijk[inside, 2]]

    # If a point misses the atlas (label 0), search a small neighborhood
    misses = np.where(labels == 0)[0]
    if len(misses) > 0:
        print(f"  {len(misses):,} points missed an atlas label, searching nearby voxels…")
        for pi in misses:
            ix, iy, iz = ijk[pi]
            best = 0
            for dx in (-1, 0, 1):
                for dy in (-1, 0, 1):
                    for dz in (-1, 0, 1):
                        x, y, z = ix + dx, iy + dy, iz + dz
                        if 0 <= x < sx and 0 <= y < sy and 0 <= z < sz:
                            v = int(data[x, y, z])
                            if v > 0:
                                best = v
                                break
                    if best: break
                if best: break
            labels[pi] = best

    region_index = np.full(ply.shape[0], UNMAPPED, dtype=np.uint8)
    label_set = set(label_to_region.keys())
    for i, lab in enumerate(labels):
        if lab in label_set:
            region_index[i] = REGION_INDEX[label_to_region[int(lab)]]

    # Split temporal lobe by hemisphere (X coord in Allen world; +X = right)
    tl_idx = REGION_INDEX["temporal_lobe_left"]
    tr_idx = REGION_INDEX["temporal_lobe_right"]
    is_temporal = (region_index == tl_idx)
    right_mask = is_temporal & (ply_world[:, 0] > 0)
    region_index[right_mask] = tr_idx

    # Stats
    print("Final region distribution:")
    counts = Counter()
    for r in region_index:
        counts[REGION_KEYS[r] if r != UNMAPPED else "(unmapped)"] += 1
    for k, v in counts.most_common():
        print(f"  {k:22s}  {v:>8,}")

    # Build region_point_indices and centroids in unit-cube coords
    # (renderer normalizes the mesh into roughly [-1,1]^3 — we mirror that here)
    span = max(ply.max(axis=0) - ply.min(axis=0))
    scale = 1.6 / span
    center = (ply.min(0) + ply.max(0)) / 2
    unit_pts = (ply - center) * scale

    region_point_indices = {k: [] for k in REGION_KEYS}
    for i in range(ply.shape[0]):
        ridx = region_index[i]
        if ridx != UNMAPPED:
            region_point_indices[REGION_KEYS[ridx]].append(int(i))

    region_centroids = {}
    for k in REGION_KEYS:
        idxs = region_point_indices[k]
        if idxs:
            pts = unit_pts[idxs]
            region_centroids[k] = [float(v) for v in pts.mean(axis=0)]
        else:
            region_centroids[k] = [0.0, 0.0, 0.0]

    # Output. Use lists (not numpy arrays) so JSON writes cleanly.
    out_data = {
        "schema_version": "1.0",
        "comment": ("Each PLY point labeled with an anatomical region using the "
                    "Allen Human Reference Atlas - 3D, 2020 (RRID:SCR_017764, CC BY 4.0). "
                    "Brain mesh by T. Simmons-Ehrhardt (https://skfb.ly/oQsLq, CC BY 4.0)."),
        "source_ply": SOURCE_PLY.name,
        "n_points": int(ply.shape[0]),
        "regions": REGION_KEYS,
        "region_index": REGION_INDEX,
        "region_centroids": region_centroids,
        "region_point_indices": region_point_indices,
    }
    OUT.parent.mkdir(parents=True, exist_ok=True)
    with OUT.open("w", encoding="utf-8") as f:
        json.dump(out_data, f)
    sz = OUT.stat().st_size / 1024
    print(f"\nWrote {OUT}  ({sz:.0f} KB)")
    for k in REGION_KEYS:
        n = len(region_point_indices[k])
        c = region_centroids[k]
        print(f"  {k:22s}  {n:>8,} pts  centroid ({c[0]:+.2f}, {c[1]:+.2f}, {c[2]:+.2f})")

if __name__ == "__main__":
    main()

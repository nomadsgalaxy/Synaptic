"""
Build the Synaptic brain data from the Allen Human Reference Atlas.

This replaces the previous Simmons-Ehrhardt-as-scaffold approach. The Allen
atlas is itself a dense, labeled point cloud (in voxel form), so we use it
directly:

  * Every rendered voxel is anatomically labeled — no alignment needed.
  * Memory neurons are placed on actual anatomical voxels using farthest-
    point sampling within each region (spreads them out organically).
  * Synapses are paths through nearby voxels — when a synapse fires, the
    dashboard animates a pulse traveling voxel-by-voxel like a real
    brainwave moving through tissue.
  * Adaptive density: regions with more memories get more rendered voxels
    so dense areas don't look sparse and quiet ones don't get crowded.

Inputs (in /sessions/funny-peaceful-meitner/mnt/outputs/allen):
  annotation_full.nii.gz   Allen 3D annotation volume (CC BY 4.0)
  voxel_count.csv          Allen ontology with structure_id_path

Outputs (assets/):
  voxels.json     ~80k labeled voxels in unit-cube coords + per-region indexing
  memories.json   each memory mapped to a voxel index + region
  synapses.json   each edge has path_voxels = [voxel_idx, ...] (8–14 hops)

Citations baked into the file headers:
  Allen Human Reference Atlas — 3D, 2020 (RRID:SCR_017764, CC BY 4.0)
    Ding et al. 2020; ontology Ding et al. 2016 DOI 10.1002/cne.24080
"""
from __future__ import annotations
import csv
import json
import math
import sys
from pathlib import Path
from collections import defaultdict, Counter

import numpy as np
import nibabel as nib

ROOT = Path("/sessions/funny-peaceful-meitner/mnt/Synaptic Disorder")
ALLEN_DIR = Path("/sessions/funny-peaceful-meitner/mnt/outputs/allen")

# Quality / density knobs ---------------------------------------------------
TARGET_VOXEL_COUNT = 110_000     # raised: paths now claim unique voxels
MIN_PER_REGION     = 2_000       # baseline so even small regions are visible
MEMORY_BOOST       = 50          # extra voxels per memory in the region
SYNAPSE_PATH_HOPS  = 12          # voxels per synapse path (incl. endpoints)
SYNAPSE_PATH_JITTER = 0.04       # max curve deviation in unit-cube coords
MAX_EDGES_PER_NODE = 8

# Anchors (canonical Allen structure IDs that root each of our region keys)
ANCHORS = {
    "frontal_lobe":     [12113],            # FroL
    "parietal_lobe":    [12131],            # ParL
    "temporal_lobe":    [12139],            # TemL  (split L/R later by X)
    "visual_cortex":    [12148],            # OccL
    "cerebellum":       [10656],            # CB
    "hippocampus":      [10294],            # HIP
    "amygdala":         [10361],            # AMY
    "corpus_callosum":  [10561],            # cc
    "brain_stem":       [10648, 10661],     # midbrain, pons
}
REGION_ORDER = [
    # most-specific first
    "cerebellum", "brain_stem", "corpus_callosum",
    "hippocampus", "amygdala",
    "visual_cortex", "parietal_lobe", "temporal_lobe", "frontal_lobe",
]
# Final region keys exposed to the dashboard
REGION_KEYS = [
    "prefrontal_cortex", "frontal_lobe", "broca_area", "wernicke_area",
    "visual_cortex", "temporal_lobe_left", "temporal_lobe_right",
    "hippocampus", "parietal_lobe", "motor_cortex",
    "cerebellum", "amygdala", "corpus_callosum", "brain_stem",
]
# How user-tag classified region keys collapse to atlas regions for voxel
# placement (not all of our 14 keys appear in Allen at the top level —
# Broca/Wernicke/Motor/Prefrontal are sub-areas of frontal/temporal/parietal).
TAG_REGION_TO_ATLAS = {
    "prefrontal_cortex":     "frontal_lobe",
    "frontal_lobe":          "frontal_lobe",
    "broca_area":            "frontal_lobe",
    "wernicke_area":         "temporal_lobe_left",
    "visual_cortex":         "visual_cortex",
    "temporal_lobe_left":    "temporal_lobe_left",
    "temporal_lobe_right":   "temporal_lobe_right",
    "hippocampus":           "hippocampus",
    "parietal_lobe":         "parietal_lobe",
    "motor_cortex":          "frontal_lobe",   # M1 is in frontal lobe
    "cerebellum":            "cerebellum",
    "amygdala":              "amygdala",
    "corpus_callosum":       "corpus_callosum",
    "brain_stem":            "brain_stem",
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

def build_label_to_atlas_region(ontology, present_labels):
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

def normalize_to_unit_cube(world_xyz):
    """Allen RAS+ (mm) -> dashboard coords (unit cube, frontal toward +Z, up = +Y).

    Allen RAS+: +X = right, +Y = anterior, +Z = superior
    Dashboard:  +X = right, +Y = up,       +Z = toward viewer (anterior)

    So we swap Y and Z, then center + uniform-scale to fit in [-1,1].
    """
    mapped = world_xyz[:, [0, 2, 1]]   # X kept, Y<-Z, Z<-Y
    bmin = mapped.min(axis=0)
    bmax = mapped.max(axis=0)
    center = (bmin + bmax) / 2
    span = (bmax - bmin).max()
    scale = 1.7 / span
    return (mapped - center) * scale

def farthest_point_sample(positions, k, rng):
    """Greedy farthest-point sample. Returns indices into positions."""
    n = positions.shape[0]
    if k >= n:
        return np.arange(n)
    # Start from a random seed
    start = rng.integers(0, n)
    chosen = [start]
    d2 = np.sum((positions - positions[start]) ** 2, axis=1)
    for _ in range(k - 1):
        idx = int(np.argmax(d2))
        chosen.append(idx)
        new_d2 = np.sum((positions - positions[idx]) ** 2, axis=1)
        d2 = np.minimum(d2, new_d2)
    return np.array(chosen)

def build_synapse_path(start_pos, end_pos, voxel_positions, n_hops, jitter, rng,
                        used_mask=None, start_idx=None, end_idx=None):
    """Return voxel indices forming a curve from start to end.

    If used_mask is provided, voxels with True at their index are excluded
    from being snapped (except start_idx and end_idx which are always allowed
    since they represent the memory neurons themselves). This makes each
    synapse path use a unique set of intermediate voxels — no shared
    pathways, like real anatomically-distinct white-matter tracts.
    """
    ts = np.linspace(0, 1, n_hops)
    direction = end_pos - start_pos
    length = np.linalg.norm(direction)
    if length < 1e-6:
        nearest = int(np.argmin(np.sum((voxel_positions - start_pos) ** 2, axis=1)))
        return [nearest] * n_hops

    rand = rng.standard_normal(3)
    perp = rand - direction * (rand @ direction) / (length ** 2)
    perp_norm = np.linalg.norm(perp)
    if perp_norm > 1e-6:
        perp = perp / perp_norm
    bow = perp * jitter * length

    points = start_pos[None, :] * (1 - ts)[:, None] + end_pos[None, :] * ts[:, None]
    points = points + bow[None, :] * np.sin(ts * np.pi)[:, None]

    path_indices = []
    last_idx = -1
    for ti, p in enumerate(points):
        d2 = np.sum((voxel_positions - p) ** 2, axis=1)
        if used_mask is not None and ti != 0 and ti != len(points) - 1:
            d2 = d2.copy()
            d2[used_mask] = np.inf
        # Endpoints snap to the actual memory voxels regardless of used_mask
        if ti == 0 and start_idx is not None:
            idx = start_idx
        elif ti == len(points) - 1 and end_idx is not None:
            idx = end_idx
        else:
            idx = int(np.argmin(d2))
            if used_mask is not None and used_mask[idx]:
                # All nearby voxels were claimed; fall back to nearest available
                d2_global = np.sum((voxel_positions - p) ** 2, axis=1)
                d2_global[used_mask] = np.inf
                if np.isfinite(d2_global.min()):
                    idx = int(np.argmin(d2_global))
                # else: out of voxels — accept overlap as last resort
        if idx != last_idx:
            path_indices.append(idx)
            last_idx = idx
    return path_indices

def main():
    if not ALLEN_DIR.exists():
        sys.exit(f"Allen data missing at {ALLEN_DIR}.")

    print("Loading Allen annotation_full + ontology...", flush=True)
    img = nib.load(ALLEN_DIR / "annotation_full.nii.gz")
    data = img.get_fdata().astype(np.int32)
    affine = img.affine
    ontology = load_ontology(ALLEN_DIR / "voxel_count.csv")

    print("Mirroring hemispheres for full bilateral coverage...", flush=True)
    mirrored = np.flip(data, axis=0)
    data = np.where(data > 0, data, mirrored)

    present = set(int(l) for l in np.unique(data) if l > 0)
    label_to_region = build_label_to_atlas_region(ontology, present)
    print(f"  {len(label_to_region)} of {len(present)} structures rolled up to a region")

    # Build per-atlas-region voxel lists
    print("Sampling voxels per region...", flush=True)
    rng = np.random.default_rng(42)
    region_voxels_world: dict[str, np.ndarray] = {}
    ijk_brain = np.argwhere(data > 0)
    labels_brain = data[ijk_brain[:, 0], ijk_brain[:, 1], ijk_brain[:, 2]]
    by_region = defaultdict(list)
    for ijk_idx, lab in zip(range(len(ijk_brain)), labels_brain):
        region = label_to_region.get(int(lab))
        if region:
            by_region[region].append(ijk_idx)
    for region, idxs in by_region.items():
        idxs_arr = np.array(idxs)
        # Convert to world coords once
        ijk_sub = ijk_brain[idxs_arr]
        homog = np.column_stack([ijk_sub.astype(np.float32), np.ones(len(ijk_sub), dtype=np.float32)])
        world_sub = (affine @ homog.T).T[:, :3]
        region_voxels_world[region] = world_sub
        print(f"  atlas {region:18s}  {len(idxs):>9,} voxels available")

    # Load memories from the existing memories.json (we'll re-classify their region)
    mem_path = ROOT / "assets" / "memories.json"
    if not mem_path.exists():
        sys.exit("assets/memories.json missing — run the previous classifier first")
    print("Loading existing memories...", flush=True)
    mem_data = json.loads(mem_path.read_text(encoding="utf-8"))
    memories = mem_data["items"]
    print(f"  {len(memories)} memories loaded")

    # Sub-classifier: memory's currently-assigned region key -> atlas key
    # Keep memory.region as our 14-key namespace; collapse for voxel placement.
    region_demand = Counter()
    for m in memories:
        atlas_region = TAG_REGION_TO_ATLAS.get(m.get("region"), "frontal_lobe")
        region_demand[atlas_region] += 1
    print("Memory demand per atlas region:")
    for k, v in region_demand.most_common():
        print(f"  {k:18s}  {v:>5d} memories")

    # Determine how many voxels to sample per atlas region
    print("Computing per-region voxel budgets...", flush=True)
    targets = {}
    for region in by_region:
        wanted = MIN_PER_REGION + region_demand.get(region, 0) * MEMORY_BOOST
        targets[region] = min(wanted, len(region_voxels_world[region]))
    total = sum(targets.values())
    if total > TARGET_VOXEL_COUNT:
        # Scale down proportionally but keep the minimum
        excess = total - TARGET_VOXEL_COUNT
        scalable = {k: max(0, v - MIN_PER_REGION) for k, v in targets.items()}
        scalable_total = sum(scalable.values())
        if scalable_total > 0:
            for k in targets:
                shave = int(round(scalable[k] / scalable_total * excess))
                targets[k] = max(MIN_PER_REGION, targets[k] - shave)
    print(f"  total voxels to render: {sum(targets.values()):,}")

    # Sample voxels per region (uniform random + farthest-point skim for spread)
    print("Sampling render voxels...", flush=True)
    sampled_world = []
    sampled_region = []
    for region, world_pts in region_voxels_world.items():
        n = targets.get(region, 0)
        if n == 0:
            continue
        if n >= len(world_pts):
            picks = world_pts
        else:
            # Random sample (cheaper than FPS for tens of thousands of points)
            idx = rng.choice(len(world_pts), n, replace=False)
            picks = world_pts[idx]
        sampled_world.append(picks)
        sampled_region.extend([region] * len(picks))
    sampled_world = np.vstack(sampled_world)
    sampled_region = np.array(sampled_region)
    print(f"  {len(sampled_world):,} render voxels selected")

    # Normalize positions to dashboard unit-cube
    print("Normalizing to dashboard coordinates...", flush=True)
    unit_pts = normalize_to_unit_cube(sampled_world).astype(np.float32)
    # Split temporal_lobe by hemisphere (X > 0 = right ear)
    is_temporal = (sampled_region == "temporal_lobe")
    right_mask = is_temporal & (unit_pts[:, 0] > 0)
    sampled_region[right_mask] = "temporal_lobe_right"
    sampled_region[is_temporal & ~right_mask] = "temporal_lobe_left"

    # Build per-region voxel index lists and centroids (in unit-cube space)
    print("Indexing voxels by region + computing centroids...", flush=True)
    region_to_voxel_indices: dict[str, list[int]] = {k: [] for k in REGION_KEYS}
    for i, r in enumerate(sampled_region):
        # collapse atlas region key into the dashboard's 14-key namespace.
        # Atlas regions = a subset; the rest (broca, wernicke, prefrontal,
        # motor) are sub-areas; for voxel placement they share the parent.
        region_to_voxel_indices.setdefault(r, []).append(i)
    region_centroids = {}
    for k, idxs in region_to_voxel_indices.items():
        if idxs:
            region_centroids[k] = unit_pts[idxs].mean(axis=0).tolist()
        else:
            region_centroids[k] = [0.0, 0.0, 0.0]

    # ---- Pass 1: figure out edges so we know each memory's degree ----
    # Pairs are derived purely from tag overlap, so no positions needed yet.
    print("Computing synapse pairs from tag overlap...", flush=True)
    mem_by_id = {m["id"]: m for m in memories}
    tag_to_mems = defaultdict(list)
    for m in memories:
        for t in m.get("tags") or []:
            tag_to_mems[t].append(m["id"])

    pair_shared = Counter()
    for tag, mids in tag_to_mems.items():
        if len(mids) > 1 and len(mids) < 200:
            for i in range(len(mids)):
                for j in range(i + 1, len(mids)):
                    a, b = mids[i], mids[j]
                    key = (a, b) if a < b else (b, a)
                    pair_shared[key] += 1

    edges_by_node = defaultdict(list)
    for (a, b), w in sorted(pair_shared.items(), key=lambda kv: -kv[1]):
        if w < 2: continue
        if len(edges_by_node[a]) >= MAX_EDGES_PER_NODE: continue
        if len(edges_by_node[b]) >= MAX_EDGES_PER_NODE: continue
        edges_by_node[a].append((b, w))
        edges_by_node[b].append((a, w))

    raw_edges = set()
    for a, others in edges_by_node.items():
        for b, w in others:
            key = (a, b) if a < b else (b, a)
            raw_edges.add((key, w))

    # Degree per memory
    degree = Counter()
    for (a, b), _w in raw_edges:
        degree[a] += 1
        degree[b] += 1
    for m in memories:
        m["degree"] = int(degree.get(m["id"], 0))

    # ---- Pass 2: place memories by degree-driven centrality ----
    # Within each region, sort memories by degree desc and sort candidate
    # voxels by distance to region centroid asc. The most-connected memory
    # gets the most-central voxel; least-connected gets the periphery.
    # This makes hub neurons visibly hub-like — central, large, connected.
    print("Placing memories on voxels (degree-driven centrality)...", flush=True)
    # Group memories by their target region key
    by_region_mems = defaultdict(list)
    for m in memories:
        key = m.get("region")
        atlas_key = TAG_REGION_TO_ATLAS.get(key, "frontal_lobe")
        target = key if region_to_voxel_indices.get(key) else atlas_key
        if not region_to_voxel_indices.get(target):
            target = "frontal_lobe"
        by_region_mems[target].append(m)

    used = set()
    for region_key, mems_in_region in by_region_mems.items():
        avail = list(region_to_voxel_indices.get(region_key, []))
        if not avail:
            continue
        centroid = np.array(region_centroids[region_key])
        # Sort voxels by distance to centroid ascending (most central first)
        avail_arr = np.array(avail)
        dists = np.linalg.norm(unit_pts[avail_arr] - centroid, axis=1)
        order = np.argsort(dists)
        sorted_voxels = avail_arr[order].tolist()
        # Sort memories by degree desc, ties broken by id for determinism
        mems_in_region.sort(key=lambda mm: (-mm.get("degree", 0), mm["id"]))
        # Assign 1:1
        cursor = 0
        for mm in mems_in_region:
            while cursor < len(sorted_voxels) and sorted_voxels[cursor] in used:
                cursor += 1
            if cursor >= len(sorted_voxels):
                # Region ran out — fall back to any unused voxel anywhere
                pick = next((i for i in range(len(unit_pts)) if i not in used), 0)
            else:
                pick = sorted_voxels[cursor]
                cursor += 1
            mm["voxel_idx"] = int(pick)
            mm["pos"] = unit_pts[pick].tolist()
            used.add(pick)
    print(f"  placed {len(memories)} memories on unique voxels")

    print(f"  {len(raw_edges)} edges, building UNIQUE voxel paths...", flush=True)
    # Reserve the memory voxels first so paths don't snap onto other neurons.
    used_mask = np.zeros(len(unit_pts), dtype=bool)
    for m in memories:
        used_mask[m["voxel_idx"]] = True

    # Process longer edges first — they need more voxels and benefit from
    # picking before short edges have crowded out their corridor.
    edges_with_dist = []
    for (a, b), w in raw_edges:
        ma = mem_by_id.get(a); mb = mem_by_id.get(b)
        if not ma or not mb: continue
        d = float(np.linalg.norm(np.array(ma["pos"]) - np.array(mb["pos"])))
        edges_with_dist.append((d, a, b, w, ma, mb))
    edges_with_dist.sort(key=lambda x: -x[0])

    edges_out = []
    overlaps = 0
    for d, a, b, w, ma, mb in edges_with_dist:
        path = build_synapse_path(
            np.array(ma["pos"], dtype=np.float32),
            np.array(mb["pos"], dtype=np.float32),
            unit_pts,
            SYNAPSE_PATH_HOPS,
            SYNAPSE_PATH_JITTER,
            rng,
            used_mask=used_mask,
            start_idx=ma["voxel_idx"],
            end_idx=mb["voxel_idx"],
        )
        # Mark intermediate voxels as used (not endpoints — those are memories)
        for vi in path[1:-1]:
            if used_mask[vi]:
                overlaps += 1
            used_mask[vi] = True
        edges_out.append({"a": a, "b": b, "w": int(w), "path": path})
    print(f"  {len(edges_out)} synapses built; {overlaps} forced overlaps "
          f"(out of {sum(len(e['path']) - 2 for e in edges_out):,} intermediate voxel slots)")

    # Write outputs
    print("\nWriting outputs...", flush=True)
    # voxels.json — flat float32 array as one big list (positions interleaved)
    voxels_payload = {
        "schema_version": "1.0",
        "comment": ("Allen Human Reference Atlas - 3D, 2020 (RRID:SCR_017764, CC BY 4.0). "
                    "Ding et al. 2020; ontology Ding et al. 2016 DOI 10.1002/cne.24080. "
                    "Voxels mirrored hemispherically and sub-sampled per region with "
                    "memory-density-aware budgets."),
        "n": int(len(unit_pts)),
        "positions": unit_pts.flatten().tolist(),
        "regions": [str(r) for r in sampled_region.tolist()],
        "region_keys": REGION_KEYS,
        "region_centroids": region_centroids,
        "region_voxel_counts": {k: int(len(v)) for k, v in region_to_voxel_indices.items()},
    }
    (ROOT / "assets" / "voxels.json").write_text(json.dumps(voxels_payload), encoding="utf-8")

    # memories.json — keep existing structure plus voxel_idx/pos
    mem_data["items"] = memories
    mem_data["voxel_source"] = "allen_atlas_v1.0"
    (ROOT / "assets" / "memories.json").write_text(json.dumps(mem_data), encoding="utf-8")

    # synapses.json — new schema with path_voxels
    synapses_payload = {
        "schema_version": "1.0",
        "comment": "Each edge carries a path_voxels list — the voxel indices a pulse traverses.",
        "edges": [{"a": e["a"], "b": e["b"], "w": e["w"], "path": e["path"]} for e in edges_out],
    }
    (ROOT / "assets" / "synapses.json").write_text(json.dumps(synapses_payload), encoding="utf-8")

    sz_v = (ROOT / "assets" / "voxels.json").stat().st_size / 1024
    sz_m = (ROOT / "assets" / "memories.json").stat().st_size / 1024
    sz_s = (ROOT / "assets" / "synapses.json").stat().st_size / 1024
    print(f"  voxels.json   {sz_v:>7.0f} KB  ({len(unit_pts):,} voxels)")
    print(f"  memories.json {sz_m:>7.0f} KB  ({len(memories):,} memories)")
    print(f"  synapses.json {sz_s:>7.0f} KB  ({len(edges_out):,} edges, ~{SYNAPSE_PATH_HOPS} voxels per path)")

if __name__ == "__main__":
    main()

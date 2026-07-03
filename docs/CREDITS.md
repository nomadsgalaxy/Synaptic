# Credits

Synaptic builds on several licensed third-party works. Their attribution is preserved in the dashboard's credits panel, in source comment headers, and here in one consolidated place.

## Project author

**Synaptic is by [NomadsGalaxy](https://github.com/nomadsgalaxy).**

The HUD's bottom-right micro-credit ("Synaptic by NomadsGalaxy" + version) is a permanent fixture; downstream forks should preserve it. Distribution channels:

- GitHub: <https://github.com/nomadsgalaxy/Synaptic>
- Claude Code plugin marketplace: `nomadsgalaxy/Synaptic`

## Anatomical region atlas — Allen Human Reference Atlas, 3D, 2020

| Field         | Value                                                                                                                                                                                |
| ------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| Resource Name | Allen Human Reference Atlas – 3D, 2020                                                                                                                                               |
| Version       | 1.0.0                                                                                                                                                                                |
| RRID          | RRID:SCR_017764                                                                                                                                                                      |
| Copyright     | © 2019 Allen Institute for Brain Science                                                                                                                                             |
| License       | [Creative Commons Attribution 4.0 International (CC BY 4.0)](http://creativecommons.org/licenses/by/4.0/) (effective Sept 1, 2022)                                                   |
| Source        | <https://download.alleninstitute.org/informatics-archive/allen_human_reference_atlas_3d_2020/version_1/>                                                                             |
| Used as       | The dashboard's brain scaffold IS Allen voxels — `assets/voxels.json` is derived directly from the atlas; region centroids, bounding volumes, and structure names are taken from it. |

**Dataset citation (use this for the atlas itself):**

> Song-Lin Ding, Joshua J. Royall, Susan M. Sunkin, Benjamin A.C. Facer, Phil Lesnar, Amy Bernard, Lydia Ng, Ed S. Lein (2020). "Allen Human Reference Atlas – 3D, 2020," RRID:SCR_017764, version 1.0.0.

**Publication citation for the structure ontology:**

> Ding, S.L., Royall, J.J., Sunkin, S.M., Ng, L., Facer, B.A.C., Lesnar, P., Guillozet-Bongaarts, A., McMurray, B., Szafer, A., Dolbeare, T.A., Stevens, A., Tirrell, L., Benner, T., Caldejon, S., Dalley, R.A., Dee, N., Lau, C., Nyhus, J., Reding, M., Riley, Z.L., Sandman, D., Shen, E., van der Kouwe, A., Varjabedian, A., Wright, M., Zollei, L., Dang, C., Knowles, J.A., Koch, C., Phillips, J.W., Sestan, N., Wohnoutka, P., Zielke, H.R., Hohmann, J.G., Jones, A.R., Bernard, A., Hawrylycz, M.J., Hof, P.R., Fischl, B., Lein, E.S. **Comprehensive cellular-resolution atlas of the adult human brain.** *Journal of Comparative Neurology*, Volume 524:16, pages 3127–3481, 1 November 2016, DOI: [10.1002/cne.24080](https://doi.org/10.1002/cne.24080)

Per the [Allen Institute Citation Policy](https://alleninstitute.org/legal/citation-policy), both citations should appear in publications, derived datasets, and any user-facing surface that exposes the atlas data. The dashboard satisfies this via the persistent micro-credit in the bottom-right HUD; downstream forks must preserve it.

## Runtime libraries

| Library                                                                    | License      | Used in                                                                                                                                                                                                                |
| -------------------------------------------------------------------------- | ------------ | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| [Three.js](https://threejs.org/)                                           | MIT          | Dashboard 3D rendering — loaded from CDN at runtime                                                                                                                                                                    |
| [Ollama](https://ollama.com) (`ollama/ollama` Docker image)                | MIT          | Bundled local LLM runtime: Tier 1 (Encoder — region classifier + embeddings via `nomic-embed-text`) and Tier 2 (Nightly — thought-bubble generator, dream pipeline summarization, narrative + dream entry composition) |
| [`@modelcontextprotocol/sdk`](https://github.com/modelcontextprotocol/sdk) | MIT          | Stdio MCP server used by `bridge/mcp-adapter`                                                                                                                                                                          |
| [`protobufjs`](https://github.com/protobufjs/protobuf.js)                  | BSD-3-Clause | OTLP/Protobuf decode in `bridge/otel-adapter`                                                                                                                                                                          |
| [`modernc.org/sqlite`](https://gitlab.com/cznic/sqlite)                    | BSD-3-Clause | Pure-Go SQLite driver in SD Core (CGO-free)                                                                                                                                                                            |
| [`gorilla/websocket`](https://github.com/gorilla/websocket)                | BSD-2-Clause | WebSocket fan-out in SD Core                                                                                                                                                                                           |
| [`fsnotify`](https://github.com/fsnotify/fsnotify)                         | BSD-3-Clause | File watcher in SD Core                                                                                                                                                                                                |

## Vendored protocol definitions

`bridge/otel-adapter/proto/opentelemetry/proto/...` contains a trimmed subset of the [OpenTelemetry protobuf definitions](https://github.com/open-telemetry/opentelemetry-proto), under the upstream **Apache 2.0** license. Only the fields the receiver actually decodes are vendored.

## Recommended-but-optional models

None of the model weights are bundled with this repo. Models are pulled by Ollama at runtime via its container entrypoint when the stack starts. Synaptic uses three model roles, each filling a distinct tier in the dream pipeline:

| Role                                    | Default                      | Tier   | Used in                                                                                                                                        |
| --------------------------------------- | ---------------------------- | ------ | ---------------------------------------------------------------------------------------------------------------------------------------------- |
| Region classifier (small generative)    | `llama3.2:3b` (~2 GB)        | Tier 1 | `classifier.go` region assignment, `bubble_generator.go` thought bubbles, salience-language scoring                                            |
| Embedder                                | `nomic-embed-text` (~274 MB) | Tier 1 | Phase 0 light-encoding, `recall.go` query embedding, dedup/synthesis/cross-region cosine inputs                                                |
| Nightly generator (mid-size generative) | `llama3.1:8b` (~4.9 GB)      | Tier 2 | Phase 0b deep enrichment, Phase 2 synthesis, Phase 8 schema, Phase 11 narrative, Phase 12 dream entry                                          |
| Oracle (external)                       | unconfigured                 | Tier 3 | `handlers_research.go` web research; configurable to OpenAI / Anthropic / Ollama-local. Default off so no paid egress without explicit opt-in. |

Each tier is independently configurable via env vars (`SD_OLLAMA_MODEL` for the classifier, `SD_EMBED_MODEL` for the embedder, `SD_NIGHTLY_MODEL` for the Tier 2 generator) so users can swap models per their hardware budget without affecting the others. Only `SD_OLLAMA_MODEL` is auto-pulled by the compose stack; the embedder and nightly model need to be pulled manually (`docker exec synaptic-ollama ollama pull <model>`) before their respective phases will run.

**Small-model alternatives for the classifier role** (Q4_K_M quants; sizes approximate; ARM64 ollama build assumed for Pi):

| Model            | Size    | Hardware    | Notes                                      |
| ---------------- | ------- | ----------- | ------------------------------------------ |
| `qwen2.5:0.5b`   | ~400 MB | Pi 4 (4 GB) | Very fast, dirt-cheap                      |
| `tinyllama:1.1b` | ~640 MB | Pi 4 (4 GB) | OK for region sorting                      |
| `llama3.2:1b`    | ~1.3 GB | Pi 4 (4 GB) | Best balance for low-end Pi                |
| `qwen2.5:1.5b`   | ~1.0 GB | Pi 4 (4 GB) | Solid drop-in for llama 1b                 |
| `gemma2:2b`      | ~1.6 GB | Pi 5 (4 GB) | Slower, better text quality                |
| `phi3:mini`      | ~2.3 GB | Pi 5 (8 GB) | Best small-model quality                   |
| `llama3.2:3b`    | ~2.0 GB | Pi 5 (8 GB) | **Default** — what `classifier.go` expects |

Each model retains its respective upstream license (Meta's Llama 3.x license, Apache 2.0 for Qwen 2.5, Gemma terms, MIT for TinyLlama, Microsoft's Phi license for Phi-3). None are redistributed in this repo.

**Tier 2 alternatives:** `llama3.1:8b` is the default; users with smaller GPUs can downgrade to `llama3.2:3b` (loses some synthesis quality but works on a Pi 5 with 8 GB of RAM). Users with bigger GPUs can upgrade to `llama3.1:70b` quants for noticeably tighter dream entries.

**Embedding alternatives:** `nomic-embed-text` is the default and what Phase 0 expects. `mxbai-embed-large` is a higher-quality drop-in (~670 MB) for users with the headroom; `all-minilm` is a smaller fallback (~46 MB) that works on minimal hardware but loses recall precision on near-duplicate detection.

## Research literature underlying the architecture

Synaptic's dream consolidation pipeline (Phases 0a/0b/1–12) is grounded in published neuroscience research on how human brains process and reorganize memories during sleep. The architecture is biologically faithful by design: every phase maps to a specific empirical finding from the literature.

**The complete bibliography lives at [`docs/research/CITATIONS.md`](research/CITATIONS.md).** That document catalogues 25 papers with full citations, mechanisms described, code locations they map to, and how each advances the project's mission. A planned future audit will add citation-count weighting and explicit pairwise contradictions between papers — until then, treat the bibliography as a starting point for engagement with the literature, not a final word on contested mechanisms.

Selected works whose findings are load-bearing for the architecture are credited here. For the complete entry, mechanism description, and code mapping, see CITATIONS.md.

All entries below use APA 7 format with full author lists and full journal names — no abbreviations. The canonical full bibliography (with mechanisms, code mappings, and goal-alignment paragraphs) is in CITATIONS.md; what follows is the credit summary.

### Foundational reviews

- **Stickgold, R. & Walker, M.P. (2010).** Overnight alchemy: Sleep-dependent memory evolution. *Nature Reviews Neuroscience*, **11**(3), 218.
- **Stickgold, R. & Walker, M.P. (2013).** Sleep-dependent memory triage: Evolving generalization through selective processing. *Nature Neuroscience*, **11**(2), 139–145.
- **Paller, K.A., Creery, J.D. & Schechtman, E. (2021).** Memory and sleep: How sleep cognition can change the waking mind for the better. *Annual Review of Psychology*, **72**, 123–150.

### Synaptic homeostasis and tagging

- **Tononi, G. & Cirelli, C. (2014).** Sleep and the price of plasticity: From synaptic and cellular homeostasis to memory consolidation and integration. *Neuron*, **81**(1), 12–34.
- **Tononi, G. & Cirelli, C. (2020).** Sleep and synaptic down-selection. *European Journal of Neuroscience*, **51**(1), 413–421.
- **Ibrahim, M.Z.B., Wang, Z. & Sajikumar, S. (2024).** Synapses tagged, memories kept: Synaptic tagging and capture hypothesis in brain health and disease. *Philosophical Transactions of the Royal Society B*, **379**(1906), 20230237.
- **Moncada, D., Ballarini, F. & Viola, H. (2015).** Behavioral tagging: A translation of the synaptic tagging and capture hypothesis. *Neural Plasticity*, **2015**, 650780.

### Replay and recombination

- **Buzsáki, G. (2015).** Hippocampal sharp wave-ripple: A cognitive biomarker for episodic memory and planning. *Hippocampus*, **25**(10), 1073–1188.
- **Schapiro, A.C., McDevitt, E.A., Rogers, T.T., Mednick, S.C. & Norman, K.A. (2018).** Human hippocampal replay during rest prioritizes weakly learned information and predicts memory performance. *Nature Communications*, **9**(1), 3920.
- **Lewis, P.A., Knoblich, G. & Poe, G. (2018).** How memory replay in sleep boosts creative problem-solving. *Trends in Cognitive Sciences*, **22**(6), 491–503.
- **Cai, D.J., Mednick, S.A., Harrison, E.M., Kanady, J.C. & Mednick, S.C. (2009).** REM, not incubation, improves creativity by priming associative networks. *Proceedings of the National Academy of Sciences*, **106**(25), 10130–10134.

### Schema integration and creative insight

- **Lacaux, C., Andrillon, T., Bastoul, C., Idir, Y., Fonteix-Galet, A., Arnulf, I. & Oudiette, D. (2021).** Sleep onset is a creative sweet spot. *Science Advances*, **7**(50), eabj5866.
- **Aghayan Golkashani, H., Ghorbani, S., Leong, R.L.F., Ong, J.L. & Chee, M.W.L. (2023).** Advantage conferred by overnight sleep on schema-related memory may last only a day. *SLEEP Advances*, **4**(1), zpad019.
- **Ashton, J.E., Staresina, B.P. & Cairney, S.A. (2022).** Sleep bolsters schematically incongruent memories. *PLOS One*, **17**(7), e0269439.

### Emotional memory + selectivity

- **Payne, J.D. & Kensinger, E.A. (2018).** Stress, sleep, and the selective consolidation of emotional memories. *Current Opinion in Behavioral Sciences*, **19**, 36–43.
- **Hutchison, I.C. & Rathore, S. (2015).** The role of REM sleep theta activity in emotional memory. *Frontiers in Psychology*, **6**, 1439.
- **Nishida, M., Pearsall, J., Buckner, R.L. & Walker, M.P. (2009).** REM sleep, prefrontal theta, and the consolidation of human emotional memory. *Cerebral Cortex*, **19**(5), 1158–1166.

### REM-specific mechanisms (and the contrarian view)

- **Liu, S., Pikovsky, A., Cohen, M.X. et al. (2023).** Human REM sleep recalibrates neural activity in support of memory formation. *Science Advances*, **9**(34), eadj1895.
- **Johnson, J.D. (2005).** REM sleep and the development of context memory. *Medical Hypotheses*, **64**(3), 499–504. Originator of the "context memory" hypothesis underlying Phase 8.7.
- **Siegel, J.M. (2001).** The REM sleep–memory consolidation hypothesis. *Science*, **294**(5544), 1058–1063. Important contrarian view; informs the architecture's hedge of keeping REM-equivalent phases opt-in.
- **Sarangi, A. & Paital, B. (2021).** Association between REM sleep and strengthening memory: A mini review. *Journal of Clinical Images and Medical Case Reports*, **2**(6), 1451.
- **Liu, J., Chen, D., Xia, T., Zeng, S., Xue, G. & Hu, X. (2025).** Slow-wave sleep and REM sleep differentially contribute to memory representational transformation. *Communications Biology*, **8**, 1302.
- **Wamsley, E.J., Trost, T. & Tucker, M. (2024).** Memory updating in dreams. *SLEEP Advances*, **5**(1), zpae096.

### Forgetting and pruning

- **Poe, G.R. (2017).** Sleep is for forgetting. *Journal of Neuroscience*, **37**(3), 464–473.
- **Genzel, L. & Wixted, J.T. (2019).** Cellular and systems consolidation of declarative memory. *Frontiers in Cellular Neuroscience*, **13**, 71.

### Citation policy

If you publish work that describes Synaptic's mechanisms, please cite the underlying primary sources (above) alongside any reference to the system itself. The system's architecture is *derived from* this literature, not original research, and academic credibility flows from citing the original investigators.

---

## Inspiration *(non-license; just credit where credit is due)*

- [Neurotorium Brain Atlas](https://neurotorium.org/tool/brain-atlas/)
- [BrainMinds 3D Atlas Viewer](https://dataportal.brainminds.jp/3d-atlas-viewer)

---

## Synaptic's own license

Software license: **TBD — Open Community License (OCL) pending.** No `LICENSE` file ships in this repo yet by design. This is intentional and will be resolved when OCL is published. The third-party works above retain their original CC BY 4.0 / MIT / BSD / Apache 2.0 licenses regardless.

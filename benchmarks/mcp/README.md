# MCP baseline

Recorded baseline: [RESULTS.md](RESULTS.md), [JSON](results/2026-10-21.json).

Reproducible scripted measurements against a new, temporary local AI Wiki server.
The runner builds the current Go source and seeds synthetic articles. It never
connects to your running wiki or loads its data. Requires Go 1.25+ and Node 22.18+.

From the repository root, install the isolated benchmark dependencies:

```bash
npm ci --prefix benchmarks/mcp
```

Populate the Go module cache before going offline; the runner builds with
`GOPROXY=off` and `GOTOOLCHAIN=local`:

```bash
go mod download
```

Run the baseline (20 measured repetitions per read scenario):

```bash
node benchmarks/mcp/run.mjs --repetitions 20
```

Raw traces, summaries and the temporary database are retained under ignored
`.cache/` directories; the spawned server stops when the run finishes. Keep raw
traces when comparing changes. Do not publish your own real documents as fixtures.

Read [METHODOLOGY.md](METHODOLOGY.md) before interpreting results. Tokens use
`cl100k_base` from js-tiktoken 1.0.21 as a fixed proxy, not Claude billing. The
script knows the target article and heading from the fixture: it measures the
cost of a correct route, not autonomous agent success. Search quality is assessed
separately from the conditional full/partial read comparisons. Production ACL,
load capacity and embedding/graph quality are outside this baseline.

The benchmark's tokenizer dependency is development-only and is not included in
the application image. Source and package licenses remain with their authors.

The manual **MCP baseline** GitHub Actions workflow runs the scripted baseline
and retains only the summary and synthetic traces, without databases or binaries.
Live model trials are deliberately not part of CI and consume model usage:

```bash
node benchmarks/mcp/agent-trials.mjs --smoke
```

```bash
node benchmarks/mcp/agent-trials.mjs
```

The optional pilot requires an authenticated native Claude CLI with Sonnet access.
It runs 8 read-only trials (2 tasks × 2 strategies × 2 repetitions) through a
recording loopback proxy. Provider usage, including cached inputs, is separate
from tokenizer payload counts. A smoke run is not part of the scored pilot.
Local Claude JSON/logs remain ignored and may include machine-specific details.

The offline grader regression tests never start a server or call a model:

```bash
node --test benchmarks/mcp/grading.test.mjs
```

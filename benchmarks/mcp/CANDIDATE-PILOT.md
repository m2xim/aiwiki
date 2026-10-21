# MCP candidate pilot

This is a new live-agent experiment against the same synthetic fixture, with
`wiki_search_sections` and bounded `wiki_read` available to both workflows.
`agent-trials.mjs` keeps its existing default eight-trial baseline (`outline`
versus `full`) and its exported `gradeTrial` interface. Candidate trials are
opt-in:

```powershell
node benchmarks/mcp/agent-trials.mjs --candidate
```

The candidate plan is eight isolated sessions: r02 and r06, two workflows, and
two repeats per task/workflow. Workflow order is counterbalanced. Both workflows
use the same frozen questions, `claude-sonnet-5-5`, and `low` effort. `--limit N`
can select a leading subset for an explicitly limited run; `--smoke` selects one
trial. Neither option changes the planned eight-session design.

Question version `candidate-v1-capacity-observations` freezes these questions:

* r02: «В действующей процедуре обработки офферов найдите буквальную отметку подтверждения»
* r06: `В разделе «Capacity observations» статьи о профиле нагрузки найдите буквальную уникальную отметку ответа.`

The r06 wording names the answer's actual section. This replaces the earlier
ambiguous «крупном разделе» phrasing; the baseline corpus and `corpus.mjs` are
unchanged. The manifest records both questions, both workflow instructions,
fixture version and SHA-256, runner SHA-256, model, effort, and historical
baseline hashes. Candidate mode writes `candidate-manifest.json` in its ignored
local run directory before the first model session and records its SHA-256 in
the result and each completed trial. That file is the frozen comparison
manifest for the run.

The only workflow difference is the retrieval route:

* `outline-bounded`: “Use wiki_search to find the matching article, read its
  outline, then read only the selected target section with wiki_read
  max_chars=4000 and the revision returned by the outline. Follow returned
  next_read exactly only while evidence is insufficient; stop when the
  requested fact is supported or has_more is false.”
* `sections-bounded`: “Use wiki_search_sections to find the target section,
  then pass its returned read_args unchanged to wiki_read. Follow returned
  next_read exactly only while evidence is insufficient; stop when the
  requested fact is supported or has_more is false.”

Both tool names are in the proxy allowlist and Claude's allowed-tool list for
every policy. A strict route pass requires the cited target article and anchor,
the expected literal in actual bounded response text for that target anchor,
and the prescribed prior outline or returned section address. A matching search
snippet alone cannot pass. `retrievalSuccess` is reported separately, so an
answer found in a snippet can count as retrieval while failing `routeCompliance`.
Overall `success` requires both. Candidate results also include
`routeComplianceSuccesses` in the design summary.

Historical comparison metadata cites the published baseline runner and corpus
hashes from `RESULTS.md`. The earlier live pilot explicitly lacked a hash of the
runner that executed it, so the published evaluator hash must not be represented
as that execution hash. The new question version defines its own experiment;
its results are not a direct before/after percentage against the old pilot.
Keep provider token usage separate from proxy payload token estimates. The old
r06 route failures and the small two-task sample remain interpretation limits.

Full Claude JSON, server logs, and raw MCP traces remain local under ignored
`.cache/mcp-agent-*` run directories. Do not publish these raw files. This
candidate mode does not start or contact any external wiki; it builds and seeds
a temporary local server per trial.

Offline grading checks (no server or model):

```powershell
node --check benchmarks/mcp/agent-trials.mjs
node --test benchmarks/mcp/grading.test.mjs benchmarks/mcp/candidate-grading.test.mjs
```

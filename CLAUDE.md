# Claude instructions

Follow [AGENTS.md](AGENTS.md); the rules there apply in full.

In short: if an MCP server `aiwiki` is configured, call `wiki_bootstrap` first, use `wiki_search` / `wiki_read` before researching, and pass the current revision as `base_revision` when writing. If it is unavailable, report that and continue; builds and tests never require a private server. Verify changes by running the commands listed in AGENTS.md yourself, and report actual results. Never commit secrets, personal addresses or data directories.

#!/usr/bin/env node

import { spawn, spawnSync } from "node:child_process";
import { createServer } from "node:http";
import { once } from "node:events";
import { appendFile, mkdir, mkdtemp, readFile, writeFile } from "node:fs/promises";
import { createHash, randomUUID } from "node:crypto";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { tmpdir } from "node:os";
import { performance } from "node:perf_hooks";
import { getEncoding } from "js-tiktoken";
import { makeCorpus } from "./corpus.mjs";

const here = path.dirname(fileURLToPath(import.meta.url));
const root = path.resolve(here, "../..");
const runRoot = path.join(root, ".cache", `mcp-agent-${randomUUID()}`);
const encoder = getEncoding("cl100k_base");
const timeoutMs = 180_000;
const toolNames = ["wiki_bootstrap", "wiki_search", "wiki_search_sections", "wiki_read"];
const model = "claude-sonnet-5-5";
const claudeCommand = process.platform === "win32" ? "claude.exe" : "claude";
const candidateQuestionVersion = "candidate-v1-capacity-observations";
const candidateQuestions = Object.freeze({
  r02: "В действующей процедуре обработки офферов найдите буквальную отметку подтверждения",
  r06: "В разделе «Capacity observations» статьи о профиле нагрузки найдите буквальную уникальную отметку ответа.",
});
const candidatePolicies = Object.freeze({
  "outline-bounded": "Use wiki_search to find the matching article, read its outline, then read only the selected target section with wiki_read max_chars=4000 and the revision returned by the outline. Follow returned next_read exactly only while evidence is insufficient; stop when the requested fact is supported or has_more is false.",
  "sections-bounded": "Use wiki_search_sections to find the target section, then pass its returned read_args unchanged to wiki_read. Follow returned next_read exactly only while evidence is insufficient; stop when the requested fact is supported or has_more is false.",
});
const historicalBaseline = Object.freeze({
  resultsDate: "2026-10-21",
  runnerSha256: "49ded6dce54f5a8e32a88365783a36d86cf7f531b3d6cede9cb993be76c3dcbd",
  corpusSha256: "e78fa42446048058d1c1bcf023516bac7bd719148327abbb1ac25bc0e2992d16",
  caveat: "Historical source hash is the published runner hash; the executed pilot runner hash was not recorded.",
});

function json(value) {
  return JSON.stringify(value);
}

function infrastructureError(message, type, cause) {
  const error = new Error(message, cause ? { cause } : undefined);
  error.infrastructureType = type;
  return error;
}

function getClaudeVersion() {
  const result = spawnSync(claudeCommand, ["--version"], {
    cwd: tmpdir(), encoding: "utf8", windowsHide: true, timeout: 10_000,
  });
  if (result.error?.code === "ENOENT") {
    throw infrastructureError(`Claude CLI executable ${claudeCommand} was not found on PATH`, "claude_cli_unavailable", result.error);
  }
  if (result.error || result.status !== 0) {
    throw infrastructureError(`Claude CLI version check failed: ${result.error?.message ?? result.stderr ?? result.stdout}`, "claude_cli_version_failed", result.error);
  }
  return result.stdout.trim();
}

function tokenCount(value) {
  return encoder.encode(typeof value === "string" ? value : json(value), [], []).length;
}

function sha256(value) {
  return createHash("sha256").update(value).digest("hex");
}

function checkedJson(response, label) {
  return response.text().then((text) => {
    if (!response.ok) throw new Error(`${label}: HTTP ${response.status}: ${text.slice(0, 500)}`);
    try {
      return JSON.parse(text);
    } catch (error) {
      throw new Error(`${label}: invalid JSON: ${error.message}`);
    }
  });
}

async function reservePort() {
  const server = createServer();
  server.listen(0, "127.0.0.1");
  await once(server, "listening");
  const { port } = server.address();
  await new Promise((resolve, reject) => server.close((error) => error ? reject(error) : resolve()));
  return port;
}

async function waitForServer(baseUrl, child) {
  const deadline = Date.now() + 20_000;
  let lastError;
  while (Date.now() < deadline) {
    if (child.exitCode !== null) throw new Error(`Wiki server exited early (${child.exitCode})`);
    try {
      const response = await fetch(`${baseUrl}/api/health`, { signal: AbortSignal.timeout(1_000) });
      if (response.ok) return;
    } catch (error) {
      lastError = error;
    }
    await new Promise((resolve) => setTimeout(resolve, 200));
  }
  throw new Error(`Wiki server did not become ready${lastError ? `: ${lastError.message}` : ""}`);
}

async function seedCorpus(baseUrl, corpus) {
  const spaces = new Map();
  for (const space of corpus.spaces) {
    const created = await checkedJson(await fetch(`${baseUrl}/api/spaces`, {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: json({ name: space.name, description: `Synthetic benchmark space ${space.key}` }),
    }), `create space ${space.key}`);
    spaces.set(space.key, created.id);
  }

  const ids = new Map();
  const createdArticles = new Map();
  for (const doc of corpus.documents) {
    const created = await checkedJson(await fetch(`${baseUrl}/api/articles`, {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: json({
        space_id: spaces.get(doc.spaceKey),
        title: doc.title,
        summary: doc.summary,
        body: doc.body,
        kind: "article",
        status: "active",
        tags: doc.tags,
        properties: doc.properties,
        author: "synthetic-benchmark",
      }),
    }), `create article ${doc.key}`);
    ids.set(doc.key, created.id);
    createdArticles.set(doc.key, { ...doc, id: created.id, revision: created.revision });
  }

  // Resolve links after every article has a server-assigned ID. The frozen corpus objects stay untouched.
  for (const doc of corpus.documents) {
    if (!doc.body.includes("[[link:")) continue;
    const seeded = createdArticles.get(doc.key);
    const body = doc.body.replace(/\[\[link:([A-Za-z0-9_-]+)\]\]/g, (match, key) => {
      const id = ids.get(key);
      if (!id) throw new Error(`Unknown corpus link ${key} in ${doc.key}`);
      const target = corpus.documents.find((candidate) => candidate.key === key);
      if (!target) throw new Error(`Unknown corpus link target ${key} in ${doc.key}`);
      return `[${target.title}](/?article=${id})`;
    });
    await checkedJson(await fetch(`${baseUrl}/api/articles/${seeded.id}`, {
      method: "PUT",
      headers: { "content-type": "application/json" },
      body: json({ body, base_revision: seeded.revision, author: "synthetic-benchmark", message: "Resolve synthetic fixture links" }),
    }), `resolve links in ${doc.key}`);
  }
  return { ids, spaces };
}

function parseRunOptions(argv) {
  let limit;
  let candidate = false;
  for (let i = 0; i < argv.length; i++) {
    if (argv[i] === "--candidate") candidate = true;
    else if (argv[i] === "--smoke") limit = 1;
    else if (argv[i] === "--limit") {
      const value = Number(argv[++i]);
      if (!Number.isInteger(value) || value < 1 || value > 8) throw new Error("--limit must be an integer from 1 to 8");
      limit = value;
    } else throw new Error(`Unknown option: ${argv[i]}`);
  }
  return { limit: limit ?? 8, candidate };
}

function listen(server) {
  return new Promise((resolve, reject) => {
    server.once("error", reject);
    server.listen(0, "127.0.0.1", () => {
      server.removeListener("error", reject);
      resolve(server.address().port);
    });
  });
}

async function startRecordingProxy(targetUrl, tracePath, corpusVersion) {
  const pendingWrites = new Set();
  const server = createServer(async (req, res) => {
    const startedAt = new Date().toISOString();
    const startedMono = performance.now();
    const chunks = [];
    for await (const chunk of req) chunks.push(chunk);
    const requestBytes = Buffer.concat(chunks);
    const rawRequest = requestBytes.toString("utf8");
    let requestJson;
    try { requestJson = JSON.parse(rawRequest); } catch { requestJson = undefined; }
    const method = requestJson?.method;
    const params = requestJson?.params;
    const requestArgsTokens = method === "tools/call" ? tokenCount(params?.name ?? "") + tokenCount(params?.arguments ?? {}) : 0;
    let status = 502;
    let responseBytes = Buffer.from("proxy error");
    let responseJson;
    let proxyError;
    try {
      if (req.url !== "/mcp" || !["GET", "POST"].includes(req.method)) {
        status = 404;
        responseBytes = Buffer.from("not found");
      } else if (method === "tools/call" && !toolNames.includes(params?.name)) {
        status = 200;
        res.setHeader("content-type", "application/json; charset=utf-8");
        responseJson = {
          jsonrpc: "2.0",
          id: requestJson?.id ?? null,
          result: { content: [{ type: "text", text: `Read-only pilot allows only: ${toolNames.join(", ")}` }], isError: true },
        };
        responseBytes = Buffer.from(json(responseJson));
      } else {
        const headers = new Headers();
        for (const [key, value] of Object.entries(req.headers)) {
          if (value !== undefined && !["host", "connection", "content-length"].includes(key.toLowerCase())) {
            headers.set(key, Array.isArray(value) ? value.join(", ") : value);
          }
        }
        const upstream = await fetch(targetUrl, {
          method: req.method,
          headers,
          body: req.method === "GET" || requestBytes.length === 0 ? undefined : requestBytes,
          signal: AbortSignal.timeout(60_000),
        });
        status = upstream.status;
        responseBytes = Buffer.from(await upstream.arrayBuffer());
        try { responseJson = JSON.parse(responseBytes.toString("utf8")); } catch { /* retain raw response */ }
        res.statusCode = status;
        for (const [key, value] of upstream.headers) {
          if (!["content-length", "transfer-encoding", "connection"].includes(key.toLowerCase())) res.setHeader(key, value);
        }
      }
    } catch (error) {
      proxyError = error.message;
    }
    if (!res.headersSent) res.statusCode = status;
    const result = responseJson?.result;
    const toolResult = result?.content?.map((part) => part?.text ?? part).join("\n") ?? result;
    const record = {
      at: startedAt,
      corpusVersion,
      http: { requestMethod: req.method, path: req.url, status },
      apiElapsedMs: Number((performance.now() - startedMono).toFixed(3)),
      method,
      params,
      result,
      rpcError: responseJson?.jsonrpc === "2.0" ? responseJson.error : undefined,
      expectedProbe: (req.method === "GET" && status === 405) || (method === "server/discover" && responseJson?.error?.code === -32601),
      isError: Boolean(result?.isError),
      proxyError,
      rawRequest,
      rawResponse: responseBytes.toString("utf8"),
      wireBytes: { request: requestBytes.length, response: responseBytes.length, total: requestBytes.length + responseBytes.length },
      tokens: {
        requestArguments: requestArgsTokens,
        responseResult: method === "tools/call" ? tokenCount(toolResult ?? "") : 0,
        toolsSchema: method === "tools/list" ? tokenCount(result?.tools ?? []) : 0,
        initializeInstructions: method === "initialize" ? tokenCount(result?.instructions ?? "") : 0,
        bootstrap: method === "tools/call" && params?.name === "wiki_bootstrap" ? tokenCount(toolResult ?? "") : 0,
      },
    };
    const write = appendFile(tracePath, `${json(record)}\n`, "utf8");
    pendingWrites.add(write);
    try {
      await write;
    } catch (error) {
      res.statusCode = 500;
      res.end(`trace write failed: ${error.message}`);
      return;
    } finally {
      pendingWrites.delete(write);
    }
    res.end(responseBytes);
  });
  const port = await listen(server);
  return { server, url: `http://127.0.0.1:${port}/mcp`, flush: async () => { await Promise.all([...pendingWrites]); } };
}

function closeServer(server) {
  if (!server?.listening) return Promise.resolve();
  return new Promise((resolve) => server.close(() => resolve()));
}

async function stopChild(child) {
  if (!child || child.exitCode !== null) return;
  child.kill();
  await Promise.race([once(child, "exit").catch(() => {}), new Promise((resolve) => setTimeout(resolve, 3_000))]);
}

async function runClaude({ prompt, configPath, outputPath, stdoutPath, stderrPath, cwd }) {
  const args = [
    "-p", prompt,
    "--model", model,
    "--effort", "low",
    "--permission-mode", "dontAsk",
    "--strict-mcp-config",
    "--mcp-config", configPath,
    "--tools", "",
    "--allowedTools", ...toolNames.map((name) => `mcp__benchwiki__${name}`),
    "--output-format", "json",
    "--no-session-persistence",
    "--setting-sources", "project",
    "--system-prompt", "Answer the user's research question using the available read-only wiki tools. Do not modify data.",
  ];
  const startedMono = performance.now();
  const child = spawn(claudeCommand, args, {
    cwd,
    windowsHide: true,
    stdio: ["ignore", "pipe", "pipe"],
    env: { ...process.env, CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC: "1" },
  });
  const out = [];
  const err = [];
  child.stdout.on("data", (chunk) => out.push(chunk));
  child.stderr.on("data", (chunk) => err.push(chunk));
  let timedOut = false;
  const timer = setTimeout(() => { timedOut = true; child.kill(); }, timeoutMs);
  let exit;
  try {
    exit = await once(child, "exit");
  } catch (error) {
    const type = error.code === "ENOENT" ? "claude_cli_unavailable" : "claude_cli_start_failed";
    throw infrastructureError(`Could not run ${claudeCommand}: ${error.message}`, type, error);
  } finally {
    clearTimeout(timer);
  }
  const [code, signal] = exit;
  const stdout = Buffer.concat(out).toString("utf8");
  const stderr = Buffer.concat(err).toString("utf8");
  await writeFile(stdoutPath, stdout, "utf8");
  await writeFile(stderrPath, stderr, "utf8");
  let parsed;
  try { parsed = JSON.parse(stdout); } catch { /* preserved for inspection */ }
  await writeFile(outputPath, `${json(parsed ?? { unparsedStdout: stdout, stderr })}\n`, "utf8");
  return { code, signal, timedOut, stdout, stderr, parsed, wallMs: Number((performance.now() - startedMono).toFixed(3)) };
}

function readTrace(file) {
  return readFile(file, "utf8").then((text) => text.split(/\r?\n/).filter(Boolean).map((line) => JSON.parse(line))).catch((error) => {
    if (error.code === "ENOENT") return [];
    throw error;
  });
}

function finalText(parsed) {
  const result = parsed?.result;
  if (typeof result === "string") return result;
  if (Array.isArray(result)) return result.map((part) => part?.text ?? "").join("\n");
  return typeof parsed?.text === "string" ? parsed.text : "";
}

function citationFields(text) {
  let articleId = "";
  let anchor = "";
  let cannotFind = false;
  try {
    const fenced = text.match(/```(?:json)?\s*([\s\S]*?)```/i);
    const value = JSON.parse(fenced ? fenced[1] : text);
    articleId = String(value.article_id ?? value.articleId ?? "");
    anchor = String(value.anchor ?? "");
    cannotFind = value.cannot_find === true || value.cannotFind === true;
    const citation = String(value.citation ?? value.source ?? "");
    const match = citation.match(/article=([^#\s)]+)(?:#([^\s)]+))?/);
    if (!articleId && match) articleId = match[1];
    if (!anchor && match) anchor = match[2] ?? "";
  } catch {
    const match = text.match(/article=([^#\s)]+)(?:#([^\s)]+))?/);
    if (match) { articleId = match[1]; anchor = match[2] ?? ""; }
  }
  return { articleId, anchor, cannotFind };
}

function gradeTrial({ task, strategy, id, text, trace, cli }) {
  if (strategy === "outline-bounded" || strategy === "sections-bounded") {
    return gradeCandidateTrial({ task, strategy, id, text, trace, cli });
  }
  const citation = citationFields(text);
  const calls = trace.filter((entry) => entry.method === "tools/call");
  const readCalls = calls.filter((entry) => entry.params?.name === "wiki_read");
  const targetReads = readCalls.filter((entry) => entry.params?.arguments?.id === id);
  const validTargetReads = targetReads.filter((entry) => entry.result && !entry.result.isError && !entry.rpcError && !entry.proxyError && entry.http.status < 400);
  const toolsErrors = trace.filter((entry) => !entry.expectedProbe && (entry.rpcError || entry.isError || entry.proxyError
    || isHttpFailure(entry)));
  const hasAnswer = text.includes(task.answer);
  const reportsCannotFind = citation.cannotFind;
  const citedArticle = citation.articleId === id;
  const hasEvidence = validTargetReads.length > 0;
  const expectedAnchor = slugForHeading(task.headingTitle, task.headingOccurrence);
  const dataFor = (entry) => {
    try { return JSON.parse(entry.result?.content?.filter(p => p.type === "text").map(p => p.text).join("")); }
    catch { return null; }
  };
  const supportsAnswer = calls.some(entry => {
    if (entry.isError || entry.rpcError || entry.proxyError) return false;
    const data = dataFor(entry);
    return (data?.id === id && String(data.body ?? data.section ?? "").includes(task.answer))
      || data?.results?.some(hit => hit.id === id && String(hit.snippet ?? "").includes(task.answer));
  });
  const correctSectionRead = validTargetReads.some(entry => entry.params?.arguments?.anchor === expectedAnchor
    && String(dataFor(entry)?.section ?? "").includes(task.answer));
  const anchorEvidence = citation.anchor === expectedAnchor && correctSectionRead;
  const outlineIndex = validTargetReads.findIndex((entry) => entry.params?.arguments?.outline === true);
  const anchorIndex = validTargetReads.findIndex((entry) => entry.params?.arguments?.anchor === expectedAnchor
    && String(dataFor(entry)?.section ?? "").includes(task.answer));
  const policyAdherent = strategy === "full"
    ? targetReads.some((entry) => entry.params?.arguments?.outline !== true && !entry.params?.arguments?.anchor)
    : outlineIndex >= 0 && anchorIndex > outlineIndex;
  const strictPolicyAdherent = strategy === "full"
    ? policyAdherent && readCalls.every((entry) => entry.params?.arguments?.outline !== true && !entry.params?.arguments?.anchor)
    : policyAdherent && readCalls.every((entry) => entry.params?.arguments?.outline === true || Boolean(entry.params?.arguments?.anchor));
  const retrievalSuccess = cli.code === 0 && !cli.timedOut && toolsErrors.length === 0 && supportsAnswer && hasAnswer
    && !reportsCannotFind && citedArticle;
  const success = retrievalSuccess && hasEvidence && strictPolicyAdherent;
  return {
    success,
    hasAnswer,
    retrievalSuccess,
    supportsAnswer,
    correctSectionRead,
    reportsCannotFind,
    citedArticle,
    anchorEvidence,
    hasEvidence,
    policyAdherent: strictPolicyAdherent,
    toolErrorCount: toolsErrors.length,
    readCallCount: readCalls.length,
    targetReadCallCount: targetReads.length,
    validTargetReadCount: validTargetReads.length,
    citation: { articleId: citation.articleId || null, anchor: citation.anchor || null },
    cli: { exitCode: cli.code, signal: cli.signal, timedOut: cli.timedOut },
  };
}

function gradeCandidateTrial({ task, strategy, id, text, trace, cli }) {
  const citation = citationFields(text);
  const calls = trace.filter((entry) => entry.method === "tools/call");
  const readCalls = calls.filter((entry) => entry.params?.name === "wiki_read");
  const dataFor = (entry) => {
    try { return JSON.parse(entry.result?.content?.filter((part) => part.type === "text").map((part) => part.text).join("")); }
    catch { return null; }
  };
  const successful = (entry) => entry.result && !entry.result.isError && !entry.rpcError && !entry.proxyError
    && (entry.http?.status ?? 200) < 400;
  const toolsErrors = trace.filter((entry) => !entry.expectedProbe && (entry.rpcError || entry.isError || entry.proxyError || isHttpFailure(entry)));
  const expectedAnchor = slugForHeading(task.headingTitle, task.headingOccurrence);
  const indexedCalls = trace.map((entry, index) => ({ entry, index }))
    .filter(({ entry }) => entry.method === "tools/call");
  const targetSearches = indexedCalls.filter(({ entry }) => entry.params?.name === "wiki_search" && successful(entry)
    && dataFor(entry)?.results?.some((hit) => hit.id === id));
  const sectionSearchHits = indexedCalls.flatMap(({ entry, index }) => {
    if (entry.params?.name !== "wiki_search_sections" || !successful(entry)) return [];
    return (dataFor(entry)?.results ?? []).filter((hit) => hit.id === id && hit.anchor === expectedAnchor)
      .map((hit) => ({ hit, index }));
  });
  const outlineRecords = readCalls.map((entry) => ({ entry, index: trace.indexOf(entry) }))
    .filter(({ entry }) => entry.params?.arguments?.id === id && entry.params?.arguments?.outline === true && successful(entry))
    .filter(({ entry }) => {
      const data = dataFor(entry);
      const headings = data?.headings ?? data?.outline;
      if (!Array.isArray(headings)) return false;
      const occurrences = new Map();
      return headings.some((heading) => {
        const title = typeof heading === "string" ? heading : heading?.title;
        if (typeof title !== "string") return heading?.anchor === expectedAnchor;
        const occurrence = (occurrences.get(title) ?? 0) + 1;
        occurrences.set(title, occurrence);
        return (heading?.anchor ?? slugForHeading(title, occurrence)) === expectedAnchor;
      });
    });
  const successfulReads = readCalls.filter(successful);
  const targetBoundedReads = successfulReads.filter((entry) => {
    const args = entry.params?.arguments ?? {};
    const data = dataFor(entry);
    return args.id === id && args.anchor === expectedAnchor && args.max_chars === 4000
      && Number.isInteger(args.revision) && args.revision > 0
      && data?.id === id && data?.anchor === expectedAnchor && data?.revision === args.revision
      && data?.partial === true && typeof data.text === "string" && Number.isInteger(data.offset)
      && Number.isInteger(data.chars) && data.chars === Array.from(data.text).length
      && Number.isInteger(data.total_chars) && typeof data.has_more === "boolean";
  });
  const sameAddress = (left, right) => left && right
    && left.id === right.id && left.revision === right.revision && left.anchor === right.anchor
    && left.offset === right.offset && left.max_chars === right.max_chars;
  const boundedEntries = targetBoundedReads.map((entry) => ({ entry, index: trace.indexOf(entry), args: entry.params.arguments, data: dataFor(entry) }));
  const firstBounded = boundedEntries[0];
  let boundedChainValid = boundedEntries.length > 0;
  for (let index = 1; index < boundedEntries.length; index++) {
    const prior = boundedEntries[index - 1];
    const current = boundedEntries[index];
    boundedChainValid = boundedChainValid && prior.data.has_more === true
      && sameAddress(prior.data.next_read, current.args)
      && current.args.offset === prior.data.offset + prior.data.chars
      && current.data.total_chars === prior.data.total_chars
      && current.data.revision === prior.data.revision && current.data.anchor === prior.data.anchor
      && current.index > prior.index;
  }
  const chunkedText = boundedChainValid ? boundedEntries.map(({ data }) => data.text).join("") : "";
  const boundedAnswer = chunkedText.includes(task.answer);
  const answerFoundInCalls = calls.some((entry) => {
    if (!successful(entry)) return false;
    const data = dataFor(entry);
    return (data?.id === id && String(data.body ?? data.section ?? data.text ?? "").includes(task.answer))
      || data?.results?.some((hit) => hit.id === id && String(hit.snippet ?? "").includes(task.answer));
  });
  const retrievalSuccess = cli.code === 0 && !cli.timedOut && toolsErrors.length === 0 && answerFoundInCalls
    && text.includes(task.answer) && !citation.cannotFind && citation.articleId === id;

  let routeWorkflow = false;
  let selectedSearchIndex = -1;
  let selectedOutline = null;
  if (strategy === "outline-bounded") {
    const targetSearch = targetSearches.find(({ index }) => firstBounded && index < firstBounded.index);
    selectedOutline = outlineRecords.find(({ index, entry }) => targetSearch && firstBounded
      && index > targetSearch.index && index < firstBounded.index
      && dataFor(entry).revision === firstBounded.args.revision);
    selectedSearchIndex = targetSearch?.index ?? -1;
    routeWorkflow = Boolean(targetSearch && selectedOutline && firstBounded
      && (firstBounded.args.offset === 0 || firstBounded.args.offset === undefined));
  } else {
    const matching = sectionSearchHits.find(({ hit, index }) => {
      const args = hit.read_args;
      return firstBounded && index < firstBounded.index && args
        && args.id === id && args.revision === hit.revision && args.anchor === expectedAnchor && args.max_chars === 4000;
    });
    const returnedArgs = matching?.hit.read_args;
    selectedSearchIndex = matching?.index ?? -1;
    routeWorkflow = Boolean(returnedArgs && firstBounded && sameAddress(returnedArgs, firstBounded.args));
  }

  const readRouteValid = readCalls.length > 0 && readCalls.every((entry) => {
    const args = entry.params?.arguments ?? {};
    if (strategy === "outline-bounded" && args.id === id && args.outline === true && successful(entry)
      && outlineRecords.some((record) => record.entry === entry)) return true;
    if (!targetBoundedReads.includes(entry)) return false;
    return true;
  });
  const firstArgsMatchRoute = strategy === "outline-bounded"
    ? Boolean(firstBounded && selectedOutline && firstBounded.args.revision === dataFor(selectedOutline.entry).revision)
    : Boolean(firstBounded && sectionSearchHits.some(({ hit, index }) => index === selectedSearchIndex && sameAddress(hit.read_args, firstBounded.args)));
  const routeCompliance = routeWorkflow && firstArgsMatchRoute && boundedChainValid && readRouteValid && boundedAnswer
    && citation.articleId === id && citation.anchor === expectedAnchor;
  const success = retrievalSuccess && routeCompliance;
  return {
    success,
    hasAnswer: text.includes(task.answer),
    retrievalSuccess,
    supportsAnswer: answerFoundInCalls,
    boundedAnswer,
    routeCompliance,
    policyAdherent: routeCompliance,
    correctSectionRead: boundedAnswer,
    reportsCannotFind: citation.cannotFind,
    citedArticle: citation.articleId === id,
    anchorEvidence: citation.anchor === expectedAnchor && boundedAnswer,
    hasEvidence: targetBoundedReads.length > 0,
    toolErrorCount: toolsErrors.length,
    readCallCount: readCalls.length,
    targetReadCallCount: readCalls.filter((entry) => entry.params?.arguments?.id === id).length,
    validTargetReadCount: targetBoundedReads.length,
    citation: { articleId: citation.articleId || null, anchor: citation.anchor || null },
    cli: { exitCode: cli.code, signal: cli.signal, timedOut: cli.timedOut },
  };
}

function slugForHeading(title, occurrence) {
  const base = title.toLowerCase().replace(/[^\p{L}\p{N}]+/gu, "-").replace(/^-+|-+$/g, "") || "section";
  return occurrence > 1 ? `${base}-${occurrence}` : base;
}

function isHttpFailure(entry) {
  return entry.http.status >= 400 && !(entry.http.requestMethod === "GET" && entry.http.status === 405);
}

function providerTokenCounts(usage) {
  if (!usage || typeof usage !== "object") return null;
  const inputTokens = usage.input_tokens ?? usage.inputTokens ?? null;
  const cachedInputTokens = usage.cache_read_input_tokens ?? usage.cacheReadInputTokens ?? null;
  const cacheCreationInputTokens = usage.cache_creation_input_tokens ?? usage.cacheCreationInputTokens ?? null;
  const outputTokens = usage.output_tokens ?? usage.outputTokens ?? null;
  const inputParts = [inputTokens, cachedInputTokens, cacheCreationInputTokens].filter(Number.isFinite);
  const totalInputTokens = inputParts.length ? inputParts.reduce((sum, value) => sum + value, 0) : null;
  const totalTokens = totalInputTokens !== null && Number.isFinite(outputTokens) ? totalInputTokens + outputTokens : null;
  return { inputTokens, cachedInputTokens, cacheCreationInputTokens, totalInputTokens, outputTokens, totalTokens };
}

async function runTrial({ trial, corpus, task, strategy, repeat, binaryPath, corpusHash, candidateManifestSha256 }) {
  const trialDir = path.join(runRoot, trial.key);
  await mkdir(trialDir, { recursive: true });
  const dataDir = path.join(trialDir, "db");
  await mkdir(dataDir, { recursive: true });
  const serverPort = await reservePort();
  const baseUrl = `http://127.0.0.1:${serverPort}`;
  const serverOut = [];
  const wiki = spawn(binaryPath, ["-addr", `127.0.0.1:${serverPort}`, "-data", dataDir, "-web", path.join(root, "web", "absent")], {
    cwd: root, windowsHide: true, stdio: ["ignore", "pipe", "pipe"],
  });
  wiki.stdout.on("data", (chunk) => serverOut.push(chunk));
  wiki.stderr.on("data", (chunk) => serverOut.push(chunk));
  let proxy;
  let cli;
  const tracePath = path.join(trialDir, "trace.jsonl");
  try {
    await waitForServer(baseUrl, wiki);
    const mapped = await seedCorpus(baseUrl, corpus);
    proxy = await startRecordingProxy(`${baseUrl}/mcp`, tracePath, corpus.version);
    const isolatedCwd = await mkdtemp(path.join(tmpdir(), "aiwiki-agent-session-"));
    const configPath = path.join(trialDir, "mcp-config.json");
    await writeFile(configPath, `${json({ mcpServers: { benchwiki: { type: "http", url: proxy.url } } })}\n`, "utf8");
    const candidate = strategy === "outline-bounded" || strategy === "sections-bounded";
    const policy = candidate ? candidatePolicies[strategy] : strategy === "full"
      ? "Use wiki_search, then read the matching article with wiki_read and no outline or anchor so you see its full body."
      : "Use wiki_search, read the matching article with wiki_read outline=true, then read only the relevant section using its returned anchor.";
    const prompt = [
      "Start with wiki_bootstrap once, without arguments. Use only read-only wiki tools.",
      `Question: ${task.query}`,
      policy,
      strategy === "full"
        ? "Return one JSON object with answer, article_id, anchor:null, and cannot_find. Set cannot_find=true when evidence is insufficient. Cite the article ID; the full-body strategy does not expose a returned anchor, so leave anchor null."
        : "Return one JSON object with answer, article_id, anchor, and cannot_find. Set cannot_find=true when evidence is insufficient. Cite the article ID and the anchor returned by wiki_read; never guess either value.",
      "Keep answer under 120 words.",
    ].join("\n\n");
    cli = await runClaude({
      prompt,
      configPath,
      outputPath: path.join(trialDir, "claude-result.json"),
      stdoutPath: path.join(trialDir, "claude.stdout"),
      stderrPath: path.join(trialDir, "claude.stderr"),
      cwd: isolatedCwd,
    });
    await proxy.flush();
    const trace = await readTrace(tracePath);
    const text = finalText(cli.parsed);
    await writeFile(path.join(trialDir, "server.log"), Buffer.concat(serverOut));
    const usage = cli.parsed?.usage ?? null;
    const modelUsage = cli.parsed?.modelUsage ?? null;
    const modelUsageKeys = modelUsage && typeof modelUsage === "object" ? Object.keys(modelUsage) : [];
    const cliCompleted = cli.code === 0 && !cli.timedOut && cli.parsed && cli.parsed.is_error !== true && typeof cli.parsed.result === "string";
    const grade = cliCompleted
      ? gradeTrial({ task, strategy, id: mapped.ids.get(task.articleKey), text, trace, cli })
      : null;
    return {
      key: trial.key,
      task: task.key,
      strategy,
      questionVersion: candidate ? candidateQuestionVersion : null,
      candidateManifestSha256: candidate ? candidateManifestSha256 : null,
      status: cliCompleted ? "completed" : "infrastructure_failure",
      infrastructureFailure: cliCompleted ? null : {
        type: cli.timedOut ? "claude_timeout" : cli.code !== 0 ? "claude_nonzero_exit" : "invalid_claude_result",
        exitCode: cli.code,
      },
      repeat,
      order: trial.order,
      articleId: mapped.ids.get(task.articleKey),
      startedAt: trace[0]?.at ?? null,
      completedAt: new Date().toISOString(),
      result: grade,
      providerUsage: usage,
      providerTokenCounts: providerTokenCounts(usage),
      providerModelUsage: modelUsage,
      resolvedProviderModel: modelUsageKeys[0] ?? null,
      providerModelTokenCounts: modelUsage && typeof modelUsage === "object"
        ? Object.fromEntries(Object.entries(modelUsage).map(([model, counts]) => [model, {
          inputTokens: counts.inputTokens ?? counts.input_tokens ?? null,
          cachedInputTokens: counts.cacheReadInputTokens ?? counts.cache_read_input_tokens ?? null,
          cacheCreationInputTokens: counts.cacheCreationInputTokens ?? counts.cache_creation_input_tokens ?? null,
          outputTokens: counts.outputTokens ?? counts.output_tokens ?? null,
        }]))
        : null,
      cliDurations: {
        durationMs: cli.parsed?.duration_ms ?? null,
        apiDurationMs: cli.parsed?.duration_api_ms ?? null,
        wallMs: cli.wallMs,
      },
      numTurns: cli.parsed?.num_turns ?? null,
      providerReportedCostUsd: cli.parsed?.total_cost_usd ?? null,
      providerCostNote: "Provider-reported usage estimate; not an invoice.",
      trace: summarizeTrace(trace),
      files: { trace: path.relative(runRoot, tracePath), claudeJson: path.relative(runRoot, path.join(trialDir, "claude-result.json")) },
      corpusSha256: corpusHash,
    };
  } finally {
    await stopChild(cli?.child);
    if (proxy) await proxy.flush().catch(() => {});
    await closeServer(proxy?.server);
    await stopChild(wiki);
    await writeFile(path.join(trialDir, "server.log"), Buffer.concat(serverOut)).catch(() => {});
  }
}

function summarizeTrace(trace) {
  const calls = trace.filter((entry) => entry.method === "tools/call");
  const sum = (items, field) => items.reduce((n, entry) => n + (entry.tokens?.[field] ?? 0), 0);
  return {
    httpRequests: trace.length,
    calls: calls.map((entry) => ({
      name: entry.params?.name ?? entry.method ?? "http",
      arguments: entry.params?.arguments ?? null,
      isError: entry.isError,
      rpcError: Boolean(entry.rpcError),
      requestBytes: entry.wireBytes.request,
      responseBytes: entry.wireBytes.response,
      requestTokens: entry.tokens.requestArguments,
      responseTokens: entry.tokens.responseResult,
    })),
    wireBytes: trace.reduce((sumBytes, entry) => sumBytes + entry.wireBytes.total, 0),
    apiElapsedMs: Number(trace.reduce((sumMs, entry) => sumMs + (entry.apiElapsedMs ?? 0), 0).toFixed(3)),
    accounting: {
      initializeInstructionTokens: sum(trace, "initializeInstructions"),
      toolsSchemaTokens: sum(trace, "toolsSchema"),
      bootstrapTokens: sum(trace, "bootstrap"),
      bootstrapAlsoIncludedInToolResultTokens: true,
      toolArgumentTokens: sum(trace, "requestArguments"),
      toolResultTokens: sum(trace, "responseResult"),
      tokenizer: "cl100k_base proxy; provider usage below is authoritative for Claude",
    },
    expectedProbes: trace.filter((entry) => entry.expectedProbe).map((entry) => ({method: entry.method ?? entry.http.requestMethod, status: entry.http.status})),
    errors: trace.filter((entry) => !entry.expectedProbe && (entry.rpcError || entry.isError || entry.proxyError || isHttpFailure(entry))).map((entry) => ({
      method: entry.method ?? entry.http.requestMethod,
      status: entry.http.status,
      message: entry.rpcError?.message ?? entry.proxyError ?? (entry.isError ? "MCP result isError=true" : undefined),
    })),
  };
}

async function buildServer(binaryPath) {
  const result = spawnSync("go", ["build", "-o", binaryPath, "./cmd/aiwiki"], {
    cwd: root,
    encoding: "utf8",
    windowsHide: true,
    timeout: 120_000,
  });
  if (result.error || result.status !== 0) {
    throw new Error(`Go server build failed: ${result.error?.message ?? result.stderr ?? result.stdout}`);
  }
}

async function main() {
  const runnerSha256 = sha256(await readFile(fileURLToPath(import.meta.url)));
  const { limit, candidate } = parseRunOptions(process.argv.slice(2));
  const corpus = makeCorpus();
  const corpusHash = sha256(json(corpus));
  const tasks = corpus.readTasks.filter((task) => ["r02", "r06"].includes(task.key))
    .map((task) => candidate ? { ...task, query: candidateQuestions[task.key] } : task);
  if (tasks.length !== 2) throw new Error("Expected frozen read fixtures r02 and r06");
  const candidateManifest = candidate ? {
    format: "agent-candidate-manifest-v1",
    questionVersion: candidateQuestionVersion,
    questions: candidateQuestions,
    policies: candidatePolicies,
    design: { tasks: ["r02", "r06"], repeatsPerTaskPolicy: 2, plannedTrials: 8, model, effort: "low", counterbalanced: true },
    fixture: { version: corpus.version, sha256: corpusHash },
    historicalBaseline,
    runnerSha256,
  } : null;
  const candidateManifestSha256 = candidateManifest ? sha256(json(candidateManifest)) : null;
  await mkdir(runRoot, { recursive: true });
  if (candidateManifest) await writeFile(path.join(runRoot, "candidate-manifest.json"), `${JSON.stringify({ ...candidateManifest, manifestSha256: candidateManifestSha256 }, null, 2)}\n`, "utf8");
  const cliVersion = getClaudeVersion();
  const binaryPath = path.join(runRoot, process.platform === "win32" ? "aiwiki.exe" : "aiwiki");
  await buildServer(binaryPath);
  const binarySha256 = sha256(await readFile(binaryPath));

  const trials = [];
  let order = 0;
  for (let repeat = 1; repeat <= 2; repeat++) {
    for (let taskIndex = 0; taskIndex < tasks.length; taskIndex++) {
      const task = tasks[taskIndex];
      const strategies = candidate
        ? ((repeat + taskIndex) % 2 === 0 ? ["outline-bounded", "sections-bounded"] : ["sections-bounded", "outline-bounded"])
        : ((repeat + taskIndex) % 2 === 0 ? ["outline", "full"] : ["full", "outline"]);
      for (const strategy of strategies) {
        order++;
        trials.push({ key: `trial-${String(order).padStart(2, "0")}`, order, task, strategy, repeat });
      }
    }
  }

  const selectedTrials = trials.slice(0, limit);
  const results = [];
  for (const trial of selectedTrials) {
    process.stdout.write(`Running ${trial.key}: ${trial.task.key}/${trial.strategy}, repeat ${trial.repeat}\n`);
    try {
      results.push(await runTrial({ trial, task: trial.task, strategy: trial.strategy, repeat: trial.repeat, corpus, binaryPath, corpusHash, candidateManifestSha256 }));
    } catch (error) {
      results.push({
        key: trial.key,
        task: trial.task.key,
        strategy: trial.strategy,
        status: "infrastructure_failure",
        infrastructureFailure: { type: error.infrastructureType ?? "trial_setup_failed", message: error.message },
        repeat: trial.repeat,
        order: trial.order,
        result: null,
      });
    }
  }
  const completed = results.filter((trial) => trial.status === "completed");
  const qualitySuccesses = completed.filter((trial) => trial.result?.success).length;
  const infrastructureFailures = results.length - completed.length;
  const summary = {
    format: candidate ? "agent-trials-candidate-v1" : "agent-trials-v1",
    createdAt: new Date().toISOString(),
    model,
    cliVersion,
    effort: "low",
    environment: { node: process.version, platform: process.platform, arch: process.arch },
    source: { runnerSha256, corpusSha256: corpusHash, historicalBaseline: candidate ? historicalBaseline : null },
    runnerSha256,
    serverBinarySha256: binarySha256,
    fixture: { version: corpus.version, documentCount: corpus.documents.length, sha256: corpusHash, tasks: tasks.map(({ key }) => key) },
    candidate: candidate ? { questionVersion: candidateQuestionVersion, manifestSha256: candidateManifestSha256, manifestFile: "candidate-manifest.json" } : null,
    design: {
      strategyCount: 2,
      repeatsPerTaskStrategy: 2,
      plannedTrials: trials.length,
      attemptedTrials: results.length,
      complete: results.length === trials.length,
      limitedRun: results.length !== trials.length,
      isolatedSessions: results.length,
      completedTrials: completed.length,
      infrastructureFailures,
      qualityDenominator: completed.length,
      qualitySuccesses,
      retrievalSuccesses: completed.filter(trial => trial.result?.retrievalSuccess).length,
      routeComplianceSuccesses: completed.filter(trial => trial.result?.routeCompliance ?? trial.result?.policyAdherent).length,
      qualityFailures: completed.length - qualitySuccesses,
      qualitySuccessRate: completed.length ? qualitySuccesses / completed.length : null,
      parallelism: 1,
      timeoutSeconds: timeoutMs / 1_000,
    },
    trials: results,
    limitations: [
      "Provider token counts are taken only from Claude JSON usage/modelUsage; cl100k_base values are payload proxies.",
      "This pilot measures two frozen read tasks on a synthetic corpus and does not support broad agent-quality claims.",
      "Full Claude JSON and raw MCP traces remain in this ignored run directory and may contain local runtime details.",
    ],
  };
  const resultsPath = path.join(runRoot, "results.json");
  await writeFile(resultsPath, `${JSON.stringify(summary, null, 2)}\n`, "utf8");
  process.stdout.write(`${results.length < trials.length ? "Limited pilot" : "Pilot complete"}: ${qualitySuccesses}/${completed.length} completed sessions met quality criteria; ${infrastructureFailures} infrastructure failures; ${trials.length - results.length} trials omitted.\nResults: ${resultsPath}\n`);
}

export { gradeTrial };
if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  main().catch((error) => {
    const prefix = error.infrastructureType ? `Infrastructure failure [${error.infrastructureType}]` : "Agent-trials runner failed";
    process.stderr.write(`${prefix}: ${error.stack ?? error.message}\n`);
    process.exitCode = 1;
  });
}

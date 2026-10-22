// Pure helpers for article export: no DOM/React dependencies so scripts/qa-export.mjs can import them directly.

export const MAX_EMBEDDED_BYTES = 15 * 1024 * 1024;
export const EXPORT_CSP = "default-src 'none'; img-src data:; style-src 'unsafe-inline'";

export function escapeHtml(s: string): string {
  return s.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;').replace(/'/g, '&#39;');
}

/** `<title>-r<revision>.html`: forbidden characters and control chars removed, title cut to 80 chars, empty -> `article`. */
export function exportFileName(title: string, revision: number): string {
  const cleaned = [...title.replace(/[\\/:*?"<>|\u0000-\u001f\u007f]/g, '').replace(/\s+/g, ' ').trim()].slice(0, 80).join('').trim();
  return `${cleaned || 'article'}-r${revision}.html`;
}

export const EXPORT_CSS = `
*{box-sizing:border-box}
html{-webkit-text-size-adjust:100%}
body{margin:0;background:#fff;color:#222;font-family:'Segoe UI',Roboto,'DejaVu Sans',Arial,sans-serif;font-size:16px;line-height:1.6}
.doc{max-width:860px;margin:0 auto;padding:32px 20px 48px}
.doc-head{border-bottom:1px solid #ddd;margin-bottom:24px;padding-bottom:14px}
.doc-head h1.doc-title{margin:0 0 8px;font-size:2em;line-height:1.25}
.doc-meta{color:#555;font-size:.9em;margin:0;display:flex;flex-wrap:wrap;gap:4px 16px}
.doc-foot{border-top:1px solid #ddd;margin-top:40px;padding-top:12px;color:#666;font-size:.85em}
h1,h2,h3,h4,h5,h6{line-height:1.3;margin:1.6em 0 .6em}
p,ul,ol,blockquote,pre,table{margin:0 0 1em}
a{color:#3b5bdb}
blockquote{margin-left:0;padding:.2em 1em;border-left:4px solid #ccc;color:#444}
code{font-family:Consolas,'DejaVu Sans Mono','Courier New',monospace;font-size:.92em;background:#f3f3f6;padding:.1em .3em;border-radius:3px}
pre{background:#f6f6f8;border:1px solid #e2e2e8;border-radius:4px;padding:12px;overflow:auto}
pre code{background:none;padding:0}
img{max-width:100%;height:auto}
hr{border:0;border-top:1px solid #ddd;margin:1.5em 0}
.markdown-table-scroll{overflow-x:auto;margin:0 0 1em}
table{border-collapse:collapse;min-width:0}
th,td{border:1px solid #bbb;padding:6px 10px;text-align:left;vertical-align:top}
th{background:#f0f0f4}
.export-missing{color:#777;font-style:italic}
@media print{
body{font-size:11pt}
.doc{max-width:none;padding:0}
pre{white-space:pre-wrap;word-break:break-word;overflow:visible}
.markdown-table-scroll{overflow:visible}
table{font-size:9pt;width:100%}
th,td{word-break:break-word;overflow-wrap:anywhere}
tr,img{break-inside:avoid}
h1,h2,h3,h4{break-after:avoid}
img{max-width:100%}
a{color:inherit;text-decoration:underline}
@page{margin:16mm}
}
`;

export function buildExportDocument(parts: { title: string; metaHtml: string; bodyHtml: string; css: string; footerHtml?: string }): string {
  return `<!doctype html>
<html lang="ru">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta http-equiv="Content-Security-Policy" content="${EXPORT_CSP}">
<title>${escapeHtml(parts.title)}</title>
<style>${parts.css.replace(/<\/style/gi, '<\\/style')}</style>
</head>
<body>
<div class="doc">
${parts.metaHtml}
<main class="markdown">
${parts.bodyHtml}
</main>
${parts.footerHtml ?? ''}
</div>
</body>
</html>
`;
}

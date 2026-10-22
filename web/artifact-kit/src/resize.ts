// Auto height: tells the embedding page how tall the document is (contract: parent.postMessage({type:'aiwiki:resize', height}, '*')).

let started = false;
let enabled = true;
let last = -1;
let timer: ReturnType<typeof setTimeout> | undefined;
let lastSent = 0;

function measure(): number {
  const b = document.body;
  if (!b) return 0;
  const cs = getComputedStyle(b);
  return Math.ceil(b.getBoundingClientRect().height + (parseFloat(cs.marginTop) || 0) + (parseFloat(cs.marginBottom) || 0));
}

function send() {
  timer = undefined;
  if (!enabled || window.parent === window) return;
  const height = measure();
  if (height <= 0 || Math.abs(height - last) < 1) return;
  last = height;
  lastSent = Date.now();
  try { window.parent.postMessage({ type: 'aiwiki:resize', height }, '*'); } catch { /* sandboxed parent: nothing to do */ }
}

function schedule() {
  if (timer !== undefined) return;
  // Throttle: at most one message per 80 ms, trailing edge so the final size is always sent.
  timer = setTimeout(send, Math.max(0, 80 - (Date.now() - lastSent)));
}

export function autoResize(on = true): void {
  enabled = on;
  if (on) { last = -1; schedule(); }
}

export function startAutoResize(): void {
  if (started || typeof document === 'undefined' || window.parent === window) return;
  started = true;
  const init = () => {
    if (typeof ResizeObserver === 'function') {
      const ro = new ResizeObserver(schedule);
      ro.observe(document.body);
      ro.observe(document.documentElement);
    }
    new MutationObserver(schedule).observe(document.body, { childList: true, subtree: true, attributes: true, characterData: true });
    window.addEventListener('load', schedule);
    window.addEventListener('resize', schedule);
    schedule();
  };
  if (document.body) init(); else document.addEventListener('DOMContentLoaded', init, { once: true });
}

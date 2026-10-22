// Height reporter for artifacts that do not use the AI Wiki kit. The server adds
// it to the served entry document (the stored files never change); the parent
// page accepts the message only from this frame and clamps the height.
(function () {
  if (window.parent === window) return;
  var pending = false, last = -1;
  function send() {
    pending = false;
    var height = Math.ceil(document.documentElement.scrollHeight);
    if (height === last) return;
    last = height;
    window.parent.postMessage({ type: 'aiwiki:resize', height: height }, '*');
  }
  function schedule() {
    if (pending) return;
    pending = true;
    requestAnimationFrame(send);
  }
  function start() {
    if (window.AIWiki) return; // the kit reports the height itself
    if (typeof ResizeObserver === 'function') new ResizeObserver(schedule).observe(document.documentElement);
    window.addEventListener('load', schedule);
    schedule();
  }
  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', start);
  else start();
})();

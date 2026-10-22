// Wiki theme for artifacts that do not use the AI Wiki kit. The wiki opens the
// frame with ?theme=light|dark; the server adds this script before the first
// element of the served entry document (the stored files never change), so it
// runs before the first paint. It sets data-theme on <html>, which matches the
// usual `:root[data-theme="dark"]` CSS; the parser adds the author's own <html>
// attributes later and keeps ours. The kit uses data-aiwiki-theme instead.
(function () {
  var m = /[?&]theme=(light|dark)(?:&|$)/.exec(location.search);
  if (!m) return;
  var root = document.documentElement;
  if (!root.hasAttribute('data-theme')) root.setAttribute('data-theme', m[1]);
})();

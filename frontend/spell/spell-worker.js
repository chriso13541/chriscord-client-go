// Spell checking runs here, off the main thread, so loading the ~550 KB
// dictionary and making suggestions never freezes the app.
// Typo.js (BSD-3-Clause) with the SCOWL-based en_US Hunspell dictionary —
// see ../licenses/typo-js-LICENSE.txt and ../licenses/en_US-dictionary-README.txt.
importScripts('typo.js');
let typo = null;
const custom = new Set();       // "Add to Dictionary" words (lower-case)
const pending = [];             // requests that arrived before the dictionary loaded

Promise.all([fetch('en_US.aff').then(r => r.text()), fetch('en_US.dic').then(r => r.text())])
  .then(([aff, dic]) => {
    typo = new Typo('en_US', aff, dic);
    postMessage({ type: 'ready' });
    pending.splice(0).forEach(handle);
  })
  .catch(err => postMessage({ type: 'error', error: String(err) }));

const known = w => custom.has(w.toLowerCase()) || typo.check(w);

function handle(m) {
  if (m.type === 'add') { custom.add(m.word.toLowerCase()); return; }
  if (m.type === 'setCustom') { custom.clear(); (m.words || []).forEach(w => custom.add(w.toLowerCase())); return; }
  if (!typo) { pending.push(m); return; }
  if (m.type === 'check') {
    postMessage({ type: 'checked', id: m.id, results: m.words.map(w => [w, known(w)]) });
  } else if (m.type === 'suggest') {
    let s = [];
    try { s = typo.suggest(m.word, 5); } catch {}
    postMessage({ type: 'suggestions', id: m.id, word: m.word, suggestions: s });
  }
}
onmessage = e => handle(e.data);

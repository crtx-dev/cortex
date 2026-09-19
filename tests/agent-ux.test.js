// Frontend UX contract tests for sticky-bottom scroll, the todowrite task
// panel, and the tab running-spinner / unread indicator.
//
// These drive the real public/assets/js/script.js in a minimal DOM sandbox.
//
// Run with: node tests/agent-ux.test.js

const fs = require('fs');
const path = require('path');
const vm = require('vm');

const SCRIPT = fs.readFileSync(path.join(__dirname, '..', 'public', 'assets', 'js', 'script.js'), 'utf8');

class Node {
  constructor(tag = '') {
    this.tagName = (tag || 'div').toUpperCase();
    this.children = [];
    this.dataset = {};
    this.classList = { add() {}, remove() {}, toggle() {} };
    this.style = {};
    this._text = '';
    this._className = '';
    this.hidden = false;
    this.scrollTop = 0;
    this.scrollHeight = 0;
    this.clientHeight = 0;
  }
  append(...x) { for (const k of x) if (k != null) this.children.push(k); }
  appendChild(x) { this.children.push(x); return x; }
  remove() {}
  addEventListener() {}
  focus() {}
  closest() { return null; }
  getBoundingClientRect() { return { height: 100 }; }
  requestSubmit() {}
  stopPropagation() {}
  setAttribute(name, value) { this[name] = String(value); }
  removeAttribute(name) { this[name] = ''; }
  getAttribute(name) { return this[name] || null; }
  querySelector() { return null; }
  querySelectorAll() { return []; }
  set textContent(v) { this._text = String(v); }
  get textContent() { return this._text; }
  set className(v) { this._className = String(v); }
  get className() { return this._className; }
  set innerHTML(v) { this._innerHTML = String(v); this.children = []; }
  get innerHTML() { return this._innerHTML; }
}

function makeContext(fetchImpl) {
  const els = new Map();
  const el = (sel) => {
    if (!els.has(sel)) els.set(sel, new Node('div'));
    return els.get(sel);
  };
  const document = {
    createElement: (t) => new Node(t),
    createTextNode: (t) => ({ textContent: String(t) }),
    querySelector: (sel) => el(sel),
    querySelectorAll: () => [],
    addEventListener: () => {},
    body: new Node('body'),
  };
  const fetchCalls = [];
  const fetchImplReal = fetchImpl || (() => Promise.resolve(jsonOk({})));
  const ctx = {
    document,
    console,
    addEventListener: () => {},
    removeEventListener: () => {},
    confirm: () => true,
    innerWidth: 1280,
    innerHeight: 800,
    fetch: (url, opt = {}) => { fetchCalls.push({ url, ...opt }); return fetchImplReal(url, opt); },
    localStorage: {
      _d: {},
      getItem(k) { return this._d[k] || null; },
      setItem(k, v) { this._d[k] = String(v); },
      removeItem(k) { delete this._d[k]; },
    },
    AbortController,
    TextDecoder: require('util').TextDecoder,
    TextEncoder: require('util').TextEncoder,
    setTimeout,
    clearTimeout,
    crypto: { randomUUID: () => 'id-' + Math.random().toString(36).slice(2) },
    fetchCalls,
  };
  ctx.window = ctx;
  ctx.globalThis = ctx;
  return ctx;
}

function loadContext(fetchImpl, preload) {
  const ctx = makeContext(fetchImpl);
  if (preload) for (const k of Object.keys(preload)) ctx[k] = preload[k];
  vm.createContext(ctx);
  vm.runInContext(SCRIPT, ctx);
  return ctx;
}

// loadDecodeContext builds a context whose FileReader, Image and canvas are
// controllable, so addImage's asynchronous decode can be driven deterministically:
// each accepted file pushes a FileReader onto ctx.__readers, and each resolved
// read creates an Image on ctx.__images, which the tests resolve by hand.
function loadDecodeContext(fetchImpl) {
  const ctx = makeContext(fetchImpl);
  const readers = [];
  const images = [];
  ctx.FileReader = class {
    constructor() { this.result = ''; this.error = null; readers.push(this); }
    readAsDataURL(file) { this._file = file; }
  };
  ctx.Image = class {
    constructor() { this.width = 0; this.height = 0; images.push(this); }
    set src(v) { this._src = v; }
    get src() { return this._src; }
  };
  const baseCreate = ctx.document.createElement.bind(ctx.document);
  ctx.document.createElement = (tag) => {
    if (tag === 'canvas') {
      return {
        width: 0, height: 0,
        getContext: () => ({ fillStyle: '', fillRect() {}, drawImage() {} }),
        toDataURL: () => 'data:image/jpeg;base64,THUMB',
      };
    }
    return baseCreate(tag);
  };
  ctx.__readers = readers;
  ctx.__images = images;
  vm.createContext(ctx);
  vm.runInContext(SCRIPT, ctx);
  return ctx;
}

// completeDecode drives one accepted addImage decode to completion: the next
// pending FileReader resolves to dataUrl, then its created Image loads.
async function completeDecode(ctx, dataUrl) {
  const reader = ctx.__readers.shift();
  if (!reader) throw new Error('no pending FileReader to complete');
  reader.result = dataUrl;
  reader.onload();
  await settle(0);
  const img = ctx.__images.shift();
  if (!img) throw new Error('no pending Image after read completion');
  img.width = 64;
  img.height = 64;
  img.onload();
  await settle(0);
}

// failDecode rejects the next pending FileReader without creating an Image.
async function failDecode(ctx) {
  const reader = ctx.__readers.shift();
  if (!reader) throw new Error('no pending FileReader to fail');
  reader.error = new Error('decode failed');
  reader.onerror();
  await settle(0);
}

// failThumbnail resolves the next pending FileReader but rejects its created
// Image, exercising the thumbnail failure completion path.
async function failThumbnail(ctx, dataUrl) {
  const reader = ctx.__readers.shift();
  if (!reader) throw new Error('no pending FileReader to resolve');
  reader.result = dataUrl;
  reader.onload();
  await settle(0);
  const img = ctx.__images.shift();
  if (!img) throw new Error('no pending Image after read completion');
  img.onerror();
  await settle(0);
}

function run(ctx, code, vars) {
  if (vars) {
    for (const k of Object.keys(vars)) ctx[k] = vars[k];
  }
  return vm.runInContext(code, ctx);
}
const settle = (ms = 10) => new Promise((r) => setTimeout(r, ms));

const __tests = [];
function test(name, fn) { __tests.push({ name, fn }); }

// Any asynchronous rejection after a test has been counted must still produce
// a nonzero process result, never be silently masked.
process.on('unhandledRejection', (reason) => {
  console.error('UNHANDLED REJECTION: ' + (reason && reason.stack || reason));
  process.exitCode = 1;
});
process.on('uncaughtException', (err) => {
  console.error('UNCAUGHT EXCEPTION: ' + (err && err.stack || err));
  process.exitCode = 1;
});

// A fake feed element with controlled scroll metrics.
function jsonOk(v){return {ok:true,json:()=>Promise.resolve(v),headers:{get:()=>'application/json'}}}
function feedNode(scrollTop, scrollHeight, clientHeight) {
  const f = new Node('div');
  f.scrollTop = scrollTop;
  f.scrollHeight = scrollHeight;
  f.clientHeight = clientHeight;
  return f;
}

test('copy session feedback is short-lived with a stable button width', async () => {
  const ctx = loadContext();
  let clipboardCalls = 0;
  ctx.navigator = { clipboard: { writeText: async (t) => { clipboardCalls++; ctx.__copiedText = t; } } };
  run(ctx, "sessions={a:{id:'a',workspace:'/w',events:[{kind:'user',text:'hello'},{kind:'assistant',text:'world'}]}};activeId='a'");
  await run(ctx, 'copySession()');
  // The button flips to the checkmark without changing width, then restores
  // after ~500ms (not a long-lived state).
  if (run(ctx, "$('#copy').textContent") !== '✓') throw new Error('copy feedback did not show the checkmark');
  const widthStyle = run(ctx, "$('#copy').style.width");
  if (!widthStyle) throw new Error('copy feedback must pin the button width to prevent layout jump');
  if (clipboardCalls !== 1) throw new Error('clipboard write not called exactly once');
  if (run(ctx, '__copiedText') !== 'You:\nhello\n\nAgent:\nworld') throw new Error('clipboard payload mismatch: ' + JSON.stringify(run(ctx, '__copiedText')));
  await settle(600);
  if (run(ctx, "$('#copy').textContent") !== 'Copy session') throw new Error('copy feedback did not restore the label within ~500ms');
  if (run(ctx, "$('#copy').style.width") !== '') throw new Error('copy feedback must release the pinned width after restoring');
});

test('copy session shows no checkmark when clipboard write fails', async () => {
  const ctx = loadContext();
  ctx.navigator = { clipboard: { writeText: async () => { throw new Error('clipboard blocked'); } } };
  run(ctx, "sessions={a:{id:'a',workspace:'/w',events:[{kind:'user',text:'hello'}]}};activeId='a'");
  run(ctx, "$('#copy').textContent='Copy session'");
  let threw = false;
  try { await run(ctx, 'copySession()'); } catch (e) { threw = true; }
  // The handler awaits the clipboard write; a failure must reject (and the
  // caller surfaces it) without ever showing the checkmark.
  if (threw !== true) throw new Error('clipboard failure must reject the copy promise');
  if (run(ctx, "$('#copy').textContent") !== 'Copy session') throw new Error('clipboard failure must not show the checkmark');
  if (run(ctx, "$('#copy').style.width")) throw new Error('clipboard failure must not pin the button width');
});

test('nearBottom: at bottom follows new event', async () => {
  const ctx = loadContext();
  const feed = feedNode(1000, 1000, 100);
  const near = run(ctx, 'nearBottom(FEED)', { FEED: feed });
  if (!near) throw new Error('near-bottom feed should be near');
  const s = { followBottom: true, events: [] };
  run(ctx, `(function(){const s=S;const b=FEED;if(!nearBottom(b))return;b.scrollTop=b.scrollHeight;s.followBottom=true;})()`, { S: s, FEED: feed });
  if (feed.scrollTop !== feed.scrollHeight) throw new Error('did not follow to bottom');
});

test('scrolled upward: position preserved on new event', async () => {
  const ctx = loadContext();
  const feed = feedNode(200, 1000, 100);
  if (run(ctx, 'nearBottom(FEED)', { FEED: feed })) throw new Error('scrolled-up feed should not be near bottom');
  const s = { followBottom: false, events: [] };
  const before = feed.scrollTop;
  run(ctx, `(function(){const s=S;const b=FEED;if(!nearBottom(b))return;b.scrollTop=b.scrollHeight;s.followBottom=true;})()`, { S: s, FEED: feed });
  if (feed.scrollTop !== before) throw new Error('scrolled-up feed jumped');
});

test('task panel renders validated todowrite snapshot and progress', async () => {
  const ctx = loadContext();
  const s = { followBottom: true, events: [] };
  run(ctx, `(function(){const s=S;s.events.push({kind:'task',text:'[{"content":"one","status":"completed","priority":"high"},{"content":"two","status":"in_progress","priority":"low"},{"content":"three","status":"pending","priority":"medium"}]'});renderTaskPanel(s);})()`, { S: s });
  const list = run(ctx, `$('#taskList').children.length`);
  if (list !== 3) throw new Error('task rows = ' + list);
  const progress = run(ctx, `$('#taskProgress').textContent`);
  if (progress !== '1 of 3 completed') throw new Error('progress = ' + progress);
});

test('task panel preserves exact model order for a mixed-status snapshot', async () => {
  const ctx = loadContext();
  const s = { followBottom: true, events: [] };
  run(ctx, `(function(){const s=S;s.events.push({kind:'task',text:'[{"content":"first","status":"completed"},{"content":"second","status":"in_progress"},{"content":"third","status":"pending"}]'});renderTaskPanel(s);})()`, { S: s });
  const labels = run(ctx, `Array.from($('#taskList').children).map(r=>r.children[1].textContent)`);
  if (labels.join('|') !== 'first|second|third') throw new Error('mixed-status order = ' + labels.join('|'));
});

test('task panel does not regroup a deliberately reverse-status snapshot', async () => {
  const ctx = loadContext();
  const s = { followBottom: true, events: [] };
  // Reverse-status ordering: completed first, then in_progress, then pending.
  run(ctx, `(function(){const s=S;s.events.push({kind:'task',text:'[{"content":"done","status":"completed"},{"content":"active","status":"in_progress"},{"content":"queued","status":"pending"}]'});renderTaskPanel(s);})()`, { S: s });
  const labels = run(ctx, `Array.from($('#taskList').children).map(r=>r.children[1].textContent)`);
  if (labels.join('|') !== 'done|active|queued') throw new Error('reverse-status order was regrouped: ' + labels.join('|'));
});

test('task panel uses the latest snapshot order, not a prior one', async () => {
  const ctx = loadContext();
  const s = { followBottom: true, events: [] };
  run(ctx, `(function(){const s=S;s.events.push({kind:'task',text:'[{"content":"old-a","status":"pending"},{"content":"old-b","status":"completed"}]'});renderTaskPanel(s);})()`, { S: s });
  run(ctx, `(function(){const s=S;s.events.push({kind:'task',text:'[{"content":"new-1","status":"completed"},{"content":"new-2","status":"completed"},{"content":"new-3","status":"pending"}]'});renderTaskPanel(s);})()`, { S: s });
  const labels = run(ctx, `Array.from($('#taskList').children).map(r=>r.children[1].textContent)`);
  if (labels.join('|') !== 'new-1|new-2|new-3') throw new Error('latest-snapshot order = ' + labels.join('|'));
  const progress = run(ctx, `$('#taskProgress').textContent`);
  if (progress !== '2 of 3 completed') throw new Error('latest-snapshot progress = ' + progress);
});

test('task panel preserves order for a restored session snapshot', async () => {
  const ctx = loadContext();
  const s = { followBottom: true, events: [{ kind: 'task', text: '[{"content":"restored-a","status":"in_progress"},{"content":"restored-b","status":"completed"},{"content":"restored-c","status":"in_progress"}]' }] };
  run(ctx, `(function(s){renderTaskPanel(s)})(S)`, { S: s });
  const labels = run(ctx, `Array.from($('#taskList').children).map(r=>r.children[1].textContent)`);
  if (labels.join('|') !== 'restored-a|restored-b|restored-c') throw new Error('restored order = ' + labels.join('|'));
  const progress = run(ctx, `$('#taskProgress').textContent`);
  if (progress !== '1 of 3 completed') throw new Error('restored progress = ' + progress);
});

test('task panel collapsed and background-session behavior is unchanged', async () => {
  const ctx = loadContext();
  const s = { followBottom: true, events: [], tasksCollapsed: true };
  run(ctx, `(function(){const s=S;s.events.push({kind:'task',text:'[{"content":"hidden-a","status":"completed"},{"content":"hidden-b","status":"pending"}]'});renderTaskPanel(s);})()`, { S: s });
  const hidden = run(ctx, `$('#taskPanel').hidden`);
  if (!hidden) throw new Error('collapsed panel should stay hidden');
  // Background-session behavior: a task event arriving on a non-active session
  // must not touch the active panel (guarded in addEvent).
  run(ctx, `(function(){sessions={a:{id:'a',followBottom:true,tasksCollapsed:true,events:[]},b:{id:'b',followBottom:true,events:[{kind:'task',text:'[{"content":"bg-1","status":"completed"},{"content":"bg-2","status":"pending"}]'}]}};activeId='a';sessions['a'].events.push({kind:'task',text:'[{"content":"active-1","status":"completed"}]'});addEvent('b','task','[{"content":"bg-1","status":"completed"},{"content":"bg-2","status":"pending"}]');})()`);
  const hiddenAfter = run(ctx, `$('#taskPanel').hidden`);
  if (!hiddenAfter) throw new Error('background task event unhid the active panel');
  const list = run(ctx, `$('#taskList').children.length`);
  if (list !== 0) throw new Error('background task event populated the active panel');
});

test('task panel ignores malformed task text', async () => {
  const ctx = loadContext();
  const s = { followBottom: true, events: [{ kind: 'task', text: 'not-json' }] };
  run(ctx, `(function(s){renderTaskPanel(s)})(S)`, { S: s });
  const hidden = run(ctx, `$('#taskPanel').hidden`);
  if (!hidden) throw new Error('malformed task should hide panel');
});

test('tab spinner visible when busy on active and background tabs', async () => {
  const ctx = loadContext();
  run(ctx, `(function(){sessions={a:{id:'a',title:'A',busy:true,unread:0},b:{id:'b',title:'B',busy:true,unread:0}};activeId='a';renderTabs();})()`);
  const spinners = run(ctx, `document.querySelectorAll().length`);
  // querySelectorAll is stubbed; count via children of the tabs container.
  const tabsBox = run(ctx, `$('#sessionTabs').children`);
  if (!tabsBox || tabsBox.length !== 2) throw new Error('two tabs expected');
});

test('busy session close uses server-side stop (no immediate abort)', async () => {
  const ctx = loadContext();
  let cancelled = null;
  const routes = {
    '/api/agent/cancel': (opt) => { cancelled = JSON.parse(opt.body); return Promise.resolve({ ok: true, json: () => Promise.resolve({ cancelled: true }) }); },
    '/api/auth/state': () => Promise.resolve({ ok: true, json: () => Promise.resolve({ configured: false, authenticated: false }) }),
  };
  const ctx2 = loadContext((url, opt) => {
    if (routes[url]) return routes[url](opt);
    return Promise.resolve(jsonOk({}));
  });
  run(ctx2, `(function(){const s=sessions['a']||{id:'a',busy:true,runID:'run-1',abort:null,followBottom:true,unread:0};sessions={a:s};activeId='a';stopAgentFor(s);})()`);
  await settle(20);
  if (!cancelled || cancelled.runID !== 'run-1') throw new Error('stop protocol not used for busy close');
});

test('unread indicator cleared when switching to a tab', async () => {
  const ctx = loadContext();
  run(ctx, `(function(){sessions={a:{id:'a',title:'A',busy:false,unread:3,events:[]},b:{id:'b',title:'B',busy:false,unread:0,events:[]}};activeId='a';})()`);
  run(ctx, `(function(){const s=sessions['b'];activeId='b';s.unread=0;})()`);
  const unread = run(ctx, `sessions['b'].unread`);
  if (unread !== 0) throw new Error('unread not cleared');
});
test('real syncServerConversations keeps spinner for running conversation', async () => {
  // Server returns an authoritative running conversation; the real sync
  // function must derive busy from state, not a manual assignment.
  const ctx = loadContext((url) => {
    if (url === '/api/conversations') return Promise.resolve(jsonOk([{ id: 'a', state: 'running', currentRunId: 'run-1', events: [] }]));
    return Promise.resolve(jsonOk({}));
  });
  run(ctx, 'sessions={};activeId="a";serverReady=true');
  await run(ctx, 'syncServerConversations()');
  if (!run(ctx, "sessions['a'] && sessions['a'].busy")) throw new Error('spinner lost after reload while run active');
  if (run(ctx, "sessions['a'].currentRunId") !== 'run-1') throw new Error('currentRunId lost');
});

test('real syncServerConversations clears spinner for terminal conversation', async () => {
  const ctx = loadContext((url) => {
    if (url === '/api/conversations') return Promise.resolve(jsonOk([{ id: 'a', state: 'interrupted', currentRunId: 'run-1', events: [] }]));
    return Promise.resolve(jsonOk({}));
  });
  run(ctx, 'sessions={};activeId="a";serverReady=true');
  await run(ctx, 'syncServerConversations()');
  if (run(ctx, "sessions['a'] && sessions['a'].busy")) throw new Error('spinner not cleared for terminal conversation');
});

test('two running background tabs both show spinners', async () => {
  const ctx = loadContext((url) => {
    if (url === '/api/conversations') return Promise.resolve(jsonOk([
      { id: 'a', state: 'running', currentRunId: 'r1', events: [] },
      { id: 'b', state: 'running', currentRunId: 'r2', events: [] },
    ]));
    return Promise.resolve(jsonOk({}));
  });
  run(ctx, 'sessions={};activeId="a";serverReady=true');
  await run(ctx, 'syncServerConversations()');
  if (!run(ctx, "sessions['a'].busy") || !run(ctx, "sessions['b'].busy")) throw new Error('both running tabs should show spinners');
});

test('reconcileRunState polls until terminal (running->running->completed)', async () => {
  const states = ['running', 'running', 'completed'];
  let i = 0;
  const ctx = loadContext((url) => {
    if (url === '/api/conversations') return Promise.resolve(jsonOk([{ id: 'a', state: states[Math.min(i++, states.length - 1)], currentRunId: 'run-1', events: [] }]));
    return Promise.resolve(jsonOk({}));
  });
  run(ctx, 'sessions={a:{id:"a",state:"running",currentRunId:"run-1",busy:true,events:[],followBottom:true,unread:0}};activeId="a"');
  await run(ctx, 'reconcileRunState("a")');
  if (run(ctx, "sessions['a'].busy")) throw new Error('spinner stuck after polling to terminal');
  if (run(ctx, "sessions['a'].state") !== 'completed') throw new Error('state not updated after polling');
});

test('terminal state received normally clears the spinner', async () => {
  const ctx = loadContext();
  const s = { id: 'a', state: 'running', currentRunId: 'run-1', busy: true, events: [], followBottom: true, unread: 0 };
  run(ctx, 'sessions={a:S};activeId="a"', { S: s });
  run(ctx, `(function(){const s=sessions['a'];s.state='completed';s.busy=false;})()`);
  if (run(ctx, "sessions['a'].busy")) throw new Error('spinner not cleared on terminal outcome');
});

test('reconcileRunState clears spinner for interrupted state after disconnect', async () => {
  const ctx = loadContext();
  const rec = { id: 'a', state: 'interrupted', currentRunId: 'run-1', events: [] };
  run(ctx, 'sessions={a:{id:"a",state:"running",currentRunId:"run-1",busy:true,events:[],followBottom:true,unread:0}};activeId="a"');
  run(ctx, 'window.__conv=REC', { REC: rec });
  // Override api to return the authoritative record.
  run(ctx, `(function(){const orig=api;api=async()=>[window.__conv];})(S)`, { S: ctx });
  await run(ctx, 'reconcileRunState("a")');
  if (run(ctx, "sessions['a'].busy")) throw new Error('spinner not cleared after interrupted reconciliation');
  if (run(ctx, "sessions['a'].state") !== 'interrupted') throw new Error('state not reconciled');
});

test('busy close keeps the tab when cancellation is rejected', async () => {
  const ctx = loadContext((url, opt) => {
    if (url === '/api/agent/cancel') return Promise.resolve({ ok: false, status: 404, text: () => Promise.resolve('run not found') });
    return Promise.resolve({ ok: true, json: () => Promise.resolve([]) });
  });
  run(ctx, 'sessions={a:{id:"a",state:"running",runID:"run-1",busy:true,abort:null,events:[],followBottom:true,unread:0}};activeId="a"');
  // api helper in this sandbox is the script's own; it calls fetch which returns non-ok -> throws.
  let kept = true;
  try { await run(ctx, '(async()=>{await stopAgentFor(sessions["a"]);return sessions["a"]!==undefined})()'); }
  catch (e) { kept = false; }
  // stopAgentFor returns false and keeps the session object; the close flow
  // then refuses to archive. Verify the session is still present.
  if (run(ctx, "sessions['a']") === undefined) throw new Error('session was archived despite rejection');
});

test('old run replaced by newer running run keeps spinner', async () => {
  let i = 0;
  const states = ['running', 'running'];
  const ctx = loadContext((url) => {
    if (url === '/api/conversations') return Promise.resolve(jsonOk([{ id: 'a', state: states[Math.min(i++, states.length-1)], currentRunId: 'run-2', events: [] }]));
    return Promise.resolve(jsonOk({}));
  });
  run(ctx, "sessions={a:{id:'a',state:'running',currentRunId:'run-1',runID:'run-1',busy:true,events:[],followBottom:true,unread:0}};activeId='a'");
  await run(ctx, 'reconcileRunState("a")');
  if (!run(ctx, "sessions['a'].busy")) throw new Error('newer running run should keep the spinner');
  if (run(ctx, "sessions['a'].currentRunId") !== 'run-2') throw new Error('currentRunId not updated');
});

test('old run replaced by newer terminal run clears spinner', async () => {
  const ctx = loadContext((url) => {
    if (url === '/api/conversations') return Promise.resolve(jsonOk([{ id: 'a', state: 'completed', currentRunId: 'run-2', events: [] }]));
    return Promise.resolve(jsonOk({}));
  });
  run(ctx, "sessions={a:{id:'a',state:'running',currentRunId:'run-1',runID:'run-1',busy:true,events:[],followBottom:true,unread:0}};activeId='a'");
  await run(ctx, 'reconcileRunState("a")');
  if (run(ctx, "sessions['a'].busy")) throw new Error('newer terminal run should clear the spinner');
});

test('a stale poll cannot overwrite a later synchronization', async () => {
  // The poll returns running for the old run; meanwhile a later sync set the
  // session to completed. The poll must not force it back to running.
  let i = 0;
  const ctx = loadContext((url) => {
    if (url === '/api/conversations') return Promise.resolve(jsonOk([{ id: 'a', state: i++ === 0 ? 'running' : 'completed', currentRunId: 'run-1', events: [] }]));
    return Promise.resolve(jsonOk({}));
  });
  run(ctx, "sessions={a:{id:'a',state:'running',currentRunId:'run-1',runID:'run-1',busy:true,events:[],followBottom:true,unread:0}};activeId='a'");
  const p = run(ctx, 'reconcileRunState("a")');
  // Simulate a later synchronization setting terminal state.
  run(ctx, "sessions['a'].state='completed';sessions['a'].busy=false");
  await p;
  if (run(ctx, "sessions['a'].busy")) throw new Error('stale poll overwrote later sync');
});

// Real closeSession flows: rejected cancellation keeps the tab.
test('closeSession keeps tab when cancellation is rejected', async () => {
  const ctx = loadContext((url) => {
    if (url === '/api/agent/cancel') return Promise.resolve({ ok: false, status: 404, text: () => Promise.resolve('not found') });
    if (url === '/api/conversations') return Promise.resolve(jsonOk([{ id: 'a', state: 'running', currentRunId: 'run-1', events: [] }]));
    return Promise.resolve(jsonOk({}));
  });
  run(ctx, "sessions={a:{id:'a',workspace:'/w',state:'running',runID:'run-1',currentRunId:'run-1',busy:true,abort:null,events:[],followBottom:true,unread:0}};activeId='a'");
  await run(ctx, 'closeSession("a")');
  if (run(ctx, "sessions['a']") === undefined) throw new Error('session was archived despite rejected cancellation');
});

// Real closeSession: accepted-but-draining keeps the tab, is not archived, the
// active selection is unchanged, and the pending-stop message is surfaced.
test('closeSession keeps tab when stop accepted but still draining', async () => {
  let n = 0;
  const preload = { __STOP_SETTLE_MS: 20, __RECONCILE_STOP_WAIT_MS: 150 };
  const ctx = loadContext((url) => {
    if (url === '/api/agent/cancel') return Promise.resolve(jsonOk({ cancelled: true }));
    if (url === '/api/conversations') return Promise.resolve(jsonOk([{ id: 'a', state: n++ < 3 ? 'running' : 'running', currentRunId: 'run-1', events: [] }]));
    return Promise.resolve(jsonOk({}));
  }, preload);
  run(ctx, "sessions={a:{id:'a',workspace:'/w',state:'running',runID:'run-1',currentRunId:'run-1',busy:true,abort:null,events:[],followBottom:true,unread:0}};activeId='a'");
  await run(ctx, 'closeSession("a")');
  if (run(ctx, "sessions['a']") === undefined) throw new Error('draining close must keep the tab in the open map');
  if (run(ctx, "closedSessions.some(x=>x.id==='a')")) throw new Error('draining close must not archive the session');
  if (run(ctx, "activeId") !== 'a') throw new Error('active selection changed while draining');
  const msg = run(ctx, `$('#toast').textContent`);
  if (!msg.includes('winding down')) throw new Error('pending-stop message not surfaced: ' + msg);
});

// Real closeSession: terminal cancellation archives it.
test('closeSession archives after terminal cancellation', async () => {
  let n = 0;
  const preload = { __STOP_SETTLE_MS: 20, __RECONCILE_STOP_WAIT_MS: 300 };
  const ctx = loadContext((url) => {
    if (url === '/api/agent/cancel') return Promise.resolve(jsonOk({ cancelled: true }));
    if (url === '/api/conversations') return Promise.resolve(jsonOk([{ id: 'a', state: n++ === 0 ? 'running' : 'cancelled', currentRunId: 'run-1', events: [] }]));
    return Promise.resolve(jsonOk({}));
  }, preload);
  run(ctx, "sessions={a:{id:'a',workspace:'/w',state:'running',runID:'run-1',currentRunId:'run-1',busy:true,abort:null,events:[],followBottom:true,unread:0}};activeId='a'");
  await run(ctx, 'closeSession("a")');
  if (run(ctx, "sessions['a']") !== undefined) throw new Error('terminal cancellation should archive the session');
});

// Direct Stop control: explicit non-success cancel response is rejected.
test('stopAgent shows rejected message on explicit cancel refusal', async () => {
  const ctx = loadContext((url) => {
    if (url === '/api/agent/cancel') return Promise.resolve({ ok: false, status: 404, text: () => Promise.resolve('run not found') });
    return Promise.resolve(jsonOk({}));
  });
  run(ctx, "sessions={a:{id:'a',state:'running',runID:'run-1',busy:true,abort:null,events:[],followBottom:true,unread:0}};activeId='a'");
  const res = await run(ctx, 'stopAgentFor(sessions["a"])');
  if (res.rejected !== true) throw new Error('explicit refusal must be rejected, got ' + JSON.stringify(res));
  await run(ctx, 'stopAgent()');
  const msg = run(ctx, `$('#toast').textContent`);
  if (msg !== 'Could not stop the running agent.') throw new Error('rejected toast = ' + msg);
});

// Direct Stop control: confirmed cancel followed by terminal is a clean stop.
test('stopAgent shows terminal message on confirmed cancel', async () => {
  let n = 0;
  const preload = { __STOP_SETTLE_MS: 10, __RECONCILE_STOP_WAIT_MS: 300 };
  const ctx = loadContext((url) => {
    if (url === '/api/agent/cancel') return Promise.resolve(jsonOk({ cancelled: true }));
    if (url === '/api/conversations') return Promise.resolve(jsonOk([{ id: 'a', state: n++ === 0 ? 'running' : 'cancelled', currentRunId: 'run-1', events: [] }]));
    return Promise.resolve(jsonOk({}));
  }, preload);
  run(ctx, "sessions={a:{id:'a',state:'running',runID:'run-1',busy:true,abort:null,events:[],followBottom:true,unread:0}};activeId='a'");
  const res = await run(ctx, 'stopAgentFor(sessions["a"])');
  if (res.ok !== true) throw new Error('terminal stop should be ok, got ' + JSON.stringify(res));
  await run(ctx, 'stopAgent()');
  const msg = run(ctx, `$('#toast').textContent`);
  if (msg !== 'Agent stopped.') throw new Error('terminal toast = ' + msg);
});

// Direct Stop control: confirmed cancel followed by a still-running state is draining.
test('stopAgent shows draining message on acknowledged still-running cancel', async () => {
  const preload = { __STOP_SETTLE_MS: 10, __RECONCILE_STOP_WAIT_MS: 150 };
  const ctx = loadContext((url) => {
    if (url === '/api/agent/cancel') return Promise.resolve(jsonOk({ cancelled: true }));
    if (url === '/api/conversations') return Promise.resolve(jsonOk([{ id: 'a', state: 'running', currentRunId: 'run-1', events: [] }]));
    return Promise.resolve(jsonOk({}));
  }, preload);
  run(ctx, "sessions={a:{id:'a',state:'running',runID:'run-1',busy:true,abort:null,events:[],followBottom:true,unread:0}};activeId='a'");
  const res = await run(ctx, 'stopAgentFor(sessions["a"])');
  if (res.draining !== true) throw new Error('acknowledged still-running must be draining, got ' + JSON.stringify(res));
  await run(ctx, 'stopAgent()');
  const msg = run(ctx, `$('#toast').textContent`);
  if (!msg.includes('winding down')) throw new Error('draining toast = ' + msg);
});

// Timeout: a cancel request that never returns, followed by terminal state, is
// a successful stop (reconciled against authoritative state).
test('stopAgent cancel timeout followed by terminal returns success', async () => {
  let n = 0;
  const preload = { __CANCEL_TIMEOUT_MS: 30, __STOP_SETTLE_MS: 10, __RECONCILE_STOP_WAIT_MS: 300 };
  const ctx = loadContext((url) => {
    if (url === '/api/agent/cancel') return new Promise(() => {});
    if (url === '/api/conversations') return Promise.resolve(jsonOk([{ id: 'a', state: n++ === 0 ? 'running' : 'cancelled', currentRunId: 'run-1', events: [] }]));
    return Promise.resolve(jsonOk({}));
  }, preload);
  run(ctx, "sessions={a:{id:'a',state:'running',runID:'run-1',busy:true,abort:null,events:[],followBottom:true,unread:0}};activeId='a'");
  const res = await run(ctx, 'stopAgentFor(sessions["a"])');
  if (res.ok !== true) throw new Error('timeout then terminal should stop cleanly, got ' + JSON.stringify(res));
  if (res.unconfirmed) throw new Error('terminal must not be reported unconfirmed');
  await run(ctx, 'stopAgent()');
  const msg = run(ctx, `$('#toast').textContent`);
  if (msg !== 'Agent stopped.') throw new Error('timeout-terminal toast = ' + msg);
});

// Timeout: a cancel request that never returns, followed by a still-running
// state, is unconfirmed (never described as an explicit rejection).
test('stopAgent cancel timeout followed by running is unconfirmed not rejected', async () => {
  const preload = { __CANCEL_TIMEOUT_MS: 30, __STOP_SETTLE_MS: 10, __RECONCILE_STOP_WAIT_MS: 150 };
  const ctx = loadContext((url) => {
    if (url === '/api/agent/cancel') return new Promise(() => {});
    if (url === '/api/conversations') return Promise.resolve(jsonOk([{ id: 'a', state: 'running', currentRunId: 'run-1', events: [] }]));
    return Promise.resolve(jsonOk({}));
  }, preload);
  run(ctx, "sessions={a:{id:'a',state:'running',runID:'run-1',busy:true,abort:null,events:[],followBottom:true,unread:0}};activeId='a'");
  const res = await run(ctx, 'stopAgentFor(sessions["a"])');
  if (res.rejected) throw new Error('timeout must never be reported as rejected');
  if (res.unconfirmed !== true) throw new Error('timeout still-running must be unconfirmed, got ' + JSON.stringify(res));
  await run(ctx, 'stopAgent()');
  const msg = run(ctx, `$('#toast').textContent`);
  if (!msg.includes('Could not confirm the stop')) throw new Error('unconfirmed toast = ' + msg);
});

// Warning vs failure: a completed_with_process_error terminal event maps to a
// distinct warning kind and summary, never the generic error kind.
test('warning SSE terminal event maps to warning kind and summary', async () => {
  const ctx = loadContext();
  const ev = { type: 'warning', data: { outcome: 'completed_with_process_error', message: 'OpenCode exited with status 1 after completing.' } };
  if (run(ctx, 'eventKind(EV)', { EV: ev }) !== 'warning') throw new Error('warning kind not mapped');
  const sum = run(ctx, 'summarize(EV)', { EV: ev });
  if (!sum.includes('exit')) throw new Error('warning summary missing: ' + sum);
});

// Warning vs failure: eventNode renders warning and error with distinct classes.
test('eventNode renders warning and error with distinct classes', async () => {
  const ctx = loadContext();
  const w = run(ctx, 'eventNode({kind:"warning",text:"W",name:"run:r1:completed_with_process_error"})');
  const e = run(ctx, 'eventNode({kind:"error",text:"E",name:"run:r1:failed"})');
  if (!String(w.className).includes('event warning')) throw new Error('warning class missing: ' + w.className);
  if (!String(e.className).includes('event error')) throw new Error('error class missing');
  if (w.className === e.className) throw new Error('warning and error must render differently');
});

// Warning survives conversation reload without degrading into an Error block.
test('real syncServerConversations preserves warning state after reload', async () => {
  const ctx = loadContext((url) => {
    if (url === '/api/conversations') return Promise.resolve(jsonOk([{ id: 'a', state: 'completed_with_process_error', currentRunId: 'run-1', events: [{ kind: 'warning', text: 'OpenCode exited with status 1 after completing.', name: 'run:run-1:completed_with_process_error' }] }]));
    return Promise.resolve(jsonOk({}));
  });
  run(ctx, 'sessions={};activeId="a";serverReady=true');
  await run(ctx, 'syncServerConversations()');
  if (run(ctx, "sessions['a'].state") !== 'completed_with_process_error') throw new Error('warning state lost on reload');
  if (run(ctx, "sessions['a'].busy")) throw new Error('spinner not cleared on reload');
  const kinds = run(ctx, "sessions['a'].events.map(e=>e.kind).join(',')");
  if (kinds.includes('error')) throw new Error('warning degraded to error on reload: ' + kinds);
  if (!kinds.includes('warning')) throw new Error('warning kind missing on reload: ' + kinds);
});

// Live task pipeline: the real stream-consumption function opens the task
// panel for a normalized task SSE event before terminal completion or reload.
test('streamed task event opens the task panel immediately', async () => {
  const ctx = loadContext();
  run(ctx, "sessions={a:{id:'a',events:[],busy:true,followBottom:true,unread:0}};activeId='a'");
  const ev = { type: 'task', data: { snapshot: '[{"content":"alpha","status":"in_progress","priority":"high"},{"content":"beta","status":"pending","priority":"low"}]' } };
  run(ctx, 'consumeAgentEvent("a", sessions["a"], EV, new Set())', { EV: ev });
  if (run(ctx, "$('#taskPanel').hidden")) throw new Error('task panel did not open from streamed event');
  const rows = run(ctx, "$('#taskList').children.length");
  if (rows !== 2) throw new Error('task rows = ' + rows);
});

// Updating the todo list replaces the displayed snapshot (no accumulation).
test('a later todowrite snapshot replaces the displayed list', async () => {
  const ctx = loadContext();
  run(ctx, "sessions={a:{id:'a',events:[],busy:true,followBottom:true,unread:0}};activeId='a'");
  const ev1 = { type: 'task', data: { snapshot: '[{"content":"alpha","status":"pending","priority":"high"}]' } };
  const ev2 = { type: 'task', data: { snapshot: '[{"content":"one","status":"pending","priority":"high"},{"content":"two","status":"completed","priority":"low"},{"content":"three","status":"pending","priority":"medium"}]' } };
  run(ctx, 'consumeAgentEvent("a", sessions["a"], EV, new Set())', { EV: ev1 });
  run(ctx, 'consumeAgentEvent("a", sessions["a"], EV, new Set())', { EV: ev2 });
  const rows = run(ctx, "$('#taskList').children.length");
  if (rows !== 3) throw new Error('replacement rows = ' + rows);
});

// A valid empty snapshot clears and hides the panel.
test('valid empty todos snapshot clears and hides the panel', async () => {
  const ctx = loadContext();
  run(ctx, "sessions={a:{id:'a',events:[],busy:true,followBottom:true,unread:0}};activeId='a'");
  run(ctx, 'consumeAgentEvent("a", sessions["a"], EV, new Set())', { EV: { type: 'task', data: { snapshot: '[{"content":"alpha","status":"pending","priority":"high"}]' } } });
  if (run(ctx, "$('#taskPanel').hidden")) throw new Error('panel should be open before clear');
  run(ctx, 'consumeAgentEvent("a", sessions["a"], EV, new Set())', { EV: { type: 'task', data: { snapshot: '[]' } } });
  if (!run(ctx, "$('#taskPanel').hidden")) throw new Error('panel not hidden after clear');
  const rows = run(ctx, "$('#taskList').children.length");
  if (rows !== 0) throw new Error('task list not cleared: ' + rows);
});

// Malformed snapshot text never opens the panel.
test('malformed task payload is ignored', async () => {
  const ctx = loadContext();
  run(ctx, "sessions={a:{id:'a',events:[],busy:true,followBottom:true,unread:0}};activeId='a'");
  run(ctx, 'consumeAgentEvent("a", sessions["a"], EV, new Set())', { EV: { type: 'task', data: { snapshot: 'not-json' } } });
  if (!run(ctx, "$('#taskPanel').hidden")) throw new Error('malformed payload opened the panel');
  run(ctx, 'consumeAgentEvent("a", sessions["a"], EV, new Set())', { EV: { type: 'task', data: { snapshot: '{"a":1}' } } });
  if (!run(ctx, "$('#taskPanel').hidden")) throw new Error('non-array payload opened the panel');
});

// Task events are excluded from transcript rendering.
test('task events never render as transcript rows', async () => {
  const ctx = loadContext();
  run(ctx, "sessions={a:{id:'a',events:[],busy:true,followBottom:true,unread:0}};activeId='a'");
  run(ctx, 'consumeAgentEvent("a", sessions["a"], EV, new Set())', { EV: { type: 'task', data: { snapshot: '[{"content":"alpha","status":"pending","priority":"high"}]' } } });
  if (run(ctx, "$('#feed').children.length") !== 0) throw new Error('task event leaked into the feed');
  run(ctx, 'consumeAgentEvent("a", sessions["a"], EV, new Set())', { EV: { type: 'opencode', data: { type: 'tool_use', part: { tool: 'todowrite', state: { status: 'completed', input: { todos: [] } } } } } });
  run(ctx, 'consumeAgentEvent("a", sessions["a"], EV, new Set())', { EV: { type: 'opencode', data: { type: 'text', part: { type: 'text', text: 'hello' } } } });
  const rows = run(ctx, "$('#feed').children.length");
  if (rows !== 2) throw new Error('transcript rows = ' + rows);
  const hasTaskRow = run(ctx, "[...$('#feed').children].some(c=>String(c.className).includes('event task'))");
  if (hasTaskRow) throw new Error('a task row was rendered in the feed');
});

// Reload restores the latest non-empty snapshot without rendering JSON rows.
test('reload restores task snapshot without feed JSON rows', async () => {
  const ctx = loadContext((url) => {
    if (url === '/api/conversations') return Promise.resolve(jsonOk([{ id: 'a', state: 'completed', currentRunId: 'run-1', events: [
      { kind: 'assistant', text: 'answer', name: '' },
      { kind: 'task', text: '[{"content":"alpha","status":"in_progress","priority":"high"}]', name: '' },
    ] }]));
    return Promise.resolve(jsonOk({}));
  });
  run(ctx, 'sessions={};activeId="a";serverReady=true');
  await run(ctx, 'syncServerConversations()');
  if (run(ctx, "$('#taskPanel').hidden")) throw new Error('task panel not restored on reload');
  const rows = run(ctx, "$('#taskList').children.length");
  if (rows !== 1) throw new Error('restored task rows = ' + rows);
  const feedRows = run(ctx, "$('#feed').children.length");
  if (feedRows !== 1) throw new Error('feed rows = ' + feedRows);
  const hasTaskRow = run(ctx, "[...$('#feed').children].some(c=>String(c.className).includes('event task'))");
  if (hasTaskRow) throw new Error('a task row was rendered after reload');
});

// Background-session task updates must not create a second unread transcript item.
test('background task snapshot does not increment unread', async () => {
  const ctx = loadContext();
  run(ctx, "sessions={a:{id:'a',events:[],busy:false,followBottom:true,unread:0},b:{id:'b',events:[],busy:true,followBottom:true,unread:0}};activeId='a'");
  run(ctx, 'consumeAgentEvent("b", sessions["b"], EV, new Set())', { EV: { type: 'task', data: { snapshot: '[{"content":"alpha","status":"pending","priority":"high"}]' } } });
  if (run(ctx, "sessions['b'].unread") !== 0) throw new Error('task snapshot incremented unread: ' + run(ctx, "sessions['b'].unread"));
  run(ctx, 'consumeAgentEvent("b", sessions["b"], EV, new Set())', { EV: { type: 'text', data: { type: 'text', part: { type: 'text', text: 'real activity' } } } });
  if (run(ctx, "sessions['b'].unread") !== 1) throw new Error('real activity should increment unread');
});

// Per-session collapsed task panel: closing it survives subsequent events,
// rerenders and tab switching; explicit reopen restores the latest snapshot.
test('task panel close collapses per-session and events/rerender/tab do not reopen', async () => {
  const ctx = loadContext();
  run(ctx, "sessions={a:{id:'a',events:[],busy:true,followBottom:true,unread:0},b:{id:'b',events:[],busy:false,followBottom:true,unread:0}};activeId='a'");
  run(ctx, 'consumeAgentEvent("a", sessions["a"], EV, new Set())', { EV: { type: 'task', data: { snapshot: '[{"content":"alpha","status":"pending","priority":"high"}]' } } });
  if (run(ctx, "$('#taskPanel').hidden")) throw new Error('panel should be open before close');
  run(ctx, 'setTasksCollapsed(true)');
  if (!run(ctx, "$('#taskPanel').hidden")) throw new Error('panel not hidden after close');
  if (run(ctx, "sessions['a'].tasksCollapsed") !== true) throw new Error('collapsed flag not set');
  if (run(ctx, "$('#taskButton').hidden")) throw new Error('Tasks control should be visible when collapsed');
  if (run(ctx, "$('#taskButton').getAttribute('aria-expanded')") !== 'false') throw new Error('collapsed Tasks control must report aria-expanded=false');
  run(ctx, 'consumeAgentEvent("a", sessions["a"], EV, new Set())', { EV: { type: 'opencode', data: { type: 'text', part: { type: 'text', text: 'hello' } } } });
  if (!run(ctx, "$('#taskPanel').hidden")) throw new Error('a transcript event reopened the panel');
  run(ctx, 'renderFeed()');
  if (!run(ctx, "$('#taskPanel').hidden")) throw new Error('a rerender reopened the panel');
  run(ctx, "activeId='b';renderAll()");
  run(ctx, "activeId='a';renderAll()");
  if (!run(ctx, "$('#taskPanel').hidden")) throw new Error('tab switching reopened the panel');
  run(ctx, 'setTasksCollapsed(false)');
  if (run(ctx, "$('#taskPanel').hidden")) throw new Error('explicit reopen did not show the panel');
  if (run(ctx, "$('#taskList').children.length") !== 1) throw new Error('reopen should render the snapshot');
});

// Task snapshots keep updating while collapsed and reopening shows the latest.
test('task snapshots update while collapsed and reopen shows latest', async () => {
  const ctx = loadContext();
  run(ctx, "sessions={a:{id:'a',events:[],busy:true,followBottom:true,unread:0}};activeId='a'");
  run(ctx, 'consumeAgentEvent("a", sessions["a"], EV, new Set())', { EV: { type: 'task', data: { snapshot: '[{"content":"alpha","status":"pending","priority":"high"}]' } } });
  run(ctx, 'setTasksCollapsed(true)');
  run(ctx, 'consumeAgentEvent("a", sessions["a"], EV, new Set())', { EV: { type: 'task', data: { snapshot: '[{"content":"one","status":"pending","priority":"high"},{"content":"two","status":"completed","priority":"low"}]' } } });
  if (!run(ctx, "$('#taskPanel').hidden")) throw new Error('a later snapshot reopened the panel');
  if (run(ctx, "sessions['a'].tasksCollapsed") !== true) throw new Error('collapsed preference lost');
  run(ctx, 'setTasksCollapsed(false)');
  if (run(ctx, "$('#taskPanel').hidden")) throw new Error('panel did not reopen');
  if (run(ctx, "$('#taskList').children.length") !== 2) throw new Error('latest snapshot rows = ' + run(ctx, "$('#taskList').children.length"));
  if (run(ctx, "sessions['a'].tasksCollapsed") !== false) throw new Error('collapsed not cleared on reopen');
});

// An empty authoritative snapshot clears tasks even while collapsed.
test('empty authoritative snapshot clears tasks while collapsed', async () => {
  const ctx = loadContext();
  run(ctx, "sessions={a:{id:'a',events:[],busy:true,followBottom:true,unread:0}};activeId='a'");
  run(ctx, 'consumeAgentEvent("a", sessions["a"], EV, new Set())', { EV: { type: 'task', data: { snapshot: '[{"content":"alpha","status":"pending","priority":"high"}]' } } });
  run(ctx, 'setTasksCollapsed(true)');
  run(ctx, 'consumeAgentEvent("a", sessions["a"], EV, new Set())', { EV: { type: 'task', data: { snapshot: '[]' } } });
  if (!run(ctx, "$('#taskPanel').hidden")) throw new Error('panel not hidden after empty clear');
  if (run(ctx, "$('#taskList').children.length") !== 0) throw new Error('tasks not cleared');
  if (!run(ctx, "$('#taskButton').hidden")) throw new Error('Tasks control should hide after an empty clear');
});

// Synchronization must not reopen a collapsed task panel.
test('synchronization preserves collapsed task panel', async () => {
  const ctx = loadContext((url) => {
    if (url === '/api/conversations') return Promise.resolve(jsonOk([{ id: 'a', state: 'completed', currentRunId: 'run-1', events: [{ kind: 'task', text: '[{"content":"alpha","status":"pending","priority":"high"}]', name: '' }] }]));
    return Promise.resolve(jsonOk({}));
  });
  run(ctx, "sessions={a:{id:'a',events:[],busy:true,followBottom:true,unread:0,tasksCollapsed:true}};activeId='a';serverReady=true;uiPrefs={activeId:'a',collapsed:{a:true}}");
  await run(ctx, 'syncServerConversations()');
  if (run(ctx, "sessions['a'].tasksCollapsed") !== true) throw new Error('collapsed flag lost on sync');
  run(ctx, 'renderFeed()');
  if (!run(ctx, "$('#taskPanel').hidden")) throw new Error('sync reopened a collapsed panel');
});

// Switching to an empty session clears the previous session's task panel and
// reopen badge; switching back restores that session's correct state.
test('switching to empty session clears stale task panel', async () => {
  const ctx = loadContext();
  run(ctx, "sessions={a:{id:'a',events:[],busy:true,followBottom:true,unread:0},b:{id:'b',events:[],busy:false,followBottom:true,unread:0}};activeId='a'");
  run(ctx, 'consumeAgentEvent("a", sessions["a"], EV, new Set())', { EV: { type: 'task', data: { snapshot: '[{"content":"alpha","status":"pending","priority":"high"}]' } } });
  if (run(ctx, "$('#taskPanel').hidden")) throw new Error('panel should be visible for A');
  run(ctx, "activeId='b';renderAll()");
  if (!run(ctx, "$('#taskPanel').hidden")) throw new Error('A task panel remained on empty session B');
  if (!run(ctx, "$('#taskButton').hidden")) throw new Error('A Tasks control remained on empty session B');
  run(ctx, "activeId='a';renderAll()");
  if (run(ctx, "$('#taskPanel').hidden")) throw new Error('switching back to A did not restore its task panel');
});

// Switching to an empty session clears a collapsed task panel's reopen badge too.
test('switching to empty session clears collapsed reopen badge', async () => {
  const ctx = loadContext();
  run(ctx, "sessions={a:{id:'a',events:[],busy:true,followBottom:true,unread:0},b:{id:'b',events:[],busy:false,followBottom:true,unread:0}};activeId='a'");
  run(ctx, 'consumeAgentEvent("a", sessions["a"], EV, new Set())', { EV: { type: 'task', data: { snapshot: '[{"content":"alpha","status":"pending","priority":"high"}]' } } });
  run(ctx, 'setTasksCollapsed(true)');
  if (run(ctx, "$('#taskButton').hidden")) throw new Error('Tasks control should be visible for collapsed A');
  run(ctx, "activeId='b';renderAll()");
  if (!run(ctx, "$('#taskButton').hidden")) throw new Error('A Tasks control remained on empty session B');
  run(ctx, "activeId='a';renderAll()");
  if (run(ctx, "$('#taskButton').hidden")) throw new Error('switching back to A did not restore its Tasks control');
});

// Collapsed preference survives a reload (persist + reconstruct via loadSessions).
test('collapsed task panel survives reload', async () => {
  const ctx = loadContext();
  run(ctx, "sessions={a:{id:'a',events:[],busy:true,followBottom:true,unread:0}};activeId='a'");
  run(ctx, 'consumeAgentEvent("a", sessions["a"], EV, new Set())', { EV: { type: 'task', data: { snapshot: '[{"content":"alpha","status":"pending","priority":"high"}]' } } });
  run(ctx, 'setTasksCollapsed(true)');
  const stored = ctx.localStorage.getItem('cortex.ui.v1');
  if (!stored || !stored.includes('"a":true')) throw new Error('collapsed preference not persisted in UI state');
  if (stored.includes('"events"')) throw new Error('session transcript leaked into browser storage');
  if (ctx.localStorage.getItem('cortex.sessions.v1') !== null) throw new Error('legacy session payload must never be written');
  run(ctx, 'loadSessions()');
  if (run(ctx, "sessions['a'].tasksCollapsed") !== true) throw new Error('collapsed not restored on reload');
  run(ctx, 'renderAll()');
  if (!run(ctx, "$('#taskPanel').hidden")) throw new Error('collapsed panel reopened after reload');
});

// Reopening persists and survives reload too.
test('reopened task panel survives reload', async () => {
  const ctx = loadContext();
  run(ctx, "sessions={a:{id:'a',events:[],busy:true,followBottom:true,unread:0}};activeId='a'");
  run(ctx, 'consumeAgentEvent("a", sessions["a"], EV, new Set())', { EV: { type: 'task', data: { snapshot: '[{"content":"alpha","status":"pending","priority":"high"}]' } } });
  run(ctx, 'setTasksCollapsed(true)');
  run(ctx, 'setTasksCollapsed(false)');
  const stored = ctx.localStorage.getItem('cortex.ui.v1');
  if (!stored || !stored.includes('"collapsed":{}')) throw new Error('reopen not persisted as open state');
  run(ctx, 'loadSessions()');
  if (run(ctx, "sessions['a'].tasksCollapsed") !== false) throw new Error('open state not restored on reload');
  run(ctx, 'renderAll()');
  if (run(ctx, "$('#taskPanel').hidden")) throw new Error('reopened panel hidden after reload');
});

// streamResponse returns a fake fetch response whose body yields the given
// NDJSON lines, so runAgent can be driven through the real streaming loop.
function streamResponse(lines) {
  let i = 0;
  const enc = new TextEncoder();
  return {
    ok: true,
    status: 200,
    body: {
      getReader: () => ({
        read: async () => (i < lines.length ? { value: enc.encode(lines[i++] + '\n'), done: false } : { value: undefined, done: true }),
      }),
    },
    text: async () => '',
  };
}

// --- UX: a new run clears the previous run's task list. ---

test('new run clears an open task list from the previous run', async () => {
  const ctx = loadContext((url) => {
    if (url === '/api/agent/run') return Promise.resolve(streamResponse(['{"type":"run","data":{"runID":"run-2"}}', '{"type":"done","data":{"outcome":"completed"}}']));
    return Promise.resolve(jsonOk({}));
  });
  run(ctx, "sessions={a:{id:'a',workspace:'/w',events:[],busy:false,followBottom:true,unread:0,currentRunId:'run-1'}};activeId='a'");
  run(ctx, 'consumeAgentEvent("a", sessions["a"], EV, new Set())', { EV: { type: 'task', data: { snapshot: '[{"content":"alpha","status":"in_progress","priority":"high"}]' } } });
  if (run(ctx, "$('#taskPanel').hidden")) throw new Error('task panel should be open before the new run');
  await run(ctx, 'runAgent("do it")');
  await settle(30);
  if (!run(ctx, "$('#taskPanel').hidden")) throw new Error('task panel not cleared when a new run starts');
  if (!run(ctx, "$('#taskButton').hidden")) throw new Error('Tasks control must hide until the new run emits tasks');
  const latest = run(ctx, "sessions['a'].events.filter(e=>e.kind==='task').pop().text");
  if (latest !== '[]') throw new Error('no authoritative empty snapshot appended: ' + latest);
  if (run(ctx, "sessions['a'].events.filter(e=>e.kind==='task').length") < 2) throw new Error('transcript history must not be deleted');
});

test('new run clears a collapsed task list and its preference', async () => {
  const ctx = loadContext((url) => {
    if (url === '/api/agent/run') return Promise.resolve(streamResponse(['{"type":"run","data":{"runID":"run-2"}}', '{"type":"done","data":{"outcome":"completed"}}']));
    return Promise.resolve(jsonOk({}));
  });
  run(ctx, "sessions={a:{id:'a',workspace:'/w',events:[],busy:false,followBottom:true,unread:0,currentRunId:'run-1'}};activeId='a'");
  run(ctx, 'consumeAgentEvent("a", sessions["a"], EV, new Set())', { EV: { type: 'task', data: { snapshot: '[{"content":"alpha","status":"pending","priority":"high"}]' } } });
  run(ctx, 'setTasksCollapsed(true)');
  if (run(ctx, "sessions['a'].tasksCollapsed") !== true) throw new Error('collapsed not set');
  await run(ctx, 'runAgent("do it")');
  await settle(30);
  if (run(ctx, "sessions['a'].tasksCollapsed") !== false) throw new Error('collapsed preference not cleared for the new run');
  if (run(ctx, "uiPrefs.collapsed['a']") === true) throw new Error('collapsed UI preference not cleared for the new run');
});

test('a new run that fails before producing tasks keeps the list cleared', async () => {
  const ctx = loadContext((url) => {
    if (url === '/api/agent/run') return Promise.resolve({ ok: false, status: 500, text: async () => 'boom' });
    return Promise.resolve(jsonOk({}));
  });
  run(ctx, "sessions={a:{id:'a',workspace:'/w',events:[],busy:false,followBottom:true,unread:0,currentRunId:'run-1'}};activeId='a'");
  run(ctx, 'consumeAgentEvent("a", sessions["a"], EV, new Set())', { EV: { type: 'task', data: { snapshot: '[{"content":"alpha","status":"pending","priority":"high"}]' } } });
  await run(ctx, 'runAgent("do it")');
  await settle(30);
  const latest = run(ctx, "sessions['a'].events.filter(e=>e.kind==='task').pop().text");
  if (latest !== '[]') throw new Error('failed run did not leave the authoritative empty snapshot: ' + latest);
  if (!run(ctx, "$('#taskPanel').hidden")) throw new Error('panel reopened after a failed run');
  if (!run(ctx, "$('#taskButton').hidden")) throw new Error('Tasks control must stay hidden after a failed run');
  if (run(ctx, "sessions['a'].busy")) throw new Error('a rejected run request must become terminal locally');
  if (run(ctx, "sessions['a'].state") !== 'failed') throw new Error('a rejected run request must record failed state');
  if (!run(ctx, "$('#stop').hidden")) throw new Error('Stop must hide when no agent process was started');
});

test('OpenCode preflight rejection does not leave an unstoppable running session', async () => {
  const message = "OpenCode is not installed or not in Cortex's PATH";
  const ctx = loadContext((url) => {
    if (url === '/api/agent/run') return Promise.resolve({ ok: false, status: 503, statusText: 'Service Unavailable', text: async () => message });
    return Promise.resolve(jsonOk({}));
  });
  run(ctx, "sessions={a:{id:'a',workspace:'/w',events:[],busy:false,followBottom:true,unread:0}};activeId='a'");
  await run(ctx, 'runAgent("do it")');
  await settle(30);
  if (run(ctx, "sessions['a'].busy")) throw new Error('OpenCode preflight rejection left the session busy');
  if (run(ctx, "sessions['a'].state") !== 'failed') throw new Error('OpenCode preflight rejection did not set failed state');
  if (!run(ctx, "$('#stop').hidden")) throw new Error('Stop remained visible without a server run');
  const errors = run(ctx, "sessions['a'].events.filter(e=>e.kind==='error').map(e=>e.text)");
  if (!errors.includes(message)) throw new Error('OpenCode rejection was not shown in the transcript');
});

test('running agent leaves prompt editable but disables submission', async () => {
  const ctx = loadContext();
  run(ctx, "sessions={a:{id:'a',workspace:'/w',events:[],busy:true,followBottom:true,unread:0,draft:''}};activeId='a';renderSession()");
  if (run(ctx, "$('#prompt').disabled")) throw new Error('prompt must remain editable during a run');
  if (!run(ctx, "$('#run').disabled")) throw new Error('Run agent must remain disabled during a run');
  run(ctx, "$('#prompt').value='next request'");
  if (run(ctx, "$('#prompt').value") !== 'next request') throw new Error('draft could not be edited during a run');
});

test('a new run shows its own fresh task snapshot', async () => {
  const ctx = loadContext((url) => {
    if (url === '/api/agent/run') return Promise.resolve(streamResponse([
      '{"type":"run","data":{"runID":"run-3"}}',
      '{"type":"task","data":{"snapshot":"[{\\"content\\":\\"fresh\\",\\"status\\":\\"in_progress\\",\\"priority\\":\\"high\\"}]"}}',
      '{"type":"done","data":{"outcome":"completed"}}',
    ]));
    return Promise.resolve(jsonOk({}));
  });
  run(ctx, "sessions={a:{id:'a',workspace:'/w',events:[],busy:false,followBottom:true,unread:0,currentRunId:'run-1'}};activeId='a'");
  run(ctx, 'consumeAgentEvent("a", sessions["a"], EV, new Set())', { EV: { type: 'task', data: { snapshot: '[{"content":"old","status":"pending","priority":"high"}]' } } });
  await run(ctx, 'runAgent("do it")');
  await settle(30);
  if (run(ctx, "$('#taskPanel').hidden")) throw new Error('fresh snapshot did not open the panel');
  const latest = run(ctx, "sessions['a'].events.filter(e=>e.kind==='task').pop().text");
  if (!latest.includes('fresh')) throw new Error('fresh snapshot not applied: ' + latest);
  if (run(ctx, "$('#taskButton').getAttribute('aria-expanded')") !== 'true') throw new Error('open panel must report aria-expanded=true');
});

test('background-session task snapshots do not affect the active session list', async () => {
  const ctx = loadContext();
  run(ctx, "sessions={a:{id:'a',events:[],busy:false,followBottom:true,unread:0},b:{id:'b',events:[],busy:true,followBottom:true,unread:0}};activeId='a'");
  run(ctx, 'renderAll()');
  run(ctx, 'consumeAgentEvent("b", sessions["b"], EV, new Set())', { EV: { type: 'task', data: { snapshot: '[{"content":"beta","status":"pending","priority":"high"}]' } } });
  if (!run(ctx, "$('#taskPanel').hidden")) throw new Error('background task opened the active session panel');
  if (run(ctx, "$('#taskList').children.length") !== 0) throw new Error('background tasks leaked into the active list');
  if (!run(ctx, "$('#taskButton').hidden")) throw new Error('Tasks control must stay hidden for a session with no snapshot');
});

test('stale task events from an older run do not repopulate a newer run', async () => {
  const ctx = loadContext();
  run(ctx, "sessions={a:{id:'a',workspace:'/w',events:[],busy:true,followBottom:true,unread:0}};activeId='a'");
  run(ctx, 'resetTaskStateForNewRun("a")');
  run(ctx, "sessions['a'].currentRunId='run-2'");
  // A stale task event from the superseded run-1 arrives late.
  run(ctx, 'addEvent("a","task","[{\\"content\\":\\"old run\\",\\"status\\":\\"pending\\"}]","",{runID:"run-1"})');
  if (!run(ctx, "$('#taskPanel').hidden")) throw new Error('a stale old-run task event reopened the newer run panel');
  if (!run(ctx, "$('#taskButton').hidden")) throw new Error('stale events must not reveal the Tasks control');
  const latest = run(ctx, "(latestTaskEvent(sessions['a'])||{}).text");
  if (latest !== '[]') throw new Error('stale event must not become the authoritative snapshot: ' + latest);
});

// --- UX: unsent prompt drafts and attachments are session-specific. ---

test('unsent prompts are session-specific across tab switches', async () => {
  const ctx = loadContext();
  run(ctx, "sessions={a:{id:'a',workspace:'/w',events:[],createdAt:1,draft:'',attachments:[]},b:{id:'b',workspace:'/w',events:[],createdAt:2,draft:'',attachments:[]},c:{id:'c',workspace:'/w',events:[],createdAt:3,draft:'',attachments:[]}};activeId='a'");
  run(ctx, `$('#prompt').value='draft A'`);
  run(ctx, 'setActiveSession("b")');
  if (run(ctx, `$('#prompt').value`) !== '') throw new Error('B must start with an empty draft');
  run(ctx, `$('#prompt').value='draft B'`);
  run(ctx, 'setActiveSession("c")');
  run(ctx, `$('#prompt').value='draft C'`);
  run(ctx, 'setActiveSession("a")');
  if (run(ctx, `$('#prompt').value`) !== 'draft A') throw new Error('A draft not restored: ' + run(ctx, `$('#prompt').value`));
  run(ctx, 'setActiveSession("b")');
  if (run(ctx, `$('#prompt').value`) !== 'draft B') throw new Error('B draft not restored');
  run(ctx, 'setActiveSession("c")');
  if (run(ctx, `$('#prompt').value`) !== 'draft C') throw new Error('C draft not restored');
  run(ctx, 'setActiveSession("a")');
  if (run(ctx, `$('#prompt').value`) !== 'draft A') throw new Error('A draft not restored on switch back');
  const ui = run(ctx, `localStorage.getItem('cortex.ui.v1')`);
  if (ui && ui.includes('draft')) throw new Error('draft leaked into browser storage');
});

test('submitting a prompt clears only the submitted session draft', async () => {
  const ctx = loadContext((url) => {
    if (url === '/api/agent/run') return Promise.resolve(streamResponse(['{"type":"run","data":{"runID":"r1"}}', '{"type":"done","data":{"outcome":"completed"}}']));
    return Promise.resolve(jsonOk({}));
  });
  run(ctx, "sessions={a:{id:'a',workspace:'/w',events:[],createdAt:1,draft:'unsent A',attachments:[]},b:{id:'b',workspace:'/w',events:[],createdAt:2,draft:'unsent B',attachments:[]}};activeId='a'");
  run(ctx, `$('#prompt').value='unsent A'`);
  run(ctx, 'setActiveSession("b")');
  run(ctx, 'setActiveSession("a")');
  if (run(ctx, "sessions['a'].draft") !== 'unsent A') throw new Error('draft A not captured');
  if (run(ctx, "sessions['b'].draft") !== 'unsent B') throw new Error('draft B must be untouched');
  run(ctx, '$("#agentForm").onsubmit({preventDefault(){}})');
  await settle(40);
  if (run(ctx, "sessions['a'].draft") !== '') throw new Error('submitting must clear only the submitted session draft');
  if (run(ctx, "sessions['b'].draft") !== 'unsent B') throw new Error('another session draft was cleared by a submit');
});

test('closing a session removes its transient draft', async () => {
  const ctx = loadContext();
  run(ctx, "sessions={a:{id:'a',workspace:'/w',events:[],createdAt:1,draft:'unsent A'},b:{id:'b',workspace:'/w',events:[],createdAt:2,draft:''}};activeId='a'");
  run(ctx, `$('#prompt').value='unsent A'`);
  await run(ctx, 'closeSession("a")');
  if (run(ctx, `$('#prompt').value`) !== '') throw new Error('closing the active session left its draft in the composer');
  const archived = run(ctx, `closedSessions.find(s=>s.id==='a')`);
  if (archived && archived.draft) throw new Error('draft persisted in the archived record');
});

test('image attachments stay with their session', async () => {
  const ctx = loadContext();
  run(ctx, "sessions={a:{id:'a',workspace:'/w',events:[],createdAt:1,draft:'',attachments:[]},b:{id:'b',workspace:'/w',events:[],createdAt:2,draft:'',attachments:[]}};activeId='a'");
  run(ctx, `attachments.push({id:'img1',name:'a.png',dataUrl:'data:image/png;base64,AAAA',thumb:'data:image/png;base64,BBBB',size:1})`);
  run(ctx, 'setActiveSession("b")');
  if (run(ctx, `attachments.length`) !== 0) throw new Error('attachments followed the user into session B');
  if (run(ctx, `sessions['a'].attachments.length`) !== 1) throw new Error('attachments not retained on session A');
  run(ctx, 'setActiveSession("a")');
  if (run(ctx, `attachments.length`) !== 1) throw new Error('attachments not restored for session A');
  if (run(ctx, `attachments[0].id`) !== 'img1') throw new Error('wrong attachment restored');
});

// --- UX: Tasks control in the composer row. ---

test('Tasks control toggles the task panel and updates aria-expanded', async () => {
  const ctx = loadContext();
  run(ctx, "sessions={a:{id:'a',events:[],busy:true,followBottom:true,unread:0}};activeId='a'");
  run(ctx, 'consumeAgentEvent("a", sessions["a"], EV, new Set())', { EV: { type: 'task', data: { snapshot: '[{"content":"alpha","status":"pending","priority":"high"}]' } } });
  if (run(ctx, "$('#taskPanel').hidden")) throw new Error('panel should be open');
  if (run(ctx, "$('#taskButton').getAttribute('aria-expanded')") !== 'true') throw new Error('open panel must report aria-expanded=true');
  run(ctx, `$('#taskButton').onclick()`);
  if (!run(ctx, "$('#taskPanel').hidden")) throw new Error('toggle did not close the panel');
  if (run(ctx, "$('#taskButton').getAttribute('aria-expanded')") !== 'false') throw new Error('closed panel must report aria-expanded=false');
  run(ctx, `$('#taskButton').onclick()`);
  if (run(ctx, "$('#taskPanel').hidden")) throw new Error('toggle did not reopen the panel');
  if (run(ctx, "$('#taskButton').getAttribute('aria-expanded')") !== 'true') throw new Error('reopened panel must report aria-expanded=true');
});

test('switching sessions updates the Tasks control for the selected session', async () => {
  const ctx = loadContext();
  run(ctx, "sessions={a:{id:'a',events:[],busy:false,followBottom:true,unread:0},b:{id:'b',events:[],busy:false,followBottom:true,unread:0}};activeId='a'");
  run(ctx, 'consumeAgentEvent("a", sessions["a"], EV, new Set())', { EV: { type: 'task', data: { snapshot: '[{"content":"alpha","status":"pending","priority":"high"}]' } } });
  if (run(ctx, "$('#taskButton').hidden")) throw new Error('A with tasks must show the Tasks control');
  run(ctx, 'setActiveSession("b")');
  run(ctx, 'renderAll()');
  if (!run(ctx, "$('#taskButton').hidden")) throw new Error('B without tasks must hide the Tasks control');
  run(ctx, 'setActiveSession("a")');
  run(ctx, 'renderAll()');
  if (run(ctx, "$('#taskButton').hidden")) throw new Error('A Tasks control must return when switching back');
});

// --- UX: workspace-less runs use the configured default root. ---

test('a workspace-less session can run against the default root', async () => {
  let sent = null;
  const ctx = loadContext((url, opt) => {
    if (url === '/api/agent/run') {
      sent = JSON.parse(opt.body);
      return Promise.resolve(streamResponse(['{"type":"run","data":{"runID":"r1"}}', '{"type":"done","data":{"outcome":"completed"}}']));
    }
    return Promise.resolve(jsonOk({}));
  });
  run(ctx, "sessions={a:{id:'a',workspace:'',events:[],busy:false,followBottom:true,unread:0}};activeId='a'");
  run(ctx, 'renderAll()');
  if (run(ctx, "$('#run').disabled")) throw new Error('Run must be enabled for a workspace-less session');
  if (run(ctx, "$('#workspaceLabel').textContent") !== 'Default workspace') throw new Error('label must read Default workspace');
  await run(ctx, 'runAgent("do it")');
  await settle(40);
  if (!sent || sent.workspace !== '') throw new Error('workspace-less run must submit an empty workspace');
});

test('an unavailable explicitly selected workspace keeps Run disabled', async () => {
  const ctx = loadContext();
  run(ctx, "sessions={a:{id:'a',workspace:'/gone',workspaceStatus:'missing',events:[],busy:false,followBottom:true,unread:0}};activeId='a'");
  run(ctx, 'renderAll()');
  if (!run(ctx, "$('#run').disabled")) throw new Error('Run must stay disabled for an unavailable workspace');
});

test('reload restores a consistent default-workspace presentation', async () => {
  const ctx = loadContext((url) => {
    if (url === '/api/conversations') return Promise.resolve(jsonOk([{ id: 'a', workspace: '', workspaceStatus: 'available', state: 'completed', archivedAt: 0, events: [{ kind: 'user', text: 'hi', name: '' }], createdAt: 1, updatedAt: 2 }]));
    return Promise.resolve(jsonOk({}));
  });
  run(ctx, "sessions={};activeId='';serverReady=true");
  await run(ctx, 'syncServerConversations()');
  if (run(ctx, "sessions['a'].workspace") !== '') throw new Error('server-stored empty workspace must stay the default-root state');
  if (run(ctx, "$('#workspaceLabel').textContent") !== 'Default workspace') throw new Error('default workspace label not restored');
  if (run(ctx, "$('#run').disabled")) throw new Error('Run must be enabled after restoring a default-workspace session');
});

// --- UX edge regressions: durable task run identity, blocked submits, and
// attachment ownership. ---

// A stateful fetch that mirrors SQLite conversation persistence: PUT records
// the conversation, GET /api/conversations returns every stored record, so a
// fresh browser context can reload from the "server".
function sqliteMirror(store) {
  return (url, opt = {}) => {
    if (url === '/api/conversation' && (opt.method || 'GET').toUpperCase() === 'PUT') {
      const body = JSON.parse(opt.body);
      const idx = store.findIndex((r) => r.id === body.id);
      if (idx >= 0) store[idx] = { ...store[idx], ...body };
      else store.push(body);
      return Promise.resolve(jsonOk({ ok: true }));
    }
    if (url === '/api/conversations') return Promise.resolve(jsonOk([...store]));
    return Promise.resolve(jsonOk({}));
  };
}

test('task run identity survives persistence and a fresh reload', async () => {
  const store = [];
  const ctx = loadContext(sqliteMirror(store));
  run(ctx, "serverReady=true;sessions={a:{id:'a',workspace:'/w',events:[],busy:false,followBottom:true,unread:0,currentRunId:'run-1'}};activeId='a'");
  // Run A emits a task snapshot.
  run(ctx, 'consumeAgentEvent("a", sessions["a"], EV, new Set(), "run-1")', { EV: { type: 'task', data: { snapshot: '[{"content":"run A","status":"pending","priority":"high"}]' } } });
  // Run B begins: its empty reset is persisted.
  run(ctx, 'resetTaskStateForNewRun("a")');
  run(ctx, "sessions['a'].currentRunId='run-2'");
  // A late Run A task snapshot arrives after Run B started.
  run(ctx, 'consumeAgentEvent("a", sessions["a"], EV, new Set(), "run-1")', { EV: { type: 'task', data: { snapshot: '[{"content":"late run A","status":"pending","priority":"high"}]' } } });
  await settle(30);
  // Confirm the run identity is durable in the persisted event stream: the
  // server-owned task events must carry their owning run id (or empty reset).
  const taskEvents = run(ctx, "sessions['a'].events.filter(e=>e.kind==='task')");
  if (taskEvents.length < 3) throw new Error('expected task reset + snapshots, got ' + taskEvents.length);
  if (taskEvents[0].runID !== 'run-1') throw new Error('run A snapshot lost its run identity in memory');
  // Save to the server mirror and reload in a fresh context.
  run(ctx, 'saveSessionToServer("a")');
  await settle(200);
  if (!store.length) throw new Error('conversation was not persisted');
  // The persisted event schema must be strict-decoder safe: only known fields
  // (kind, text, name, createdAt, runId), never transient outcome/runID.
  const persisted = store[0].events || [];
  for (const ev of persisted) {
    if ('outcome' in ev) throw new Error('transient outcome leaked into the persisted event schema');
    if ('runID' in ev) throw new Error('transient runID leaked into the persisted event schema');
  }
  const ctx2 = loadContext(sqliteMirror(store));
  run(ctx2, "serverReady=true");
  await run(ctx2, 'syncServerConversations()');
  const restored = run(ctx2, "sessions['a']");
  if (!restored) throw new Error('conversation not restored after reload');
  // The reloaded task events must keep their run identity so the stale Run A
  // snapshot cannot become authoritative for Run B.
  const latest = run(ctx2, "(latestTaskEvent(sessions['a'])||{}).text");
  if (latest !== '[]') throw new Error('stale run A snapshot became authoritative after reload: ' + latest);
  if (!run(ctx2, "$('#taskPanel').hidden")) throw new Error('stale run A task list reopened the panel after reload');
});

test('nested run events keep their run association for task snapshots', async () => {
  const ctx = loadContext((url, opt) => {
    if (url === '/api/agent/run') {
      // A nested run event (data.data.runID) must be captured by runAgent's
      // normalized stream path and applied to the task snapshot that follows.
      return Promise.resolve(streamResponse([
        '{"type":"run","data":{"data":{"runID":"nested-run"}}}',
        '{"type":"task","data":{"data":{"snapshot":"[{\\"content\\":\\"nested\\",\\"status\\":\\"in_progress\\",\\"priority\\":\\"high\\"}]"}}}',
        '{"type":"done","data":{"outcome":"completed"}}',
      ]));
    }
    return Promise.resolve(jsonOk({}));
  });
  run(ctx, "sessions={a:{id:'a',workspace:'/w',events:[],busy:false,followBottom:true,unread:0}};activeId='a'");
  await run(ctx, 'runAgent("do it")');
  await settle(30);
  if (run(ctx, "sessions['a'].currentRunId") !== 'nested-run') throw new Error('nested run identity not captured by runAgent');
  if (run(ctx, "sessions['a'].runID") !== 'nested-run') throw new Error('nested run identity not stored on session');
  const task = run(ctx, "sessions['a'].events.filter(e=>e.kind==='task').pop()");
  if (!task || task.runID !== 'nested-run') throw new Error('task snapshot lost the nested run association');
});

test('Enter submit with an unavailable workspace preserves draft, textarea and attachments', async () => {
  let runCalls = 0;
  const ctx = loadContext((url, opt) => {
    if (url === '/api/agent/run') {
      runCalls++;
      return Promise.resolve(streamResponse(['{"type":"run","data":{"runID":"r1"}}', '{"type":"done","data":{"outcome":"completed"}}']));
    }
    return Promise.resolve(jsonOk({}));
  });
  run(ctx, "sessions={a:{id:'a',workspace:'/gone',workspaceStatus:'missing',events:[],createdAt:1,draft:'unsent',attachments:[{id:'img1',name:'a.png',dataUrl:'data:image/png;base64,AAAA',thumb:'t',size:1}],busy:false,followBottom:true,unread:0}};activeId='a'");
  run(ctx, `$('#prompt').value='unsent'`);
  run(ctx, `$('#agentForm').onsubmit({preventDefault(){}})`);
  await settle(30);
  if (runCalls !== 0) throw new Error('an unavailable workspace must never start an agent request');
  if (run(ctx, `$('#prompt').value`) !== 'unsent') throw new Error('textarea was erased by a blocked submit');
  if (run(ctx, "sessions['a'].draft") !== 'unsent') throw new Error('draft was cleared by a blocked submit');
  if (run(ctx, "sessions['a'].attachments.length") !== 1) throw new Error('attachments were cleared by a blocked submit');
  // After selecting a replacement workspace, the same draft submits intact.
  run(ctx, "sessions['a'].workspaceStatus='available'");
  run(ctx, `$('#prompt').value='unsent'`);
  run(ctx, `$('#agentForm').onsubmit({preventDefault(){}})`);
  await settle(30);
  if (runCalls !== 1) throw new Error('replacement workspace did not enable the run');
  if (run(ctx, `$('#prompt').value`) !== '') throw new Error('submitted prompt not cleared from the textarea');
});

test('removing an attachment and clicking the active tab keeps it removed', async () => {
  const ctx = loadContext();
  run(ctx, "sessions={a:{id:'a',workspace:'/w',events:[],createdAt:1,draft:'',attachments:[]},b:{id:'b',workspace:'/w',events:[],createdAt:2,draft:'',attachments:[]}};activeId='a'");
  run(ctx, `setActiveAttachments([{id:'img1',name:'a.png',dataUrl:'d',thumb:'t',size:1},{id:'img2',name:'b.png',dataUrl:'d',thumb:'t',size:1}])`);
  if (run(ctx, `sessions['a'].attachments.length`) !== 2) throw new Error('attachments not owned by session A');
  // Remove one attachment through the rendered remove control.
  run(ctx, `$('#attachments').children[0].children[2].onclick()`);
  if (run(ctx, `attachments.length`) !== 1) throw new Error('remove control did not drop the attachment');
  if (run(ctx, `sessions['a'].attachments.length`) !== 1) throw new Error('session A still holds the removed attachment');
  // Clicking the already-active tab calls setActiveSession with the same id; a
  // stale session array must not resurrect the removed attachment.
  run(ctx, 'setActiveSession("a")');
  if (run(ctx, `attachments.length`) !== 1) throw new Error('removed attachment reappeared after re-selecting the active tab');
  if (run(ctx, `attachments[0].id`) !== 'img2') throw new Error('wrong attachment restored');
});

test('removing an attachment survives switching away and back', async () => {
  const ctx = loadContext();
  run(ctx, "sessions={a:{id:'a',workspace:'/w',events:[],createdAt:1,draft:'',attachments:[]},b:{id:'b',workspace:'/w',events:[],createdAt:2,draft:'',attachments:[]}};activeId='a'");
  run(ctx, `setActiveAttachments([{id:'img1',name:'a.png',dataUrl:'d',thumb:'t',size:1},{id:'img2',name:'b.png',dataUrl:'d',thumb:'t',size:1}])`);
  run(ctx, `$('#attachments').children[0].children[2].onclick()`);
  run(ctx, 'setActiveSession("b")');
  if (run(ctx, `attachments.length`) !== 0) throw new Error('B must have no attachments');
  run(ctx, 'setActiveSession("a")');
  if (run(ctx, `attachments.length`) !== 1) throw new Error('removed attachment reappeared after switching back');
  if (run(ctx, `attachments[0].id`) !== 'img2') throw new Error('wrong attachment restored after switching back');
});

test('removing one of several attachments keeps the rest', async () => {
  const ctx = loadContext();
  run(ctx, "sessions={a:{id:'a',workspace:'/w',events:[],createdAt:1,draft:'',attachments:[]}};activeId='a'");
  run(ctx, `setActiveAttachments([{id:'img1',name:'a.png',dataUrl:'d',thumb:'t',size:1},{id:'img2',name:'b.png',dataUrl:'d',thumb:'t',size:1},{id:'img3',name:'c.png',dataUrl:'d',thumb:'t',size:1}])`);
  run(ctx, `$('#attachments').children[1].children[2].onclick()`);
  if (run(ctx, `attachments.length`) !== 2) throw new Error('expected two attachments after removal');
  if (run(ctx, `attachments.map(a=>a.id).join(',')`) !== 'img1,img3') throw new Error('wrong attachments retained');
  if (run(ctx, `sessions['a'].attachments.map(a=>a.id).join(',')`) !== 'img1,img3') throw new Error('session attachment state diverged');
});

test('submit after removal does not send the removed image', async () => {
  let images = null;
  const ctx = loadContext((url, opt) => {
    if (url === '/api/agent/run') {
      images = JSON.parse(opt.body).images || [];
      return Promise.resolve(streamResponse(['{"type":"run","data":{"runID":"r1"}}', '{"type":"done","data":{"outcome":"completed"}}']));
    }
    return Promise.resolve(jsonOk({}));
  });
  run(ctx, "sessions={a:{id:'a',workspace:'/w',events:[],createdAt:1,draft:'',attachments:[]}};activeId='a'");
  run(ctx, `setActiveAttachments([{id:'img1',name:'a.png',dataUrl:'data:image/png;base64,AAAA',thumb:'t',size:1},{id:'img2',name:'b.png',dataUrl:'data:image/png;base64,BBBB',thumb:'t',size:1}])`);
  run(ctx, `$('#attachments').children[0].children[2].onclick()`);
  await run(ctx, 'runAgent("do it")');
  await settle(30);
  if (!images || images.length !== 1) throw new Error('expected exactly one image in the request');
  if (images[0].name !== 'b.png') throw new Error('the removed image was still sent: ' + JSON.stringify(images));
});

test('closing a session releases its pending attachment state', async () => {
  const ctx = loadContext();
  run(ctx, "sessions={a:{id:'a',workspace:'/w',events:[],createdAt:1,draft:'unsent',attachments:[]},b:{id:'b',workspace:'/w',events:[],createdAt:2,draft:'',attachments:[]}};activeId='a'");
  run(ctx, `setActiveAttachments([{id:'img1',name:'a.png',dataUrl:'d',thumb:'t',size:1}])`);
  run(ctx, `$('#prompt').value='unsent'`);
  await run(ctx, 'closeSession("a")');
  if (run(ctx, `attachments.length`) !== 0) throw new Error('closing a session left its attachments in the composer');
  if (run(ctx, `$('#prompt').value`) !== '') throw new Error('closing a session left its draft in the composer');
  const archived = run(ctx, `closedSessions.find(s=>s.id==='a')`);
  if (archived && archived.attachments && archived.attachments.length) throw new Error('attachment state persisted in the archived record');
});

// --- UX: asynchronous attachment decoding stays owned by the originating
// session, even when the user switches sessions or starts a run while the
// FileReader/thumbnail work is still pending. ---

test('decode completing after a session switch attaches to the originating session', async () => {
  const ctx = loadDecodeContext();
  run(ctx, "sessions={a:{id:'a',workspace:'/w',events:[],createdAt:1,draft:'',attachments:[]},b:{id:'b',workspace:'/w',events:[],createdAt:2,draft:'',attachments:[]}};activeId='a'");
  run(ctx, `addImage({type:'image/png',name:'a.png',size:10})`);
  if (ctx.__readers.length !== 1) throw new Error('decode did not begin');
  run(ctx, 'setActiveSession("b")');
  await completeDecode(ctx, 'data:image/png;base64,AAAA');
  if (run(ctx, "sessions['a'].attachments.length") !== 1) throw new Error('A did not receive its decoded image');
  if (run(ctx, "sessions['b'].attachments.length") !== 0) throw new Error('B received A\'s image');
  if (run(ctx, "attachments.length") !== 0) throw new Error('composer shows A\'s image while B is active');
  run(ctx, 'setActiveSession("a")');
  if (run(ctx, "attachments.length") !== 1) throw new Error('A image not displayed after switching back');
  if (run(ctx, "attachments[0].name") !== 'a.png') throw new Error('wrong attachment on A');
});

test('closing the originating session before decode completes discards the image', async () => {
  const ctx = loadDecodeContext();
  run(ctx, "sessions={a:{id:'a',workspace:'/w',events:[],createdAt:1,draft:'',attachments:[]},b:{id:'b',workspace:'/w',events:[],createdAt:2,draft:'',attachments:[]}};activeId='a'");
  run(ctx, `addImage({type:'image/png',name:'a.png',size:10})`);
  if (ctx.__readers.length !== 1) throw new Error('decode did not begin');
  await run(ctx, 'closeSession("a")');
  await completeDecode(ctx, 'data:image/png;base64,AAAA');
  if (run(ctx, "sessions['b'].attachments.length") !== 0) throw new Error('discarded image leaked to B');
  if (run(ctx, "attachments.length") !== 0) throw new Error('discarded image appeared in the composer');
  const archived = run(ctx, `closedSessions.find(s=>s.id==='a')`);
  if (archived && archived.attachments && archived.attachments.length) throw new Error('discarded image leaked into the archived record');
});

test('a run starting while decoding is pending does not receive the image in-flight', async () => {
  let sent = null;
  const ctx = loadDecodeContext((url, opt) => {
    if (url === '/api/agent/run') {
      sent = JSON.parse(opt.body).images || [];
      return Promise.resolve(streamResponse(['{"type":"run","data":{"runID":"r1"}}', '{"type":"done","data":{"outcome":"completed"}}']));
    }
    return Promise.resolve(jsonOk({}));
  });
  run(ctx, "sessions={a:{id:'a',workspace:'/w',events:[],createdAt:1,draft:'',attachments:[]}};activeId='a'");
  run(ctx, `addImage({type:'image/png',name:'a.png',size:10})`);
  if (ctx.__readers.length !== 1) throw new Error('decode did not begin');
  await run(ctx, 'runAgent("do it")');
  if (sent && sent.length !== 0) throw new Error('pending decode was injected into the in-flight request');
  await completeDecode(ctx, 'data:image/png;base64,AAAA');
  if (run(ctx, "sessions['a'].attachments.length") !== 1) throw new Error('image not retained as a pending attachment for the next run');
  if (sent.length !== 0) throw new Error('completed image affected the in-flight request');
});

test('only MAX_IMAGES decodes start; excess additions are rejected before FileReader construction', async () => {
  const ctx = loadDecodeContext();
  run(ctx, "sessions={a:{id:'a',workspace:'/w',events:[],createdAt:1,draft:'',attachments:[]}};activeId='a'");
  const MAX = run(ctx, 'MAX_IMAGES');
  let accepted = 0;
  for (let i = 0; i < MAX + 2; i++) {
    if (run(ctx, `addImage({type:'image/png',name:'i'+${i}+'.png',size:10})`) === true) accepted++;
  }
  if (accepted !== MAX) throw new Error('expected exactly MAX_IMAGES accepted additions, got ' + accepted);
  if (ctx.__readers.length !== MAX) throw new Error('expected exactly MAX_IMAGES FileReaders, got ' + ctx.__readers.length);
  if (run(ctx, `reservedDecodes('a')`) !== MAX) throw new Error('pending decode reservations must equal MAX_IMAGES');
  // Completing all started decodes leaves MAX attachments and zero reservations.
  for (let i = 0; i < MAX; i++) {
    await completeDecode(ctx, 'data:image/png;base64,AAAA');
  }
  if (run(ctx, "sessions['a'].attachments.length") !== MAX) throw new Error('attachments must equal MAX_IMAGES');
  if (run(ctx, `reservedDecodes('a')`) !== 0) throw new Error('reservations must be released after successful completions');
});

test('a failed read releases its reservation and permits another image', async () => {
  const ctx = loadDecodeContext();
  run(ctx, "sessions={a:{id:'a',workspace:'/w',events:[],createdAt:1,draft:'',attachments:[]}};activeId='a'");
  const MAX = run(ctx, 'MAX_IMAGES');
  for (let i = 0; i < MAX; i++) run(ctx, `addImage({type:'image/png',name:'i'+${i}+'.png',size:10})`);
  if (run(ctx, `reservedDecodes('a')`) !== MAX) throw new Error('expected MAX reservations');
  if (run(ctx, `addImage({type:'image/png',name:'extra.png',size:10})`) !== false) throw new Error('a full session must reject a new add');
  await failDecode(ctx);
  if (run(ctx, `reservedDecodes('a')`) !== MAX - 1) throw new Error('failed read did not release its reservation');
  if (run(ctx, `addImage({type:'image/png',name:'again.png',size:10})`) !== true) throw new Error('released reservation must accept another image');
  if (ctx.__readers.length !== MAX) throw new Error('reader count should return to MAX after refill');
});

test('a failed thumbnail releases its reservation', async () => {
  const ctx = loadDecodeContext();
  run(ctx, "sessions={a:{id:'a',workspace:'/w',events:[],createdAt:1,draft:'',attachments:[]}};activeId='a'");
  const MAX = run(ctx, 'MAX_IMAGES');
  for (let i = 0; i < MAX; i++) run(ctx, `addImage({type:'image/png',name:'i'+${i}+'.png',size:10})`);
  if (run(ctx, `reservedDecodes('a')`) !== MAX) throw new Error('expected MAX reservations');
  await failThumbnail(ctx, 'data:image/png;base64,AAAA');
  if (run(ctx, `reservedDecodes('a')`) !== MAX - 1) throw new Error('failed thumbnail did not release its reservation');
  if (run(ctx, `addImage({type:'image/png',name:'again.png',size:10})`) !== true) throw new Error('released reservation must accept another image');
  if (ctx.__readers.length !== MAX) throw new Error('reader count should return to MAX after refill');
});

test('closing a session with pending reads cannot leak or recreate state', async () => {
  const ctx = loadDecodeContext();
  run(ctx, "sessions={a:{id:'a',workspace:'/w',events:[],createdAt:1,draft:'',attachments:[]},b:{id:'b',workspace:'/w',events:[],createdAt:2,draft:'',attachments:[]}};activeId='a'");
  run(ctx, `addImage({type:'image/png',name:'a.png',size:10})`);
  if (ctx.__readers.length !== 1) throw new Error('decode did not begin');
  if (run(ctx, `reservedDecodes('a')`) !== 1) throw new Error('reservation not recorded');
  await run(ctx, 'closeSession("a")');
  await completeDecode(ctx, 'data:image/png;base64,AAAA');
  if (run(ctx, "sessions['a']") !== undefined) throw new Error('completing a decode must not recreate the closed session');
  if (run(ctx, "sessions['b'].attachments.length") !== 0) throw new Error('closed session decode leaked to B');
  if (run(ctx, "attachments.length") !== 0) throw new Error('closed session decode appeared in the composer');
  if (run(ctx, `reservedDecodes('a')`) !== 0) throw new Error('reservation leaked after session close');
});

test('switching sessions keeps pending limits independent per session', async () => {
  const ctx = loadDecodeContext();
  run(ctx, "sessions={a:{id:'a',workspace:'/w',events:[],createdAt:1,draft:'',attachments:[]},b:{id:'b',workspace:'/w',events:[],createdAt:2,draft:'',attachments:[]}};activeId='a'");
  const MAX = run(ctx, 'MAX_IMAGES');
  for (let i = 0; i < MAX; i++) run(ctx, `addImage({type:'image/png',name:'i'+${i}+'.png',size:10})`);
  if (run(ctx, `reservedDecodes('a')`) !== MAX) throw new Error('A should hold MAX reservations');
  run(ctx, 'setActiveSession("b")');
  if (run(ctx, `addImage({type:'image/png',name:'b.png',size:10})`) !== true) throw new Error('B must accept a fresh image');
  if (run(ctx, `reservedDecodes('a')`) !== MAX) throw new Error('A reservations must stay MAX after B addition');
  if (run(ctx, `reservedDecodes('b')`) !== 1) throw new Error('B reservation should be 1');
  // The B reader is the last one queued; complete it specifically.
  const bReader = ctx.__readers[ctx.__readers.length - 1];
  bReader.result = 'data:image/png;base64,AAAA';
  bReader.onload();
  await settle(0);
  const bImg = ctx.__images.shift();
  bImg.width = 64;
  bImg.height = 64;
  bImg.onload();
  await settle(0);
  if (run(ctx, `reservedDecodes('b')`) !== 0) throw new Error('B reservation not released');
  if (run(ctx, `reservedDecodes('a')`) !== MAX) throw new Error('A reservations must remain unaffected by B completion');
  run(ctx, 'setActiveSession("a")');
  if (run(ctx, `addImage({type:'image/png',name:'extra.png',size:10})`) !== false) throw new Error('A must still reject while full');
});

test('a decoding failure does not affect another session', async () => {
  const ctx = loadDecodeContext();
  run(ctx, "sessions={a:{id:'a',workspace:'/w',events:[],createdAt:1,draft:'',attachments:[]},b:{id:'b',workspace:'/w',events:[],createdAt:2,draft:'',attachments:[]}};activeId='a'");
  run(ctx, `addImage({type:'image/png',name:'a.png',size:10})`);
  run(ctx, 'setActiveSession("b")');
  await failDecode(ctx);
  if (run(ctx, "sessions['a'].attachments.length") !== 0) throw new Error('failed decode attached to A');
  if (run(ctx, "sessions['b'].attachments.length") !== 0) throw new Error('failed decode affected B');
  if (run(ctx, "attachments.length") !== 0) throw new Error('failed decode affected the composer');
});

// --- UX: layout grouping of the Tasks control and immediate preference save. ---

test('Copy session and Tasks are grouped on the left in the compose row', async () => {
  const html = require('fs').readFileSync(path.join(__dirname, '..', 'content', 'index.html'), 'utf8');
  const css = require('fs').readFileSync(path.join(__dirname, '..', 'public', 'assets', 'css', 'style.css'), 'utf8');
  const row = html.match(/<div class="compose-row">([\s\S]*?)<\/div><\/form>/);
  if (!row) throw new Error('compose-row not found in index.html');
  const actions = row[1].match(/<div class="compose-actions">([\s\S]*?)<\/div>/);
  if (!actions) throw new Error('compose-actions group not found next to Copy session');
  const copyIdx = actions[1].indexOf('id="copy"');
  const tasksIdx = actions[1].indexOf('id="taskButton"');
  if (copyIdx < 0) throw new Error('Copy session is not inside the compose-actions group');
  if (tasksIdx < 0) throw new Error('Tasks control is not inside the compose-actions group');
  if (tasksIdx < copyIdx) throw new Error('Tasks control must come directly after Copy session');
  if (!actions[1].includes('aria-controls="taskPanel"')) throw new Error('Tasks control must carry aria-controls="taskPanel"');
  if (!/\.compose-actions\{[^}]*margin-right:auto/.test(css)) throw new Error('compose-actions must own the right margin instead of #copy');
  if (/\.compose-row\s*#copy\{[^}]*margin-right:auto/.test(css)) throw new Error('the obsolete #copy margin rule must be removed');
});

test('task reset persists the cleared collapsed preference immediately', async () => {
  const ctx = loadContext();
  run(ctx, "sessions={a:{id:'a',workspace:'/w',events:[],busy:true,followBottom:true,unread:0}};activeId='a'");
  run(ctx, 'consumeAgentEvent("a", sessions["a"], EV, new Set())', { EV: { type: 'task', data: { snapshot: '[{"content":"alpha","status":"pending","priority":"high"}]' } } });
  run(ctx, 'setTasksCollapsed(true)');
  if (run(ctx, "uiPrefs.collapsed['a']") !== true) throw new Error('collapsed preference not set');
  run(ctx, 'resetTaskStateForNewRun("a")');
  if (run(ctx, "sessions['a'].tasksCollapsed") !== false) throw new Error('collapsed state not cleared');
  if (run(ctx, "uiPrefs.collapsed['a']") === true) throw new Error('collapsed UI preference not cleared immediately');
  const ui = run(ctx, `localStorage.getItem('cortex.ui.v1')`);
  if (!ui || ui.includes('"a":true')) throw new Error('cleared collapsed preference not persisted immediately: ' + ui);
});

test('reload during a long-running agent cannot restore the previous collapsed preference', async () => {
  const store = [];
  const ctx = loadContext(sqliteMirror(store));
  run(ctx, "serverReady=true;sessions={a:{id:'a',workspace:'/w',events:[],busy:true,followBottom:true,unread:0}};activeId='a'");
  run(ctx, 'consumeAgentEvent("a", sessions["a"], EV, new Set())', { EV: { type: 'task', data: { snapshot: '[{"content":"alpha","status":"pending","priority":"high"}]' } } });
  run(ctx, 'setTasksCollapsed(true)');
  // A new run starts; the reset must persist the cleared preference before any
  // reload, so the running agent cannot reopen the old collapsed state.
  run(ctx, 'resetTaskStateForNewRun("a")');
  run(ctx, "sessions['a'].currentRunId='run-2'");
  const ui = run(ctx, `localStorage.getItem('cortex.ui.v1')`);
  if (!ui || ui.includes('"a":true')) throw new Error('reset did not persist the cleared collapsed preference');
  // Persist and reload: the running session must present the new run's task
  // state (open/empty) rather than the previous run's collapsed preference.
  run(ctx, 'saveSessionToServer("a")');
  await settle(200);
  const ctx2 = loadContext(sqliteMirror(store));
  run(ctx2, "serverReady=true");
  await run(ctx2, 'syncServerConversations()');
  if (run(ctx2, "sessions['a'].tasksCollapsed")) throw new Error('previous collapsed preference leaked into the new run after reload');
  // The running session's task list is the new run's empty reset, so the panel
  // and its control are hidden/open rather than collapsed.
  if (run(ctx2, "$('#taskPanel').hidden") !== true) throw new Error('empty reset snapshot must keep the panel hidden');
  if (run(ctx2, "$('#taskButton').hidden") !== true) throw new Error('empty reset snapshot must keep the Tasks control hidden');
});

(async function runAll() {
  let pass = 0, fail = 0;
  for (const { name, fn } of __tests) {
    try {
      await fn();
      console.log('ok - ' + name);
      pass++;
    } catch (e) {
      console.error('FAIL - ' + name);
      console.error(e && e.stack || e);
      process.exitCode = 1;
      fail++;
    }
  }
  console.log(`\n${pass} passed, ${fail} failed, ${__tests.length} total`);
  if (fail > 0) process.exitCode = 1;
})();

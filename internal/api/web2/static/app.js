// The whole client.
//
// Its vocabulary is four verbs — fetch a fragment, put it somewhere, post a
// form, ask before something destructive — driven by data attributes on
// server-rendered HTML. It knows nothing about trading, settings, formatting or
// translation; all of that is in Go now.
//
// It exists at all for one reason: the API token must travel in an Authorization
// header rather than a URL, and a browser navigating to a link cannot set
// headers. So navigation is fetch + swap.

const TOKEN_KEY = 'gfb.token';
const PERSIST_KEY = 'gfb.token.persist';
const THEME_KEY = 'gfb.theme';
const RISK_KEY = 'gfb.riskAck';

let token = sessionStorage.getItem(TOKEN_KEY) || localStorage.getItem(PERSIST_KEY) || '';
let authRequired = false;
let stream = null;

const $ = (sel, root = document) => root.querySelector(sel);
const $$ = (sel, root = document) => [...root.querySelectorAll(sel)];

// ----- transport -----

const headers = (extra) => (token ? { ...extra, Authorization: `Bearer ${token}` } : extra);

/**
 * request performs one call. A 401 forgets the token and shows the login screen:
 * continuing to send a credential the server has rejected only produces a stream
 * of failures.
 */
async function request(url, opts = {}) {
  const res = await fetch(url, { ...opts, headers: headers(opts.headers) });

  if (res.status === 401) {
    authRequired = true;

    const had = !!token;

    setToken('', false);
    showLogin(had);

    throw new Error('unauthorized');
  }

  const body = await res.text();

  if (!res.ok) throw new Error(stripTags(body) || `HTTP ${res.status}`);

  return body;
}

/** stripTags reduces an HTML error page to its text, for a toast. */
const stripTags = (s) => s.replace(/<[^>]*>/g, ' ').trim().slice(0, 300);

// ----- swapping -----

/**
 * swap replaces a target's contents with server-rendered HTML.
 *
 * This is the one place innerHTML is used, and it is safe *because* the HTML
 * came from html/template, which escapes contextually. Nothing here builds
 * markup out of data — that is exactly what moving rendering to Go removed.
 */
function swap(target, html) {
  target.innerHTML = html;

  wire(target);
}

/** resolve turns a data-target value into an element. */
function resolve(el, spec) {
  if (!spec || spec === 'self') return el;
  if (spec === 'closest-panel') return el.closest('.panel') || el;

  return $(spec) || $('#view');
}

/** load fetches a fragment into a target. */
async function load(url, target) {
  try {
    swap(target, await request(url));
  } catch (err) {
    if (String(err) !== 'Error: unauthorized') toast(String(err.message || err), 'err');
  }
}

// ----- wiring -----

/**
 * listen installs the delegated handlers, once, for the life of the page.
 *
 * Everything driven by an event is handled here rather than bound per element.
 * That is not only shorter — it is the difference between working and not.
 * Binding on insertion means every newly swapped fragment must be walked again,
 * and anything the walk forgets silently does nothing: the tab bar is rendered
 * once into the shell, so a per-element pass over swapped fragments never sees
 * it, and the navigation quietly did nothing at all. Delegation cannot miss an
 * element, and cannot double-bind one either.
 */
function listen() {
  document.addEventListener('click', (e) => {
    const el = e.target.closest(
      '[data-nav],[data-post],[data-delete],[data-dismiss],[data-refresh],[data-fill]');

    if (!el) return;

    e.preventDefault();

    if (el.dataset.nav !== undefined) return navigate(el.dataset.nav);
    if (el.dataset.refresh !== undefined) return navigate(current(), false);

    if (el.dataset.dismiss !== undefined) {
      localStorage.setItem(RISK_KEY, '1');
      el.closest('.banner')?.remove();

      return;
    }

    if (el.dataset.fill !== undefined) return fill(el);

    act(el);
  });

  document.addEventListener('submit', (e) => {
    const form = e.target.closest('form[data-form],form[data-inline-get]');
    if (!form) return;

    e.preventDefault();

    if (form.dataset.inlineGet !== undefined) narrow(form);
    else submit(form);
  });

  // A select inside a filtering form applies on change, without a button.
  document.addEventListener('change', (e) => {
    const form = e.target.closest('form[data-inline-get]');

    if (form) narrow(form);
  });
}

/**
 * wire starts the fetches a freshly inserted subtree declares.
 *
 * Only data-fetch/data-poll live here: they have to run when an element appears,
 * not when something happens to it, so they cannot be delegated.
 */
function wire(root) {
  for (const el of $$('[data-fetch]', root)) {
    const target = resolve(el, el.dataset.target);

    load(el.dataset.fetch, target);

    if (el.dataset.poll) startPoll(el, () => load(el.dataset.fetch, target));
  }
}

/** narrow re-fetches a view with a filtering form's current values. */
function narrow(form) {
  const q = new URLSearchParams(new FormData(form)).toString();

  load(`${form.dataset.inlineGet}?${q}`, resolve(form, form.dataset.target));
}

/**
 * startPoll repeats fn while the element is on the page and the tab is visible.
 *
 * The timer cancels itself once its element is swapped away, which is simpler
 * and more reliable than watching the DOM for removals: a poll that outlives its
 * view keeps hitting the API forever, and against a rate-limited broker that
 * competes with the trading loop.
 */
function startPoll(el, fn) {
  const ms = Number(el.dataset.poll);
  if (!ms) return;

  const id = setInterval(() => {
    if (!el.isConnected) return clearInterval(id);
    if (!document.hidden) fn();
  }, ms);
}

/** act runs a data-post or data-delete button, after any confirmation. */
async function act(el) {
  const confirmText = el.dataset.confirm;
  if (confirmText && !window.confirm(confirmText)) return;

  const url = el.dataset.post || el.dataset.delete;
  const opts = el.dataset.delete
    ? { method: 'DELETE' }
    : {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: el.dataset.body || '{}',
    };

  el.disabled = true;

  try {
    await request(url, opts);
    toast(el.dataset.done || 'OK', 'ok');
    navigate(current(), false);
  } catch (err) {
    if (String(err) !== 'Error: unauthorized') toast(String(err.message || err), 'err');
  } finally {
    el.disabled = false;
  }
}

/**
 * fill writes data-fill-value into the element data-fill names, e.g. the
 * optimizer's "apply" button copying a tested value into the strategy form's
 * matching input. It does not submit anything — the operator still reviews
 * and saves the field themselves.
 */
function fill(el) {
  const target = document.getElementById(el.dataset.fill);
  if (!target) return;

  target.value = el.dataset.fillValue ?? '';
  target.dispatchEvent(new Event('input', { bubbles: true }));
}

/** submit posts a form and reports the server's answer beside the button. */
async function submit(form) {
  const status = $('[data-form-status]', form);
  const button = $('button[type=submit]', form);

  if (status) status.textContent = '…';

  if (button) button.disabled = true;

  try {
    const body = await request(form.action, { method: 'POST', body: new FormData(form) });

    if (status) status.textContent = stripTags(body) || 'OK';

    toast(stripTags(body) || 'OK', 'ok');
  } catch (err) {
    const message = String(err.message || err);

    if (status) status.textContent = message;

    if (message !== 'unauthorized') toast(message, 'err');
  } finally {
    if (button) button.disabled = false;
  }
}

// ----- navigation -----

const current = () => (location.hash || '#/dashboard').replace(/^#\/?/, '') || 'dashboard';

function navigate(name, push = true) {
  if (push && current() !== name) location.hash = `#/${name}`;

  for (const tab of $$('[data-nav]')) {
    tab.setAttribute('aria-current', tab.dataset.nav === name ? 'page' : 'false');
  }

  load(`/v2/f/${encodeURIComponent(name)}`, $('#view'));
}

// ----- auth -----

function setToken(value, persist) {
  token = value || '';

  sessionStorage.setItem(TOKEN_KEY, token);

  if (persist) localStorage.setItem(PERSIST_KEY, token);
  else localStorage.removeItem(PERSIST_KEY);
}

async function showLogin(rejected) {
  stream?.abort();

  stream = null;

  $('#tabs').hidden = true;
  $('#lock').hidden = true;

  swap($('#view'), await request(`/v2/f/login?rejected=${rejected ? 1 : 0}`).catch(() => ''));

  const unlock = $('#unlock');
  const input = $('#token');

  const go = () => {
    if (!input.value.trim()) return;

    setToken(input.value.trim(), $('#persist').checked);
    start();
  };

  unlock?.addEventListener('click', go);
  input?.addEventListener('keydown', (e) => e.key === 'Enter' && go());
  input?.focus();
}

// ----- live event stream -----

/**
 * connect reads the SSE stream with fetch so the token can travel in a header.
 * EventSource cannot set one, which is why the old page put it in the query
 * string — where it lands in access logs, proxy logs and browser history.
 */
async function connect() {
  const control = new AbortController();

  stream = control;

  let backoff = 1000;

  while (!control.signal.aborted) {
    try {
      const res = await fetch('/api/events', {
        headers: headers({ Accept: 'text/event-stream' }),
        signal: control.signal,
      });

      if (res.status === 401) return showLogin(true);
      if (!res.ok || !res.body) throw new Error(String(res.status));

      setLive(true);

      backoff = 1000;

      const reader = res.body.getReader();

      // The payload is not parsed: the pill only reports that the stream is up,
      // and every view refreshes itself by polling. Anything more would be a
      // second rendering path.
      for (;;) {
        const { done } = await reader.read();
        if (done) break;
      }
    } catch {
      if (control.signal.aborted) return;
    }

    setLive(false);

    await new Promise((r) => setTimeout(r, backoff));

    backoff = Math.min(backoff * 2, 30000);
  }
}

// setLive reflects the event stream's state in the top bar.
//
// It has to set the TEXT, not just the colour: the label is server-rendered as
// "reconnecting..." and, without this, said so forever no matter how healthy the
// stream was. The two labels come from data attributes so this stays translated
// without shipping the catalogs to the browser.
function setLive(up) {
  const pill = $('#live-pill');
  if (!pill) return;

  pill.className = `pill ${up ? 'on' : 'warn'}`;
  pill.textContent = up ? pill.dataset.up : pill.dataset.down;
  pill.title = up ? '' : pill.dataset.help;
}

// ----- chrome -----

function toast(message, kind) {
  const host = $('#toasts');
  if (!host) return;

  const node = document.createElement('div');

  node.className = `toast ${kind}`;
  node.textContent = message;

  host.append(node);
  setTimeout(() => node.remove(), 5000);
}

function banners() {
  const host = $('#banners');

  host.replaceChildren();

  if (!authRequired) host.append($('#banner-noauth').content.cloneNode(true));

  if (localStorage.getItem(RISK_KEY) !== '1') {
    host.append($('#banner-risk').content.cloneNode(true));
  }

  wire(host);
}

async function start() {
  banners();

  $('#tabs').hidden = false;
  $('#lock').hidden = !authRequired || !token;

  wire(document);
  navigate(current(), false);
  connect();
}

async function boot() {
  document.documentElement.dataset.theme = localStorage.getItem(THEME_KEY) || 'dark';

  $('#theme').addEventListener('click', () => {
    const next = document.documentElement.dataset.theme === 'dark' ? 'light' : 'dark';

    document.documentElement.dataset.theme = next;
    localStorage.setItem(THEME_KEY, next);
  });

  // The language is a cookie the server reads, so a reload renders in it.
  $('#lang').addEventListener('change', (e) => {
    document.cookie = `lang=${encodeURIComponent(e.target.value)};path=/;max-age=31536000;samesite=strict`;
    location.reload();
  });

  $('#lock').addEventListener('click', () => {
    setToken('', false);
    showLogin(false);
  });

  listen();

  window.addEventListener('hashchange', () => navigate(current(), false));

  // One unauthenticated probe tells us whether to show a login screen at all.
  authRequired = await fetch('/health')
    .then((r) => r.json())
    .then((b) => !!b.auth_required)
    .catch(() => false);

  if (authRequired && !token) {
    banners();
    showLogin(false);

    return;
  }

  start();
}

boot();

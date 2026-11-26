// mailpilot web: opens mails without reloading the page, refreshes the list
// when a mail arrives or is analysed, and runs the mailbox buttons with an
// undo. Everything also works without JavaScript (plain links and forms).
'use strict';

const $ = (sel, root = document) => root.querySelector(sel);

async function fetchPartial(url, part) {
  const res = await fetch(url, { headers: { 'X-Partial': part }, credentials: 'same-origin' });
  if (res.status === 401) { location.href = '/login'; return null; }
  if (!res.ok) throw new Error('HTTP ' + res.status);
  return new DOMParser().parseFromString('<body>' + await res.text() + '</body>', 'text/html').body;
}

function selectItem(id) {
  document.querySelectorAll('.item.selected').forEach((el) => el.classList.remove('selected'));
  const li = document.querySelector('.item[data-id="' + id + '"]');
  if (li) li.classList.add('selected');
}

// byUser: Louis opened this mail himself (click, link, page load). Only then
// may it be marked read; a refresh after an analysis never does.
async function openPane(url, push, byUser) {
  const frag = await fetchPartial(url, 'pane');
  if (!frag) return;
  const pane = $('#pane');
  pane.replaceChildren(...frag.childNodes);
  pane.scrollTop = 0;
  const mail = $('.mail', pane);
  document.body.classList.toggle('has-pane', !!mail);
  if (mail) {
    selectItem(mail.dataset.id);
    document.title = ($('.mail-subject', pane)?.textContent || 'mailpilot') + ' · mailpilot';
  }
  if (push) history.pushState({ pane: true }, '', url);
  if (byUser) markOnOpen();
}

document.addEventListener('click', (e) => {
  const back = e.target.closest('a[data-back]');
  if (back && window.matchMedia('(max-width: 820px)').matches) {
    e.preventDefault();
    document.body.classList.remove('has-pane');
    history.pushState({}, '', back.href);
    return;
  }
  const a = e.target.closest('a[data-pane]');
  if (!a || e.metaKey || e.ctrlKey || e.shiftKey || e.button !== 0) return;
  e.preventDefault();
  openPane(a.href, true, true).catch(() => { location.href = a.href; });
});

window.addEventListener('popstate', () => {
  const m = new URL(location.href).searchParams.get('m');
  if (m || new URL(location.href).searchParams.get('u')) openPane(location.href, false, true).catch(() => location.reload());
  else { document.body.classList.remove('has-pane'); $('#pane').innerHTML = ''; }
});

async function refreshList() {
  const before = new Set([...document.querySelectorAll('.item')].map((el) => el.dataset.id));
  const frag = await fetchPartial(location.href, 'list');
  if (!frag) return;
  const nav = $('#nav', frag), list = $('#list', frag);
  if (nav) $('#nav').replaceChildren(...nav.childNodes);
  if (list) {
    const scroll = $('#list').scrollTop;
    $('#list').replaceChildren(...list.childNodes);
    $('#list').scrollTop = scroll;
    document.querySelectorAll('.item').forEach((el) => { if (!before.has(el.dataset.id)) el.classList.add('new'); });
  }
  const mail = $('.mail');
  if (mail) {
    selectItem(mail.dataset.id);
    // the open mail may just have been analysed
    if (!('byloc' in mail.dataset) && $('.ai .muted', mail)) openPane(location.href, false).catch(() => {});
  }
}

function live() {
  if (!window.EventSource) return;
  const dot = $('#live');
  const es = new EventSource('/events');
  es.onopen = () => dot && dot.classList.add('on');
  es.onerror = () => dot && dot.classList.remove('on');
  let timer = null;
  es.addEventListener('change', () => {
    clearTimeout(timer);
    timer = setTimeout(() => refreshList().catch(() => {}), 300);
  });
}

// ---------- mailbox actions ----------

const csrf = () => document.body.dataset.csrf || '';

async function post(fields) {
  const body = new URLSearchParams(fields);
  body.set('csrf', csrf());
  const res = await fetch('/action', {
    method: 'POST', body, credentials: 'same-origin',
    headers: { Accept: 'application/json' },
  });
  if (res.status === 401) { location.href = '/login'; return null; }
  let data;
  try { data = await res.json(); } catch { data = { ok: false, message: 'Erreur ' + res.status }; }
  return data;
}

let toastTimer = null;
function toast(message, undo) {
  const t = $('#toast');
  if (!t) return;
  t.replaceChildren(document.createTextNode(message));
  if (undo) {
    const b = document.createElement('button');
    b.type = 'button';
    b.textContent = 'Annuler';
    b.addEventListener('click', async () => {
      t.hidden = true;
      const f = { op: 'deplacer', dest: undo.dest };
      if (undo.id) f.id = undo.id; else { f.f = undo.f; f.v = undo.v; f.u = undo.u; }
      const r = await post(f).catch(() => null);
      toast(r && r.ok ? 'Annulé' : (r ? r.message : 'Annulation impossible'));
      refreshList().catch(() => {});
    });
    t.append(b);
  }
  t.hidden = false;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => { t.hidden = true; }, undo ? 8000 : 3500);
}

function formFields(form) {
  return Object.fromEntries(new FormData(form).entries());
}

function listItem(id) { return document.querySelector('.item[data-id="' + id + '"]'); }

// After a mail leaves the list: open the next one on a wide screen, go back
// to the list on a phone.
function leave(id) {
  const li = listItem(id);
  const next = li && (li.nextElementSibling?.matches('.item') ? li.nextElementSibling : li.previousElementSibling);
  if (li) li.remove();
  const mobile = window.matchMedia('(max-width: 820px)').matches;
  const back = $('.mail a[data-back]');
  if (!mobile && next && next.matches('.item')) {
    const a = $('a[data-pane]', next);
    openPane(a.href, true, false).catch(() => {});
    return;
  }
  document.body.classList.remove('has-pane');
  $('#pane').innerHTML = '';
  if (back) history.pushState({}, '', back.getAttribute('href'));
}

async function runAction(form) {
  const fields = formFields(form);
  const mail = form.closest('.mail');
  const id = mail && mail.dataset.id;
  form.querySelectorAll('button').forEach((b) => { b.disabled = true; });
  let r = null;
  try { r = await post(fields); } catch { r = null; }
  form.querySelectorAll('button').forEach((b) => { b.disabled = false; });
  if (!r) { toast('Action impossible : réseau indisponible'); return; }
  toast(r.message, r.ok ? r.undo : null);
  if (!r.ok) {
    if (r.gone) refreshList().catch(() => {});
    return;
  }
  if (r.gone) { leave(id); return; }
  if (fields.op === 'lu' || fields.op === 'nonlu') setSeenUI(id, fields.op === 'lu');
}

function setSeenUI(id, seen) {
  const li = listItem(id);
  if (li) li.classList.toggle('unread', !seen);
  openPane(location.href, false, false).catch(() => {});
}

// Mark read when Louis opens an unread mail himself (setting
// web.mark_read_on_open; the server only sets the flag on this POST).
function markOnOpen() {
  const bar = $('.mail .toolbar[data-mark-on-open]');
  if (!bar) return;
  bar.removeAttribute('data-mark-on-open');
  const form = $('button[data-seen-toggle]', bar)?.form;
  if (!form) return;
  const id = bar.closest('.mail').dataset.id;
  post(formFields(form)).then((r) => { if (r && r.ok) setSeenUI(id, true); }).catch(() => {});
}

document.addEventListener('submit', (e) => {
  const form = e.target;
  if (form.closest('.toolbar')) { e.preventDefault(); runAction(form); return; }
  if (form.matches('.send-form')) { e.preventDefault(); prepareSend(form); return; }
  if (form.matches('[data-ajax]')) { e.preventDefault(); ajaxForm(form); }
});

// ---------- replies, sending, reminders ----------

// ajaxForm posts a compose or reminder form, then reloads the open mail.
async function ajaxForm(form) {
  const busy = form.dataset.busy;
  const box = form.closest('.compose') || form.closest('.confirm-dialog');
  const buttons = form.querySelectorAll('button');
  buttons.forEach((b) => { b.disabled = true; });
  if (busy && box) { box.classList.add('busy'); box.dataset.busyText = busy; }
  let data = null;
  try {
    const res = await fetch(form.action, {
      method: 'POST', body: new URLSearchParams(new FormData(form)), credentials: 'same-origin',
      headers: { Accept: 'application/json' },
    });
    if (res.status === 401) { location.href = '/login'; return; }
    try { data = await res.json(); } catch { data = { ok: false, message: 'Erreur ' + res.status }; }
  } catch { data = { ok: false, message: 'Réseau indisponible' }; }
  if (box) box.classList.remove('busy');
  buttons.forEach((b) => { b.disabled = false; });
  toast(data.message);
  const dlg = form.closest('dialog');
  if (dlg && data.ok) dlg.close();
  if (data.ok && data.redirect) { setTimeout(() => { location.href = data.redirect; }, 700); return; }
  if (data.ok) {
    await openPane(location.href, false, false).catch(() => {});
    if (form.closest('.compose')) $('#compose')?.scrollIntoView({ block: 'nearest' });
  }
}

// prepareSend is the first click on "Envoyer": the server shows exactly
// what will leave, in a dialog with a single-use confirmation.
async function prepareSend(form) {
  const fields = form.closest('.compose')?.querySelectorAll('.editor textarea, .editor input[type=text]') || [];
  if ([...fields].some((f) => f.value !== f.defaultValue)) {
    toast('Enregistre tes modifications avant d\'envoyer.');
    return;
  }
  let res;
  try {
    res = await fetch('/envoi/preparer', { method: 'POST', body: new URLSearchParams(new FormData(form)), credentials: 'same-origin' });
  } catch { toast('Réseau indisponible'); return; }
  if (res.status === 401) { location.href = '/login'; return; }
  const text = await res.text();
  if (!res.ok) { toast(text.trim()); return; }
  const dlg = $('#confirm');
  dlg.replaceChildren(...new DOMParser().parseFromString('<body>' + text + '</body>', 'text/html').body.childNodes);
  dlg.showModal();
}

document.addEventListener('keydown', (e) => {
  if (e.metaKey || e.ctrlKey || e.altKey) return;
  if (e.target.closest('input, select, textarea, [contenteditable], dialog')) return;
  if ($('#confirm')?.open) return;
  if (e.key === 'Escape' && document.body.classList.contains('has-pane')) {
    const back = $('.mail a[data-back]');
    if (back) back.click();
    return;
  }
  const btn = $('.mail .toolbar button[data-key="' + CSS.escape(e.key) + '"]');
  if (btn) { e.preventDefault(); runAction(btn.form); }
});

if ($('#list')) live();
if ($('.mail')) markOnOpen();
if ('serviceWorker' in navigator) navigator.serviceWorker.register('/sw.js').catch(() => {});

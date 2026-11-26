// mailpilot web: opens mails without reloading the page and refreshes the
// list when a mail arrives or is analysed. Everything also works without
// JavaScript (plain links).
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

async function openPane(url, push) {
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
  openPane(a.href, true).catch(() => { location.href = a.href; });
});

window.addEventListener('popstate', () => {
  const m = new URL(location.href).searchParams.get('m');
  if (m) openPane(location.href, false).catch(() => location.reload());
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
    if ($('.ai .muted', mail)) openPane(location.href, false).catch(() => {});
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

if ($('#list')) live();
if ('serviceWorker' in navigator) navigator.serviceWorker.register('/sw.js').catch(() => {});

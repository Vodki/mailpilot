// Films the demo on the real interface (tools/e2e/run.sh video DIR): a real
// mailpilot against GreenMail, fake Telegram/Todoist/claude. Captions and
// the pointer are drawn over the page for the video only (bypassCSP: the
// page's own CSP forbids inline styles).
'use strict';
const { chromium } = require('playwright');
const { execFileSync } = require('child_process');
const path = require('path');

const BASE = 'http://127.0.0.1:8484';
const W = 1440, H = 900;
const box = (...args) => execFileSync('python3', [path.join(__dirname, 'mailbox.py'), ...args]).toString().trim();

const OVERLAY = `
(() => {
  if (document.getElementById('demo-caption')) return;
  const st = document.createElement('style');
  st.textContent = \`
    #demo-caption { position: fixed; left: 50%; bottom: 28px; transform: translateX(-50%); z-index: 2147483647;
      max-width: 1000px; padding: 14px 24px; border-radius: 14px; background: rgba(17, 22, 30, .92); color: #fff;
      font: 600 21px/1.35 system-ui, sans-serif; text-align: center; box-shadow: 0 10px 40px rgba(0,0,0,.3);
      transition: opacity .35s; pointer-events: none; }
    #demo-caption small { display: block; font-weight: 400; font-size: 15px; opacity: .8; margin-top: 4px; }
    #demo-caption.off { opacity: 0; }
    #demo-pointer { position: fixed; z-index: 2147483647; width: 22px; height: 22px; margin: -4px 0 0 -4px; pointer-events: none;
      transition: left .55s cubic-bezier(.3,.7,.3,1), top .55s cubic-bezier(.3,.7,.3,1); }
    #demo-pointer.down { transform: scale(.82); }\`;
  document.head.append(st);
  const c = document.createElement('div'); c.id = 'demo-caption'; c.className = 'off'; document.body.append(c);
  const p = document.createElement('div'); p.id = 'demo-pointer';
  p.innerHTML = '<svg viewBox="0 0 24 24" width="22" height="22"><path d="M4 2l15 9-6.5 1.5L9 19z" fill="#111" stroke="#fff" stroke-width="1.6" stroke-linejoin="round"/></svg>';
  const pos = JSON.parse(sessionStorage.getItem('demo-pointer') || '[720,450]');
  p.style.left = pos[0] + 'px'; p.style.top = pos[1] + 'px';
  document.body.append(p);
  const cap = JSON.parse(sessionStorage.getItem('demo-caption') || 'null');
  if (cap) { c.innerHTML = cap; c.className = ''; }
})();`;

(async () => {
  const dir = process.argv[2];
  const browser = await chromium.launch();
  const ctx = await browser.newContext({ viewport: { width: W, height: H }, deviceScaleFactor: 1, bypassCSP: true, recordVideo: { dir, size: { width: W, height: H } } });
  await ctx.addInitScript(`document.addEventListener('DOMContentLoaded', () => { ${OVERLAY} });`);
  const page = await ctx.newPage();
  page.setDefaultTimeout(30000);
  const wait = (ms) => page.waitForTimeout(ms);

  async function caption(title, sub) {
    const html = title ? title + (sub ? '<small>' + sub + '</small>' : '') : null;
    await page.evaluate((h) => {
      sessionStorage.setItem('demo-caption', JSON.stringify(h));
      const c = document.getElementById('demo-caption');
      if (!c) return;
      if (!h) { c.className = 'off'; return; }
      c.className = 'off';
      setTimeout(() => { c.innerHTML = h; c.className = ''; }, 200);
    }, html);
    await wait(400);
  }
  async function pointTo(locator) {
    const el = locator.first();
    await el.scrollIntoViewIfNeeded();
    const b = await el.boundingBox();
    const x = Math.round(b.x + Math.min(b.width / 2, 60)), y = Math.round(b.y + b.height / 2);
    await page.evaluate(([x, y]) => {
      sessionStorage.setItem('demo-pointer', JSON.stringify([x, y]));
      const p = document.getElementById('demo-pointer');
      if (p) { p.style.left = x + 'px'; p.style.top = y + 'px'; }
    }, [x, y]);
    await wait(650);
  }
  async function click(sel) {
    const el = typeof sel === 'string' ? page.locator(sel) : sel;
    await pointTo(el);
    await page.evaluate(() => document.getElementById('demo-pointer')?.classList.add('down'));
    await wait(120);
    await el.first().click();
    await page.evaluate(() => document.getElementById('demo-pointer')?.classList.remove('down')).catch(() => {});
  }
  async function type(sel, text) {
    await click(sel);
    await page.locator(sel).first().fill('');
    await page.locator(sel).first().pressSequentially(text, { delay: 45 });
  }

  // 1. Login.
  await page.goto(BASE + '/login');
  await caption('mailpilot', 'Ta boîte mail, augmentée par l\'IA. Ici sur un serveur de test, avec un modèle simulé.');
  await wait(1500);
  await type('#password', process.env.E2E_PASSWORD || 'demo-motdepasse-123');
  await click('button[type=submit]');
  await page.waitForSelector('.items');
  await wait(500);

  // 2. Mails arrive and get sorted live.
  await caption('Les mails arrivent…', 'mailpilot surveille la boîte en direct : chaque mail est trié et résumé par l\'IA.');
  await wait(1200);
  box('deliver', 'toiture', 'news', 'facture', 'inject');
  await page.waitForFunction(() => document.querySelectorAll('.item').length >= 4);
  await wait(1200);
  await page.waitForFunction(() => document.querySelectorAll('.item .summary:not(.muted)').length >= 4, null, { timeout: 60000 });
  await caption('Priorité, catégorie et résumé, dès la liste', 'Le mail piégé est signalé en rouge : ses instructions à l\'IA sont ignorées.');
  await pointTo(page.locator('.item:has-text("toiture")'));
  await wait(1800);
  await pointTo(page.locator('.item:has-text("Vérification")'));
  await wait(2200);

  // 3. Today.
  await caption('La page « Aujourd\'hui »', 'Ce qui compte, ce qui attend ta réponse, tes rappels Todoist.');
  await click('.nav-item:has-text("Aujourd")');
  await page.waitForSelector('.today');
  await wait(3500);

  // 4. Reading with the AI insert.
  await click('.today .item:has-text("toiture") a');
  await page.waitForSelector('.mail-subject:has-text("toiture")');
  await caption('L\'encart IA en tête du mail', 'Résumé, action à faire avec son échéance, rappel déjà ajouté à Todoist.');
  await wait(3500);

  // 5. AI reply, confirmed send.
  await caption('Répondre avec l\'IA', 'Le brouillon est rédigé en ton nom et enregistré dans Brouillons.');
  await click('#compose button:has-text("Répondre avec l\'IA")');
  await page.waitForSelector('#compose textarea[name=body]', { timeout: 60000 });
  await wait(800);
  await pointTo(page.locator('#compose textarea'));
  await wait(2200);
  await caption('Tu relis, tu modifies, tu envoies', 'Rien ne part sans une confirmation qui montre exactement quoi et à qui.');
  await click('#compose .send-form button');
  await page.waitForSelector('#confirm[open]');
  await wait(3200);
  await click('#confirm button:has-text("Oui, envoyer")');
  await page.waitForSelector('#toast:has-text("envoyée")');
  await wait(2000);

  // 6. Mailbox actions.
  await caption('Archiver, supprimer, déplacer', 'En un clic, avec « Annuler ». Rien n\'est jamais supprimé définitivement.');
  await click('.nav-item:has-text("Boîte de réception")');
  await page.waitForSelector('.item:has-text("bricoleur")');
  await wait(600);
  await click('.item:has-text("bricoleur") a');
  await page.waitForSelector('.mail-subject:has-text("bricoleur")');
  await wait(900);
  await click('.toolbar button[title^="Archiver"]');
  await page.waitForSelector('#toast button');
  await wait(2600);

  // 7. Suspect mail.
  await click('.item:has-text("Vérification") a');
  await page.waitForSelector('.alert');
  await caption('Un mail piégé reste inoffensif', 'Instructions à l\'IA ignorées, aucune réponse rédigée automatiquement.');
  await wait(3500);

  // 8. Search and « Demander à ta boîte ».
  await caption('Rechercher', 'Par mots, par expéditeur (de:), par date (avant:, après:).');
  await type('.search input', 'facture');
  await page.keyboard.press('Enter');
  await page.waitForSelector('.search-head');
  await wait(2500);
  await caption('« Demander à ta boîte »', 'Une question en français : l\'IA répond à partir de tes mails, et cite ses sources.');
  await type('.search input', 'combien je dois payer pour l\'électricité ?');
  await page.keyboard.press('Enter');
  await page.waitForSelector('.search-head');
  await wait(1000);
  await click('.ask button');
  await page.waitForSelector('.answer', { timeout: 60000 });
  await wait(3000);
  await caption('Chaque source est vérifiée et cliquable', 'L\'IA ne peut citer que des mails réellement trouvés.');
  await click('.sources a');
  await page.waitForURL(/&m=\d+/);
  await wait(3000);

  // 9. End.
  await caption('Envoi coupé par défaut · double confirmation · rien de supprimé', 'mailpilot : l\'IA propose, toi seul agis.');
  await wait(4000);
  await ctx.close();
  await browser.close();
})().catch((e) => { console.error(e); process.exit(1); });

// End-to-end tests of the web interface (tools/e2e/run.sh): a real
// mailpilot, GreenMail, fake Telegram/Todoist/claude, Chromium.
'use strict';
const { chromium, devices } = require('playwright');
const { execFileSync } = require('child_process');
const assert = require('assert/strict');
const path = require('path');

const BASE = 'http://127.0.0.1:8484';
const box = (...args) => execFileSync('python3', [path.join(__dirname, 'mailbox.py'), ...args]).toString().trim();
const unseen = () => Number(box('unseen', 'louis@example.test'));

async function step(name, fn) {
  const t = Date.now();
  await fn();
  console.log(`ok  ${name} (${Date.now() - t} ms)`);
}

(async () => {
  const browser = await chromium.launch();
  const ctx = await browser.newContext({ viewport: { width: 1440, height: 900 } });
  const page = await ctx.newPage();
  page.setDefaultTimeout(15000);
  const errors = [];
  page.on('pageerror', (e) => errors.push(String(e)));
  page.on('console', (m) => { if (m.type() === 'error') errors.push(m.text()); });
  try {
    await step('login', async () => {
      await page.goto(BASE + '/');
      await page.fill('#password', process.env.E2E_PASSWORD);
      await page.click('button[type=submit]');
      await page.waitForSelector('.items');
    });

    await step('an urgent mail shows up with its summary in under 10 s', async () => {
      await page.waitForSelector('#live.on');
      const t = Date.now();
      box('deliver', 'toiture', 'news', 'facture', 'inject');
      await page.waitForSelector('.item:has-text("toiture") .summary:has-text("couvreur")', { timeout: 10000 });
      console.log(`    roof summary after ${Date.now() - t} ms`);
      await page.waitForFunction(() => document.querySelectorAll('.item .summary:not(.muted)').length === 4, null, { timeout: 15000 });
    });

    await step('browsing changes no \\Seen flag', async () => {
      assert.equal(unseen(), 4);
      for (const v of ['urgent', 'todo', 'bills', 'suspect', 'inbox']) await page.goto(BASE + '/v/' + v);
      await page.goto(BASE + '/aujourdhui');
      await page.goto(BASE + '/recherche?q=facture');
      await page.goto(BASE + '/dossier?f=Factures');
      assert.equal(unseen(), 4);
    });

    await step('« Aujourd\'hui » page', async () => {
      await page.goto(BASE + '/aujourdhui');
      const important = page.locator('.day-sec:has(h3:has-text("Ce qui compte"))');
      assert.equal(await important.locator('.item:has-text("toiture")').count(), 1);
      assert.equal(await important.locator('.item:has-text("Vérification")').count(), 0, 'suspect mail among the important ones');
      assert.match(await page.textContent('.tiles'), /1\s*Urgents/);
    });

    await step('classic search: words and sender', async () => {
      await page.goto(BASE + '/v/inbox');
      await page.fill('.search input', 'de:energie');
      await page.press('.search input', 'Enter');
      await page.waitForSelector('.search-head');
      assert.deepEqual(await page.locator('.item .subject').allInnerTexts(), ["Facture d'électricité de novembre"]);
      await page.goto(BASE + '/recherche?q=' + encodeURIComponent('devis fenêtres'));
      assert.equal(await page.locator('.item').count(), 0, 'only mails mailpilot stored are searched');
    });

    await step('« Demander à ta boîte » cites only mails found, sources open', async () => {
      await page.goto(BASE + '/recherche?q=' + encodeURIComponent('facture électricité novembre'));
      await page.click('.ask button');
      await page.waitForSelector('.answer');
      assert.match(await page.textContent('.answer-text'), /84,20/);
      const src = page.locator('.sources a');
      assert.equal(await src.count(), 1);
      assert.match(await src.innerText(), /Énergie Service/);
      await src.click();
      await page.waitForURL(/\/recherche\?q=.*&m=\d+/);
      await page.waitForSelector('.mail .toolbar');
      // opened by Louis: marked read
      await page.waitForSelector('.item.selected:not(.unread):has-text("Facture")');
    });

    await step('opening a mail marks it read (Louis\'s own click only)', async () => {
      await page.goto(BASE + '/v/inbox');
      await page.click('.item:has-text("toiture") a');
      await page.waitForSelector('.mail-subject:has-text("toiture")');
      await page.waitForFunction(() => !document.querySelector('.item.unread .subject')?.textContent.includes('toiture'));
      // the facture was opened from the answer's source just before
      for (let i = 0; i < 20 && unseen() !== 2; i++) await page.waitForTimeout(250);
      assert.equal(unseen(), 2);
    });

    await step('AI reply: nothing leaves before the confirmation', async () => {
      await page.click('#compose button:has-text("Répondre avec l\'IA")');
      await page.waitForSelector('#compose textarea[name=body]');
      await page.click('#compose .send-form button');
      await page.waitForSelector('#confirm[open]');
      assert.match(await page.textContent('#confirm'), /pierre@example\.test/);
      assert.equal(Number(box('count', 'pierre@example.test')), 0, 'sent before the confirmation');
      await page.click('#confirm button:has-text("Oui, envoyer")');
      await page.waitForSelector('#toast:has-text("Réponse envoyée")');
      for (let i = 0; i < 20 && Number(box('count', 'pierre@example.test')) === 0; i++) await page.waitForTimeout(250);
      assert.equal(Number(box('count', 'pierre@example.test')), 1);
    });

    await step('suspect mail: no AI reply offered', async () => {
      await page.click('.item:has-text("Vérification") a');
      await page.waitForSelector('.mail-subject:has-text("Vérification")');
      await page.waitForSelector('.alert');
      assert.equal(await page.locator('#compose button:has-text("Répondre avec")').count(), 0);
    });

    await step('archive, then undo', async () => {
      await page.click('.item:has-text("bricoleur") a');
      await page.waitForSelector('.mail-subject:has-text("bricoleur")');
      await page.click('.toolbar button[title^="Archiver"]');
      await page.waitForSelector('#toast button:has-text("Annuler")');
      assert.equal(await page.locator('.item:has-text("bricoleur")').count(), 0);
      await page.click('#toast button');
      await page.waitForSelector('.item:has-text("bricoleur")');
    });

    await step('phone: no horizontal scroll', async () => {
      const phone = await browser.newContext({ ...devices['iPhone 13'] });
      await phone.addCookies(await ctx.cookies());
      const p = await phone.newPage();
      p.on('pageerror', (e) => errors.push('phone: ' + String(e)));
      for (const u of ['/aujourdhui', '/v/inbox', '/recherche?q=facture']) {
        await p.goto(BASE + u);
        const over = await p.evaluate(() => document.documentElement.scrollWidth - window.innerWidth);
        assert.ok(over <= 0, `${u}: ${over}px of horizontal scroll`);
      }
      await phone.close();
    });

    assert.deepEqual(errors, [], 'errors in the page');
    console.log('all end-to-end tests passed');
  } catch (e) {
    await page.screenshot({ path: 'e2e-failure.png' }).catch(() => {});
    console.error(e);
    process.exitCode = 1;
  } finally {
    await browser.close();
  }
})();

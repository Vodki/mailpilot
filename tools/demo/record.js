const { chromium } = require('playwright');
(async () => {
  const dir = process.argv[2];
  const browser = await chromium.launch();
  const ctx = await browser.newContext({ viewport: { width: 1600, height: 900 }, recordVideo: { dir, size: { width: 1600, height: 900 } } });
  const page = await ctx.newPage();
  await page.goto('http://localhost:8080/');
  await page.waitForFunction(() => window.demoDone === true, null, { timeout: 600000, polling: 500 });
  await ctx.close(); await browser.close();
})();

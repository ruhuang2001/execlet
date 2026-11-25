const assert = require('node:assert/strict');
const { createHash } = require('node:crypto');
const { chromium } = require('playwright-core');

async function main() {
  const browser = await chromium.launch({ chromiumSandbox: false, timeout: 15_000 });
  try {
    const page = await browser.newPage({ viewport: { width: 800, height: 480 }, deviceScaleFactor: 1 });
    page.setDefaultTimeout(10_000);
    await page.goto('file:///tmp/execlet-playwright.html');
    const title = await page.title();
    const heading = await page.locator('h1').textContent();
    assert.equal(title, 'Execlet browser QA');
    assert.equal(heading, 'Execlet browser check');

    const png = await page.screenshot({ path: '/tmp/execlet-playwright.png', type: 'png' });
    console.log(JSON.stringify({
      browser: 'chromium', version: browser.version(), title, heading,
      bytes: png.length, sha256: createHash('sha256').update(png).digest('hex'),
    }));
  } finally {
    await browser.close();
  }
}

main().catch(error => {
  console.error(error);
  process.exitCode = 1;
});

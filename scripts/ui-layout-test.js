#!/usr/bin/env node
// Checks that the device table fits the page instead of growing a horizontal
// scrollbar. This has regressed twice, both times because a change added width
// to a row without anyone measuring the result, so it is asserted here rather
// than eyeballed.
//
//   npm install playwright && npx playwright install chromium
//   node scripts/ui-layout-test.js [screenshot.png]
//
// Set PLAYWRIGHT_MODULE to point at an existing Playwright install instead:
//   PLAYWRIGHT_MODULE=/path/to/node_modules/playwright node scripts/ui-layout-test.js
//
// The page serves its own stub API, so no agent needs to be running.

const http = require('http');
const fs = require('fs');
const path = require('path');

const playwright = require(process.env.PLAYWRIGHT_MODULE || 'playwright');
const WEB = path.join(__dirname, '..', 'web');

// The widest row the UI can produce: monitored (so it offers Release port) and
// busy (so it also offers End session), giving three action buttons at once.
const device = (id, uuid, port, busy) => ({
  id, uuid, class: 'serial', online: true, busy, exposed: true,
  claimed_by: busy ? 'e3f84311' : '',
  meta: {
    path: port, transport: 'usb', product: `USB Serial Device (${port})`,
    fingerprint_confidence: 'weak', control_lines_allowed: true,
    assert_lines_on_open: false, monitored: true, port_held: true, resets: 0,
  },
});

const API = {
  '/api/status': {
    connected: true, relay_url: 'ws://127.0.0.1:8443/ws', mode: 'ssh',
    ssh: { enabled: true, target: 'root@relay.example.com:22' }, stopped: false,
  },
  '/api/devices': [
    device('fluent-hill', 'u1', 'COM3', false),
    device('roaming-pheasant', 'u2', 'COM4', true),
  ],
  '/api/sessions': [{
    device_id: 'roaming-pheasant', session_id: 'e3f84311',
    bytes_in: 1329, bytes_out: 0, opened: '2026-01-01T12:00:00Z',
  }],
  '/api/activity': [],
};

// Below this the table is allowed its own scrollbar: nine columns do not fit a
// narrow window without hiding data, which is what .tablewrap is for. The page
// itself must never scroll sideways at any width.
const DESKTOP_MIN = 1050;
// At a normal desktop width the action buttons belong on one line. Wrapping is
// the graceful fallback for narrower windows, not the intended look.
const ONE_LINE_MIN = 1300;

const WIDTHS = [1440, 1378, 1200, 1050, 900, 760];

function serve() {
  return http.createServer((req, res) => {
    const url = req.url.split('?')[0];
    if (API[url] !== undefined) {
      res.writeHead(200, { 'Content-Type': 'application/json' });
      return res.end(JSON.stringify(API[url]));
    }
    const file = path.join(WEB, url === '/' ? 'index.html' : url);
    if (!file.startsWith(WEB) || !fs.existsSync(file)) {
      res.writeHead(404);
      return res.end();
    }
    const type = file.endsWith('.js') ? 'text/javascript'
      : file.endsWith('.css') ? 'text/css' : 'text/html';
    res.writeHead(200, { 'Content-Type': type });
    res.end(fs.readFileSync(file));
  });
}

async function measure(page) {
  return page.evaluate(() => {
    const wrap = document.querySelector('.tablewrap');
    const rows = [...document.querySelectorAll('tbody tr')].map((tr) => {
      const buttons = [...tr.querySelectorAll('td.actions button')];
      // Cluster button tops rather than comparing them exactly: a bordered
      // .ghost button sits a pixel off its neighbours on the same line.
      const tops = buttons.map(b => b.getBoundingClientRect().top).sort((a, z) => a - z);
      let lines = tops.length ? 1 : 0;
      for (let i = 1; i < tops.length; i++) if (tops[i] - tops[i - 1] > 8) lines++;
      return {
        id: tr.querySelector('td').textContent.trim(),
        buttons: buttons.length,
        lines,
      };
    });
    const doc = document.documentElement;
    return {
      tableOverflow: wrap.scrollWidth - wrap.clientWidth,
      pageOverflow: doc.scrollWidth - doc.clientWidth,
      rows,
    };
  });
}

(async () => {
  const server = serve();
  await new Promise(r => server.listen(0, '127.0.0.1', r));
  const url = `http://127.0.0.1:${server.address().port}/`;
  const browser = await playwright.chromium.launch();
  const failures = [];

  for (const width of WIDTHS) {
    const page = await browser.newPage({ viewport: { width, height: 1000 } });
    await page.goto(url, { waitUntil: 'networkidle' });
    await page.waitForSelector('tbody tr', { timeout: 5000 });
    const m = await measure(page);

    if (m.pageOverflow > 1) {
      failures.push(`${width}px: the page scrolls sideways by ${m.pageOverflow}px`);
    }
    if (width >= DESKTOP_MIN && m.tableOverflow > 1) {
      failures.push(`${width}px: the device table scrolls sideways by ${m.tableOverflow}px`);
    }
    if (width >= ONE_LINE_MIN) {
      for (const r of m.rows) {
        if (r.lines > 1) {
          failures.push(`${width}px: ${r.id}'s ${r.buttons} buttons wrapped onto ${r.lines} lines`);
        }
      }
    }

    const table = m.tableOverflow > 1 ? `scrolls ${m.tableOverflow}px` : 'fits';
    const shape = m.rows.map(r => `${r.buttons}btn/${r.lines}line`).join(' ');
    console.log(`${String(width).padStart(4)}px  table ${table.padEnd(13)} rows ${shape}`);

    if (process.argv[2] && width === 1378) {
      await page.screenshot({ path: process.argv[2], fullPage: true });
    }
    await page.close();
  }

  await browser.close();
  server.close();

  if (failures.length) {
    console.error('\nFAIL');
    for (const f of failures) console.error('  ' + f);
    process.exit(1);
  }
  console.log('\nPASS: no page overflow anywhere, table fits at >=' + DESKTOP_MIN +
    'px, buttons on one line at >=' + ONE_LINE_MIN + 'px');
})();

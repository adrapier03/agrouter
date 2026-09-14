#!/usr/bin/env node
/**
 * Antigravity GSuite OAuth → agrouter
 *
 * Flow per akun (email|password dari accounts.txt):
 *   1. Start loopback HTTP server di 127.0.0.1:14451 (redirect_uri)
 *   2. Buka Google OAuth consent (client antigravity, PKCE S256)
 *   3. Login GSuite: email → password → workspace terms → consent allow
 *   4. Tangkap authorization code dari redirect → exchange token
 *   5. loadCodeAssist mode:1 → projectId
 *   6. POST ke agrouter /admin/accounts
 *
 * Usage: node ag_gsuite.js [--headless] [-n 3] [--file accounts.txt]
 */
const http = require('http');
const crypto = require('crypto');
const fs = require('fs');
const path = require('path');
let _cloak = null;
async function getCloak() { if (!_cloak) _cloak = await import('cloakbrowser'); return _cloak; }

const CONFIG = {
  CLIENT_ID: process.env.AG_CLIENT_ID || ('1071006060591-tmhssin2h21lcre235vtolojh4g403ep' + '.' + 'apps.googleusercontent.com'),
  CLIENT_SECRET: process.env.AG_CLIENT_SECRET || ('GOCSPX-' + 'K58FWR486LdLJ1mLB8sXC4z6qDAf'),
  SCOPES: 'https://www.googleapis.com/auth/cloud-platform https://www.googleapis.com/auth/userinfo.email https://www.googleapis.com/auth/userinfo.profile https://www.googleapis.com/auth/cclog https://www.googleapis.com/auth/experimentsandconfigs',
  REDIRECT_PORT: 14451,
  REDIRECT_URI: 'http://127.0.0.1:14451/callback',
  AGROUTER: 'http://127.0.0.1:20129',
  OUT_JSON: path.join(__dirname, 'ag_accounts.json'),
};

function sleep(ms) { return new Promise(r => setTimeout(r, ms)); }
function log(i, msg) { const ts = new Date().toISOString().slice(11, 19); console.log(`[${ts}] [#${i}] ${msg}`); }

function loadAccounts(customFile) {
  const p = path.resolve(__dirname, customFile || 'accounts.txt');
  return fs.readFileSync(p, 'utf-8').split(/\r?\n/).map(l => l.trim()).filter(l => l && !l.startsWith('#')).map(l => {
    let email = '', password = '';
    if (l.includes('|')) {
      const parts = l.split('|');
      email = parts[0]; password = parts[1];
    } else if (l.includes(':')) {
      const parts = l.split(':');
      email = parts[0]; password = parts[1];
    } else if (l.includes('\t')) {
      const parts = l.split('\t');
      email = parts[0]; password = parts[1];
    }
    return { email: (email || '').trim(), password: (password || '').trim() };
  }).filter(a => a.email && a.password);
}

function loadJson() { try { return JSON.parse(fs.readFileSync(CONFIG.OUT_JSON, 'utf-8')); } catch { return []; } }
function saveResult(rec) {
  const all = loadJson(); all.push(rec);
  fs.writeFileSync(CONFIG.OUT_JSON, JSON.stringify(all, null, 2));
  fs.appendFileSync(path.join(__dirname, 'result.txt'), `${rec.email}:${rec.status}:${rec.refreshToken ? 'token-ok' : 'no-token'}\n`);
}

// PKCE
function pkce() {
  const verifier = crypto.randomBytes(48).toString('base64url');
  const challenge = crypto.createHash('sha256').update(verifier).digest('base64url');
  return { verifier, challenge };
}

// Wait until fn() true, with deadline
async function until(fn, ms, step = 700) {
  const end = Date.now() + ms;
  while (Date.now() < end) {
    if (await fn()) return true;
    await sleep(step);
  }
  return false;
}

async function clickAny(target, texts, label, timeout = 5000) {
  for (const t of texts) {
    const loc = target.getByRole('button', { name: t }).or(target.locator(`button:has-text("${t}"), [role="button"]:has-text("${t}"), div[role="link"]:has-text("${t}")`)).first();
    if (await loc.isVisible({ timeout: Math.min(timeout, 1200) }).catch(() => false)) {
      await loc.click().catch(() => {});
      log('-', `clicked ${label} (${t})`);
      return true;
    }
  }
  return false;
}

async function fillAny(target, labels, value, timeout = 5000) {
  for (const t of labels) {
    const loc = target.locator(`input[aria-label="${t}"]`).first();
    if (await loc.isVisible({ timeout: Math.min(timeout, 1200) }).catch(() => false)) {
      await loc.fill(value);
      return true;
    }
  }
  const anyInput = target.locator('input[type="email"], input[type="password"], input[name="identifier"], input[name="Passwd"]').first();
  if (await anyInput.isVisible({ timeout: 1500 }).catch(() => false)) {
    await anyInput.fill(value);
    return true;
  }
  return false;
}

let activeServer = null;

// Loopback capture: start server, resolve with the auth code
function waitForCode(timeoutMs) {
  return new Promise((resolve, reject) => {
    if (activeServer) {
      try { activeServer.close(); } catch {}
      activeServer = null;
    }
    const srv = http.createServer((req, res) => {
      const u = new URL(req.url, `http://127.0.0.1:${CONFIG.REDIRECT_PORT}`);
      if (u.pathname === '/callback') {
        const code = u.searchParams.get('code');
        const err = u.searchParams.get('error');
        res.writeHead(200, { 'Content-Type': 'text/html; charset=utf-8' });
        res.end(code ? '<h2>OK — akun berhasil dihubungkan ke agrouter!</h2>' : `<h2>Error: ${err}</h2>`);
        setTimeout(() => {
          try { srv.close(); } catch {}
          if (activeServer === srv) activeServer = null;
          code ? resolve(code) : reject(new Error('oauth error: ' + err));
        }, 300);
      } else { res.writeHead(404); res.end(); }
    });
    activeServer = srv;
    srv.on('error', (e) => {
      if (activeServer === srv) activeServer = null;
      reject(e);
    });
    srv.listen(CONFIG.REDIRECT_PORT, '127.0.0.1', () => {});
    setTimeout(() => {
      try { srv.close(); } catch {}
      if (activeServer === srv) activeServer = null;
      reject(new Error('timeout waiting oauth redirect'));
    }, timeoutMs);
  });
}

async function googleLogin(page, acc, idx) {
  // force English
  const url = new URL(page.url());
  if (url.searchParams.get('hl') !== 'en') {
    url.searchParams.set('hl', 'en');
    await page.goto(url.toString(), { waitUntil: 'domcontentloaded' }).catch(() => {});
    await sleep(2500);
  }

  log(idx, '[google] email');
  if (!(await fillAny(page, ['Email or phone', 'Email'], acc.email, 8000)))
    throw new Error('email field not found');
  if (!(await clickAny(page, ['Next', 'Berikutnya'], 'next-email', 5000)))
    await page.keyboard.press('Enter');
  await sleep(3500);

  log(idx, `[google] url after email: ${page.url().slice(0, 70)}`);
  // password page can take a while; poll up to ~20s
  let pwFilled = false;
  for (let i = 0; i < 14 && !pwFilled; i++) {
    pwFilled = await fillAny(page, ['Enter your password', 'Password'], acc.password, 1500);
    if (!pwFilled) await sleep(1200);
  }
  if (!pwFilled) throw new Error('password field not found');
  await page.keyboard.press('Enter');
  await sleep(4500);

  // Interstitial "Make sure that you downloaded this app from Google" → click Sign in
  {
    const si = page.getByRole('button', { name: 'Sign in' }).first();
    if (await si.isVisible({ timeout: 5000 }).catch(() => false)) {
      log(idx, '[google] interstitial: clicking Sign in');
      await si.click().catch(() => {});
      await sleep(5000);
    }
  }

  // error patterns (read DOM, not screenshots)
  const bodyText = (await page.evaluate(() => document.body.innerText).catch(() => '')).slice(0, 3000);
  for (const [pat, why] of [
    [/account (was )?deleted|recently deleted/i, 'account deleted'],
    [/wrong password|incorrect password/i, 'wrong password'],
    [/couldn.?t sign you in|unusual traffic/i, 'bot detection'],
    [/verify it.?s you|2-step verification/i, '2FA required'],
  ]) {
    if (pat.test(bodyText)) throw new Error('google: ' + why);
  }

  await clickAny(page, ['I understand', 'Saya mengerti'], 'workspace terms', 3000);

  log(idx, `[google] url after pw: ${page.url().slice(0, 70)}`);
  log(idx, '[google] consent screen');
  await until(async () => /oauth|consent|nativeapp|signin|selectaccount|myaccount|14451/i.test(page.url()), 15000);
  await sleep(2000);
  // consent chain: interstitial Sign in -> Continue/Allow (poll sampai redirect ke loopback)
  for (let i = 0; i < 10; i++) {
    const done = await until(async () => /14451|code=/.test(page.url()), 1500);
    if (done) break;
    // nativeapp interstitial: tombolnya DIV#submit_approve_access (bukan role button)
    let clicked = false;
    try {
      const si = page.locator('#submit_approve_access, button:has-text("Sign in"), a:has-text("Sign in")').first();
      if (await si.isVisible({ timeout: 1500 }).catch(() => false)) {
        await si.click();
        clicked = true;
        log(idx, `[consent ${i}] clicked interstitial Sign in`);
        await sleep(3500);
      }
    } catch {}
    if (!clicked) {
      clicked = await clickAny(page, ['Continue', 'Allow', 'Allow all', 'Lanjutkan', 'Izinkan'], 'consent', 2500);
      if (clicked) log(idx, `[consent ${i}] clicked consent`);
    }
    if (!clicked) await sleep(2000);
  }
}

async function processAccount(idx, acc, headless) {
  log(idx, `=== ${acc.email} ===`);
  const { launchContext } = await getCloak();
  const fpSeed = 10000 + Math.floor(Math.random() * 90000);
  const { verifier, challenge } = pkce();
  const state = crypto.randomBytes(12).toString('hex');

  const authUrl = 'https://accounts.google.com/o/oauth2/v2/auth?' + new URLSearchParams({
    client_id: CONFIG.CLIENT_ID,
    response_type: 'code',
    redirect_uri: CONFIG.REDIRECT_URI,
    scope: CONFIG.SCOPES,
    state,
    access_type: 'offline',
    prompt: 'consent',
    code_challenge: challenge,
    code_challenge_method: 'S256',
  }).toString();

  const codePromise = waitForCode(180000);
  const context = await launchContext({
    headless,
    humanize: true,
    stealthArgs: true,
    locale: 'en-US',
    args: ['--no-sandbox', '--disable-dev-shm-usage', `--fingerprint=${fpSeed}`, '--fingerprint-platform=windows'],
  });
  const page = await context.newPage();

  try {
    await page.goto(authUrl, { waitUntil: 'domcontentloaded', timeout: 30000 });
    await sleep(2500);
    await googleLogin(page, acc, idx);

    log(idx, `[google] url after consent chain: ${page.url().slice(0, 90)}`);
  log(idx, '[oauth] waiting redirect with code...');
    const code = await codePromise;
    log(idx, '[oauth] code captured, exchanging...');
    const tr = await fetch('https://oauth2.googleapis.com/token', {
      method: 'POST',
      headers: { 'Content-Type': 'application/x-www-form-urlencoded', Accept: 'application/json' },
      body: new URLSearchParams({
        grant_type: 'authorization_code',
        client_id: CONFIG.CLIENT_ID,
        client_secret: CONFIG.CLIENT_SECRET,
        code,
        redirect_uri: CONFIG.REDIRECT_URI,
        code_verifier: verifier,
      }),
    });
    if (!tr.ok) throw new Error('token exchange: ' + (await tr.text()).slice(0, 200));
    const tok = await tr.json();
    if (!tok.refresh_token) throw new Error('no refresh_token in response (offline access denied?)');

    // projectId via loadCodeAssist
    let projectId = '';
    try {
      const lca = await fetch('https://cloudcode-pa.googleapis.com/v1internal:loadCodeAssist', {
        method: 'POST',
        headers: { Authorization: `Bearer ${tok.access_token}`, 'Content-Type': 'application/json', 'User-Agent': 'antigravity/ide/2.1.1 darwin/arm64' },
        body: JSON.stringify({ metadata: { ideType: 'IDE_UNSPECIFIED', platform: 'PLATFORM_UNSPECIFIED', pluginType: 'GEMINI' }, mode: 1 }),
      });
      if (lca.ok) {
        const j = await lca.json();
        const cp = j.cloudaicompanionProject;
        projectId = typeof cp === 'object' ? (cp?.id || '') : (cp || '');
      }
    } catch (e) { log(idx, `[!] projectId: ${e.message}`); }

    // register to agrouter
    const adminTok = process.env.AGROUTER_ADMIN_TOKEN || (() => {
      try { return fs.readFileSync('/root/agrouter-data/admin-token.txt', 'utf-8').trim(); } catch { return '2a86013935fb7166a07556070f6f77f57df2f778802ca6d2'; }
    })();
    const reg = await fetch(`${CONFIG.AGROUTER}/admin/accounts`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', 'Authorization': `Bearer ${adminTok}` },
      body: JSON.stringify({ email: acc.email, refreshToken: tok.refresh_token, projectId }),
    });
    if (!reg.ok) throw new Error('agrouter register: ' + (await reg.text()).slice(0, 120));
    const regj = await reg.json();

    log(idx, `OK id=${regj.id} projectId=${projectId}`);
    saveResult({ email: acc.email, status: 'ok', refreshToken: tok.refresh_token, projectId, agrouterId: regj.id, completedAt: new Date().toISOString() });
    return true;
  } catch (e) {
    log(idx, `FAIL: ${e.message}`);
    saveResult({ email: acc.email, status: 'fail', error: e.message, completedAt: new Date().toISOString() });
    return false;
  } finally {
    if (activeServer) {
      try { activeServer.close(); } catch {}
      activeServer = null;
    }
    await context.close().catch(() => {});
  }
}

process.on('SIGTERM', () => {
  if (activeServer) { try { activeServer.close(); } catch {} activeServer = null; }
  process.exit(0);
});
process.on('SIGINT', () => {
  if (activeServer) { try { activeServer.close(); } catch {} activeServer = null; }
  process.exit(0);
});

(async () => {
  const args = process.argv.slice(2);
  const headless = !args.includes('--headful');
  let limit = Infinity, file = null;
  for (let i = 0; i < args.length; i++) {
    if (args[i] === '-n') limit = parseInt(args[i + 1]) || Infinity;
    if (args[i] === '--file') file = args[i + 1];
  }
  const accounts = loadAccounts(file).slice(0, limit);
  console.log(`antigravity gsuite oauth — ${accounts.length} akun, headless=${headless}`);
  let okCount = 0, failCount = 0;
  for (let i = 0; i < accounts.length; i++) {
    const ok = await processAccount(i + 1, accounts[i], headless);
    if (ok) okCount++; else failCount++;
    if (i < accounts.length - 1) await sleep(4000 + Math.random() * 4000);
  }
  console.log(`\nSELESAI: ${okCount} ok / ${failCount} fail`);
})();

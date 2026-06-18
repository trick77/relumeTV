const app = document.getElementById("app");

// Convert a CIE xy colour to an approximate sRGB string for the tile. Brightness
// is intentionally NOT used: the swatch shows the lamp's colour only. We fix the
// luminance and normalise to the brightest channel so every hue renders at full,
// consistent brightness regardless of how dim the lamp actually is.
function xyToRGB(x, y) {
  if (!y) return "#1c1f28";
  const Y = 1;
  const X = (Y / y) * x;
  const Z = (Y / y) * (1 - x - y);
  let r = X * 1.656492 - Y * 0.354851 - Z * 0.255038;
  let g = -X * 0.707196 + Y * 1.655397 + Z * 0.036152;
  let b = X * 0.051713 - Y * 0.121364 + Z * 1.011530;
  // Drop out-of-gamut negatives, then scale to full brightness so only the hue
  // matters — the lamp's brightness must not dim the swatch.
  r = Math.max(r, 0);
  g = Math.max(g, 0);
  b = Math.max(b, 0);
  const max = Math.max(r, g, b, 1e-6);
  r /= max;
  g /= max;
  b /= max;
  const gamma = (c) => (c <= 0.0031308 ? 12.92 * c : 1.055 * Math.pow(c, 1 / 2.4) - 0.055);
  [r, g, b] = [gamma(r), gamma(g), gamma(b)].map((c) => Math.round(Math.min(Math.max(c, 0), 1) * 255));
  return `rgb(${r},${g},${b})`;
}

const esc = (s) =>
  String(s ?? "").replace(/[&<>"]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c]));

function healthLabel(h) {
  return {
    "streaming-pro": "Active · streaming to Pro",
    "entertainment-fallback": "Active · entertainment fallback → REST",
    "active-rest": "Active",
    "idle": "Idle · TV not driving",
    "no-tv": "Waiting for TV pairing",
    "unpaired-pro": "Hue Bridge Pro not paired",
  }[h] || h;
}

// healthDotClass colours the status dot: green when actively driving the lights,
// amber for a degraded fallback or anything needing attention.
function healthDotClass(h) {
  if (h === "streaming-pro" || h === "active-rest") return "dot ok pulse";
  if (h === "entertainment-fallback") return "dot pulse"; // amber: degraded (DTLS failed → REST)
  if (h === "idle") return "dot pulse"; // amber standby
  return "dot pulse"; // amber: needs attention (no-tv / unpaired-pro)
}

// currentMode is the path relume is forwarding on RIGHT NOW, not the configured
// startup mode. The TV only drives over entertainment/DTLS while its stream is
// actually up; in every other case (rest mode, fallback, or entertainment
// configured but the TV not streaming) the live path is REST.
function currentMode(s) {
  return s.dtlsStreamUp ? "entertainment" : "rest";
}

// modeSub describes the active forward path under the MODE label. It must never
// read as a contradiction: in entertainment mode without a DTLS stream, say
// explicitly whether that is a fallback (DTLS failed) or simply the TV not
// streaming entertainment at all.
function modeSub(s) {
  if (s.dtlsStreamUp) return "DTLS stream up";
  if (s.fallback) return "fallback to REST (DTLS unavailable)";
  if (s.mode === "entertainment") return "REST · TV not streaming entertainment";
  return "REST";
}

// cap upper-cases the first letter for display (e.g. the mode label), without
// touching the underlying lower-case value relume uses internally.
function cap(str) {
  return str ? str.charAt(0).toUpperCase() + str.slice(1) : str;
}

// _startedAtMs holds relume's start time (ms epoch) so the uptime can tick every
// second between snapshot pushes. fmtUptime renders only the largest unit: days
// once uptime reaches a day, hours past an hour, otherwise minutes/seconds.
let _startedAtMs = null;
function fmtUptime(ms) {
  if (!(ms >= 0)) return "";
  const s = Math.floor(ms / 1000);
  const d = Math.floor(s / 86400);
  const h = Math.floor((s % 86400) / 3600);
  const m = Math.floor((s % 3600) / 60);
  const sec = s % 60;
  if (d > 0) return `${d}d`;
  if (h > 0) return `${h}h`;
  if (m > 0) return `${m}m`;
  return `${sec}s`;
}
function tickUptime() {
  const el = document.getElementById("uptime");
  if (el && _startedAtMs) el.textContent = "↑ " + fmtUptime(Date.now() - _startedAtMs);
}

function renderSetup(s) {
  const proPill = s.proPaired ? `<span class="pill ok">done</span>` : `<span class="pill wait">waiting</span>`;
  const tvPill = s.tvClients.length ? `<span class="pill ok">done</span>` : `<span class="pill wait">waiting</span>`;
  app.innerHTML = `
    <div class="wrap">
      <div class="top"><div class="brand">re<span>lume</span></div><div class="ver">v${esc(s.version)} · first run</div></div>
      <p class="lead">Three steps until your Ambilight TV drives the Hue Bridge Pro again.</p>
      <div class="steps">
        <div class="step ${s.proPaired ? "done" : "active"}">
          <div class="rail"><div class="num">${s.proPaired ? "✓" : "1"}</div><div class="line"></div></div>
          <div class="card"><h3>Pair Hue Bridge Pro ${proPill}</h3>
            <div class="d">${
              s.proPaired
                ? `${esc(s.proName)} · ${esc(s.proHost)} · certificate pinned`
                : "Briefly press the link button on the Hue Bridge Pro."
            }</div></div>
        </div>
        <div class="step ${s.proPaired ? (s.tvClients.length ? "done" : "active") : "todo"}">
          <div class="rail"><div class="num">${s.tvClients.length ? "✓" : "2"}</div><div class="line"></div></div>
          <div class="card"><h3>Connect your TV ${tvPill}</h3>
            <div class="d">On the TV, start the Ambilight+Hue bridge search and pick relume. Advertised as “${esc(s.bridgeName)}”.</div>
            ${
              !s.tvClients.length
                ? `<div class="action"><span class="dot pulse"></span><div><div class="big">Waiting for TV search…</div></div></div>`
                : ""
            }</div>
        </div>
        <div class="step ${s.tvClients.length ? "active" : "todo"}">
          <div class="rail"><div class="num">3</div></div>
          <div class="card"><h3>Check lights &amp; go</h3>
            <div class="d">${s.lights.length} lights loaded from the Pro.</div>
            <div style="margin-top:12px"><button class="btn primary" onclick="flash()">Test flash</button></div></div>
        </div>
      </div>
    </div>`;
}

function renderDashboard(s) {
  // Show only the lamps the TV is actively driving. Fall back to all lamps
  // while nothing is driven yet (cold start / TV idle before first stream).
  // Note: driven is sticky — a lamp stays driven once seen this run (the backend
  // never un-drives), so this is effectively "ever driven since start", retained
  // across stream pauses. A shrunk entertainment area clears only on restart.
  const drivenLights = s.lights.filter((l) => l.driven);
  const shown = drivenLights.length > 0 ? drivenLights : s.lights;
  const lights = shown
    .map((l) => {
      const col = l.on ? xyToRGB(l.x, l.y) : "";
      return `<div class="lamp ${l.driven ? "driven" : ""} ${l.on ? "" : "off"}">
        <div class="swatch" style="${l.on ? `background:${col};box-shadow:0 0 20px ${col}` : ""}"></div>
        <div class="nm">${esc(l.name)}</div>
        <div class="st">${l.on ? `<span class="ok">on</span>` : "off"}</div></div>`;
    })
    .join("");
  const driven = drivenLights.length;
  const pending =
    !s.proPaired || s.pendingTV
      ? `<div class="card pending"><h3>⚠ Needs attention</h3>
          ${
            !s.proPaired
              ? `<div class="pendrow"><div class="info"><b>Hue Bridge Pro pairing</b><div>Press the link button on the Pro</div></div><span class="dot pulse"></span></div>`
              : ""
          }
          ${
            s.pendingTV
              ? `<div class="pendrow"><div class="info"><b>TV is pairing…</b><div>Auto-accepting</div></div><span class="dot pulse"></span></div>`
              : ""
          }
        </div>`
      : "";
  app.innerHTML = `
    <div class="wrap">
      <div class="top"><div class="brand">re<span>lume</span></div><div class="ver">v${esc(s.version)}</div>
        <div class="spacer"></div><div class="health"><span class="${healthDotClass(s.health)}"></span> ${esc(healthLabel(s.health))}</div></div>
      <div class="pipe">
        <div class="step"><div class="lbl">Hue Bridge Pro</div><div class="val">${s.proPaired ? `<span class="ok">✓</span> Paired` : "— Unpaired"}${s.startedAt ? ` <span class="up" id="uptime">↑ ${esc(fmtUptime(Date.now() - Date.parse(s.startedAt)))}</span>` : ""}</div><div class="sub">${esc(s.proName)} ${esc(s.proHost)}</div></div>
        <div class="step"><div class="lbl">TV pairing</div><div class="val">${s.tvClients.length} client(s)</div><div class="sub">${esc(s.tvClients.join(", "))}</div></div>
        <div class="step"><div class="lbl">Mode <span class="info" title="Entertainment: low-latency DTLS stream to the Hue Bridge Pro (default). REST: per-light REST writes — the automatic fallback when the TV is not streaming entertainment.">i</span></div><div class="val">${esc(cap(currentMode(s)))}${s.fallback ? " (fallback)" : ""}</div><div class="sub">${esc(modeSub(s))}</div></div>
        <div class="step"><div class="lbl">Lights</div><div class="val">${s.lights.length}</div><div class="sub">${driven} driven by TV</div></div>
      </div>
      <div class="grid">
        <div class="card"><h3>Lights <span class="cnt">${shown.length} shown · ${driven} driven</span></h3><div class="lights">${lights}</div></div>
        <div class="side">${pending}
          <div class="card"><h3>Actions</h3><button class="btn primary" onclick="flash()">Test flash</button></div>
        </div>
      </div>
      <div class="card log"><h3>Live events</h3><div id="log"></div></div>
    </div>`;
}

let logLines = [];
const logRow = (e) =>
  `<div class="logrow"><span class="ts">${esc((e.time || "").slice(11, 19))}</span><span class="tag">${esc(e.level)}</span><span class="msg">${esc(e.msg)}</span></div>`;

function render(s) {
  _startedAtMs = s.startedAt ? Date.parse(s.startedAt) : null;
  if (s.proPaired && s.tvClients.length > 0) renderDashboard(s);
  else renderSetup(s);
  tickUptime();
  const logEl = document.getElementById("log");
  if (logEl) logEl.innerHTML = logLines.map(logRow).join("");
}

function pushLog(e) {
  logLines.unshift(e);
  if (logLines.length > 100) logLines.pop();
  const logEl = document.getElementById("log");
  if (logEl) logEl.innerHTML = logLines.map(logRow).join("");
}

async function flash() {
  try {
    await fetch("/api/actions/flash", { method: "POST" });
  } catch (_) {}
}
window.flash = flash;

async function boot() {
  try {
    const s = await (await fetch("/api/state")).json();
    render(s);
  } catch (_) {}
  // Tick the uptime every second between snapshot pushes.
  setInterval(tickUptime, 1000);
  const es = new EventSource("/api/events");
  es.onmessage = (msg) => {
    const f = JSON.parse(msg.data);
    if (f.kind === "snapshot") render(f.snapshot);
    else if (f.kind === "event") pushLog(f.event);
  };
}
boot();

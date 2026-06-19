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
// amber for a degraded fallback or anything needing attention. The healthy green
// state is steady (no pulse) — a constant glow reads as calm; only attention
// states pulse to draw the eye.
function healthDotClass(h) {
  if (h === "streaming-pro" || h === "active-rest") return "dot ok"; // steady green
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

// mountDashboard builds the dashboard skeleton exactly once. Every value that
// changes between snapshots lives in a stable, id-addressed node so update()
// can write it in place — never via innerHTML on #app. Rebuilding the whole
// tree each second (the old approach) reset CSS animations and killed
// transitions, which read as flicker; mount-once + in-place update is smooth.
function mountDashboard(s) {
  // Detached lamp nodes from a previous mount are stale — start fresh.
  lampEls = new Map();
  lastShownIds = null;
  lastPendSig = null;
  app.innerHTML = `
    <div class="wrap">
      <div class="top"><div class="brand">re<span>lume</span></div><div class="ver">v${esc(s.version)}</div>
        <div class="spacer"></div><div class="health"><span id="health-dot" class="dot"></span> <span id="health-label"></span></div></div>
      <div class="pipe">
        <div class="step"><div class="lbl">Hue Bridge Pro</div><div class="val"><span id="pro-paired"></span>${s.startedAt ? ` <span class="up" id="uptime"></span>` : ""}</div><div class="sub" id="pro-sub"></div></div>
        <div class="step"><div class="lbl">TV pairing</div><div class="val" id="tv-val"></div><div class="sub" id="tv-sub"></div></div>
        <div class="step"><div class="lbl">Mode <span class="info" title="Entertainment: low-latency DTLS stream to the Hue Bridge Pro (default). REST: per-light REST writes — the automatic fallback when the TV is not streaming entertainment.">i</span></div><div class="val" id="mode-val"></div><div class="sub" id="mode-sub"></div></div>
        <div class="step"><div class="lbl">Lights</div><div class="val" id="lights-val"></div><div class="sub" id="lights-sub"></div></div>
      </div>
      <div class="grid">
        <div class="card"><h3>Lights <span class="cnt" id="lights-cnt"></span></h3><div class="lights" id="lights"></div></div>
        <div class="side"><div id="pending"></div>
          <div class="card"><h3>Actions</h3><button class="btn primary" onclick="flash()">Test flash</button></div>
        </div>
      </div>
      <div class="card log"><h3>Live events</h3><div id="log"></div></div>
    </div>`;
  document.getElementById("log").innerHTML = logLines.map(logRow).join("");
}

// createLamp builds a single lamp tile once; updateLamp mutates it in place.
function createLamp(id) {
  const el = document.createElement("div");
  el.className = "lamp";
  el.dataset.id = id;
  el.innerHTML = `<div class="swatch"></div><div class="nm"></div><div class="st"></div>`;
  return el;
}

// updateLamp writes only fields that actually changed (guarded via dataset), so
// setting an identical colour never retriggers the swatch's CSS transition.
function updateLamp(el, l) {
  const col = l.on ? xyToRGB(l.x, l.y) : "";
  if (el.dataset.col !== col) {
    const sw = el.querySelector(".swatch");
    sw.style.background = l.on ? col : "";
    sw.style.boxShadow = l.on ? `0 0 20px ${col}` : "";
    el.dataset.col = col;
  }
  if (el.dataset.nm !== l.name) {
    el.querySelector(".nm").textContent = l.name;
    el.dataset.nm = l.name;
  }
  const on = l.on ? "1" : "";
  if (el.dataset.on !== on) {
    el.querySelector(".st").innerHTML = l.on ? `<span class="ok">on</span>` : "off";
    el.dataset.on = on;
  }
  el.classList.toggle("driven", !!l.driven);
  el.classList.toggle("off", !l.on);
}

// setText writes textContent only when it changed — a guard so identical
// snapshots cause no DOM mutation (hence no repaint) at all.
function setText(id, txt) {
  const el = document.getElementById(id);
  if (el && el.textContent !== txt) el.textContent = txt;
}

// updateDashboard reconciles the live skeleton against a fresh snapshot. It
// never touches #app.innerHTML; every change is a targeted node mutation.
function updateDashboard(s) {
  // Header.
  const dotEl = document.getElementById("health-dot");
  const dotCls = healthDotClass(s.health);
  if (dotEl.className !== dotCls) dotEl.className = dotCls;
  setText("health-label", healthLabel(s.health));

  const proEl = document.getElementById("pro-paired");
  const proHtml = s.proPaired ? `<span class="ok">✓</span> Paired` : "— Unpaired";
  if (proEl.dataset.paired !== proHtml) {
    proEl.innerHTML = proHtml;
    proEl.dataset.paired = proHtml;
  }
  setText("pro-sub", `${s.proName} ${s.proHost}`);
  setText("tv-val", `${s.tvClients.length} client(s)`);
  setText("tv-sub", s.tvClients.join(", "));
  setText("mode-val", `${cap(currentMode(s))}${s.fallback ? " (fallback)" : ""}`);
  setText("mode-sub", modeSub(s));
  setText("lights-val", String(s.lights.length));

  // Lamps — show only the lamps the TV is actively driving. Fall back to all
  // lamps while nothing is driven yet (cold start / TV idle before first
  // stream). driven is sticky (the backend never un-drives), so the shown set
  // only ever grows — re-ordering is rare.
  const drivenLights = s.lights.filter((l) => l.driven);
  const shown = drivenLights.length > 0 ? drivenLights : s.lights;
  const driven = drivenLights.length;
  setText("lights-sub", `${driven} driven by TV`);
  setText("lights-cnt", `${shown.length} shown · ${driven} driven`);

  const grid = document.getElementById("lights");
  const seen = new Set();
  for (const l of shown) {
    let el = lampEls.get(l.id);
    if (!el) {
      el = createLamp(l.id);
      lampEls.set(l.id, el);
    }
    updateLamp(el, l);
    seen.add(l.id);
  }
  for (const [id, el] of lampEls) {
    if (!seen.has(id)) {
      el.remove();
      lampEls.delete(id);
    }
  }
  // Reorder/insert only when the shown set or its order changed.
  const ids = shown.map((l) => l.id).join(",");
  if (lastShownIds !== ids) {
    for (const l of shown) grid.appendChild(lampEls.get(l.id));
    lastShownIds = ids;
  }

  // Needs-attention card — rebuilt only when its state changes (rare).
  const pendSig = `${s.proPaired ? 0 : 1}${s.pendingTV ? 1 : 0}`;
  if (lastPendSig !== pendSig) {
    document.getElementById("pending").innerHTML =
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
    lastPendSig = pendSig;
  }
}

// setupSig captures everything renderSetup depends on, so the setup view is
// rebuilt only when one of those changes — not on every 1s snapshot.
function setupSig(s) {
  return `${s.proPaired}|${s.proName}|${s.proHost}|${s.tvClients.length}|${s.lights.length}|${s.bridgeName}|${s.version}`;
}

let logLines = [];
let lampEls = new Map();
let lastShownIds = null;
let lastPendSig = null;
let mounted = null; // "dashboard" | "setup"
let lastSetupSig = null;
const logRow = (e) =>
  `<div class="logrow"><span class="ts">${esc((e.time || "").slice(11, 19))}</span><span class="tag">${esc(e.level)}</span><span class="msg">${esc(e.msg)}</span></div>`;

// render dispatches each snapshot: it mounts the right view once (or on a view
// switch), then applies in-place updates. The setup view is transient and
// rebuilt only when its signature changes; the dashboard updates in place.
function render(s) {
  _startedAtMs = s.startedAt ? Date.parse(s.startedAt) : null;
  if (s.proPaired && s.tvClients.length > 0) {
    if (mounted !== "dashboard") {
      mountDashboard(s);
      mounted = "dashboard";
    }
    updateDashboard(s);
  } else {
    const sig = setupSig(s);
    if (mounted !== "setup" || lastSetupSig !== sig) {
      renderSetup(s);
      mounted = "setup";
      lastSetupSig = sig;
    }
  }
  tickUptime();
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

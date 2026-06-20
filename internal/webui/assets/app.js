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
  return (h === "streaming-pro" || h === "entertainment-fallback" || h === "active-rest")
    ? "Active"
    : "Inactive";
}

// healthDotClass colours the status dot consistently with the binary pill: a steady
// green when the TV is actively driving the lights (Active), amber-pulsing otherwise
// (Inactive). The pulse is reserved to draw the eye to standby/attention states. The
// degraded "entertainment-fallback" detail is no longer shown here — it stays visible
// via the Stream card's amber ● indicator.
function healthDotClass(h) {
  if (h === "streaming-pro" || h === "entertainment-fallback" || h === "active-rest") return "dot ok";
  return "dot pulse";
}

// currentMode is the path relumeTV is forwarding on RIGHT NOW, not the configured
// startup mode. The TV only drives over entertainment/DTLS while its stream is
// actually up; in every other case (rest mode, fallback, or entertainment
// configured but the TV not streaming) the live path is REST.
function currentMode(s) {
  return s.dtlsStreamUp ? "entertainment" : "rest";
}

// modeSub describes the active forward path under the MODE label. The MODE value
// already shows REST/Entertainment (and "(fallback)"), so the sub must NOT repeat
// it — it only adds the reason: a fallback's cause, the TV not streaming, or what
// the plain REST path does.
function modeSub(s) {
  if (s.dtlsStreamUp) return "DTLS stream up";
  if (s.fallback) return "DTLS unavailable";
  if (s.mode === "entertainment") return "TV not streaming entertainment";
  return "Per-light writes to the Hue Bridge Pro";
}

// streamVal shows the live entertainment frame rate while the TV is streaming to the
// Pro: in DTLS mode it shows both the TV input rate and relumeTV's upsampled send rate
// (in → out fps); on the REST paths it shows relumeTV's outgoing write rate to the Pro
// (writes/s). Idle/unpaired states show a dash — streamSub explains why.
function streamVal(s) {
  switch (s.health) {
    case "streaming-pro":
      return `<span class="ok">●</span> ${s.streamFps || 0} → ${s.proSendFps || 0} fps`;
    case "entertainment-fallback":
      // Amber dot: streaming, but degraded (DTLS to the Pro failed → REST fallback).
      return `<span class="warn">●</span> ${s.streamFps || 0} fps in`;
    case "active-rest":
      return `<span class="ok">●</span> ${s.proWriteRate || 0} writes/s`;
    default:
      return "—";
  }
}

// streamSub explains the stream state under the Stream label. Kept distinct from the
// Mode card: this card is about the live path to the Pro, not the configured path. On
// the REST paths it also carries the outgoing write rate so both directions are visible.
function streamSub(s) {
  switch (s.health) {
    case "streaming-pro": return "DTLS → Hue Bridge Pro";
    case "entertainment-fallback": return `REST fallback · ${s.proWriteRate || 0} writes/s`;
    case "active-rest": return "REST → Hue Bridge Pro";
    case "idle": return "TV not driving";
    case "no-tv": return "no TV paired";
    default: return "Hue Bridge Pro not paired";
  }
}

// jitterDisplay shows how much relumeTV's easing cut the stream's brightness jitter —
// the reduction of the smoothed sent max jump vs the TV input max jump over the last
// window. A longdash when there is no value: not streaming to the Hue Bridge Pro over
// DTLS (smoothing only applies there), or nothing jumped to smooth.
function jitterDisplay(s) {
  if (!s.dtlsStreamUp || !s.jitterInBri) return "—";
  const pct = Math.max(0, Math.round(100 * (1 - (s.jitterSentBri || 0) / s.jitterInBri)));
  return pct > 0 ? `−${pct}%` : "0%";
}

// forwardErrDecayMs is how long the amber "N err" warning stays after the most
// recent failed Pro write. Once writes have been succeeding for this long, the
// card decays back to the healthy state — a long-resolved fault must not leave a
// permanent warning. The card re-renders on every snapshot push (~1s), so the
// decay resolves within ~1s of the window expiring.
const forwardErrDecayMs = 60000;

// forwardErrActive reports whether the forward-error warning should still show:
// there have been errors AND the last one is recent enough not to have decayed.
function forwardErrActive(s) {
  if (!(s.forwardErrors > 0) || !s.lastForwardErr) return false;
  return Date.now() - Date.parse(s.lastForwardErr) < forwardErrDecayMs;
}

// backpressureVal shows how relumeTV shields the Hue Bridge Pro. coalesceRate (drops/s)
// is HEALTHY — the optimistic path sparing the Pro a write it could not keep up
// with — so it is never coloured as a fault. forwardErrors is the real failure
// signal (down Pro / 503 overflow); it appears in amber only while recent, then
// decays away (see forwardErrActive).
function backpressureVal(s) {
  const n = s.coalesceRate || 0;
  const drops = `<span class="ok">●</span> ${n} ${n === 1 ? "drop" : "drops"}/s`;
  if (forwardErrActive(s)) {
    return `${drops} <span class="warn">● ${s.forwardErrors} err</span>`;
  }
  return drops;
}

// backpressureSub explains the Backpressure value: coalesced frames are spared
// writes (good), forward errors are failed writes to the Pro (bad). The sub flags
// errors only while the warning is active, otherwise it states the benign meaning.
// Returns ready-to-insert HTML (escaped dynamic count + a structural <br>), so the
// call site inserts it without esc() — mirroring how streamSub's <br> is structural.
function backpressureSub(s) {
  if (forwardErrActive(s)) return `${esc(s.forwardErrors)} failed Hue Bridge Pro writes`;
  return "Avoided extra writes<br>to Hue Bridge Pro";
}

// modeLabel renders the live forward path for display: "Entertainment" as a word,
// but "REST" as the acronym it is — never the title-cased "Rest".
function modeLabel(s) {
  return currentMode(s) === "entertainment" ? "Entertainment" : "REST";
}

// tvModel extracts the device/model name from a Hue "app#model" devicetype
// (e.g. "Ambilight#65OLED806" → "65OLED806"); falls back to the whole string.
function tvModel(dt) {
  const i = dt.indexOf("#");
  return i >= 0 ? dt.slice(i + 1) : dt;
}

// _startedAtMs holds relumeTV's start time (ms epoch) so the uptime can tick every
// second between snapshot pushes. fmtUptime renders only the largest unit, spelled
// out with correct singular/plural: weeks once past 7 days, then days/hours/
// minutes/seconds (e.g. "1 week", "2 days", "1 hour", "50 seconds").
let _startedAtMs = null;
function fmtUptime(ms) {
  if (!(ms >= 0)) return "";
  const unit = (n, name) => `${n} ${name}${n === 1 ? "" : "s"}`;
  const s = Math.floor(ms / 1000);
  const w = Math.floor(s / 604800);
  const d = Math.floor(s / 86400);
  const h = Math.floor((s % 86400) / 3600);
  const m = Math.floor((s % 3600) / 60);
  const sec = s % 60;
  if (w > 0) return unit(w, "week");
  if (d > 0) return unit(d, "day");
  if (h > 0) return unit(h, "hour");
  if (m > 0) return unit(m, "minute");
  return unit(sec, "second");
}
function tickUptime() {
  const el = document.getElementById("uptime");
  if (el && _startedAtMs) el.textContent = "↑ " + fmtUptime(Date.now() - _startedAtMs);
}

// _lastActivityMs holds the time (ms epoch) of the most recent Ambilight write, so
// the Liveness card can tick the elapsed time every second between snapshot pushes.
// This also covers DTLS streaming: the backend marks activity per decoded frame, so
// the card reads "live" throughout a stream, not just on the REST path.
let _lastActivityMs = null;
// fmtSince renders the elapsed time since the last write — "live" while it is fresh
// (under fmtSinceLive ms), otherwise the largest spelled-out unit like fmtUptime.
const fmtSinceLive = 2500;
function fmtSince(ms) {
  if (!(ms >= 0)) return "—";
  if (ms < fmtSinceLive) return "live";
  return fmtUptime(ms) + " ago";
}
function tickLiveness() {
  const el = document.getElementById("liveness");
  if (!el) return;
  el.innerHTML = _lastActivityMs
    ? (Date.now() - _lastActivityMs < fmtSinceLive
        ? `<span class="ok">●</span> live`
        : esc(fmtSince(Date.now() - _lastActivityMs)))
    : "—";
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
            <div class="d">On the TV, start the Ambilight+Hue bridge search and pick relumeTV. Advertised as “${esc(s.bridgeName)}”.</div>
            ${
              !s.tvClients.length
                ? `<div class="action"><span class="dot pulse"></span><div><div class="big">Waiting for TV search…</div></div></div>`
                : ""
            }</div>
        </div>
        <div class="step ${s.tvClients.length ? "active" : "todo"}">
          <div class="rail"><div class="num">3</div></div>
          <div class="card"><h3>Check lights &amp; go</h3>
            <div class="d">${s.lights.length} lights loaded from the Hue Bridge Pro.</div></div>
        </div>
      </div>
    </div>`;
}

function renderDashboard(s) {
  // Show only the lamps the TV is actively driving. Fall back to all lamps
  // while nothing is driven (cold start, or TV idle / not streaming). "driven" is
  // a live, windowed signal from the backend: it reflects the lamps the TV is
  // streaming RIGHT NOW and empties shortly after the stream stops.
  const drivenLights = s.lights.filter((l) => l.driven);
  const shown = drivenLights.length > 0 ? drivenLights : s.lights;
  const lights = shown
    .map((l) => {
      const col = l.on ? xyToRGB(l.x, l.y) : "";
      return `<div class="lamp ${l.driven ? "driven" : ""} ${l.on ? "" : "off"}">
        <div class="swatch" style="${l.on ? `background:${col};box-shadow:0 0 20px ${col}` : ""}"></div>
        <div class="nm">${esc(l.name)}</div>
        <div class="st">${l.on ? `<span class="ok">On</span>` : "Off"}</div></div>`;
    })
    .join("");
  const driven = drivenLights.length;
  const pending = !s.proPaired
    ? `<div class="card pending"><h3>⚠ Needs attention</h3>
          <div class="pendrow"><div class="info"><b>Hue Bridge Pro pairing</b><div>Press the link button on the Hue Bridge Pro</div></div><span class="dot pulse"></span></div>
        </div>`
    : "";
  app.innerHTML = `
    <div class="wrap">
      <div class="top"><div class="brand">re<span>lume</span></div><div class="ver">v${esc(s.version)}</div>
        <div class="spacer"></div><div class="health"><span class="${healthDotClass(s.health)}"></span> ${esc(healthLabel(s.health))}</div></div>
      <div class="pipe">
        <div class="step"><div class="lbl">Hue Bridge Pro</div><div class="val">${s.proPaired ? `<span class="ok">✓</span> Paired` : "— Unpaired"}</div><div class="sub">${esc(s.proHost)}${s.proBridgeId ? `<br>${esc(s.proBridgeId.toUpperCase())}` : ""}</div></div>
        <div class="step"><div class="lbl">TV pairing</div><div class="val">${s.tvClients.length ? "Philips TV" : "—"}</div><div class="sub">${s.tvClients.map(c => esc(tvModel(c))).join("<br>")}</div></div>
        <div class="step"><div class="lbl">Mode <span class="info" tabindex="0" data-tip="Entertainment: low-latency DTLS stream to the Hue Bridge Pro (default). REST: per-light REST writes — the automatic fallback when the TV is not streaming entertainment.">i</span></div><div class="val">${modeLabel(s)}${s.fallback ? " (fallback)" : ""}</div><div class="sub">${esc(modeSub(s))}</div></div>
        <div class="step"><div class="lbl">Uptime</div><div class="val" id="uptime">${s.startedAt ? esc("↑ " + fmtUptime(Date.now() - Date.parse(s.startedAt))) : "—"}</div><div class="sub">Running</div></div>
      </div>
      <div class="pipe row2">
        <div class="step"><div class="lbl">Lights</div><div class="val">${driven}</div><div class="sub">Driven by TV</div></div>
        <div class="step"><div class="lbl">Stream <span class="info" tabindex="0" data-tip="Jitter is the largest brightness jump between two consecutive frames. relumeTV eases each colour toward the latest TV frame with a ${s.smoothingTauMs || 40} ms time constant, so the TV's hard scene cuts reach the lamps as a fast fade instead of a flicker. The figure is the reduction this buys: −45% means the biggest jump on the stream sent to the Hue Bridge Pro is 45% smaller than on the TV input — more negative is smoother. 0% when nothing jumped, or the cut passed through unsmoothed (e.g. tau set to 0). DTLS path only.">i</span></div><div class="val">${streamVal(s)}</div><div class="sub">${esc(streamSub(s))}<br>Jitter ${jitterDisplay(s)}</div></div>
        <div class="step"><div class="lbl">Backpressure <span class="info" tabindex="0" data-tip="Drops/s: Ambilight frames relumeTV coalesced away because the Hue Bridge Pro could not keep up — healthy, it spares the Hue Bridge Pro writes it cannot accept. Errors: failed writes to the Hue Bridge Pro (unreachable / 503 overflow) — the real fault signal.">i</span></div><div class="val">${backpressureVal(s)}</div><div class="sub">${backpressureSub(s)}</div></div>
        <div class="step"><div class="lbl">Liveness</div><div class="val" id="liveness">—</div><div class="sub">Since last write</div></div>
      </div>
      <div class="grid">${pending}
        <div class="card"><h3>Lights <span class="cnt">${shown.length} shown · ${driven} driven</span></h3><div class="lights">${lights}</div></div>
      </div>
      <div class="card log"><h3>Live events</h3><div id="log"></div></div>
    </div>`;
}

let logLines = [];
const logRow = (e) =>
  `<div class="logrow"><span class="ts">${esc((e.time || "").slice(11, 19))}</span><span class="tag">${esc(e.level)}</span><span class="msg">${esc(e.msg)}</span>${e.attrs ? `<span class="attrs">${esc(e.attrs)}</span>` : ``}</div>`;

function render(s) {
  _startedAtMs = s.startedAt ? Date.parse(s.startedAt) : null;
  _lastActivityMs = s.lastActivity ? Date.parse(s.lastActivity) : null;
  if (s.proPaired && s.tvClients.length > 0) renderDashboard(s);
  else renderSetup(s);
  tickUptime();
  tickLiveness();
  const logEl = document.getElementById("log");
  if (logEl) logEl.innerHTML = logLines.map(logRow).join("");
}

function pushLog(e) {
  logLines.unshift(e);
  if (logLines.length > 100) logLines.pop();
  const logEl = document.getElementById("log");
  if (logEl) logEl.innerHTML = logLines.map(logRow).join("");
}

// Tooltip for .info[data-tip] icons. The element lives on <body> (not inside the
// .pipe, whose overflow:hidden would clip it) and is positioned under the icon.
// Event delegation on document survives the full-innerHTML re-renders. Works on
// hover/focus and toggles on click (touch).
let _tipEl = null;
function tipNode() {
  if (!_tipEl) {
    _tipEl = document.createElement("div");
    _tipEl.id = "tip";
    document.body.appendChild(_tipEl);
  }
  return _tipEl;
}
function showTip(icon) {
  const tip = tipNode();
  tip.textContent = icon.getAttribute("data-tip") || "";
  const r = icon.getBoundingClientRect();
  // Place below the icon, clamped to the viewport so it never runs off-screen.
  tip.style.left = Math.min(r.left, window.innerWidth - 272) + "px";
  tip.style.top = r.bottom + 8 + "px";
  tip.classList.add("show");
}
function hideTip() {
  if (_tipEl) _tipEl.classList.remove("show");
}
document.addEventListener("mouseover", (e) => {
  const icon = e.target.closest?.(".info[data-tip]");
  if (icon) showTip(icon);
});
document.addEventListener("mouseout", (e) => {
  if (e.target.closest?.(".info[data-tip]")) hideTip();
});
document.addEventListener("focusin", (e) => {
  const icon = e.target.closest?.(".info[data-tip]");
  if (icon) showTip(icon);
});
document.addEventListener("focusout", hideTip);
document.addEventListener("click", (e) => {
  const icon = e.target.closest?.(".info[data-tip]");
  if (!icon) {
    hideTip();
    return;
  }
  const tip = tipNode();
  tip.classList.contains("show") ? hideTip() : showTip(icon);
});

async function boot() {
  try {
    const s = await (await fetch("/api/state")).json();
    render(s);
  } catch (_) {}
  // Tick the uptime and liveness every second between snapshot pushes.
  setInterval(() => {
    tickUptime();
    tickLiveness();
  }, 1000);
  const es = new EventSource("/api/events");
  es.onmessage = (msg) => {
    const f = JSON.parse(msg.data);
    if (f.kind === "snapshot") render(f.snapshot);
    else if (f.kind === "event") pushLog(f.event);
  };
}
boot();

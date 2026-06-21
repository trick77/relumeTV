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

// currentMode is the path relume-tv is forwarding on RIGHT NOW, not the configured
// startup mode. The TV only drives over entertainment/DTLS while its stream is
// actually up; in every other case (rest mode, fallback, or entertainment
// configured but the TV not streaming) the live path is REST.
function currentMode(s) {
  return s.dtlsStreamUp ? "entertainment" : "rest";
}

// proPathSub describes the live forward path to the Hue Bridge Pro, shown under the
// Mode card. Every active path reads uniformly as "<protocol> → Hue Bridge Pro" (DTLS
// while streaming, REST otherwise — including the fallback, whose "(fallback)" tag
// already sits on the Mode value). Idle/unpaired states have no active path, so they
// explain why.
function proPathSub(s) {
  switch (s.health) {
    case "streaming-pro":          return "DTLS → Hue Bridge Pro";
    case "entertainment-fallback": return "REST → Hue Bridge Pro";
    case "active-rest":            return "REST → Hue Bridge Pro";
    case "idle":                   return "TV not driving";
    case "no-tv":                  return "no TV paired";
    default:                       return "Hue Bridge Pro not paired";
  }
}

// jitterDisplay shows how much relume-tv's easing cut the stream's brightness jitter —
// the reduction of the smoothed sent max jump vs the TV input max jump over the last
// window. Defaults to 0% (rather than a dash) when there is no current measurement —
// e.g. not streaming to the Hue Bridge Pro over DTLS, or nothing jumped to smooth.
function jitterDisplay(s) {
  if (!s.dtlsStreamUp || !s.jitterInBri) return "0%";
  const pct = Math.max(0, Math.round(100 * (1 - (s.jitterSentBri || 0) / s.jitterInBri)));
  return `${pct}%`;
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

// backpressureVal shows how relume-tv shields the Hue Bridge Pro. coalesceRate (drops/s)
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
// call site inserts it without esc().
function backpressureSub(s) {
  if (forwardErrActive(s)) return `${esc(s.forwardErrors)} failed Hue Bridge Pro writes`;
  return "Avoided extra writes";
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

// _startedAtMs holds relume-tv's start time (ms epoch) so the uptime can tick every
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
// fmtSinceLive is the freshness window: an update newer than this reads as "Live" on the
// Liveness card; past it the card shows how long since the last update.
const fmtSinceLive = 2500;
// livenessSub is the Liveness card subtitle. The throughput now lives on its own
// Received/Sent cards, so Liveness is a pure liveness indicator: a static label while a
// stream is live, otherwise how long since the last update ("for 49 seconds"), or a
// cold-start note before any activity. Keyed off _lastActivityMs (the same freshness
// livenessVal uses), so it reads truthfully without depending on health-label semantics.
function livenessSub() {
  if (_lastActivityMs && Date.now() - _lastActivityMs < fmtSinceLive) {
    return "Streaming to Hue Bridge Pro";
  }
  if (!_lastActivityMs) return "No updates yet";
  return "for " + fmtUptime(Date.now() - _lastActivityMs);
}
// livenessVal is the Liveness card value: always just the dotted status (● Live / ● Idle).
// The elapsed-since-last-update detail lives in the subtitle (livenessSub), so the value
// never shows a duration — no number appears twice on the card.
function livenessVal() {
  if (_lastActivityMs && Date.now() - _lastActivityMs < fmtSinceLive) {
    return `<span class="ok">●</span> Live`;
  }
  return `<span class="idle">●</span> Idle`;
}
// tickLiveness refreshes both the value and the subtitle every second between snapshot
// pushes, so the "for N seconds" elapsed counts up smoothly. The subtitle is the next
// `.sub` sibling of the value element (id "liveness").
function tickLiveness() {
  const el = document.getElementById("liveness");
  if (!el) return;
  el.innerHTML = livenessVal();
  const sub = el.parentElement?.querySelector(".sub");
  if (sub) sub.textContent = livenessSub();
}

// receivedSub shows how fast relume-tv is receiving from the TV: DTLS frames/s while the TV
// streams (incl. the outbound REST fallback — the receive side is still DTLS), else inbound
// REST control calls/s on the plain-REST path, else a longdash. Keyed off the live counters
// (not health) so entertainment-fallback correctly stays on fps.
function receivedSub(s) {
  if (s.streamFps > 0)    return `${s.streamFps} fps`;
  if (s.restRecvRate > 0) return `${s.restRecvRate} calls/s`;
  return "—";
}
// sentSub shows how fast relume-tv drives the Hue Bridge Pro: DTLS frames/s (the 50 Hz
// sendLoop) while streaming, REST writes/s on the REST/fallback path, else a longdash.
function sentSub(s) {
  if (s.proSendFps > 0)   return `${s.proSendFps} fps`;
  if (s.proWriteRate > 0) return `${s.proWriteRate} writes/s`;
  return "—";
}

// SETUP_STEPS are the six wizard steps. body(s) returns the step's description HTML,
// and the optional action(s) returns extra UI shown only while the step is active. The
// state machine lives in the backend; this is a pure renderer of s.currentStep.
const SETUP_STEPS = [
  {
    title: "Pair Hue Bridge Pro",
    body: (s) =>
      s.currentStep > 1
        ? `${esc(s.proName || "Hue Bridge Pro")} · ${esc(s.proHost || s.discoveredHost)} · certificate pinned`
        : s.discoveredHost
          ? `Hue Bridge Pro found at <b>${esc(s.discoveredHost)}</b>. Briefly press the link button on top of the bridge.`
          : "Looking for your Hue Bridge Pro…",
  },
  {
    title: "Disconnect the Hue Bridge Pro from power",
    body: (s) =>
      s.currentStep > 2
        ? "Hue Bridge Pro is off — detected."
        : `Pull the power on the Hue Bridge Pro now. ${
            s.proReachable ? "We'll detect it (still reachable…)." : "We'll detect it."
          }`,
    action: (s) =>
      `<div class="action"><span class="dot pulse"></span>
         <div><div class="big">${s.proReachable ? "Hue Bridge Pro still reachable…" : "Waiting…"}</div></div></div>`,
  },
  {
    title: "Reboot your TV",
    body: () =>
      "On the TV: <b>Android Settings → Device Preferences → Restart</b>. After it boots, the TV re-detects relume-tv automatically.",
    action: () =>
      `<div class="action"><span class="dot pulse"></span><div><div class="big">Waiting for the TV to reboot…</div></div></div>`,
  },
  {
    title: "Start the relume-tv scan on your TV",
    body: (s) =>
      `In the TV's <b>Ambilight+Hue</b> settings, start the bridge search, pick <b>${esc(s.bridgeName)}</b> and <b>link</b> it (confirm the pairing on the TV — this is essential, not just selecting it). Then wait here: <b>do not assign any bulbs yet</b> — that's the final step, after the Hue Bridge Pro is back on.`,
    action: () =>
      `<div class="action"><span class="dot pulse"></span><div><div class="big">Waiting for the TV to link…</div></div></div>`,
  },
  {
    title: "Turn the Hue Bridge Pro back on",
    body: (s) =>
      s.currentStep > 5
        ? "Hue Bridge Pro is back — detected."
        : "Plug the Hue Bridge Pro back in. relume-tv reconnects automatically.",
    action: () =>
      `<div class="action"><span class="dot pulse"></span><div><div class="big">Waiting for the Hue Bridge Pro…</div></div></div>`,
  },
  {
    title: "Assign your color bulbs, then press Finish",
    body: (s) =>
      `In the TV's Ambilight+Hue menu, assign your color bulbs and press <b>Finish</b>. ${s.lights.length} lights loaded from the Hue Bridge Pro.`,
    action: () =>
      `<div class="action"><span class="dot pulse"></span><div><div class="big">Waiting for the first Ambilight data…</div></div></div>`,
  },
];

// layoutStepperLines positions each connector line from the bottom of its step's circle
// to the top of the next step's circle, with a symmetric gap so the line never touches
// either circle. Driven by the real geometry (circles are vertically centered in their
// variable-height cards), which pure CSS can't reach across to the next card.
function layoutStepperLines() {
  const steps = [...document.querySelectorAll(".steps .step")];
  const gap = 7; // breathing space between a line end and a circle
  for (let i = 0; i < steps.length - 1; i++) {
    const line = steps[i].querySelector(".line");
    if (!line) continue;
    const rail = steps[i].querySelector(".rail").getBoundingClientRect();
    const cur = steps[i].querySelector(".num").getBoundingClientRect();
    const next = steps[i + 1].querySelector(".num").getBoundingClientRect();
    const top = cur.bottom + gap - rail.top; // rail-relative
    const height = next.top - gap - (cur.bottom + gap);
    line.style.top = top + "px";
    line.style.height = Math.max(0, height) + "px";
  }
}

function renderSetup(s) {
  const cur = s.currentStep || 1;
  const banner =
    s.precondMsg && cur === 1
      ? `<div class="card pending setup-banner"><h3>⚠ Needs attention</h3><div class="d">${esc(s.precondMsg)}</div></div>`
      : "";
  const steps = SETUP_STEPS.map((step, i) => {
    const n = i + 1;
    const state = cur > n ? "done" : cur === n ? "active" : "todo";
    const last = n === SETUP_STEPS.length;
    const num = cur > n ? "✓" : String(n);
    const action = state === "active" && step.action ? step.action(s) : "";
    return `
      <div class="step ${state}">
        <div class="rail"><div class="num">${num}</div>${last ? "" : `<div class="line"></div>`}</div>
        <div class="card"><h3>${esc(step.title)}</h3>
          <div class="d">${step.body(s)}</div>
          ${action}</div>
      </div>`;
  }).join("");
  app.innerHTML = `
    <div class="wrap">
      <div class="top"><div class="brand">re<span>lume</span>-tv</div><div class="ver">v${esc(s.version)}${s.firstRun ? " · first run" : ""}</div></div>
      <p class="lead">Six steps until your Ambilight TV drives the Hue Bridge Pro.</p>
      ${banner}
      <div class="steps">${steps}</div>
    </div>`;
  layoutStepperLines();
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
      <div class="top"><div class="brand">re<span>lume</span>-tv</div><div class="ver">v${esc(s.version)}</div>
        <div class="spacer"></div><div class="health"><span class="${healthDotClass(s.health)}"></span> ${esc(healthLabel(s.health))}</div></div>
      <div class="pipe">
        <div class="step"><div class="lbl">Hue Bridge Pro</div><div class="val">${s.proPaired ? `<span class="ok">✓</span> Paired` : "— Unpaired"}</div><div class="sub">${esc(s.proHost)}${s.proBridgeId ? `<br>${esc(s.proBridgeId.toUpperCase())}` : ""}</div></div>
        <div class="step"><div class="lbl">TV pairing</div><div class="val">${s.tvClients.length ? "Philips TV" : "—"}</div><div class="sub">${s.tvClients.map(c => esc(tvModel(c))).join("<br>")}</div></div>
        <div class="step"><div class="lbl">Mode <span class="info" tabindex="0" data-tip="Entertainment: low-latency DTLS stream to the Hue Bridge Pro (default). REST: per-light REST writes — the automatic fallback when the TV is not streaming entertainment.">i</span></div><div class="val">${modeLabel(s)}${s.fallback ? " (fallback)" : ""}</div><div class="sub">${esc(proPathSub(s))}</div></div>
        <div class="step"><div class="lbl">Liveness</div><div class="val" id="liveness">${livenessVal()}</div><div class="sub">${esc(livenessSub())}</div></div>
        <div class="step"><div class="lbl">Uptime</div><div class="val" id="uptime">${s.startedAt ? esc("↑ " + fmtUptime(Date.now() - Date.parse(s.startedAt))) : "—"}</div><div class="sub">Running</div></div>
      </div>
      <div class="pipe row2">
        <div class="step"><div class="lbl">Lights</div><div class="val">${driven}</div><div class="sub">Driven by TV</div></div>
        <div class="step"><div class="lbl">Jitter-reduction <span class="info" tabindex="0" data-tip="How much relume-tv's ${s.smoothingTauMs || 40} ms easing shrinks the biggest brightness jump on the DTLS stream to the Hue Bridge Pro vs the TV input. Higher is smoother; 0% when not streaming or nothing jumped.">i</span></div><div class="val">${jitterDisplay(s)}</div><div class="sub">vs TV input</div></div>
        <div class="step"><div class="lbl">Backpressure <span class="info" tabindex="0" data-tip="Drops/s: Ambilight frames relume-tv coalesced away because the Hue Bridge Pro could not keep up — healthy, it spares the Hue Bridge Pro writes it cannot accept. Errors: failed writes to the Hue Bridge Pro (unreachable / 503 overflow) — the real fault signal.">i</span></div><div class="val">${backpressureVal(s)}</div><div class="sub">${backpressureSub(s)}</div></div>
        <div class="step"><div class="lbl">Received</div><div class="val">${esc(receivedSub(s))}</div><div class="sub">from TV</div></div>
        <div class="step"><div class="lbl">Sent</div><div class="val">${esc(sentSub(s))}</div><div class="sub">to Hue Bridge Pro</div></div>
      </div>
      <div class="grid">${pending}
        <div class="card"><h3>Lights <span class="cnt">${shown.length} shown · ${driven} driven</span></h3><div class="lights">${lights}</div></div>
      </div>
      <div class="note"><svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M9 18h6"/><path d="M10 22h4"/><path d="M15.09 14c.18-.98.65-1.74 1.41-2.5A4.65 4.65 0 0 0 18 8 6 6 0 0 0 6 8c0 1 .23 2.23 1.5 3.5A4.61 4.61 0 0 1 8.91 14"/></svg><b>Tip</b> — after relume-tv restarts, if the hue lights stop responding, open the TV's Ambilight menu and toggle the Ambilight style (not Hue+Ambilight menu) off and back to follow video.</div>
      <div class="card log${logCollapsed ? " collapsed" : ""}"><h3 class="log-head" role="button" tabindex="0" aria-expanded="${!logCollapsed}" aria-controls="log">Live events<span class="chev" aria-hidden="true"></span></h3><div id="log"></div></div>
    </div>`;
}

// Live-events log: collapsed by default, toggled by its header. renderDashboard rebuilds
// app.innerHTML every snapshot (~1s), so the collapse state lives here (not in the DOM)
// and is baked into the card markup (the `collapsed` class + event count) on each render.
let logCollapsed = true;
function toggleLog() {
  logCollapsed = !logCollapsed;
  const card = document.querySelector(".card.log");
  if (!card) return;
  card.classList.toggle("collapsed", logCollapsed);
  const head = card.querySelector(".log-head");
  if (head) head.setAttribute("aria-expanded", String(!logCollapsed));
}
document.addEventListener("click", (e) => {
  if (e.target.closest?.(".log-head")) toggleLog();
});
document.addEventListener("keydown", (e) => {
  if ((e.key === "Enter" || e.key === " ") && e.target.closest?.(".log-head")) {
    e.preventDefault();
    toggleLog();
  }
});

let logLines = [];
// Level decoration: INFO/DEBUG read like the timestamp (faint); WARN/ERROR get a soft,
// on-palette tint so they stand out without a hard red. Mapped from known level prefixes
// (not interpolated) so the level string can never inject a class.
const lvlClass = (lvl) => {
  const l = (lvl || "").toUpperCase();
  if (l.startsWith("WARN")) return " tag-warn";
  if (l.startsWith("ERROR")) return " tag-error";
  return "";
};
const logRow = (e) =>
  `<div class="logrow"><span class="ts">${esc((e.time || "").slice(11, 19))}</span><span class="tag${lvlClass(e.level)}">${esc(e.level)}</span><span class="msg">${esc(e.msg)}</span>${e.attrs ? `<span class="attrs">${esc(e.attrs)}</span>` : ``}</div>`;

function render(s) {
  _startedAtMs = s.startedAt ? Date.parse(s.startedAt) : null;
  _lastActivityMs = s.lastActivity ? Date.parse(s.lastActivity) : null;
  // Steady-state gate: the backend's committed setup-complete state decides, NOT the
  // live TV activity — so an idle/off TV after setup keeps the dashboard instead of
  // flipping back to the wizard. The first TV activity is what commits the setup
  // (backend), and from then on setupComplete stays true across restarts.
  if (s.setupComplete) renderDashboard(s);
  else renderSetup(s);
  tickUptime();
  tickLiveness();
  renderLog();
}

function renderLog() {
  const logEl = document.getElementById("log");
  if (logEl) logEl.innerHTML = logLines.map(logRow).join("");
}

function pushLog(e) {
  logLines.unshift(e);
  if (logLines.length > 100) logLines.pop();
  renderLog();
}

// Re-flow the stepper connector lines when the viewport resizes (card heights, and thus
// circle positions, can change with width). No-op when the wizard isn't shown.
window.addEventListener("resize", () => {
  if (document.querySelector(".steps")) layoutStepperLines();
});

// Tooltip for .info[data-tip] icons. The element lives on <body> (not inside the
// .pipe, whose overflow:hidden would clip it) and is positioned under the icon.
// Event delegation on document survives the full-innerHTML re-renders. On devices
// with a real pointer it shows on hover/focus; on touch-only devices hover/focus
// are suppressed so the tap (a single click) cleanly toggles the tip instead of the
// hover showing it only for the click to immediately hide it again.
const _canHover = window.matchMedia("(hover: hover)").matches;
let _tipEl = null;
// Track the shown tip by its data-tip text, NOT the icon node: renderDashboard rebuilds
// app.innerHTML every snapshot (~1s), so the icon nodes are recreated and a stored node
// reference would go stale — tap-to-dismiss would then never match the new node.
let _shownKey = null;
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
  _shownKey = icon.getAttribute("data-tip");
}
function hideTip() {
  if (_tipEl) _tipEl.classList.remove("show");
  _shownKey = null;
}
document.addEventListener("mouseover", (e) => {
  if (!_canHover) return;
  const icon = e.target.closest?.(".info[data-tip]");
  if (icon) showTip(icon);
});
document.addEventListener("mouseout", (e) => {
  if (_canHover && e.target.closest?.(".info[data-tip]")) hideTip();
});
document.addEventListener("focusin", (e) => {
  if (!_canHover) return;
  const icon = e.target.closest?.(".info[data-tip]");
  if (icon) showTip(icon);
});
document.addEventListener("focusout", () => {
  if (_canHover) hideTip();
});
document.addEventListener("click", (e) => {
  const icon = e.target.closest?.(".info[data-tip]");
  if (!icon) {
    hideTip();
    return;
  }
  // Tap the same icon again to dismiss; tap a different one to switch.
  _shownKey === icon.getAttribute("data-tip") ? hideTip() : showTip(icon);
});
// Keyboard activation (Enter/Space). focusin is gated to hover devices (so a touch tap,
// which also focuses the icon, doesn't show-then-hide via the click); this keydown keeps
// the tip reachable for keyboard users on non-hover devices. A bare <span> emits no
// synthetic click on Enter, so there is no double-trigger.
document.addEventListener("keydown", (e) => {
  if (e.key !== "Enter" && e.key !== " ") return;
  const icon = e.target.closest?.(".info[data-tip]");
  if (!icon) return;
  e.preventDefault();
  _shownKey === icon.getAttribute("data-tip") ? hideTip() : showTip(icon);
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
    // Guard the parse: one malformed frame must not throw out of the handler and
    // wedge the live stream — skip it and keep going.
    let f;
    try {
      f = JSON.parse(msg.data);
    } catch (_) {
      return;
    }
    if (f.kind === "snapshot") render(f.snapshot);
    else if (f.kind === "event") pushLog(f.event);
  };
  es.onerror = () => {
    // EventSource reconnects on its own; refetch the current state so a snapshot
    // dropped or missed during the outage cannot leave the dashboard stuck on stale
    // data. The fetch is a cached read and harmlessly fails while still disconnected.
    fetch("/api/state")
      .then((r) => r.json())
      .then(render)
      .catch(() => {});
  };
}
boot();

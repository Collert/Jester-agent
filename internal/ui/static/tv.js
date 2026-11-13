// The TV page (templates/tv.html): swaps in a fresh copy whenever the agent's state changes (SSE from /events),
// ticks the clock, and, in a browser the agent can't steer (cog), takes itself to the screen it should show.
const app = document.getElementById("app");
let busy = false, again = false;
let shown = null; // the HTML last swapped in: app.innerHTML never reads back exactly as the server sent it

async function refresh() {
    if (busy) { again = true; return; }
    busy = true;
    try {
        const res = await fetch("/?partial=1", { cache: "no-store" });
        if (res.ok) {
            const html = await res.text();
            // Only when it changed: a swap redraws the whole TV, which a Pi Zero feels.
            if (html !== shown) { app.innerHTML = html; shown = html; }  // our own server's HTML, rendered and escaped there
            follow();
        }
    } catch { /* the agent is restarting: the stream reconnects and refreshes */ }
    busy = false;
    if (again) { again = false; refresh(); }
}

// Going to the screen (data-go, set while the browser should be on it). First the page makes sure the screen's
// server answers at all: a browser that can't open a page shows its own error page and stays on it, where the agent
// can't see it. When nothing answers the agent is told instead, and the TV says so and tries again shortly.
const wanted = () => app.querySelector("[data-go]")?.dataset.go;
let going = null;
async function follow() {
    const go = wanted();
    if (!go || go === going) return;
    going = go;
    const giveUp = new AbortController();
    const timer = setTimeout(() => giveUp.abort(), 20000);
    try {
        await fetch(new URL(go).origin + "/", { method: "HEAD", mode: "no-cors", cache: "no-store", credentials: "omit", signal: giveUp.signal });
    } catch {
        going = null;
        fetch("/screen-failed", { method: "POST" }).catch(() => {});
        return;
    } finally {
        clearTimeout(timer);
    }
    if (wanted() === go) location.replace(go);
    else { going = null; follow(); } // changed while checking
}

function tick() {
    const el = app.querySelector("[data-clock]");
    if (el) {
        const now = new Date();
        el.textContent = `${now.getHours() % 12 || 12}:${String(now.getMinutes()).padStart(2, "0")}`;
    }
}

// The spinners turn an eighth at a time, eight times a second, and only while one is on the page: a CSS animation
// would have Chromium redraw the TV 60 times a second for as long as it ran (see tv.css).
let turn = 0;
function spin() {
    const spinners = app.querySelectorAll(".spin");
    if (!spinners.length) return;
    turn = (turn + 1) % 8;
    for (const el of spinners) el.style.transform = `rotate(${turn * 45}deg)`;
}

const events = new EventSource("/events");
events.onmessage = refresh;
events.onopen = refresh;
setInterval(tick, 5000);
setInterval(spin, 125);
tick();
follow();

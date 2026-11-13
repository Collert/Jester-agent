// The TV page (templates/tv.html): swaps in a fresh copy whenever the agent's state changes (SSE from /events),
// ticks the clock, and, when the agent can't steer the browser, takes itself to the screen it should show.
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

function follow() {
    const go = app.querySelector("[data-go]")?.dataset.go;
    if (go) location.replace(go);
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

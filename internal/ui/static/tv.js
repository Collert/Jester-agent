// The TV page (templates/tv.html): swaps in a fresh copy whenever the agent's state changes (SSE from /events),
// ticks the clock, and, when the agent can't steer the browser, takes itself to the screen it should show.
const app = document.getElementById("app");
let busy = false, again = false;

async function refresh() {
    if (busy) { again = true; return; }
    busy = true;
    try {
        const res = await fetch("/?partial=1", { cache: "no-store" });
        if (res.ok) {
            const html = await res.text();
            if (html !== app.innerHTML) app.innerHTML = html;  // our own server's HTML, rendered and escaped there
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

const events = new EventSource("/events");
events.onmessage = refresh;
events.onopen = refresh;
setInterval(tick, 5000);
tick();
follow();

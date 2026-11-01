// The phone's setup page (templates/portal.html): the six code boxes, and the one password field following the
// network that's picked. Without this the page still works (a plain code field, the password under the list).

const code = document.querySelector("[data-code]");
if (code) {
    const input = code.querySelector("input");
    const boxes = [...code.querySelectorAll(".box")];
    const draw = () => {
        input.value = input.value.replace(/\D/g, "").slice(0, 6);
        boxes.forEach((box, i) => {
            box.textContent = input.value[i] || "";
            box.classList.toggle("cur", document.activeElement === input && i === Math.min(input.value.length, 5) && input.value.length < 6);
        });
    };
    code.classList.add("js");
    for (const kind of ["input", "focus", "blur"]) input.addEventListener(kind, draw);
    draw();
}

const wifi = document.querySelector("[data-wifi]");
if (wifi) {
    const pw = wifi.querySelector("[data-pw]");
    const place = () => {
        const picked = wifi.querySelector("input[name=ssid]:checked")?.closest("[data-net]");
        const slot = picked?.querySelector("[data-slot]");
        // Open networks and the cable need no password; "Other network…" may or may not.
        if (!slot || !(picked.hasAttribute("data-secure") || picked.hasAttribute("data-other"))) { pw.hidden = true; return; }
        pw.hidden = false;
        const value = pw.querySelector("input");
        const saved = pw.querySelector("[data-saved-key]");
        // The note about Imager's password belongs to the network Imager saved (the one picked when the page came).
        const onSaved = !!saved && picked.querySelector("input[name=ssid]").defaultChecked;
        if (saved) saved.hidden = !onSaved;
        value.placeholder = onSaved ? "Set in Raspberry Pi Imager" : picked.hasAttribute("data-other") ? "Password (leave empty if none)" : "Wi-Fi password";
        if (pw.parentElement !== slot) {
            slot.append(pw);
            // The saved password belongs to the saved network: another one starts empty.
            value.value = picked.querySelector("input[name=ssid]").defaultChecked ? value.defaultValue : "";
        }
    };
    wifi.addEventListener("change", (event) => { if (event.target.name === "ssid") place(); });
    place();

    const show = wifi.querySelector("[data-show]");
    show?.addEventListener("click", () => {
        const input = pw.querySelector("input");
        input.type = input.type === "password" ? "text" : "password";
        show.textContent = input.type === "password" ? "Show" : "Hide";
    });
    wifi.querySelector("[data-scan]")?.addEventListener("click", (event) => event.currentTarget.classList.add("busy"));
}

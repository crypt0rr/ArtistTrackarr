(() => {
  const themeToggle = document.querySelector("[data-theme-toggle]");
  if (themeToggle) {
    const key = "artist-trackarr-theme";
    let saved = "light";
    try {
      const candidate = localStorage.getItem(key);
      if (["light", "dark"].includes(candidate)) saved = candidate;
    } catch (_) {}
    const applyTheme = (theme) => {
      if (theme === "dark") document.documentElement.dataset.theme = "dark";
      else delete document.documentElement.dataset.theme;
      const nextTheme = theme === "dark" ? "light" : "dark";
      const label = `Switch to ${nextTheme} mode`;
      themeToggle.setAttribute("aria-label", label);
      themeToggle.setAttribute("aria-pressed", String(theme === "dark"));
      themeToggle.setAttribute("title", label);
      const labelNode = themeToggle.querySelector("[data-theme-label]");
      if (labelNode) labelNode.textContent = label;
      try { localStorage.setItem(key, theme); } catch (_) {}
    };
    applyTheme(saved);
    themeToggle.addEventListener("click", () => {
      applyTheme(document.documentElement.dataset.theme === "dark" ? "light" : "dark");
    });
  }

  const form = document.querySelector("[data-destination-form]");
  if (form) {
    const visible = {
      ntfy: ["host", "username", "password", "topic"],
      email: ["host", "port", "username", "password", "from", "to"],
      discord: ["token", "target"],
      telegram: ["token", "target"],
      gotify: ["host", "token"],
      generic: ["target"],
      advanced: ["raw_url"]
    };
    const refresh = () => {
      const service = form.querySelector("[data-service]").value;
      form.querySelectorAll("[data-field]").forEach((field) => {
        field.hidden = !visible[service].includes(field.dataset.field);
      });
    };
    form.querySelector("[data-service]").addEventListener("change", refresh);
    refresh();
  }
  const copy = document.querySelector("[data-copy]");
  if (copy) copy.addEventListener("click", async () => {
    await navigator.clipboard.writeText(document.querySelector("[data-copy-value]").value);
    copy.textContent = "Copied";
  });

  document.querySelectorAll("[data-multi-follow]").forEach((multi) => {
    const choices = [...multi.querySelectorAll("[data-artist-choice]")];
    const selectAll = multi.querySelector("[data-select-all]");
    const count = multi.querySelector("[data-selected-count]");
    const submit = multi.querySelector("[data-follow-selected]");
    const refresh = () => {
      const selected = choices.filter((choice) => choice.checked).length;
      count.textContent = `${selected} selected`;
      submit.disabled = selected === 0;
      selectAll.checked = selected > 0 && selected === choices.length;
      selectAll.indeterminate = selected > 0 && selected < choices.length;
    };
    selectAll.addEventListener("change", () => {
      choices.forEach((choice) => { choice.checked = selectAll.checked; });
      refresh();
    });
    choices.forEach((choice) => choice.addEventListener("change", refresh));
    refresh();
  });

  const deliveryScroll = document.querySelector("[data-delivery-scroll]");
  if (deliveryScroll) {
    const sizeDeliveryLog = () => {
      const rows = [...deliveryScroll.children];
      if (rows.length <= 4) {
        deliveryScroll.style.maxHeight = "none";
        deliveryScroll.removeAttribute("tabindex");
        return;
      }
      const visibleHeight = rows.slice(0, 4).reduce(
        (height, row) => height + row.getBoundingClientRect().height,
        0
      );
      deliveryScroll.style.maxHeight = `${Math.ceil(visibleHeight)}px`;
    };
    sizeDeliveryLog();
    window.addEventListener("resize", sizeDeliveryLog);
  }
})();

(() => {
  document.querySelectorAll("img[data-artwork-fallback]").forEach((image) => {
    image.addEventListener("error", () => {
      if (image.dataset.artworkFallbackApplied) return;
      const fallback = image.dataset.artworkFallback;
      if (!fallback || image.currentSrc === fallback || image.src === fallback) return;
      image.dataset.artworkFallbackApplied = "true";
      image.removeAttribute("srcset");
      image.src = fallback;
    }, { once: false });
  });

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

  const providerHealth = document.querySelector("[data-provider-health]");
  if (providerHealth) {
    const refreshURL = providerHealth.dataset.refreshUrl;
    const refreshStatus = document.querySelector("[data-health-refresh-status]");
    let refreshTimer;
    let countdownTimer;

    const displayCountdown = (value) => {
      if (!value) return "";
      const remaining = new Date(value).getTime() - Date.now();
      if (!Number.isFinite(remaining)) return "";
      if (remaining <= 0) return "due now";
      const seconds = Math.ceil(remaining / 1000);
      if (seconds < 60) return `in ${seconds}s`;
      const minutes = Math.ceil(seconds / 60);
      if (minutes < 60) return `in ${minutes}m`;
      const hours = Math.floor(minutes / 60);
      const remainder = minutes % 60;
      return remainder ? `in ${hours}h ${remainder}m` : `in ${hours}h`;
    };

    const updateCountdowns = () => {
      providerHealth.querySelectorAll("[data-health-countdown]").forEach((node) => {
        node.textContent = displayCountdown(node.dataset.healthCountdown);
      });
    };

    const appendTimestamp = (row, label, iso, display, countdown) => {
      if (!iso) return;
      const detail = document.createElement("small");
      detail.append(document.createTextNode(`${label} `));
      const time = document.createElement("time");
      time.dateTime = iso;
      time.textContent = display || iso;
      detail.append(time);
      if (countdown) {
        detail.append(document.createTextNode(" "));
        const remaining = document.createElement("span");
        remaining.dataset.healthCountdown = iso;
        detail.append(remaining);
      }
      row.append(detail);
    };

    const render = (providers) => {
      providerHealth.replaceChildren();
      if (!providers.length) {
        const empty = document.createElement("div");
        empty.className = "empty";
        empty.textContent = "No provider health data yet.";
        providerHealth.append(empty);
        return;
      }
      const allowedClasses = new Set(["sent", "ambiguous", "failed"]);
      providers.forEach((provider) => {
        const row = document.createElement("div");
        row.className = "health-row";
        const heading = document.createElement("div");
        heading.className = "health-heading";
        const name = document.createElement("strong");
        name.textContent = provider.provider;
        const badge = document.createElement("span");
        badge.className = `badge ${allowedClasses.has(provider.status_class) ? provider.status_class : "failed"}`;
        badge.dataset.healthStatus = "";
        badge.textContent = provider.status;
        heading.append(name, badge);
        row.append(heading);
        appendTimestamp(row, "Last success", provider.last_success_at, provider.last_success_display, false);
        appendTimestamp(row, "Last failure", provider.last_failure_at, provider.last_failure_display, false);
        appendTimestamp(row, "Next check", provider.next_check_at, provider.next_check_display, true);
        if (provider.last_error) {
          const error = document.createElement("small");
          error.className = "health-error";
          error.textContent = provider.last_error;
          row.append(error);
        }
        appendTimestamp(row, "Updated", provider.updated_at, provider.updated_display, false);
        providerHealth.append(row);
      });
      updateCountdowns();
    };

    const refresh = async () => {
      try {
        const response = await fetch(refreshURL, {
          headers: { Accept: "application/json" },
          cache: "no-store",
        });
        if (!response.ok) throw new Error(`provider health request failed (${response.status})`);
        render(await response.json());
        if (refreshStatus) refreshStatus.textContent = "Updated just now";
      } catch (_) {
        if (refreshStatus) refreshStatus.textContent = "Live refresh unavailable";
      }
    };

    refresh();
    refreshTimer = window.setInterval(refresh, 30000);
    countdownTimer = window.setInterval(updateCountdowns, 1000);
    window.addEventListener("pagehide", () => {
      window.clearInterval(refreshTimer);
      window.clearInterval(countdownTimer);
    }, { once: true });
  }
})();

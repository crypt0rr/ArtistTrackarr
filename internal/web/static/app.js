(() => {
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
})();

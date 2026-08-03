// Apply the saved theme before the stylesheet is painted. This file is served
// from the same origin so it remains compatible with the strict CSP.
try {
  if (localStorage.getItem("artist-trackarr-theme") === "dark") {
    document.documentElement.dataset.theme = "dark";
  }
} catch (_) {}

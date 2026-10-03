// Loaded in <head> without defer: set the theme before the first paint.
// Basecoat stores the choice in localStorage "themeMode" (basecoat.theme.set).
(function () {
  var mode = null;
  try { mode = localStorage.getItem("themeMode"); } catch (e) {}
  if (mode === "dark" || (!mode && window.matchMedia("(prefers-color-scheme: dark)").matches)) {
    document.documentElement.classList.add("dark");
  }
})();

function toast(category, title) {
  var t = document.getElementById("toaster");
  if (t && t.toast) t.toast({ category: category, title: title });
}

document.addEventListener("click", function (e) {
  var el = e.target.closest("[data-theme-toggle],[data-copy],[data-reveal]");
  if (!el) return;
  if (el.hasAttribute("data-theme-toggle")) {
    window.basecoat && window.basecoat.theme.toggle();
  } else if (el.dataset.copy) {
    var v = document.getElementById(el.dataset.copy).value;
    navigator.clipboard.writeText(v).then(
      function () { toast("success", "Copied"); },
      function () { toast("error", "Copy failed"); });
  } else if (el.dataset.reveal) {
    var input = document.getElementById(el.dataset.reveal);
    var show = input.type === "password";
    input.type = show ? "text" : "password";
    el.textContent = show ? "Hide" : "Show";
  }
});

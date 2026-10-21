// i18n — 中英文切换
const I18N = { zh: {}, en: {} };
let currentLang = "zh";

function t(key) {
  return (I18N[currentLang] && I18N[currentLang][key]) || I18N.zh[key] || key;
}

function applyI18n() {
  document.querySelectorAll("[data-i18n]").forEach((el) => {
    const k = el.getAttribute("data-i18n");
    const v = t(k);
    if (v) el.textContent = v;
  });
  document.querySelectorAll("[data-i18n-title]").forEach((el) => {
    const k = el.getAttribute("data-i18n-title");
    const v = t(k);
    if (v) el.title = v;
  });
  document.querySelectorAll("[data-i18n-placeholder]").forEach((el) => {
    const k = el.getAttribute("data-i18n-placeholder");
    const v = t(k);
    if (v) el.placeholder = v;
  });
  document.querySelectorAll("[data-i18n-aria]").forEach((el) => {
    const k = el.getAttribute("data-i18n-aria");
    const v = t(k);
    if (v) el.setAttribute("aria-label", v);
  });
  document.documentElement.lang = currentLang === "zh" ? "zh-CN" : "en";
}

async function loadLang() {
  try {
    const r = await fetch("/api/ui");
    const d = await r.json();
    currentLang = d.lang || "zh";
  } catch (_) {}
  applyI18n();
}

async function setLang(lang) {
  currentLang = lang;
  try {
    await fetch("/api/ui", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ lang }),
    });
  } catch (_) {}
  applyI18n();
}

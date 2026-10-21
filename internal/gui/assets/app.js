"use strict";

const $ = (s, el = document) => el.querySelector(s);
const esc = (s) => String(s ?? "").replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));

async function api(path, opts = {}) {
  const r = await fetch(path, {
    ...opts,
    headers: opts.body ? { "Content-Type": "application/json" } : undefined,
  });
  const j = await r.json().catch(() => ({}));
  if (!r.ok) throw new Error(j.error || r.statusText);
  return j;
}

const STATUS = {
  working: "工作中", idle: "空闲", waiting: "等待", checking: "检查中", approval: "待批准",
  done: "完成", rejected: "打回", failed: "失败", stopped: "已停止",
};
const status = (s) => `<span class="state"><i class="dot ${esc(s)}"></i>${STATUS[s] || esc(s)}</span>`;

// 桌面窗口里左上角有红绿灯，品牌行要往下让一点；浏览器里（morula web）不用。
if (!/^https?:$/.test(location.protocol) || location.hostname === "wails.localhost" || window._wails) {
  document.body.classList.add("native");
  // Wails 的运行时处理 --wails-draggable：按住标题栏、侧栏顶部拖动窗口，双击缩放
  const rt = document.createElement("script");
  rt.type = "module";
  rt.src = "/wails/runtime.js";
  document.head.append(rt);
  // 全屏时红绿灯藏起来，侧栏收窄、顶上不留空。窗口和屏幕一样大就当作全屏
  const fs = () => document.body.classList.toggle("fs", innerWidth >= screen.width && innerHeight >= screen.height);
  addEventListener("resize", fs);
  fs();
}

const store = {
  get(k) { try { return localStorage.getItem(k); } catch { return null; } },
  set(k, v) { try { localStorage.setItem(k, v); } catch {} },
};
/* ---------- 字体大小 ---------- */
// CSS 里的字号都乘 --fz；13px 为 1。先用本机缓存的值，免得打开时字号跳一下，再以设置为准。
function applyFont(n) {
  n = [13, 14, 15, 16].includes(+n) ? +n : 14;
  document.documentElement.style.setProperty("--fz", String(n / 13));
  store.set("font", n);
  const el = document.querySelector("#font-seg");
  if (el?.childElementCount) setSeg(el, String(n));
}
applyFont(store.get("font"));

/* ---------- 分段控件 ---------- */
// el 上记当前值（data-value），点按钮时改值并在 el 上发 change 事件。
function seg(el, opts, value) {
  el.innerHTML = opts.map((o) => `<button type="button" data-v="${esc(o.v)}">${esc(o.l)}</button>`).join("");
  setSeg(el, value);
}
function setSeg(el, value) {
  el.dataset.value = value ?? "";
  el.querySelectorAll("button").forEach((b) => b.classList.toggle("on", b.dataset.v === String(value ?? "")));
}
document.addEventListener("click", (e) => {
  const b = e.target.closest(".seg button");
  if (!b) return;
  const el = b.parentElement;
  if (el.dataset.value === b.dataset.v) return;
  setSeg(el, b.dataset.v);
  el.dispatchEvent(new Event("change", { bubbles: true }));
});

/* ---------- 切页 ---------- */
const VIEWS = ["chat", "projects", "routines", "agents", "staff"]; // 前四个对应 ⌘1–⌘4
const PANES = ["settings", "approval", "memory", "import", "harness", "subs", "providers", "plugins"]; // 设置弹窗里的页；subs 是「账号」页的旧名
let view = "chat";
let pane = "";               // 设置弹窗当前的页；弹窗关着时为空
function show(v) {
  if (PANES.includes(v)) return openPrefs(v);
  if (!VIEWS.includes(v)) v = "chat";
  closePrefs();
  view = v;
  const lit = v === "staff" ? "agents" : v; // 配置页亮 Agent
  document.querySelectorAll("#nav button[data-view], #nav-foot button[data-view]").forEach((b) => b.classList.toggle("on", b.dataset.view === lit));
  document.querySelectorAll(".view").forEach((x) => x.classList.toggle("on", x.id === "v-" + v));
  if (v !== "projects") closeTask();
  dockChat(v === "projects" ? curProject : 0);
  store.set("view", v);
  if (v === "agents") guard(v, loadAgents);
  if (v === "projects") guard(v, loadProjects);
  if (v === "staff") guard(v, loadStaff);
  if (v === "routines") guard(v, loadRoutines);
  if (v === "chat") $("#input").focus();
  renderSide();
}
function openPrefs(p) {
  if (p === "subs") p = "providers"; // 「订阅」并进了「账号」
  pane = p;
  $("#prefs").hidden = false;
  $("#nav-foot [data-view=settings]").classList.add("on");
  document.querySelectorAll("#prefs-nav button").forEach((b) => b.classList.toggle("on", b.dataset.pane === p));
  document.querySelectorAll(".pane").forEach((x) => x.classList.toggle("on", x.id === "v-" + p));
  guard(p, { plugins: openPlugins, providers: loadAccounts, settings: loadSettings, harness: loadHarnessPane, memory: loadMemory, approval: loadApproval, import: loadImport }[p]);
}

/* ---------- 出错边界 ---------- */
// guard 是一页的出错边界：加载函数抛错或返回的 promise 失败时，只这一页盖上出错卡片和重试按钮，别的页照常。
function guard(id, fn) {
  const sec = $("#v-" + id);
  const col = sec?.querySelector(".col");
  col?.querySelector(":scope > .pg-broke")?.remove();
  col?.querySelectorAll(":scope > [data-broke-hid]").forEach((x) => { x.hidden = false; delete x.dataset.brokeHid; });
  if (!fn) return;
  const broke = (err) => {
    console.error("[morula] " + id + " 页出错", err);
    if (!col) return;
    [...col.children].forEach((x) => { if (!x.hidden) { x.hidden = true; x.dataset.brokeHid = "1"; } });
    col.insertAdjacentHTML("afterbegin", `<div class="card list pg-broke"><div class="row"><div class="pref-l"><div class="name">这一页出错了</div>
      <div class="sub err-t">${esc(err?.message || String(err))}</div></div><button class="btn" type="button">重试</button></div></div>`);
    col.querySelector(".pg-broke button").addEventListener("click", () => guard(id, fn));
  };
  try {
    const r = fn();
    if (r && typeof r.catch === "function") r.catch(broke);
  } catch (err) { broke(err); }
}
function closePrefs() {
  if (!pane) return;
  pane = "";
  $("#prefs").hidden = true;
  $("#nav-foot [data-view=settings]").classList.remove("on");
  if (setupBack) { setupBack = false; openSetup(obStep); } // 从接入弹窗去账号页登录后回来
}
$("#prefs-nav").addEventListener("click", (e) => {
  const b = e.target.closest("button[data-pane]");
  if (b) openPrefs(b.dataset.pane);
});
$("#prefs-close").addEventListener("click", closePrefs);
$("#prefs").addEventListener("mousedown", (e) => { if (e.target.id === "prefs") closePrefs(); });
document.querySelectorAll("#nav, #nav-foot").forEach((n) => n.addEventListener("click", (e) => {
  const b = e.target.closest("button[data-view]");
  if (!b) return;
  if (b.dataset.view === "projects") { curProject = 0; store.set("project", 0); }
  show(b.dataset.view);
}));
// 输入法正在选字时按的回车不算发送。WKWebView 里选字的那一下回车先结束输入法再发 keydown，
// 这时 isComposing 已经是 false，只能看 keyCode 是不是 229
const ime = (e) => e.isComposing || e.keyCode === 229;
document.addEventListener("keydown", (e) => {
  if (e.metaKey && e.key >= "1" && e.key <= "4") {
    e.preventDefault();
    show(VIEWS[e.key - 1]);
  }
  if (e.metaKey && e.key === ",") { e.preventDefault(); show(pane || "settings"); }
  if (e.metaKey && e.key.toLowerCase() === "k") { e.preventDefault(); return skOpen(); }
  if (e.metaKey && e.key === "\\") { e.preventDefault(); return fold(); }
  if (e.key === "Escape") {
    if (!$("#sk").hidden) skClose();
    else if (!$("#rt-dlg").hidden) rtClose();
    else if (!$("#agent-dlg").hidden) closeAgentForm();
    else if (!$("#proj-dlg").hidden) closeProjectForm();
    else if (pane) closePrefs(); else if (curTask) closeTask();
  }
});
// 任何地方的 data-goto 链接都跳到项目页
document.addEventListener("click", (e) => {
  const a = e.target.closest("[data-goto]");
  if (!a) return;
  e.preventDefault();
  openProject(+a.dataset.goto);
});
// 项目链接显示项目名。事件里没带名字、也还没查到时先写「项目 #编号」，查到项目列表后换成名字。
const projLink = (id, name) => {
  name = name || names[id];
  if (!name) fillNames();
  return `<a href="#" class="plink" data-goto="${+id}">${esc(name || "项目 #" + id)}</a>`;
};
let naming = null;
// fillNames 查一次项目列表，记下名字，并把页面上所有项目链接换成现在的名字（项目改过名也跟着换）。
function fillNames(list) {
  const apply = (l) => {
    for (const p of l || []) names[p.id] = p.name;
    document.querySelectorAll(".plink[data-goto]").forEach((a) => {
      const n = names[a.dataset.goto];
      if (n && a.textContent !== n) a.textContent = n;
    });
  };
  if (list) return apply(list);
  naming ||= api("/api/projects").then(apply).catch(() => {}).finally(() => { naming = null; });
}

/* ---------- 助理 ---------- */
const chat = $("#chat");
// 跟着最新消息走：停在底部附近时，内容变多（新消息、图片加载完）或对话区变矮（流程图、问题卡片展开）
// 都滚到底。用户往上翻了就不跟，翻回底部或自己发消息后再跟。
let follow = true;
function toBottom() { if (follow) chat.scrollTop = chat.scrollHeight; }
chat.addEventListener("scroll", () => { follow = chat.scrollHeight - chat.scrollTop - chat.clientHeight < 80; tickOn(); });

// 快速定位：右边每条用户消息一道刻度，最多列最近 30 条；滚到哪条，哪条的刻度加深
const ticksEl = $("#ticks");
let ticksRAF = 0;
function paintTicks() {
  if (ticksRAF) return;
  ticksRAF = requestAnimationFrame(() => {
    ticksRAF = 0;
    const ms = [...chat.querySelectorAll(".msg.user[data-turn]")].slice(-30);
    ticksEl.hidden = ms.length < 2;
    ticksEl.innerHTML = ms.map((m) => {
      const t = (m.querySelector(".body")?.textContent || "").trim().split("\n")[0] || "（附件）";
      return `<button type="button" class="tick" data-tick="${esc(m.dataset.turn)}" aria-label="${esc(t)}"><i></i><span class="tick-tip">${esc(t)}</span></button>`;
    }).join("");
    tickOn();
  });
}
function tickOn() {
  if (ticksEl.hidden) return;
  const line = chat.getBoundingClientRect().top + chat.clientHeight / 3;
  let on = null;
  for (const b of ticksEl.children) {
    const m = chat.querySelector(`.msg.user[data-turn="${CSS.escape(b.dataset.tick)}"]`);
    if (m && m.getBoundingClientRect().top <= line) on = b;
  }
  on ||= ticksEl.firstElementChild;
  for (const b of ticksEl.children) b.classList.toggle("on", b === on);
}
ticksEl.addEventListener("click", (e) => {
  const b = e.target.closest("[data-tick]");
  const m = b && chat.querySelector(`.msg.user[data-turn="${CSS.escape(b.dataset.tick)}"]`);
  if (!m) return;
  follow = false;
  m.scrollIntoView({ block: "start", behavior: "smooth" });
});
// 对话里增删了用户消息（包括按天读回旧记录、清空）时重画；流式输出改的是回复里面，不碰这里
new MutationObserver((rs) => {
  const hit = (n) => n.nodeType === 1 && (n.matches(".msg.user") || n.querySelector?.(".msg.user"));
  if (rs.some((r) => [...r.addedNodes].some(hit) || [...r.removedNodes].some(hit))) paintTicks();
}).observe(chat, { childList: true, subtree: true });
new ResizeObserver(toBottom).observe(chat);
new MutationObserver(toBottom).observe(chat, { childList: true, subtree: true, characterData: true });
chat.addEventListener("load", toBottom, true);
let target = chat; // 渲染到哪里：平时是 chat，加载历史时是一个临时容器，带 turn 的事件是那条消息下面的容器
let inHistory = false; // 正在渲染更早的日子：卡片都已失效，不进下方的回答区
let replay = false; // 正在画打开页面时读回的事件
// 每条用户消息下面一个容器，这条消息的处理过程、卡片、回复都画在里面（事件的 turn 字段）。
// 几条消息并行时，各自的内容留在各自的消息下面，不按到达顺序交错。
const threads = {}; // 用户消息 id -> { box, msg, text }
let lastDay = "";  // 上一条事件属于哪天，跨天时插一条日期分隔线
const input = $("#input");

// 链接只认网页和本机的绝对路径，点了交给 morula 打开（open.go），页面自己不跳转。
const LINK = /\[([^\]\n]+)\]\((?:&lt;([^\n]+?)&gt;|([^\s()]+))\)/g;
const linkable = (t) => /^(https?:\/\/|file:\/\/|\/|~\/)/.test(t);
document.addEventListener("click", async (e) => {
  const a = e.target.closest("[data-open]");
  if (!a) return;
  e.preventDefault();
  try { await api("/api/open", { method: "POST", body: JSON.stringify({ link: a.dataset.open }) }); }
  catch (err) { a.title = err.message; a.classList.add("bad"); }
});
// Markdown：标题、段落、列表（有序、无序、任务、嵌套）、引用、表格、分隔线、代码块，
// 行内的代码、粗体、斜体、删除线、链接。先转义再加标签，不认 HTML。
function mdInline(s) {
  // 链接和行内代码先换成占位符，路径里的 ` 和 * 不再被当成格式
  const keep = [];
  const hold = (h) => `\u0000${keep.push(h) - 1}\u0000`;
  s = esc(s)
    .replace(/`([^`\n]+)`/g, (_, c) => hold(`<code>${c}</code>`))
    .replace(LINK, (m, text, angled, bare) => {
      const t = angled ?? bare;
      return linkable(t) ? hold(`<a href="#" class="flink" data-open="${t}" title="${t}">${text}</a>`) : m;
    })
    .replace(/(^|[\s(（])(https?:\/\/[^\s<>()（）]+[^\s<>()（）.,;:!?。，；：！？])/g, (_, pre, u) => pre + hold(`<a href="#" class="flink" data-open="${u}" title="${u}">${u}</a>`))
    .replace(/\*\*([^*\n]+?)\*\*/g, "<b>$1</b>")
    .replace(/__([^_\n]+?)__/g, "<b>$1</b>")
    .replace(/(^|[^*\w])\*([^*\s][^*\n]*?)\*(?!\*)/g, "$1<i>$2</i>")
    .replace(/~~([^~\n]+?)~~/g, "<s>$1</s>");
  return s.replace(/\u0000(\d+)\u0000/g, (_, n) => keep[n]);
}
const MD_LI = /^(\s*)([-*+]|\d{1,3}[.)])\s+(.*)$/;
const MD_ROW = (l) => /^\s*\|.*\|\s*$/.test(l);
const mdCells = (l) => l.trim().replace(/^\||\|$/g, "").split(/(?<!\\)\|/).map((c) => c.trim().replace(/\\\|/g, "|"));
function mdList(lines) {
  // lines 是连续的列表行（含缩进的续行）。缩进比上一项深的算上一项的子列表
  const root = [], stack = [];
  for (const l of lines) {
    const m = l.match(MD_LI);
    if (!m) { // 续行接在上一项后面
      const top = stack.at(-1)?.list.at(-1);
      if (top) top.text += "\n" + l.trim();
      continue;
    }
    const ind = m[1].replace(/\t/g, "  ").length, ord = /\d/.test(m[2]);
    const item = { text: m[3], kids: [], ord, start: ord ? parseInt(m[2]) : 1 };
    while (stack.length > 1 && ind < stack.at(-1).ind) stack.pop();
    if (!stack.length) stack.push({ ind, list: root });
    else if (ind > stack.at(-1).ind && stack.at(-1).list.length) stack.push({ ind, list: stack.at(-1).list.at(-1).kids });
    stack.at(-1).list.push(item);
  }
  const render = (items) => {
    if (!items.length) return "";
    const ord = items[0].ord, tag = ord ? "ol" : "ul";
    const start = ord && items[0].start !== 1 ? ` start="${items[0].start}"` : "";
    return `<${tag}${start}>` + items.map((it) => {
      let t = it.text, box = "";
      const tm = t.match(/^\[([ xX])\]\s+(.*)$/s);
      if (tm) { box = `<span class="md-box${tm[1] === " " ? "" : " on"}" aria-hidden="true"></span>`; t = tm[2]; }
      return `<li${tm ? ' class="md-task"' : ""}>${box}${mdInline(t).replace(/\n/g, "<br>")}${render(it.kids)}</li>`;
    }).join("") + `</${tag}>`;
  };
  return render(root);
}
function md(src) {
  const lines = String(src ?? "").replace(/\r\n?/g, "\n").split("\n");
  const out = [];
  let para = [];
  const flush = () => { if (para.length) out.push(`<p>${para.map(mdInline).join("<br>")}</p>`); para = []; };
  for (let i = 0; i < lines.length; i++) {
    const l = lines[i];
    let m;
    if ((m = l.match(/^\s*(```+|~~~+)\s*([\w+-]*)/))) { // 代码块，没收尾时到结尾
      flush();
      const fence = m[1], body = [];
      for (i++; i < lines.length && !lines[i].trim().startsWith(fence); i++) body.push(lines[i]);
      out.push(`<pre><code>${esc(body.join("\n"))}</code></pre>`);
      continue;
    }
    if (!l.trim()) { flush(); continue; }
    if ((m = l.match(/^\s{0,3}(#{1,6})\s+(.*?)\s*#*\s*$/))) {
      flush();
      const n = Math.min(m[1].length, 4);
      out.push(`<div class="md-h h${n}">${mdInline(m[2])}</div>`);
      continue;
    }
    if (/^\s{0,3}([-*_])(\s*\1){2,}\s*$/.test(l) && !para.length) { flush(); out.push("<hr>"); continue; }
    if (MD_ROW(l) && i + 1 < lines.length && /^\s*\|?\s*:?-{2,}:?\s*(\|\s*:?-{2,}:?\s*)*\|?\s*$/.test(lines[i + 1])) {
      flush();
      const head = mdCells(l), al = mdCells(lines[i + 1]).map((c) => c.endsWith(":") ? (c.startsWith(":") ? "center" : "right") : "");
      const td = (tag, c, k) => `<${tag}${al[k] ? ` style="text-align:${al[k]}"` : ""}>${mdInline(c)}</${tag}>`;
      let h = `<div class="md-tbl"><table><thead><tr>${head.map((c, k) => td("th", c, k)).join("")}</tr></thead><tbody>`;
      for (i += 2; i < lines.length && MD_ROW(lines[i]); i++) h += `<tr>${mdCells(lines[i]).map((c, k) => td("td", c, k)).join("")}</tr>`;
      i--;
      out.push(h + "</tbody></table></div>");
      continue;
    }
    if (/^\s{0,3}>/.test(l)) {
      flush();
      const q = [];
      for (; i < lines.length && /^\s{0,3}>/.test(lines[i]); i++) q.push(lines[i].replace(/^\s{0,3}>\s?/, ""));
      i--;
      out.push(`<blockquote>${md(q.join("\n"))}</blockquote>`);
      continue;
    }
    if (MD_LI.test(l)) {
      flush();
      const items = [];
      for (; i < lines.length; i++) {
        const x = lines[i];
        if (MD_LI.test(x) || (items.length && /^\s+\S/.test(x))) items.push(x);
        else break;
      }
      i--;
      out.push(mdList(items));
      continue;
    }
    para.push(l.trim());
  }
  flush();
  return `<div class="md">${out.join("")}</div>`;
}
const toolName = (n) => String(n).replace(/^mcp__morula__/, "morula · ");

let epoch = 0, seq = 0, progress = null;
let summaries = {}, earlier = []; // 每天的总结；还没加载的更早日期（从近到远）
const names = {}; // 项目编号 -> 名字，从 route 事件里收集
const tools = {};
// 助理的处理过程（activity 事件）：一组过程画成一块，
// 结束后折叠成一行「处理过程 · N 步 · T 秒」（start 带 label 时用 label，例如「验收过程」）。中间插进卡片、归入项目这类事件时，后面的过程另起一块，
// 折叠时整组一起折。
const acts = {}; // 组号 -> { segs, steps, start, last, status, timer, ended }
const TOOL_LABEL = { list_projects: "查项目", list_agents: "查团队", read_project: "读项目", ask_user: "问你",
  create_project: "新建项目", dispatch: "派活", propose_team_change: "提议改团队", update_project: "更新项目记忆",
  rename_project: "改项目名", merge_projects: "合并项目", undo_merge: "撤销合并", move_runs: "挪安排", delete_project: "删项目",
  list_harnesses: "查模型", transfer: "转接", run_command: "运行命令", update_memory: "更新记忆", search_chats: "找对话",
  list_routines: "查例行事项", save_routine: "存例行事项", remove_routine: "删例行事项" };
const WAIT_LABEL = { ask_user: "等你回答", create_project: "等你定项目名", propose_team_change: "等你确认改动",
  merge_projects: "等你确认合并", delete_project: "等你确认删除", remove_routine: "等你确认删除" };
const toolLabel = (t) => TOOL_LABEL[t] || t;
// 工具参数的一句话：问题、项目名、任务个数，其余取第一个字符串值。
function argsBrief(tool, raw) {
  let a;
  try { a = JSON.parse(raw || "{}"); } catch { return raw || ""; }
  if (tool === "ask_user") return a.question || "";
  if (tool === "create_project") return a.name || "";
  if (tool === "dispatch") return `${(a.tasks || []).length} 个任务` + (a.tasks?.length ? "：" + a.tasks.map((t) => t.title).join("；") : "");
  if (a.project_id) return `项目 ${a.project_id}`;
  const v = Object.values(a).find((x) => typeof x === "string" && x);
  return v || "";
}
function actTick(a) {
  const st = a.segs[a.segs.length - 1]?.querySelector(".proc-st span");
  if (st) st.textContent = `${a.status} · ${Math.max(0, Math.round((Date.now() - a.start) / 1000))} 秒`;
}
// actSeg 返回这组过程现在该往里加的一块：还在对话末尾就接着用，否则另起一块，状态行挪过去。
function actSeg(a) {
  let seg = a.segs[a.segs.length - 1];
  if (seg && target.lastElementChild === seg) return seg;
  seg?.querySelector(".proc-st")?.remove();
  seg = document.createElement("div");
  seg.className = "proc live";
  seg.innerHTML = `<button type="button" class="proc-h"></button>` +
    (a.ended ? "" : `<div class="proc-st"><i class="dot working"></i><span></span></div>`) + `<div class="proc-b"></div>`;
  target.appendChild(seg);
  a.segs.push(seg);
  actTick(a);
  return seg;
}
// 过程里的行分两层：助理说的话和思考各占一行；工具、命令、改文件是「操作」，连着的几步收进一组，
// 组头写几步、哪几样、有没有失败。正在进行的那组展开，后面接上别的行或整个过程结束时收起，用户点开过的不动。
const ACT_IC = {
  think: '<path d="M10 3.5l1.6 3.9 3.9 1.6-3.9 1.6L10 14.5l-1.6-3.9L4.5 9l3.9-1.6z"/>',
  cmd: '<path d="M4 6l4 4-4 4M10 14h6"/>',
  tool: '<path d="M12.5 4.5a3.5 3.5 0 0 0-4.3 4.3L4 13l3 3 4.2-4.2a3.5 3.5 0 0 0 4.3-4.3l-2.2 2.2-2.1-.6-.6-2.1z"/>',
  file: '<path d="M5 15l1-3.5L13.5 4l2.5 2.5L8.5 14z"/>',
};
const actIc = (k) => `<svg class="act-ic" viewBox="0 0 20 20" aria-hidden="true">${ACT_IC[k]}</svg>`;
function actGroupHead(g) {
  const rows = g.querySelectorAll(":scope > .act-grp-b > .act-row");
  const bad = g.querySelectorAll(":scope > .act-grp-b > .act-row .act-st.bad").length;
  const open = g.querySelectorAll(":scope > .act-grp-b > .act-row:not(.fin)").length;
  const kinds = [...new Set([...rows].map((r) => r.dataset.k))].slice(0, 4);
  g.classList.toggle("solo", rows.length < 2);
  g.querySelector(".act-gs").innerHTML = `<span class="chev">›</span><span>${rows.length} 步操作</span>` +
    `<span class="muted clip mono">${esc(kinds.join("、"))}</span>` +
    (open ? '<i class="dot working"></i>' : bad ? `<span class="act-st bad">${bad} 步失败</span>` : "");
}
function actShut(seg) {
  seg.querySelectorAll(".act-grp:not([data-user])").forEach((g) => g.classList.add("shut"));
}
function actRow(a, html, op) {
  const seg = actSeg(a), b = seg.querySelector(".proc-b");
  let last = b.lastElementChild;
  if (op) {
    if (!last?.classList.contains("act-grp")) {
      b.insertAdjacentHTML("beforeend", '<div class="act-grp"><button type="button" class="act-gs"></button><div class="act-grp-b"></div></div>');
      last = b.lastElementChild;
    }
    last.querySelector(".act-grp-b").insertAdjacentHTML("beforeend", html);
    actGroupHead(last);
  } else {
    actShut(seg);
    b.insertAdjacentHTML("beforeend", html);
  }
  seg.dataset.n = (+seg.dataset.n || 0) + 1;
  a.steps++;
}
// 找这组过程里最后一个还没收尾的工具或命令行。
function actOpen(a, sel) {
  for (let i = a.segs.length - 1; i >= 0; i--) {
    const rows = a.segs[i].querySelectorAll(sel + ":not(.fin)");
    if (rows.length) return rows[rows.length - 1];
  }
  return null;
}
function actFinish(row, st, bad, out) {
  if (!row) return;
  row.classList.add("fin");
  const s = row.querySelector(".act-st");
  s.textContent = bad ? st : ""; // 成功只画一个勾，失败写出来
  s.classList.toggle("bad", !!bad);
  s.classList.toggle("ok", !bad);
  if (out) row.insertAdjacentHTML("beforeend", `<pre class="tool-out">${esc(out)}</pre>`);
  const g = row.closest(".act-grp");
  if (g) actGroupHead(g);
}
function activity(e) {
  const ts = Date.parse(e.ts) || null;
  let a = acts[e.group];
  if (!a) a = acts[e.group] = { segs: [], steps: 0, start: ts || Date.now(), last: ts || Date.now(), status: "正在思考", timer: null, ended: false, turn: e.turn || "" };
  if (ts) a.last = ts;
  if (a.ended) return;
  switch (e.kind) {
    case "start":
      a.status = e.text || "正在思考";
      a.label = e.label || "处理过程";
      actSeg(a);
      if (!SHOT) a.timer = setInterval(() => actTick(a), 1000);
      break;
    case "message":
      a.status = "正在思考";
      actRow(a, `<div class="act-msg">${md(e.text || "")}</div>`);
      break;
    case "thinking": {
      const first = (e.text || "").split("\n")[0].replace(/\*\*/g, "");
      a.status = "正在思考";
      actRow(a, `<details class="act-row act-think"><summary>${actIc("think")}<span class="act-k">思考</span><span class="clip">${esc(first)}</span></summary><div class="act-x">${md(e.text || "")}</div></details>`);
      break;
    }
    case "tool":
      a.status = WAIT_LABEL[e.tool] || "正在" + toolLabel(e.tool);
      actRow(a, `<details class="act-row act-tool" data-tool="${esc(e.tool)}" data-k="${esc(toolLabel(e.tool))}"><summary>${actIc("tool")}<span class="act-k">${esc(toolLabel(e.tool))}</span>` +
        `<span class="clip">${esc(argsBrief(e.tool, e.args))}</span><span class="act-st"></span></summary><pre class="tool-out">${esc(e.args || "")}</pre></details>`, true);
      break;
    case "tool_done":
      a.status = "正在思考";
      actFinish(actOpen(a, `.act-tool[data-tool="${CSS.escape(e.tool || "")}"]`), e.failed ? "失败" : "完成", e.failed, e.text);
      break;
    case "command":
      a.status = "正在执行命令";
      actRow(a, `<details class="act-row act-cmd" data-k="命令"><summary>${actIc("cmd")}<span class="clip mono">${esc(e.text || "")}</span><span class="act-st"></span></summary></details>`, true);
      break;
    case "done":
      a.status = "正在思考";
      actFinish(actOpen(a, ".act-cmd"), e.code ? "没成功" : "完成", !!e.code, e.text);
      break;
    case "file":
      actRow(a, `<details class="act-row act-file fin" data-k="改文件"><summary>${actIc("file")}<span class="act-k">改文件</span><span class="clip mono">${esc((e.text || "").split("\n").join("、"))}</span></summary></details>`, true);
      break;
    case "end": {
      a.ended = true;
      clearInterval(a.timer);
      const secs = Math.max(0, Math.round((a.last - a.start) / 1000));
      const tail = { failed: " · 失败", stopped: " · 已停止", rejected: " · 没通过" }[e.state] || "";
      if (e.drop) { // 先显示出来的那句其实是回复，回复另有气泡
        const seg = a.segs[a.segs.length - 1], rows = seg?.querySelectorAll(".act-msg");
        if (rows?.length) { rows[rows.length - 1].remove(); seg.dataset.n = +seg.dataset.n - 1; a.steps--; }
      }
      a.segs.forEach((seg, i) => {
        seg.querySelector(".proc-st")?.remove();
        actShut(seg);
        seg.querySelectorAll(".act-row:not(.fin) .act-st").forEach((st) => { st.closest(".act-row").classList.add("fin"); st.textContent = "没结束"; }); // 过程结束时还没收到结果的操作
        seg.querySelectorAll(".act-grp").forEach(actGroupHead);
        if (!+seg.dataset.n) { seg.remove(); return; }
        seg.classList.remove("live");
        seg.classList.add("folded");
        const last = i === a.segs.length - 1;
        seg.querySelector(".proc-h").innerHTML = `<span class="chev">›</span>${esc(a.label || "处理过程")} · ${seg.dataset.n} 步` + (last ? ` · ${secs} 秒${tail}` : "");
      });
      break;
    }
  }
}
// bubble 画一条消息。不是用户的消息，名字前面画说话人的头像（和 Agent 页同一套颜色和图案）；
// 子 agent 说的话左边再加一条它颜色的竖线，和助理说的话分开。
function bubble(role, who) {
  const d = document.createElement("div");
  d.className = "msg " + role;
  if (role === "user") {
    d.innerHTML = `<div class="who">你</div><div class="body"></div>`;
  } else {
    const me = agentList.find((a) => a.role === "助理")?.name || "助理", name = who || me;
    const [c, t] = lookFor(name, me);
    if (name !== me) d.classList.add("peer");
    d.style.cssText = hueVars(c);
    d.innerHTML = `<div class="who"><span class="face" aria-hidden="true">${berry(t)}</span>` +
      `<b class="wn">${esc(name)}</b></div><div class="body"></div>`;
  }
  target.appendChild(d);
  return d;
}
function note(cls, html) {
  const d = document.createElement("div");
  d.className = "note " + cls;
  d.innerHTML = html;
  target.appendChild(d);
  return d;
}
const WEEK = "日一二三四五六";
function dayLabel(day) {
  const [y, m, d] = day.split("-").map(Number);
  return `${m}月${d}日 周${WEEK[new Date(y, m - 1, d).getDay()]}`;
}
function divider(day, summary) {
  const d = document.createElement("div");
  d.className = "day";
  d.innerHTML = `<span>${esc(dayLabel(day))}</span>` +
    (summary ? `<details class="day-sum"><summary>当天总结</summary><div class="body">${md(summary)}</div></details>` : "");
  target.appendChild(d);
}
function clearProgress() {
  if (progress) { progress.remove(); progress = null; }
}
// reviewBadge 只给去掉审稿 agent 之前的旧回复显示审稿结果；新回复没有 verdict。
function reviewBadge(e) {
  if (!e.verdict || e.verdict === "fixed") return "";
  const n = (e.changes || []).length;
  const badge = e.verdict === "pass" ? ["ok", "已审稿 · 原样通过"]
    : e.verdict === "revised" ? ["ok", `已审稿 · 改了 ${n} 处`]
    : e.verdict === "off" ? ["warn", "未审稿 · 团队里没有「审稿」agent"]
    : ["warn", "未审稿 · 审稿失败，下面是原稿"];
  const list = (title, xs) => xs && xs.length ? `<div class="rv-h">${title}</div><ul>${xs.map((x) => `<li>${esc(x)}</li>`).join("")}</ul>` : "";
  const more = e.verdict === "failed" ? `<div class="rv-h">失败原因</div><pre class="tool-out">${esc(e.reviewError)}</pre>`
    : e.verdict === "off" ? ""
    : list("改动", e.changes) + list("需要你确认", e.gaps) +
      `<div class="rv-h">规范检查</div><pre class="tool-out">${esc(e.lint || "（无输出）")}</pre>` +
      (e.verdict === "revised" ? `<div class="rv-h">原稿</div><pre class="tool-out">${esc(e.original)}</pre>` : "") +
      `<div class="rv-h">${esc(e.reviewer)} · ${esc(e.model)} · ${(e.ms / 1000).toFixed(1)} 秒</div>`;
  return `<details class="review ${badge[0]}"><summary>${badge[1]}</summary>${more}</details>`;
}
// 回复落在看不到的上方（那条消息后面又有别的消息）时，底部提示一下，点了跳过去。
let jumpTo = null;
function seen(el) {
  const r = el.getBoundingClientRect(), c = chat.getBoundingClientRect();
  return r.bottom > c.top + 24 && r.top < c.bottom - 24;
}
function jumpHint(th, el) {
  // 等这一帧自动滚到底以后再看新到的那条在不在视野里
  requestAnimationFrame(() => {
    if (!el || !th.box.nextElementSibling || seen(el)) return;
    jumpTo = { ...th, el };
    const j = $("#turn-jump");
    j.innerHTML = `<button type="button" class="tj-btn"><svg viewBox="0 0 24 24"><path d="m6 15 6-6 6 6"/></svg><span class="clip">「${esc(oneLine(th.text) || "附件")}」有新回复</span></button>`;
    j.hidden = false;
  });
}
const oneLine = (t) => String(t).replace(/\s+/g, " ").trim();
function gotoTurn(th) {
  jumpTo = null;
  $("#turn-jump").hidden = true;
  follow = false;
  (th.el || th.msg).scrollIntoView({ block: th.el ? "end" : "start", behavior: "smooth" });
  th.msg.classList.remove("tj-flash");
  void th.msg.offsetWidth;
  th.msg.classList.add("tj-flash");
}
$("#turn-jump").addEventListener("click", () => jumpTo && gotoTurn(jumpTo));
chat.addEventListener("scroll", () => { if (jumpTo && seen(jumpTo.el)) { jumpTo = null; $("#turn-jump").hidden = true; } });
function apply(e) {
  if (!inHistory && toInbox(e)) return;
  if (e.day && e.day !== lastDay) {
    lastDay = e.day;
    divider(e.day, summaries[e.day]);
  }
  // 重启后补的过程收尾不带 turn，按这组过程开始时的 turn 放
  const turn = e.turn || (e.type === "activity" && acts[e.group]?.turn) || "";
  const th = turn && threads[turn];
  if (th && e.project) th.pids.add(+e.project);
  if (pinPid) refilter();
  if (!th) return applyHere(e);
  const saved = target;
  target = th.box;
  const before = th.box.lastElementChild;
  try { applyHere(e); } finally { target = saved; }
  const el = th.box.lastElementChild;
  if (!inHistory && !replay && el !== before && (e.type === "reply" || e.type === "ask" || e.type === "permission")) jumpHint(th, el);
}
function applyHere(e) {
  if (e.type !== "progress" && e.type !== "idle" && e.type !== "reviewing" && e.type !== "queue") clearProgress();
  switch (e.type) {
    case "activity":
      activity(e);
      break;
    case "note":
    case "direct":
    case "notice":
      note("sys", esc(e.text));
      break;
    case "progress":
      clearProgress();
      progress = note("sys reviewing", esc(e.text));
      break;
    case "summary":
      summaries[e.of] = e.text;
      chat.querySelectorAll(`[data-sum-of="${e.of}"]`).forEach((x) => x.remove());
      note("sys", `<details class="day-sum"${e.brief ? " open" : ""}><summary>${e.brief ? `${esc(dayLabel(e.of))} 的盘点` : `已写好 ${esc(dayLabel(e.of))} 的总结`}</summary><div class="body">${md(e.text)}</div></details>`);
      break;
    case "user": {
      const b = bubble("user");
      if (e.merged) b.insertAdjacentHTML("afterbegin", '<span class="tag msg-tag">补充给正在处理的那条</span>');
      // 例行事项到点发的：开头的「【例行事项「X」到点了】」换成标签
      if (e.routine) b.insertAdjacentHTML("afterbegin", `<span class="tag msg-tag rt">例行事项 · ${esc(e.routine)}</span>`);
      b.querySelector(".body").textContent = e.routine ? String(e.text || "").replace(/^【例行事项「[^」]*」到点了】\n?/, "") : e.text || "";
      b.querySelector(".body").hidden = !e.text;
      if (e.files?.length) b.insertAdjacentHTML("afterbegin", `<div class="msg-files">${e.files.map((f) => f.image
        ? `<img class="mf-img" src="${fileURL(f.id)}" alt="${esc(f.name)}" title="${esc(f.name)}" loading="lazy">`
        : `<span class="att-i" title="${esc(f.path)}"><svg viewBox="0 0 24 24"><path d="M14 3H7a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h10a2 2 0 0 0 2-2V8z"/><path d="M14 3v5h5"/></svg><span class="att-n clip">${esc(f.name)}</span></span>`).join("")}</div>`);
      msgActs(b, e, !!e.id);
      if (e.id && !e.merged) {
        const box = document.createElement("div");
        box.className = "thread";
        target.appendChild(box);
        b.dataset.turn = box.dataset.turn = e.id;
        threads[e.id] = { box, msg: b, text: e.text || "", pids: new Set(e.project ? [+e.project] : []) };
      }
      break;
    }
    case "rewind":
      break; // 撤回后 epoch 已变，页面已按撤回后的内容重画
    case "clear":
      note("sys", "已清空对话页。助理不再参考之前的对话，这里也不再显示；记录仍保存在这台 Mac 上。");
      break;
    case "route": {
      names[e.project] = e.name;
      const why = [e.reason, e.quote ? `引用项目摘要「${e.quote}」` : ""].filter(Boolean).join(" · ");
      note("route", `<span>${e.created ? "新建项目" : "放进了"}</span>${projLink(e.project, e.name)}${why ? `<span class="why clip" title="${esc(why)}">— ${esc(why)}</span>` : ""}`);
      break;
    }
    case "reorg": { // 助理或用户整理项目：改名、合并、撤销合并、挪安排
      if (e.name) names[e.project] = e.name;
      if (e.srcName) names[e.src] = e.srcName;
      const p = projLink(e.project, e.name);
      const html = {
        rename: `<span>改名</span><span class="clip">「${esc(e.old)}」→</span>${p}<button class="undo-btn" type="button" data-rename="${+e.project}" data-to="${esc(e.old)}">改回</button>`,
        merge: `<span>把「${esc(e.srcName)}」并进</span>${p}<span class="muted">挪了 ${+e.runs} 次安排</span><button class="undo-btn" type="button" data-unmerge="${+e.merge}">撤销</button>`,
        unmerge: `<span>撤销合并</span>${projLink(e.src, e.srcName)}<span>从</span>${p}<span>分了出来</span>${e.kept ? '<span class="why clip">— 并进去的那段项目记忆改过，留着没删</span>' : ""}`,
        move: `<span>${+e.runs} 次安排挪到</span>${p}`,
        remove: `<span>删除项目「${esc(e.name)}」</span><span class="muted">项目文件夹里的文件不动</span>`,
        routine: `<span>${{ add: "新建", update: "修改", remove: "删除" }[e.act] || ""}例行事项</span>${e.act === "remove" ? `<span>「${esc(e.routineName)}」</span>`
          : `<a href="#" class="plink" data-show="routines">${esc(e.routineName)}</a>`}<span class="muted">${esc(e.when || "")}${e.act !== "remove" && e.enabled === false ? " · 关着" : ""}</span>`,
      }[e.op];
      if (e.op === "routine" && view === "routines" && !replay) loadRoutines();
      if (html) note("route", html);
      if (e.op === "unmerge") // 合并记录上的撤销按钮不再能点
        chat.querySelectorAll(`[data-unmerge="${+e.merge}"]`).forEach((b) => { b.disabled = true; b.textContent = "已撤销"; });
      break;
    }
    case "ask":
    case "permission":
      card(e);
      break;
    case "reviewing": // 旧记录中的审稿提示，兼容处理
      break;
    case "reply": {
      const d = bubble("assistant", e.agent);
      d.querySelector(".body").innerHTML = md(e.text);
      if (e.project) {
        d.querySelector(".who").insertAdjacentHTML("beforeend", `<span class="faint">·</span>${projLink(e.project, names[e.project])}${e.report ? '<span class="tag">汇报</span>' : e.run ? '<span class="tag">安排</span>' : ""}` +
          (e.report ? "" : `<select class="re-proj" data-from="${+e.project}" title="放错了项目时选对的那个，会发一句话告诉助理"><option value="">换项目</option>${projOpts(+e.project)}</select>`));
      }
      if (e.goto === "import") d.querySelector(".body").insertAdjacentHTML("beforeend", '<p><button class="btn line" data-show="import">打开整理页</button></p>');
      if (e.show === "staff") d.querySelector(".body").insertAdjacentHTML("beforeend", '<p><button class="btn line" data-show="staff">打开配置页</button></p>');
      if (e.show === "agents") d.querySelector(".body").insertAdjacentHTML("beforeend", '<p><button class="btn line" data-show="agents">打开 Agent 页</button></p>');
      if (e.picks?.length) d.querySelector(".body").insertAdjacentHTML("beforeend", `<div class="go-on">${e.picks.map((x) =>
        `<button class="btn line" type="button" data-pick="${esc(x.text)}" title="${esc(x.text)}">${esc(x.label)}</button>`).join("")}</div>`);
      d.insertAdjacentHTML("beforeend", reviewBadge(e));
      msgActs(d, e, false);
      break;
    }
    case "task":
      note("task", `<i class="dot ${/失败|做不了|没通过/.test(e.text) ? "failed" : /完成/.test(e.text) ? "done" : /打回/.test(e.text) ? "rejected" : "working"}"></i>` +
        `<span class="name">${esc(e.agent)}</span><span class="clip">${esc(e.title)}</span><span class="muted">${esc(e.text)}</span>`);
      break;
    case "permission_done":
      cardDone(e);
      break;
    // 旧版事件类型，仅用于兼容历史记录的显示
    case "tool": {
      const d = note("tool", `<details><summary><span class="tool-name">${esc(toolName(e.name))}</span> <span class="muted mono">${esc(JSON.stringify(e.input || "").slice(0, 120))}</span><span class="tool-st"></span></summary><pre class="tool-out"></pre></details>`);
      tools[e.id] = d;
      break;
    }
    case "tool_result": {
      const d = tools[e.id];
      if (!d) break;
      d.querySelector(".tool-st").textContent = e.error ? "失败" : "完成";
      d.querySelector(".tool-out").textContent = e.text || "（无输出）";
      break;
    }
    case "error":
    case "exit": {
      // 换日总结失败的卡片：那天的总结后来写成了就不再显示。旧记录没有 summaryOf，从文字里认日期。
      const of = e.summaryOf || (/^(\d{4}-\d{2}-\d{2}) 的总结没写成/.exec(e.text || "") || [])[1];
      if (of && summaries[of]) break;
      const d = note("err", esc(e.text));
      if (of) d.dataset.sumOf = of;
      break;
    }
  }
}
// 消息下方的一行：时间、复制；用户自己的消息再加撤回和编辑。鼠标移到消息上才显示。
const ICON_COPY = '<svg viewBox="0 0 24 24"><rect x="9" y="9" width="11" height="11" rx="2"/><path d="M5 15V6a2 2 0 0 1 2-2h8"/></svg>';
const ICON_UNDO = '<svg viewBox="0 0 24 24"><path d="M9 14 4 9l5-5"/><path d="M4 9h10.5a5.5 5.5 0 0 1 0 11H11"/></svg>';
const ICON_EDIT = '<svg viewBox="0 0 24 24"><path d="M4 20h4L19 9l-4-4L4 16z"/><path d="m13.5 6.5 4 4"/></svg>';
const hhmm = (ts) => { const d = new Date(ts); return isNaN(d) ? "" : `${String(d.getHours()).padStart(2, "0")}:${String(d.getMinutes()).padStart(2, "0")}`; };
function msgActs(d, e, mine) {
  d._ev = e;
  d.insertAdjacentHTML("beforeend", `<div class="msg-acts"><span class="faint">${hhmm(e.ts)}</span>` +
    `<button type="button" class="ma" data-ma="copy" title="复制" aria-label="复制">${ICON_COPY}</button>` +
    (mine ? `<button type="button" class="ma" data-ma="undo" title="撤回：去掉这条和它之后的对话，内容放回输入框" aria-label="撤回">${ICON_UNDO}</button>` +
      `<button type="button" class="ma" data-ma="edit" title="编辑：改完重新发，这条之后的对话会去掉" aria-label="编辑">${ICON_EDIT}</button>` : "") +
    `</div>`);
}
async function copyText(text) {
  try { await navigator.clipboard.writeText(text); return; } catch {}
  const t = document.createElement("textarea"); // 拿不到剪贴板权限时的退路
  t.value = text; document.body.appendChild(t); t.select(); document.execCommand("copy"); t.remove();
}
// 撤回和编辑前，助理还在处理消息时先停下：停止马上生效，不等 harness 进程退出。
async function stopIfBusy() {
  if (chatBusy) await api("/api/chat/stop", { method: "POST" });
}
async function rewind(e, text, files) {
  try {
    await stopIfBusy();
    await api("/api/chat", { method: "POST", body: JSON.stringify({ text, files: files.map((f) => f.id), rewind: e.id, project: +e.project || 0 }) });
    return true;
  } catch (err) {
    $("#chat-state").textContent = err.message;
    return false;
  }
}
chat.addEventListener("click", (ev) => {
  const h = ev.target.closest(".proc-h");
  if (h) h.parentElement.classList.toggle("open");
  const g = ev.target.closest(".act-gs")?.parentElement;
  if (g) { g.classList.toggle("shut"); g.dataset.user = "1"; }
});
chat.addEventListener("click", async (ev) => {
  const b = ev.target.closest("[data-ma]");
  if (!b) return;
  const d = b.closest(".msg"), e = d._ev;
  if (b.dataset.ma === "copy") {
    await copyText(e.text || "");
    b.classList.add("done");
    b.title = "已复制";
    setTimeout(() => { b.classList.remove("done"); b.title = "复制"; }, 1500);
  } else if (b.dataset.ma === "undo") {
    try {
      await stopIfBusy();
      await api("/api/chat/rewind", { method: "POST", body: JSON.stringify({ id: e.id }) });
    } catch (err) {
      $("#chat-state").textContent = err.message;
      return;
    }
    clearAtt();
    pending = (e.files || []).map((f) => ({ key: ++attSeq, name: f.name, size: f.size, image: f.image, id: f.id, url: f.image ? fileURL(f.id) : "" }));
    input.value = e.text || "";
    input.dispatchEvent(new Event("input"));
    renderAtt();
  } else if (b.dataset.ma === "edit") {
    editMsg(d, e);
  }
});
// 就地编辑：消息变成输入框，Enter 或「发送」撤回这条和它之后的对话再发新内容，Esc 或「取消」放弃。
function editMsg(d, e) {
  if (d.classList.contains("editing")) return;
  d.classList.add("editing");
  const body = d.querySelector(".body");
  const box = document.createElement("div");
  box.className = "msg-edit";
  box.innerHTML = `<textarea rows="2"></textarea><div class="msg-edit-ft"><span class="faint">这条之后的对话会去掉</span><span class="grow"></span>` +
    `<button type="button" class="btn line" data-ed="cancel">取消</button><button type="button" class="btn primary" data-ed="send">发送</button></div>`;
  body.after(box);
  const ta = box.querySelector("textarea");
  ta.value = e.text || "";
  const fit = () => { ta.style.height = "auto"; ta.style.height = ta.scrollHeight + "px"; };
  const close = () => { box.remove(); d.classList.remove("editing"); };
  const send = async () => {
    const text = ta.value.trim();
    if (!text && !(e.files || []).length) return;
    box.querySelectorAll("button").forEach((x) => (x.disabled = true));
    if (!await rewind(e, text, e.files || [])) box.querySelectorAll("button").forEach((x) => (x.disabled = false));
  };
  ta.addEventListener("input", fit);
  ta.addEventListener("keydown", (k) => {
    if (k.key === "Escape") { k.preventDefault(); close(); }
    if (k.key === "Enter" && !k.shiftKey && !ime(k)) { k.preventDefault(); send(); }
  });
  box.addEventListener("click", (k) => {
    const x = k.target.closest("[data-ed]");
    if (x) x.dataset.ed === "send" ? send() : close();
  });
  fit();
  ta.focus();
  ta.setSelectionRange(ta.value.length, ta.value.length);
}
// 助理在忙或后台有安排在跑时显示停止；点停止两样一起停。
// 输入框底部的审批档位和上下文用量（renderComposer 画）
const TIERS = [
  { v: "ask", l: "每次都问", d: "写文件、跑会改东西的命令、联网之前都等你批准。只读的命令不问。" },
  { v: "list", l: "按清单问", d: "只读的命令、项目目录里的改动直接做；危险操作清单上的事等你批准。" },
  { v: "all", l: "全部放行", d: "清单上的事也不问，直接执行。删除文件、改写 git 历史、清数据库仍等你批准。", risk: true },
];
let composer = { tier: "list", context: 0, limit: 0 };
let chatBusy = false, busyRuns = 0;
function setBusy(busy, runs = 0) {
  if (chatBusy && !busy) loadComposer(); // 一轮结束，上下文用量变了
  chatBusy = busy; busyRuns = runs;
  canSend();
  $("#chat-state").textContent = busy ? `${directNow ? "「" + directNow.agent + "」" : "助理"}在忙…` : runs ? `后台 ${runs} 次安排在跑` : "";
}
// 直接对话：输入框上方一条，写明在和谁对话、放在哪个项目，带「结束」
let directNow = null;
const INPUT_PH = $("#input").placeholder, PROJ_PH = "在这个项目里要做的事告诉助理，不用说是哪个项目。可以粘贴图片、拖入文件";
// 不在直接对话时的提示：对话搬到项目页右边时用项目里的说法（dockChat）
const idlePH = () => document.querySelector(".chat-col").classList.contains("pf") ? PROJ_PH : INPUT_PH;
function renderDirect(d) {
  directNow = d || null;
  const b = $("#dbar");
  b.hidden = !d;
  $("#input").placeholder = d ? `发给「${d.agent}」，不经过助理。写 @助理 或点「结束」回到助理` : idlePH();
  if (!d) { b.innerHTML = ""; return; }
  b.innerHTML = `<span class="dbar-dot"></span><span class="dbar-t clip">正在和「<b>${esc(d.agent)}</b>」对话<span class="faint">·</span>${projLink(d.project, d.projectName)}</span>` +
    `<span class="grow"></span><span class="err-t" id="dbar-err"></span><button type="button" class="btn line" id="dbar-end" title="结束后它写纪要，放进收件箱和项目记忆">结束</button>`;
}
$("#dbar").addEventListener("click", async (e) => {
  if (!e.target.closest("#dbar-end")) return;
  try { renderDirect((await api("/api/chat/direct", { method: "POST", body: JSON.stringify({ agent: "" }) })).direct); }
  catch (err) { $("#dbar-err").textContent = err.message; }
});
// 地址带 ?shot 时只取一次不挂起，给无头浏览器截图用。
const SHOT = /[?&]shot\b/.test(location.search);
async function pollChat() {
  for (;;) {
    try {
      const r = await api(`/api/chat/poll?epoch=${epoch}&after=${seq}` + (SHOT ? "&nowait" : ""));
      if (r.epoch !== epoch) {
        epoch = r.epoch; seq = 0; progress = null; lastDay = "";
        chat.innerHTML = "";
        for (const id in threads) delete threads[id];
        jumpTo = null; $("#turn-jump").hidden = true;
        for (const g in acts) { clearInterval(acts[g].timer); delete acts[g]; }
        follow = true;
        asking.clear(); renderDock(false);
        inbox.clear(); renderInbox();
        await loadHistoryIndex();
      }
      replay = seq === 0; // 打开页面时读回的旧事件不提示「有新回复」
      for (const e of r.events) { apply(e); seq = e.seq; }
      if (replay && r.events.length) fillNames(); // 读回的旧事件里是当时的项目名，换成现在的
      replay = false;
      renderDirect(r.direct);
      setBusy(r.busy, r.runs);
      renderQueue(r.queue || [], r.busy);
      if (r.events.some((e) => e.type === "task" || e.type === "reply" || e.type === "route" || e.type === "reorg")) refreshProjectSoon();
      if (SHOT) return;
    } catch {
      await new Promise((ok) => setTimeout(ok, 2000));
    }
  }
}
// 顶部的「查看更早」：每点一次往前加载一天
async function loadHistoryIndex() {
  const h = await api("/api/history").catch(() => ({ days: [], today: "" }));
  summaries = {};
  for (const d of h.days) if (d.hasSummary) summaries[d.day] = d.summary;
  earlier = h.days.map((d) => d.day).filter((d) => d < h.today).reverse();
  renderEarlierButton();
}
function renderEarlierButton() {
  let b = $("#earlier");
  if (!earlier.length) { b?.remove(); return; }
  if (!b) {
    b = document.createElement("button");
    b.id = "earlier";
    b.className = "btn earlier";
    chat.prepend(b);
  }
  b.textContent = `查看更早：${dayLabel(earlier[0])}`;
}
async function loadEarlier() {
  const day = earlier.shift();
  const evs = await api("/api/history/" + day);
  const box = document.createElement("div");
  const saved = [target, lastDay, progress];
  target = box; lastDay = ""; progress = null; inHistory = true;
  if (!evs.length) divider(day, summaries[day]);
  for (const e of evs) apply(e);
  box.querySelectorAll(".perm:not(.done)").forEach((p) => {
    p.classList.remove("pending");
    p.classList.add("done");
    p.querySelector(".perm-st").textContent = "已失效";
  });
  [target, lastDay, progress] = saved;
  inHistory = false;
  const h = chat.scrollHeight;
  follow = false; // 往前翻是要看旧消息，不跳回底部
  $("#earlier").after(...box.childNodes);
  chat.scrollTop += chat.scrollHeight - h; // 保持当前看到的位置不跳
  renderEarlierButton();
}

chat.addEventListener("click", async (e) => {
  if (e.target.id === "earlier") return loadEarlier();
  const g = e.target.closest("[data-show]");
  if (g) return show(g.dataset.show);
  const rn = e.target.closest("[data-rename]");
  if (rn) {
    rn.disabled = true;
    return api("/api/projects/" + rn.dataset.rename, { method: "PATCH", body: JSON.stringify({ name: rn.dataset.to }) })
      .then(() => { rn.textContent = "已改回"; fillNames(); }).catch((err) => { rn.disabled = false; $("#chat-state").textContent = err.message; });
  }
  const um = e.target.closest("[data-unmerge]");
  if (um) {
    um.disabled = true;
    return api(`/api/merges/${um.dataset.unmerge}/undo`, { method: "POST" })
      .then(() => { um.textContent = "已撤销"; }).catch((err) => { um.disabled = false; $("#chat-state").textContent = err.message; });
  }
  // 「接着做」：把要发的话填进输入框，用户改完自己发
  const p = e.target.closest("[data-pick]");
  if (p) {
    input.value = p.dataset.pick;
    input.dispatchEvent(new Event("input"));
    input.focus();
  }
});

// 回复上的「换项目」：选一个项目就发一句话告诉助理，由助理挪过去。
// 选项画回复时就放好，按下时再按最新的项目列表同步换一遍：下拉框打开前要已经有选项，不能等请求回来
function projOpts(from) {
  return projectList.filter((x) => x.id !== from).map((x) => `<option value="${esc(x.name)}">${esc(x.name)}</option>`).join("");
}
chat.addEventListener("pointerdown", (e) => {
  const sel = e.target.closest(".re-proj");
  if (sel && !sel.disabled) sel.innerHTML = '<option value="">换项目</option>' + projOpts(+sel.dataset.from);
});
chat.addEventListener("change", async (e) => {
  const sel = e.target.closest(".re-proj");
  if (!sel || !sel.value) return;
  const text = `这件事应该放在「${sel.value}」，不是「${names[sel.dataset.from] || "项目 #" + sel.dataset.from}」`;
  sel.disabled = true;
  try {
    await api("/api/chat", { method: "POST", body: JSON.stringify({ text, files: [] }) });
    follow = true; toBottom();
  } catch (err) {
    sel.disabled = false;
    $("#chat-state").textContent = err.message;
  }
});

/* ---------- 问题卡片 ---------- */
// 助理和子 agent 要用户拍板时（选项目、确认改团队、批准危险操作）发一张卡片。对话里留一条记录，
// 回答在输入框上方的卡片里：数字键或上下键加回车选，最后一行可以写补充，多张卡片时左右翻页。
const asking = new Map(); // id -> 事件，还没回答的卡片，按到达顺序
const cardEv = {};        // id -> 事件，用来在回答后写出选了什么
let qPage = 0, qSel = 0;
function cardOpts(e) {
  if (e.type === "permission") return [{ value: "allow", label: "批准" }, { value: "deny", label: "拒绝" }];
  return e.options || [];
}
function cardBody(e) {
  const kv = Object.entries(e.input || {}).map(([k, v]) => `<b>${esc(k)}</b><span>${esc(v)}</span>`).join("");
  const sub = e.type === "permission" ? e.description : e.reason;
  return (sub ? `<div class="sub q-sub">${esc(sub)}</div>` : "") +
    ((e.items || []).length ? `<ul class="q-items">${e.items.map((x) => `<li>${esc(x)}</li>`).join("")}</ul>` : "") +
    (kv ? `<div class="kv">${kv}</div>` : "");
}
function card(e) {
  cardEv[e.id] = e;
  const title = e.type === "permission" ? (e.display || "请求批准") : e.text;
  const d = note("perm", `
    <div class="perm-hd"><i class="dot approval"></i><span class="name">${esc(title)}</span>${e.project ? `<span class="grow"></span>${projLink(e.project, names[e.project])}` : ""}</div>
    ${cardBody(e)}
    <div class="perm-act"><span class="muted perm-st">${e.stale ? "已失效：morula 重新打开过，这个问题不用再回答了" : "等你在下方回答"}</span></div>`);
  d.dataset.id = e.id;
  if (e.stale) d.classList.add("done");
  // 等回答时完整内容只在下方卡片里显示，对话里只留标题一行；回答后再展开成记录
  else if (!inHistory) { d.classList.add("pending"); asking.set(e.id, e); qPage = asking.size - 1; qSel = 0; renderDock(true); }
}
function cardDone(e) {
  const d = target.querySelector(`.perm[data-id="${CSS.escape(e.id)}"]`) || chat.querySelector(`.perm[data-id="${CSS.escape(e.id)}"]`);
  if (asking.delete(e.id)) { qPage = Math.min(qPage, asking.size - 1); qSel = 0; renderDock(false); }
  if (!d) return;
  d.classList.remove("pending");
  d.classList.add("done");
  const c = cardEv[e.id];
  const o = c && cardOpts(c).find((x) => x.value === e.choice);
  d.querySelector(".perm-st").textContent = e.expired ? "已失效" : e.text ? "你补充了：" + e.text
    : e.choice && e.choice !== "allow" && e.choice !== "deny" ? "已选择：" + (o ? o.label : e.choice)
    : e.allow ? "已批准" : "已拒绝";
}
/* ---------- 收件箱 ---------- */
// 用户正在说话时到的汇报、不急的提示和卡片（事件带 inbox）不插进对话，放这里。
// 读到哪儿记在本机浏览器里，只用来算未读条数；读不到时全部算未读。
const inbox = new Map(); // id -> 事件
let ibxRead = "";
try { ibxRead = localStorage.getItem("ibxRead") || ""; } catch {}
function toInbox(e) {
  if (e.type === "permission_done" && inbox.has(e.id)) {
    inbox.get(e.id).done = e;
    renderInbox();
    return true;
  }
  if (!e.inbox) return false;
  if (e.type !== "activity") { inbox.set(e.id || "s" + e.seq, e); renderInbox(); } // 过程不显示，结论在汇报里
  return true;
}
const ibxOpen = (e) => e.type === "ask" && !e.done && !e.stale;
function renderInbox() {
  const list = [...inbox.values()];
  const un = list.filter((e) => ibxOpen(e) || (e.ts || "") > ibxRead);
  $("#ibx-btn").hidden = !list.length;
  $("#ibx-n").hidden = !un.length;
  $("#ibx-n").textContent = un.length;
  $("#ibx-n").classList.toggle("bad", un.some((e) => e.failed));
  if (!$("#ibx").hidden) drawInbox();
}
function ibxItem(e) {
  const meta = (dot, kind) => `<div class="ibx-meta"><i class="dot ${dot}"></i><span>${kind}</span>${e.agent ? `<span>${esc(e.agent)}</span>` : ""}<span class="faint">${hhmm(e.ts)}</span>${e.project ? projLink(e.project, names[e.project]) : ""}</div>`;
  const cls = "ibx-it" + ((e.ts || "") > ibxRead ? " new" : "");
  if (e.type === "ask") {
    const opts = cardOpts(e), d = e.done;
    const o = d && opts.find((x) => x.value === d.choice);
    const st = e.stale ? "已失效：morula 重新打开过，这个问题不用再回答了" : !d ? "" : d.expired ? "已失效" : d.text ? "你补充了：" + d.text : "已选择：" + (o ? o.label : d.choice);
    return `<div class="${cls}" data-id="${esc(e.id)}">${meta(st ? "idle" : "approval", "要你决定")}<div class="ibx-q">${esc(e.text)}</div>` +
      (e.reason ? `<div class="muted ibx-why">${esc(e.reason)}</div>` : "") +
      (st ? `<div class="muted">${esc(st)}</div>` : `<div class="ibx-acts">${opts.map((x) => `<button type="button" class="btn line" data-ibx-ans="${esc(x.value)}">${esc(x.label)}</button>`).join("")}</div>`) + `</div>`;
  }
  const kind = e.type !== "reply" ? "提示" : e.report ? (e.failed ? "汇报 · 没做完" : "汇报") : e.minutes ? "对话纪要" : "消息";
  const go = e.show === "staff" ? '<div class="ibx-acts"><button class="btn line" data-show="staff">打开配置页</button></div>' : "";
  return `<div class="${cls}">${meta(e.failed ? "failed" : e.type === "reply" ? "done" : "idle", kind)}<div class="ibx-body">${e.type === "reply" ? md(e.text) : esc(e.text)}</div>${go}</div>`;
}
function drawInbox() {
  const list = [...inbox.values()].reverse();
  $("#ibx").innerHTML = `<div class="ibx-hd"><b>收件箱</b><span class="muted clip">你在说话时到的汇报、提示和不急的问题</span><span class="grow"></span>` +
    `<button type="button" class="q-nav" data-ibx="close" aria-label="关闭"><svg viewBox="0 0 24 24"><path d="M6 6l12 12M18 6 6 18"/></svg></button></div>` +
    `<div class="ibx-list">${list.length ? list.map(ibxItem).join("") : '<div class="muted ibx-empty">没有新的</div>'}</div>`;
}
function ibxToggle(open) {
  $("#ibx").hidden = !open;
  $("#ibx-btn").setAttribute("aria-expanded", open);
  if (!open) return;
  drawInbox(); // 先按旧的已读位置画，这次新到的带标记
  for (const e of inbox.values()) if ((e.ts || "") > ibxRead) ibxRead = e.ts;
  try { localStorage.setItem("ibxRead", ibxRead); } catch {}
  renderInbox();
}
$("#ibx-btn").addEventListener("click", () => ibxToggle($("#ibx").hidden));
document.addEventListener("click", (k) => {
  if (!$("#ibx").hidden && !k.target.closest("#ibx, #ibx-btn")) ibxToggle(false);
});
document.addEventListener("keydown", (k) => { if (k.key === "Escape" && !$("#ibx").hidden) ibxToggle(false); });
$("#ibx").addEventListener("click", async (k) => {
  if (k.target.closest('[data-ibx="close"]')) return ibxToggle(false);
  const g = k.target.closest("[data-show]");
  if (g) { ibxToggle(false); return show(g.dataset.show); }
  const b = k.target.closest("[data-ibx-ans]");
  if (!b) return;
  const it = b.closest(".ibx-it"), btns = it.querySelectorAll("[data-ibx-ans]");
  btns.forEach((x) => (x.disabled = true));
  try {
    await api("/api/chat/permission", { method: "POST", body: JSON.stringify({ id: it.dataset.id, choice: b.dataset.ibxAns, allow: true }) });
  } catch (err) {
    btns.forEach((x) => (x.disabled = false));
    it.insertAdjacentHTML("beforeend", `<div class="err-t">${esc(err.message)}</div>`);
  }
});
function renderDock(focus) {
  const box = $("#qdock");
  const list = [...asking.values()];
  if (!list.length) {
    const had = box.contains(document.activeElement);
    box.hidden = true; box.innerHTML = "";
    if (had) input.focus();
    return;
  }
  const e = list[Math.max(0, qPage)];
  const opts = cardOpts(e);
  const title = e.type === "permission" ? (e.display || "请求批准") : e.text;
  box.hidden = false;
  box.dataset.id = e.id;
  box.innerHTML = `
    <div class="q-hd"><b class="q-title">${esc(title)}</b><span class="grow"></span>
      ${list.length > 1 ? `<button type="button" class="q-nav" data-q="prev" aria-label="上一张"${qPage ? "" : " disabled"}><svg viewBox="0 0 24 24"><path d="m15 6-6 6 6 6"/></svg></button><span class="q-pg">${qPage + 1}/${list.length}</span><button type="button" class="q-nav" data-q="next" aria-label="下一张"${qPage < list.length - 1 ? "" : " disabled"}><svg viewBox="0 0 24 24"><path d="m9 6 6 6-6 6"/></svg></button>` : ""}
      <button type="button" class="q-nav" data-q="close" title="${e.type === "permission" ? "拒绝" : "不处理"}" aria-label="关闭"><svg viewBox="0 0 24 24"><path d="M6 6l12 12M18 6 6 18"/></svg></button></div>
    ${cardBody(e)}
    <div class="q-opts" role="listbox">${opts.map((o, i) => `<div class="q-opt${i === qSel ? " on" : ""}" role="option" data-qi="${i}"><span class="q-n">${i + 1}</span><span class="q-l">${esc(o.label)}</span><svg class="q-go" viewBox="0 0 24 24"><path d="M5 12h14M13 6l6 6-6 6"/></svg></div>`).join("")}
    ${e.free ? `<div class="q-opt q-free${qSel === opts.length ? " on" : ""}" data-qi="${opts.length}"><svg class="q-pen" viewBox="0 0 24 24"><path d="M4 20h4L19 9l-4-4L4 16v4z"/></svg><input class="q-in" placeholder="其他补充…（回车发送）" aria-label="其他补充"></div>` : ""}
    </div><div class="q-st muted"></div>`;
  if (focus && !(document.activeElement === input && input.value.trim())) box.focus();
}
async function answerCard(i, text) {
  const box = $("#qdock");
  const e = asking.get(box.dataset.id);
  if (!e) return;
  const o = cardOpts(e)[i];
  if (text == null && !o) return;
  const body = text != null ? { id: e.id, text, allow: false }
    : e.type === "permission" ? { id: e.id, allow: o.value === "allow" }
    : { id: e.id, choice: o.value, allow: true };
  box.querySelectorAll(".q-opt").forEach((x) => x.classList.add("off"));
  try {
    await api("/api/chat/permission", { method: "POST", body: JSON.stringify(body) });
  } catch (err) {
    box.querySelectorAll(".q-opt").forEach((x) => x.classList.remove("off"));
    box.querySelector(".q-st").textContent = err.message;
  }
}
function closeCard(e) {
  const opts = cardOpts(e);
  const i = e.type === "permission" ? 1 : opts.findIndex((o) => /^(cancel|no)$/.test(o.value));
  answerCard(i < 0 ? opts.length - 1 : i);
}
function moveSel(n) {
  const e = asking.get($("#qdock").dataset.id);
  const max = cardOpts(e).length + (e.free ? 1 : 0);
  qSel = (qSel + n + max) % max;
  $("#qdock").querySelectorAll(".q-opt").forEach((x) => x.classList.toggle("on", +x.dataset.qi === qSel));
  const fr = $("#qdock .q-free");
  if (fr && qSel === cardOpts(e).length) fr.querySelector("input").focus();
  else if (document.activeElement.classList.contains("q-in")) $("#qdock").focus();
}
$("#qdock").addEventListener("click", (k) => {
  const e = asking.get($("#qdock").dataset.id);
  if (!e) return;
  const q = k.target.closest("[data-q]");
  if (q) {
    if (q.dataset.q === "close") return closeCard(e);
    qPage += q.dataset.q === "next" ? 1 : -1; qSel = 0;
    return renderDock(true);
  }
  const o = k.target.closest(".q-opt");
  if (!o || o.classList.contains("q-free")) return;
  answerCard(+o.dataset.qi);
});
$("#qdock").addEventListener("keydown", (k) => {
  if (ime(k)) return;
  const e = asking.get($("#qdock").dataset.id);
  if (!e) return;
  const inText = k.target.classList.contains("q-in");
  if (k.key === "ArrowDown" || k.key === "ArrowUp") { k.preventDefault(); return moveSel(k.key === "ArrowDown" ? 1 : -1); }
  if (k.key === "Escape") { k.preventDefault(); return input.focus(); }
  if (inText) {
    if (k.key === "Enter" && !k.shiftKey && k.target.value.trim()) { k.preventDefault(); answerCard(-1, k.target.value.trim()); }
    return;
  }
  if (k.key === "Enter") { k.preventDefault(); return answerCard(qSel); }
  if (/^[1-9]$/.test(k.key) && +k.key <= cardOpts(e).length) { k.preventDefault(); return answerCard(+k.key - 1); }
  if ((k.key === "ArrowLeft" || k.key === "ArrowRight") && asking.size > 1) {
    const n = qPage + (k.key === "ArrowRight" ? 1 : -1);
    if (n >= 0 && n < asking.size) { k.preventDefault(); qPage = n; qSel = 0; renderDock(true); }
  }
});
$("#qdock").addEventListener("focusin", (k) => {
  if (k.target.classList.contains("q-in")) { qSel = cardOpts(asking.get($("#qdock").dataset.id)).length; $("#qdock").querySelectorAll(".q-opt").forEach((x) => x.classList.toggle("on", +x.dataset.qi === qSel)); }
});

/* ---------- 排队 ---------- */
// 助理在忙时发的消息先排队，挂在输入框上方。默认等当前这条处理完再接着处理；
// 也可以补充到当前这条（助理带着补充重新判断或重新安排），或单独并行处理。
let queueKey = "";
function renderQueue(list, busy) {
  const box = $("#queue");
  const key = busy + JSON.stringify(list.map((q) => q.id));
  if (key === queueKey) return;
  queueKey = key;
  box.hidden = !list.length;
  box.innerHTML = list.length ? `<div class="qu-hd muted">${busy ? `排队中 ${list.length} 条 · 助理处理完当前这条后按顺序接着做` : "这些消息还没发出"}</div>` +
    list.map((q) => `<div class="qu-row" data-qid="${esc(q.id)}">
      <span class="qu-t clip" title="${esc(q.text)}">${esc(q.text || (q.files || []).map((f) => f.name).join("、"))}</span>
      ${busy ? `<button type="button" class="btn line sm" data-how="merge"${q.files?.length ? ' disabled title="带附件的消息不能补充，可以单独并行"' : ' title="助理带着这条重新判断当前这条"'}>补充到当前这条</button>
      <button type="button" class="btn line sm" data-how="parallel" title="不等当前这条，现在就处理">单独并行</button>`
      : '<button type="button" class="btn line sm" data-how="parallel">现在发送</button>'}
      <button type="button" class="q-nav" data-how="drop" title="不发了" aria-label="不发了"><svg viewBox="0 0 24 24"><path d="M6 6l12 12M18 6 6 18"/></svg></button></div>`).join("") : "";
}
$("#queue").addEventListener("click", async (k) => {
  const b = k.target.closest("[data-how]");
  if (!b || b.disabled) return;
  const row = b.closest(".qu-row");
  row.querySelectorAll("button").forEach((x) => (x.disabled = true));
  try {
    await api("/api/chat/queue", { method: "POST", body: JSON.stringify({ id: row.dataset.qid, how: b.dataset.how }) });
    if (b.dataset.how !== "drop") { follow = true; toBottom(); }
  } catch (err) {
    $("#chat-state").textContent = err.message;
    queueKey = "";
  }
});

/* ---------- 附件 ---------- */
// 粘贴的图片、拖进来的文件、点「+」选的文件，先上传到 morula 数据目录，发消息时只带 id。
// 上传完之前不能发送。浏览器拿不到文件夹里的内容，拖进文件夹时提示改拖文件。
let pending = []; // { key, name, image, url, id, err }
let attSeq = 0;
function fileURL(id) { return "/api/files/" + id.split("/").map(encodeURIComponent).join("/"); }
// 发送键在忙且没有要发的东西时变成停止键
function canSend() {
  const up = pending.some((f) => !f.id && !f.err);
  const has = !!input.value.trim() || pending.some((f) => f.id);
  const stop = chatBusy && !has && !up; // 后台的活在右栏「进行中」各自停
  const b = $("#send");
  b.classList.toggle("stop", stop);
  b.disabled = !stop && (up || !has);
  b.title = stop ? "停止助理最新的这一轮；例行事项和派出的活在右栏各自停" : "发送（Enter 发送，Shift+Enter 换行）";
  b.setAttribute("aria-label", stop ? "停止" : "发送");
}
function renderAtt() {
  const el = $("#att");
  el.hidden = !pending.length;
  el.innerHTML = pending.map((f) => `<span class="att-i${f.err ? " err" : ""}${f.id ? "" : f.err ? "" : " up"}" title="${esc(f.err || f.name)}">
    ${f.image ? `<img src="${f.url}" alt="">` : '<svg viewBox="0 0 24 24"><path d="M14 3H7a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h10a2 2 0 0 0 2-2V8z"/><path d="M14 3v5h5"/></svg>'}
    <span class="att-n clip">${esc(f.name)}</span>${f.err ? '<span class="att-s">失败</span>' : f.id ? "" : '<span class="att-s">上传中</span>'}
    <button type="button" class="att-x" data-rm="${f.key}" aria-label="移除">×</button></span>`).join("");
  canSend();
}
function addFiles(list) {
  for (const file of list) {
    const f = { key: ++attSeq, name: file.name || "粘贴的图片.png", image: /^image\/(png|jpeg|gif|webp)$/.test(file.type) };
    if (f.image) f.url = URL.createObjectURL(file);
    pending.push(f);
    api("/api/files?name=" + encodeURIComponent(f.name), { method: "POST", body: file })
      .then((r) => { f.id = r.id; f.image = r.image; })
      .catch((err) => { f.err = err.message; })
      .finally(renderAtt);
  }
  renderAtt();
  input.focus();
}
// 窗口（app.go）开了原生拖放：拖进来的文件由 Go 按路径复制好，结果交到这里。
window.morulaDropped = (list) => {
  dragDepth = 0;
  $("#drop-hint").hidden = true;
  if (view !== "chat" || !$("#prefs").hidden) return;
  for (const r of list) pending.push({ key: ++attSeq, name: r.name, id: r.id, image: r.image, url: r.image ? fileURL(r.id) : "", err: r.err });
  renderAtt();
  input.focus();
};
function clearAtt() {
  for (const f of pending) if (f.url) URL.revokeObjectURL(f.url);
  pending = [];
  renderAtt();
}
$("#att").addEventListener("click", (e) => {
  const b = e.target.closest("[data-rm]");
  if (!b) return;
  const i = pending.findIndex((f) => f.key === +b.dataset.rm);
  if (i >= 0) { if (pending[i].url) URL.revokeObjectURL(pending[i].url); pending.splice(i, 1); }
  renderAtt();
});
$("#att-add").addEventListener("click", () => $("#att-pick").click());
$("#att-pick").addEventListener("change", (e) => { addFiles([...e.target.files]); e.target.value = ""; });
input.addEventListener("paste", (e) => {
  const files = [...(e.clipboardData?.files || [])];
  if (!files.length) return; // 只有文字时照常粘贴
  e.preventDefault();
  addFiles(files);
});
// 拖放在 window 的捕获阶段处理并停止传播：Wails 运行时在 documentElement 上监听拖放，
// 没开原生文件拖放时会把放置效果设成「不允许」。不在对话页时也拦下，免得窗口打开被拖进来的文件。
let dragDepth = 0;
const isFileDrag = (e) => [...(e.dataTransfer?.types || [])].includes("Files");
for (const t of ["dragenter", "dragover", "dragleave", "drop"]) window.addEventListener(t, (e) => {
  if (!isFileDrag(e)) return;
  e.preventDefault();
  e.stopPropagation();
  const here = view === "chat" && $("#prefs").hidden;
  if (t === "dragenter") dragDepth++;
  if (t === "dragleave") dragDepth = Math.max(0, dragDepth - 1);
  if (t === "drop") dragDepth = 0;
  e.dataTransfer.dropEffect = here ? "copy" : "none";
  $("#drop-hint").hidden = !here || dragDepth === 0;
  if (t !== "drop" || !here) return;
  if (window._wails?.flags?.enableFileDrop) return; // 窗口里由原生层给路径，见 morulaDropped
  const items = [...(e.dataTransfer.items || [])];
  const dirs = items.filter((it) => it.webkitGetAsEntry?.()?.isDirectory).length;
  const files = [...e.dataTransfer.files].filter((_, i) => !items[i]?.webkitGetAsEntry?.()?.isDirectory);
  if (dirs) $("#chat-state").textContent = `跳过了 ${dirs} 个文件夹：只能加文件，把文件夹里的文件拖进来`;
  if (files.length) addFiles(files);
}, true);

/* ---------- 斜杠命令 ---------- */
// 输入框以 / 开头、还没有空格时，在输入框上方列出匹配的命令。上下键选，Enter 或 Tab 执行，Esc 关闭。
const COMMANDS = [
  { name: "/clear", desc: "清空对话页。助理不再参考清空前的对话，记录仍留在本机", run: clearChat },
];
// 输入框以 @ 开头、还没有空格时，列出能直接对话的 agent；选中后补全名字，不发送。@助理 结束对话、回到助理。
function mentions(v) {
  const q = v.slice(1).toLowerCase();
  const who = agentList.filter((a) => a.role !== "验收" && (a.role !== "助理" || directNow) && a.name !== directNow?.agent);
  return who.filter((a) => a.name.toLowerCase().startsWith(q)).map((a) => ({
    name: "@" + a.name,
    desc: a.role === "助理" ? "结束和别人的对话，回到助理" : `直接和它对话 · ${a.role}${a.skills?.length ? " · " + a.skills.join("、") : ""}`,
    fill: true,
  }));
}
let slashHit = [], slashAt = 0;
function slashUpdate() {
  const v = input.value;
  slashHit = /^\/\S*$/.test(v) ? COMMANDS.filter((c) => c.name.startsWith(v))
    : /^[@＠]\S*$/.test(v) ? mentions(v) : [];
  slashAt = Math.min(slashAt, Math.max(0, slashHit.length - 1));
  const el = $("#slash");
  el.hidden = !slashHit.length;
  el.innerHTML = slashHit.map((c, i) => `<button type="button" role="option" class="slash-i${i === slashAt ? " on" : ""}" data-cmd="${i}" aria-selected="${i === slashAt}">
    <b>${esc(c.name)}</b><span>${esc(c.desc)}</span></button>`).join("");
}
function slashRun(c) {
  if (c.fill) { // 补全 @名字，接着写要说的话
    input.value = c.name + " ";
    slashUpdate();
    return canSend();
  }
  input.value = "";
  input.style.height = "";
  slashUpdate();
  canSend();
  c.run();
}
$("#slash").addEventListener("mousedown", (e) => e.preventDefault()); // 点菜单时输入框不失焦
$("#slash").addEventListener("click", (e) => {
  const b = e.target.closest("[data-cmd]");
  if (b) slashRun(slashHit[+b.dataset.cmd]);
});
async function clearChat() {
  try {
    await api("/api/chat/clear", { method: "POST" });
    $("#chat-state").textContent = "";
  } catch (err) {
    $("#chat-state").textContent = err.message;
  }
}

input.addEventListener("input", () => {
  input.style.height = "auto";
  input.style.height = input.scrollHeight + "px";
  slashAt = 0;
  slashUpdate();
  canSend();
});
input.addEventListener("keydown", (e) => {
  if (slashHit.length && !ime(e)) {
    if (e.key === "ArrowDown" || e.key === "ArrowUp") {
      e.preventDefault();
      slashAt = (slashAt + (e.key === "ArrowDown" ? 1 : slashHit.length - 1)) % slashHit.length;
      return slashUpdate();
    }
    if (e.key === "Enter" && !e.shiftKey || e.key === "Tab") {
      e.preventDefault();
      return slashRun(slashHit[slashAt]);
    }
    if (e.key === "Escape") {
      e.preventDefault();
      slashHit = [];
      $("#slash").hidden = true;
      return;
    }
  }
  if (e.key === "Enter" && !e.shiftKey && !ime(e)) {
    e.preventDefault();
    $("#composer").requestSubmit();
  }
});
input.addEventListener("blur", () => { $("#slash").hidden = true; });
input.addEventListener("focus", () => {
  slashUpdate();
  if (!agentList.length) api("/api/agents").then((l) => { agentList = l; assignLooks(l); slashUpdate(); }).catch(() => {});
});
$("#composer").addEventListener("submit", async (e) => {
  e.preventDefault();
  // 只有点按钮才停；输入框里按 Enter 不会停掉在跑的活
  if ($("#send").classList.contains("stop")) return e.submitter === $("#send") && stopChat();
  const text = input.value.trim();
  const cmd = COMMANDS.find((c) => c.name === text);
  if (cmd) return slashRun(cmd);
  const files = pending.filter((f) => f.id).map((f) => f.id);
  if ($("#send").disabled || !text && !files.length) return;
  try {
    await api("/api/chat", { method: "POST", body: JSON.stringify({ text, files, project: pinPid }) });
    follow = true; toBottom();
    input.value = "";
    input.style.height = "";
    clearAtt();
  } catch (err) {
    $("#chat-state").textContent = err.message;
  }
});
// 点停止后界面马上回到空闲；助理收尾、harness 进程退出在后台完成。
// 只停最后开始的那一条；还有别的在处理时界面保持在忙
function stopChat() {
  setBusy(false, busyRuns);
  api("/api/chat/stop", { method: "POST" }).then((r) => { if (r.busy) setBusy(true, busyRuns); }).catch(() => {});
}

// 长期记忆：设置弹窗里的一页
/* ---------- 审批与通知 ---------- */
let apRules = [], apPath = "";
function renderApproval(a) {
  apRules = a.rules || []; apPath = a.path || "";
  $("#ap-rules").innerHTML = apRules.map((r, i) => `<span class="dg-tg">${esc(r)}<button class="dg-x" type="button" data-ap-del="${i}" title="去掉这一条" aria-label="去掉：${esc(r)}">×</button></span>`).join("") ||
    `<span class="muted">清单是空的：agent 做任何事都不会先问你</span>`;
  setSeg($("#ap-ask"), a.ask ? "1" : "0");
  setSeg($("#ap-report"), a.report ? "1" : "0");
  $("#ap-st").textContent = a.error || "";
}
async function loadApproval() {
  try { renderApproval(await api("/api/approval")); } catch (err) { $("#ap-st").textContent = err.message; }
}
async function saveApproval(body) {
  try { renderApproval(await api("/api/approval", { method: "PUT", body: JSON.stringify(body) })); $("#ap-st").textContent = "已保存"; }
  catch (err) { $("#ap-st").textContent = err.message; }
}
const onOff = [{ v: "1", l: "开" }, { v: "0", l: "关" }];
seg($("#ap-ask"), onOff, "1");
seg($("#ap-report"), onOff, "1");
$("#ap-ask").addEventListener("change", (e) => saveApproval({ ask: e.currentTarget.dataset.value === "1" }));
$("#ap-report").addEventListener("change", (e) => saveApproval({ report: e.currentTarget.dataset.value === "1" }));
$("#ap-rules").addEventListener("click", (e) => {
  const b = e.target.closest("[data-ap-del]");
  if (b) saveApproval({ rules: apRules.filter((_, i) => i !== +b.dataset.apDel) });
});
$("#ap-add").addEventListener("submit", (e) => {
  e.preventDefault();
  const t = e.currentTarget.rule.value.trim();
  if (!t) return;
  e.currentTarget.rule.value = "";
  saveApproval({ rules: [...apRules, t] });
});
$("#ap-open").addEventListener("click", async () => {
  try { await api("/api/open", { method: "POST", body: JSON.stringify({ link: apPath }) }); }
  catch (err) { $("#ap-st").textContent = err.message; }
});

async function loadMemory() {
  $("#mem-st").textContent = "读取中…";
  try {
    const m = await api("/api/memory");
    $("#mem-text").value = m.text;
    $("#mem-path").textContent = m.path;
    $("#mem-st").textContent = "";
  } catch (err) { $("#mem-st").textContent = err.message; }
}
$("#mem-pane").addEventListener("submit", async (e) => {
  e.preventDefault();
  try {
    await api("/api/memory", { method: "PUT", body: JSON.stringify({ text: $("#mem-text").value }) });
    $("#mem-st").textContent = "已保存";
  } catch (err) {
    $("#mem-st").textContent = err.message;
  }
});

/* ---------- Agent ---------- */
let harnesses = [];
async function loadHarnesses() {
  harnesses = await api("/api/harnesses").catch(() => []);
  return harnesses;
}
const hOf = (name) => harnesses.find((h) => h.name === name);
async function loadAgents() {
  if (!harnesses.length) await loadHarnesses();
  if (!projectList.length) projectList = await api("/api/projects").catch(() => []);
  renderAgents(await api("/api/agents"));
}
let skillList = null;
async function loadSkills() {
  if (!skillList) skillList = (await api("/api/skills").catch(() => ({ skills: [] }))).skills;
  return skillList;
}
// chips 画一组勾选框：opts 是 [{v, l, t}]，on 是选中的值。many 为 true 时只画选中的，其余放进下拉框，选了再加进来。
function chips(el, opts, on, empty, many) {
  const set = new Set((on || []).map(String));
  const box = (o) => `<label title="${esc(o.t || "")}"><input type="checkbox" value="${esc(o.v)}" ${set.has(String(o.v)) ? "checked" : ""}>${esc(o.l)}</label>`;
  if (!opts.length) { el.innerHTML = `<span class="faint">${esc(empty)}</span>`; return; }
  if (!many) { el.innerHTML = opts.map(box).join(""); return; }
  el._opts = opts;
  const rest = opts.filter((o) => !set.has(String(o.v)));
  el.innerHTML = opts.filter((o) => set.has(String(o.v))).map(box).join("") +
    (rest.length ? `<select class="chip-add"><option value="">＋ 加 skill</option>${rest.map((o) => `<option value="${esc(o.v)}" title="${esc(o.t || "")}">${esc(o.l)}</option>`).join("")}</select>` : "");
}
document.addEventListener("change", (e) => {
  const sel = e.target.closest(".chips select.chip-add");
  if (!sel || !sel.value) return;
  const el = sel.parentElement;
  chips(el, el._opts, [...chipVals(el), sel.value], "", true);
});
const chipVals = (el) => [...el.querySelectorAll("input:checked")].map((c) => c.value);
let agentList = [];
const EFFORT_ZH = { minimal: "最低", low: "低", medium: "中", high: "高", xhigh: "很高", max: "最高" };
let agTab = "all";
// liveOf 是开工时实际用的模型：模型不在 harness 的列表里时换成列表第一个（和后端 liveModel 一样）。
function liveOf(a) {
  const h = hOf(a.runtime), ms = h?.ready ? h.models || [] : [];
  if (!ms.length || ms.some((m) => m.id === a.model)) return null;
  return ms[0].id;
}
const shortModel = (m) => String(m || "").split("/").pop();
function renderAgents(list) {
  agentList = list;
  const as = list.find((a) => a.role === "助理");
  renderPick(as);
  renderComposer();
  footer();
  assignLooks(list);
  const n = { all: list.length, on: list.filter((a) => !a.off).length, off: list.filter((a) => a.off).length };
  for (const k in n) $("#ag-n-" + k).textContent = n[k];
  setSeg($("#ag-tabs"), agTab);
  const row = (a) => {
    const h = hOf(a.runtime), st = a.stats || {}, asst = a.role === "助理", acc = a.name === "验收";
    const fixed = asst || acc;
    const tag = asst || acc ? '<span class="tag">固定</span>'
      : a.skills?.length ? `<span class="tag accent">skill · ${esc(a.skills[0])}${a.skills.length > 1 ? ` 等 ${a.skills.length} 个` : ""}</span>` : "";
    const about = a.profile ? esc(a.profile)
      : a.describing ? '<span class="faint">助理在写介绍…</span>'
      : a.instructions ? esc(a.instructions) : '<span class="faint">没有介绍</span>';
    const live = liveOf(a);
    const eff = a.effort ? EFFORT_ZH[a.effort] || a.effort : fixed ? "模型默认" : "按任务定";
    const model = `<span class="${live ? "ag-gone" : ""}">${esc(shortModel(a.model))}</span>`;
    const rec = asst ? (a.turns ? `今天 ${a.turns} 轮` : "今天还没对话")
      : !st.total ? "没派过活"
      : acc ? `验过 ${st.total} 次 · 打回 ${st.missed} 次`
      : `做过 ${st.total} 次 · 打回 ${st.missed} 次`;
    const hwarn = !h ? "不认识这个 AI 工具" : !h.ready ? "AI 工具还不能用" : "";
    return `
    <div class="ag${a.off ? " off" : ""}" data-edit="${esc(a.name)}" role="button" tabindex="0" title="编辑「${esc(a.name)}」">
      ${avatar(a)}
      <div class="ag-t">
        <div class="ag-name"><b class="clip">${esc(a.name)}</b>${tag}</div>
        <div class="ag-d clip">${about}</div>
        <div class="ag-meta clip">${esc(hzName(a.runtime))}<span class="ag-dot">·</span>${model}<span class="ag-dot">·</span>${esc(eff)}<span class="ag-dot">·</span>${rec}${hwarn ? `<span class="ag-dot">·</span><span class="mate-warn" title="${esc(h?.reason || "")}">${hwarn}</span>` : ""}</div>
        ${live ? `<div class="ag-warn">${esc(shortModel(a.model))} 已不在 ${esc(hzName(a.runtime))} 的模型列表里，干活时改用 ${esc(shortModel(live))}<button type="button" class="btn sm line" data-edit="${esc(a.name)}">换模型</button></div>` : ""}
      </div>
      <button type="button" class="rt-sw${a.off ? "" : " on"}" data-agsw="${esc(a.name)}" role="switch" aria-checked="${!a.off}"
        ${asst ? 'disabled title="助理一直在岗"' : `title="${a.off ? "打开：接新活" : "关掉：不接新活"}"`}></button>
    </div>`;
  };
  const shown = list.filter((a) => agTab === "all" || (agTab === "off") === !!a.off);
  // 助理排第一，验收第二，其余按花名册顺序
  const rank = (a) => (a.role === "助理" ? 0 : a.name === "验收" ? 1 : 2);
  shown.sort((x, y) => rank(x) - rank(y));
  $("#agents").innerHTML = shown.length ? `<div class="card list ag-list">${shown.map(row).join("")}</div>`
    : `<div class="rt-empty">${agTab === "off" ? "没有关着的 agent" : "还没有 agent"}</div>`;
}
$("#ag-tabs").addEventListener("change", () => { agTab = $("#ag-tabs").dataset.value; renderAgents(agentList); });
$("#ag-new").addEventListener("click", () => openAgentForm(null));
$("#ag-import").addEventListener("click", () => openPrefs("import"));
// ———— 流程：一条消息在 agent 之间怎么走 ————
// 只在助理页右栏画。结束超过 10 分钟的不再显示。
const FLOW_KEEP = 10 * 60 * 1000;
let flowFold = {}; // 用户手动展开或收起过的泳道
const FLOW_ST = { working: "进行中", done: "已完成", failed: "失败", stopped: "已停止" };
function flowDur(f) {
  const s = Math.max(0, Math.round(((f.ended ? Date.parse(f.ended) : Date.now()) - Date.parse(f.started)) / 1000));
  return s < 60 ? `${s} 秒` : s < 3600 ? `${Math.floor(s / 60)} 分钟` : `${Math.floor(s / 3600)} 小时`;
}
// 助理页右栏：每条安排一段，写项目、原话和每一步谁在做。在跑的列出步骤，结束的变淡、只留一行，
// 点标题可以展开或收起步骤。全部列出，多了在右栏里滚动
const LANE_ST = { waiting: "没开始", working: "正在做", done: "做完了", failed: "失败" };
function renderLanes(el, list) {
  const run = list.filter((f) => f.state === "working").length;
  el.innerHTML = `<div class="lane-top"><span>进行中</span><span>${run}</span></div>` + list.map((f) => {
    const open = flowFold[f.id] ?? f.state === "working";
    const where = f.projectName ? esc(f.projectName) : f.state === "working" ? (f.steps.some((st) => st.state === "working" && !st.agent) ? "等你回答" : "处理中") : "对话";
    const steps = f.steps.map((st) => {
      const wait = st.state === "working" && !st.agent; // 等你回答的一步
      return `<div class="lane-st ${wait ? "wait" : st.state}${st.task ? " click" : ""}" ${st.task ? `data-ftask="${st.task}" data-fproj="${f.project}"` : ""}
        title="${esc(`${st.agent || "你"} · ${st.what} · ${LANE_ST[st.state] || st.state}`)}"><i class="lane-ck"></i><span class="clip"><b>${esc(st.agent || "你")}</b>${esc(st.what)}</span></div>`;
    }).join("");
    return `<div class="lane ${f.state}${f.state === "working" ? "" : " old"}">
      <div class="lane-hr"><button type="button" class="lane-h" data-fold="${f.id}" aria-expanded="${open}">
        <span class="tag accent">${where}</span><span class="grow"></span><span>${FLOW_ST[f.state] || esc(f.state)} · ${flowDur(f)}</span></button>
        ${f.state === "working" ? `<button type="button" class="lane-x" data-fstop="${f.id}" title="只停这一条：助理对它的处理和派出的活" aria-label="停止这一条">停止</button>` : ""}</div>
      <div class="lane-q clip2${f.turn && threads[f.turn] ? " click" : ""}" ${f.turn ? `data-turn="${esc(f.turn)}"` : ""} title="${esc(f.text)}${f.turn && threads[f.turn] ? "（点一下跳到这条消息）" : ""}">${esc(f.text)}</div>
      ${open && steps ? `<div class="lane-s">${steps}</div>` : ""}</div>`;
  }).join("");
}
function renderFlows(el, list) {
  const now = Date.now();
  list = list.filter((f) => f.state === "working" || now - Date.parse(f.ended) < FLOW_KEEP);
  el.hidden = !list.length;
  if (list.length) renderLanes(el, list);
}
let flowBusy = false;
async function loadFlows() {
  if (flowBusy || document.hidden || view !== "chat") return;
  flowBusy = true;
  try {
    if (!agentList.length) { agentList = await api("/api/agents"); assignLooks(agentList); }
    renderFlows($("#flows"), await api("/api/flows"));
  } catch {} finally { flowBusy = false; }
}
setInterval(loadFlows, 1500);
$("#flows").addEventListener("click", (e) => {
  const x = e.target.closest("[data-fstop]");
  if (x) {
    x.disabled = true;
    return api(`/api/flows/${x.dataset.fstop}/stop`, { method: "POST" }).catch(() => {}).finally(() => { flowBusy = false; loadFlows(); });
  }
  const fold = e.target.closest("[data-fold]");
  if (fold) {
    flowFold[fold.dataset.fold] = fold.getAttribute("aria-expanded") !== "true";
    flowBusy = false;
    return loadFlows();
  }
  const q = e.target.closest("[data-turn]");
  if (q && threads[q.dataset.turn]) {
    if (view !== "chat") show("chat");
    return gotoTurn(threads[q.dataset.turn]);
  }
  const n = e.target.closest("[data-ftask]");
  if (!n) return;
  // 点任务节点：到项目页打开这个任务
  curProject = +n.dataset.fproj;
  store.set("project", curProject);
  if (view !== "projects") show("projects"); else loadProjects();
  curTask = +n.dataset.ftask;
  trajOpen = false;
});
// shortAgo 只留最大的单位：「2 小时前」「3 天前」
function shortAgo(iso) {
  const m = Math.round((Date.now() - Date.parse(iso)) / 60000);
  if (!(m >= 0)) return "";
  return m < 1 ? "刚刚" : m < 60 ? `${m} 分钟前` : m < 1440 ? `${Math.floor(m / 60)} 小时前` : `${Math.floor(m / 1440)} 天前`;
}
// avatar 是 agent 的头像：浅色渐变的圆角方块，中间一个白色细线几何标记。
// 头像都是 morula 的桑葚，靠颜色区分：助理固定用桑葚紫，成员按花名册顺序，色相和转角各自从名字算出的位置往后取第一个没人用的，
// 前 9 位成员颜色不重样；新加成员不影响已有成员。右下角的点表示是否在干活：绿色在干活，灰色空闲。
function nameHash(name) {
  let h = 0;
  for (const c of name) h = (h * 31 + c.codePointAt(0)) >>> 0;
  return h;
}
// 第一个是助理的桑葚紫，和 build/darwin/icon/morula.svg 一致
const HUES = [254, 205, 168, 335, 22, 142, 45, 290, 188, 228];
const TURNS = [0, 60, 120, 180, 240, 300];
// 桑葚的九颗籽，d、m、l、p 由深到浅，颜色在 app.css 的 .berry 里按 --h 算
const BERRY = [[508, 826, 76, 91, "d"], [760, 481, 78, -11, "d"], [412, 262, 80, -110, "p"], [263, 512, 82, -176, "l"], [675, 732, 92, 51, "d"],
  [632, 324, 102, -59, "l"], [370, 669, 113, 135, "l"], [448, 418, 128, -120, "p"], [593, 575, 132, 30, "m"]]
  .map(([x, y, rx, r, k]) => `<ellipse class="${k}" cx="${x}" cy="${y}" rx="${rx}" ry="137" transform="rotate(${r} ${x} ${y})"/>`).join("");
function berry(t) {
  return `<svg class="berry" viewBox="83 115 858 858" aria-hidden="true"><g transform="rotate(${TURNS[t] || 0} 512 544)">${BERRY}</g></svg>`;
}
function hueVars(c) {
  return `--h:${HUES[c]};--c1:hsl(${HUES[c]} 72% 88%);--c2:hsl(${HUES[c]} 50% 60%)`;
}
let lookOf = {};
// pick 从 start 往后找第一个没用过的序号；都用过了就从头再来一轮
function pick(start, n, used) {
  if (used.size >= n) used.clear();
  let i = start % n;
  while (used.has(i)) i = (i + 1) % n;
  used.add(i);
  return i;
}
function assignLooks(list) {
  lookOf = {};
  const hues = new Set([0]), turns = new Set();
  for (const a of list) {
    if (a.role === "助理") { lookOf[a.name] = [0, 0]; continue; }
    const h = nameHash(a.name);
    lookOf[a.name] = [pick(h, HUES.length, hues), pick(h >>> 4, TURNS.length, turns)];
  }
}
function lookFor(name, me) {
  return lookOf[name] || (name === me ? [0, 0] : [1 + (nameHash(name) % (HUES.length - 1)), (nameHash(name) >>> 4) % TURNS.length]);
}
function avatar(a) {
  const [c, t] = lookFor(a.name, a.role === "助理" ? a.name : ""), on = a.status === "working";
  return `<span class="face${on ? " on" : ""}" style="${hueVars(c)}" title="${on ? "正在做：" + esc(a.task || "工作中") : "空闲"}">${berry(t)}<i></i></span>`;
}
// tileIcon 是卡片左上角的方块：取名字第一个字，底色按名字算，同一个名字颜色不变。
function tileIcon(name, accent) {
  let h = 0;
  for (const c of String(name)) h = (h * 31 + c.codePointAt(0)) >>> 0;
  return `<div class="tile-ic${accent ? " as" : ""}" style="--hue:${h % 360}">${esc([...String(name)][0] || "?")}</div>`;
}
// harness 自己的图标，文件在 icons/ 下（出处见 icons/README.md）。
const HZ_ICONS = { codex: "codex-color.svg", opencode: "opencode.svg", pi: "pi.svg", hermes: "hermesagent.svg", workbuddy: "codebuddy-color.svg" };
// hzName 是 harness 在界面上的名字：workbuddy 跑的是终端版 codebuddy，和桌面版 WorkBuddy 用同一个账号，统一叫 WorkBuddy。
const hzName = (n) => ({ workbuddy: "WorkBuddy" })[n] || n;
// iconFile 画 icons/ 下的一个图标：单色图标用 mask 按文字颜色画，跟着主题变；自带底色方块的彩色图标铺满。
const FULL_ICONS = new Set(["codex-color.svg", "codebuddy-color.svg", "kiro-color.svg"]);
function iconFile(f, title) {
  const u = "icons/" + encodeURIComponent(f), t = title ? ` title="${esc(title)}"` : "";
  if (!f.includes("-color")) return `<span class="logo"${t}><i style="--u:url('${u}')"></i></span>`;
  return `<span class="logo${FULL_ICONS.has(f) ? " full" : ""}"${t}><img src="${u}" alt=""></span>`;
}
// harnessIcon 有图标时用图标，没有时退回 tileIcon。
function harnessIcon(name) {
  return HZ_ICONS[name] ? iconFile(HZ_ICONS[name], hzName(name)) : tileIcon(hzName(name));
}
$("#v-agents").addEventListener("click", async (e) => {
  const sw = e.target.closest("[data-agsw]");
  if (sw) {
    if (sw.disabled) return;
    const a = agentList.find((x) => x.name === sw.dataset.agsw);
    $("#ag-err").textContent = "";
    try {
      renderAgents(await api("/api/agents/" + encodeURIComponent(a.name), { method: "PATCH", body: JSON.stringify({ off: !a.off }) }));
    } catch (err) { $("#ag-err").textContent = err.message; }
    return;
  }
  const ed = e.target.closest("[data-edit]");
  if (ed) openAgentForm(agentList.find((a) => a.name === ed.dataset.edit));
  else if (e.target.closest("[data-staff]")) show("staff");
});
$("#agents").addEventListener("keydown", (e) => {
  const ed = e.target.closest(".ag[data-edit]");
  if (ed && e.target === ed && (e.key === "Enter" || e.key === " ")) { e.preventDefault(); openAgentForm(agentList.find((a) => a.name === ed.dataset.edit)); }
});
const form = $("#agent-form");
let editing = null; // 正在编辑的 agent 名字；null 表示新增
function fillModels(runtime, model, effort) {
  const h = hOf(runtime);
  const models = h ? h.models : [];
  const ms = $("#af-model");
  ms.innerHTML = models.map((m) => `<option value="${esc(m.id)}">${esc(m.name || m.id)}</option>`).join("") +
    (model && !models.some((m) => m.id === model) ? `<option value="${esc(model)}">${esc(model)}（不在列表里）</option>` : "") +
    (models.length ? "" : `<option value="">没有可选模型</option>`);
  if (model) ms.value = model;
  fillEfforts(effort);
}
function fillEfforts(effort) {
  const h = hOf($("#af-runtime").value);
  const m = h?.models.find((x) => x.id === $("#af-model").value);
  const efforts = m?.efforts || [];
  seg($("#af-effort"), [{ v: "", l: m?.default ? `默认（${m.default}）` : "默认" }, ...efforts.map((x) => ({ v: x, l: x }))],
    efforts.includes(effort) ? effort : "");
}
async function openAgentForm(a) {
  await loadHarnesses();
  editing = a ? a.name : null;
  form.reset();
  $("#agent-err").textContent = "";
  $("#af-runtime").innerHTML = harnesses.map((h) => `<option value="${esc(h.name)}">${esc(hzName(h.name))}${h.ready ? "" : "（还不能用）"}</option>`).join("");
  form.name.value = a ? a.name : "";
  form.name.disabled = !!a;
  const roles = ["执行", "审阅", "规划"];
  if (a && a.role === "助理") roles.unshift("助理");
  seg($("#af-role"), roles.map((r) => ({ v: r, l: r })), a ? a.role : "执行");
  form.instructions.value = a ? a.instructions : "";
  const sk = await loadSkills();
  const mine = a ? a.skills || [] : [];
  chips($("#af-skills"), [...sk.map((x) => ({ v: x.name, l: x.name, t: x.description })),
    ...mine.filter((n) => !sk.some((x) => x.name === n)).map((n) => ({ v: n, l: n + "（本机没有）" }))], mine, "本机没有 skill", true);
  chips($("#af-projects"), projectList.map((p) => ({ v: p.id, l: p.name, t: p.summary })), a ? a.projects : [], "还没有项目");
  const rt = a ? a.runtime : (harnesses.find((h) => h.ready) || harnesses[0])?.name;
  $("#af-runtime").value = rt;
  fillModels(rt, a ? a.model : "", a ? a.effort : "");
  form.profile.value = a ? a.profile || "" : "";
  $("#af-title").textContent = a ? "编辑「" + a.name + "」" : "新增 Agent";
  $("#af-submit").textContent = a ? "保存" : "添加";
  $("#af-describe").hidden = !a;
  $("#af-describe").disabled = !!a?.describing;
  $("#af-describe").textContent = a?.describing ? "助理在写…" : "让助理重写";
  $("#af-profile-sub").textContent = a?.describeErr ? "上次没写成：" + a.describeErr : "卡片上给你看的介绍。绑了 skill 时由助理按 skill 写，换了 skill 会重写；你也可以自己改";
  $("#af-del").hidden = !a || a.role === "助理"; // 助理要先把助理交给别人才能删，否则对话没人接
  $("#af-del").dataset.armed = "";
  $("#af-del").textContent = "删除";
  $("#af-assist").hidden = !a || a.role === "助理";
  $("#af-talk").hidden = !a || a.role === "助理";
  $("#agent-dlg").hidden = false;
  (a ? form.instructions : form.name).focus();
}
function closeAgentForm() {
  $("#agent-dlg").hidden = true;
  form.reset();
  $("#agent-err").textContent = "";
}
$("#af-del").addEventListener("click", async (e) => {
  const b = e.currentTarget;
  if (b.dataset.armed !== "1") {
    b.dataset.armed = "1";
    b.textContent = "确认删除？";
    setTimeout(() => { b.dataset.armed = ""; b.textContent = "删除"; }, 2500);
    return;
  }
  try {
    renderAgents(await api("/api/agents/" + encodeURIComponent(editing), { method: "DELETE" }));
    closeAgentForm();
  } catch (err) { $("#agent-err").textContent = err.message; }
});
$("#af-talk").addEventListener("click", async () => {
  try {
    renderDirect((await api("/api/chat/direct", { method: "POST", body: JSON.stringify({ agent: editing }) })).direct);
    closeAgentForm();
    show("chat");
    $("#input").focus();
  } catch (err) { $("#agent-err").textContent = err.message; }
});
$("#af-assist").addEventListener("click", async () => {
  try {
    renderAgents(await api("/api/agents/" + encodeURIComponent(editing) + "/assistant", { method: "POST" }));
    closeAgentForm();
  } catch (err) { $("#agent-err").textContent = err.message; }
});
$("#af-describe").addEventListener("click", async (e) => {
  const b = e.currentTarget;
  b.disabled = true;
  b.textContent = "助理在写…";
  try {
    renderAgents(await api("/api/agents/" + encodeURIComponent(editing) + "/describe", { method: "POST" }));
    $("#af-profile-sub").textContent = "助理在后台写，写好后卡片上会更新，不用等在这里";
  } catch (err) { $("#agent-err").textContent = err.message; b.disabled = false; b.textContent = "让助理重写"; }
});
$("#af-runtime").addEventListener("change", () => fillModels($("#af-runtime").value, "", ""));
$("#af-model").addEventListener("change", () => fillEfforts(""));
$("#agent-cancel").addEventListener("click", closeAgentForm);
form.addEventListener("submit", async (e) => {
  e.preventDefault();
  const body = {
    name: form.name.value, role: $("#af-role").dataset.value, runtime: $("#af-runtime").value,
    model: $("#af-model").value, effort: $("#af-effort").dataset.value, instructions: form.instructions.value,
    skills: chipVals($("#af-skills")), projects: chipVals($("#af-projects")).map(Number),
  };
  const prof = form.profile.value.trim(), old = agentList.find((a) => a.name === editing);
  if (editing ? prof !== (old?.profile || "") : prof) body.profile = prof; // 只在自己改了介绍时才送，没改时换 skill 让助理重写
  try {
    if (editing) {
      const { name, ...patch } = body;
      renderAgents(await api("/api/agents/" + encodeURIComponent(editing), { method: "PATCH", body: JSON.stringify(patch) }));
    } else {
      renderAgents(await api("/api/agents", { method: "POST", body: JSON.stringify(body) }));
    }
    closeAgentForm();
  } catch (err) {
    $("#agent-err").textContent = err.message;
  }
});
setInterval(() => { if (view === "agents") loadAgents().catch(() => {}); }, 4000);

/* ---------- 侧边栏底部 ---------- */
let projectList = [];
function footer() { renderSide(); }
// 侧栏的项目列表：进行中的排前面，其余按最后一次动的时间；两周没动的收进「更早的」，默认项目一直在上面。
const OLD_AFTER = 14 * 864e5;
let olderOpen = store.get("olderOpen") === "1";
function renderSide() {
  const box = $("#sb-projs");
  const cur = view === "projects" ? curProject : -1;
  $(".sb-sec").classList.toggle("on", cur === 0);
  const old = (p) => !p.default && !p.waiting && p.status !== "working" && Date.now() - Date.parse(p.updatedAt) > OLD_AFTER;
  // 新建：助理一天内建的、还没安排过的项目
  const fresh = (p) => !p.default && !p.runs && Date.now() - Date.parse(p.createdAt) < 864e5;
  const rank = (p) => p.status === "working" ? 0 : p.waiting ? 1 : 2;
  const recent = projectList.filter((p) => !old(p)).sort((a, b) => rank(a) - rank(b) || (a.default - b.default) || String(b.updatedAt).localeCompare(a.updatedAt));
  const older = projectList.filter(old);
  const row = (p, cls) => {
    const [k, st] = p.status === "working" ? ["working", "进行中"] : p.waiting ? ["wait", "待批准"] : p.status === "failed" ? ["failed", "失败"]
      : fresh(p) ? ["new", "新建"] : p.default ? ["", "默认"] : ["", cls ? when(p.updatedAt).slice(0, 5) : ""];
    return `<button type="button" class="sb-p${cls}${p.id === cur ? " on" : ""}" data-pid="${p.id}" title="${esc(p.name)}"${p.id === cur ? ' aria-current="page"' : ""}>` +
      `<i class="sb-ic ${k}" aria-hidden="true">${esc([...p.name.trim()][0] || "?")}</i><span class="sb-pn">${esc(p.name)}</span>${st ? `<span class="sb-st ${k}">${st}</span>` : ""}</button>`;
  };
  if (!projectList.length) { box.innerHTML = '<div class="sb-empty">还没有项目</div>'; return; }
  const open = olderOpen || older.some((p) => p.id === cur);
  box.innerHTML = recent.map((p) => row(p, "")).join("") + (older.length
    ? `<button type="button" class="sb-more${open ? " open" : ""}" aria-expanded="${open}"><i class="sb-dot" aria-hidden="true"></i>更早的 ${older.length} 个<svg class="sb-chev" viewBox="0 0 20 20" aria-hidden="true"><path d="M8 6l4 4-4 4"/></svg></button>` +
      (open ? older.map((p) => row(p, " old")).join("") : "")
    : "");
}
$("#sb-projs").addEventListener("click", (e) => {
  if (e.target.closest(".sb-more")) { olderOpen = !olderOpen; store.set("olderOpen", olderOpen ? "1" : "0"); return renderSide(); }
  const b = e.target.closest("[data-pid]");
  if (b) openProject(+b.dataset.pid);
});

/* ---------- 侧栏：收起 ---------- */
function fold(off = !document.body.classList.contains("sb-off")) {
  document.body.classList.toggle("sb-off", off);
  const b = $("#sb-fold");
  b.title = off ? "展开侧栏（⌘\\）" : "收起侧栏（⌘\\）";
  b.setAttribute("aria-label", off ? "展开侧栏" : "收起侧栏");
  store.set("sideOff", off ? "1" : "0");
}
$("#sb-fold").addEventListener("click", () => fold());
$("#sb-add").addEventListener("click", () => openProjectForm());
// 项目列表能收起，像 Qoder 的「最近任务」；收起与否记在本机
function projsFold(off) {
  $("#sb-projs").classList.toggle("shut", off);
  const b = $("#sb-tog");
  b.classList.toggle("off", off);
  b.setAttribute("aria-expanded", String(!off));
  b.title = b.ariaLabel = off ? "展开项目列表" : "收起项目列表";
  store.set("projsOff", off ? "1" : "0");
}
$("#sb-tog").addEventListener("click", () => projsFold(!$("#sb-projs").classList.contains("shut")));
if (store.get("projsOff") === "1") projsFold(true);
if (store.get("sideOff") === "1") fold(true);
/* ---------- 搜索（⌘K） ---------- */
let skHits = [], skAt = 0, skSeq = 0, skTimer = 0;
function skOpen() {
  closePrefs();
  $("#sk").hidden = false;
  $("#sk-q").value = "";
  $("#sk-body").innerHTML = "";
  skHits = [];
  $("#sk-q").focus();
}
function skClose() { $("#sk").hidden = true; }
$("#sb-search").addEventListener("click", skOpen);
$("#sk").addEventListener("mousedown", (e) => { if (e.target.id === "sk") skClose(); });
// mark 把 text 里出现 q 的地方标出来（不分大小写）；先切开再转义
function mark(text, q) {
  const t = String(text || ""), lt = t.toLowerCase(), lq = q.toLowerCase();
  if (!lq) return esc(t);
  let out = "", i = 0;
  for (let j = lt.indexOf(lq); j >= 0; j = lt.indexOf(lq, i)) {
    out += esc(t.slice(i, j)) + "<mark>" + esc(t.slice(j, j + lq.length)) + "</mark>";
    i = j + lq.length;
  }
  return out + esc(t.slice(i));
}
$("#sk-q").addEventListener("input", () => {
  clearTimeout(skTimer);
  skTimer = setTimeout(skFind, 150);
});
async function skFind() {
  const q = $("#sk-q").value.trim(), seq = ++skSeq;
  if (!q) { $("#sk-body").innerHTML = ""; skHits = []; return; }
  const r = await api("/api/search?q=" + encodeURIComponent(q)).catch((err) => ({ err }));
  if (seq !== skSeq) return;
  if (r.err) { $("#sk-body").innerHTML = `<div class="sk-none">${esc(r.err.message)}</div>`; return; }
  const name = (id) => projectList.find((p) => p.id === id)?.name || "";
  skHits = [
    ...r.projects.map((p) => ({ g: "项目", go: () => openProject(p.id), t: mark(p.name, q), s: p.hit ? mark(p.hit, q) : esc(projectList.find((x) => x.id === p.id)?.summary || "") })),
    ...r.runs.map((x) => ({ g: "安排", go: () => openProject(x.projectId), t: mark(x.request, q), s: esc([name(x.projectId), when(x.createdAt), RUN_ST[x.status] || x.status].filter(Boolean).join(" · ")) })),
    ...r.chats.map((c) => ({ g: "对话", go: () => jumpChat(c.day, (el) => el.textContent.toLowerCase().includes(q.toLowerCase())), t: mark(c.line, q), s: esc(dayLabel(c.day)) })),
  ];
  skAt = 0;
  skRender();
}
function skRender() {
  if (!skHits.length) { $("#sk-body").innerHTML = '<div class="sk-none">没找到</div>'; return; }
  let g = "";
  $("#sk-body").innerHTML = skHits.map((h, i) => (h.g !== g ? `<div class="sk-g">${(g = h.g)}</div>` : "") +
    `<button type="button" class="sk-r${i === skAt ? " on" : ""}" data-i="${i}"><span class="sk-t">${h.t}</span>${h.s ? `<span class="sk-s">${h.s}</span>` : ""}</button>`).join("");
}
function skGo(i) {
  const h = skHits[i];
  if (!h) return;
  skClose();
  h.go();
}
$("#sk-body").addEventListener("click", (e) => { const b = e.target.closest("[data-i]"); if (b) skGo(+b.dataset.i); });
$("#sk-q").addEventListener("keydown", (e) => {
  if (ime(e)) return;
  if (e.key === "ArrowDown" || e.key === "ArrowUp") {
    e.preventDefault();
    if (!skHits.length) return;
    skAt = (skAt + (e.key === "ArrowDown" ? 1 : skHits.length - 1)) % skHits.length;
    skRender();
    $("#sk-body .sk-r.on")?.scrollIntoView({ block: "nearest" });
  }
  if (e.key === "Enter") { e.preventDefault(); skGo(skAt); }
});
// jumpChat 打开对话，把 day 那天加载进来，滚到第一条（今天的是最后一条）符合 pick 的消息并闪一下
async function jumpChat(day, pick) {
  show("chat");
  const past = earlier.includes(day); // 不在「更早」里的就是今天这一段
  while (earlier.includes(day)) await loadEarlier();
  const all = [...chat.querySelectorAll(".msg")].filter((m) => pick(m));
  flash(past ? all[0] : all.at(-1));
}
// jumpTurn 跳到某条用户消息。对话按换日时间分天，和日历日期可能差一天，所以从 iso 的前一天起往回找
async function jumpTurn(turn, iso) {
  show("chat");
  const d = new Date(iso); d.setDate(d.getDate() - 1);
  const floor = d.toLocaleDateString("sv");
  while (!threads[turn] && earlier.length && earlier[0] >= floor) await loadEarlier();
  flash(threads[turn]?.msg);
}
function flash(m) {
  if (!m) return;
  follow = false;
  m.scrollIntoView({ block: "center" });
  m.classList.remove("sk-hit");
  void m.offsetWidth;
  m.classList.add("sk-hit");
}

/* ---------- 例行事项 ---------- */
let routines = [];
let rtTab = "list";
let rtEdit = null; // 表单里正在改的那件；新建时为空
const WD = ["日", "一", "二", "三", "四", "五", "六"];
const rtDays = (d) => {
  if ((d & 127) === 127) return "每天";
  if (d === 62) return "工作日";
  if (d === 65) return "周末";
  const on = [1, 2, 3, 4, 5, 6, 0].filter((i) => d & (1 << i));
  return "每周" + on.map((i) => WD[i]).join("、");
};
// 「今天 08:30」「昨天 21:00」「10-13 09:00」
function rtWhen(iso) {
  const t = new Date(iso);
  if (isNaN(t)) return "";
  const hm = t.toTimeString().slice(0, 5), d0 = new Date(); d0.setHours(0, 0, 0, 0);
  const days = Math.round((new Date(t).setHours(0, 0, 0, 0) - d0) / 864e5);
  return (days === 0 ? "今天" : days === -1 ? "昨天" : days === 1 ? "明天" : String(iso).slice(5, 10)) + " " + hm;
}
function rtTook(a, b) {
  const s = Math.round((Date.parse(b) - Date.parse(a)) / 1000);
  if (!(s >= 0)) return "";
  return "用时 " + (s < 60 ? s + " 秒" : s < 3600 ? Math.round(s / 60) + " 分" : (s / 3600).toFixed(1) + " 小时");
}
const rtWho = (a) => a || "助理";
function rtResult(x) {
  const at = rtWhen(x.startedAt), see = x.turn ? `<a data-turn="${esc(x.turn)}" data-at="${esc(x.startedAt)}">看对话</a>` : "";
  switch (x.status) {
    case "working": return `<i class="dot working"></i><span>${at} 发出，正在做</span>${see}`;
    case "done": return `<i class="dot done"></i><span>${at} 做完 · ${rtTook(x.startedAt, x.endedAt)}</span>${see}`;
    case "failed": return `<i class="dot failed"></i><span>${at} 失败${x.note ? " · " + esc(x.note) : ""}</span>${see}`;
    default: return `<i class="dot stopped"></i><span>${at} 停了${x.note ? " · " + esc(x.note) : ""}</span>${see}`;
  }
}
async function loadRoutines() {
  if (!agentList.length) agentList = await api("/api/agents").catch(() => []);
  routines = await api("/api/routines");
  $("#rt-n").textContent = routines.length;
  setSeg($("#rt-tabs"), rtTab);
  if (rtTab === "runs") return rtRuns();
  if (!routines.length) {
    $("#rt-body").innerHTML = `<div class="card rt-empty">还没有例行事项。点「＋ 新建」，写一句到点要助理做的话，例如「昨天做完什么、要我看什么、今天打算做什么」。</div>`;
    return;
  }
  const pname = (id) => projectList.find((p) => p.id === id)?.name;
  $("#rt-body").innerHTML = `<div class="rt-grid">${routines.map((r) => `
    <div class="card rt-card${r.enabled ? "" : " off"}" data-rt="${r.id}">
      <div class="rt-top"><span class="rt-name">${esc(r.name)}</span><button type="button" class="rt-sw${r.enabled ? " on" : ""}" data-sw="${r.id}" role="switch" aria-checked="${r.enabled}" title="${r.enabled ? "关掉" : "打开"}"></button></div>
      <div class="rt-q">「${esc(r.prompt)}」</div>
      <div class="rt-meta"><span class="rt-when">${rtDays(r.days)} ${esc(r.at)}</span><span>${esc(rtWho(r.agent))}${pname(r.projectId) ? " · " + esc(pname(r.projectId)) : ""}</span>${r.next ? `<span class="rt-next">· 下次 ${rtWhen(r.next)}</span>` : r.enabled ? "" : `<span class="rt-next">· 关着</span>`}</div>
      <div class="rt-last">${r.last ? rtResult(r.last) : "<span>还没跑过</span>"}<span class="rt-acts"><button type="button" class="btn sm" data-run="${r.id}">现在跑一次</button></span></div>
    </div>`).join("")}</div>`;
}
async function rtRuns() {
  const runs = await api("/api/routines/runs");
  const st = { working: "正在做", done: "做完", failed: "失败", stopped: "停了" };
  $("#rt-body").innerHTML = `<div class="card list rt-log">${runs.length ? runs.map((x) => `
    <div class="row line"><i class="dot ${x.status === "done" ? "done" : x.status === "working" ? "working" : x.status === "failed" ? "failed" : "stopped"}"></i>
      <span class="rt-ln"><b>${esc(x.name || "已删除的事项")}</b> · ${st[x.status] || esc(x.status)}${x.status === "done" ? " · " + rtTook(x.startedAt, x.endedAt) : ""}${x.note ? " · " + esc(x.note) : ""}</span>
      <span class="rt-lt">${rtWhen(x.startedAt)}</span>${x.turn ? `<a class="btn sm" data-turn="${esc(x.turn)}" data-at="${esc(x.startedAt)}">看对话</a>` : ""}</div>`).join("")
    : '<div class="row empty">还没有执行记录</div>'}</div>`;
}
$("#rt-tabs").addEventListener("change", () => { rtTab = $("#rt-tabs").dataset.value; guard("routines", loadRoutines); });
$("#rt-body").addEventListener("click", async (e) => {
  const t = e.target.closest("[data-turn]");
  if (t) return jumpTurn(t.dataset.turn, t.dataset.at);
  const sw = e.target.closest("[data-sw]"), run = e.target.closest("[data-run]");
  $("#rt-err").textContent = "";
  try {
    if (sw) {
      const r = routines.find((x) => x.id === +sw.dataset.sw);
      sw.disabled = true;
      await api("/api/routines/" + r.id, { method: "PUT", body: JSON.stringify({ ...r, enabled: !r.enabled }) });
      return loadRoutines();
    }
    if (run) {
      run.disabled = true;
      await api(`/api/routines/${run.dataset.run}/run`, { method: "POST" });
      return loadRoutines();
    }
  } catch (err) {
    $("#rt-err").textContent = err.message;
    return loadRoutines();
  }
  const c = e.target.closest("[data-rt]");
  if (c) rtOpen(routines.find((x) => x.id === +c.dataset.rt));
});
$("#rt-new").addEventListener("click", () => rtOpen(null));
function rtOpen(r) {
  rtEdit = r;
  const f = $("#rt-form");
  $("#rf-title").textContent = r ? "修改例行事项" : "新建例行事项";
  $("#rf-submit").textContent = r ? "保存" : "新建";
  $("#rf-del").hidden = !r;
  $("#rf-del").textContent = "删除";
  delete $("#rf-del").dataset.sure;
  $("#rf-err").textContent = "";
  f.name.value = r?.name || "";
  f.prompt.value = r?.prompt || "";
  f.at.value = r?.at || "08:30";
  const days = r ? r.days : 127;
  $("#rf-days").innerHTML = [1, 2, 3, 4, 5, 6, 0].map((i) => `<label><input type="checkbox" value="${i}"${days & (1 << i) ? " checked" : ""}>${WD[i]}</label>`).join("");
  const me = agentList.find((a) => a.role === "助理")?.name || "助理";
  f.agent.innerHTML = `<option value="">${esc(me)}</option>` + agentList.filter((a) => a.role !== "助理").map((a) => `<option>${esc(a.name)}</option>`).join("");
  // 花名册里已经没有的 agent 也列出来，免得打开表单就把它改成助理
  if (r?.agent && ![...f.agent.options].some((o) => o.value === r.agent)) f.agent.insertAdjacentHTML("beforeend", `<option>${esc(r.agent)}</option>`);
  f.agent.value = r?.agent || "";
  f.projectId.innerHTML = '<option value="0">由助理判断</option>' + projectList.map((p) => `<option value="${p.id}">${esc(p.name)}</option>`).join("");
  f.projectId.value = String(r?.projectId || 0);
  $("#rt-dlg").hidden = false;
  f.name.focus();
}
function rtClose() { $("#rt-dlg").hidden = true; rtEdit = null; }
$("#rf-cancel").addEventListener("click", rtClose);
$("#rt-dlg").addEventListener("mousedown", (e) => { if (e.target.id === "rt-dlg") rtClose(); });
$("#rt-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  const f = e.target;
  const days = [...$("#rf-days").querySelectorAll("input:checked")].reduce((d, x) => d | (1 << +x.value), 0);
  const body = { name: f.name.value, prompt: f.prompt.value, at: f.at.value, days, agent: f.agent.value, projectId: +f.projectId.value, enabled: rtEdit ? rtEdit.enabled : true };
  try {
    await api(rtEdit ? "/api/routines/" + rtEdit.id : "/api/routines", { method: rtEdit ? "PUT" : "POST", body: JSON.stringify(body) });
    rtClose();
    loadRoutines();
  } catch (err) { $("#rf-err").textContent = err.message; }
});
$("#rf-del").addEventListener("click", async () => {
  // 窗口里没有 confirm 弹窗，点两次才删
  const del = $("#rf-del");
  if (!rtEdit) return;
  if (!del.dataset.sure) { del.dataset.sure = "1"; del.textContent = "再点一次删除（执行记录一起删）"; return; }
  await api("/api/routines/" + rtEdit.id, { method: "DELETE" }).catch((err) => { $("#rf-err").textContent = err.message; });
  rtClose();
  loadRoutines();
});
setInterval(() => { if (view === "routines" && $("#rt-dlg").hidden) loadRoutines().catch(() => {}); }, 15000);

/* ---------- 项目 ---------- */
let curProject = +store.get("project") || 0; // 0 表示项目总览
let detail = null;           // 当前项目的详情：{project, runs, live, running}
let curTask = 0;             // 抽屉里显示的任务
let trajOpen = false;        // 抽屉里是否展开了轨迹
const openRuns = new Set();  // 展开的安排
let openRunsOf = 0;          // openRuns 属于哪个项目；换项目时重置
let mvOpen = false;          // 项目记忆的旧版本列表展开着
const RUN_ST = { working: "进行中", done: "已完成", failed: "失败", stopped: "已停止" };
const when = (iso) => esc(String(iso || "").slice(5, 16).replace("T", " "));

function setCrumb() {
  const p = curProject && detail?.project;
  $("#crumb-proj-sep").hidden = !p;
  $("#crumb-proj").textContent = p ? p.name : "";
  const top = $("[data-crumb-projects]");
  top.outerHTML = p ? `<span data-crumb-projects>项目</span>` : `<b data-crumb-projects>项目</b>`;
}
async function loadProjects() {
  projectList = await api("/api/projects").catch(() => projectList);
  fillNames(projectList);
  if (!curProject && !agentList.length) agentList = await api("/api/agents").catch(() => []);
  footer();
  if (curProject && !projectList.some((p) => p.id === curProject)) curProject = 0;
  if (curProject) return loadProject();
  detail = null;
  setCrumb();
  closeTask();
  const by = {};
  for (const a of agentList) for (const id of a.projects || []) (by[id] ||= []).push(a.name);
  const add = `
    <div class="card tile tile-add">
      <button type="button" class="mate-add-btn" data-new-project>＋ 新建项目</button>
      <div class="faint">也可以在「助理」里说要做的事，助理会建项目</div>
    </div>`;
  const ordered = [...projectList.filter((p) => p.default), ...projectList.filter((p) => !p.default)];
  $("#proj-main").innerHTML = `<div class="tiles">${add}${ordered.map((p) => {
      const ags = by[p.id] || [];
      const seg = String(p.dir || "").split("/").filter(Boolean);
      const dir = seg.length > 2 ? "…/" + seg.slice(-2).join("/") : p.dir || "";
      return `
      <div class="card tile proj-tile" data-pid="${p.id}">
        <div class="tile-hd">
          ${tileIcon(p.name)}
          <div class="tile-t">
            <div class="name clip">${esc(p.name)}</div>
            <div class="tile-st">${p.status ? status(p.status) : '<span class="faint">还没有安排</span>'}<span class="faint">· ${when(p.updatedAt)} 更新</span></div>
          </div>
          <button class="btn pill">打开</button>
        </div>
        <div class="tile-d clip2">${esc(p.summary) || '<span class="faint">还没有项目摘要</span>'}</div>
        <div class="tile-ft">
          ${p.default ? '<span class="chip def" title="闲聊、问答和不属于其他项目的零散小事归到这里">默认项目</span>' : ""}
          ${dir ? `<span class="chip mono dir" title="${esc(p.dir)}">${esc(dir)}</span>` : ""}
          ${ags.length ? `<span class="chip" title="${esc(ags.join("、"))}">${ags.length} 个常用 agent</span>` : ""}
        </div>
      </div>`;
    }).join("")}</div>`;
}
async function loadProject() {
  try { detail = await api("/api/projects/" + curProject); }
  catch { detail = null; return; }
  if (openRunsOf !== curProject) {
    openRunsOf = curProject;
    openRuns.clear();
    mvOpen = false;
    if (detail.runs[0]) openRuns.add(detail.runs[0].id);
  }
  for (const id of Object.keys(detail.running)) openRuns.add(+id);
  renderProject();
}
function openProject(id) {
  curProject = id;
  store.set("project", id);
  closeTask();
  show("projects");
}
function pickProject(id) {
  curProject = id;
  store.set("project", id);
  closeTask();
  dockChat(view === "projects" ? id : 0);
  renderSide();
  loadProjects();
}
document.querySelector("#v-projects header").addEventListener("click", (e) => {
  if (e.target.closest("[data-crumb-projects]") && curProject) pickProject(0);
});

function taskTree(run) {
  const kids = {};
  for (const t of run.tasks) (kids[t.parentId] ||= []).push(t);
  const node = (t) => {
    const live = detail.live[t.id];
    return `<li><div class="node${t.id === curTask ? " on" : ""}" data-tid="${t.id}">
        <i class="dot ${esc(t.status)}"></i>
        <span class="name">${esc(t.agent || "待分配")}</span>
        <span class="task">${esc(t.title)}</span>
        ${t.attempts > 1 ? `<span class="tag warn">第 ${t.attempts} 次</span>` : ""}
        <span class="meta">${STATUS[t.status] || esc(t.status)}</span>
      </div>
      ${live ? `<div class="live mono">${esc(live)}</div>` : ""}
      ${kids[t.id] ? `<ul>${kids[t.id].map(node).join("")}</ul>` : ""}</li>`;
  };
  const top = kids[0] || [];
  return top.length ? `<div class="tree"><ul>${top.map(node).join("")}</ul></div>` : `<div class="sub" style="padding:8px 0">还没有派出任务</div>`;
}
function renderProject() {
  const p = detail.project;
  const runs = detail.runs;
  const vers = detail.memoryVersions || [];
  setCrumb();
  const text = (v, cls, empty) => v ? `<div class="pf-text ${cls}">${esc(v)}</div>` : `<div class="pf-text empty">${empty}</div>`;
  $("#proj-main").innerHTML = `
    <div class="proj-head">
      <div class="grow" style="min-width:0"><h1>${esc(p.name)}</h1><div class="sub mono clip" title="${esc(p.dir)}">${esc(p.dir)}</div></div>
      ${p.owner ? `<span class="chip" title="派到这个项目的活整件交给它">负责人 ${esc(p.owner)}</span>` : ""}
      ${detail.default ? '<span class="chip def" title="闲聊、问答和不属于其他项目的零散小事归到这里">默认项目</span>'
        : '<button class="btn" id="proj-default" title="闲聊、问答和不属于其他项目的零散小事归到这里">设为默认项目</button>'}
      <button class="btn" id="proj-edit">编辑项目</button>
      <button class="btn danger" id="proj-del" title="只删 morula 里的记录，项目文件夹里的文件不动">删除项目</button>
    </div>
    <div class="card list">
      <div class="row top"><div class="pref-l"><div class="name">项目摘要</div><div class="sub">助理只靠它判断消息属不属于这个项目</div></div>
        ${text(p.summary, "", "还没有摘要")}</div>
      <div class="row top"><div class="pref-l"><div class="name">项目记忆</div><div class="sub">选定项目后才加载；每次安排结束由助理更新</div>
        ${vers.length ? `<button class="btn sm mv-tg" type="button" data-mv-toggle aria-expanded="${mvOpen}">旧版本 · ${vers.length}</button>` : ""}</div>
        ${text(p.memory, "mem-view", "还没有记忆")}</div>
      ${mvOpen && vers.length ? `<div class="row mv-list">${vers.map((v) => `
        <div class="mv-row"><div class="mv-h"><span class="muted">${when(v.createdAt)} 被替换</span><span class="grow"></span>
          <button class="btn sm" type="button" data-mv-restore="${v.id}">退回到这一版</button></div>
          <div class="pf-text mv-b">${esc(v.memory)}</div></div>`).join("")}</div>` : ""}
    </div>
    <div class="gl">安排 · ${runs.length}</div>
    <div class="card list">
      ${runs.length ? "" : `<div class="row empty">还没有安排。在右边跟助理说这个项目要做的事。</div>`}
      ${runs.map((r) => {
        const running = detail.running[r.id];
        const st = running ? "working" : r.status;
        const open = openRuns.has(r.id);
        return `
        <div class="row click run-row${open ? " open" : ""}" data-run="${r.id}">
          <span class="chev">▶</span><i class="dot ${esc(st)}"></i>
          <span class="req" title="${esc(r.request)}">${esc(r.request)}</span>
          <span class="when">#${r.id} · ${when(r.createdAt)}</span>
          <span class="state" style="width:52px">${RUN_ST[st] || esc(st)}</span>
          ${running ? `<button class="btn danger" data-stop="${r.id}">停止</button>` : ""}
        </div>
        ${open ? `<div class="run-body">${taskTree(r)}${r.reply ? `<details class="run-rep"><summary>助理的汇报 ›</summary><div class="body">${md(r.reply)}</div></details>` : ""}</div>` : ""}`;
      }).join("")}
    </div>`;
  renderTask();
}
function findTask(id) {
  for (const r of detail?.runs || []) for (const t of r.tasks) if (t.id === id) return t;
  return null;
}
/* ---------- 项目页右边的助理 ---------- */
// 项目页右边就是助理页的那一个对话，搬过来只显示这个项目的消息；在这里发的消息直接归到这个项目（POST /api/chat 的 project）。
// 一条消息算这个项目的：在这个项目页里发的，或助理把它放进了这个项目（回复、放进项目的记录上带着项目编号）。
let pinPid = 0, refilterAt = 0;
const chatCol = document.querySelector(".chat-col"), chatHome = chatCol.parentElement;
function dockChat(pid) {
  if (pid === pinPid) return;
  pinPid = pid;
  if (pid) $("#pchat").appendChild(chatCol);
  else chatHome.prepend(chatCol);
  $("#pchat").hidden = !pid;
  chatCol.classList.toggle("pf", !!pid);
  chatCol.querySelector("header .crumb").innerHTML = pid ? "<b>助理</b><span class=\"muted\">只在这个项目里做事</span>" : "<b>助理</b>";
  if (!directNow) input.placeholder = idlePH();
  applyFilter();
  follow = true; toBottom();
}
function refilter() {
  if (!refilterAt) refilterAt = requestAnimationFrame(() => { refilterAt = 0; applyFilter(); });
}
function applyFilter() {
  let any = false, day = null, dayHas = false;
  const endDay = () => { if (day) day.classList.toggle("pf-off", !dayHas); };
  for (const el of chat.children) {
    if (el.id === "pf-empty") continue;
    if (!pinPid) { el.classList.remove("pf-off"); continue; }
    if (el.classList.contains("day")) { endDay(); day = el; dayHas = false; continue; }
    const th = el.dataset.turn && threads[el.dataset.turn];
    const on = !!th && th.pids.has(pinPid);
    el.classList.toggle("pf-off", !on);
    if (on) { any = true; dayHas = true; }
  }
  endDay();
  let empty = $("#pf-empty");
  if (pinPid && !any) {
    if (!empty) {
      empty = document.createElement("div");
      empty.id = "pf-empty";
      empty.className = "note sys";
      chat.appendChild(empty);
    }
    empty.textContent = "这个项目里还没有对话。在这里发的消息直接归到这个项目，助理只在这个项目里做事。";
  } else if (empty) empty.remove();
}
function closeTask() {
  curTask = 0; trajOpen = false;
  $("#task-panel").hidden = true;
  document.querySelectorAll(".node.on").forEach((n) => n.classList.remove("on"));
}
function renderTask() {
  const panel = $("#task-panel");
  const t = findTask(curTask);
  if (!t) { panel.hidden = true; return; }
  panel.hidden = false;
  const sec = (title, body, cls = "") => body ? `<div class="sec"><div class="sec-h">${title}</div><div class="sec-b ${cls}">${cls.includes("mono") ? esc(body) : md(body)}</div></div>` : "";
  const live = detail.live[t.id];
  const scroll = $(".dr-body", panel)?.scrollTop || 0;
  panel.innerHTML = `
    <div class="dr-hd"><h2>${esc(t.title)}</h2><button class="btn" id="tp-close" title="Esc">关闭</button></div>
    <div class="dr-body">
      <div class="kv">
        <b>状态</b>${status(t.status)}
        <b>执行者</b><span>${esc(t.agent || "待分配")}</span>
        <b>AI 工具</b><span class="mono">${esc(t.runtime || "—")} · ${esc(t.model || "—")}</span>
        <b>位置</b><span>第 ${t.depth} 层 · 第 ${t.attempts} 次</span>
        <b>用量</b><span>${Number(t.tokens || 0).toLocaleString("en-US")}</span>
      </div>
      ${live ? sec("正在做", live, "mono live") : ""}
      ${sec("任务说明", t.brief)}
      ${sec("自动检查", t.check, "mono")}
      ${sec("检查结果", t.checkOut, "mono")}
      ${sec("汇报", t.report)}
      ${sec("打回 / 备注", t.feedback)}
      ${sec("危险操作审批", t.approval)}
      ${t.session ? `<div class="sec"><div class="sec-h">过程 <span class="faint">来自 AI 工具的记录</span><span class="grow"></span><button class="btn line" id="tp-traj">${trajOpen ? "收起" : "展开"}</button></div><div id="traj"></div></div>` : ""}
    </div>`;
  $(".dr-body", panel).scrollTop = scroll;
  if (trajOpen && t.session) loadTraj(t.id);
}
const ROLE = { user: "输入", agent: "agent", tool: "命令", output: "输出" };
async function loadTraj(id) {
  try {
    const steps = await api(`/api/tasks/${id}/trajectory`);
    if (curTask !== id || !$("#traj")) return;
    $("#traj").innerHTML = steps.map((s) => `<div class="step ${esc(s.role)}"><span class="step-r">${ROLE[s.role] || esc(s.role)}</span>` +
      (s.role === "user" || s.role === "output"
        ? `<details><summary class="muted clip">${esc(s.text.slice(0, 80))}</summary><pre class="tool-out">${esc(s.text)}</pre></details>`
        : `<div class="${s.role === "tool" ? "mono" : ""} step-t">${esc(s.text)}</div>`) + `</div>`).join("") || '<div class="muted">没有记录</div>';
  } catch (err) {
    if ($("#traj")) $("#traj").innerHTML = `<div class="form-err">${esc(err.message)}</div>`;
  }
}
$("#proj-main").addEventListener("click", async (e) => {
  const tile = e.target.closest(".proj-tile[data-pid]");
  if (tile) return pickProject(+tile.dataset.pid);
  const n = e.target.closest("[data-tid]");
  if (n) { curTask = +n.dataset.tid; trajOpen = false; renderProject(); return; }
  const s = e.target.closest("[data-stop]");
  if (s) { s.disabled = true; await api(`/api/runs/${s.dataset.stop}/stop`, { method: "POST" }).catch(() => {}); return loadProject(); }
  const r = e.target.closest("[data-run]");
  if (r) {
    const id = +r.dataset.run;
    openRuns.has(id) ? openRuns.delete(id) : openRuns.add(id);
    return renderProject();
  }
  if (e.target.id === "proj-edit") { openProjectForm(detail.project); return; }
  if (e.target.closest("[data-mv-toggle]")) { mvOpen = !mvOpen; return renderProject(); }
  const mv = e.target.closest("[data-mv-restore]");
  if (mv) {
    if (mv.dataset.armed !== "1") {
      mv.dataset.armed = "1"; mv.textContent = "确认退回？";
      setTimeout(() => { mv.dataset.armed = ""; mv.textContent = "退回到这一版"; }, 2500);
      return;
    }
    mv.disabled = true;
    await api(`/api/projects/${curProject}/memory/restore`, { method: "POST", body: JSON.stringify({ version: +mv.dataset.mvRestore }) }).catch(() => {});
    return loadProject();
  }
  if (e.target.id === "proj-default") {
    await api(`/api/projects/${curProject}/default`, { method: "POST" }).catch(() => {});
    return loadProjects();
  }
  if (e.target.id === "proj-del") {
    const b = e.target;
    if (b.dataset.armed !== "1") {
      b.dataset.armed = "1"; b.textContent = "确认删除？";
      setTimeout(() => { b.dataset.armed = ""; b.textContent = "删除项目"; }, 2500);
      return;
    }
    await api("/api/projects/" + curProject, { method: "DELETE" }).catch(() => {});
    pickProject(0);
  }
});
$("#task-panel").addEventListener("click", (e) => {
  if (e.target.id === "tp-close") { closeTask(); return; }
  if (e.target.id === "tp-traj") { trajOpen = !trajOpen; renderTask(); }
});
// 弹窗开着时不重画，免得打断输入
const editingProject = () => !$("#proj-dlg").hidden;
let soon = null;
function refreshProjectSoon() {
  clearTimeout(soon);
  soon = setTimeout(() => { if (view === "projects" && !editingProject()) loadProjects(); }, 300);
}
setInterval(() => {
  if (view !== "projects" || editingProject()) return;
  const busy = detail && Object.keys(detail.running).length;
  if (busy || Date.now() % 6000 < 2000) loadProjects();
}, 2000);
setInterval(() => { if (view !== "projects") api("/api/projects").then((l) => { projectList = l; fillNames(l); footer(); }).catch(() => {}); }, 5000);

// 项目弹窗：p 为空时新建，否则编辑这个项目的摘要和记忆（项目名不能改）。
const pform = $("#proj-form");
let pformOf = 0;
async function openProjectForm(p) {
  pform.reset();
  if (p && !agentList.length) agentList = await api("/api/agents").catch(() => []);
  const own = agentList.filter((a) => a.role !== "助理" && a.role !== "验收");
  pform.owner.innerHTML = `<option value="">没有，由助理兼任</option>` + own.map((a) => `<option value="${esc(a.name)}">${esc(a.name)}</option>`).join("") +
    (p?.owner && !own.some((a) => a.name === p.owner) ? `<option value="${esc(p.owner)}">${esc(p.owner)}（不在团队里）</option>` : "");
  pform.owner.value = p?.owner || "";
  $("#pf-owner-row").hidden = !p;
  pformOf = p ? p.id : 0;
  $("#pf-err").textContent = "";
  $("#pf-title").textContent = p ? `编辑「${p.name}」` : "新建项目";
  $("#pf-submit").textContent = p ? "保存" : "新建";
  $("#pf-name-row").hidden = !!p;
  $("#pf-mem-row").hidden = !p;
  pform.name.required = !p;
  pform.summary.value = p ? p.summary : "";
  pform.memory.value = p ? p.memory : "";
  $("#proj-dlg").hidden = false;
  (p ? pform.summary : pform.name).focus();
}
function closeProjectForm() { $("#proj-dlg").hidden = true; pform.reset(); }
$("#pf-cancel").addEventListener("click", closeProjectForm);
$("#proj-dlg").addEventListener("mousedown", (e) => { if (e.target.id === "proj-dlg") closeProjectForm(); });
$("#proj-main").addEventListener("click", (e) => { if (e.target.closest("[data-new-project]")) openProjectForm(); });
pform.addEventListener("submit", async (e) => {
  e.preventDefault();
  try {
    if (pformOf) {
      await api("/api/projects/" + pformOf, { method: "PATCH", body: JSON.stringify({ summary: pform.summary.value, memory: pform.memory.value, owner: pform.owner.value }) });
      closeProjectForm();
      return loadProjects();
    }
    const p = await api("/api/projects", { method: "POST", body: JSON.stringify({ name: pform.name.value, summary: pform.summary.value }) });
    closeProjectForm();
    pickProject(p.id);
  } catch (err) { $("#pf-err").textContent = err.message; }
});

/* ---------- 设置 ---------- */
// 改了就存，不用点保存。
const lim = $("#limits");
const limSeg = (k) => lim.querySelector(`.seg[data-name="${k}"]`);
for (const el of lim.querySelectorAll(".seg[data-opts]")) seg(el, el.dataset.opts.split(",").map((v) => ({ v, l: v })), "");
async function loadSettings() {
  const l = await api("/api/settings");
  setSeg(limSeg("maxDepth"), l.maxDepth);
  setSeg(limSeg("retries"), l.retries);
  lim.parallel.value = l.parallel;
  lim.root.value = l.root;
  $("#limits-st").textContent = "";
  api("/api/gateway").then(renderGateway, (err) => renderGateway({ error: err.message }));
  api("/api/onboard").then((o) => {
    const by = Object.entries(o.byHarness || {}).map(([h, n]) => `${hzName(h)} ${n}`).join(" · ");
    $("#onboard-sub").textContent = o.organize.state === "working" ? "助理正在整理…" : o.organize.state === "ready" ? "整理结果等你勾选" :
      o.pending ? `还有 ${o.pending} 个对话没整理（${by}）` : "没有要整理的对话";
  }, (err) => { $("#onboard-sub").textContent = err.message; });
}

/* ---------- 设置 · Harness ---------- */
// 每个 harness 一行，写版本、能不能用、为什么；有新版本时按原来的安装方式更新。
// 没装的收在页底，列官方安装命令：可以复制到终端跑，也可以点「安装」由 morula 跑。
// 每一行有开关：关掉的工具在 morula 里不出现（和启动流程第 1 步勾的是同一份）；「订阅」打开和第 2 步一样的订阅窗口。
// hzAll 是全部工具，含没勾的；全局的 harnesses 只有勾上的，Agent 页和对话框用它。
let hzAll = [], hzTools = {}, hzPicked = {}; // hzPicked：工具 → 勾上的模型数
async function loadHarnessPane() {
  $("#hz-st").textContent = "正在查版本…";
  const [all, tools, on] = await Promise.all([api("/api/harnesses?cli"), api("/api/tools"), api("/api/harnesses")]);
  hzAll = all;
  hzPicked = Object.fromEntries(on.map((h) => [h.name, (h.models || []).length]));
  hzTools = Object.fromEntries(tools.map((t) => [t.name, t]));
  $("#hz-st").textContent = "";
  const have = hzAll.filter((h) => !h.cli || h.cli.path), miss = hzAll.filter((h) => h.cli && !h.cli.path);
  $("#harnesses").innerHTML = have.map(harnessRow).join("") || `<div class="row empty">本机还没有 AI 工具，从下面挑一个安装</div>`;
  const ups = have.filter((h) => h.cli?.update);
  $("#hz-up").hidden = !ups.length;
  $("#hz-up").innerHTML = `<div class="row"><div class="pref-l"><div class="name">${ups.length} 个 AI 工具有新版本</div>
    <div class="sub clip" title="${esc(ups.map((h) => `${h.name} ${h.cli.version} → ${h.cli.latest}`).join("\n"))}">${ups.map((h) => esc(hzName(h.name))).join("、")}</div></div>
    <button class="btn primary" type="button" data-hz-all>全部更新</button></div>`;
  $("#hz-more").hidden = !miss.length;
  $("#hz-more").innerHTML = `<div class="row click${hzMore ? " open" : ""}" data-hz-more><div class="pref-l"><div class="name">安装其他 AI 工具（${miss.length}）</div>
    <div class="sub">把命令复制到「终端」里运行，装好后回到这一页刷新。用 npm 安装的要先装 Node.js</div></div><span class="chev">›</span></div>
    <div${hzMore ? "" : " hidden"}>${miss.map(installRow).join("")}</div>`;
}
let hzMore = false;
function installRow(h) {
  const sub = h.reason === "未接入" ? "morula 暂时还不能用它干活" : "装好就能用";
  return `<div class="row hz-in"><div class="hz-ic">${harnessIcon(h.name)}</div><div class="pref-l"><div class="name">${esc(hzName(h.name))}</div><div class="sub">${sub}</div>
    <div class="hz-cmds">${(h.cli.install || []).map((c) => `<div class="hz-cmd"><span class="tag">${esc(c.via)}</span><code class="mono clip" title="${esc(c.command)}">${esc(c.command)}</code>
      <button class="btn line" type="button" data-cp="${esc(c.command)}">复制</button>
      <button class="btn primary" type="button" data-hz-install="${esc(h.name)}" data-via="${esc(c.via)}"${c.missing ? ` disabled title="${esc(c.missing)}"` : ""}>${c.missing ? esc(c.missing) : "安装"}</button></div>`).join("")}</div>
    <div class="sub" data-hz-out="${esc(h.name)}" hidden></div></div></div>`;
}
// 点「安装」：morula 跑这条官方安装命令，装完刷新所在的页面。设置页和接入模型的弹窗共用。
document.addEventListener("click", async (e) => {
  const b = e.target.closest("[data-hz-install]");
  if (!b) return;
  const name = b.dataset.hzInstall, out = b.closest(".row").querySelector(`[data-hz-out="${CSS.escape(name)}"]`);
  b.closest(".hz-cmds").querySelectorAll("button[data-hz-install]").forEach((x) => { x.disabled = true; });
  b.textContent = "安装中…";
  out.hidden = false;
  out.className = "sub";
  out.textContent = "正在安装，通常要半分钟到几分钟，装完会自动刷新。";
  try {
    await api(`/api/harnesses/${encodeURIComponent(name)}/install`, { method: "POST", body: JSON.stringify({ via: b.dataset.via }) });
    if (!$("#setup").hidden) checkReady();
    if (!$("#v-harness").hidden) loadHarnessPane();
  } catch (err) {
    out.className = "form-err";
    out.textContent = `没装上：${err.message}`;
    b.textContent = "重试";
    b.closest(".hz-cmds").querySelectorAll("button[data-hz-install]").forEach((x) => { x.disabled = !!x.title; });
  }
});
async function updateHarness(name) {
  const r = await api(`/api/harnesses/${encodeURIComponent(name)}/update`, { method: "POST" });
  return r.output;
}
$("#v-harness").addEventListener("click", async (e) => {
  const sw = e.target.closest("[data-hz-on]"), sb = e.target.closest("[data-hz-sub]");
  if (sb) return openTsub(sb.dataset.hzSub);
  if (sw) {
    const n = sw.dataset.hzOn, on = Object.values(hzTools).filter((t) => t.enabled && t.name !== n).map((t) => t.name);
    if (!sw.classList.contains("on")) on.push(n);
    sw.disabled = true;
    $("#hz-err").textContent = "";
    try {
      await api("/api/tools/enabled", { method: "PUT", body: JSON.stringify({ on }) });
      await loadHarnessPane();
      checkReady();
    } catch (err) { $("#hz-err").textContent = err.message; sw.disabled = false; }
    return;
  }
  const cp = e.target.closest("[data-cp]");
  if (cp) {
    await copyText(cp.dataset.cp);
    cp.textContent = "已复制";
    setTimeout(() => { cp.textContent = "复制"; }, 1500);
    return;
  }
  if (e.target.closest("[data-hz-more]")) {
    hzMore = !hzMore;
    const r = e.target.closest("[data-hz-more]");
    r.classList.toggle("open", hzMore);
    r.nextElementSibling.hidden = !hzMore;
    return;
  }
  const one = e.target.closest("[data-hz-up]"), all = e.target.closest("[data-hz-all]");
  if (!one && !all) return;
  e.stopPropagation();
  const names = one ? [one.dataset.hzUp] : hzAll.filter((h) => h.cli?.update).map((h) => h.name);
  $("#hz-err").textContent = "";
  $("#v-harness").querySelectorAll("[data-hz-up], [data-hz-all]").forEach((b) => { b.disabled = true; });
  const errs = [];
  for (const n of names) {
    $("#hz-st").textContent = `正在更新 ${n}…`;
    try { await updateHarness(n); } catch (err) { errs.push(`${n}：${err.message}`); }
  }
  $("#hz-err").textContent = errs.join("\n\n");
  await loadHarnessPane().catch((err) => { $("#hz-err").textContent = err.message; });
});
// 每个 harness 一行：名字、版本、能力；下面一行写能不能用、为什么。点开看它的说明（harness.go 的 Profile）和模型。
const CAN = [["tools", "能用 morula 的工具"], ["resume", "能接着上次做"], ["images", "能看图片"]];
const hvOpen = new Set();
function harnessRow(h) {
  const ms = h.models || [], own = ms.filter((m) => !m.via), via = ms.length - own.length, pf = h.profile, c = h.cli || {}, t = hzTools[h.name] || {};
  const sub = c.broken ? "装了，但打不开，点开看原因" : !t.enabled ? "关着，morula 里不用它" : h.ready && (hzPicked[h.name] ?? ms.length) < ms.length ? `可以干活 · 勾了 ${hzPicked[h.name]} / ${ms.length} 个模型`
    : h.ready ? `可以干活 · ${own.length} 个模型` + (via ? ` · 另有 ${via} 个插件模型` : "")
    : h.reason === "未接入" ? "已装，morula 暂时还不能用它干活" : h.reason || "";
  const tags = (c.version ? `<span class="tag mono">${esc(c.version)}</span>` : "") +
    (pf ? CAN.filter(([k]) => pf[k]).map(([, l]) => `<span class="tag">${l}</span>`).join("") : "");
  const open = hvOpen.has(h.name), more = !!(pf || c.path);
  const right = (c.update ? `<button class="btn line" type="button" data-hz-up="${esc(h.name)}" title="${esc(c.command)}">更新到 ${esc(c.latest)}</button>`
    : `<span class="state"><i class="dot ${h.ready && t.enabled ? "done" : c.broken ? "failed" : "idle"}"></i>${!t.enabled ? "没用" : h.ready ? "已就绪" : "不能用"}</span>`) +
    (t.sub && t.enabled && !c.broken ? `<button class="btn line" type="button" data-hz-sub="${esc(h.name)}">订阅</button>` : "") +
    `<button type="button" class="rt-sw${t.enabled ? " on" : ""}" data-hz-on="${esc(h.name)}" role="switch" aria-checked="${!!t.enabled}" title="${t.enabled ? "关掉此工具" : "打开此工具"}"${c.broken && !t.enabled ? " disabled" : ""}></button>`;
  const how = c.command ? `${c.command}` + (c.latest && !c.update ? `（已是最新 ${c.latest}）` : "") : "看不出是怎么装的，请按原来的方式自己更新";
  const body = !more ? "" : `<div class="hv-body"${open ? "" : " hidden"}><div class="kv">
      ${c.path ? `<b>位置</b><span class="mono">${esc(c.path)}</span><b>更新</b><span class="mono">${esc(how)}</span>` : ""}
      ${c.broken ? `<b>原因</b><span class="mono">${esc(c.broken)}</span>` : ""}
      ${c.broken && c.install?.length ? `<b>重装</b><span class="mono">${c.install.map((x) => esc(x.command)).join("<br>")}</span>` : ""}
      ${pf ? `<b>命令</b><span class="mono">${esc(pf.bin)}</span>
      <b>对话记录</b><span class="mono">${esc(pf.sessions)}</span>
      <b>权限</b><span>${esc(pf.sandbox)}</span>
      <b>能力</b><span>${CAN.map(([k, l]) => (pf[k] ? "" : "不") + l).join("；")}</span>
      ` : ""}${own.length ? `<b>模型</b><span class="hv-ms">${own.map((m) => `<span class="chip mono" title="${esc(m.name || m.id)}${m.efforts?.length ? " · 思考强度 " + esc(m.efforts.join(" / ")) : ""}">${esc(m.id)}</span>`).join("")}</span>` : ""}
    </div></div>`;
  return `<div class="row${more ? " click" : ""}${open ? " open" : ""}" data-hv="${esc(h.name)}"><div class="hz-ic">${harnessIcon(h.name)}</div><div class="pref-l"><div class="acts"><span class="name">${esc(hzName(h.name))}</span>${tags}</div><div class="sub clip" title="${esc(sub)}">${esc(sub)}</div></div>
      ${right}${more ? '<span class="chev">›</span>' : ""}</div>${body}`;
}
$("#harnesses").addEventListener("click", (e) => {
  const r = e.target.closest("[data-hv]");
  if (!r || !r.classList.contains("click") || e.target.closest(".hv-body, button")) return;
  const n = r.dataset.hv, b = r.nextElementSibling;
  if (!b?.classList.contains("hv-body")) return;
  b.hidden = !b.hidden;
  r.classList.toggle("open", !b.hidden);
  b.hidden ? hvOpen.delete(n) : hvOpen.add(n);
});
function renderGateway(g) {
  const row = (name, sub, ok, st) => `<div class="row"><div class="pref-l"><div class="name">${name}</div><div class="sub clip" title="${esc(sub)}">${esc(sub)}</div></div>
    <span class="state"><i class="dot ${ok ? "done" : "failed"}"></i>${st}</span></div>`;
  $("#gateway").innerHTML =
    row("模型转接", g.running ? "只给 morula 自己的 AI 工具用" : g.error || "", g.running, g.running ? "运行中" : "未启动") +
    row("插件环境", g.bun ? `Bun ${g.bunVersion || ""}` : g.bunError || "", !!g.bun, g.bun ? "已就绪" : "缺少 Bun") +
    `<div class="row click" data-pane-go="plugins"><div class="pref-l"><div class="name">插件</div><div class="sub">已装 ${g.plugins ?? 0} 个，启用 ${g.enabled ?? 0} 个</div></div><span class="muted">管理 ›</span></div>`;
}
$("#gateway").addEventListener("click", (e) => { if (e.target.closest("[data-pane-go]")) show("plugins"); });
async function saveLimits() {
  const body = { maxDepth: +limSeg("maxDepth").dataset.value, parallel: +lim.parallel.value, retries: +limSeg("retries").dataset.value, root: lim.root.value };
  try {
    await api("/api/settings", { method: "POST", body: JSON.stringify(body) });
    $("#limits-st").textContent = "已保存";
  } catch (err) { $("#limits-st").textContent = err.message; }
}
lim.addEventListener("change", saveLimits);
lim.addEventListener("submit", (e) => { e.preventDefault(); saveLimits(); });

$("#onboard-set").addEventListener("click", () => show("import"));

/* ---------- 整理以前的对话 ---------- */
// 助理上线后在对话里问一次要不要整理；回答过后从设置进来。
let imp = null, impTimer = 0, impDone = null;
async function loadImport(rescan) {
  clearTimeout(impTimer);
  try {
    imp = await api("/api/onboard" + (rescan ? "?rescan=1" : ""));
    $("#imp-err").textContent = "";
  } catch (err) { $("#imp-err").textContent = err.message; return; }
  renderImport();
  if (imp.organize.state === "working" && pane === "import") impTimer = setTimeout(loadImport, 3000);
}
function renderImport() {
  const o = imp.organize, body = $("#imp-body");
  const by = Object.entries(imp.byHarness || {}).map(([h, n]) => `${esc(hzName(h))} ${n} 个`).join("，");
  $("#imp-st").textContent = o.state === "working" ? "整理中…" : "";
  if (o.state === "ready") return renderProposals(o);
  const done = impDone ? `<div class="note sys">${impDone}</div>` : "";
  if (o.state === "working") {
    const min = Math.max(0, Math.round((Date.now() - Date.parse(o.started)) / 60000));
    body.innerHTML = done + `<div class="card list"><div class="row"><div class="pref-l"><div class="name">助理在整理 ${o.sessions.length} 个对话</div>
      <div class="sub">${esc(o.model)} · 已用 ${min} 分钟。可以先去别的页面，整理完回到这里勾选。</div></div>${status("working")}</div></div>`;
    return;
  }
  // 照画布「从别的工具导入」：每个来源一张卡片，写明新会话、跳过的会话、上次整理的时间，各自能整理
  const AV = { codex: "cx", opencode: "oc" };
  const cards = (imp.sources || []).map((x) => {
    const n = Math.min(x.pending, 200);
    const sub = !x.found ? `本机没找到它的数据：${x.dir}` : x.last ? `本机 · 上次整理 ${String(x.last).slice(5, 16).replace("T", " ")}` : "本机 · 没整理过";
    const btn = x.found && x.pending ? `<button class="btn line sm" data-org="${esc(x.name)}" ${imp.assistant ? "" : "disabled"}>整理新的 ${n} 个对话</button>` : "";
    return `<div class="card list imp-src">
      <div class="row"><span class="imp-av mono">${esc(AV[x.name] || x.name.slice(0, 2))}</span><div class="pref-l"><div class="name">${esc(hzName(x.name))}</div><div class="sub clip" title="${esc(x.dir)}">${esc(sub)}</div></div><span class="grow"></span>${btn}</div>
      ${x.found ? `<div class="row imp-k"><span class="name">对话</span><span class="imp-v">${x.pending ? `新的 ${x.pending} 个` : "没有新的"}${x.skipped ? `<span class="faint">另有 ${x.skipped} 个整理过或是 morula 自己的，跳过</span>` : ""}${x.pending > n ? `<span class="faint">一次整理最近的 ${n} 个，剩下的下次再整理</span>` : ""}</span><span class="tag">${x.pending ? "新 " + x.pending : "无新"}</span></div>` : ""}
    </div>`;
  }).join("");
  const skill = (imp.skillDirs || []).length ? `<div class="card list imp-src"><div class="row imp-k"><span class="name">skill</span><span class="imp-v"><span>不用搬，morula 直接读 ${imp.skillDirs.map((d) => `<code>${esc(d)}</code>`).join("、")}</span></span><span class="tag">已共用</span></div></div>` : "";
  body.innerHTML = done +
    (o.state === "failed" ? `<div class="card list"><div class="row"><div class="pref-l"><div class="name">上次整理失败</div><div class="sub err-t">${esc(o.error)}</div></div>${status("failed")}</div></div>` : "") +
    cards + skill +
    `<ul class="imp-notes faint">
      <li>由${esc(imp.assistant || "助理")}整理，只读取每个对话的标题、开头和所在文件夹。${imp.assistant ? "" : "团队里没有助理，现在整理不了。"}</li>
      <li>你勾选后才建项目；原来的对话不会被改动。</li>
      <li>整理过的对话不再出现；没勾选的下次还会出现。</li>
    </ul>`;
}
function renderProposals(o) {
  const title = {};
  for (const s of o.sessions) title[s.harness + "/" + s.id] = s;
  const names = Object.fromEntries((projectList || []).map((p) => [p.id, p.name]));
  const rows = o.proposals.map((p, i) => {
    const ss = p.sessions.map((k) => title[k]).filter(Boolean);
    return `<div class="card list imp-p" data-i="${i}">
      <div class="row pref"><label class="acts"><input type="checkbox" class="imp-on" checked>
        ${p.existing ? `<span class="name">${esc(names[p.existing] || p.name)}</span><span class="tag accent">并入已有项目</span>` : `<input class="imp-name" value="${esc(p.name)}" spellcheck="false">`}</label>
        <span class="grow"></span><span class="tag">${ss.length} 个对话</span></div>
      ${p.existing ? "" : `<div class="row pref"><div class="pref-l"><div class="name">项目摘要</div></div><input class="imp-sum wide" value="${esc(p.summary)}"></div>
      <div class="row pref"><div class="pref-l"><div class="name">项目文件夹</div><div class="sub">留空时在项目存放位置下新建</div></div><input class="imp-dir mono wide" value="${esc(p.dir)}" spellcheck="false"></div>`}
      <details class="imp-more"><summary class="muted">项目记忆和对话</summary><div class="body">
        <div class="pf-text">${esc(p.memory) || '<span class="faint">（空）</span>'}</div>
        ${ss.map((s) => `<div class="sub clip" title="${esc(s.first)}"><span class="tag">${esc(hzName(s.harness))}</span> ${esc(s.updated.slice(0, 10))} · ${esc(s.title)} <span class="faint mono">${esc(s.dir)}</span></div>`).join("")}
      </div></details></div>`;
  }).join("");
  const chats = o.chats.map((k) => title[k]).filter(Boolean);
  $("#imp-body").innerHTML = (rows || `<div class="card list"><div class="row empty">助理没有归出项目</div></div>`) +
    `<div class="card list"><details class="imp-more"><summary class="muted">${chats.length} 个对话是闲聊或一次性提问，不建项目；确认后下次不再出现</summary><div class="body">
      ${chats.map((s) => `<div class="sub clip"><span class="tag">${esc(hzName(s.harness))}</span> ${esc(s.title)}</div>`).join("")}</div></details>
    <div class="row acts"><span class="form-err" id="imp-form-err"></span><span class="grow"></span>
      <button class="btn" id="imp-all">全不选</button><button class="btn" id="imp-discard">放弃这次结果</button><button class="btn primary" id="imp-apply">建勾选的项目</button></div></div>`;
}
$("#imp-rescan").addEventListener("click", () => loadImport(true));
$("#imp-body").addEventListener("click", async (e) => {
  const b = e.target.closest("button");
  if (!b) return;
  if (b.dataset.org) {
    b.disabled = true;
    impDone = null;
    try { await api("/api/onboard/start", { method: "POST", body: JSON.stringify({ only: [b.dataset.org] }) }); } catch (err) { $("#imp-err").textContent = err.message; }
    loadImport();
  }
  if (b.id === "imp-all") {
    const on = b.textContent === "全选";
    document.querySelectorAll(".imp-on").forEach((c) => (c.checked = on));
    b.textContent = on ? "全不选" : "全选";
  }
  if (b.id === "imp-discard") {
    await api("/api/onboard/discard", { method: "POST" }).catch(() => {});
    impDone = null;
    loadImport();
  }
  if (b.id === "imp-apply") {
    const items = [...document.querySelectorAll(".imp-p")].filter((c) => $(".imp-on", c).checked).map((c) => {
      const p = { ...imp.organize.proposals[+c.dataset.i] };
      if (!p.existing) Object.assign(p, { name: $(".imp-name", c).value.trim(), summary: $(".imp-sum", c).value.trim(), dir: $(".imp-dir", c).value.trim() });
      return p;
    });
    b.disabled = true;
    try {
      const r = await api("/api/onboard/apply", { method: "POST", body: JSON.stringify({ items }) });
      const bad = items.map((p, i) => r.errors[i] ? `「${esc(p.name)}」没建成：${esc(r.errors[i])}` : "").filter(Boolean);
      impDone = `已建或并入 ${items.length - bad.length} 个项目。` + (bad.length ? "<br>" + bad.join("<br>") : "");
      projectList = await api("/api/projects");
      footer();
    } catch (err) { $("#imp-form-err").textContent = err.message; b.disabled = false; return; }
    loadImport();
  }
});

/* ---------- 按项目和 skill 配置子 agent ---------- */
// 整理建好项目后、或某个项目的任务总落到通用执行者时，助理在对话里问；也可以从 Agent 页进来。
let stf = null, stfTimer = 0, stfDone = null;
async function loadStaff() {
  clearTimeout(stfTimer);
  try {
    [stf] = await Promise.all([api("/api/staff"), loadHarnesses()]);
    projectList = await api("/api/projects").catch(() => projectList);
    skillList = stf.skills;
    $("#staff-err").textContent = "";
  } catch (err) { $("#staff-err").textContent = err.message; return; }
  renderStaff();
  if (stf.staff.state === "working" && view === "staff") stfTimer = setTimeout(loadStaff, 3000);
}
function renderStaff() {
  const s = stf.staff, body = $("#staff-body");
  $("#staff-st").textContent = s.state === "working" ? "提议中…" : "";
  const done = stfDone ? `<div class="note sys">${stfDone}</div>` : "";
  if (s.state === "ready") return renderStaffProposals(s);
  if (s.state === "working") {
    const min = Math.max(0, Math.round((Date.now() - Date.parse(s.started)) / 60000));
    body.innerHTML = done + `<div class="card list"><div class="row"><div class="pref-l"><div class="name">助理在提议 agent</div>
      <div class="sub">${esc(s.model)} · 已用 ${min} 分钟。${s.why ? esc(s.why) + "。" : ""}可以先去别的页面，提议好后回到这里勾选。</div></div>${status("working")}</div></div>`;
    return;
  }
  const as = stf.assistant;
  body.innerHTML = done + `<div class="card list">
    ${s.state === "failed" ? `<div class="row"><div class="pref-l"><div class="name">上次提议失败</div><div class="sub err-t">${esc(s.error)}</div></div>${status("failed")}</div>` : ""}
    <div class="row"><div class="pref-l"><div class="name">依据</div><div class="sub">${projectList.length} 个项目，本机 ${stf.skills.length} 个 skill</div></div></div>
    <div class="row"><div class="pref-l"><div class="name">由谁提议</div><div class="sub">${as ? esc(as.runtime + " · " + as.model) : "团队里没有助理"}</div></div></div>
    <div class="row acts"><span class="muted">${projectList.length ? "" : "还没有项目。先整理以前的对话或新建项目。"}</span><span class="grow"></span>
      <button class="btn primary" id="staff-start" ${projectList.length && as ? "" : "disabled"}>让助理提议</button></div></div>
    <div class="card list"><details class="imp-more"><summary class="muted">本机的 ${stf.skills.length} 个 skill</summary><div class="body">
      ${stf.skills.map((k) => `<div class="sub clip" title="${esc(k.description)}"><span class="name">${esc(k.name)}</span> <span class="faint mono">${esc(k.source)}</span> · ${esc(k.description)}</div>`).join("")}</div></details></div>`;
}
function staffModels(sel) {
  const as = stf.assistant || {};
  const opts = [`<option value="">和助理一样（${esc((as.runtime || "") + " · " + (as.model || ""))}）</option>`];
  for (const h of harnesses) if (h.ready) for (const m of h.models) opts.push(`<option value="${esc(h.name + "\t" + m.id)}">${esc(h.name + " · " + (m.name || m.id))}</option>`);
  return opts.join("");
}
function renderStaffProposals(s) {
  const pn = Object.fromEntries(projectList.map((p) => [p.id, p.name]));
  const pl = (ids) => (ids || []).map((id) => pn[id] || "#" + id).join("、") || "无";
  const rows = s.proposals.map((p, i) => `<div class="card list imp-p st-p" data-i="${i}">
      <div class="row pref"><label class="acts"><input type="checkbox" class="st-on" checked>
        <span class="tag ${p.action === "new" ? "accent" : "warn"}">${p.action === "new" ? "新建" : "修改"}</span>
        <span class="name">${esc(p.name)}</span><span class="tag">${esc(p.role)}</span></label></div>
      <div class="row pref"><div class="pref-l"><div class="name">为什么</div></div><div class="sub" style="max-width:min(420px,50%)">${esc(p.reason)}</div></div>
      <div class="row pref"><div class="pref-l"><div class="name">职责说明</div></div><textarea class="st-ins">${esc(p.instructions)}</textarea></div>
      <div class="row pref"><div class="pref-l"><div class="name">skill</div></div><div class="chips st-sk"></div></div>
      <div class="row pref"><div class="pref-l"><div class="name">常用于</div></div><div class="chips st-pj"></div></div>
      ${p.action === "new" ? `<div class="row pref"><div class="pref-l"><div class="name">模型</div></div><select class="st-model">${staffModels()}</select></div>` : ""}
      ${p.old ? `<details class="imp-more"><summary class="muted">改之前</summary><div class="body">
        <div class="sub old">职责说明：${esc(p.old.instructions) || "（空）"}</div>
        <div class="sub">skill：${esc((p.old.skills || []).join("、") || "无")}</div>
        <div class="sub">常用于：${esc(pl(p.old.projects))}</div></div></details>` : ""}
    </div>`).join("");
  $("#staff-body").innerHTML = (s.why ? `<div class="note sys">${esc(s.why)}</div>` : "") +
    (rows || `<div class="card list"><div class="row empty">助理没有提议</div></div>`) +
    `<div class="card list"><div class="row acts"><span class="form-err" id="staff-form-err"></span><span class="grow"></span>
      <button class="btn" id="staff-discard">放弃这次提议</button><button class="btn primary" id="staff-apply">采用勾选的</button></div></div>`;
  document.querySelectorAll(".st-p").forEach((c) => {
    const p = s.proposals[+c.dataset.i];
    const sk = [...stf.skills.map((x) => ({ v: x.name, l: x.name, t: x.description }))];
    chips($(".st-sk", c), sk, p.skills, "本机没有 skill", true);
    chips($(".st-pj", c), projectList.map((x) => ({ v: x.id, l: x.name, t: x.summary })), p.projects, "还没有项目");
  });
}
$("#staff-body").addEventListener("click", async (e) => {
  const b = e.target.closest("button");
  if (!b) return;
  if (b.id === "staff-start") {
    b.disabled = true;
    stfDone = null;
    try { await api("/api/staff/start", { method: "POST" }); } catch (err) { $("#staff-err").textContent = err.message; }
    loadStaff();
  }
  if (b.id === "staff-discard") {
    await api("/api/staff/discard", { method: "POST" }).catch(() => {});
    stfDone = null;
    loadStaff();
  }
  if (b.id === "staff-apply") {
    const items = [...document.querySelectorAll(".st-p")].filter((c) => $(".st-on", c).checked).map((c) => {
      const p = { ...stf.staff.proposals[+c.dataset.i] };
      p.instructions = $(".st-ins", c).value.trim();
      p.skills = chipVals($(".st-sk", c));
      p.projects = chipVals($(".st-pj", c)).map(Number);
      const m = $(".st-model", c)?.value;
      if (m) {
        const [runtime, model] = m.split("\t");
        Object.assign(p, { runtime, model, effort: "" });
      }
      delete p.old;
      return p;
    });
    b.disabled = true;
    try {
      const r = await api("/api/staff/apply", { method: "POST", body: JSON.stringify({ items }) });
      const bad = items.map((p, i) => r.errors[i] ? `「${esc(p.name)}」没改成：${esc(r.errors[i])}` : "").filter(Boolean);
      stfDone = `已采用 ${items.length - bad.length} 项。` + (bad.length ? "<br>" + bad.join("<br>") : "");
    } catch (err) { $("#staff-form-err").textContent = err.message; b.disabled = false; return; }
    loadStaff();
  }
});

/* ---------- 订阅 ---------- */
function ago(iso, future) {
  const t = Date.parse(iso);
  if (!t) return "";
  let m = Math.round(Math.abs(t - Date.now()) / 60000);
  const s = m < 1 ? "不到 1 分钟" : m < 60 ? `${m} 分钟` : m < 1440 ? `${Math.floor(m / 60)} 小时 ${m % 60} 分` : `${Math.floor(m / 1440)} 天 ${Math.floor((m % 1440) / 60)} 小时`;
  return future ? s + "后重置" : s + "前";
}
const WIN = { "5 hours": "5 小时", "7 days": "每周", "7 天": "每周", Credits: "积分", "Add-on credits": "加购积分", "Shared credits": "共享积分", "Dedicated credits": "专属积分" };
const SUBSRC = { codex: "codex 记录", plugin: "插件" };
// pace 是节奏：已用的比例减去窗口已过去的比例，单位是百分点。不知道窗口长度、或已过重置时间时为 null。
function pace(w) {
  const reset = Date.parse(w.resetsAt || ""), span = (w.minutes || 0) * 60000;
  if (!reset || !span || reset <= Date.now()) return null;
  const gone = Math.min(1, Math.max(0, 1 - (reset - Date.now()) / span)) * 100;
  return { gone, d: Math.round(w.used - gone) };
}
// 订阅卡片可以看剩余或已用，选择记在本机。
let subsMode = store.get("subsMode") === "used" ? "used" : "left", subsData = null;
const num = (n) => Number(n).toLocaleString("zh-CN", { maximumFractionDigits: 2 });
const day = (iso) => { const t = new Date(iso); return isNaN(t) ? "" : `${t.getMonth() + 1} 月 ${t.getDate()} 日`; };
function quotaWin(w) {
  const used = Math.round(w.used), left = Math.max(0, 100 - used), lvl = used >= 85 ? "hi" : used >= 60 ? "mid" : "", p = pace(w);
  const show = subsMode === "used" ? used : left;
  const reset = Date.parse(w.resetsAt || "");
  const when = !reset ? "" : reset <= Date.now() ? "已过重置时间" : ago(w.resetsAt, true);
  const pc = p ? `<span class="qt-pace${p.d > 0 ? " ahead" : ""}" title="已用 ${used}%，这段时间已过去 ${Math.round(p.gone)}%。正数表示用得比平均快">节奏 ${p.d > 0 ? "+" : p.d < 0 ? "−" : ""}${Math.abs(p.d)}%</span>` : "";
  const unit = w.unit ? " " + esc(w.unit) : "";
  const amt = w.limit ? `${subsMode === "used" ? num(Math.max(0, w.limit - (w.amount || 0))) : num(w.amount || 0)} / ${num(w.limit)}${unit}` : w.display ? esc(w.display) : "";
  const runs = w.runsOut && w.runsOutAt ? `<div class="qt-ln qt-exp"><span>${num(w.runsOut)}${unit} 于 ${esc(day(w.runsOutAt))}到期</span></div>` : "";
  return `<div class="qt-w">
    <div class="qt-top"><span class="qt-n">${esc(WIN[w.name] || w.name)}</span><span class="grow"></span><b>${show}%</b><span class="muted">${subsMode === "used" ? "已用" : "剩余"}</span></div>
    <div class="qt-bar"><i class="${lvl}" style="width:${Math.min(100, subsMode === "used" ? used : left)}%"></i>${p && subsMode === "used" ? `<s style="left:${p.gone.toFixed(1)}%" title="匀速用到现在的位置"></s>` : ""}</div>
    ${when || pc || amt ? `<div class="qt-ln">${[amt, esc(when)].filter(Boolean).map((x) => `<span>${x}</span>`).join('<span class="faint">·</span>')}<span class="grow"></span>${pc}</div>` : ""}
    ${runs}
  </div>`;
}
// 详情里的额度：一个窗口一张卡片；账号不止一个时每个账号前写名字。
// stale：额度记下的时间超过 10 分钟，琥珀色写明截至什么时候。codex 的额度来自最近一次会话，morula 不主动刷新。
function acctWins(a, i, all) {
  const t = Date.parse(a.readAt || ""), stale = t && Date.now() - t > 10 * 60000;
  return `${all.length > 1 ? `<div class="qt-ah">${esc(a.user || "账号")}${a.plan ? `<span class="tag">${esc(a.plan)}</span>` : ""}</div>` : ""}
    ${a.balance ? `<div class="qt-bal">${esc(a.balance)}</div>` : ""}
    ${a.until && day(a.until) ? `<div class="qt-bal">套餐到 ${esc(day(a.until))}</div>` : ""}
    ${a.error ? `<div class="qt-err err-t">${esc(a.error)}</div>` : ""}
    ${(a.windows || []).length ? `<div class="ac-wins">${a.windows.map((w) => `<div class="ac-win">${quotaWin(w)}</div>`).join("")}</div>` : a.error ? "" : '<div class="qt-none muted">没有额度数据</div>'}
    ${stale ? `<div class="qt-stale">截至 ${esc(ago(a.readAt))}的数据${a.source === "codex" ? "，来自最近一次使用 codex，这次没有刷新" : "，这次没有刷新"}</div>` : ""}`;
}
function renderSubs() {
  const r = subsData || {};
  $("#subs-at").textContent = r.readAt ? ago(r.readAt) + "读取" : "";
  $("#subs-mode").querySelectorAll("button").forEach((b) => b.classList.toggle("on", b.dataset.mode === subsMode));
  renderProviders();
}
async function loadSubs(refresh) {
  $("#subs-at").textContent = "读取额度…";
  try {
    subsData = await api("/api/accounts" + (refresh ? "?refresh=1" : ""));
  } catch (err) {
    subsData = { accounts: [], error: err.message };
  }
  renderSubs();
}
$("#subs-mode").addEventListener("click", (e) => {
  const b = e.target.closest("[data-mode]");
  if (!b) return;
  subsMode = b.dataset.mode;
  store.set("subsMode", subsMode);
  renderSubs();
});

/* ---------- 插件 ---------- */
let plug = { plugins: [], providers: [] };
const signing = {}; // provider → 正在进行的登录：{ method, mode, session, url, instructions, err, busy }
const AUTH = { oauth: "已登录", api: "API 密钥" };

const plugSt = () => $(pane === "providers" ? "#prov-st" : "#plug-st");
async function loadPlugins(refresh) {
  plugSt().textContent = "读取中…";
  try {
    setPlug(await api("/api/plugins" + (refresh ? "?refresh=1" : "")));
  } catch (err) {
    setPlug({ plugins: [], providers: [], error: err.message });
  }
}
function setPlug(r) {
  plug = r;
  harnesses = []; // 模型列表变了，agent 表单下次重读
  $("#plug-st").textContent = $("#prov-st").textContent = "";
  $("#plug-err").textContent = $("#prov-err").textContent = [r.bun, r.error].filter(Boolean).join("\n");
  renderMine();
  renderPkgs();
  renderProviders();
}
// 进插件页：上次看哪个子页就打开哪个；第一次进时没装插件开「发现」，装了开「已安装」。
async function openPlugins() {
  await loadPlugins();
  setPlugTab(plugTab === "find" || plugTab === "mine" ? plugTab : (plug.plugins || []).length ? "mine" : "find");
}
// 插件页两个子页：「发现」在 npm 上按关键字搜，「已安装」一行一个插件。没填关键字时只列不带 @scope/ 的包，见 plugin.Search。
let plugTab = store.get("plugTab"), pkgs = null, pkgErr = "", updates = {}; // updates：包名 → 新版本
const installing = new Set();
function setPlugTab(t) {
  plugTab = t;
  store.set("plugTab", t);
  $("#plug-find").hidden = t !== "find";
  $("#plug-mine").hidden = t !== "mine";
  $("#plug-tabs").querySelectorAll("button").forEach((b) => b.classList.toggle("on", b.dataset.tab === t));
  if (t === "find" && !pkgs && !pkgErr) searchPkgs("");
  if (t === "find") loadPicks();
}
// picks.json：精选订阅的包和介绍，以及按包名配的图标。插件页、已安装列表、账号页都用这份图标表。
let picks = null, pkgIcons = {}, picksLoad = null;
function loadPicks() {
  return picksLoad ||= fetch("picks.json").then((r) => r.json()).then((j) => {
    picks = j.picks || [];
    pkgIcons = { ...(j.icons || {}) };
    for (const p of picks) if (p.icon) pkgIcons[p.name] = p.icon;
  }, () => { picks = []; }).then(() => { renderPkgs(); renderMine(); renderProviders(); });
}
// pkgIcon 用 icons/ 下的图标；包名在图标表里查不到时退回 tileIcon。单色图标用 mask 按文字颜色画，跟着主题变。
function pkgIcon(pkg, label) {
  if (!picksLoad) loadPicks();
  return pkgIcons[pkg] ? iconFile(pkgIcons[pkg]) : tileIcon(label);
}
// cleanDesc 去掉 npm 说明里的 markdown 图片、链接地址和标记符号，剩不下字时返回空。
function cleanDesc(s) {
  s = String(s || "").replace(/!\[[^\]]*\]\([^)]*\)/g, " ").replace(/\[([^\]]*)\]\([^)]*\)/g, "$1")
    .replace(/https?:\/\/\S+/g, " ").replace(/[#*`>]+/g, " ").replace(/\s+/g, " ").trim().replace(/[:：]$/, "");
  return /[\p{L}\p{N}]{2}/u.test(s) ? s : "";
}
$("#plug-tabs").addEventListener("click", (e) => { const b = e.target.closest("[data-tab]"); if (b) setPlugTab(b.dataset.tab); });
const fmtN = (n) => n >= 1e4 ? `${+(n / 1e4).toFixed(1)} 万` : String(n);
async function searchPkgs(q) {
  pkgs = null; pkgErr = "";
  $("#plug-q-st").textContent = "正在搜索…";
  renderPkgs();
  try {
    const r = await api("/api/plugins/search?q=" + encodeURIComponent(q));
    pkgs = r.packages;
    $("#plug-q-st").textContent = `${q ? `「${q}」` : "全部"}：${pkgs.length} 个，按热门排`;
  } catch (err) {
    pkgErr = err.message;
    $("#plug-q-st").textContent = "";
  }
  renderPkgs();
}
function renderPkgs() {
  renderPicks();
  const box = $("#plug-pkgs");
  if (pkgErr) { box.innerHTML = `<div class="card row empty err-t">${esc(pkgErr)}</div>`; return; }
  if (!pkgs) { box.innerHTML = '<div class="card row empty">搜索中…</div>'; return; }
  const mine = new Set((plug.plugins || []).map((p) => p.name));
  box.innerHTML = pkgs.map((k) => {
    const has = mine.has(k.name), busy = installing.has(k.name);
    let d;
    return `<div class="card pl-card pk-card">
      <div class="tile-hd">${pkgIcon(k.name, k.name.replace(/^@[^/]+\//, "").replace(/^opencode-/, ""))}
        <div class="tile-t"><div class="acts"><span class="name clip" title="${esc(k.name)}">${esc(k.name)}</span></div>
          <div class="sub clip">${[k.publisher ? esc(k.publisher) : "", k.updated ? esc(ago(k.updated)) + "更新" : ""].filter(Boolean).join(" · ")}</div></div></div>
      <div class="tile-d pk-d">${(d = (picks || []).find((x) => x.name === k.name)?.desc || cleanDesc(k.description)) ? esc(d) : '<span class="faint">没有写说明</span>'}</div>
      <div class="tile-ft"><span class="pk-m" title="最近 7 天下载量">↓ ${fmtN(k.weekly)}/周</span><span class="tag mono">${esc(k.version)}</span>
        ${k.homepage ? `<span class="flink" data-open="${esc(k.homepage)}">主页</span>` : ""}<span class="grow"></span>
        ${has ? '<span class="state"><i class="dot done"></i>已安装</span>' : `<button class="btn line" data-pkg-add="${esc(k.name)}"${busy ? " disabled" : ""}>${busy ? "安装中…" : "安装"}</button>`}</div>
    </div>`;
  }).join("") || '<div class="card row empty">没有搜到插件。换个关键字试试。</div>';
}
// 精选订阅的卡片：订阅名当标题，包名写在下面；搜索结果里有这个包时带上下载量和说明。
function renderPicks() {
  const mine = new Set((plug.plugins || []).map((p) => p.name));
  $("#plug-picks").innerHTML = (picks || []).map((p) => {
    const k = (pkgs || []).find((x) => x.name === p.name), has = mine.has(p.name), busy = installing.has(p.name);
    let d;
    return `<div class="card pl-card pk-card">
      <div class="tile-hd">${pkgIcon(p.name, p.sub)}
        <div class="tile-t"><span class="name clip">${esc(p.sub)}</span><div class="sub clip mono" title="${esc(p.name)}">${esc(p.name)}</div></div></div>
      ${(d = p.desc || cleanDesc(k?.description)) ? `<div class="tile-d pk-d" title="${esc(d)}">${esc(d)}</div>` : ""}
      <div class="tile-ft">${k ? `<span class="pk-m" title="最近 7 天下载量">↓ ${fmtN(k.weekly)}/周</span>` : ""}<span class="tag warn" title="未经逐一验证">未验证</span>
        <span class="flink" data-open="https://www.npmjs.com/package/${esc(p.name)}">插件主页</span><span class="grow"></span>
        ${has ? '<span class="state"><i class="dot done"></i>已安装</span>' : `<button class="btn line" data-pkg-add="${esc(p.name)}"${busy ? " disabled" : ""}>${busy ? "安装中…" : "安装"}</button>`}</div>
    </div>`;
  }).join("") || '<div class="card row empty">读取中…</div>';
}
$("#plug-search").addEventListener("submit", (e) => { e.preventDefault(); searchPkgs(e.target.q.value.trim()); });
async function installPkg(spec, name) {
  installing.add(name);
  renderPkgs();
  $("#plug-err").textContent = "";
  try {
    setPlug(await api("/api/plugins", { method: "POST", body: JSON.stringify({ spec }) }));
    delete updates[name];
    renderMine();
  } catch (err) {
    $("#plug-err").textContent = `装 ${spec} 没成功：${err.message}`;
  } finally {
    installing.delete(name);
    renderPkgs();
    renderMine();
  }
}
// 已安装：一行一个插件。说明来自包里的 package.json（plugin.Meta），提供的订阅来自插件运行器（Provider.Spec）。
function renderMine() {
  const ps = plug.plugins || [];
  $("#plugins").innerHTML = ps.map(plugRow).join("") || '<div class="row empty">还没有装插件。到「发现」搜一个装上。</div>';
  $("#plug-bun").textContent = "";
}
function plugRow(p) {
  const info = p.info || {}, title = info.title || p.name, nv = updates[p.name], busy = installing.has(p.name);
  const provs = (plug.providers || []).filter((x) => !x.builtin && (x.spec === p.spec || x.npm === p.name));
  const st = p.off ? ["idle", "已停用"] : p.error ? ["failed", "加载失败"] : ["done", "已启用"];
  const out = provs.some((x) => !x.signedIn);
  return `<div class="row pl-row${p.off ? " off" : ""}">${pkgIcon(p.name, title)}
    <div class="pref-l">
      <div class="acts"><span class="name clip">${esc(title)}</span>${info.version ? `<span class="tag mono">${esc(info.version)}</span>` : ""}${nv ? `<span class="tag accent">有新版本 ${esc(nv)}</span>` : ""}</div>
      <div class="sub clip" title="${esc(info.description || p.spec)}">${esc(info.description || p.spec)}</div>
      ${provs.length ? `<div class="pl-prov">${provs.map((x) => `<button class="chip pl-sub" type="button" data-go-prov="${esc(x.id)}" title="到「账号」${x.signedIn ? "查看" : "登录"}"><i class="dot ${x.signedIn ? "done" : "idle"}"></i>${esc(x.name || x.id)} · ${x.signedIn ? "已登录" : "未登录"}</button>`).join("")}</div>` : ""}
      ${info.notice ? `<div class="pl-notice">${esc(info.notice)}</div>` : ""}
      ${p.error ? `<div class="err-t pl-err" title="${esc(p.error)}">${esc(p.error)}</div>` : ""}
    </div>
    <span class="state"><i class="dot ${st[0]}"></i>${st[1]}</span>
    <span class="acts">${info.homepage ? `<span class="flink" data-open="${esc(info.homepage)}">主页</span>` : ""}
      ${out && !p.off ? `<button class="btn line" data-go-prov="1">登录</button>` : ""}
      ${nv ? `<button class="btn line" data-pkg-up="${esc(p.name)}"${busy ? " disabled" : ""}>${busy ? "更新中…" : "更新"}</button>` : ""}
      <button class="btn" data-plug-off="${esc(p.name)}" data-off="${p.off ? 0 : 1}">${p.off ? "启用" : "停用"}</button>
      <button class="btn danger" data-plug-rm="${esc(p.name)}">移除</button></span>
  </div>`;
}
$("#plug-upd").addEventListener("click", async (e) => {
  const b = e.currentTarget;
  b.disabled = true;
  $("#plug-upd-st").textContent = "正在检查新版本…";
  try {
    const r = await api("/api/plugins/updates");
    updates = r.updates || {};
    const n = Object.keys(updates).length, bad = Object.keys(r.errors || {});
    $("#plug-upd-st").textContent = (n ? `${n} 个插件有新版本` : "都是最新版本") + (bad.length ? `；${bad.join("、")} 查不到` : "");
  } catch (err) {
    $("#plug-upd-st").textContent = "检查更新失败：" + err.message;
  } finally {
    b.disabled = false;
    renderMine();
  }
});
// 账号：codex 订阅排第一行，后面是插件订阅和 API key。每行右边是额度的前两个窗口；点开是详情：额度、模型勾选、测试模型。
const openSrc = new Set(), srcTest = {}; // srcTest：来源 → { model, busy, ok, text, error, ms }
function codexProv() {
  const c = plug.codex;
  if (!c) return null;
  return { id: "codex", name: "codex", codex: true, installed: c.installed, signedIn: c.loggedIn, logging: c.logging, url: c.url, err: c.error,
    plan: c.plan, authType: "oauth", models: c.models || [] };
}
const fmtCtx = (n) => !n ? "" : n >= 1e6 ? `${+(n / 1e6).toFixed(1)}M` : `${Math.round(n / 1e3)}K`;
// acctOf 是这个来源在额度数据里的账号。插件账号按 provider id 对，老数据没有 id 时按名字对。
function acctOf(p) {
  const as = subsData?.accounts || [];
  return p.codex ? as.filter((a) => a.source === "codex") : as.filter((a) => a.source === "plugin" && (a.provider ? a.provider === p.id : a.agent === (p.name || p.id)));
}
// 一行右边的额度：前两个窗口各一条细条。
function acctQuota(p) {
  if (p.builtin) return '<span class="ac-q"><span class="faint">按量计费，没有额度可读</span></span>';
  if (!p.signedIn) return '<span class="ac-q"></span>';
  const a = acctOf(p)[0];
  if (!a) return `<span class="ac-q"><span class="faint">${subsData ? (p.codex ? "还没有用过 codex" : "这个插件不提供额度") : "读取额度…"}</span></span>`;
  const ws = (a.windows || []).slice(0, 2);
  if (!ws.length) return `<span class="ac-q"><span class="faint clip" title="${esc(a.error || "")}">${a.error ? "额度读不到" : "没有额度数据"}</span></span>`;
  return `<span class="ac-q">${ws.map((w) => {
    const used = Math.round(w.used), v = subsMode === "used" ? used : Math.max(0, 100 - used);
    return `<span class="ac-qr"><span class="k">${esc(WIN[w.name] || w.name)}</span><span class="ac-qb"><i class="${used >= 85 ? "hi" : ""}" style="width:${Math.min(100, v)}%"></i></span><b>${v}%</b></span>`;
  }).join("")}</span>`;
}
// usedBy 是用这个来源的某个模型的 agent 名字。codex 订阅的模型名不带前缀；插件和 API key 的模型在 codex 里写作 来源/模型，在 opencode、pi 里写作 morula/来源/模型。
function usedBy(p, m) {
  const ids = p.codex ? [m.id] : [`${p.id}/${m.id}`, `morula/${p.id}/${m.id}`];
  return agentList.filter((a) => (p.codex ? a.runtime === "codex" : true) && ids.includes(a.model)).map((a) => a.name);
}
const whoText = (ns) => !ns.length ? "" : ns.length === 1 ? `${ns[0]}在用` : `${ns[0]}等 ${ns.length} 个在用`;
function renderProviders() {
  const list = [codexProv(), ...(plug.providers || [])].filter(Boolean);
  $("#providers").innerHTML = list.map((p) => {
    const s = signing[p.id], open = openSrc.has(p.id);
    const all = p.models || [], off = (plug.off || {})[p.id] || [], on = all.filter((m) => !off.includes(m.id)).length;
    const count = off.length ? `已选 ${on} / ${all.length} 个模型` : `${all.length} 个模型`;
    const host = p.builtin ? String(p.api || "").replace(/^https?:\/\//, "").split("/")[0] : "";
    const sub = p.codex && !p.installed ? "这台 Mac 上没装 codex"
      : p.codex ? (p.signedIn ? `只给 codex 用 · ${count}` : "登录后可以用 codex 自己的模型")
      : p.builtin ? [host, count].filter(Boolean).join(" · ")
      : p.signedIn ? [p.account, `可用于 codex、opencode、pi · ${count}`].filter(Boolean).join(" · ") : `${all.length} 个模型 · 登录后可用`;
    const methods = p.methods || [];
    const acts = p.signedIn || (p.codex && !p.installed) ? ""
      : p.codex ? `<button class="btn line" data-codex-login${p.logging ? " disabled" : ""}>${p.logging ? "登录中…" : "登录"}</button>`
      : s ? `<button class="btn" data-sign-cancel="${esc(p.id)}">取消</button>`
      : methods.map((m, i) => `<button class="btn line" data-signin="${esc(p.id)}" data-method="${i}">${esc(zhAuth(m.label) || (m.type === "api" ? "填 API 密钥" : "登录"))}</button>`).join("");
    const state = p.builtin ? ["done", "可用"] : p.signedIn ? ["done", "已登录"] : ["idle", "未登录"];
    const kind = p.codex ? "ChatGPT 订阅" : p.builtin ? "API 密钥" : "插件";
    let html = `<div class="row src${open ? " open" : ""}" data-src="${esc(p.id)}">
      <span class="chev">▶</span>${p.codex ? harnessIcon("codex") : pkgIcon(p.npm, p.name || p.id)}
      <div class="pref-l"><div class="acts"><span class="name clip" title="${esc(p.name || p.id)}">${esc(p.name || p.id)}</span><span class="tag">${kind}</span>${p.plan ? `<span class="tag">${esc(p.plan)}</span>` : ""}</div><div class="sub clip">${esc(sub)}</div></div>
      ${acts ? `<span class="acts src-go${methods.length > 1 && !s ? " many" : ""}">${acts}</span>` : acctQuota(p)}
      <span class="state"><i class="dot ${state[0]}"></i>${state[1]}</span></div>`;
    if (p.codex && p.logging && p.url) html += `<div class="row sign"><div class="pref-l"><div class="sub clip">已在浏览器打开登录页，登录完成后这里会自动更新。没打开时手动访问：<span class="mono sel">${esc(p.url)}</span></div></div></div>`;
    if (p.codex && p.err) html += `<div class="row sign"><div class="form-err">${esc(p.err)}</div></div>`;
    if (s && !p.signedIn) html += signRow(p, s);
    if (open) html += srcDetail(p, off);
    return html;
  }).join("") || `<div class="row empty">还没有账号。到「设置 / 插件」装一个插件，或点右上角「填 API 密钥」。</div>`;
}
function srcDetail(p, off) {
  const all = p.models || [], id = esc(p.id);
  const acct = p.codex ? "在 codex 里登录的账号"
    : p.builtin ? `服务地址 ${p.api || ""}`.trim() : p.account || "没有账号名";
  const acctActs = p.codex ? "" : p.builtin ? `<button class="btn danger sm" data-api-rm="${id}">删除</button>`
    : p.signedIn ? `<button class="btn sm" data-signout="${id}">退出</button>` : "";
  let html = `<div class="src-d">
    <div class="src-h">账号</div>
    <div class="src-acct"><i class="dot ${p.signedIn ? "done" : "idle"}"></i><span class="clip">${esc(p.signedIn ? acct : "未登录")}</span>${p.plan ? `<span class="tag">${esc(p.plan)}</span>` : ""}<span class="grow"></span>${acctActs}</div>`;
  const as = p.signedIn && !p.builtin ? acctOf(p) : [];
  if (as.length) html += `<div class="src-h">额度</div>${as.map(acctWins).join("")}`;
  if (p.signedIn && p.checkin) html += ckinRow(p);
  const free = all.filter((m) => m.free);
  html += `<div class="src-h">模型<span class="faint">已选 ${all.length - all.filter((m) => off.includes(m.id)).length} / ${all.length}</span><span class="grow"></span>
      ${all.length ? `<button class="btn sm" data-pick="all" data-for="${id}">全选</button><button class="btn sm" data-pick="none" data-for="${id}">全不选</button>${free.length ? `<button class="btn sm" data-pick="free" data-for="${id}">只选免费</button>` : ""}` : ""}</div>
    <div class="mpick">${all.map((m) => {
      const badges = [m.free ? '<span class="mb free">免费</span>' : "", m.rate ? `<span class="mb">${esc(+m.rate.toFixed(2))}×</span>` : "", m.context ? `<span class="mb">${fmtCtx(m.context)}</span>` : ""].join("");
      const who = whoText(usedBy(p, m));
      return `<label title="${esc(m.name || m.id)}"><input type="checkbox" data-mon="${esc(m.id)}" data-for="${id}"${off.includes(m.id) ? "" : " checked"}>${mName(m)}${badges}${who ? `<span class="mp-who">${esc(who)}</span>` : ""}</label>`;
    }).join("") || '<span class="faint">这个来源没有列出模型</span>'}</div>
    <div class="faint src-note">取消勾选的模型不出现在 agent 的模型列表里。已经在用它的 agent，开工时换成列表里的第一个。${p.codex ? "这里和「AI 工具 / codex / 订阅」勾的是同一份。" : ""}</div>`;
  const t = srcTest[p.id] || {}, ms = all.filter((m) => !off.includes(m.id));
  if (p.signedIn && ms.length) {
    html += `<div class="src-h">测试模型</div>
    <div class="src-test"><select data-test-model="${id}">${ms.map((m) => `<option value="${esc(m.id)}"${m.id === t.model ? " selected" : ""}>${esc(m.name || m.id)}</option>`).join("")}</select>
      <button class="btn" data-test="${id}"${t.busy ? " disabled" : ""}>${t.busy ? "测试中…" : "测试"}</button>
      <span class="${t.error ? "err-t" : "muted"} clip">${t.busy ? "会真实调用一次模型" : t.error ? esc(t.error) : t.ok ? `能用 · ${(t.ms / 1000).toFixed(1)} 秒 · 回复「${esc(t.text)}」` : "会真实调用一次模型"}</span></div>`;
  }
  return html + "</div>";
}
// ckinRow 是每日签到一节：开关、最近一次的结果、「现在签到」。打开后 morula 每天替用户领一次（plugin/checkin.go）。
function ckinRow(p) {
  const c = (plug.checkin || {})[p.id] || {}, id = esc(p.id);
  const last = !c.at ? "还没签过到" : c.outcome === "failed" ? `签到失败：${c.message || "原因不明"}`
    : c.outcome === "claimed" ? `${ago(c.at)}领到了${c.credit ? ` ${num(c.credit)} 积分` : "奖励"}`
    : c.outcome === "done" ? `${ago(c.at)}看过，今天已经领过` : `${ago(c.at)}看过，今天没有可领的`;
  const text = c.on ? last : `关着。打开后 morula 每天替你领一次签到奖励${c.at ? `。上次：${last}` : ""}`;
  return `<div class="src-h">每日签到</div>
    <div class="src-acct"><button type="button" class="rt-sw${c.on ? " on" : ""}" data-ckin="${id}" role="switch" aria-checked="${!!c.on}" title="${c.on ? "关掉每日签到" : "打开每日签到"}"></button>
      <span class="clip ${c.outcome === "failed" ? "err-t" : "muted"}" title="${esc(text)}">${esc(text)}</span><span class="grow"></span>
      <button class="btn sm" data-ckin-run="${id}">现在签到</button></div>`;
}
// mName 是模型勾选列表里的名字：有和 id 不同的名字时写名字，id 跟在后面（Qoder 的 id 是 dmodel 这类代号）。
const mName = (m) => m.name && m.name !== m.id ? `<span class="mp-n clip">${esc(m.name)}</span><span class="mono mp-id">${esc(m.id)}</span>` : `<span class="mp-n mono clip">${esc(m.id)}</span>`;
// zhAuth 把 OpenCode 插件常见的英文登录说明换成中文；认不出的原样显示。
function zhAuth(t) {
  const s = String(t || "");
  const rules = [
    [/^Sign in with (.+) Enterprise \(VPC\)$/, "$1 企业版（VPC）登录"],
    [/^Sign in with (.+) PAT$/, "用 $1 的访问令牌登录"],
    [/^Sign in with (.+?)(?: in (?:the )?browser)?$/, "用 $1 账号登录"],
    [/^Sign in to (.+) in the browser and authorize this device$/, "在浏览器里登录 $1，授权这台设备"],
    [/^Create a PAT at (\S+) if you don't have one$/, "还没有访问令牌的话，到 $1 生成一个"],
    [/^Your (.+) Personal Access Token \(from (\S+)\)$/, "$1 的个人访问令牌（在 $2 生成）"],
    [/^Your enterprise's VPC instance: .*$/, "企业的 VPC 实例：填名字或地址"],
  ];
  for (const [re, zh] of rules) if (re.test(s)) return s.replace(re, zh);
  return s;
}
function signRow(p, s) {
  const m = (p.methods || [])[s.method] || {};
  let body = "";
  if (s.mode === "prompts") {
    body = (m.prompts || []).map((q) => q.type === "select"
      ? `<label class="acts"><span class="muted">${esc(zhAuth(q.message))}</span><select data-prompt="${esc(q.key)}">${(q.options || []).map((o) => `<option value="${esc(o.value)}">${esc(o.label)}</option>`).join("")}</select></label>`
      : `<label class="acts"><span class="muted">${esc(zhAuth(q.message))}</span><input${q.type === "password" ? ' type="password"' : ""} data-prompt="${esc(q.key)}" placeholder="${esc(q.placeholder || "")}" spellcheck="false"></label>`).join("")
      + `<button class="btn primary" data-sign-go="${esc(p.id)}">继续</button>`;
  } else if (s.mode === "key") {
    body = `<input type="password" class="mono wide" data-sign-key placeholder="API 密钥" spellcheck="false"><button class="btn primary" data-sign-key-go="${esc(p.id)}">保存</button>`;
  } else if (s.mode === "code") {
    body = `<input class="mono wide" data-sign-code placeholder="粘贴授权码" spellcheck="false"><button class="btn primary" data-sign-code-go="${esc(p.id)}">完成登录</button>`;
  } else if (s.mode === "auto") {
    body = `<span class="muted">${s.url ? "已在浏览器打开登录页，登录完成后这里会自动更新。" : "正在登录…"}</span>`;
  } else {
    body = '<span class="muted">正在准备登录…</span>';
  }
  const link = s.url ? `<div class="sub clip">登录页没打开时手动访问：<span class="mono sel">${esc(s.url)}</span></div>` : "";
  return `<div class="row sign"><div class="pref-l">
    ${s.instructions ? `<div class="sub">${esc(zhAuth(s.instructions))}</div>` : ""}${link}
    <div class="acts">${body}</div>${s.err ? `<div class="form-err">${esc(s.err)}</div>` : ""}</div></div>`;
}
async function plugDo(btn, path, opts) {
  btn.disabled = true;
  plugSt().textContent = "处理中…";
  try {
    setPlug(await api(path, opts));
  } catch (err) {
    plugSt().textContent = "";
    $(pane === "providers" ? "#prov-err" : "#plug-err").textContent = err.message;
  } finally {
    btn.disabled = false;
  }
}
async function signIn(id, method, inputs) {
  const s = (signing[id] = { method, mode: "" });
  renderProviders();
  try {
    const r = await api("/api/plugins/signin", { method: "POST", body: JSON.stringify({ provider: id, method, inputs }) });
    if (signing[id] !== s) return;
    Object.assign(s, r);
    if (r.mode === "done") { delete signing[id]; return loadPlugins(true); }
    renderProviders();
    if (r.mode === "auto") await finish(id, s, {});
  } catch (err) {
    s.err = err.message;
    renderProviders();
  }
}
async function finish(id, s, body) {
  try {
    const r = await api("/api/plugins/signin/" + encodeURIComponent(s.session), { method: "POST", body: JSON.stringify(body) });
    if (signing[id] !== s) return;
    delete signing[id];
    setPlug(r);
  } catch (err) {
    if (signing[id] !== s) return;
    s.err = err.message;
    if (s.mode === "auto") s.mode = "failed";
    renderProviders();
  }
}

// 进账号页：账号列表、额度、agent 名单（模型旁写谁在用）一起读。刷新时额度也重读。
function loadAccounts(refresh) {
  if (!agentList.length) api("/api/agents").then((l) => { agentList = l; renderProviders(); }, () => {});
  return Promise.all([loadPlugins(refresh), loadSubs(refresh)]);
}
$("#prov-refresh").addEventListener("click", () => loadAccounts(true));
$("#prov-plug").addEventListener("click", () => { plugTab = "find"; openPrefs("plugins"); });
$("#prov-api").addEventListener("click", () => openApiKey());

/* ---------- 首次启动 ---------- */
// 四步：选 AI 工具 → 接入订阅 → 选模型并验证 → 整理以前的对话。
// 第一次打开（助理没上线或还没做过自我介绍，也没点过「以后再说」）时自动弹出，每次启动只弹一次。
let ready = null, readyTimer = 0, setupBack = false, setupShown = false;
let guideState = null;  // onboard.asked、onboard.intro、onboard.hidden
let obStep = 1, obTools = [], obToolTimer = 0, obSubs = {}, obSubTimer = 0;
let obModel = null, obVerify = null;            // 第 3 步选的模型（{rt, id}）和验证结果
let obSessions = null, obCost = null, obPick = new Set(), obErr = "";  // 第 4 步
async function checkReady() {
  clearTimeout(readyTimer);
  try {
    ready = await api("/api/ready");
  } catch { return; }
  guideState = await api("/api/onboard?asked").catch(() => guideState || { asked: true, intro: true, hidden: true });
  harnesses = await api("/api/harnesses").catch(() => harnesses);
  renderGuide();
  if (!setupShown && !guideState.hidden && !(ready.online && guideState.intro)) { setupShown = true; openSetup(); }
  if (!$("#setup").hidden && obStep === 3 && !obVerify?.running) renderOb();
}
// hzLine 是快速上手卡片上「本机检测到哪些 harness」的一句话。
function hzLine() {
  const ok = (harnesses || []).filter((h) => h.ready).map((h) => hzName(h.name));
  if (!harnesses) return "";
  return ok.length ? `本机检测到 ${esc(ok.join("、"))}。` : "本机没找到能用的 AI 工具，点「接入」可以直接安装。";
}
// 助理不能用时，助理页输入框上方留一行提示和「接入」按钮，点了打开接入弹窗。上线后不显示。
function renderGuide() {
  const r = ready, box = $("#qs");
  box.hidden = r.online;
  if (box.hidden) return;
  box.innerHTML = `<div class="qs-hd"><div class="pref-l"><div class="name">助理还不能用</div>
      <div class="sub">${r.reason ? esc(r.reason) + "。" : ""}选一个 AI 工具和模型给助理，就能开始。${hzLine()}</div></div>
      <button class="btn primary" type="button" data-qs="setup">接入</button></div>`;
}
$("#qs").addEventListener("click", (e) => { if (e.target.closest("[data-qs=setup]")) openSetup(); });
// 打开弹窗时后台扫本机工具的历史对话，到第 4 步时已经扫完。
function openSetup(n) {
  $("#setup").hidden = false;
  obSessions = null;
  api("/api/onboard?rescan").catch(() => {});
  obGo(n || 1);
}
function obGo(n) {
  obStep = n;
  obErr = "";
  document.querySelectorAll("#guide-steps li").forEach((li, i) => {
    li.classList.toggle("on", i + 1 === n);
    li.classList.toggle("done", i + 1 < n);
  });
  $("#ob-body").scrollTop = 0;
  if (n === 1) { renderOb(); loadObTools(); }
  if (n === 2) { renderOb(); loadObSubs(); }
  if (n === 3) { renderOb(); checkReady(); }
  if (n === 4) { renderOb(); loadObSessions(); }
}
function renderOb() {
  const [body, ft] = [obToolsView, obSubsView, obModelView, obSessView][obStep - 1]();
  $("#ob-body").innerHTML = body;
  $("#ob-ft").innerHTML = ft;
}
const obBusy = (t) => `<p class="sub"><i class="dot working"></i>&nbsp; ${esc(t)}</p>`;
// meterHTML 画剩余额度的条：用掉七成标黄，九成标红。
function meterHTML(remaining) {
  const r = Math.max(0, Math.min(100, Math.round(remaining))), used = 100 - r;
  return `<span class="meter"><span class="bar"><i class="${used >= 90 ? "hi" : used >= 70 ? "mid" : ""}" style="width:${r}%"></i></span></span><span class="sub">剩余 ${r}%</span>`;
}

// 第 1 步：五个 AI 工具，装了的勾选，没装的在这里装。
async function loadObTools() {
  clearTimeout(obToolTimer);
  const was = new Set(obTools.filter((t) => t.installing).map((t) => t.name));
  try { obTools = await api("/api/tools"); } catch (err) { obErr = err.message; }
  if (obStep === 1) renderOb();
  if (obTools.some((t) => was.has(t.name) && !t.installing)) checkReady(); // 刚装完，助理能用的工具变了
  if (obTools.some((t) => t.installing)) obToolTimer = setTimeout(loadObTools, 1500);
}
function obToolRow(t) {
  const head = `${harnessIcon(t.name)}<div class="pref-l"><div class="name">${esc(t.label)}</div>`;
  if (t.installed) {
    const sub = t.name === "workbuddy" ? "已装（终端版 codebuddy）" : t.name === "hermes" ? "已装 · morula 还不能派活给 hermes" : "已装";
    return `<label class="row">${head}<div class="sub">${sub}</div></div><input type="checkbox" data-ob-tool="${esc(t.name)}"${t.enabled ? " checked" : ""}></label>`;
  }
  if (t.installing) return `<div class="row">${head}<div class="sub">安装中…</div></div><span class="ob-bar"><i></i></span></div>`;
  const can = (t.install || []).find((c) => !c.missing);
  const hint = { codex: " · 用 ChatGPT 账号登录", workbuddy: " · 装终端版 codebuddy" }[t.name] || "";
  const sub = t.error ? `<div class="form-err">${esc(t.error)}</div>` + (t.install?.[0] ? `<div class="sub mono sel clip">${esc((can || t.install[0]).command)}</div>` : "")
    : t.broken ? `<div class="sub clip" title="${esc(t.broken)}">装坏了，跑不起来：${esc(t.broken)}</div>`
    : `<div class="sub">没装${can ? hint : " · " + esc(t.install?.[0]?.missing || "不知道怎么装")}</div>`;
  const first = !obTools.some((x) => x.installed) && t.name === "codex";
  return `<div class="row">${head}${sub}</div><button class="btn ${first ? "primary" : "line"} sm" type="button" data-ob-install="${esc(t.name)}"${can ? "" : " disabled"}>${t.error ? "重试" : t.broken ? "重装" : "安装"}</button></div>`;
}
function obToolsView() {
  if (!obTools.length) return [obErr ? `<div class="form-err">${esc(obErr)}</div>` : obBusy("正在检查本机的 AI 工具…"), ""];
  const n = obTools.filter((t) => t.installed && t.enabled).length;
  const intro = obTools.some((t) => t.installed) ? "助理和 agent 借本机的 AI 工具干活。勾上的才会在 morula 里用。"
    : "这台 Mac 上还没有能用的 AI 工具。至少装一个，助理才能干活；不知道选哪个就装 codex。";
  return [`<p class="sub">${intro}</p><div class="card list">${obTools.map(obToolRow).join("")}</div>`,
    `<span class="sub grow">${n ? `勾了 ${n} 个` : "至少装一个并勾上"}</span><button class="btn primary" type="button" data-ob="next"${n ? "" : " disabled"}>下一步</button>`];
}

// 第 2 步：勾上的工具各一行，点「配置」打开它的订阅窗口。hermes 没有订阅，不列。
const obSubNames = () => obTools.filter((t) => t.installed && t.enabled && t.sub).map((t) => t.name);
async function loadObSubs() {
  clearTimeout(obSubTimer);
  await Promise.all(obSubNames().map(async (n) => {
    obSubs[n] = await api(`/api/tools/${encodeURIComponent(n)}/sub`).catch((err) => ({ name: n, error: err.message, models: [] }));
  }));
  if (obStep === 2) renderOb();
  if (obSubs.codex?.account?.logging) obSubTimer = setTimeout(loadObSubs, 2000); // codex 正在登录时等它完成
}
// subLine 是一个工具这一行的小字：need 为 true 时「配置」用主按钮。
function subLine(s) {
  const ms = s.models || [], on = ms.filter((m) => m.on).length;
  if (s.error) return { html: `<span class="form-err">${esc(s.error)}</span>` };
  if (s.name === "codex") {
    const a = s.account || {};
    if (!a.loggedIn) return a.logging ? { html: '<i class="dot working"></i>&nbsp; 等你在浏览器里登录 ChatGPT 账号…' } : { html: "没登录 · 用 ChatGPT 账号登录", need: true };
    const w = (a.windows || [])[0];
    return { html: `<div class="ob-q">${a.plan ? `<span class="tag">${esc(a.plan)}</span>` : ""}${w ? meterHTML(w.remaining) : ""}<span class="sub">· 勾了 ${on} 个模型</span></div>` };
  }
  if (!ms.length) return { html: s.name === "workbuddy" ? "查不到模型 · 可能还没登录 WorkBuddy 账号" : "还没有来源 · 登录或借 morula 的订阅", need: true };
  const src = (s.sources || []).length;
  return { html: `<span class="${on ? "ob-ok" : ""}">${src ? `${src} 个来源 · ` : ""}勾了 ${on} / ${ms.length} 个模型</span>`, need: !on };
}
function obSubsView() {
  const names = obSubNames();
  const rows = names.map((n) => {
    const s = obSubs[n];
    if (!s) return `<div class="row">${harnessIcon(n)}<div class="pref-l"><div class="name">${esc(hzName(n))}</div><div class="sub">读取中…</div></div></div>`;
    const l = subLine(s);
    return `<div class="row">${harnessIcon(n)}<div class="pref-l"><div class="name">${esc(hzName(n))}</div><div class="sub">${l.html}</div></div>
      <button class="btn ${l.need ? "primary" : "line"} sm" type="button" data-ob-sub="${esc(n)}">配置</button></div>`;
  });
  return [`<p class="sub">第 1 步勾上的每个工具一行，各自配置登录的账号和用哪些模型。已经配好的可以直接下一步。</p>` +
      (rows.length ? `<div class="card list">${rows.join("")}</div>` : '<p class="sub">勾上的工具都不用配订阅。</p>'),
    '<button class="btn" type="button" data-ob="back">上一步</button><span class="grow"></span><button class="btn primary" type="button" data-ob="next">下一步</button>'];
}

// 模型的小标签：WorkBuddy 的点数倍率（x0.00 是免费）、1M 上下文。
const freeModel = (m) => /^x0(\.0+)? /.test(m.credits || "");
function modelTags(m) {
  const c = /^x([\d.]+)/.exec(m.credits || "");
  return (c ? (+c[1] === 0 ? ' <b class="ob-free">免费</b>' : ` <em>${esc(c[1])}×</em>`) : "") + (m.context >= 1e6 ? " <em>1M</em>" : "");
}
// 第 3 步：给助理选模型，按工具分组，验证结果在底栏左边。
function obModelView() {
  const tools = (ready?.tools || []).filter((t) => t.models.length);
  const as = ready?.assistant;
  const all = tools.flatMap((t) => t.models.map((m) => ({ rt: t.name, id: m.id })));
  if (!obModel || !all.some((x) => x.rt === obModel.rt && x.id === obModel.id))
    obModel = all.find((x) => x.rt === as?.runtime && x.id === as?.model) || all[0] || null;
  const body = tools.length ? '<p class="sub">助理用哪个模型。以后在「设置 / 模型」里随时换。</p><div class="card list">' + tools.map((t) =>
      `<div class="ob-grp">${esc(hzName(t.name))}</div>` + t.models.map((m) => {
        const on = obModel?.rt === t.name && obModel.id === m.id;
        return `<label class="row ob-m"><input class="ob-rad" type="radio" name="ob-model" value="${esc(t.name + "\t" + m.id)}"${on ? " checked" : ""}>
          <span class="grow">${esc(m.name || m.id)}</span>${modelTags(m)}${as?.runtime === t.name && as.model === m.id ? '<span class="tag accent">助理在用</span>' : ""}</label>`;
      }).join("")).join("") + "</div>" +
      (obVerify?.ok && obVerify.text ? `<div class="note ob-note">助理：「${esc([...obVerify.text].slice(0, 120).join("") + ([...obVerify.text].length > 120 ? "…" : ""))}」</div>` : "")
    : ready ? '<p class="sub">第 2 步勾上的模型里还没有能用的。回上一步配置订阅。</p>' : obBusy("读取中…");
  return [body, obModelFt()];
}
const obPicked = () => (ready?.tools || []).find((t) => t.name === obModel?.rt)?.models.find((m) => m.id === obModel.id);
function obModelFt() {
  const v = obVerify, m = obPicked(), es = m?.efforts || [];
  const want = ready?.assistant?.runtime === obModel?.rt && ready.assistant.model === m?.id ? ready.assistant.effort : m?.default;
  const st = !v ? "" : v.running ? `<span class="sub grow"><i class="dot working"></i>&nbsp; 助理正在用 ${esc(v.model)} 做自我介绍…</span>`
    : v.ok ? `<span class="sub grow ob-ok"><i class="dot done"></i>&nbsp; 已调通，用了 ${v.secs} 秒</span>` : `<span class="form-err grow">${esc(v.error)}</span>`;
  return '<button class="btn" type="button" data-ob="back">上一步</button>' + (st || '<span class="grow"></span>') +
    (es.length ? `<select id="ob-effort" aria-label="思考强度">${es.map((x) => `<option value="${esc(x)}"${x === want ? " selected" : ""}>${esc(EFFORT_ZH[x] || x)}</option>`).join("")}</select>` : "") +
    (v?.ok ? '<button class="btn primary" type="button" data-ob="next">下一步</button>'
      : `<button class="btn primary" type="button" data-ob="go"${m && !v?.running ? "" : " disabled"}>让助理上线</button>`);
}
async function obGoOnline() {
  const m = obPicked();
  if (!m) return;
  const effort = $("#ob-effort")?.value || "";
  obVerify = { running: true, model: `${hzName(obModel.rt)} · ${m.name || m.id}` };
  $("#ob-ft").innerHTML = obModelFt();
  try {
    ready = await api("/api/ready/assistant", { method: "POST", body: JSON.stringify({ runtime: obModel.rt, model: m.id, effort }) });
    if (!ready.online) throw new Error(ready.reason || "助理还不能用");
    loadAgents();
    const t0 = Date.now();
    const r = await api("/api/ready/intro", { method: "POST" });
    if (!r.ok) throw new Error(r.error || "没有回复");
    obVerify = { ok: true, secs: Math.round((Date.now() - t0) / 1000), text: r.text };
  } catch (err) { obVerify = { error: "没调通：" + err.message + "。换个模型再试。" }; }
  if (obStep === 3) renderOb();
  checkReady();
}

// 第 4 步：最近 5 段对话，默认全勾；「查看全部」打开单独的窗口。
const sessKey = (s) => s.harness + "/" + s.id;
async function loadObSessions() {
  try {
    const o = await api("/api/onboard?list&cost");
    obSessions = o.sessions || [];
    obCost = o.cost || null;
    obPick = new Set(obSessions.map(sessKey));
  } catch (err) { obSessions = []; obErr = err.message; }
  if (obStep === 4) renderOb();
}
const tildePath = (d) => String(d || "").replace(/^\/Users\/[^/]+/, "~");
const sessTitle = (s) => s.title || s.first || "（没有标题）";
// obWhen 写对话最后更新的时间：今天、昨天写时刻，更早的写日期；day 为 false 时只写时刻（查看全部按天分了组）。
function obWhen(iso, day = true) {
  const d = new Date(iso), now = new Date(), t = d.toTimeString().slice(0, 5);
  const diff = Math.round((new Date(now.toDateString()) - new Date(d.toDateString())) / 864e5);
  if (!day) return t;
  return diff === 0 ? "今天 " + t : diff === 1 ? "昨天 " + t : `${String(d.getMonth() + 1).padStart(2, "0")}-${String(d.getDate()).padStart(2, "0")}`;
}
function obDay(iso) {
  const d = new Date(iso), diff = Math.round((new Date(new Date().toDateString()) - new Date(d.toDateString())) / 864e5);
  return diff === 0 ? "今天" : diff === 1 ? "昨天" : `${String(d.getMonth() + 1).padStart(2, "0")}-${String(d.getDate()).padStart(2, "0")}`;
}
const sessRow = (s, day) => `<label class="row ob-s"><input type="checkbox" data-ob-pick="${esc(sessKey(s))}"${obPick.has(sessKey(s)) ? " checked" : ""}>
  <div class="pref-l"><div class="name clip">${esc(sessTitle(s))}</div><div class="sub clip">${esc(hzName(s.harness))} · ${esc(tildePath(s.dir))}</div></div><span class="sub">${esc(obWhen(s.updated, day))}</span></label>`;
function obSessView() {
  if (!obSessions) return [obBusy("正在读本机以前的对话…"), ""];
  const n = obSessions.length, k = obPick.size, c = obCost;
  const err = obErr ? `<div class="form-err">${esc(obErr)}</div>` : "";
  if (!n) return [`<p class="sub">勾上的工具里没有找到以前的对话。</p>${err}`,
    '<span class="grow"></span><button class="btn primary" type="button" data-ob="fresh">完成</button>'];
  const cost = c ? `：把最近 ${c.sessions} 段的标题和开头交给 ${esc(c.model)}，约 ${c.chars.toLocaleString()} 字${c.left ? `；一次最多整理 ${c.sessions} 段，剩下的以后在「设置 / 以前的对话」里接着整理` : ""}` : "";
  return [`<p class="sub">接入后助理按项目整理，会用掉较多模型额度${cost}。</p>
    <div class="sec-h"><span class="grow">共 ${n} 段，最近 ${Math.min(5, n)} 段</span>${n > 5 ? `<a class="ob-lnk" data-ob="all">查看全部 ${n} 段 ›</a>` : ""}</div>
    <div class="card list">${obSessions.slice(0, 5).map((s) => sessRow(s, true)).join("")}</div>${err}`,
    `<button class="btn" type="button" data-ob="fresh">不接入，全新开始</button><span class="grow"></span>
    ${k < n ? `<button class="btn line" type="button" data-ob="some"${k ? "" : " disabled"}>只接入勾上的 ${k} 段</button>` : ""}<button class="btn primary" type="button" data-ob="every">全部接入</button>`];
}
async function obImport(ids) {
  try {
    if (ids) await api("/api/onboard/start", { method: "POST", body: JSON.stringify({ ids }) });
    else await api("/api/onboard/start", { method: "POST" });
    $("#ob-all").hidden = true;
    $("#setup").hidden = true;
    show("import");
  } catch (err) { obErr = err.message; renderOb(); }
}

$("#setup").addEventListener("change", async (e) => {
  const t = e.target;
  if (t.dataset.obTool) {
    const on = obTools.filter((x) => x.installed && (x.name === t.dataset.obTool ? t.checked : x.enabled)).map((x) => x.name);
    try { obTools = await api("/api/tools/enabled", { method: "PUT", body: JSON.stringify({ on }) }); } catch (err) { obErr = err.message; }
    renderOb();
    checkReady();
  } else if (t.name === "ob-model") {
    const [rt, id] = t.value.split("\t");
    obModel = { rt, id };
    obVerify = null;
    $("#ob-ft").innerHTML = obModelFt();
  } else if (t.dataset.obPick) {
    t.checked ? obPick.add(t.dataset.obPick) : obPick.delete(t.dataset.obPick);
    $("#ob-ft").innerHTML = obSessView()[1];
  }
});
$("#setup").addEventListener("click", async (e) => {
  const ins = e.target.closest("[data-ob-install]"), sub = e.target.closest("[data-ob-sub]"), act = e.target.closest("[data-ob]");
  if (ins) {
    ins.disabled = true;
    try { obTools = await api(`/api/tools/${encodeURIComponent(ins.dataset.obInstall)}/install`, { method: "POST" }); } catch (err) { obErr = err.message; }
    renderOb();
    loadObTools();
    return;
  }
  if (sub) return openTsub(sub.dataset.obSub);
  if (!act) return;
  const a = act.dataset.ob;
  if (a === "next") obGo(obStep + 1);
  else if (a === "back") obGo(obStep - 1);
  else if (a === "go") obGoOnline();
  else if (a === "all") openObAll();
  else if (a === "some") { act.disabled = true; obImport([...obPick]); }
  else if (a === "every") { act.disabled = true; obImport(null); }
  else if (a === "fresh") {
    await api("/api/onboard/skip", { method: "POST" }).catch(() => {});
    $("#setup").hidden = true;
    show("chat");
    checkReady();
  }
});
// 助理已上线时关掉就不再自动弹；没上线时下次启动还会弹
$("#setup-close").addEventListener("click", async () => {
  $("#setup").hidden = true;
  clearTimeout(obToolTimer);
  clearTimeout(obSubTimer);
  if (ready?.online) await api("/api/onboard/hide", { method: "POST" }).catch(() => {});
  checkReady();
});

/* ---------- 一个 AI 工具的订阅窗口 ---------- */
// 账号（codex、WorkBuddy）或来源（opencode、pi），加上勾哪些模型。只有勾上的模型出现在 morula 里。
let tsub = null, tsubPick = new Set(), tsubTest = {}, tsubTimer = 0, tsubMsg = "";
// 启动流程第 2 步已经读过的直接用；从「设置 / AI 工具」打开时先读一次。
async function openTsub(name) {
  tsub = obSubs[name] || { name, models: [], loading: true };
  tsubPick = new Set(tsub.models.filter((m) => m.on).map((m) => m.id));
  tsubTest = {};
  tsubMsg = "";
  $("#tsub").hidden = false;
  renderTsub();
  if (tsub.loading) await tsubReload(false);
}
// srcKey 是模型属于哪个来源：经网关的按 via，工具自己登录的按 id 里「/」前面那段。和 tools.go 的 toolSources 一致。
const srcKey = (m) => m.via || m.id.split("/")[0];
const chipLabel = (n, m) => (n === "codex" || n === "workbuddy" ? m.name || m.id : m.id);
function renderTsub() {
  const s = tsub, n = s.name, ms = s.models || [], label = esc(hzName(n));
  $("#tsub-hd").innerHTML = `${harnessIcon(n)}<h2>${label} 订阅</h2>`;
  if (s.loading) return void ($("#tsub-body").innerHTML = obBusy("读取中…"));
  let acct = "";
  if (n === "codex") {
    const a = s.account || {};
    acct = `<div class="sec ob-first"><div class="sec-h">账号</div><div class="card list"><div class="row">` + (a.loggedIn
      ? `<div class="pref-l"><div class="name">ChatGPT 账号 ${a.plan ? `<span class="tag">${esc(a.plan)}</span>` : ""}</div>
          ${(a.windows || []).map((w) => `<div class="ob-q"><span class="sub ob-wn">${esc(w.name)}</span>${meterHTML(w.remaining)}</div>`).join("") || '<div class="sub">还没读到额度：codex 跑过一次之后才有。</div>'}</div><span class="tag accent">已登录</span>`
      : `<div class="pref-l"><div class="name">还没登录</div><div class="sub">${a.logging ? "已在浏览器打开登录页，登录完成后这里自动更新。" : "会在浏览器里打开 OpenAI 的登录页。"}</div>
          ${a.logging && a.url ? `<div class="sub clip">登录页没打开时手动访问：<span class="mono sel">${esc(a.url)}</span></div>` : ""}${a.error ? `<div class="form-err">${esc(a.error)}</div>` : ""}</div>
          <button class="btn primary sm" type="button" data-tsub="login"${a.logging ? " disabled" : ""}>${a.logging ? "登录中…" : "登录"}</button>`) +
      `</div></div>${a.readAt && a.loggedIn ? `<p class="sub ob-cap">额度读自 codex 最近一次运行的记录。</p>` : ""}</div>`;
  } else if (n === "workbuddy") {
    acct = `<div class="sec ob-first"><div class="sec-h">账号</div><div class="card list"><div class="row"><div class="pref-l"><div class="name">WorkBuddy 账号</div>
      <div class="sub">终端版 codebuddy 和桌面版 WorkBuddy 用同一个账号。登录要在终端里进 codebuddy，输入 /login。morula 读不到账号名和积分。</div></div>
      <button class="btn line sm" type="button" data-tsub="login">在终端里登录</button></div></div></div>`;
  } else {
    const srcs = s.sources || [];
    acct = `<div class="sec ob-first"><div class="sec-h">来源</div><div class="card list">` + srcs.map((x) => {
      const mine = ms.filter((m) => srcKey(m) === x.id), k = mine.filter((m) => tsubPick.has(m.id)).length;
      return `<label class="row"><input type="checkbox" data-tsub-src="${esc(x.id)}"${k === mine.length ? " checked" : ""}${k && k < mine.length ? " data-mixed" : ""}>
        <div class="pref-l"><div class="name">${esc(x.name)} <span class="sub">· ${x.kind === "gateway" ? "经 morula 的网关" : label + " 自己登录的"}</span></div><div class="sub">${x.models} 个模型 · 勾了 ${k} 个</div></div></label>`;
    }).join("") +
      `<div class="row click" data-tsub="login"><span class="muted">＋</span><div class="pref-l"><div class="name ob-add">登录一个服务（在终端里打开 <span class="mono">${esc(s.login?.command || n)}</span>）</div></div></div>
      <div class="row click" data-tsub="key"><span class="muted">＋</span><div class="pref-l"><div class="name ob-add">填一个 API 密钥</div></div></div>
      <div class="row click" data-tsub="plug"><span class="muted">＋</span><div class="pref-l"><div class="name ob-add">用插件登录别的订阅（去「设置 / 账号」）</div></div></div>
      </div><p class="sub ob-cap">${label} 自己登录的服务从它自己的配置读，morula 不改它；API 密钥和插件订阅经 morula 的网关借给它。</p></div>`;
  }
  const k = ms.filter((m) => tsubPick.has(m.id)).length;
  const models = `<div class="sec"><div class="sec-h"><span class="grow">模型${ms.length ? ` · 勾了 ${k} / ${ms.length} 个` : ""}</span>` +
    (ms.length ? '<button class="btn sm" type="button" data-tsub-all="all">全选</button><button class="btn sm" type="button" data-tsub-all="none">全不选</button>' +
      (ms.some(freeModel) ? '<button class="btn sm" type="button" data-tsub-all="free">只选免费</button>' : "") : "") + "</div>" +
    (ms.length ? `<div class="ob-chips">${ms.map((m) => {
      const on = tsubPick.has(m.id), t = tsubTest[m.id];
      const res = !t ? "" : t.running ? " <em>测试中</em>" : t.ok ? ` <em class="ob-ok">${(t.ms / 1000).toFixed(1)} 秒</em>` : ' <em class="ob-bad">没调通</em>';
      return `<button class="ob-mc${on ? " on" : ""}" type="button" data-tsub-m="${esc(m.id)}" title="${esc(t?.error || m.id)}">${on ? "✓ " : ""}${esc(chipLabel(n, m))}${modelTags(m)}${res}</button>`;
    }).join("")}</div>`
      : `<p class="sub">${s.error ? esc(s.error) : n === "workbuddy" ? "查不到模型。登录 WorkBuddy 账号后点「重新获取模型」。" : n === "codex" ? "登录后这里列出 codex 的模型。" : "还没有来源。登录一个服务或填一个 API 密钥后，这里列出它的模型。"}</p>`) +
    `<div class="ob-acts"><button class="btn line sm" type="button" data-tsub="refresh">重新获取模型</button>${ms.length ? `<button class="btn line sm" type="button" data-tsub="test"${k ? "" : " disabled"}>测试勾上的模型</button>` : ""}<span class="sub">${esc(tsubMsg)}</span></div></div>`;
  $("#tsub-body").innerHTML = acct + models;
  $("#tsub-body").querySelectorAll("[data-mixed]").forEach((x) => { x.indeterminate = true; });
}
async function tsubReload(refresh) {
  const n = tsub.name, before = new Set(tsub.models.map((m) => m.id));
  const s = await api(`/api/tools/${encodeURIComponent(n)}/sub${refresh ? "?refresh" : ""}`).catch((err) => ({ ...tsub, error: err.message }));
  if ($("#tsub").hidden || tsub.name !== n) return;
  // 新出现的模型按服务端的勾选，原来就有的保留窗口里的勾选
  for (const m of s.models || []) if (!before.has(m.id) && m.on) tsubPick.add(m.id);
  tsub = s;
  renderTsub();
}
function tsubWatchLogin() {
  clearTimeout(tsubTimer);
  tsubTimer = setTimeout(async () => {
    await tsubReload(false);
    if (tsub.account?.logging && !$("#tsub").hidden) tsubWatchLogin();
  }, 2000);
}
$("#tsub").addEventListener("change", (e) => {
  const id = e.target.dataset.tsubSrc;
  if (id === undefined) return;
  for (const m of tsub.models) if (srcKey(m) === id) e.target.checked ? tsubPick.add(m.id) : tsubPick.delete(m.id);
  renderTsub();
});
$("#tsub").addEventListener("click", async (e) => {
  const chip = e.target.closest("[data-tsub-m]"), all = e.target.closest("[data-tsub-all]"), act = e.target.closest("[data-tsub]");
  if (chip) {
    const id = chip.dataset.tsubM;
    tsubPick.has(id) ? tsubPick.delete(id) : tsubPick.add(id);
    return renderTsub();
  }
  if (all) {
    const v = all.dataset.tsubAll;
    tsubPick = new Set(v === "none" ? [] : tsub.models.filter((m) => v === "all" || freeModel(m)).map((m) => m.id));
    return renderTsub();
  }
  if (!act) return;
  const n = tsub.name, a = act.dataset.tsub;
  if (a === "login") {
    try {
      await api(`/api/tools/${encodeURIComponent(n)}/login`, { method: "POST" });
      tsubMsg = n === "codex" ? "" : `已在终端里打开 ${tsub.login?.command || n}。${tsub.login?.hint || ""}，登录好后点「重新获取模型」。`;
      if (n === "codex") { await tsubReload(false); tsubWatchLogin(); return; }
    } catch (err) { tsubMsg = "没打开登录：" + err.message; }
    renderTsub();
  } else if (a === "refresh") {
    tsubMsg = "正在重新获取模型…";
    renderTsub();
    await tsubReload(true);
    tsubMsg = "模型列表 · 刚刚更新";
    renderTsub();
  } else if (a === "test") {
    const ids = tsub.models.filter((m) => tsubPick.has(m.id)).map((m) => m.id);
    act.disabled = true;
    for (const [i, id] of ids.entries()) {
      if ($("#tsub").hidden || tsub.name !== n) return;
      tsubTest[id] = { running: true };
      tsubMsg = `正在测第 ${i + 1} / ${ids.length} 个`;
      renderTsub();
      tsubTest[id] = await api(`/api/tools/${encodeURIComponent(n)}/test`, { method: "POST", body: JSON.stringify({ model: id }) }).catch((err) => ({ ok: false, error: err.message }));
    }
    const bad = ids.filter((id) => !tsubTest[id]?.ok).length;
    tsubMsg = bad ? `测了 ${ids.length} 个，${bad} 个没调通（鼠标停在标签上看原因）` : `测了 ${ids.length} 个，都调通了`;
    renderTsub();
  } else if (a === "key") {
    openApiKey();
  } else if (a === "plug") {
    setupBack = true;
    $("#tsub").hidden = true;
    $("#setup").hidden = true;
    show("providers");
  }
});
$("#tsub-cancel").addEventListener("click", () => { $("#tsub").hidden = true; clearTimeout(tsubTimer); });
$("#tsub-save").addEventListener("click", async (e) => {
  const n = tsub.name;
  e.target.disabled = true;
  $("#tsub-st").textContent = "";
  try {
    if (tsub.models.length) obSubs[n] = await api(`/api/tools/${encodeURIComponent(n)}/models`, { method: "PUT", body: JSON.stringify({ ids: [...tsubPick] }) });
    else obSubs[n] = tsub;
    $("#tsub").hidden = true;
    clearTimeout(tsubTimer);
    if (obStep === 2 && !$("#setup").hidden) renderOb();
    if (pane === "harness") loadHarnessPane();
    checkReady();
  } catch (err) { $("#tsub-st").innerHTML = `<span class="form-err">${esc(err.message)}</span>`; }
  finally { e.target.disabled = false; }
});

/* ---------- 第 4 步「查看全部」 ---------- */
function openObAll() {
  const hs = [...new Set(obSessions.map((s) => s.harness))];
  $("#oba-hz").innerHTML = '<option value="">全部工具</option>' + hs.map((h) => `<option value="${esc(h)}">${esc(hzName(h))}</option>`).join("");
  $("#oba-q").value = "";
  $("#oba-t").textContent = `以前的对话 · ${obSessions.length} 段`;
  $("#ob-all").hidden = false;
  renderObAll();
}
function obAllRows() {
  const q = $("#oba-q").value.trim().toLowerCase(), h = $("#oba-hz").value;
  return obSessions.filter((s) => (!h || s.harness === h) && (!q || [s.title, s.first, s.dir].some((x) => String(x || "").toLowerCase().includes(q))));
}
function renderObAll() {
  const rows = obAllRows();
  let day = "", html = "";
  for (const s of rows) {
    const d = obDay(s.updated);
    if (d !== day) { day = d; html += `<div class="ob-grp">${esc(d)}</div>`; }
    html += sessRow(s, false);
  }
  $("#oba-list").innerHTML = html || '<div class="row empty">没有对得上的对话</div>';
  obAllFt();
}
function obAllFt() {
  $("#oba-n").textContent = `勾了 ${obPick.size} 段`;
  $("#oba-go").textContent = `接入勾上的 ${obPick.size} 段`;
  $("#oba-go").disabled = !obPick.size;
}
$("#oba-q").addEventListener("input", renderObAll);
$("#oba-hz").addEventListener("change", renderObAll);
$("#ob-all").addEventListener("change", (e) => {
  const k = e.target.dataset.obPick;
  if (!k) return;
  e.target.checked ? obPick.add(k) : obPick.delete(k);
  obAllFt();
});
$("#ob-all").addEventListener("click", (e) => {
  const b = e.target.closest("[data-oba]");
  if (!b) return;
  for (const s of obAllRows()) b.dataset.oba === "all" ? obPick.add(sessKey(s)) : obPick.delete(sessKey(s));
  renderObAll();
});
$("#oba-close").addEventListener("click", () => { $("#ob-all").hidden = true; if (obStep === 4) renderOb(); });
$("#oba-go").addEventListener("click", (e) => { e.target.disabled = true; obImport([...obPick]); });

/* ---------- 填一个 API 密钥 ---------- */
function openApiKey() {
  $("#setup-api-st").textContent = "";
  $("#apikey").hidden = false;
}
$("#ak-close").addEventListener("click", () => { $("#apikey").hidden = true; });
$("#setup-api").addEventListener("submit", async (e) => {
  e.preventDefault();
  const f = e.target, st = $("#setup-api-st"), btn = f.querySelector("[type=submit]");
  const body = { name: f.name.value, baseUrl: f.baseUrl.value, key: f.key.value, models: f.models.value.split(/[,，\s]+/).filter(Boolean) };
  btn.disabled = true;
  st.textContent = "正在读取模型列表…";
  try {
    const a = await api("/api/apikeys", { method: "POST", body: JSON.stringify(body) });
    st.textContent = `已保存「${a.name}」，${a.models.length} 个模型。`;
    f.key.value = "";
    $("#apikey").hidden = true;
    if (!$("#tsub").hidden) tsubReload(true);
    checkReady();
    if (pane === "providers") loadPlugins(true);
  } catch (err) {
    st.innerHTML = `<span class="form-err">${esc(err.message)}</span>`;
  } finally { btn.disabled = false; }
});
/* ---------- 对话框里选助理的 harness、模型、推理强度 ---------- */
// 改了立即写回助理这个 agent；验收 agent 不跟着改。
function renderPick(as) {
  const box = document.querySelector("#composer .pick");
  box.hidden = !as || !harnesses.length;
  if (box.hidden) return;
  box.dataset.agent = as.name;
  const srcs = pickSources(as);
  $("#as-rt").innerHTML = srcs.map((x) => `<option value="${esc(x.v)}">${esc(x.l)}</option>`).join("");
  $("#as-rt").value = srcOf(as.runtime, as.model);
  fillPick(as.model, as.effort);
}
// 第一个下拉框选模型来源：harness 自己的模型算一个来源（codex、opencode…），
// 插件和 API key 来源（codex 里模型写作 来源/模型，例如 workbuddy/glm-5.3）各算一个。值是「harness|来源」。
// opencode、pi 的网关模型写作 morula/来源/模型；它们自己的模型 id 写作 provider/模型，按 provider 各算一个来源，
// 来源前加「@」（例如 opencode|@deepseek），和网关来源分开：同一个 DeepSeek，opencode 里配的和 WorkBuddy 的是两个订阅。
const srcOf = (rt, model) => {
  const m = String(model);
  if (rt !== "codex" && !m.startsWith("morula/")) return rt + "|" + (m.includes("/") ? "@" + m.split("/")[0] : "");
  const p = rt === "codex" ? m : m.slice(7);
  return rt + "|" + (p.includes("/") ? p.split("/")[0] : "");
};
function pickSources(as) {
  const out = [];
  for (const h of harnesses) {
    if (!h.ready && h.name !== as.runtime) continue;
    const seen = new Set();
    for (const m of h.models) {
      const v = srcOf(h.name, m.id);
      if (seen.has(v)) continue;
      seen.add(v);
      const p = v.split("|")[1], nm = String(m.name || "");
      out.push({ v, l: p ? (nm.includes(" · ") ? nm.split(" · ")[0] : p) : h.name });
    }
    if (!seen.size) out.push({ v: h.name + "|", l: h.name + (h.ready ? "" : "（还不能用）") });
  }
  if (!out.some((x) => x.v === srcOf(as.runtime, as.model))) out.unshift({ v: srcOf(as.runtime, as.model), l: srcOf(as.runtime, as.model).replace("|", " · ") + "（不可用）" });
  return out;
}
const pickModels = () => {
  const [rt, p] = $("#as-rt").value.split("|");
  return (hOf(rt)?.models || []).filter((m) => srcOf(rt, m.id) === rt + "|" + p);
};
function fillPick(model, effort) {
  const ms = pickModels();
  let html = ms.map((m) => `<option value="${esc(m.id)}">${esc(String(m.name || m.id).split(" · ").pop())}</option>`).join("");
  if (model && !ms.some((m) => m.id === model)) html = `<option value="${esc(model)}">${esc(model)}（不可用）</option>` + html;
  $("#as-model").innerHTML = html || '<option value="">没有模型</option>';
  $("#as-model").value = model || ms[0]?.id || "";
  fillPickEffort(effort);
}
function fillPickEffort(effort) {
  const m = pickModels().find((x) => x.id === $("#as-model").value);
  const es = m?.efforts || [];
  const ef = $("#as-effort");
  ef.hidden = !es.length;
  ef.innerHTML = `<option value="">${m?.default ? `思考 默认（${esc(m.default)}）` : "思考 默认"}</option>` + es.map((x) => `<option value="${esc(x)}">思考 ${esc(x)}</option>`).join("");
  ef.value = es.includes(effort) ? effort : "";
  pickLabel();
}
// 输入框右下角只露一个按钮：模型名加推理强度；点开是菜单，三个隐藏的下拉框存当前值。
const optText = (sel) => sel.selectedOptions[0]?.textContent || "";
// 按钮只写「harness · 模型」，思考强度用默认时不写；订阅和全名放在悬停提示里，模型名太长时截断。
function pickText() {
  const v = $("#as-rt").value, rt = v.split("|")[0];
  return { pre: rt + " · ", src: !ownSrc(v) ? srcName(v, optText($("#as-rt"))) + " · " : "", model: optText($("#as-model")) };
}
function pickLabel() {
  const t = pickText(), ef = $("#as-effort");
  const e = ef.hidden || !ef.value ? "" : " · " + (EFFORT_ZH[ef.value] || ef.value);
  $("#pick-rt").textContent = t.pre;
  $("#pick-model").textContent = t.model;
  $("#pick-effort").textContent = e;
  $("#pick-btn").title = t.pre + t.src + t.model + e;
}
// 菜单照画布的 ModelPick：模型一层带上下文窗口，下面是推理强度，最底下是说明和「管理模型」。没就绪的 harness 不列。
let pickQ = "";
const ICON = {
  check: '<path d="M20 6 9 17l-5-5"/>',
  search: '<circle cx="11" cy="11" r="7"/><path d="m20 20-3.5-3.5"/>',
  left: '<path d="m15 6-6 6 6 6"/>',
  right: '<path d="m9 6 6 6-6 6"/>',
};
const icon = (n) => `<svg viewBox="0 0 24 24">${ICON[n]}</svg>`;
// 订阅的名字，总是跟在 harness 名后面显示，所以不再写 harness：harness 自己的模型叫「自带」（codex 的是 ChatGPT 订阅），
// harness 里自己配的 provider 写「provider（自配）」，经网关用的 ChatGPT 订阅，插件和 API key 来源用它们的名字。
const ownSrc = (v) => { const [rt, p] = v.split("|"); return !p || p === "@" + rt; };
function srcName(v, label = "") {
  const [rt, p] = v.split("|");
  if (ownSrc(v)) return rt === "codex" ? "ChatGPT 订阅" : "自带";
  if (p.startsWith("@")) return p.slice(1) + "（自配）";
  if (p === "codex") return "ChatGPT 订阅";
  const pv = (plug.providers || []).find((x) => x.id === p);
  return pv?.name || label.replace(/（不可用）$/, "") || p;
}
// 给人看的「harness · 订阅 · 模型名」，不写模型 id；用 harness 自带的模型时不写订阅。
function modelText(rt, model) {
  const v = srcOf(rt, model), m = hOf(rt)?.models.find((x) => x.id === model);
  return rt + " · " + (ownSrc(v) ? "" : srcName(v) + " · ") + (m ? String(m.name || m.id).split(" · ").pop() : model);
}
// harness h 能用的订阅，来自 #as-rt 的选项（pickSources）。
const srcsOf = (h) => [...$("#as-rt").options].filter((o) => o.value.split("|")[0] === h).map((o) => ({ v: o.value, l: srcName(o.value, o.textContent) }));
// 菜单分三层，一层一屏：第一层列能用的 harness，点进去列这个 harness 能用的订阅，再点进去列模型；顶上的路径点回上一层。
// 打开时在第一层，当前用的 harness、订阅、模型打勾。点模型才保存；翻 harness、订阅不改设置。
// 第一层的搜索框找所有能用的模型，每条写明 harness 和订阅；第三层的只找这个订阅下的。
let pickH = "", pickS = "", pickLv = 3;
const modelName = (m) => String(m.name || m.id).split(" · ").pop();
const srcModels = (v) => { const rt = v.split("|")[0]; return (hOf(rt)?.models || []).filter((m) => srcOf(rt, m.id) === v); };
const pickHs = () => harnesses.filter((h) => srcsOf(h.name).length);
// 设置里改了模型来源后 harnesses 会清空（setPlug），打开菜单前重读一次，再按新列表重填订阅。
async function pickFresh() {
  if (harnesses.length) return;
  await loadHarnesses();
  const as = agentList.find((a) => a.role === "助理");
  if (as) renderPick(as);
}
function modelRow(m, v, on, note) {
  return `<button type="button" class="pick-it" role="menuitemradio" aria-checked="${on}"${m.gone ? " disabled" : ""} data-m="${esc(m.id)}" data-v="${esc(v)}" title="${esc(m.id)}">` +
    (note ? `<span>${esc(modelName(m))}<small class="pick-src">${esc(note)}</small></span>` : `<span>${esc(modelName(m))}</span>`) + (note ? "" :
    m.gone ? '<em class="cmp-r">已不在列表里</em>' : m.context ? `<em class="cmp-r mono">${fmtCtx(m.context).toLowerCase()}</em>` : "") + icon("check") + "</button>";
}
// 往下一层的一行：名字、右边的数量、当前在用的打勾、箭头。
const navRow = (attr, val, name, n, on) => `<button type="button" class="pick-it" role="menuitemradio" aria-checked="${on}" ${attr}="${esc(val)}">` +
  `<span>${esc(name)}</span><em class="cmp-r">${esc(n)}</em>${icon("check")}<i class="pick-go">${icon("right")}</i></button>`;
function pickList() {
  const md = $("#as-model"), cur = $("#as-rt").value, q = pickQ.trim().toLowerCase();
  const hit = (m) => !q || (m.id + " " + (m.name || "")).toLowerCase().includes(q);
  if (pickLv === 1 && q) {
    const rows = pickHs().flatMap((h) => srcsOf(h.name).flatMap((s) => srcModels(s.v).filter(hit)
      .map((m) => modelRow(m, s.v, s.v === cur && m.id === md.value, h.name + " · " + s.l))));
    return rows.join("") || `<div class="cmp-empty">没有找到「${esc(pickQ)}」</div>`;
  }
  if (pickLv === 1) return pickHs().map((h) => { const n = srcsOf(h.name).length; return navRow("data-h", h.name, h.name, n + " 个订阅", h.name === cur.split("|")[0]); }).join("");
  if (pickLv === 2) return srcsOf(pickH).map((s) => navRow("data-s", s.v, s.l, srcModels(s.v).length + " 个模型", s.v === cur)).join("") ||
    '<div class="cmp-empty">没有可用的订阅</div>';
  const ms = srcModels(pickS).filter(hit);
  if (pickS === cur && md.value && !ms.some((m) => m.id === md.value) && (!q || md.value.toLowerCase().includes(q)))
    ms.unshift({ id: md.value, name: md.value, gone: true });
  if (!ms.length) return `<div class="cmp-empty">${q ? `没有找到「${esc(pickQ)}」` : "这个订阅下没有模型"}</div>`;
  return ms.map((m) => modelRow(m, pickS, pickS === cur && m.id === md.value)).join("");
}
function pickMenu() {
  const ef = $("#as-effort"), sub = srcsOf(pickH).find((s) => s.v === pickS);
  let html = '<div class="pick-crumb">';
  if (pickLv === 1) html += "<b>选 AI 工具</b>";
  else {
    html += `<button type="button" data-lv="${pickLv - 1}" aria-label="返回上一层">${icon("left")}</button>` +
      `<button type="button" class="pick-cr" data-lv="1">${esc(pickH)}</button>`;
    if (pickLv === 3) html += `<i>›</i><button type="button" class="pick-cr" data-lv="2" title="${esc(sub?.l || "")}">${esc(sub?.l || "")}</button>`;
  }
  html += "</div>";
  if (pickLv !== 2) html += `<label class="cmp-search">${icon("search")}<input id="pick-q" placeholder="${pickLv === 1 ? "在所有 AI 工具里找模型" : "找模型"}" autocomplete="off" value="${esc(pickQ)}"></label>`;
  html += `<div class="pick-list" id="pick-list">${pickList()}</div>`;
  if (pickLv === 3 && !ef.hidden && pickS === $("#as-rt").value) {
    const dflt = ef.options[0].textContent.match(/（(.+)）/)?.[1] || "";
    html += `<div class="pick-sep"></div><div class="cmp-ef"><span>思考强度</span><div class="seg" role="radiogroup" aria-label="思考强度">` +
      [...ef.options].map((o) => `<button type="button" role="radio" aria-checked="${o.value === ef.value}" class="${o.value === ef.value ? "on" : ""}" data-ef="${esc(o.value)}"` +
        `${o.value ? "" : ` title="模型默认${dflt ? "：" + esc(EFFORT_ZH[dflt] || dflt) : ""}"`}>${o.value ? esc(EFFORT_ZH[o.value] || o.value) : "默认"}</button>`).join("") + "</div></div>";
  }
  html += `<div class="pick-sep"></div><div class="cmp-ft"><span>改了立即生效，下一条消息起用</span><button type="button" class="cmp-link" data-prefs="providers">管理模型</button></div>`;
  $("#pick-menu").innerHTML = html;
}
// 输入框底部的三个弹层（上下文、审批、模型）同一时间只开一个。
const POPS = { pick: ["#pick-btn", "#pick-menu"], tier: ["#tier-btn", "#tier-menu"], cx: ["#cx-btn", "#cx-menu"] };
function popOpen(name, on) {
  for (const [k, [b, m]] of Object.entries(POPS)) {
    const v = k === name && on;
    $(m).hidden = !v;
    $(b).setAttribute("aria-expanded", v);
  }
}
const pickOpen = (on) => popOpen("pick", on);
$("#pick-btn").addEventListener("click", async (e) => {
  e.stopPropagation();
  const open = $("#pick-menu").hidden;
  if (open) await pickFresh();
  if (open) { pickQ = ""; pickS = $("#as-rt").value; pickH = pickS.split("|")[0]; pickLv = 1; pickMenu(); }
  pickOpen(open);
  if (open) $("#pick-q")?.focus();
});
$("#pick-menu").addEventListener("input", (e) => {
  if (e.target.id !== "pick-q") return;
  pickQ = e.target.value;
  $("#pick-list").innerHTML = pickList();
});
$("#pick-menu").addEventListener("click", (e) => {
  e.stopPropagation();
  const m = e.target.closest("[data-m]:not(:disabled)"), f = e.target.closest("[data-ef]");
  const h = e.target.closest("[data-h]"), sb = e.target.closest("[data-s]"), lv = e.target.closest("[data-lv]");
  if (h || sb || lv) {
    if (h) { pickH = h.dataset.h; pickLv = 2; }
    else if (sb) { pickS = sb.dataset.s; pickLv = 3; }
    else pickLv = +lv.dataset.lv;
    pickQ = "";
    pickMenu();
    return $("#pick-q")?.focus();
  }
  if (m) {
    const v = m.dataset.v || pickS;
    if (v !== $("#as-rt").value) {
      $("#as-rt").value = v;
      fillPick(m.dataset.m, "");
      savePick();
    } else if (m.dataset.m !== $("#as-model").value) {
      $("#as-model").value = m.dataset.m;
      $("#as-model").dispatchEvent(new Event("change"));
    }
    return pickOpen(false);
  }
  if (f && f.dataset.ef !== $("#as-effort").value) {
    $("#as-effort").value = f.dataset.ef;
    $("#as-effort").dispatchEvent(new Event("change"));
    pickMenu();
  }
});
// 弹层底部的「管理模型」「编辑清单」打开设置里对应的一页。
document.querySelector("#composer").addEventListener("click", (e) => {
  const b = e.target.closest(".cmp-menu [data-prefs], #pick-menu [data-prefs]");
  if (!b) return;
  popOpen("", false);
  openPrefs(b.dataset.prefs);
}, true);
document.addEventListener("click", () => popOpen("", false));
document.addEventListener("keydown", (e) => { if (e.key === "Escape") popOpen("", false); });

/* ---------- 输入框底部：审批档位和上下文 ---------- */
// 档位选了就一直有效，直到再改（work.Engine.Tier）。上下文按最后一次请求算（agent.go 记的 assistant.context）。
async function loadComposer() {
  try { composer = await api("/api/composer"); } catch { return; }
  renderComposer();
}
// 助理模型的上下文窗口；harness 没给时用压缩线当分母。
function cxWindow() {
  const as = agentList.find((a) => a.role === "助理");
  return (as && hOf(as.runtime)?.models.find((m) => m.id === (liveOf(as) || as.model))?.context) || composer.limit || 0;
}
function renderComposer() {
  const t = TIERS.find((x) => x.v === composer.tier) || TIERS[1];
  $("#tier-l").textContent = t.l;
  $("#tier-btn").classList.toggle("risk", !!t.risk);
  const win = cxWindow(), pct = win ? Math.min(100, Math.round((composer.context / win) * 100)) : 0;
  $("#cx-arc").setAttribute("stroke-dasharray", `${pct} 100`);
  $("#cx-pct").textContent = pct + "%";
  $("#cx-btn").classList.toggle("hi", pct >= 80);
}
function tierMenu() {
  $("#tier-menu").innerHTML = `<div class="pick-h">agent 做这些事之前要不要问你</div>` + TIERS.map((t) =>
    (t.risk ? '<div class="pick-sep"></div>' : "") +
    `<button type="button" class="pick-it cmp-tier${t.risk ? " risk" : ""}" role="menuitemradio" aria-checked="${t.v === composer.tier}" data-tier="${t.v}">` +
    `<span><b>${t.l}</b><small>${t.d}</small></span>${icon("check")}</button>`).join("") +
    `<div class="pick-sep"></div><div class="cmp-ft"><span>选了一直有效，直到你再改</span><button type="button" class="cmp-link" data-prefs="approval">编辑清单</button></div>`;
}
$("#tier-btn").addEventListener("click", (e) => {
  e.stopPropagation();
  const open = $("#tier-menu").hidden;
  if (open) tierMenu();
  popOpen("tier", open);
});
$("#tier-menu").addEventListener("click", async (e) => {
  e.stopPropagation();
  const b = e.target.closest("[data-tier]");
  if (!b || b.dataset.tier === composer.tier) return;
  try {
    composer = await api("/api/composer/tier", { method: "PUT", body: JSON.stringify({ tier: b.dataset.tier }) });
    renderComposer();
    tierMenu();
    popOpen("", false);
  } catch (err) { $("#chat-state").textContent = err.message; }
});
function cxMenu() {
  const as = agentList.find((a) => a.role === "助理");
  const win = cxWindow(), n = composer.context, pct = win ? Math.min(100, Math.round((n / win) * 100)) : 0;
  const lim = composer.limit ? fmtCtx(composer.limit).toLowerCase() : "";
  const rt = esc(as?.runtime || "");
  const how = hOf(as?.runtime)?.profile?.compact
    ? `快满时（${lim}），${rt} 会自己精简，不用重新开始。`
    : `快满时（${lim}），助理会记下要点，从下一条消息起重新开始。`;
  // 各块是 morula 按发出去、收回来的内容估的，总数是 harness 给的（ctxmix.go）
  const mix = composer.mix, parts = CX_PARTS.map((p) => ({ ...p, n: mix?.[p.k] || 0 }));
  const bar = mix && win
    ? parts.map((p, i) => `<i class="cx-${i}" style="width:${(p.n / win) * 100}%"></i>`).join("")
    : `<i style="width:${pct}%"></i>`;
  const leg = mix
    ? `<div class="cmp-leg">${parts.map((p, i) => `<div><i class="cx-${i}"></i><span>${p.l}</span><b class="mono">${p.n < 1e3 ? p.n : fmtCtx(p.n).toLowerCase()}</b></div>`).join("")}</div>`
    : "";
  $("#cx-menu").innerHTML = `<div class="cmp-cx-h"><b>助理记得的内容</b><span class="mono">${n ? fmtCtx(n).toLowerCase() : "0"} / ${win ? fmtCtx(win).toLowerCase() : "?"} · ${pct}%</span></div>` +
    `<div class="cmp-bar">${bar}</div>` + leg +
    `<p class="cmp-note">${n ? "" : "今天还没开始对话。"}${n && !mix ? "下次重新开始后分块显示。" : ""}${mix ? "各块是估算。" : ""}${how}</p>` +
    (n ? `<div class="pick-sep"></div><div class="cmp-ft"><span id="cx-st"></span><button type="button" class="cmp-link" id="cx-hand">记下要点，重新开始</button></div>` : "");
}
const CX_PARTS = [{ k: "role", l: "职责说明" }, { k: "memory", l: "记忆" }, { k: "chat", l: "对话" }, { k: "tools", l: "查到的资料" }];
$("#cx-btn").addEventListener("click", (e) => {
  e.stopPropagation();
  const open = $("#cx-menu").hidden;
  if (open) cxMenu();
  popOpen("cx", open);
});
$("#cx-menu").addEventListener("click", async (e) => {
  e.stopPropagation();
  const b = e.target.closest("#cx-hand");
  if (!b) return;
  b.disabled = true;
  $("#cx-st").textContent = "助理在记要点…";
  try {
    composer = await api("/api/composer/handoff", { method: "POST" });
    renderComposer();
    cxMenu();
    $("#cx-st").textContent = "已重新开始，下一条消息从记下的要点接着做";
  } catch (err) {
    $("#cx-st").textContent = err.message;
    b.disabled = false;
  }
});
loadComposer();
async function savePick() {
  const box = document.querySelector("#composer .pick");
  const body = { runtime: $("#as-rt").value.split("|")[0], model: $("#as-model").value, effort: $("#as-effort").value };
  box.querySelectorAll("select, button").forEach((x) => (x.disabled = true));
  try {
    renderAgents(await api("/api/agents/" + encodeURIComponent(box.dataset.agent), { method: "PATCH", body: JSON.stringify(body) }));
    const t = pickText();
    $("#chat-state").textContent = `已改用 ${t.model}`;
    checkReady();
  } catch (err) {
    $("#chat-state").textContent = err.message;
    loadAgents();
  } finally { box.querySelectorAll("select, button").forEach((x) => (x.disabled = false)); }
}
$("#as-rt").addEventListener("change", () => { fillPick("", ""); savePick(); });
$("#as-model").addEventListener("change", () => { fillPickEffort(""); savePick(); });
$("#as-effort").addEventListener("change", savePick);
$("#font-seg").addEventListener("change", (e) => {
  applyFont(e.currentTarget.dataset.value);
  api("/api/ui", { method: "POST", body: JSON.stringify({ font: +e.currentTarget.dataset.value }) }).catch(() => {});
});
seg($("#font-seg"), [{ v: "13", l: "小" }, { v: "14", l: "标准" }, { v: "15", l: "大" }, { v: "16", l: "特大" }], store.get("font") || "14");
applyFont(store.get("font"));
api("/api/ui").then((u) => applyFont(u.font)).catch(() => {});
const plugForm = $("#plug-form");
plugForm.addEventListener("submit", async (e) => {
  e.preventDefault();
  const b = $("#plug-submit"), spec = plugForm.spec.value.trim();
  if (!spec) return;
  b.disabled = plugForm.spec.disabled = true;
  b.textContent = "安装中…";
  $("#plug-form-err").textContent = "";
  $("#plug-hint").textContent = `正在下载并加载 ${spec}，可能要几十秒`;
  try {
    setPlug(await api("/api/plugins", { method: "POST", body: JSON.stringify({ spec }) }));
    plugForm.reset();
    $("#plug-hint").textContent = `已装好 ${spec}。到「已安装」看它提供的订阅，点订阅到「账号」登录。`;
  } catch (err) {
    $("#plug-form-err").textContent = err.message;
    $("#plug-hint").textContent = "";
  } finally {
    b.disabled = plugForm.spec.disabled = false;
    b.textContent = "安装";
  }
});
const onPlug = async (e) => {
  const b = e.target.closest("button");
  if (!b) return;
  const d = b.dataset;
  if (d.goProv) return show("providers");
  if (d.pkgAdd) return installPkg(d.pkgAdd, d.pkgAdd);
  if (d.pkgUp) return installPkg(d.pkgUp, d.pkgUp);
  if (d.plugOff) return plugDo(b, "/api/plugins/off", { method: "POST", body: JSON.stringify({ name: d.plugOff, off: d.off === "1" }) });
  if (d.plugRm) {
    if (b.dataset.sure !== "1") { b.dataset.sure = "1"; b.textContent = "确认移除"; setTimeout(() => { b.dataset.sure = ""; b.textContent = "移除"; }, 3000); return; }
    return plugDo(b, "/api/plugins/" + d.plugRm, { method: "DELETE" });
  }
  if (d.apiRm) return plugDo(b, "/api/apikeys/" + encodeURIComponent(d.apiRm), { method: "DELETE" });
  if (d.ckin) return plugDo(b, "/api/plugins/checkin/" + encodeURIComponent(d.ckin), { method: "PUT", body: JSON.stringify({ on: !b.classList.contains("on") }) });
  if (d.ckinRun) return plugDo(b, `/api/plugins/checkin/${encodeURIComponent(d.ckinRun)}/run`, { method: "POST" });
  if (d.signout) return plugDo(b, "/api/plugins/signout", { method: "POST", body: JSON.stringify({ provider: d.signout }) });
  if (d.signCancel) { delete signing[d.signCancel]; return renderProviders(); }
  if (d.signin) {
    const p = plug.providers.find((x) => x.id === d.signin);
    const m = +d.method;
    if ((p.methods[m].prompts || []).length) { signing[p.id] = { method: m, mode: "prompts" }; return renderProviders(); }
    return signIn(p.id, m);
  }
  if (d.signGo) {
    const inputs = {};
    b.closest(".sign").querySelectorAll("[data-prompt]").forEach((x) => (inputs[x.dataset.prompt] = x.value));
    return signIn(d.signGo, signing[d.signGo].method, inputs);
  }
  if (d.signKeyGo) {
    const key = b.closest(".sign").querySelector("[data-sign-key]").value;
    try {
      setPlug(await api("/api/plugins/apikey", { method: "POST", body: JSON.stringify({ provider: d.signKeyGo, key }) }));
      delete signing[d.signKeyGo];
      renderProviders();
    } catch (err) { signing[d.signKeyGo].err = err.message; renderProviders(); }
    return;
  }
  if (d.signCodeGo) {
    const s = signing[d.signCodeGo];
    s.err = "";
    return finish(d.signCodeGo, s, { code: b.closest(".sign").querySelector("[data-sign-code]").value });
  }
  if (b.matches("[data-codex-login]")) {
    b.disabled = true;
    try { await api("/api/codex/login", { method: "POST" }); } catch (err) { $("#prov-err").textContent = err.message; }
    loadPlugins();
    const t = setInterval(async () => { await loadPlugins(); if (!plug.codex?.logging) clearInterval(t); }, 3000);
    return;
  }
  if (d.pick) {
    const p = d.for === "codex" ? codexProv() : plug.providers.find((x) => x.id === d.for);
    const all = (p?.models || []);
    const off = d.pick === "all" ? [] : d.pick === "none" ? all.map((m) => m.id) : all.filter((m) => !m.free).map((m) => m.id);
    return setOff(b, d.for, off);
  }
  if (d.test) {
    const t = (srcTest[d.test] = { model: b.parentElement.querySelector("select").value, busy: true });
    renderProviders();
    try {
      Object.assign(t, await api("/api/sources/test", { method: "POST", body: JSON.stringify({ source: d.test, model: t.model }) }));
    } catch (err) { t.error = err.message; }
    t.busy = false;
    return renderProviders();
  }
};
$("#v-plugins").addEventListener("click", onPlug);
$("#v-providers").addEventListener("click", onPlug);
// 点账号一行（不是按钮）展开详情。
$("#providers").addEventListener("click", (e) => {
  if (e.target.closest("button, input, select, label, .sel")) return;
  const r = e.target.closest(".row.src");
  if (!r) return;
  const id = r.dataset.src;
  if (openSrc.has(id)) return openSrc.delete(id), renderProviders();
  openSrc.add(id);
  renderProviders();
});
$("#providers").addEventListener("change", (e) => {
  const c = e.target.closest("[data-mon]");
  if (!c) return;
  const off = new Set((plug.off || {})[c.dataset.for] || []);
  if (c.checked) off.delete(c.dataset.mon); else off.add(c.dataset.mon);
  setOff(c, c.dataset.for, [...off]);
});
async function setOff(el, id, off) {
  el.disabled = true;
  try {
    setPlug(await api("/api/sources/" + encodeURIComponent(id) + "/off", { method: "PUT", body: JSON.stringify({ off }) }));
  } catch (err) { $("#prov-err").textContent = err.message; renderProviders(); }
}

/* ---------- 版本与更新 ---------- */
const upd = $("#update");
async function checkUpdate() {
  try {
    const r = await api("/api/update");
    if (r.available) {
      upd.hidden = false;
      upd.disabled = false;
      upd.textContent = `更新到 ${r.version} 并重启`;
      upd.title = r.notes || "";
    } else {
      upd.hidden = true;
    }
  } catch {}
}
upd.addEventListener("click", async () => {
  upd.disabled = true;
  upd.textContent = "正在更新…";
  try {
    await api("/api/update", { method: "POST" });
    upd.textContent = "即将重启…";
  } catch (err) {
    upd.disabled = false;
    upd.textContent = "更新失败，点此重试";
    upd.title = err.message;
  }
});

(async () => {
  const v = await api("/api/version");
  $("#ver").textContent = "morula " + v.version + (v.bundle ? "" : " · 开发运行");
  $("#sb-set").title = "设置（⌘,）· " + $("#ver").textContent;
  agentList = await api("/api/agents").catch(() => []); // 先拿到花名册，对话里的头像才和 Agent 页一致
  assignLooks(agentList);
  pollChat();
  checkReady();
  loadAgents();
  api("/api/projects").then((l) => { projectList = l; footer(); }).catch(() => {});
  checkUpdate();
  setInterval(checkUpdate, 60 * 1000);
  window.addEventListener("focus", checkUpdate);
  show(VIEWS.includes(location.hash.slice(1)) ? location.hash.slice(1) : VIEWS.includes(store.get("view")) ? store.get("view") : "chat");
})();

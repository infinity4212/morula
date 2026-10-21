// morula 的插件运行器：在 Bun 里加载 OpenCode 格式的 provider 插件，
// 通过 stdin/stdout 和 morula 对话，一行一条 JSON。
//
// morula → host：{id, method, params}；host 回 {id, result} 或 {id, error}。
// fetch 先回 {id, event: "head", status, headers}，再回若干 {id, event: "chunk", data}（base64），
// 最后 {id, result: null}。{method: "abort", params: {id}} 取消一个 fetch。
// host 主动发：{event: "log", message}（插件打的日志）、{event: "auth", provider}（凭据存下或刷新了）。
//
// 插件拿到的输入与 OpenCode 给的一样（PluginInput）：client 的 auth.set 存进 morula 的
// plugin-auth.json，形状与 OpenCode 的 auth.json 相同；其余 client 调用回 {data: undefined}。
//
// 只实现 morula 用到的钩子：config、auth.methods、auth.loader、auth.usage、auth.refresh、
// auth.checkin、provider.models。每个 provider 只有一个账号。

import fs from "node:fs"
import path from "node:path"
import readline from "node:readline"
import { pathToFileURL } from "node:url"

const write = (msg) => process.stdout.write(JSON.stringify(msg) + "\n")
const errText = (e) => String(e?.message ?? e)
// 插件的 console 输出不能进 stdout，否则会打乱协议
for (const k of ["log", "info", "warn", "error", "debug"]) console[k] = (...a) => process.stderr.write(a.map(String).join(" ") + "\n")

let authPath = ""
let directory = process.cwd()
const hooks = [] // {spec, hooks}
const loaded = [] // {spec, error?}
let config = { provider: {} }

// ---- 凭据 ----------------------------------------------------------------

function readAuth() {
  try {
    return JSON.parse(fs.readFileSync(authPath, "utf8"))
  } catch {
    return {}
  }
}
function setAuth(provider, value) {
  const all = readAuth()
  if (value) all[provider] = value
  else delete all[provider]
  const tmp = authPath + ".tmp"
  fs.writeFileSync(tmp, JSON.stringify(all, null, 2), { mode: 0o600 })
  fs.renameSync(tmp, authPath)
  loaders.delete(provider)
  write({ event: "auth", provider })
}
const authOf = (provider) => readAuth()[provider]

function makeClient() {
  const ok = (data) => ({ data })
  const known = {
    auth: {
      set: async ({ path: p, body } = {}) => {
        if (p?.id) setAuth(p.id, body)
        return ok(true)
      },
    },
    tui: { showToast: async ({ body } = {}) => (write({ event: "log", message: String(body?.message ?? "") }), ok(true)) },
    app: { log: async ({ body } = {}) => (write({ event: "log", message: String(body?.message ?? "") }), ok(true)) },
    config: { get: async () => ok(config) },
  }
  // 插件问到其他接口时回空，不让它抛错
  const loose = (obj) =>
    new Proxy(obj, {
      get(t, k) {
        if (k in t) return typeof t[k] === "object" && t[k] !== null ? loose(t[k]) : t[k]
        if (k === "then") return undefined
        return loose(Object.assign(async () => ok(undefined), {}))
      },
    })
  return loose(known)
}

// ---- 加载 ----------------------------------------------------------------

function readJSON(p) {
  try {
    return JSON.parse(fs.readFileSync(p, "utf8"))
  } catch {
    return null
  }
}

// entryFiles 列出一个包里可能的入口文件，顺序与 OpenCode 一致：
// exports["./server"]、main、exports["."]、字符串形式的 exports，最后是根目录的 index.*
function entryFiles(target) {
  const st = fs.statSync(target, { throwIfNoEntry: false })
  if (!st) throw new Error(`找不到插件：${target}`)
  if (!st.isDirectory()) return [target]
  const pkg = readJSON(path.join(target, "package.json")) ?? {}
  const exp = pkg.exports
  const table = exp && typeof exp === "object" && !Array.isArray(exp) ? exp : {}
  const cond = (e) => (e && typeof e === "object" ? e.import ?? e.default : e) // 条件导出取 import，没有再取 default
  const named = [cond(table["./server"]), pkg.main, cond(table["."]), typeof exp === "string" ? exp : undefined]
  const files = named.filter((f) => typeof f === "string" && f.trim() !== "").map((f) => path.resolve(target, f.trim()))
  for (const name of ["index.mjs", "index.js", "index.ts"]) {
    const full = path.join(target, name)
    if (fs.existsSync(full)) files.push(full)
  }
  if (files.length === 0) throw new Error(`${pkg.name || target} 不是 OpenCode 插件：package.json 没有写入口文件`)
  return Array.from(new Set(files))
}

// pluginFns 取出模块导出的插件函数：v1 格式是 default 导出 {id, server}；
// 其他格式里，每个导出的函数、或带 server 函数的对象各算一个，同一个值只算一次
function pluginFns(mod) {
  const v1 = mod.default
  if (v1 && typeof v1 === "object" && ("server" in v1 || "id" in v1)) return typeof v1.server === "function" ? [v1.server] : []
  const fns = []
  for (const v of new Set(Object.values(mod))) {
    const fn = typeof v === "function" ? v : typeof v?.server === "function" ? v.server : null
    if (fn) fns.push(fn)
  }
  return fns
}

// importPlugin 按顺序试每个入口文件，返回第一个导出了插件函数的文件里的函数
async function importPlugin(target) {
  for (const file of entryFiles(target)) {
    const fns = pluginFns(await import(pathToFileURL(file).href))
    if (fns.length > 0) return fns
  }
  throw new Error("没有导出 OpenCode 插件函数")
}

const OPENCODE_URL = "http://127.0.0.1:4096" // OpenCode 服务的默认地址；插件只读它，morula 不在这里起服务

async function loadPlugins(list) {
  const input = {
    client: makeClient(),
    project: { id: "morula", worktree: directory, vcs: undefined, time: { created: Date.now() } },
    directory,
    worktree: directory,
    serverUrl: new URL(OPENCODE_URL),
    $: Bun.$,
  }
  for (const { spec, target, options } of list) {
    try {
      for (const fn of await importPlugin(target)) hooks.push({ spec, hooks: (await fn(input, options)) ?? {} })
      loaded.push({ spec })
    } catch (e) {
      loaded.push({ spec, error: errText(e) })
    }
  }
  config = { provider: {} }
  for (const { spec, hooks: h } of hooks) {
    if (typeof h.config !== "function") continue
    try {
      await h.config(config)
    } catch (e) {
      write({ event: "log", message: `${spec} 的 config 钩子出错：${errText(e)}` })
    }
  }
}

// auths 是每个 provider 的 auth 钩子；同一个 id 有两个插件时用后加载的那个（与 OpenCode 相同）
function auths() {
  const m = new Map()
  for (const h of hooks) if (h.hooks.auth?.provider) m.set(h.hooks.auth.provider, { spec: h.spec, auth: h.hooks.auth, all: h.hooks })
  return m
}
function hookOf(provider) {
  const a = auths().get(provider)
  if (!a) throw new Error(`没有插件提供 ${provider}`)
  return a
}

// ---- 模型 ----------------------------------------------------------------

function efforts(m) {
  if (m?.variants && typeof m.variants === "object") return Object.keys(m.variants)
  return []
}
function modelRow(id, m) {
  return {
    id: m?.id ?? id,
    name: m?.name ?? id,
    context: m?.limit?.context ?? 0,
    reasoning: !!(m?.reasoning ?? m?.capabilities?.reasoning),
    efforts: efforts(m),
    free: !!m?.free,
    rate: typeof m?.rate === "number" ? m.rate : 0,
  }
}

async function modelsOf(provider) {
  const cfg = config.provider?.[provider] ?? {}
  let models = cfg.models ?? {}
  const a = auths().get(provider)
  const auth = authOf(provider)
  const fn = a?.all?.provider?.id === provider ? a.all.provider.models : null
  if (typeof fn === "function" && auth) {
    try {
      const got = await fn({ id: provider, models }, { auth })
      if (got && typeof got === "object") models = got
    } catch (e) {
      write({ event: "log", message: `${provider} 列模型失败：${e?.message ?? e}` })
    }
  }
  return Object.entries(models).map(([id, m]) => modelRow(id, m))
}

async function providers() {
  const out = []
  for (const [id, a] of auths()) {
    const cfg = config.provider?.[id] ?? {}
    const auth = authOf(id)
    out.push({
      id,
      spec: a.spec,
      name: cfg.name ?? id,
      npm: cfg.npm ?? "",
      api: cfg.api ?? "",
      methods: (a.auth.methods ?? []).map((m) => ({
        type: m.type,
        label: m.label ?? m.type,
        prompts: (m.prompts ?? []).map((p) => ({ type: p.type, key: p.key, message: p.message, placeholder: p.placeholder, options: p.options })),
      })),
      usage: typeof a.auth.usage === "function",
      checkin: typeof a.auth.checkin === "function",
      signedIn: !!auth,
      authType: auth?.type ?? "",
      account: auth?.accountId ?? "",
      models: await modelsOf(id),
    })
  }
  return out
}

// ---- 登录 ----------------------------------------------------------------

const sessions = new Map() // 会话 id → {provider, cb}
let nextSession = 1

// saved 把 OpenCode 回调的结果存成 auth.json 里的一条
function saved(provider, r, type) {
  if (!r || r.type !== "success") throw new Error(r?.error || "登录失败")
  const { type: _t, provider: _p, ...rest } = r
  const value = "key" in rest && !("access" in rest) ? { type: "api", key: rest.key } : { type, ...rest }
  setAuth(r.provider ?? provider, value)
  return { ok: true }
}

async function authorize({ provider, method, inputs }) {
  const m = hookOf(provider).auth.methods?.[method]
  if (!m) throw new Error("没有这个登录方式")
  if (m.type === "api") {
    if (typeof m.authorize !== "function") return { mode: "key" }
    return { mode: "done", ...saved(provider, await m.authorize(inputs ?? {}), "api") }
  }
  const r = await m.authorize(inputs ?? {})
  const id = String(nextSession++)
  sessions.set(id, { provider, cb: r.callback })
  return { session: id, url: r.url ?? "", instructions: r.instructions ?? "", mode: r.method === "code" ? "code" : "auto" }
}

async function callback({ session, code }) {
  const s = sessions.get(session)
  if (!s) throw new Error("登录会话已过期，请重新登录")
  sessions.delete(session)
  const r = code !== undefined && code !== null ? await s.cb(code) : await s.cb()
  return saved(s.provider, r, "oauth")
}

function apiKey({ provider, key }) {
  hookOf(provider)
  if (!String(key ?? "").trim()) throw new Error("key 是空的")
  setAuth(provider, { type: "api", key: String(key).trim() })
  return { ok: true }
}

// ---- 请求 ----------------------------------------------------------------

const loaders = new Map() // provider → loader 给出的选项

// fresh 在凭据快到期时用插件的 refresh 钩子换新（OpenCode 没有这个钩子，OpenCode 插件自己在 fetch 里刷新）
async function fresh(provider) {
  const a = hookOf(provider).auth
  const auth = authOf(provider)
  if (!auth || typeof a.refresh !== "function" || !auth.expires) return auth
  const lead = a.refreshLead ?? 60_000
  if (auth.expires - Date.now() > lead) return auth
  const r = await a.refresh(auth)
  if (r && typeof r === "object") {
    const next = r.type === "success" ? (({ type, ...x }) => ({ type: auth.type, ...x }))(r) : { ...auth, ...r }
    setAuth(provider, next)
    return next
  }
  return auth
}

async function loaderOf(provider) {
  if (loaders.has(provider)) return loaders.get(provider)
  const a = hookOf(provider).auth
  await fresh(provider)
  const getAuth = async () => authOf(provider)
  const cfg = config.provider?.[provider] ?? {}
  const opts = (typeof a.loader === "function" ? await a.loader(getAuth, { id: provider, ...cfg, models: cfg.models ?? {} }) : {}) ?? {}
  const auth = authOf(provider)
  const out = {
    baseURL: opts.baseURL ?? cfg.options?.baseURL ?? cfg.api ?? "",
    apiKey: opts.apiKey ?? (auth?.type === "api" ? auth.key : ""),
    headers: opts.headers ?? {},
    fetch: typeof opts.fetch === "function" ? opts.fetch : null,
  }
  loaders.set(provider, out)
  return out
}

const inflight = new Map() // 请求 id → AbortController

async function doFetch(id, p) {
  const ctl = new AbortController()
  inflight.set(id, ctl)
  try {
    const l = await loaderOf(p.provider)
    if (!l.baseURL) throw new Error(`${p.provider} 没有给出接口地址`)
    const url = l.baseURL.replace(/\/+$/, "") + p.path
    const headers = new Headers(p.headers ?? {})
    for (const [k, v] of Object.entries(l.headers)) headers.set(k, v)
    if (l.apiKey && !headers.has("authorization")) headers.set("authorization", `Bearer ${l.apiKey}`)
    const body = p.body ? Buffer.from(p.body, "base64") : undefined
    const init = { method: p.method ?? "POST", headers, body, signal: ctl.signal }
    const res = l.fetch ? await l.fetch(url, init) : await fetch(url, init)
    const hs = {}
    res.headers.forEach((v, k) => (hs[k] = v))
    write({ id, event: "head", status: res.status, headers: hs })
    if (res.body) {
      for await (const chunk of res.body) {
        if (ctl.signal.aborted) break
        write({ id, event: "chunk", data: Buffer.from(chunk).toString("base64") })
      }
    }
    write({ id, result: null })
  } catch (e) {
    write({ id, error: { message: String(e?.message ?? e) } })
  } finally {
    inflight.delete(id)
  }
}

// ---- 循环 ----------------------------------------------------------------

const handlers = {
  async init(p) {
    authPath = p.authPath
    directory = p.directory ?? directory
    await loadPlugins(p.plugins ?? [])
    return { plugins: loaded }
  },
  providers,
  authorize,
  callback,
  apiKey,
  signOut: ({ provider }) => (setAuth(provider, null), { ok: true }),
  async usage({ provider }) {
    const a = hookOf(provider).auth
    if (typeof a.usage !== "function") return null
    await fresh(provider)
    return await a.usage(async () => authOf(provider), { id: provider })
  },
  // checkin 每日签到钩子：回 {outcome: claimed|done|inactive|failed, credit?, message?, signIn?}
  async checkin({ provider }) {
    const a = hookOf(provider).auth
    if (typeof a.checkin !== "function") throw new Error(`${provider} 没有每日签到`)
    if (!authOf(provider)) throw new Error(`${provider} 还没登录`)
    await fresh(provider)
    return await a.checkin(async () => authOf(provider), { id: provider })
  },
}

let ready = null // init 的 Promise；其他方法都等它完成

// onLine 处理 morula 发来的一行。abort 立即生效；init 之外的方法排在 init 后面；
// fetch 的结果由 doFetch 分几条写回，其余方法写回一条 {id, result} 或 {id, error}
function onLine(line) {
  let msg
  try {
    msg = JSON.parse(line)
  } catch {
    return // 空行和不是 JSON 的行不理
  }
  const id = msg.id
  const method = msg.method
  const params = msg.params ?? {}
  const fail = (e) => write({ id, error: { message: errText(e) } })
  if (method === "abort") {
    inflight.get(params.id)?.abort()
    return
  }
  if (method === "init") {
    ready = handlers.init(params)
    ready.then((result) => write({ id, result }), fail)
    return
  }
  if (method !== "fetch" && !handlers[method]) {
    write({ id, error: { message: `没有这个方法：${method}` } })
    return
  }
  const after = ready ?? Promise.reject(new Error("插件运行器还没初始化"))
  if (method === "fetch") {
    after.then(() => doFetch(id, params), fail)
    return
  }
  after.then(() => handlers[method](params)).then((result) => write({ id, result: result ?? null }), fail)
}

readline.createInterface({ input: process.stdin, terminal: false }).on("line", onLine).on("close", () => process.exit(0))

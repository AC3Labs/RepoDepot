// The Repo Depot — page behaviour (Alpine.js components and helpers).
"use strict";

const csrf = () => (document.querySelector('meta[name="csrf"]') || {}).content || "";

async function api(method, url, body) {
  const opts = { method, headers: { Accept: "application/json", "X-CSRF-Token": csrf() } };
  if (body !== undefined) { opts.headers["Content-Type"] = "application/json"; opts.body = JSON.stringify(body); }
  const res = await fetch(url, opts);
  let data = null;
  try { data = await res.json(); } catch (e) { /* empty */ }
  if (res.status === 401) { location.href = "/login?next=" + encodeURIComponent(location.pathname + location.search); throw new Error("Signed out"); }
  if (!res.ok) throw new Error((data && data.message) || "Request failed (" + res.status + ")");
  return data;
}

function toast(text, kind, sub) { Alpine.store("toasts").add(text, kind || "ok", sub); }

document.addEventListener("alpine:init", () => {
  Alpine.store("toasts", {
    list: [], n: 0,
    add(text, kind, sub) {
      const id = ++this.n;
      this.list.push({ id, text, kind, sub });
      setTimeout(() => this.remove(id), kind === "bad" ? 9000 : 4500);
    },
    remove(id) { this.list = this.list.filter(t => t.id !== id); },
  });
});

// ---- formatting ----

const esc = s => String(s == null ? "" : s).replace(/[&<>"']/g, c => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
const title = s => s ? s[0].toUpperCase() + s.slice(1) : "";
const osName = os => ({ linux: "Linux", windows: "Windows", darwin: "macOS" }[os] || os || "Unknown");

function size(n) {
  if (!n) return "";
  const u = ["B", "KB", "MB", "GB"]; let i = 0;
  while (n >= 1024 && i < u.length - 1) { n /= 1024; i++; }
  return (i ? n.toFixed(1) : n) + " " + u[i];
}

function fmtDate(s) {
  if (!s) return "";
  const d = new Date(s.includes("T") ? s : s.replace(" ", "T") + "Z");
  if (isNaN(d)) return s;
  const diff = (Date.now() - d) / 1000;
  if (diff < 60) return "just now";
  if (diff < 3600) return Math.floor(diff / 60) + " min ago";
  if (diff < 86400) return Math.floor(diff / 3600) + " h ago";
  if (diff < 86400 * 7) return Math.floor(diff / 86400) + " d ago";
  return d.toLocaleDateString(undefined, { year: "numeric", month: "short", day: "numeric" });
}

const ICONS = {
  linux: '<svg viewBox="0 0 24 24" width="16" height="16" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><path d="M12 3c-2.5 0-3.5 2-3.5 4.5 0 1.5-.5 2.5-1.5 4S5 15 5.5 17c.3 1.2 1.5 1.7 2.5 1.5M12 3c2.5 0 3.5 2 3.5 4.5 0 1.5.5 2.5 1.5 4s2 3.5 1.5 5.5c-.3 1.2-1.5 1.7-2.5 1.5"/><path d="M8 18.5c1 1.5 2.5 2.5 4 2.5s3-1 4-2.5"/></svg>',
  windows: '<svg viewBox="0 0 24 24" width="16" height="16" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linejoin="round"><path d="M3 5.5 10.5 4.5v7H3zM13 4.2 21 3v8.5h-8zM3 13h7.5v7L3 19zM13 13h8v8l-8-1.2z"/></svg>',
  unknown: '<svg viewBox="0 0 24 24" width="16" height="16" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><rect x="3" y="4" width="18" height="7" rx="2"/><rect x="3" y="13" width="18" height="7" rx="2"/></svg>',
  compose: '<svg viewBox="0 0 24 24" width="19" height="19" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M3 13h17c1 0 1.6-1 2-2-1-.5-2-.3-2.6.3C19 9 18 8 17 8c-.4 1.3-.2 2.3.3 3"/><path d="M3 13c0 4 3 7 8 7 6 0 9-3 10-7"/><path d="M5 10h3v3H5zM8 10h3v3H8zM11 10h3v3h-3zM8 7h3v3H8z"/></svg>',
  release: '<svg viewBox="0 0 24 24" width="19" height="19" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M21 8 12 3 3 8v8l9 5 9-5z"/><path d="m3 8 9 5 9-5M12 13v8"/></svg>',
  source: '<svg viewBox="0 0 24 24" width="19" height="19" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="m8 7-5 5 5 5M16 7l5 5-5 5M14 4l-4 16"/></svg>',
  track: '<svg viewBox="0 0 24 24" width="19" height="19" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M2 12s3.5-7 10-7 10 7 10 7-3.5 7-10 7S2 12 2 12z"/><circle cx="12" cy="12" r="3"/></svg>',
};
const osIcon = os => ICONS[os] || ICONS.unknown;
const methodIcon = m => ICONS[m] || ICONS.source;
const methodLabel = m => ({ compose: "Docker Compose", release: "Release", source: "Source", track: "Track only" }[m] || m);

function busy(a) { return ["installing", "updating", "uninstalling"].includes(a.status) || a.active_job > 0; }

function statusPill(a) {
  const map = {
    installing: ["info busy", "Installing"], updating: ["info busy", "Updating"], uninstalling: ["warn busy", "Removing"],
    error: ["bad", "Problem"], installed: ["ok", "Installed"],
  };
  let [cls, label] = map[a.status] || ["muted", a.status];
  if (a.status === "installed" && a.update_available) [cls, label] = ["warn", "Update available"];
  if (a.status === "installed" && a.held) [cls, label] = ["muted", "Updates on hold"];
  if (a.status === "installed" && a.machine_status === "offline") [cls, label] = ["muted", "Machine offline"];
  return '<span class="pill ' + cls + '">' + esc(label) + "</span>";
}

function jobPill(j) {
  const [cls, label] = { running: ["info busy", "Running"], done: ["ok", "Done"], error: ["bad", "Failed"] }[j.status] || ["muted", j.status];
  return '<span class="pill ' + cls + '">' + label + "</span>";
}

function colorLog(text) {
  return esc(text || "").split("\n").map(l => {
    if (l.startsWith("$ ")) return '<span class="cmd">' + l + "</span>";
    if (l.startsWith("✓")) return '<span class="ok">' + l + "</span>";
    if (l.startsWith("✗")) return '<span class="bad">' + l + "</span>";
    return l;
  }).join("\n");
}

const LANG = { Go: "#00ADD8", Python: "#3572A5", JavaScript: "#f1e05a", TypeScript: "#3178c6", Rust: "#dea584", "C++": "#f34b7d", C: "#555555",
  "C#": "#178600", Java: "#b07219", PHP: "#4F5D95", Ruby: "#701516", Shell: "#89e051", Kotlin: "#A97BFF", Swift: "#F05138", Dart: "#00B4AB",
  Vue: "#41b883", Svelte: "#ff3e00", HTML: "#e34c26", CSS: "#563d7c", Lua: "#000080", Elixir: "#6e4a7e", Zig: "#ec915c", Nix: "#7e7eff" };
const langColor = l => LANG[l] || "#8b949e";

async function copyText(t) {
  try { await navigator.clipboard.writeText(t); toast("Copied"); }
  catch (e) {
    const ta = document.createElement("textarea"); ta.value = t; document.body.appendChild(ta); ta.select();
    document.execCommand("copy"); ta.remove(); toast("Copied");
  }
}

// ---- shell: sidebar, global search, update badge ----

function shell(updates) {
  return {
    updates, menu: false,
    init() { setInterval(() => this.poll(), 30000); },
    async poll() { try { this.updates = (await api("GET", "/api/status")).updates; } catch (e) { } },
    go(q) { location.href = "/discover" + (q ? "?q=" + encodeURIComponent(q) : ""); },
    hotkey(e) {
      const typing = /INPUT|TEXTAREA|SELECT/.test(document.activeElement.tagName);
      if (e.key === "/" && !typing) {
        const big = document.querySelector(".searchbox.big input");
        (big || this.$refs.q).focus(); e.preventDefault();
      }
    },
  };
}

// ---- My Depot ----

function depot(apps, machines, view, lastCheck) {
  return {
    apps, machines, view: localStorage.getItem("depot-view") || view, lastCheck, checking: false,
    q: "", machine: "", filter: "all", selected: {}, timer: null,
    get updatable() { return this.apps.filter(a => a.update_available && !busy(a) && a.method !== "track"); },
    get shown() {
      const q = this.q.toLowerCase();
      return this.apps.filter(a =>
        (!q || (a.repo + " " + a.machine_name + " " + a.description).toLowerCase().includes(q)) &&
        (!this.machine || String(a.machine_id) === String(this.machine)) &&
        (this.filter !== "updates" || a.update_available) &&
        (this.filter !== "problems" || a.status === "error" || a.check_error || a.machine_status === "offline"));
    },
    start() { this.schedule(); },
    schedule() {
      clearTimeout(this.timer);
      const fast = this.apps.some(busy) || this.checking;
      this.timer = setTimeout(() => this.refresh(), fast ? 1500 : 20000);
    },
    async refresh() {
      try {
        const r = await api("GET", "/api/apps");
        const before = Object.fromEntries(this.apps.map(a => [a.id, a]));
        for (const a of r.apps) {
          const b = before[a.id];
          if (b && busy(b) && !busy(a)) {
            if (a.status === "error") toast("Couldn't finish " + a.name + " on " + a.machine_name, "bad", "See its Activity tab for the log.");
            else toast(a.name + " on " + a.machine_name + " is " + (a.installed_label || "ready"));
          }
        }
        this.apps = r.apps; this.lastCheck = r.last_check;
      } catch (e) { }
      this.schedule();
    },
    lastCheckLabel() { return this.lastCheck ? fmtDate(new Date(this.lastCheck * 1000).toISOString()) : "Not yet"; },
    setView(v) { this.view = v; try { localStorage.setItem("depot-view", v); } catch (e) { } },
    allSelected() { return this.shown.length > 0 && this.shown.every(a => this.selected[a.id]); },
    selectAll(on) { for (const a of this.shown) this.selected[a.id] = on; },
    selectedUpdatable() { return this.apps.filter(a => this.selected[a.id] && a.update_available && !busy(a) && a.method !== "track").map(a => a.id); },
    async checkNow() {
      this.checking = true;
      try {
        const r = await api("POST", "/api/check");
        toast(r.updates ? r.updates + (r.updates === 1 ? " update" : " updates") + " available" : "Everything is up to date", r.updates ? "info" : "ok");
      } catch (e) { toast(e.message, "bad"); }
      this.checking = false; this.refresh();
    },
    async updateOne(a) {
      try { await api("POST", "/api/apps/" + a.id + "/update", {}); a.status = "updating"; toast("Updating " + a.name, "info"); this.schedule(); }
      catch (e) { toast(e.message, "bad"); }
    },
    async markUpdated(a) {
      try { await api("POST", "/api/apps/" + a.id + "/mark", {}); toast(a.name + " marked as " + a.latest_label); this.refresh(); }
      catch (e) { toast(e.message, "bad"); }
    },
    async updateIds(ids) {
      try { const r = await api("POST", "/api/apps/update", { ids }); this.reportMany(r); } catch (e) { toast(e.message, "bad"); }
    },
    async updateAll() {
      try { const r = await api("POST", "/api/apps/update", { all: true }); this.reportMany(r); } catch (e) { toast(e.message, "bad"); }
    },
    reportMany(r) {
      if (r.started) toast("Updating " + r.started + (r.started === 1 ? " app" : " apps"), "info");
      for (const f of r.failed || []) toast(f, "bad");
      this.selected = {}; this.refresh();
    },
  };
}

// ---- Discover ----

function discover(initial) {
  return {
    q: initial || "", sort: "", page: 1, items: [], total: 0, loading: false, error: "", cursor: -1, ctl: null, prefetched: {}, stars: false,
    suggestions: [["Self-hosted", "topic:self-hosted"], ["Media server", "media server"], ["Home automation", "home automation"],
      ["Dashboards", "topic:dashboard self-hosted"], ["Backups", "backup tool"], ["Monitoring", "topic:monitoring"],
      ["Password managers", "password manager self-hosted"], ["CLI tools", "topic:cli"]],
    start() {
      const p = new URLSearchParams(location.search);
      this.sort = p.get("sort") || "";
      this.search(1);
      this.$nextTick(() => this.$refs.input.focus());
    },
    setSort(s) { this.sort = s; this.search(1); },
    key(page) { return "rd-search|" + this.q.trim() + "|" + this.sort + "|" + page; },
    async loadStars(page) {
      if (this.ctl) this.ctl.abort();
      this.stars = true; this.q = ""; this.loading = true; this.error = "";
      try {
        const r = await api("GET", "/api/starred?page=" + page);
        this.items = page === 1 ? r.items : this.items.concat(r.items);
        this.page = page; this.total = this.items.length + (r.more ? 1 : 0); this.cursor = -1;
      } catch (e) { this.error = e.message; this.items = []; }
      this.loading = false;
    },
    async search(page) {
      this.stars = false;
      const q = this.q.trim();
      const url = new URL(location); q ? url.searchParams.set("q", q) : url.searchParams.delete("q");
      this.sort ? url.searchParams.set("sort", this.sort) : url.searchParams.delete("sort");
      history.replaceState(null, "", url);
      const k = this.key(page);
      let cached = null;
      try { cached = JSON.parse(sessionStorage.getItem(k) || "null"); } catch (e) { }
      if (cached) { this.apply(cached, page); return; }
      if (this.ctl) this.ctl.abort();
      this.ctl = new AbortController();
      this.loading = true; this.error = "";
      if (page === 1) this.cursor = -1;
      try {
        const res = await fetch("/api/search?" + new URLSearchParams({ q, sort: this.sort, page }), { headers: { Accept: "application/json" }, signal: this.ctl.signal });
        const data = await res.json();
        if (!res.ok) throw new Error(data.message || "Search failed");
        try { sessionStorage.setItem(k, JSON.stringify(data)); } catch (e) { }
        this.apply(data, page);
        if (data.exact) location.href = "/repo/" + data.exact;
      } catch (e) {
        if (e.name !== "AbortError") { this.error = e.message; this.items = []; }
      }
      this.loading = false;
    },
    apply(data, page) {
      this.total = data.total; this.page = page;
      this.items = page === 1 ? data.items : this.items.concat(data.items);
    },
    prefetch(r) {
      if (this.prefetched[r.full_name]) return;
      this.prefetched[r.full_name] = true;
      fetch("/api/repo/" + r.full_name + "/inspect", { headers: { Accept: "application/json" } }).catch(() => { });
    },
    keys(e) {
      if (!this.items.length) return;
      if (e.key === "ArrowDown") { this.cursor = Math.min(this.cursor + 1, this.items.length - 1); e.preventDefault(); this.scrollTo(); }
      else if (e.key === "ArrowUp") { this.cursor = Math.max(this.cursor - 1, 0); e.preventDefault(); this.scrollTo(); }
      else if (e.key === "Enter" && this.cursor >= 0) { location.href = "/repo/" + this.items[this.cursor].full_name; }
    },
    scrollTo() { this.$nextTick(() => { const el = document.querySelector(".result.kb"); el && el.scrollIntoView({ block: "nearest" }); }); },
  };
}

// ---- repo page & install dialog ----

function repoPage(full, machines, defaults) {
  const [owner, name] = full.split("/");
  return {
    owner, name, machines, defaults, info: null, readme: null, error: "", tab: "readme", dlg: false, busy: false, composeText: "",
    fits: [], logo: "", form: {},
    get repo() { return this.info ? this.info.inspect.repo : {}; },
    async start() {
      const branch = "";
      const [ins, rd] = await Promise.allSettled([
        api("GET", "/api/repo/" + full + "/inspect"),
        api("GET", "/api/repo/" + full + "/readme" + (branch ? "?branch=" + branch : "")),
      ]);
      if (ins.status === "rejected") { this.error = ins.reason.message; return; }
      this.info = ins.value; this.fits = this.info.machines; this.logo = this.info.logo;
      document.title = this.repo.full_name + " · The Repo Depot";
      this.readme = rd.status === "fulfilled" ? rd.value.html : "";
      if (!this.info.logo.startsWith("/logos/")) setTimeout(() => this.refreshLogo(), 2500);
      if (new URLSearchParams(location.search).get("install")) this.openInstall();
    },
    async refreshLogo() {
      try { const r = await api("GET", "/api/repo/" + full + "/inspect"); if (r.logo !== this.logo) this.logo = r.logo; } catch (e) { }
    },
    openInstall() {
      if (!this.info) return;
      const i = this.info.inspect;
      const track = this.defaults.track === "auto" ? i.default_track : this.defaults.track;
      this.form = {
        machines: [], method: i.recommended, track: (track === "release" && !i.latest) ? "commit" : track, branch: "", version: "",
        dirs: {}, assets: {}, auto_update: this.defaults.auto_update, notify: this.defaults.notify,
        options: { compose_file: (i.compose_files || [])[0] || "", compose_override: "", env: "", post_install: "",
          generated_compose: (i.compose_files && i.compose_files.length) ? "" : (i.has_dockerfile ? this.info.generated_compose : ""),
          link_binary: this.defaults.link_binaries, service: false, service_exec: "", service_args: "" },
      };
      for (const m of this.fits) { this.form.dirs[m.id] = m.dir; this.form.assets[m.id] = m.asset; }
      const ok = this.fits.filter(m => this.usable(m));
      if (ok.length === 1) this.form.machines = [ok[0].id];
      this.methodChanged();
      this.dlg = true;
    },
    usable(m) {
      if (this.form.method === "track") return true; // installed already; nothing is sent to it
      if (m.status !== "online" || (m.reason && m.reason.all)) return false;
      return !(m.reason && m.reason[this.form.method]);
    },
    whyNot(m) {
      if (this.form.method === "track") return "";
      if (m.status !== "online") return "Offline";
      if (m.reason && m.reason.all) return m.reason.all;
      return (m.reason && m.reason[this.form.method]) || "";
    },
    methodChanged() {
      if (this.form.method === "release") this.form.track = "release";
      if (this.form.method === "track") { for (const m of this.fits) this.form.dirs[m.id] = ""; }
      else { for (const m of this.fits) if (!this.form.dirs[m.id]) this.form.dirs[m.id] = m.dir; }
      this.form.machines = this.form.machines.filter(id => this.usable(this.fits.find(f => f.id === id)));
      this.composeText = "";
    },
    async viewCompose() {
      if (this.composeText) { this.composeText = ""; return; }
      try { this.composeText = (await api("GET", "/api/repo/" + full + "/file?path=" + encodeURIComponent(this.form.options.compose_file))).text; }
      catch (e) { toast(e.message, "bad"); }
    },
    summary() {
      if (!this.info) return "";
      const t = { release: "releases", tag: "tags", commit: "commits" }[this.form.track];
      if (this.form.method === "track") return "Track only · updates from " + t;
      return methodLabel(this.form.method) + " · updates from " + t + (this.form.auto_update ? " · automatic" : "");
    },
    async install() {
      this.busy = true;
      const f = this.form;
      const body = { repo: full, machines: f.machines, method: f.method, track: f.track, branch: f.branch, version: f.version,
        dirs: Object.fromEntries(f.machines.map(id => [String(id), f.dirs[id]])),
        assets: Object.fromEntries(f.machines.map(id => [String(id), f.assets[id] || ""])),
        options: f.options, auto_update: f.auto_update, notify: f.notify };
      try {
        const r = await api("POST", "/api/install", body);
        const ok = r.results.filter(x => !x.error);
        for (const x of r.results.filter(x => x.error)) toast(x.machine + ": " + x.error, "bad");
        if (ok.length && f.method === "track") {
          toast("Tracking " + name + " on " + ok.map(x => x.machine).join(", "));
          setTimeout(() => { location.href = ok.length === 1 ? "/apps/" + ok[0].app_id : "/"; }, 400);
        } else if (ok.length) {
          toast("Installing on " + ok.map(x => x.machine).join(", "), "info");
          setTimeout(() => { location.href = ok.length === 1 ? "/apps/" + ok[0].app_id : "/"; }, 400);
        }
      } catch (e) { toast(e.message, "bad"); }
      this.busy = false;
    },
  };
}

// ---- app page ----

function appPage(app, jobs) {
  return {
    a: app, jobs, tab: "activity", open: {}, removeData: false, saving: false, timer: null, changes: null, changesError: "", versions: null,
    s: { track: app.track, branch: app.branch, auto_update: app.auto_update, notify: app.notify, held: app.held,
      options: Object.assign({ prerelease: "", ignore_versions: "" }, app.options),
      installed: app.installed_ref, location: app.location || app.dir },
    start() {
      if (this.jobs.length) this.open[this.jobs[0].id] = true;
      const t = new URLSearchParams(location.search).get("tab");
      if (t === "changes") this.openChanges(); else if (t === "versions") this.openVersions(); else if (t) this.tab = t;
      this.schedule();
    },
    schedule() {
      clearTimeout(this.timer);
      this.timer = setTimeout(() => this.refresh(), busy(this.a) ? 1200 : 15000);
    },
    async refresh() {
      try {
        const [apps, jobs] = await Promise.all([api("GET", "/api/apps"), api("GET", "/api/jobs?app=" + this.a.id)]);
        const me = apps.apps.find(x => x.id === this.a.id);
        if (!me) { toast(this.a.name + " was removed"); location.href = "/"; return; }
        const was = busy(this.a);
        this.a = me; this.jobs = jobs.jobs;
        if (this.jobs.length && busy(me)) this.open[this.jobs[0].id] = true;
        if (was && !busy(me)) toast(me.status === "error" ? "Something went wrong; see the log" : me.name + " is " + me.installed_label, me.status === "error" ? "bad" : "ok");
      } catch (e) { }
      this.schedule();
    },
    toggle(j) { this.open[j.id] = !this.open[j.id]; },
    async run(path, body, msg) {
      try { await api("POST", "/api/apps/" + this.a.id + path, body || {}); this.a.status = "updating"; this.tab = "activity"; toast(msg, "info"); this.refresh(); }
      catch (e) { toast(e.message, "bad"); }
    },
    update() { this.run("/update", {}, "Updating " + this.a.name); },
    async openChanges() {
      this.tab = "changes";
      if (this.changes && this.changes.for === this.a.latest_ref) return;
      this.changes = null; this.changesError = "";
      try { const r = await api("GET", "/api/apps/" + this.a.id + "/changes"); r.for = this.a.latest_ref; this.changes = r; }
      catch (e) { this.changes = { releases: [], commits: [] }; this.changesError = e.message; }
    },
    async openVersions() {
      this.tab = "versions";
      if (this.versions) return;
      try { this.versions = (await api("GET", "/api/apps/" + this.a.id + "/versions")).versions; }
      catch (e) { this.versions = []; toast(e.message, "bad"); }
    },
    async installVersion(v) {
      if (!confirm("Put " + this.a.name + " on " + this.a.machine_name + " on " + v.label + "?")) return;
      try {
        const r = await api("POST", "/api/apps/" + this.a.id + "/update", { ref: v.ref });
        this.a.status = "updating"; this.tab = "activity"; this.versions = null;
        toast("Installing " + v.label, "info", r.skipped ? r.skipped + " is skipped until you offer it again." : "");
        this.refresh();
      } catch (e) { toast(e.message, "bad"); }
    },
    async rollback() {
      if (!confirm("Roll " + this.a.name + " back to " + this.a.previous_label + "?")) return;
      try {
        const r = await api("POST", "/api/apps/" + this.a.id + "/update", { ref: this.a.previous_ref });
        this.a.status = "updating"; this.tab = "activity";
        toast("Rolling back to " + this.a.previous_label, "info", r.skipped ? r.skipped + " is skipped until you offer it again." : "");
        this.refresh();
      } catch (e) { toast(e.message, "bad"); }
    },
    async skip() {
      try { await api("POST", "/api/apps/" + this.a.id + "/skip", {}); toast("Skipped " + this.a.latest_label); this.refresh(); }
      catch (e) { toast(e.message, "bad"); }
    },
    async unskip() {
      try { await api("POST", "/api/apps/" + this.a.id + "/skip", { undo: true }); toast(this.a.latest_label + " is offered again"); this.refresh(); }
      catch (e) { toast(e.message, "bad"); }
    },
    async setHeld(on) {
      this.s.held = on;
      try { await api("POST", "/api/apps/" + this.a.id + "/settings", this.s); toast(on ? "Updates on hold" : "Updates resumed"); this.refresh(); }
      catch (e) { toast(e.message, "bad"); }
    },
    async findLogo() {
      try { const r = await api("POST", "/api/apps/" + this.a.id + "/logo"); this.a.logo_url = r.logo_url + "?t=" + Date.now(); toast("Logo updated"); }
      catch (e) { toast(e.message, "bad"); }
    },
    async markUpdated() {
      try { await api("POST", "/api/apps/" + this.a.id + "/mark", {}); toast(this.a.name + " marked as " + this.a.latest_label); this.refresh(); }
      catch (e) { toast(e.message, "bad"); }
    },
    reinstall() { this.run("/reinstall", {}, "Reinstalling " + this.a.name); },
    async uninstall() {
      if (!confirm("Uninstall " + this.a.name + " from " + this.a.machine_name + (this.removeData ? " and delete its data" : "") + "?")) return;
      try { await api("POST", "/api/apps/" + this.a.id + "/uninstall", { remove_data: this.removeData }); this.a.status = "uninstalling"; this.tab = "activity"; toast("Uninstalling " + this.a.name, "info"); this.refresh(); }
      catch (e) { toast(e.message, "bad"); }
    },
    async unregister() {
      if (!confirm("Stop tracking " + this.a.name + "? It stays installed on " + this.a.machine_name + ".")) return;
      try { await api("POST", "/api/apps/" + this.a.id + "/unregister"); location.href = "/"; } catch (e) { toast(e.message, "bad"); }
    },
    async saveSettings(reinstall) {
      this.saving = true;
      try {
        await api("POST", "/api/apps/" + this.a.id + "/settings", this.s);
        toast("Saved");
        if (reinstall) this.reinstall(); else this.refresh();
      } catch (e) { toast(e.message, "bad"); }
      this.saving = false;
    },
  };
}

// ---- machines ----

// newerVersion: is version a newer than b? (numbers compared in order)
function newerVersion(a, b) {
  const x = (a.match(/\d+/g) || []).map(Number), y = (b.match(/\d+/g) || []).map(Number);
  for (let i = 0; i < Math.max(x.length, y.length); i++) {
    if ((x[i] || 0) !== (y[i] || 0)) return (x[i] || 0) > (y[i] || 0);
  }
  return false;
}

function machinesPage(list, port, welcome, version) {
  return {
    list, port, welcome, version, state: {}, apps: {}, seen: {}, dlg: false, created: false, saving: false, waiting: false, online: false,
    form: {}, cmds: {}, poll: null,
    start() {
      for (const m of this.list) this.check(m);
      if (this.welcome && this.list.length === 0) this.openAdd();
    },
    async check(m) {
      this.state[m.id] = "checking";
      try {
        const r = await api("GET", "/api/machines/" + m.id + "/status");
        Object.assign(m, r.machine); this.apps[m.id] = r.apps; this.seen[m.id] = r.last_seen;
      } catch (e) { m.status = "offline"; }
      this.state[m.id] = "";
    },
    openAdd() {
      this.form = { id: 0, name: "", host: "", port: this.port, use_tls: false, token: "" };
      this.created = false; this.online = false; this.dlg = true;
      this.$nextTick(() => this.$refs.name && this.$refs.name.focus());
    },
    openEdit(m) {
      this.form = { id: m.id, name: m.name, host: m.host, port: m.port, use_tls: m.use_tls, token: "", apps: this.apps[m.id] };
      this.created = false; this.dlg = true;
    },
    async save() {
      this.saving = true;
      try {
        const r = await api("POST", "/api/machines" + (this.form.id ? "/" + this.form.id : ""), this.form);
        if (this.form.id) {
          const i = this.list.findIndex(x => x.id === this.form.id); this.list[i] = r.machine; this.dlg = false; toast("Saved"); this.check(r.machine);
        } else {
          this.list.push(r.machine); this.cmds = r.commands; this.created = true; this.waitOnline(r.machine);
        }
      } catch (e) { toast(e.message, "bad"); }
      this.saving = false;
    },
    async newToken() {
      try { const r = await api("POST", "/api/machines/" + this.form.id + "/token"); this.cmds = r.commands; this.created = true; this.waitOnline(this.list.find(x => x.id === this.form.id)); }
      catch (e) { toast(e.message, "bad"); }
    },
    waitOnline(m) {
      this.waiting = true; this.online = false; clearInterval(this.poll);
      this.poll = setInterval(async () => {
        if (!this.dlg) { clearInterval(this.poll); this.waiting = false; return; }
        await this.check(m);
        if (m.status === "online") { this.online = true; this.waiting = false; clearInterval(this.poll); toast(m.name + " is connected"); }
      }, 2500);
    },
    async remove() {
      if (!confirm("Delete " + this.form.name + " from The Repo Depot? Its agent keeps running until you uninstall it there.")) return;
      try { await api("POST", "/api/machines/" + this.form.id + "/delete"); this.list = this.list.filter(x => x.id !== this.form.id); this.dlg = false; toast("Deleted"); }
      catch (e) { toast(e.message, "bad"); }
    },
    agentOld(m) { return !!m.agent_version && /\d/.test(this.version) && /\d/.test(m.agent_version) && newerVersion(this.version, m.agent_version); },
    copy: copyText,
  };
}

// ---- activity ----

function activityPage(jobs, events) {
  return {
    jobs, events, view: "jobs", open: {}, filter: "", timer: null,
    get shown() { return this.jobs.filter(j => !this.filter || j.status === this.filter); },
    start() { this.schedule(); },
    schedule() { this.timer = setTimeout(() => this.refresh(), this.jobs.some(j => j.status === "running") ? 1500 : 10000); },
    async refresh() {
      try {
        const [j, e] = await Promise.all([api("GET", "/api/jobs"), api("GET", "/api/events")]);
        this.jobs = j.jobs; this.events = e.events;
      } catch (e) { }
      this.schedule();
    },
    toggle(j) { this.open[j.id] = !this.open[j.id]; },
  };
}

// ---- settings ----

function settingsPage(tab, admin) {
  return {
    tab: tab || (admin ? "general" : "account"), saving: false,
    users: [], me: Number((document.querySelector('meta[name="user"]') || {}).content || 0),
    inv: { email: "", name: "", role: "member" }, inviting: false, link: "", linkNote: "",
    start() { if (admin) this.loadUsers(); },
    go(t) { this.tab = t; const u = new URL(location); u.searchParams.set("tab", t); history.replaceState(null, "", u); },
    async save(form) {
      this.saving = true;
      const body = {};
      for (const [k, v] of new FormData(form).entries()) body[k] = v; // later values (checked boxes) win
      try {
        await api("POST", "/api/settings", body);
        toast("Settings saved");
        if ("theme" in body || "density" in body || "site_name" in body) setTimeout(() => location.reload(), 300);
        for (const el of form.querySelectorAll('input[type="password"]')) el.value = "";
      } catch (e) { toast(e.message, "bad"); }
      this.saving = false;
    },
    async clearSecret(btn, key) {
      try { await api("POST", "/api/settings", { [key]: "__clear__" }); toast("Cleared"); setTimeout(() => location.reload(), 300); }
      catch (e) { toast(e.message, "bad"); }
    },
    async test(kind) {
      try {
        const r = await api("POST", "/api/settings/test-" + kind);
        if (kind === "email") toast("Test email sent", "ok", "To " + r.to + ". Check your inbox (and spam folder).");
        else if (kind === "webhook") toast("Test sent to the webhook");
        else toast("Test notification sent", "ok", "Check the ntfy app on your phone.");
      } catch (e) { toast(e.message, "bad", kind === "ntfy" || kind === "email" ? "Save your changes first if you just edited them." : ""); }
    },
    async loadUsers() { try { this.users = (await api("GET", "/api/users")).users; } catch (e) { toast(e.message, "bad"); } },
    showLink(r, who) {
      this.link = r.link;
      this.linkNote = r.emailed ? "Invite emailed to " + who + ". You can also share this link:"
        : (r.email_error ? "The email couldn't be sent (" + r.email_error + "). Send " + who + " this link yourself:" : "Send " + who + " this link:");
      if (r.emailed) toast("Invite sent to " + who); else toast("Invite created", "info", "Copy the link and send it to " + who + ".");
    },
    async invite() {
      this.inviting = true;
      try {
        const r = await api("POST", "/api/users/invite", this.inv);
        this.showLink(r, this.inv.email);
        this.inv = { email: "", name: "", role: this.inv.role };
        this.loadUsers();
      } catch (e) { toast(e.message, "bad"); }
      this.inviting = false;
    },
    async resend(u) {
      try { this.showLink(await api("POST", "/api/users/" + u.id + "/resend"), u.email); this.loadUsers(); }
      catch (e) { toast(e.message, "bad"); }
    },
    async setRole(u, ev) {
      const role = ev.target.value;
      try { await api("POST", "/api/users/" + u.id + "/role", { role }); u.role = role; toast(u.name + " is now " + (role === "admin" ? "an admin" : "a " + role)); }
      catch (e) { ev.target.value = u.role; toast(e.message, "bad"); }
    },
    async removeUser(u) {
      if (!confirm(u.pending ? "Cancel the invite for " + u.email + "?" : "Remove " + u.name + " (" + u.email + ")? They're signed out straight away.")) return;
      try { await api("POST", "/api/users/" + u.id + "/delete"); this.users = this.users.filter(x => x.id !== u.id); toast(u.pending ? "Invite cancelled" : "Removed " + u.name); }
      catch (e) { toast(e.message, "bad"); }
    },
    copy: copyText,
    async saveAccount(form) {
      const f = Object.fromEntries(new FormData(form).entries());
      try { await api("POST", "/api/account", { Name: f.name, Email: f.email, Current: f.current, New: f.new }); toast("Account saved"); form.current.value = ""; form.new.value = ""; }
      catch (e) { toast(e.message, "bad"); }
    },
  };
}

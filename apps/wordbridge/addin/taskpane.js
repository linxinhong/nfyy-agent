/* WordBridge macOS 加载项 —— 任务窗格脚本
 * 经 WebSocket 与常驻桥通信：桥把命令推过来，本脚本在 Word.run 里执行后
 * 按 request_id 回传结果。数据契约与 com_worker.ComWorker 完全对齐。
 */

/* global Office, Word */

let ws = null;
let reconnectTimer = null;
let helloInterval = null;

// region_id → { text, ooxml, name, path }
const regions = new Map();

function wsUrl() {
  const proto = window.location.protocol === "https:" ? "wss://" : "ws://";
  return proto + window.location.host + "/addin-ws";
}

function randomHex(n) {
  const bytes = new Uint8Array(n / 2);
  if (window.crypto && crypto.getRandomValues) {
    crypto.getRandomValues(bytes);
  } else {
    for (let i = 0; i < bytes.length; i++) bytes[i] = Math.floor(Math.random() * 256);
  }
  return Array.from(bytes, (b) => b.toString(16).padStart(2, "0")).join("");
}

async function sha256hex(text) {
  try {
    const buf = await crypto.subtle.digest("SHA-256", new TextEncoder().encode(text));
    return Array.from(new Uint8Array(buf))
      .map((b) => b.toString(16).padStart(2, "0"))
      .join("")
      .slice(0, 16);
  } catch (e) {
    return null;
  }
}

function setStatus(on) {
  const dot = document.getElementById("status");
  const txt = document.getElementById("statusText");
  if (!dot || !txt) return;
  dot.className = on ? "status on" : "status";
  txt.textContent = on ? "已连接桥" : "连接中…";
}

function connect() {
  try {
    ws = new WebSocket(wsUrl());
  } catch (e) {
    scheduleReconnect();
    return;
  }
  ws.onopen = () => {
    send({ type: "hello" });
    setStatus(true);
  };
  ws.onmessage = (ev) => {
    let msg;
    try {
      msg = JSON.parse(ev.data);
    } catch (e) {
      return;
    }
    if (msg && msg.method) {
      dispatch(msg);
    }
  };
  ws.onclose = () => {
    setStatus(false);
    scheduleReconnect();
  };
  ws.onerror = () => {
    try { ws.close(); } catch (e) {}
  };
}

function scheduleReconnect() {
  if (reconnectTimer) return;
  reconnectTimer = setTimeout(() => {
    reconnectTimer = null;
    connect();
  }, 1500);
}

function send(obj) {
  if (ws && ws.readyState === WebSocket.OPEN) {
    ws.send(JSON.stringify(obj));
  }
}

async function dispatch(msg) {
  const { id, method, params } = msg;
  let reply;
  try {
    const result = await handle(method, params || {});
    reply = { id, ok: true, result };
  } catch (e) {
    let err = (e && e.message) || String(e);
    try {
      const dbg = (e && e.debugInfo) ? JSON.stringify(e.debugInfo) : "";
      const loc = (e && e.errorLocation) ? e.errorLocation : "";
      if (dbg || loc) err = err + " || " + (loc || "") + (dbg ? (" " + dbg) : "");
    } catch (_) {}
    reply = { id, ok: false, error: err };
  }
  send(reply);
}

// ── 文档信息 ──

async function getDocProps(context) {
  const props = context.document.properties;
  props.load("title");
  await context.sync();
  const name = props.title || "未命名文档";
  return { name, path: "" };
}

// ── 命令实现 ──

async function handle(method, params) {
  switch (method) {
    case "capture": return capture(params);
    case "read_document": return readDocument(params);
    case "find_text": return findText(params);
    case "list_documents": return listDocuments(params);
    case "get_doc_info": return getDocInfo(params);
    case "apply_patch": return applyPatch(params);
    case "apply_text": return applyText(params);
    case "apply_ooxml": return applyOoxml(params);
    case "diff_check": return diffCheck(params);
    case "create_document": return createDocument(params);
    default:
      throw new Error("未知方法: " + method);
  }
}

async function capture() {
  return Word.run(async (context) => {
    const selection = context.document.getSelection();
    selection.load("text");
    await context.sync();
    const text = selection.text;
    if (!text || !text.trim()) return [];

    const ooxml = selection.getOoxml();
    await context.sync();
    const ooxmlValue = ooxml.value || "";

    const props = await getDocProps(context);
    const regionId = "hb_" + randomHex(16);
    const hash = await sha256hex(text);
    regions.set(regionId, { text, ooxml: ooxmlValue, name: props.name, path: props.path });
    return [{
      region_id: regionId,
      text: text,
      ooxml: ooxmlValue,
      hash: hash,
      bookmark: regionId,
      doc_path: props.path,
      source: "active",
    }];
  });
}

async function readDocument() {
  return Word.run(async (context) => {
    const body = context.document.body;
    body.load("text");
    await context.sync();
    const text = body.text;
    if (!text || !text.trim()) return { status: "error", message: "文档正文为空" };

    const props = await getDocProps(context);
    const regionId = "hb_" + randomHex(16);
    const hash = await sha256hex(text);
    regions.set(regionId, { text, ooxml: "", name: props.name, path: props.path });
    return {
      status: "ok",
      name: props.name,
      path: props.path,
      capture: {
        region_id: regionId,
        text: text,
        ooxml: "",
        hash: hash,
        bookmark: regionId,
        doc_path: props.path,
        source: "read_document",
      },
    };
  });
}

async function findText(params) {
  const text = params.text;
  const maxMatches = params.max_matches || 20;
  if (!text) return [];
  return Word.run(async (context) => {
    const results = context.document.body.search(text, { matchCase: true, matchWholeWord: false });
    results.load("items");
    await context.sync();
    const props = await getDocProps(context);
    const out = [];
    const count = Math.min(results.items.length, maxMatches);
    for (let i = 0; i < count; i++) {
      const r = results.items[i];
      r.load("text");
      await context.sync();
      const txt = r.text;
      const regionId = "hb_" + randomHex(16);
      const hash = await sha256hex(txt);
      regions.set(regionId, { text: txt, ooxml: "", name: props.name, path: props.path });
      out.push({
        region_id: regionId,
        text: txt,
        ooxml: "",
        hash: hash,
        bookmark: regionId,
        doc_path: props.path,
        source: "find",
      });
    }
    return out;
  });
}

async function listDocuments() {
  return Word.run(async (context) => {
    const props = await getDocProps(context);
    return [{ name: props.name, path: props.path, active: true }];
  });
}

async function getDocInfo() {
  return Word.run(async (context) => {
    const props = await getDocProps(context);
    return {
      status: "ok",
      name: props.name,
      path: props.path,
      saved: null,
      paragraphs: 0,
    };
  });
}

// 定位 region 原文：在全文里精确搜索该 region 的文本，要求唯一命中。
async function locateRegion(context, stored) {
  const res = context.document.body.search(stored.text, { matchCase: true, matchWholeWord: false });
  res.load("items");
  await context.sync();
  if (res.items.length !== 1) return null;
  return res.items[0];
}

// ── 强制修订（track changes） ──
// 写回前把 changeTrackingMode 切到 TrackAll，写完后还原用户原状态。
// 全程 fail-safe：读不到/设不上时返回 null，调用方跳过强制，按用户原设置写回，
// 绝不因强制修订失败而阻断写回，也不会让文档停留在 TrackAll。
async function forceTracking(context) {
  let prev = null;
  try {
    const doc = context.document;
    doc.load("changeTrackingMode");
    await context.sync();
    prev = doc.changeTrackingMode;
  } catch (e) {
    return null;
  }
  // Word.ChangeTrackingMode: "Off" | "TrackAll" | "TrackMineOnly"
  if (typeof prev !== "string" || prev === "TrackAll") return null;
  try {
    context.document.changeTrackingMode = "TrackAll";
    await context.sync();
  } catch (e) {
    return null;
  }
  return async function restore() {
    try {
      context.document.changeTrackingMode = prev;
      await context.sync();
    } catch (e) {
      /* 还原失败不掩盖写回结果 */
    }
  };
}

async function applyPatch(params) {
  const { region_id, patches, expected_hash } = params;
  return Word.run(async (context) => {
    const stored = regions.get(region_id);
    if (!stored) {
      return { status: "not_found", reason: "region 不在当前面板（面板重载后请重新获取选区）" };
    }

    // 定位 region 原文作为唯一锚点：这样短 find 只需在选区内唯一，不受全文干扰。
    const rr = await locateRegion(context, stored);
    if (!rr) {
      return { status: "conflict", reason: "无法唯一定位原文，请重新获取选区" };
    }
    // 期望 hash 守卫：确认原文未变。
    if (expected_hash) {
      rr.load("text");
      await context.sync();
      const cur = await sha256hex(rr.text);
      if (cur !== null && cur !== expected_hash) {
        return { status: "conflict", reason: "原文已变化，请重新获取选区" };
      }
    }

    // 强制修订：写回期间切换为 TrackAll，完成后还原用户原状态。
    const restore = await forceTracking(context);
    try {
      let applied = 0;
      for (let i = 0; i < patches.length; i++) {
        const p = patches[i];
        const find = (typeof p.find === "string") ? p.find : "";
        const replace = (typeof p.replace === "string") ? p.replace : "";
        if (find === "" && replace === "") continue;
        if (find === replace) continue;

        if (find === "") {
          // 纯插入：插到选区开头
          rr.insertText(replace, "Start");
          await context.sync();
          applied += 1;
          continue;
        }

        // 在选区内搜索（而非全文），短 find 也能唯一命中
        const res = rr.search(find, { matchCase: true, matchWholeWord: false });
        res.load("items");
        await context.sync();
        if (res.items.length === 0) {
          return { status: "not_found", reason: `选区内未找到 "${find}"`, patch_index: i, patches_applied: applied };
        }
        if (res.items.length > 1) {
          return { status: "conflict", reason: `"${find}" 在选区内不唯一(${res.items.length})`, patch_index: i, patches_applied: applied };
        }
        const fr = res.items[0];
        fr.delete();
        await context.sync();
        if (replace !== "") {
          fr.insertText(replace, "Start");
          await context.sync();
        }
        applied += 1;
      }
      return { status: "applied", region_id, patches_applied: applied };
    } finally {
      if (restore) {
        try { await restore(); } catch (e) { /* 还原失败不掩盖写回结果 */ }
      }
    }
  });
}

async function applyText(params) {
  const { region_id, new_text } = params;
  return Word.run(async (context) => {
    const stored = regions.get(region_id);
    if (!stored) return { status: "not_found", reason: "region 不在当前面板（面板重载后请重新获取选区）" };
    const rr = await locateRegion(context, stored);
    if (!rr) return { status: "conflict", reason: "无法唯一定位原文，请重新获取选区" };
    const restore = await forceTracking(context);
    try {
      rr.delete();
      await context.sync();
      rr.insertText(new_text, "Start");
      await context.sync();
      return { status: "applied", region_id };
    } finally {
      if (restore) {
        try { await restore(); } catch (e) { /* 还原失败不掩盖写回结果 */ }
      }
    }
  });
}

async function applyOoxml(params) {
  const { region_id, new_ooxml } = params;
  return Word.run(async (context) => {
    const stored = regions.get(region_id);
    if (!stored) return { status: "not_found", reason: "region 不在当前面板（面板重载后请重新获取选区）" };
    const rr = await locateRegion(context, stored);
    if (!rr) return { status: "conflict", reason: "无法唯一定位原文，请重新获取选区" };
    const restore = await forceTracking(context);
    try {
      rr.delete();
      await context.sync();
      rr.insertOoxml(new_ooxml, "Start");
      await context.sync();
      return { status: "applied", region_id };
    } finally {
      if (restore) {
        try { await restore(); } catch (e) { /* 还原失败不掩盖写回结果 */ }
      }
    }
  });
}

async function diffCheck(params) {
  const { region_id } = params;
  return Word.run(async (context) => {
    const stored = regions.get(region_id);
    if (!stored) return { status: "not_found", reason: "书签已不存在" };
    const rr = await locateRegion(context, stored);
    if (!rr) return { status: "not_found", reason: "书签已不存在" };
    rr.load("text");
    await context.sync();
    return {
      status: "ok",
      current_text: rr.text,
      current_hash: await sha256hex(rr.text),
    };
  });
}

async function createDocument(params) {
  const { name, path, base64 } = params;
  if (typeof Word.createDocument !== "function") {
    return { status: "ok", name, path, opened: false };
  }
  try {
    Word.createDocument(base64);
    return { status: "ok", name, path, opened: true };
  } catch (e) {
    return { status: "error", message: "新建文档失败: " + e };
  }
}

// ── 文档自动开面板打标 ──
// 客户端打标：首次打开面板后，把当前文档标记为「随文档自动打开」。
// 之后每次打开该文档，Word 会按 manifest 里 TaskpaneId=Office.AutoShowTaskpaneWithDocument 自动开面板。
function tagAutoOpen() {
  try {
    const settings = Office.context.document.settings;
    settings.set("Office.AutoShowTaskpaneWithDocument", true);
    settings.saveAsync(function () {
      /* 打标结果不影响功能，忽略 */
    });
  } catch (e) {
    /* 忽略打标失败 */
  }
}

// ── 启动 ──

Office.onReady(function (info) {
  tagAutoOpen();
  connect();
});

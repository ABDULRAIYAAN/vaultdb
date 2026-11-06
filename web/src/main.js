import "./style.css";
import { animate } from "motion";
import { createTreeViz } from "./btree-viz.js";
import { createHnswViz } from "./hnsw-viz.js";

const reduceMotion = matchMedia("(prefers-reduced-motion: reduce)").matches;
const $ = (id) => document.getElementById(id);

// ---------- Hero: live B-tree ----------
const tree = createTreeViz($("tree-stage"), $("tree-caption"), reduceMotion);
const opening = [40, 15, 72, 8, 23, 56, 91, 34, 63, 5, 47, 80];
// The card swings into its resting 3D angle as the page opens.
if (!reduceMotion) {
  animate(
    $("tree-card"),
    { transform: ["rotateX(28deg) rotateY(-32deg) translateZ(-120px)", getComputedStyle($("tree-card")).transform], opacity: [0, 1] },
    { duration: 1.3, ease: [0.2, 0.8, 0.2, 1] }
  );
}
if (reduceMotion) {
  opening.forEach((k) => tree.insert(k));
} else {
  // The page's one self-playing moment: the tree builds itself once.
  opening.forEach((k, i) => setTimeout(() => tree.insert(k), 500 + i * 650));
}
$("tree-form").addEventListener("submit", (e) => {
  e.preventDefault();
  const input = $("tree-key");
  const n = Number(input.value);
  if (input.value === "" || !Number.isInteger(n) || n < 0 || n > 999) {
    $("tree-caption").textContent = "Type a whole number from 0 to 999.";
    return;
  }
  tree.insert(n);
  input.value = "";
  input.focus();
});
$("tree-reset").addEventListener("click", () => tree.reset());
let resizeTimer;
addEventListener("resize", () => {
  clearTimeout(resizeTimer);
  resizeTimer = setTimeout(() => tree.relayout(), 150);
});

// ---------- Playground ----------
const out = $("term-out");
const input = $("term-input");
const history = [];
let histPos = 0;
let busy = false;

function formatBytes(n) {
  if (n >= 1 << 20) return (n / (1 << 20)).toFixed(1) + " MB";
  if (n >= 1024) return (n / 1024).toFixed(1) + " KB";
  return n + " B";
}

// Count a number up or down to its new value, so changes are visible.
function setStat(id, value, format = String) {
  const node = $(id);
  const from = Number(node.dataset.v || 0);
  node.dataset.v = value;
  if (reduceMotion || from === value) {
    node.textContent = format(value);
    return;
  }
  animate(from, value, { duration: 0.5, onUpdate: (v) => (node.textContent = format(Math.round(v))) });
  animate(node, { color: ["#e8e2d4", "#5fd3b4"] }, { duration: 0.6 });
}

function print(cmd, reply, isError) {
  const c = document.createElement("pre");
  c.className = "cmd";
  c.textContent = cmd;
  const r = document.createElement("pre");
  r.className = "reply" + (isError ? " err" : "");
  r.textContent = reply;
  out.append(c, r);
  out.scrollTop = out.scrollHeight;
  if (!reduceMotion) animate(r, { opacity: [0, 1] }, { duration: 0.25 });
}

async function run(cmd) {
  cmd = cmd.trim();
  if (!cmd || busy) return;
  busy = true;
  history.push(cmd);
  histPos = history.length;
  try {
    const res = await fetch("/api/exec", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ command: cmd }),
    });
    const data = await res.json();
    if (data.error) {
      print(cmd, data.error, true);
    } else {
      print(cmd, data.reply, data.reply.startsWith("(error)"));
      setStat("st-keys", data.keys);
      setStat("st-bytes", data.bytes, formatBytes);
      $("st-time").textContent = data.micros < 1 ? "under 1 µs" : `${data.micros} µs`;
    }
  } catch {
    print(cmd, "Could not reach the server. Check your connection and try again.", true);
  } finally {
    busy = false;
    clearTimeout(statsTimer);
    statsTimer = setTimeout(refreshServerLine, 800);
  }
}
let statsTimer;

$("term-form").addEventListener("submit", (e) => {
  e.preventDefault();
  const cmd = input.value;
  input.value = "";
  run(cmd);
});
input.addEventListener("keydown", (e) => {
  if (e.key === "ArrowUp" && histPos > 0) {
    input.value = history[--histPos];
    e.preventDefault();
  } else if (e.key === "ArrowDown") {
    histPos = Math.min(history.length, histPos + 1);
    input.value = history[histPos] || "";
    e.preventDefault();
  }
});
$("terminal").addEventListener("click", (e) => {
  if (!window.getSelection().toString() && e.target.tagName !== "A") input.focus();
});

const examples = [
  {
    title: "Keys and ranges",
    hint: "SET, GET, PREFIX on the B-tree",
    cmds: ["SET user:1 Raiyaan", "SET user:2 Ali", "SET user:10 Sara", "SET order:1 pizza", "GET user:1", "PREFIX user:"],
  },
  {
    title: "SQL table",
    hint: "CREATE, INSERT, SELECT with WHERE",
    cmds: [
      "CREATE TABLE students (id INT PRIMARY KEY, name TEXT, gpa FLOAT)",
      "INSERT INTO students VALUES (1, 'Raiyaan', 3.9), (2, 'Ali', 3.2), (3, 'Sara', 3.6)",
      "SELECT name, gpa FROM students WHERE gpa > 3.5 ORDER BY gpa DESC",
      "PREFIX sql:row:students:",
    ],
  },
  {
    title: "Vector search",
    hint: "find the closest meaning with HNSW",
    cmds: [
      "VCREATE notes 3",
      'VADD notes car "[1, 0, 0]" "The car broke down"',
      'VADD notes vehicle "[0.9, 0.1, 0]" "My vehicle stopped working"',
      'VADD notes cake "[0, 0, 1]" "I love chocolate cake"',
      'VSEARCH notes "[1, 0.05, 0]" 2',
    ],
  },
];
for (const ex of examples) {
  const b = document.createElement("button");
  b.type = "button";
  b.className = "example";
  const t = document.createElement("strong");
  t.textContent = ex.title;
  const h = document.createElement("span");
  h.textContent = ex.hint;
  b.append(t, h);
  b.addEventListener("click", async () => {
    b.disabled = true;
    for (const c of ex.cmds) {
      await run(c);
      if (!reduceMotion) await new Promise((r) => setTimeout(r, 260));
    }
    b.disabled = false;
    input.focus();
  });
  $("examples").appendChild(b);
}

$("term-reset").addEventListener("click", async () => {
  try {
    const res = await fetch("/api/reset", { method: "POST" });
    const data = await res.json();
    print("(empty my sandbox)", data.reply || data.error, Boolean(data.error));
    if (!data.error) {
      setStat("st-keys", 0);
      setStat("st-bytes", 0, formatBytes);
    }
  } catch {
    print("(empty my sandbox)", "Could not reach the server.", true);
  }
});

// Real numbers about this server, nothing invented.
function refreshServerLine() {
  fetch("/api/stats")
    .then((r) => r.json())
    .then((s) => {
      const n = s.commandsServed.toLocaleString("en");
      const up = s.uptimeSeconds >= 3600 ? `${Math.floor(s.uptimeSeconds / 3600)} h` : `${Math.max(1, Math.floor(s.uptimeSeconds / 60))} min`;
      $("server-line").textContent = `This server has answered ${n} ${s.commandsServed === 1 ? "command" : "commands"} since it started ${up} ago.`;
      if (!s.askEnabled) $("ask").hidden = true;
    })
    .catch(() => {});
}
refreshServerLine();

// ---------- HNSW drawing ----------
const hnsw = createHnswViz($("hnsw"), reduceMotion);
$("hnsw-run").addEventListener("click", (e) => hnsw.search(e.currentTarget));
$("hnsw-turn").addEventListener("click", () => hnsw.turn());

// ---------- Ask the docs ----------
$("ask-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  const q = $("ask-input").value.trim();
  const box = $("ask-out");
  if (!q) return;
  const button = e.currentTarget.querySelector("button");
  button.disabled = true;
  box.replaceChildren(Object.assign(document.createElement("p"), { className: "ask-hint", textContent: "Searching VaultDB's docs..." }));
  try {
    const res = await fetch("/api/ask", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ question: q }),
    });
    const data = await res.json();
    box.replaceChildren();
    if (data.error) {
      box.appendChild(Object.assign(document.createElement("p"), { className: "ask-error", textContent: data.error }));
      return;
    }
    if (data.answer) {
      box.appendChild(Object.assign(document.createElement("p"), { className: "answer", textContent: data.answer }));
    } else {
      box.appendChild(Object.assign(document.createElement("p"), { className: "ask-hint", textContent: "These are the passages VaultDB found most relevant:" }));
    }
    const list = document.createElement("div");
    list.className = "sources";
    (data.sources || []).forEach((s, i) => {
      const d = document.createElement("details");
      if (!data.answer && i === 0) d.open = true;
      const sum = document.createElement("summary");
      const b = document.createElement("b");
      b.textContent = `[${s.n}]`;
      sum.append(b, document.createTextNode(s.file));
      const pre = document.createElement("pre");
      pre.textContent = s.text;
      d.append(sum, pre);
      list.appendChild(d);
    });
    box.appendChild(list);
  } catch {
    box.replaceChildren(Object.assign(document.createElement("p"), { className: "ask-error", textContent: "Could not reach the server. Check your connection and try again." }));
  } finally {
    button.disabled = false;
  }
});

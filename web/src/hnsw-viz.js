// A two-dimensional drawing of an HNSW search: three layers, each a graph
// linking every point to its nearest neighbours on that layer. A search
// starts at the top and hops greedily towards the target, then drops down.
import { animate } from "motion";

const NS = "http://www.w3.org/2000/svg";
const W = 520;
const LAYER_H = 150;
const PAD = 16;

// Small seeded random generator, so the drawing is the same on every visit.
function rng(seed) {
  return () => {
    seed = (seed * 1664525 + 1013904223) >>> 0;
    return seed / 4294967296;
  };
}

function el(name, attrs, parent) {
  const e = document.createElementNS(NS, name);
  for (const [k, v] of Object.entries(attrs)) e.setAttribute(k, v);
  parent.appendChild(e);
  return e;
}

const dist = (a, b) => Math.hypot(a.x - b.x, a.y - b.y);

export function createHnswViz(host, reduceMotion) {
  const rand = rng(7);
  const points = Array.from({ length: 70 }, (_, i) => ({
    id: i,
    x: PAD + rand() * (W - 2 * PAD),
    y: PAD + rand() * (LAYER_H - 2 * PAD - 10),
    // Layer chosen like HNSW does: most points only on layer 0.
    level: rand() < 0.08 ? 2 : rand() < 0.25 ? 1 : 0,
  }));
  points[0].level = 2; // make sure the top layer has an entry point
  const layers = [0, 1, 2].map((l) => points.filter((p) => p.level >= l));
  const links = layers.map((members, l) => {
    const k = l === 0 ? 4 : 3;
    const m = new Map();
    for (const p of members) {
      m.set(p.id, members.filter((q) => q !== p).sort((a, b) => dist(p, a) - dist(p, b)).slice(0, k));
    }
    return m;
  });

  const H = LAYER_H * 3 + 30;
  const svg = el("svg", { viewBox: `0 0 ${W} ${H}` }, host);
  const yOff = (l) => (2 - l) * (LAYER_H + 10) + 4;
  const groups = [];
  for (let l = 2; l >= 0; l--) {
    const g = el("g", { transform: `translate(0 ${yOff(l)})` }, svg);
    el("rect", { class: "plane", x: 1, y: 1, width: W - 2, height: LAYER_H - 2, rx: 8 }, g);
    const t = el("text", { class: "lbl", x: W - 12, y: LAYER_H - 10, "text-anchor": "end" }, g);
    t.textContent = l === 0 ? "layer 0: every point" : `layer ${l}: ${layers[l].length} points`;
    const drawn = new Set();
    for (const [id, nbrs] of links[l]) {
      const p = points[id];
      for (const q of nbrs) {
        const key = Math.min(id, q.id) + "-" + Math.max(id, q.id);
        if (drawn.has(key)) continue;
        drawn.add(key);
        el("line", { class: "edge", x1: p.x, y1: p.y, x2: q.x, y2: q.y }, g);
      }
    }
    const dots = new Map();
    for (const p of layers[l]) dots.set(p.id, el("circle", { class: "pt", cx: p.x, cy: p.y, r: l === 0 ? 3.2 : 4.2 }, g));
    groups[l] = { g, dots };
  }
  const overlay = el("g", {}, svg);

  let running = false;
  async function search(button) {
    if (running) return;
    running = true;
    button.disabled = true;
    overlay.replaceChildren();
    for (const { dots } of groups) for (const c of dots.values()) c.setAttribute("class", "pt");

    const target = { x: PAD + Math.random() * (W - 2 * PAD), y: PAD + Math.random() * (LAYER_H - 2 * PAD - 10) };
    const step = reduceMotion ? 0 : 0.28;
    let cur = points[0];
    for (let l = 2; l >= 0; l--) {
      const y = yOff(l);
      const q = el("circle", { class: "query", cx: target.x, cy: target.y + y, r: 7 }, overlay);
      if (step) await animate(q, { opacity: [0, 1] }, { duration: step }).finished;
      groups[l].dots.get(cur.id).setAttribute("class", "pt visited");
      for (;;) {
        const better = links[l].get(cur.id).reduce((best, n) => (dist(n, target) < dist(best, target) ? n : best), cur);
        if (better === cur) break;
        const hop = el("line", { class: "hop", x1: cur.x, y1: cur.y + y, x2: better.x, y2: better.y + y }, overlay);
        const len = dist(cur, better);
        hop.setAttribute("stroke-dasharray", len);
        if (step) await animate(hop, { strokeDashoffset: [len, 0] }, { duration: step }).finished;
        cur = better;
        groups[l].dots.get(cur.id).setAttribute("class", "pt visited");
      }
      if (l > 0) {
        el("line", { class: "drop", x1: cur.x, y1: cur.y + y, x2: cur.x, y2: cur.y + yOff(l - 1) }, overlay);
      }
    }
    groups[0].dots.get(cur.id).setAttribute("class", "pt best");
    running = false;
    button.disabled = false;
  }

  return { search };
}

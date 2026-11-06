// HNSW search drawn in 3D: three graph layers stacked in space, each linking
// its points to their nearest neighbours on that layer. A search starts at
// the top, hops greedily towards the target, then drops through to the
// layer below at the same point.
import { animate } from "motion";

const NS = "http://www.w3.org/2000/svg";
const W = 440;
const H = 300;
const PAD = 18;
const GAP = 120; // distance between layers in 3D space
const VIEWS = [
  "rotateX(62deg) rotateZ(-38deg)",
  "rotateX(62deg) rotateZ(22deg)",
  "rotateX(58deg) rotateZ(-118deg)",
];

function rng(seed) {
  return () => {
    seed = (seed * 1664525 + 1013904223) >>> 0;
    return seed / 4294967296;
  };
}

function svgEl(name, attrs, parent) {
  const e = document.createElementNS(NS, name);
  for (const [k, v] of Object.entries(attrs)) e.setAttribute(k, v);
  parent.appendChild(e);
  return e;
}

const dist = (a, b) => Math.hypot(a.x - b.x, a.y - b.y);

export function createHnswViz(host, reduceMotion) {
  const rand = rng(11);
  const points = Array.from({ length: 64 }, (_, i) => ({
    id: i,
    x: PAD + rand() * (W - 2 * PAD),
    y: PAD + rand() * (H - 2 * PAD),
    level: rand() < 0.1 ? 2 : rand() < 0.28 ? 1 : 0,
  }));
  points[0].level = 2;
  const layers = [0, 1, 2].map((l) => points.filter((p) => p.level >= l));
  const links = layers.map((members, l) => {
    const k = l === 0 ? 4 : 3;
    const m = new Map();
    for (const p of members) m.set(p.id, members.filter((q) => q !== p).sort((a, b) => dist(p, a) - dist(p, b)).slice(0, k));
    return m;
  });

  const stack = document.createElement("div");
  stack.className = "hnsw-stack";
  host.appendChild(stack);
  let view = 0;
  stack.style.transform = VIEWS[0];

  const planes = [];
  for (let l = 0; l <= 2; l++) {
    const plane = document.createElement("div");
    plane.className = "hnsw-layer";
    plane.style.transform = `translateZ(${(l - 1) * GAP}px)`;
    const svg = svgEl("svg", { viewBox: `0 0 ${W} ${H}` }, plane);
    const t = svgEl("text", { class: "lbl", x: W - 14, y: H - 14, "text-anchor": "end" }, svg);
    t.textContent = l === 0 ? `layer 0: all ${points.length} points` : `layer ${l}: ${layers[l].length} points`;
    const drawn = new Set();
    for (const [id, nbrs] of links[l]) {
      const p = points[id];
      for (const q of nbrs) {
        const key = Math.min(id, q.id) + "-" + Math.max(id, q.id);
        if (drawn.has(key)) continue;
        drawn.add(key);
        svgEl("line", { class: "edge", x1: p.x, y1: p.y, x2: q.x, y2: q.y }, svg);
      }
    }
    const dots = new Map();
    for (const p of layers[l]) dots.set(p.id, svgEl("circle", { class: "pt", cx: p.x, cy: p.y, r: l === 0 ? 3.4 : 4.6 }, svg));
    const overlay = svgEl("g", {}, svg);
    stack.appendChild(plane);
    planes[l] = { plane, dots, overlay };
  }

  // Thin vertical threads show where a search drops to the layer below.
  const threads = [];
  function thread(l, p) {
    const d = document.createElement("div");
    Object.assign(d.style, {
      position: "absolute",
      left: `${(p.x / W) * 100}%`,
      top: `${(p.y / H) * 100}%`,
      width: "2px",
      height: `${GAP}px`,
      marginLeft: "-1px",
      background: "linear-gradient(#c8743a, rgba(200,116,58,0.15))",
      transformOrigin: "top center",
      transform: "rotateX(-90deg)",
    });
    planes[l].plane.appendChild(d);
    threads.push(d);
    return d;
  }

  // Entrance: the layers drift apart in 3D the first time they come into view.
  if (!reduceMotion) {
    const io = new IntersectionObserver((entries) => {
      if (!entries.some((e) => e.isIntersecting)) return;
      io.disconnect();
      planes.forEach(({ plane }, l) =>
        animate(plane, { transform: ["translateZ(0px)", `translateZ(${(l - 1) * GAP}px)`], opacity: [0, 1] }, { duration: 1.1, delay: l * 0.12, ease: [0.2, 0.8, 0.2, 1] })
      );
      animate(stack, { transform: ["rotateX(20deg) rotateZ(-10deg)", VIEWS[0]] }, { duration: 1.4, ease: [0.2, 0.8, 0.2, 1] });
    }, { threshold: 0.35 });
    io.observe(host);
  }

  function turn() {
    view = (view + 1) % VIEWS.length;
    if (reduceMotion) stack.style.transform = VIEWS[view];
    else animate(stack, { transform: VIEWS[view] }, { duration: 0.9, ease: [0.4, 0, 0.2, 1] });
  }

  let running = false;
  async function search(button) {
    if (running) return;
    running = true;
    button.disabled = true;
    for (const { overlay, dots } of planes) {
      overlay.replaceChildren();
      for (const c of dots.values()) c.setAttribute("class", "pt");
    }
    threads.splice(0).forEach((t) => t.remove());

    const target = { x: PAD + Math.random() * (W - 2 * PAD), y: PAD + Math.random() * (H - 2 * PAD) };
    const step = reduceMotion ? 0 : 0.3;
    let cur = points[0];
    for (let l = 2; l >= 0; l--) {
      const { overlay, dots } = planes[l];
      const q = svgEl("circle", { class: "query", cx: target.x, cy: target.y, r: 8 }, overlay);
      if (step) await animate(q, { opacity: [0, 1] }, { duration: step }).finished;
      if (l < 2) {
        const ring = svgEl("circle", { class: "landing", cx: cur.x, cy: cur.y, r: 7 }, overlay);
        if (step) animate(ring, { opacity: [1, 0] }, { duration: 0.8 });
      }
      dots.get(cur.id).setAttribute("class", "pt visited");
      for (;;) {
        const better = links[l].get(cur.id).reduce((best, n) => (dist(n, target) < dist(best, target) ? n : best), cur);
        if (better === cur) break;
        const hop = svgEl("line", { class: "hop", x1: cur.x, y1: cur.y, x2: better.x, y2: better.y }, overlay);
        const len = dist(cur, better);
        hop.setAttribute("stroke-dasharray", len);
        if (step) await animate(hop, { strokeDashoffset: [len, 0] }, { duration: step }).finished;
        cur = better;
        dots.get(cur.id).setAttribute("class", "pt visited");
      }
      if (l > 0) {
        const t = thread(l, cur);
        if (step) await animate(t, { opacity: [0, 1] }, { duration: step }).finished;
      }
    }
    planes[0].dots.get(cur.id).setAttribute("class", "pt best");
    running = false;
    button.disabled = false;
  }

  return { search, turn };
}

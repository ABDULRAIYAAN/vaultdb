// A small B-tree drawn live in the hero. It uses the same insert-and-split
// rules as VaultDB's Go B-tree, with tiny nodes (at most 3 keys) so the
// splits happen often enough to see.
import { animate } from "motion";

const MAX_KEYS = 3;
const NODE_H = 36;
const KEY_W = 34;
const LEVEL_GAP = 60;

let nextId = 1;
const newNode = (keys = [], children = []) => ({ id: nextId++, keys, children });

export function createTreeViz(stage, caption, reduceMotion) {
  let root = null;
  const els = new Map(); // node id -> element
  const svg = document.createElementNS("http://www.w3.org/2000/svg", "svg");
  svg.setAttribute("class", "tree-edges");
  svg.setAttribute("aria-hidden", "true");
  stage.appendChild(svg);

  // Insert key; returns a description of what happened, for the caption.
  function insert(key) {
    const events = { splits: [], risen: [], grew: false };
    if (!root) {
      root = newNode([key]);
      return events;
    }
    if (contains(root, key)) return null;
    if (root.keys.length === MAX_KEYS) {
      const old = root;
      root = newNode([], [old]);
      split(root, 0, events);
      events.grew = true;
    }
    let n = root;
    for (;;) {
      let i = n.keys.findIndex((k) => key < k);
      if (i < 0) i = n.keys.length;
      if (n.children.length === 0) {
        n.keys.splice(i, 0, key);
        return events;
      }
      if (n.children[i].keys.length === MAX_KEYS) {
        split(n, i, events);
        if (key > n.keys[i]) i++;
      }
      n = n.children[i];
    }
  }

  // Split the full child parent.children[i]; its middle key moves up.
  function split(parent, i, events) {
    const child = parent.children[i];
    const mid = Math.floor(MAX_KEYS / 2);
    const middle = child.keys[mid];
    const right = newNode(child.keys.slice(mid + 1), child.children.slice(mid + 1));
    child.keys = child.keys.slice(0, mid);
    child.children = child.children.slice(0, mid + 1);
    parent.keys.splice(i, 0, middle);
    parent.children.splice(i + 1, 0, right);
    events.splits.push(child.keys.concat([middle], right.keys));
    events.risen.push(middle);
  }

  function contains(n, key) {
    while (n) {
      if (n.keys.includes(key)) return true;
      let i = n.keys.findIndex((k) => key < k);
      if (i < 0) i = n.keys.length;
      n = n.children[i];
    }
    return false;
  }

  // Lay the tree out: leaves side by side, each parent centred over its children.
  function layout() {
    const pos = new Map();
    const width = stage.clientWidth;
    const levels = [];
    (function walk(n, d) {
      (levels[d] ||= []).push(n);
      n.children.forEach((c) => walk(c, d + 1));
    })(root, 0);
    const depth = levels.length;
    const leaves = levels[depth - 1];
    const w = (n) => n.keys.length * KEY_W + 4;
    const gap = 14;
    const total = leaves.reduce((s, n) => s + w(n), 0) + gap * (leaves.length - 1);
    const scale = Math.min(1, (width - 24) / total);
    let x = (width - total * scale) / 2;
    const top = Math.max(18, (stage.clientHeight - (depth - 1) * LEVEL_GAP - NODE_H) / 2);
    for (const n of leaves) {
      pos.set(n.id, { x, y: top + (depth - 1) * LEVEL_GAP, w: w(n) });
      x += (w(n) + gap) * scale;
    }
    for (let d = depth - 2; d >= 0; d--) {
      for (const n of levels[d]) {
        const first = pos.get(n.children[0].id);
        const last = pos.get(n.children[n.children.length - 1].id);
        const cx = (first.x + last.x + last.w * scale) / 2;
        pos.set(n.id, { x: cx - (w(n) * scale) / 2, y: top + d * LEVEL_GAP, w: w(n) });
      }
    }
    return { pos, scale, levels };
  }

  function render(events, insertedKey) {
    if (!root) return;
    const { pos, scale, levels } = layout();
    const alive = new Set();
    const risen = new Set(events ? events.risen : []);
    const dur = reduceMotion ? 0 : 0.55;

    for (const level of levels) {
      for (const n of level) {
        alive.add(n.id);
        let el = els.get(n.id);
        const p = pos.get(n.id);
        const isNew = !el;
        if (isNew) {
          el = document.createElement("div");
          el.className = "tnode";
          stage.appendChild(el);
          els.set(n.id, el);
        }
        el.replaceChildren(
          ...n.keys.map((k) => {
            const s = document.createElement("span");
            s.textContent = k;
            if (k === insertedKey) s.className = "fresh";
            else if (risen.has(k)) s.className = "risen";
            return s;
          })
        );
        const transform = `translate(${p.x}px, ${p.y}px) scale(${scale})`;
        if (isNew) {
          el.style.transform = transform;
          el.style.transformOrigin = "0 0";
          if (dur) animate(el, { opacity: [0, 1] }, { duration: dur });
        } else {
          animate(el, { transform }, { duration: dur, ease: [0.2, 0.8, 0.2, 1] });
        }
        // Fade the highlight after a moment.
        setTimeout(() => el.querySelectorAll(".fresh, .risen").forEach((s) => (s.className = "")), 1400);
      }
    }
    for (const [id, el] of els) {
      if (!alive.has(id)) {
        el.remove();
        els.delete(id);
      }
    }

    // Edges: hidden during the move, redrawn at the new positions.
    svg.replaceChildren();
    for (const level of levels) {
      for (const n of level) {
        const p = pos.get(n.id);
        n.children.forEach((c, i) => {
          const q = pos.get(c.id);
          const line = document.createElementNS("http://www.w3.org/2000/svg", "line");
          const slot = (KEY_W * i + 2) * scale;
          line.setAttribute("x1", p.x + Math.min(slot, p.w * scale));
          line.setAttribute("y1", p.y + NODE_H * scale);
          line.setAttribute("x2", q.x + (q.w * scale) / 2);
          line.setAttribute("y2", q.y);
          svg.appendChild(line);
        });
      }
    }
    if (dur) animate(svg, { opacity: [0, 1] }, { duration: 0.35, delay: dur * 0.8 });
  }

  function describe(key, events) {
    if (!events) return `${key} is already in the tree. A B-tree keeps each key once.`;
    if (events.splits.length === 0) return `Inserted ${key}. It fitted into a node that had room, in sorted order.`;
    const parts = events.risen.map((m) => `${m} moved up`).join(", then ");
    return `Inserted ${key}. A full node split in two and ${parts}${events.grew ? ", so the tree grew a new level at the top" : ""}. Every leaf stays at the same depth.`;
  }

  return {
    insert(key) {
      const events = insert(key);
      caption.textContent = describe(key, events);
      if (events) render(events, key);
    },
    reset() {
      root = null;
      for (const el of els.values()) el.remove();
      els.clear();
      svg.replaceChildren();
      caption.textContent = "Empty tree. Insert a number to start.";
    },
    relayout() {
      render(null, null);
    },
  };
}

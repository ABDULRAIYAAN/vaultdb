// Builds the playground into web/dist:
//   - bundles src/main.js (and the CSS and fonts it imports) with esbuild,
//     giving each file a content hash so browsers can cache it forever
//   - copies the HTML pages, pointing them at the hashed files
//   - copies images and public files as they are
//
// Usage: npm install && npm run build
// NODE_DEPS can point at a node_modules folder elsewhere.
import { createRequire } from "node:module";
import { cpSync, existsSync, mkdirSync, readFileSync, readdirSync, rmSync, writeFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const here = dirname(fileURLToPath(import.meta.url));
const src = join(here, "src");
const dist = join(here, "dist");
const deps = process.env.NODE_DEPS || join(here, "node_modules");
const esbuild = createRequire(join(deps, "_.js"))("esbuild");

// Fonts are copied next to the CSS so esbuild can hash and bundle them.
const fonts = {
  "bricolage.woff2": "@fontsource-variable/bricolage-grotesque/files/bricolage-grotesque-latin-wght-normal.woff2",
  "jbmono-400.woff2": "@fontsource/jetbrains-mono/files/jetbrains-mono-latin-400-normal.woff2",
  "jbmono-600.woff2": "@fontsource/jetbrains-mono/files/jetbrains-mono-latin-600-normal.woff2",
};
mkdirSync(join(src, "fonts"), { recursive: true });
for (const [name, from] of Object.entries(fonts)) {
  cpSync(join(deps, from), join(src, "fonts", name));
}

rmSync(dist, { recursive: true, force: true });
const result = await esbuild.build({
  entryPoints: { app: join(src, "main.js") },
  bundle: true,
  minify: true,
  format: "esm",
  target: ["es2020", "safari14"],
  outdir: join(dist, "assets"),
  entryNames: "[name]-[hash]",
  assetNames: "fonts/[name]-[hash]",
  loader: { ".woff2": "file" },
  nodePaths: [deps],
  metafile: true,
  legalComments: "none",
});

const outputs = Object.keys(result.metafile.outputs).map((p) => p.split("/assets/")[1]);
const js = outputs.find((f) => /^app-.*\.js$/.test(f));
const css = outputs.find((f) => /^app-.*\.css$/.test(f));

for (const page of readdirSync(src).filter((f) => f.endsWith(".html"))) {
  const html = readFileSync(join(src, page), "utf8")
    .replaceAll("/assets/app.js", `/assets/${js}`)
    .replaceAll("/assets/app.css", `/assets/${css}`);
  writeFileSync(join(dist, page), html);
}
cpSync(join(src, "img"), join(dist, "img"), { recursive: true });
if (existsSync(join(src, "public"))) cpSync(join(src, "public"), dist, { recursive: true });

let total = 0;
for (const [file, info] of Object.entries(result.metafile.outputs)) total += info.bytes;
console.log(`built ${js}, ${css} and fonts: ${(total / 1024).toFixed(0)} KB total`);

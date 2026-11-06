// Compiles the Go engine to WebAssembly for the website:
//   src/vaultdb.wasm  the engine (cmd/vaultdb-wasm)
//   src/wasm_exec.js  Go's loader, copied from the same Go install so the
//                     two always match
//
// Usage: npm run wasm   (needs Go on PATH)
import { execFileSync } from "node:child_process";
import { copyFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const here = dirname(fileURLToPath(import.meta.url));
const root = join(here, "..");
const env = { ...process.env, GOOS: "js", GOARCH: "wasm" };

execFileSync("go", ["build", "-trimpath", "-ldflags=-s -w", "-o", join(here, "src", "vaultdb.wasm"), "./cmd/vaultdb-wasm"], {
  cwd: root,
  env,
  stdio: "inherit",
});
const goroot = execFileSync("go", ["env", "GOROOT"], { encoding: "utf8" }).trim();
copyFileSync(join(goroot, "lib", "wasm", "wasm_exec.js"), join(here, "src", "wasm_exec.js"));
console.log("built src/vaultdb.wasm");

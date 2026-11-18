// main.ts — the entrypoint the suite runs as `node /bot/main.ts`.
// It spawns the committed static Go binary ./forge with inherited
// stdio and environment, and exits with the binary's exit code. The
// binary owns the whole lifecycle: it binds a port, registers with the
// env, serves, and exits when the env reports done.
import { spawn } from "node:child_process";
import { fileURLToPath } from "node:url";
import path from "node:path";

const dir = path.dirname(fileURLToPath(import.meta.url));
const forge = path.join(dir, "forge");

const child = spawn(forge, [], { stdio: "inherit", env: process.env });
child.on("error", (err) => {
  console.error(`failed to start ${forge}: ${err.message}`);
  process.exit(1);
});
child.on("exit", (code, signal) => {
  if (signal) process.kill(process.pid, signal);
  process.exit(code ?? 0);
});

// Entrypoint: launch the vendored static forge binary next to this file.
// The binary does all the work; this launcher only forwards stdio, env and
// signals, then exits with the child's code.
import { spawn } from "node:child_process";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";

const dir = dirname(fileURLToPath(import.meta.url));
const bin = join(dir, "forge");

const child = spawn(bin, [], { stdio: "inherit", env: process.env });

child.on("error", (err) => {
  console.error(err);
  process.exit(1);
});

child.on("exit", (code, signal) => {
  if (signal) {
    process.kill(process.pid, signal);
  } else {
    process.exit(code ?? 0);
  }
});

process.on("SIGTERM", () => child.kill("SIGTERM"));
process.on("SIGINT", () => child.kill("SIGINT"));

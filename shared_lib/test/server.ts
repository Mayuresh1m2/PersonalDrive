// Starts a throwaway pdrive server (plain HTTP, temp dirs) for integration tests.
import { execFileSync, spawn, type ChildProcess } from "node:child_process";
import { existsSync, mkdirSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { createServer } from "node:net";

const here = dirname(fileURLToPath(import.meta.url));
export const repoRoot = resolve(here, "../..");

export function pdriveBinary(): string {
  const bin = join(repoRoot, "server/bin/pdrive");
  if (!existsSync(bin)) {
    execFileSync("make", ["-C", join(repoRoot, "server"), "build"], { stdio: "inherit" });
  }
  return bin;
}

async function freePort(): Promise<number> {
  return new Promise((res) => {
    const s = createServer();
    s.listen(0, "127.0.0.1", () => {
      const port = (s.address() as { port: number }).port;
      s.close(() => res(port));
    });
  });
}

export interface TestServer {
  url: string;
  dir: string;
  pdrive: (...args: string[]) => string;
  stop: () => Promise<void>;
}

/** Starts a server with users alice (admin) and bob, password "<name> test password". */
export async function startServer(): Promise<TestServer> {
  const bin = pdriveBinary();
  const dir = mkdtempSync(join(tmpdir(), "pdrive-sdk-"));
  const port = await freePort();
  const config = join(dir, "config.yaml");
  mkdirSync(join(dir, "disk-a"));
  const pdrive = (...args: string[]) =>
    execFileSync(bin, ["--config", config, ...args], { encoding: "utf8", input: args.includes("user") ? `${args.at(-1)} test password\n` : "" });
  pdrive("init", "--library", join(dir, "disk-a"), "--state", join(dir, "state"), "--listen", `127.0.0.1:${port}`, "--no-tls");
  writeFileSync(config, (await import("node:fs")).readFileSync(config, "utf8").replace("mdns: true", "mdns: false"));
  execFileSync(bin, ["--config", config, "user", "add", "alice", "--admin"], { input: "alice test password\n" });
  execFileSync(bin, ["--config", config, "user", "add", "bob"], { input: "bob test password\n" });

  const proc: ChildProcess = spawn(bin, ["--config", config, "serve"], { stdio: "ignore" });
  const url = `http://127.0.0.1:${port}`;
  for (let i = 0; i < 100; i++) {
    try {
      if ((await fetch(url + "/healthz")).ok) break;
    } catch {
      /* not up yet */
    }
    await new Promise((r) => setTimeout(r, 100));
  }
  return {
    url,
    dir,
    pdrive,
    stop: async () => {
      proc.kill();
      await new Promise((r) => proc.once("exit", r));
      rmSync(dir, { recursive: true, force: true });
    },
  };
}

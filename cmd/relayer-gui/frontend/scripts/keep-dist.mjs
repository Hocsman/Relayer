import { cp, mkdir, writeFile } from "node:fs/promises";

const distribution = new URL("../dist/", import.meta.url);
await mkdir(distribution, { recursive: true });
await writeFile(new URL(".gitkeep", distribution), "", { flag: "w" });

const serverDist = new URL("../../../../internal/server/dist/", import.meta.url);
try {
  await mkdir(serverDist, { recursive: true });
  await cp(distribution, serverDist, { recursive: true });
} catch (err) {
  console.warn("Could not copy assets to internal/server/dist:", err);
}

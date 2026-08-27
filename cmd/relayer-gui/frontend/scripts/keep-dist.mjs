import { mkdir, writeFile } from "node:fs/promises";

const distribution = new URL("../dist/", import.meta.url);
await mkdir(distribution, { recursive: true });
await writeFile(new URL(".gitkeep", distribution), "", { flag: "w" });

import { readFile, writeFile } from "node:fs/promises";
import { join } from "node:path";
import type { Extension } from "../index";

// A small persistence example, not a transcript database. For concurrent external
// writers or substantial history, use SQLite transactions and a retention policy.
export default ((proxy) => {
  const path = join(proxy.getDataDir(), "completed.json");
  proxy.on("finish", async ({ outcome }) => {
    if (outcome !== "complete") return;
    let completed = 0;
    try {
      completed = JSON.parse(await readFile(path, "utf8")).completed;
    } catch (error) {
      if ((error as NodeJS.ErrnoException).code !== "ENOENT") throw error;
    }
    await writeFile(path, JSON.stringify({ completed: completed + 1 }));
    proxy.log("info", "Saved completed request count");
  });
}) satisfies Extension;

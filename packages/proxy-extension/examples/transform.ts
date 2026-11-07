import type { ExtensionAPI } from "../index";

export default function (proxy: ExtensionAPI) {
  proxy.on("prepared", (request, context) => {
    if (context.threadId) proxy.updateThread<number>((count) => (count ?? 0) + 1);
    if (request.path !== "/v1/responses" || !request.body) return;
    const input = JSON.parse(request.body);
    return { body: JSON.stringify({ ...input, instructions: "Use concise answers." }) };
  });
  proxy.on("response", ({ headers }) => ({ headers: { ...headers, "X-Example-Extension": ["active"] } }));
}

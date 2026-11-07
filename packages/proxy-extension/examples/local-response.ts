import type { ExtensionAPI } from "../index";

// Operator-provisioned example, not enabled or installed by default.
export default function (proxy: ExtensionAPI) {
  proxy.on("request", (request) => {
    if (request.path === "/v1/responses/compact" && request.headers["X-Example-Local"]?.[0] === "true") {
      return { response: { status: 200, headers: { "Content-Type": ["application/json"] }, body: '{"output":[]}' } };
    }
  });
}

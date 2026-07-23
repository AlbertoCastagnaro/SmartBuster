// status.ts — HTTP status classification for the tree/findings views (spec
// §4.3: "403/protected paths are visually marked"). Only status-code-driven
// signals are implemented: a real open-listing directory can't be
// distinguished from an ordinary 200 without inspecting response content,
// which the wire doesn't carry (and shouldn't have to, to stay lightweight)
// — so that part of the spec's wording is left alone rather than faked with
// an unreliable size-based guess.

export type StatusClass = "ok" | "redirect" | "protected" | "error" | "other";

export function classifyStatus(status: number): StatusClass {
  if (status === 401 || status === 403) return "protected";
  if (status >= 200 && status < 300) return "ok";
  if (status >= 300 && status < 400) return "redirect";
  if (status >= 400) return "error";
  return "other";
}

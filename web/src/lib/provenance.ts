// provenance.ts — the single source of truth for how a Candidate/Finding's
// Provenance string (spec §5: "the story") maps to a stable category, label,
// and color. Every view (tree, frontier, findings) imports this instead of
// rolling its own logic, so a color/tag means the same thing everywhere.
//
// Wire format recap (internal/engine): Provenance is a "+"-joined union
// (seed.UnionProvenance) of individual source tags, and corpus's own tag is
// itself "corpus:" + tags-joined-by-"+" (internal/corpus/select.go) — so the
// two "+" uses nest ambiguously in the raw string (e.g.
// "wordlist+corpus:php+wordpress" splits into ["wordlist","corpus:php",
// "wordpress"], where "wordpress" is really a continuation of corpus's tag
// list, not its own source). parseProvenance resolves that the same way a
// human reading the string would: a token starting a new source only if it
// carries a recognized "prefix:" or is itself a known bare source name;
// anything else extends the previous source's tag list.

export type ProvenanceCategory =
  | "wordlist"
  | "corpus"
  | "crawl:html"
  | "crawl:js"
  | "wayback"
  | "robots"
  | "sitemap"
  | "nmap"
  | "user"
  | "headless"
  | "generated"
  | "probe"
  | "unknown";

export interface ProvenanceTag {
  category: ProvenanceCategory;
  /** Sub-detail after the category's own prefix, e.g. "disallow" for robots:disallow, "/admin" for recursion. */
  detail?: string;
  /** Additional bare tags folded in (corpus tech tags: "php", "wordpress", ...). */
  tags: string[];
}

// Bare (no-colon) tokens that are themselves a complete source, not a
// continuation of the previous group's tag list.
const BARE_CATEGORIES = new Set(["wordlist", "sitemap", "headless", "user", "probe"]);

function categoryFor(prefix: string, detail: string): ProvenanceCategory {
  switch (prefix) {
    case "wordlist":
    case "recursion": // Phase 1 fallback recursing into a subdir — same source as wordlist, just deeper
      return "wordlist";
    case "corpus":
      return "corpus";
    case "crawl":
      return detail === "js" ? "crawl:js" : "crawl:html";
    case "wayback":
      return "wayback";
    case "robots":
      return "robots";
    case "sitemap":
      return "sitemap";
    case "nmap":
      return "nmap";
    case "user":
      return "user";
    case "headless":
      return "headless";
    case "generated":
      return "generated";
    case "probe":
      return "probe";
    default:
      return "unknown";
  }
}

/** Parses a raw Provenance/Provenance-union string into one tag per distinct source. */
export function parseProvenance(raw: string | undefined | null): ProvenanceTag[] {
  if (!raw) return [];
  const tokens = raw.split("+").filter(Boolean);
  const groups: ProvenanceTag[] = [];

  for (const token of tokens) {
    const colon = token.indexOf(":");
    const startsNewGroup = colon !== -1 || BARE_CATEGORIES.has(token);
    if (startsNewGroup) {
      const prefix = colon === -1 ? token : token.slice(0, colon);
      const detail = colon === -1 ? "" : token.slice(colon + 1);
      groups.push({ category: categoryFor(prefix, detail), detail: detail || undefined, tags: [] });
    } else if (groups.length > 0) {
      groups[groups.length - 1].tags.push(token);
    } else {
      // A stray bare token with nothing to attach to (shouldn't happen in
      // practice, but fall back to its own unknown-category group rather
      // than silently dropping it).
      groups.push({ category: "unknown", detail: token, tags: [] });
    }
  }
  return groups;
}

export interface ProvenanceStyle {
  label: string;
  /** CSS custom property name (see app.css `--prov-*`) carrying this category's color. */
  colorVar: string;
}

const STYLES: Record<ProvenanceCategory, ProvenanceStyle> = {
  wordlist: { label: "wordlist", colorVar: "--prov-wordlist" },
  corpus: { label: "corpus", colorVar: "--prov-corpus" },
  "crawl:html": { label: "crawl:html", colorVar: "--prov-crawl-html" },
  "crawl:js": { label: "crawl:js", colorVar: "--prov-crawl-js" },
  wayback: { label: "wayback", colorVar: "--prov-wayback" },
  robots: { label: "robots", colorVar: "--prov-robots" },
  sitemap: { label: "sitemap", colorVar: "--prov-sitemap" },
  nmap: { label: "nmap", colorVar: "--prov-nmap" },
  user: { label: "user", colorVar: "--prov-user" },
  headless: { label: "headless", colorVar: "--prov-headless" },
  generated: { label: "generated", colorVar: "--prov-generated" },
  probe: { label: "probe", colorVar: "--prov-probe" },
  unknown: { label: "?", colorVar: "--prov-unknown" },
};

export function styleFor(category: ProvenanceCategory): ProvenanceStyle {
  return STYLES[category];
}

/** A short, deterministic display string for a full provenance union, e.g. "corpus+crawl:html". */
export function provenanceLabel(raw: string | undefined | null): string {
  const tags = parseProvenance(raw);
  if (tags.length === 0) return "unknown";
  return tags.map((t) => STYLES[t.category].label).join("+");
}

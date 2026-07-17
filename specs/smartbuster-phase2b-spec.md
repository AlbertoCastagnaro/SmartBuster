# smartbuster — Phase 2b Build Specification (Corpus & Selection)

*Build-ready spec for the tagged corpus, profile-driven selection, runtime extension generation, and the per-candidate tech scoring that finally makes detection change what gets fired first. Consumes the Phase 2a `TargetProfile`. Read alongside the Phase 2a spec and the implementation plan (Phase 2/3 boundary).*

---

## 0. Integration contract — verify against committed code first

2b binds to Phase 1 and 2a internals. The 2a handoff already confirmed the key signatures; confirm the rest against the code.

| # | Attachment point | Confirmed / assumption | Action |
|---|---|---|---|
| A | `TargetProfile.MatchScore(tags []string) float64` | **confirmed present** (2a), returns [0,1] | 2b's scoring calls it |
| B | `TargetProfile.ExtensionsForStack() []string` | **confirmed present** (2a) | 2b uses it for stem→file expansion (same set calibration probes) |
| C | `Frontier.Reprioritize(fn func(*Candidate))` | **confirmed present, unused** (2a) | 2b drives it on profile finalize/refine |
| D | `Candidate` struct | Phase 1 has `Path,Type,BasePrio,Score,Depth,ParentDir,Provenance`; **no `Tags`** | 2b **adds `Tags []string`** and populates it from the corpus |
| E | Root-dir seeding call site | Phase 1 seeds the root from `wordlist.Load(file)` → `[]Candidate` | 2b **replaces** the source with `corpus.Select(profile)`; `-w file` still supported as a fallback/override |
| F | Frontier `Score` formula | Phase 1 `Score = BasePrio` | 2b sets `Score = BasePrio * (1 + TECH_BOOST_W * MatchScore(Tags))` (§5) |
| G | `wordlist` package | loads a flat file | keep it for `-w`; corpus is the new default path |

**Sequencing:** scan start becomes: 2a `profileTarget()` → **2b `corpus.Select(provisionalProfile)` seeds the frontier (tagged candidates)** → root calibration (extensions from 2a) → scan. When 2a's `RefineAfterCalibration` updates the profile, fire `Frontier.Reprioritize(applyMatchScore)` so existing candidates re-rank (this is what compensates on the selection side for 2a note ii).

## 1. Scope

**In (2b):** tagged corpus store (SQLite) + ingestion pipeline from SecLists; `corpus.Select(profile)`; runtime `stem × ExtensionsForStack` expansion + backup-file generation; per-candidate tech scoring via `MatchScore`; candidate-set dedup with provenance merge; user-wordlist import (tagged); "reorder-not-exclude" guarantee.

**Out (→ Phase 3):** co-occurrence tables, online Markov naming-convention learning, response-semantics boosting, exploration/exploitation. (The `Reprioritize` hook 2b establishes is what Phase 3 extends with more signals.)

## 2. Corpus store (`internal/corpus/`) — SQLite via `modernc.org/sqlite`

DDL:
```sql
CREATE TABLE terms (
  id      INTEGER PRIMARY KEY,
  term    TEXT NOT NULL,                    -- "admin", "config.php", "wp-login.php"
  type    INTEGER NOT NULL,                 -- 0 dir,1 file,2 stem,3 fullpath (matches CandidateType)
  weight  REAL NOT NULL,                    -- commonality prior in [0,1]
  UNIQUE(term, type)
);
CREATE TABLE term_tags (
  term_id INTEGER NOT NULL REFERENCES terms(id),
  tag     TEXT NOT NULL,                    -- "generic","php","wordpress","tomcat","backup",...
  PRIMARY KEY(term_id, tag)
);
CREATE TABLE ingest_meta (
  key TEXT PRIMARY KEY, value TEXT          -- seclists commit, ingest timestamp, source_map hash
);
CREATE INDEX idx_term_tags_tag ON term_tags(tag);
CREATE INDEX idx_terms_weight  ON terms(weight DESC);
```
The DB ships **prebuilt** (a `smartbuster corpus build` command produces it from a SecLists checkout); the binary loads it read-only at scan time. An **embedded minimal corpus** (raft-small + a few CMS lists) is bundled via `embed.FS` so the tool works with zero setup; `corpus build` upgrades to the full set. `cooccurrence` table is created by Phase 3, not here.

## 3. Ingestion pipeline (`internal/corpus/ingest.go`, `smartbuster corpus build`)

Input: a SecLists checkout path + a declarative **source map** (`sourcemap.yaml`, user-editable). Steps:

1. **Walk** SecLists per the source map's globs.
2. For each matched file, assign `{type, tags}` from its rule.
3. **Read + clean** lines (trim, drop comments/blanks, drop lines with control chars, cap length).
4. **Commonality scoring** (§4).
5. **Upsert** into `terms`/`term_tags`, deduping by `(term,type)` and unioning tags.
6. Record `ingest_meta` (SecLists commit if a git dir, source_map hash) for reproducibility.

Source-map format and starter rules:
```yaml
# glob (relative to SecLists root) : { type, tags, freq_rank? }
Discovery/Web-Content/directory-list-2.3-medium.txt: { type: dir,  tags: [generic], freq_rank: true }
Discovery/Web-Content/raft-*-directories*.txt:        { type: dir,  tags: [generic] }
Discovery/Web-Content/raft-*-files*.txt:              { type: file, tags: [generic] }
Discovery/Web-Content/CMS/wordpress*.txt:             { type: file, tags: [wordpress, php] }
Discovery/Web-Content/CMS/Joomla*.txt:                { type: file, tags: [joomla, php] }
Discovery/Web-Content/CMS/Drupal*.txt:                { type: file, tags: [drupal, php] }
Discovery/Web-Content/*PHP*.txt:                      { type: file, tags: [php] }
Discovery/Web-Content/apache*.txt:                    { type: dir,  tags: [apache] }
Discovery/Web-Content/tomcat*.txt:                    { type: dir,  tags: [tomcat, java] }
Discovery/Web-Content/IIS-*.txt:                      { type: file, tags: [iis, aspnet] }
Discovery/Web-Content/Common-DB-Backups.txt:          { type: file, tags: [backup] }
Discovery/Web-Content/backup-files.txt:               { type: file, tags: [backup] }
Discovery/Web-Content/api/*.txt:                       { type: dir,  tags: [api] }
```
`freq_rank: true` marks the list whose *line order* encodes real-world frequency (directory-list-2.3), used as the primary commonality signal.

## 4. Commonality scoring

Produce `weight ∈ [0,1]` per term:
- **Rank signal (primary):** for the `freq_rank` list, `rank_score = 1 - position/len` (line 0 → ~1.0). Terms absent from it get `rank_score = 0`.
- **Corroboration signal:** `presence = count(distinct source files containing term) / totalFiles`.
- **Combine:** `weight = 0.7 * rank_score + 0.3 * normalize(presence)`. Terms in *no* frequency list but in several wordlists still get a modest weight via `presence` (so tech-specific terms like `wp-config.php` aren't buried at 0). Clamp to `[0.01, 1.0]` so nothing is exactly zero (nothing is unreachable — reorder-not-exclude).

## 5. Selection-as-query (`corpus.Select`)

```go
func Select(p *profile.TargetProfile, cfg SelectConfig) []Candidate
```
1. **Tag set** = backend-layer tech tags from `p` ∪ `{"generic"}` ∪ (`{"backup"}` always — backups exist regardless of stack). Edge-only techs (e.g. Cloudflare) contribute nothing to selection.
2. **Query** (weight-ordered, bounded):
```sql
SELECT t.term, t.type, t.weight, group_concat(tt.tag) AS tags
FROM terms t JOIN term_tags tt ON tt.term_id = t.id
WHERE tt.tag IN (:tagset)
GROUP BY t.id
ORDER BY t.weight DESC
LIMIT :maxCandidates;                         -- default 0 = unbounded (see perf note)
```
3. **Build candidates**: `BasePrio = weight`, `Tags = tags`, `Provenance = "corpus:" + join(tags)`.
4. **Score** each: `Score = BasePrio * (1 + TECH_BOOST_W * p.MatchScore(Tags))` (`TECH_BOOST_W` default 2.0). Generic-only terms → `MatchScore≈0` → unboosted but present. WordPress term on a WP target → strong boost. **Never excluded**, only reordered.
5. **Expand + dedup** (§6), push into the frontier.

**Perf note:** for the big lists (directory-list-2.3-big ≈ 1.2M lines), unbounded resident candidates are heavy. 2b default uses the **medium** corpus (raft + directory-list-2.3-medium, tens of thousands) resident. A `--corpus-stream` mode (pull next weight-descending batch from SQLite as the frontier drains) is noted as a Phase 7 perf option; not required for 2b. `Reprioritize` operates on resident candidates only.

## 6. Runtime extension generation & dedup (`corpus/expand.go`)

- **`type=file` / `fullpath`**: use as-is.
- **`type=stem`** (extensionless, from stem lists): expand to `stem + ext` for each `ext ∈ p.ExtensionsForStack()`. Each expansion inherits the stem's `BasePrio` (slightly decayed per extra ext so the bare/primary form ranks first).
- **`type=dir`**: dir candidate (recursion-eligible per Phase 1).
- **Ambiguous generic words** (from directory-list): apply Phase 1's dot-heuristic; additionally, **only when a backend stack is detected**, treat an extensionless generic word as a stem and expand it (`config → config.php`). This gates the combinatorial blowup to stacked targets.
- **Backup generation**: for sensitive stems/files (`config`, `.env`, `wp-config.php`, `web.config`, `settings`, `database`, `backup`), also emit `<name>.bak/.old/.zip/.tar.gz/.swp/~` and (for `x.php`) `x.php.bak`. Give backups-of-sensitive-files a priority bump (`BACKUP_SENSITIVE_BOOST` default ×1.3) — a readable `config.php.bak` is high value.
- **Dedup**: collapse duplicate `Path`s across layers; keep the **max** score; **union tags and provenance** (so a term from generic+wordpress records both, and the audit shows why it was tried).

## 7. Frontier integration & reprioritization

- Selected+expanded candidates are pushed with `Score` set per §5.4.
- `applyMatchScore(c *Candidate)` recomputes `c.Score = c.BasePrio * (1 + TECH_BOOST_W * profile.MatchScore(c.Tags))`.
- Call `Frontier.Reprioritize(applyMatchScore)` (a) once when the provisional profile is finalized just before scan, and (b) whenever 2a's `RefineAfterCalibration` mutates the profile. Guard against thrash: only reprioritize if the profile's tech set/confidences actually changed.
- New candidates created mid-scan (recursion children in Phase 1) inherit the corpus term set for their directory via `Select` scoped to that subtree, scored the same way.

## 8. Config additions
```go
CorpusDB       string   // path to prebuilt DB; "" → embedded minimal corpus
SecListsPath   string   // for `corpus build`
SourceMap      string   // sourcemap.yaml; "" → embedded default
CorpusMax      int      // Select LIMIT; 0 = unbounded (medium corpus default)
TechBoostW     float64  // default 2.0
CorpusStream   bool     // default false (Phase 7 perf)
```
`-w <file>` still works: bypasses the corpus, loads a flat list tagged `generic` (Phase 1 behavior preserved as an override). `smartbuster corpus build --seclists <path> [--source-map f] [--out db]` builds the DB; `corpus import <file> --tags a,b --type dir|file` adds a user list.

## 9. Tests, fixtures, DoD

**Fixtures:** a tiny synthetic SecLists tree (`test/fixtures/seclists/`) exercising the source map (a freq-ranked list, a raft-dir list, a wordpress list, a backup list); reuse Phase 2a's `wordpress`, `php_apache`, `dotnet_iis`, and a stack-less `honest` HTTP fixture.

**Assertions / DoD:**
1. **Ingestion**: the synthetic tree produces correct `(term,type,tags,weight)`; a term in multiple lists is deduped with unioned tags; `freq_rank` list drives ordering.
2. **Selection ordering**: on a WordPress+PHP profile, `wp-*`/`.php` terms outrank generic; on a stack-less target, order is pure commonality; **generic terms are present in every case** (assert a known generic term is never dropped — reorder-not-exclude).
3. **Scoring**: `Score = BasePrio*(1+2*MatchScore)`; a generic term and a matched term with equal `BasePrio` rank matched-first; the generic one is still enqueued.
4. **Extension expansion**: on a PHP profile a `stem` yields `.php` variants; `config` → `config.php` only when a stack is detected; `config.php` → `config.php.bak` with the sensitivity boost.
5. **Dedup**: a term from generic+wordpress becomes one candidate with both tags in provenance.
6. **Reprioritize**: mutate the profile post-calibration (add a backend) → `Frontier.Reprioritize` re-ranks resident candidates; no thrash when profile is unchanged.
7. **`-w file` override** still works and tags everything `generic`.
8. **Integration / efficiency**: a scan against the WordPress fixture reaches the planted wp-paths in **materially fewer requests** than the Phase 1 flat-order baseline (this is the first concrete requests-to-coverage win — record the number; it seeds the Phase 7 eval).
9. `go build ./...`, `go vet ./...`, `go test -race ./...` clean; CLI smoke test on a WP-signaling server shows wp-paths fired early.

## 10. Build order & handoff
`corpus/schema` + `modernc.org/sqlite` open/migrate → `sourcemap` loader → `ingest` + `corpus build` CLI → `commonality scoring` → `Select` (query + candidate build) → `expand` (extensions/backups/dedup) → scoring + `applyMatchScore` → engine wiring (add `Candidate.Tags` per contract D; swap root seeding to `Select` per E; `Reprioritize` calls per C/§7). Write the SecLists fixture tree + ingestion/selection table-tests **before** engine wiring, so corpus behavior is validated standalone.

**Before writing Phase 3, report back:** the final `Candidate` shape (with `Tags`), the `Select`/`applyMatchScore` signatures, how reprioritization is triggered, and any §0 deviation — Phase 3 (co-occurrence, Markov, response-semantics) plugs additional terms into the very same `Reprioritize` scoring path.

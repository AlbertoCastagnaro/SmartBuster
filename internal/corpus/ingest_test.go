package corpus

import (
	"database/sql"
	"os"
	"testing"
)

// seclistsFixtureRoot is the synthetic SecLists tree spec §9 asks for: a
// freq-ranked list, a raft-dir list, a wordpress list, a backup list.
const seclistsFixtureRoot = "../../test/fixtures/seclists"
const seclistsFixtureSourceMap = "../../test/fixtures/seclists_sourcemap.yaml"

func loadFixtureSourceMap(t *testing.T) *SourceMap {
	t.Helper()
	sm, err := LoadSourceMap(seclistsFixtureSourceMap)
	if err != nil {
		t.Fatal(err)
	}
	return sm
}

func openMemDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestIngest_ProducesCorrectTermTypeTagsWeight(t *testing.T) {
	db := openMemDB(t)
	sm := loadFixtureSourceMap(t)

	if _, err := Ingest(db, os.DirFS(seclistsFixtureRoot), seclistsFixtureRoot, sm, "testhash"); err != nil {
		t.Fatal(err)
	}

	// "admin" appears in both the freq_rank list (position 0) and the
	// raft-dir list, both type:dir, both tagged "generic" — one row, tags
	// unioned, and its rank_score should dominate (weight near 1.0).
	var weight float64
	var typ int
	if err := db.QueryRow(`SELECT weight, type FROM terms WHERE term = 'admin'`).Scan(&weight, &typ); err != nil {
		t.Fatal(err)
	}
	if typ != int(TypeDir) {
		t.Errorf("admin type = %d, want %d (dir)", typ, TypeDir)
	}
	if weight < 0.9 {
		t.Errorf("admin weight = %v, want close to 1.0 (rank 0 in the freq_rank list)", weight)
	}

	// wp-login.php: type file, tagged wordpress+php.
	var wpTags []string
	rows, err := db.Query(`SELECT tt.tag FROM terms t JOIN term_tags tt ON tt.term_id = t.id WHERE t.term = 'wp-login.php'`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var tag string
		rows.Scan(&tag)
		wpTags = append(wpTags, tag)
	}
	rows.Close()
	if !containsTag(wpTags, "wordpress") || !containsTag(wpTags, "php") {
		t.Errorf("wp-login.php tags = %v, want [wordpress php]", wpTags)
	}

	// "config" is in the freq_rank list only (last line -> low rank_score);
	// still clamped to a nonzero weight (reorder-not-exclude, spec §4).
	var configWeight float64
	if err := db.QueryRow(`SELECT weight FROM terms WHERE term = 'config'`).Scan(&configWeight); err != nil {
		t.Fatal(err)
	}
	if configWeight < 0.01 {
		t.Errorf("config weight = %v, want >= 0.01 (clamped floor)", configWeight)
	}
	if configWeight >= weight {
		t.Errorf("config weight (%v, last in freq_rank list) should be less than admin's (%v, first)", configWeight, weight)
	}
}

func TestIngest_FreqRankListDrivesOrdering(t *testing.T) {
	db := openMemDB(t)
	sm := loadFixtureSourceMap(t)
	if _, err := Ingest(db, os.DirFS(seclistsFixtureRoot), seclistsFixtureRoot, sm, "testhash"); err != nil {
		t.Fatal(err)
	}

	rows, err := db.Query(`SELECT term FROM terms WHERE type = ? ORDER BY weight DESC`, int(TypeDir))
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var order []string
	for rows.Next() {
		var term string
		rows.Scan(&term)
		order = append(order, term)
	}

	// directory-list-2.3-medium.txt is: admin, login, images, backup, config
	// (in that rank order); admin should sort first.
	if len(order) == 0 || order[0] != "admin" {
		t.Errorf("expected admin (rank 0 in the freq_rank list) first, got order %v", order)
	}
}

func TestIngest_DedupUnionsTagsAcrossLists(t *testing.T) {
	db := openMemDB(t)
	sm := loadFixtureSourceMap(t)
	if _, err := Ingest(db, os.DirFS(seclistsFixtureRoot), seclistsFixtureRoot, sm, "testhash"); err != nil {
		t.Fatal(err)
	}

	// "admin" is a dir in both directory-list-2.3-medium.txt and
	// raft-small-directories.txt, both tagged "generic" — must collapse to
	// exactly one (term,type) row.
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM terms WHERE term = 'admin' AND type = ?`, int(TypeDir)).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("expected admin(dir) deduped to 1 row, got %d", count)
	}
}

func TestIngest_RecordsIngestMeta(t *testing.T) {
	db := openMemDB(t)
	sm := loadFixtureSourceMap(t)
	if _, err := Ingest(db, os.DirFS(seclistsFixtureRoot), seclistsFixtureRoot, sm, "testhash"); err != nil {
		t.Fatal(err)
	}
	if v, ok, err := GetMeta(db, "source_map_hash"); err != nil || !ok || v != "testhash" {
		t.Errorf("source_map_hash meta = %q, %v, %v; want \"testhash\", true, nil", v, ok, err)
	}
	if _, ok, err := GetMeta(db, "ingested_at"); err != nil || !ok {
		t.Errorf("ingested_at meta missing: ok=%v err=%v", ok, err)
	}
}

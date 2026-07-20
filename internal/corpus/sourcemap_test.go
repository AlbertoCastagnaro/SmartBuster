package corpus

import "testing"

func TestParseSourceMap_PreservesOrderAndFields(t *testing.T) {
	data := []byte(`
b-glob.txt: { type: file, tags: [php], freq_rank: false }
a-glob.txt: { type: dir,  tags: [generic], freq_rank: true }
`)
	sm, err := ParseSourceMap(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(sm.Rules) != 2 {
		t.Fatalf("expected 2 rules, got %d", len(sm.Rules))
	}
	if sm.Rules[0].Glob != "b-glob.txt" || sm.Rules[1].Glob != "a-glob.txt" {
		t.Errorf("expected document order preserved (b-glob then a-glob), got %v", sm.Rules)
	}
	if sm.Rules[1].Type != TypeDir || !sm.Rules[1].FreqRank {
		t.Errorf("a-glob.txt: expected type=dir, freq_rank=true, got %+v", sm.Rules[1])
	}
	if len(sm.Rules[0].Tags) != 1 || sm.Rules[0].Tags[0] != "php" {
		t.Errorf("b-glob.txt: expected tags=[php], got %v", sm.Rules[0].Tags)
	}
}

func TestParseSourceMap_UnknownTypeErrors(t *testing.T) {
	_, err := ParseSourceMap([]byte(`x.txt: { type: bogus, tags: [generic] }`))
	if err == nil {
		t.Fatal("expected an error for an unknown type")
	}
}

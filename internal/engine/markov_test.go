package engine

import "testing"

func TestMarkovModel_ColdStartGuard(t *testing.T) {
	m := NewMarkovModel(3, 8)
	for i := 0; i < 7; i++ {
		m.Train("get_user.php")
	}
	if got := m.convSignal("get_secret.php"); got != 0 {
		t.Errorf("expected neutral 0 before MARKOV_MIN_SAMPLES trained segments, got %v", got)
	}

	m.Train("get_role.php") // 8th sample clears the cold-start guard
	if got := m.convSignal("get_secret.php"); got == 0 {
		t.Errorf("expected a non-zero signal once the cold-start guard clears, got %v", got)
	}
}

func TestMarkovModel_ConvSignal_FavorsTrainedStyle(t *testing.T) {
	m := NewMarkovModel(3, 8)
	trained := []string{
		"get_user.php", "get_role.php", "get_order.php", "get_item.php",
		"get_status.php", "get_price.php", "get_stock.php", "get_review.php",
	}
	for _, s := range trained {
		m.Train(s)
	}

	inStyle := m.convSignal("get_secret.php")
	outOfStyle := m.convSignal("zzz9x7q.dat")
	if inStyle <= outOfStyle {
		t.Errorf("expected in-style segment to score higher: get_secret.php=%v zzz9x7q.dat=%v", inStyle, outOfStyle)
	}
	if inStyle <= 0 || inStyle >= 1 {
		t.Errorf("expected convSignal in (0,1), got %v", inStyle)
	}
}

func TestMarkovModel_ConvSignal_EmptySegment(t *testing.T) {
	m := NewMarkovModel(3, 1)
	m.Train("get_user.php")
	if got := m.convSignal(""); got != 0 {
		t.Errorf("expected 0 for an empty segment, got %v", got)
	}
}

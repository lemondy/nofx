package store

import (
	"errors"
	"testing"

	"gorm.io/gorm"
)

// review 2026-10-28 #12: Count failure differs from an absent mark, and cannot erase good cache data.
func TestReview28AIManagedCountErrorAndCache(t *testing.T) {
	st := newTestStore(t)
	s := st.AIManaged()
	if err := s.Mark("t", "AIUSDT", "long"); err != nil {
		t.Fatal(err)
	}
	if marked, err := s.IsMarkedWithError("t", "AIUSDT", "LONG"); err != nil || !marked {
		t.Fatalf("confirmed AI: %v %v", marked, err)
	}
	if marked, err := s.IsMarkedWithError("t", "MANUALUSDT", "long"); err != nil || marked {
		t.Fatalf("confirmed manual: %v %v", marked, err)
	}
	if err := st.GormDB().Callback().Query().Before("gorm:query").Register("review28:count", func(tx *gorm.DB) { tx.AddError(errors.New("count unavailable")) }); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.GormDB().Callback().Query().Remove("review28:count") })
	for _, symbol := range []string{"AIUSDT", "MANUALUSDT", "UNKNOWNUSDT"} {
		if _, err := s.IsMarkedWithError("t", symbol, "long"); err == nil {
			t.Fatalf("%s hid Count error", symbol)
		}
		marked, known := s.LastKnownMarked("t", symbol, "long")
		if known != (symbol != "UNKNOWNUSDT") || marked != (symbol == "AIUSDT") {
			t.Fatalf("%s corrupted last success: %v %v", symbol, marked, known)
		}
	}
	if _, known := s.LastKnownMarked("other", "AIUSDT", "long"); known {
		t.Fatal("cache crossed trader boundary")
	}
	if _, known := s.LastKnownMarked("t", "AIUSDT", "short"); known {
		t.Fatal("cache crossed position side")
	}
}

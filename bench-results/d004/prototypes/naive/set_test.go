package d004

import (
	"math/rand/v2"
	"slices"
	"testing"
)

// model is a sorted-slice reference implementation.
type model struct {
	desc bool
	ps   []Price
}

func (m *model) less(a, b Price) bool {
	if m.desc {
		return a > b
	}
	return a < b
}
func (m *model) idx(p Price) (int, bool) {
	return slices.BinarySearchFunc(m.ps, p, func(e, t Price) int {
		switch {
		case e == t:
			return 0
		case m.less(e, t):
			return -1
		default:
			return 1
		}
	})
}
func (m *model) has(p Price) bool { _, ok := m.idx(p); return ok }
func (m *model) insert(p Price)   { i, _ := m.idx(p); m.ps = slices.Insert(m.ps, i, p) }
func (m *model) delete(p Price)   { i, _ := m.idx(p); m.ps = slices.Delete(m.ps, i, i+1) }

func check(t *testing.T, s levelSet, m *model, step int) {
	t.Helper()
	if s.Len() != len(m.ps) {
		t.Fatalf("step %d: Len = %d, want %d", step, s.Len(), len(m.ps))
	}
	b := s.Best()
	if len(m.ps) == 0 {
		if b != nil {
			t.Fatalf("step %d: Best = %v on empty set", step, b.Price)
		}
	} else if b == nil || b.Price != m.ps[0] {
		t.Fatalf("step %d: Best = %v, want %d", step, b, m.ps[0])
	}
	var got []Price
	s.Walk(func(l *Level) bool { got = append(got, l.Price); return true })
	if !slices.Equal(got, m.ps) {
		t.Fatalf("step %d: Walk = %v, want %v", step, got, m.ps)
	}
}

func TestEmpty(t *testing.T) {
	for _, desc := range []bool{false, true} {
		s := newLevels(desc)
		if s.Len() != 0 || s.Best() != nil || s.Find(1) != nil {
			t.Fatalf("desc=%v: non-empty new set", desc)
		}
		s.Walk(func(*Level) bool { t.Fatal("Walk visited a level of an empty set"); return true })
	}
}

func TestPointerStability(t *testing.T) {
	s := newLevels(false)
	ptrs := map[Price]*Level{}
	for p := Price(1); p <= 2000; p++ {
		l := s.Insert(p * 7 % 2003)
		l.TotalQty = int64(p)
		ptrs[l.Price] = l
	}
	for p := Price(1); p <= 2000; p += 2 {
		s.Delete(p * 7 % 2003)
		delete(ptrs, p*7%2003)
	}
	for p, l := range ptrs {
		if f := s.Find(p); f != l {
			t.Fatalf("Find(%d) returned a different pointer after unrelated deletes", p)
		}
	}
}

func TestWalkStopsEarly(t *testing.T) {
	s := newLevels(true)
	for p := Price(1); p <= 100; p++ {
		s.Insert(p)
	}
	var got []Price
	s.Walk(func(l *Level) bool { got = append(got, l.Price); return len(got) < 3 })
	if !slices.Equal(got, []Price{100, 99, 98}) {
		t.Fatalf("Walk = %v", got)
	}
}

func TestAgainstModel(t *testing.T) {
	for _, desc := range []bool{false, true} {
		for seed := uint64(1); seed <= 20; seed++ {
			r := rand.New(rand.NewPCG(seed, 0))
			s, m := newLevels(desc), &model{desc: desc}
			span := Price(1 + r.IntN(500))
			for step := range 5000 {
				p := 1 + Price(r.Int64N(int64(span)))
				switch {
				case m.has(p) && r.IntN(2) == 0:
					s.Delete(p)
					m.delete(p)
				case !m.has(p):
					if s.Find(p) != nil {
						t.Fatalf("Find(%d) found a missing level", p)
					}
					if l := s.Insert(p); l == nil || l.Price != p {
						t.Fatalf("Insert(%d) = %v", p, l)
					}
					m.insert(p)
				default:
					if l := s.Find(p); l == nil || l.Price != p {
						t.Fatalf("Find(%d) = %v", p, l)
					}
				}
				if step%7 == 0 || step < 50 {
					check(t, s, m, step)
				}
			}
			for len(m.ps) > 0 { // drain from the best, as matching does
				p := m.ps[0]
				s.Delete(p)
				m.delete(p)
				check(t, s, m, -1)
			}
		}
	}
}

package d004

import "slices"

type naive struct {
	desc bool
	ls   []*Level
}

func newLevels(desc bool) levelSet { return &naive{desc: desc} }

func (s *naive) cmp(a *Level, p Price) int {
	switch {
	case a.Price == p:
		return 0
	case (a.Price < p) != s.desc:
		return -1
	default:
		return 1
	}
}
func (s *naive) Find(p Price) *Level {
	if i, ok := slices.BinarySearchFunc(s.ls, p, s.cmp); ok {
		return s.ls[i]
	}
	return nil
}
func (s *naive) Insert(p Price) *Level {
	i, _ := slices.BinarySearchFunc(s.ls, p, s.cmp)
	l := &Level{Price: p}
	s.ls = slices.Insert(s.ls, i, l)
	return l
}
func (s *naive) Delete(p Price) {
	i, _ := slices.BinarySearchFunc(s.ls, p, s.cmp)
	s.ls = slices.Delete(s.ls, i, i+1)
}
func (s *naive) Best() *Level {
	if len(s.ls) == 0 {
		return nil
	}
	return s.ls[0]
}
func (s *naive) Walk(fn func(*Level) bool) {
	for _, l := range s.ls {
		if !fn(l) {
			return
		}
	}
}
func (s *naive) Len() int { return len(s.ls) }

package ugo

type stopWaitingManager struct {
	largestLeastUnackedSent uint64
	state                   bool
}

func (s *stopWaitingManager) GetStopWaitingFrame(force bool) uint64 {
	if s.state == true {
		s.state = false
		return s.largestLeastUnackedSent
	}

	return 0
}

func (s *stopWaitingManager) SetBoundary(p uint64) {
	s.largestLeastUnackedSent = p + 1
	s.state = true
}

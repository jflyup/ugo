package ugo

type stopWaitingManager struct {
	largestLeastUnackedSent uint32
	state                   bool
}

func (s *stopWaitingManager) GetStopWaitingFrame(force bool) uint32 {
	if s.state == true {
		s.state = false
		return s.largestLeastUnackedSent
	}
	
	return 0
}

func (s *stopWaitingManager) SetBoundary(p uint32) {
	s.largestLeastUnackedSent = p + 1
	s.state = true
}

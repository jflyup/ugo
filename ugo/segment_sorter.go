package ugo

import (
	"errors"

	"github.com/jflyup/ugo/ugo/protocol"

	"github.com/jflyup/ugo/ugo/utils"
)

type segmentSorter struct {
	queuedFrames map[uint32]*segment
	readPosition uint32
	gaps         *utils.ByteIntervalList
}

var (
	errOverlappingStreamData           = errors.New("OverlappingStreamData")
	errTooManyGapsInReceivedStreamData = errors.New("Too many gaps in received StreamFrame data")
	errDuplicateStreamData             = errors.New("Overlapping Stream Data")
	errEmptyStreamData                 = errors.New("Stream Data empty")
)

func newStreamFrameSorter() *segmentSorter {
	s := segmentSorter{
		gaps:         utils.NewByteIntervalList(),
		queuedFrames: make(map[uint32]*segment),
	}
	s.gaps.PushFront(utils.ByteInterval{Start: 0, End: protocol.MaxByteCount})
	return &s
}

func (s *segmentSorter) Push(frame *segment) error {
	_, ok := s.queuedFrames[frame.Offset]
	if ok {
		return errDuplicateStreamData
	}

	start := frame.Offset
	end := frame.Offset + frame.DataLen()

	if start == end {
		return errEmptyStreamData
	}

	var foundInGap bool

	for gap := s.gaps.Front(); gap != nil; gap = gap.Next() {
		// the complete frame lies before or after the gap
		if end <= gap.Value.Start || start > gap.Value.End {
			continue
		}

		if start < gap.Value.Start {
			return errOverlappingStreamData
		}

		if start < gap.Value.End && end > gap.Value.End {
			return errOverlappingStreamData
		}

		foundInGap = true

		if start == gap.Value.Start {
			if end == gap.Value.End {
				s.gaps.Remove(gap)
				break
			}
			if end < gap.Value.End {
				gap.Value.Start = end
				break
			}
		}

		if end == gap.Value.End {
			gap.Value.End = start
			break
		}

		if end < gap.Value.End {
			intv := utils.ByteInterval{Start: end, End: gap.Value.End}
			s.gaps.InsertAfter(intv, gap)
			gap.Value.End = start
			break
		}
	}

	if !foundInGap {
		return errDuplicateStreamData
	}

	if s.gaps.Len() > protocol.MaxStreamFrameSorterGaps {
		return errTooManyGapsInReceivedStreamData
	}

	s.queuedFrames[frame.Offset] = frame
	return nil
}

func (s *segmentSorter) Pop() *segment {
	frame := s.Head()
	if frame != nil {
		s.readPosition += frame.DataLen()
		delete(s.queuedFrames, frame.Offset)
	}
	return frame
}

func (s *segmentSorter) Head() *segment {
	frame, ok := s.queuedFrames[s.readPosition]
	if ok {
		return frame
	}
	return nil
}

package ugo

import (
	"errors"

	"github.com/jflyup/ugo/ugo/protocol"

	"github.com/jflyup/ugo/ugo/utils"
)

type segmentSorter struct {
	queuedFrames map[uint64]*segment
	readPosition uint64
	gaps         *utils.ByteIntervalList
}

var (
	errOverlappingStreamData           = errors.New("OverlappingStreamData")
	errTooManyGapsInReceivedStreamData = errors.New("Too many gaps in received StreamFrame data")
	errDuplicateStreamData             = errors.New("Overlapping Stream Data")
	errEmptyStreamData                 = errors.New("Stream Data empty")
)

func newSegmentSorter() *segmentSorter {
	s := segmentSorter{
		gaps:         utils.NewByteIntervalList(),
		queuedFrames: make(map[uint64]*segment),
	}
	s.gaps.PushFront(utils.ByteInterval{Start: 0, End: protocol.MaxByteCount})
	return &s
}

func (s *segmentSorter) push(seg *segment) error {
	_, ok := s.queuedFrames[seg.offset]
	if ok {
		return errDuplicateStreamData
	}

	start := seg.offset
	end := seg.offset + seg.DataLen()

	if start == end {
		return errEmptyStreamData
	}

	var foundInGap bool

	for gap := s.gaps.Front(); gap != nil; gap = gap.Next() {
		// the complete segment lies before or after the gap
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

	s.queuedFrames[seg.offset] = seg
	return nil
}

func (s *segmentSorter) pop() *segment {
	seg := s.head()
	if seg != nil {
		s.readPosition += seg.DataLen()
		delete(s.queuedFrames, seg.offset)
	}
	return seg
}

func (s *segmentSorter) head() *segment {
	frame, ok := s.queuedFrames[s.readPosition]
	if ok {
		return frame
	}
	return nil
}

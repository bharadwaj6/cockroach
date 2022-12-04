// Copyright 2022 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package tsearch

import (
	"math"
	"sort"
	"strings"

	"github.com/cockroachdb/errors"
)

// EvalTSQuery runs the provided TSQuery against the provided TSVector,
// returning whether or not the query matches the vector.
func EvalTSQuery(q TSQuery, v TSVector) (bool, error) {
	evaluator := tsEvaluator{
		v: v,
		q: q,
	}
	return evaluator.eval()
}

type tsEvaluator struct {
	v TSVector
	q TSQuery
}

func (e *tsEvaluator) eval() (bool, error) {
	return e.evalNode(e.q.root)
}

// evalNode is used to evaluate a query node that's not nested within any
// followed by operators. it returns true if the match was successful.
func (e *tsEvaluator) evalNode(node *tsNode) (bool, error) {
	switch node.op {
	case invalid:
		// If there's no operator we're evaluating a leaf term.
		prefixMatch := false
		if len(node.term.positions) > 0 && node.term.positions[0].weight == weightStar {
			prefixMatch = true
		}

		// To evaluate a term, we search the vector for a match.
		target := node.term.lexeme
		i := sort.Search(len(e.v), func(i int) bool {
			return e.v[i].lexeme >= target
		})
		if i < len(e.v) {
			t := e.v[i]
			if prefixMatch {
				return strings.HasPrefix(t.lexeme, target), nil
			}
			return t.lexeme == target, nil
		}
		return false, nil
	case and:
		// Match if both operands are true.
		l, err := e.evalNode(node.l)
		if err != nil || !l {
			return false, err
		}
		return e.evalNode(node.r)
	case or:
		// Match if either operand is true.
		l, err := e.evalNode(node.l)
		if err != nil {
			return false, err
		}
		if l {
			return true, nil
		}
		return e.evalNode(node.r)
	case not:
		// Match if the operand is false.
		ret, err := e.evalNode(node.l)
		return !ret, err
	case followedby:
		// For followed-by queries, we recurse into the special followed-by handler.
		// Then, we return true if there is at least one position at which the
		// followed-by query matches.
		positions, err := e.evalWithinFollowedBy(node)
		return len(positions.positions) > 0, err
	}
	return false, errors.AssertionFailedf("invalid operator %d", node.op)
}

// tsPositionSet keeps track of metadata for a followed-by match. It's used to
// pass information about followed by queries during evaluation of them.
type tsPositionSet struct {
	// positions is the list of positions that the match is successful at (or,
	// if invert is true, unsuccessful at).
	positions []tsPosition
	// width is the width of the match. This is important to track to deal with
	// chained followed by queries with possibly different widths (<-> vs <2> etc).
	// A match of a single term within a followed by has width 0.
	width int
	// invert, if true, indicates that this match should be inverted. It's used
	// to handle followed by matches within not operators.
	invert bool
}

// emitMode is a bitfield that controls the output of followed by matches.
type emitMode int

const (
	// emitMatches causes evalFollowedBy to emit matches - positions at which
	// the left argument is found separated from the right argument by the right
	// width.
	emitMatches emitMode = 1 << iota
	// emitLeftUnmatched causes evalFollowedBy to emit places at which the left
	// arm doesn't match.
	emitLeftUnmatched
	// emitRightUnmatched causes evalFollowedBy to emit places at which the right
	// arm doesn't match.
	emitRightUnmatched
)

// evalFollowedBy handles evaluating a followed by operator. It needs
// information about the positions at which the left and right arms of the
// followed by operator matches, as well as the offsets for each of the arms:
// the number of lexemes apart each of the matches were.
// the emitMode controls the output - see the comments on each of the emitMode
// values for details.
// This function is a little bit confusing, because it's operating on two
// input position sets, and not directly on search terms. Its job is to do set
// operations on the input sets, depending on emitMode - an intersection or
// difference depending on the desired outcome by evalWithinFollowedBy.
// This code tries to follow the Postgres implementation in
// src/backend/utils/adt/tsvector_op.c.
func (e *tsEvaluator) evalFollowedBy(
	lPositions, rPositions tsPositionSet, lOffset, rOffset int, emitMode emitMode,
) (tsPositionSet, error) {
	// Followed by makes sure that two terms are separated by exactly n words.
	// First, find all slots that match for the left expression.

	// Find the offsetted intersection of 2 sorted integer lists, using the
	// followedN as the offset.
	var ret tsPositionSet
	var lIdx, rIdx int
	// Loop through the two sorted position lists, until the position on the
	// right is as least as large as the position on the left.
	for {
		lExhausted := lIdx >= len(lPositions.positions)
		rExhausted := rIdx >= len(rPositions.positions)
		if lExhausted && rExhausted {
			break
		}
		var lPos, rPos int
		if !lExhausted {
			lPos = int(lPositions.positions[lIdx].position) + lOffset
		} else {
			// Quit unless we're outputting all of the RHS, which we will if we have
			// a negative match on the LHS.
			if emitMode&emitRightUnmatched == 0 {
				break
			}
			lPos = math.MaxInt64
		}
		if !rExhausted {
			rPos = int(rPositions.positions[rIdx].position) + rOffset
		} else {
			// Quit unless we're outputting all of the LHS, which we will if we have
			// a negative match on the RHS.
			if emitMode&emitLeftUnmatched == 0 {
				break
			}
			rPos = math.MaxInt64
		}

		if lPos < rPos {
			if emitMode&emitLeftUnmatched > 0 {
				ret.positions = append(ret.positions, tsPosition{position: uint16(lPos)})
			}
			lIdx++
		} else if lPos == rPos {
			if emitMode&emitMatches > 0 {
				ret.positions = append(ret.positions, tsPosition{position: uint16(rPos)})
			}
			lIdx++
			rIdx++
		} else {
			if emitMode&emitRightUnmatched > 0 {
				ret.positions = append(ret.positions, tsPosition{position: uint16(rPos)})
			}
			rIdx++
		}
	}
	return ret, nil
}

// evalWithinFollowedBy is the evaluator for subexpressions of a followed by
// operator. Instead of just returning true or false, and possibly short
// circuiting on boolean ops, we need to return all of the tspositions at which
// each arm of the followed by expression matches.
func (e *tsEvaluator) evalWithinFollowedBy(node *tsNode) (tsPositionSet, error) {
	switch node.op {
	case invalid:
		// We're evaluating a leaf (a term).
		prefixMatch := false
		if len(node.term.positions) > 0 && node.term.positions[0].weight == weightStar {
			prefixMatch = true
		}

		// To evaluate a term, we search the vector for a match.
		target := node.term.lexeme
		i := sort.Search(len(e.v), func(i int) bool {
			return e.v[i].lexeme >= target
		})
		if i >= len(e.v) {
			// No match.
			return tsPositionSet{}, nil
		}
		var ret []tsPosition
		if prefixMatch {
			for j := i; j < len(e.v); j++ {
				t := e.v[j]
				if !strings.HasPrefix(t.lexeme, target) {
					break
				}
				ret = append(ret, t.positions...)
			}
			ret = sortAndUniqTSPositions(ret)
			return tsPositionSet{positions: ret}, nil
		} else if e.v[i].lexeme != target {
			// No match.
			return tsPositionSet{}, nil
		}
		// Return all of the positions at which the term is present.
		return tsPositionSet{positions: e.v[i].positions}, nil
	case or:
		var lOffset, rOffset, width int

		lPositions, err := e.evalWithinFollowedBy(node.l)
		if err != nil {
			return tsPositionSet{}, err
		}
		rPositions, err := e.evalWithinFollowedBy(node.r)
		if err != nil {
			return tsPositionSet{}, err
		}

		width = lPositions.width
		if rPositions.width > width {
			width = rPositions.width
		}
		lOffset = width - lPositions.width
		rOffset = width - rPositions.width

		mode := emitMatches | emitLeftUnmatched | emitRightUnmatched
		invertResults := false
		switch {
		case lPositions.invert && rPositions.invert:
			invertResults = true
			mode = emitMatches
		case lPositions.invert:
			invertResults = true
			mode = emitLeftUnmatched
		case rPositions.invert:
			invertResults = true
			mode = emitRightUnmatched
		}
		ret, err := e.evalFollowedBy(lPositions, rPositions, lOffset, rOffset, mode)
		if invertResults {
			ret.invert = true
		}
		ret.width = width
		return ret, err
	case not:
		ret, err := e.evalWithinFollowedBy(node.l)
		if err != nil {
			return tsPositionSet{}, err
		}
		ret.invert = !ret.invert
		return ret, nil
	case followedby:
		// Followed by and and have similar handling.
		fallthrough
	case and:
		var lOffset, rOffset, width int

		lPositions, err := e.evalWithinFollowedBy(node.l)
		if err != nil {
			return tsPositionSet{}, err
		}
		rPositions, err := e.evalWithinFollowedBy(node.r)
		if err != nil {
			return tsPositionSet{}, err
		}
		if node.op == followedby {
			lOffset = int(node.followedN) + rPositions.width
			width = lOffset + lPositions.width
		} else {
			width = lPositions.width
			if rPositions.width > width {
				width = rPositions.width
			}
			lOffset = width - lPositions.width
			rOffset = width - rPositions.width
		}

		mode := emitMatches
		invertResults := false
		switch {
		case lPositions.invert && rPositions.invert:
			invertResults = true
			mode |= emitLeftUnmatched | emitRightUnmatched
		case lPositions.invert:
			mode = emitRightUnmatched
		case rPositions.invert:
			mode = emitLeftUnmatched
		}
		ret, err := e.evalFollowedBy(lPositions, rPositions, lOffset, rOffset, mode)
		if invertResults {
			ret.invert = true
		}
		ret.width = width
		return ret, err
	}
	return tsPositionSet{}, errors.AssertionFailedf("invalid operator %d", node.op)
}

// sortAndUniqTSPositions sorts and uniquifies the input tsPosition list by
// their position attributes.
func sortAndUniqTSPositions(pos []tsPosition) []tsPosition {
	if len(pos) <= 1 {
		return pos
	}
	sort.Slice(pos, func(i, j int) bool {
		return pos[i].position < pos[j].position
	})
	// Then distinct: (wouldn't it be nice if Go had generics?)
	lastUniqueIdx := 0
	for j := 1; j < len(pos); j++ {
		if pos[j].position != pos[lastUniqueIdx].position {
			// We found a unique entry, at index i. The last unique entry in the array
			// was at lastUniqueIdx, so set the entry after that one to our new unique
			// entry, and bump lastUniqueIdx for the next loop iteration.
			lastUniqueIdx++
			pos[lastUniqueIdx] = pos[j]
		}
	}
	pos = pos[:lastUniqueIdx+1]
	if len(pos) > maxTSVectorPositions {
		// Postgres silently truncates position lists to length 256.
		pos = pos[:maxTSVectorPositions]
	}
	return pos
}

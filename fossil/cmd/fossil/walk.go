/*
 * Generic traversal routines.
 */

package main

/* tree walker, for gc and archiver */
type WalkPtr struct {
	data    []byte
	isEntry int
	n       int
	m       int
	e       Entry
	typ     uint8
	tag     uint32
}

func initWalk(w *WalkPtr, b *Block, size uint) {
	*w = WalkPtr{}
	switch b.l.typ {
	case BtData:
		return

	case BtDir:
		w.data = b.data
		w.m = int(size / VtEntrySize)
		w.isEntry = 1
		return

	default:
		w.data = b.data
		w.m = int(size / VtScoreSize)
		w.typ = b.l.typ
		w.tag = b.l.tag
		return
	}
}

func nextWalk(w *WalkPtr, score VtScore, typ *uint8, tag *uint32, e **Entry) bool {
	if w.n >= w.m {
		return false
	}

	if w.isEntry != 0 {
		*e = &w.e
		entryUnpack(&w.e, w.data, w.n)
		copy(score[:], w.e.score[:VtScoreSize])
		*typ = uint8(etype(&w.e))
		*tag = w.e.tag
	} else {
		*e = nil
		copy(score[:], w.data[w.n*VtScoreSize:][:VtScoreSize])
		*typ = w.typ - 1
		*tag = w.tag
	}

	w.n++
	return true
}

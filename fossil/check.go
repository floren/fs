package main

import (
	"fmt"
	"sort"

	"github.com/floren/fs/venti"
)

type Fsck struct {
	/* filled in by caller */
	printblocks   bool
	useventi      bool
	flags         int
	printdirs     bool
	printfiles    bool
	walksnapshots bool
	printf        func(string, ...interface{}) (int, error)
	clre          func(*Fsck, *Block, int) error
	clrp          func(*Fsck, *Block, int) error
	close         func(*Fsck, *Block, uint32) error
	clri          func(*Fsck, string, *MetaBlock, int, *Block) error

	/* used internally */
	fs        *Fs
	cache     *Cache
	amap      []uint8 /* all blocks seen so far */
	emap      []uint8 /* all blocks seen in this epoch */
	xmap      []uint8 /* all blocks in this epoch with parents in this epoch */
	errmap    []uint8 /* blocks with errors */
	smap      []uint8 /* walked sources */
	nblocks   int
	bsize     int
	walkdepth int
	hint      uint32 /* where the next root probably is */
	nseen     int
	quantum   int
	nclre     int
	nclrp     int
	nclose    int
	nclri     int
}

func (chk *Fsck) init(fs *Fs) {
	chk.fs = fs
	chk.cache = fs.cache
	chk.nblocks = int(chk.cache.localSize(PartData))
	chk.bsize = fs.blockSize
	chk.walkdepth = 0
	chk.hint = 0
	chk.quantum = chk.nblocks / 100
	if chk.quantum == 0 {
		chk.quantum = 1
	}
	if chk.printf == nil {
		chk.printf = printnop
	}
	if chk.clre == nil {
		chk.clre = clrenop
	}
	if chk.close == nil {
		chk.close = closenop
	}
	if chk.clri == nil {
		chk.clri = clrinop
	}
}

/*
 * BUG: Should merge checkEpochs and checkDirs so that
 * bad blocks are only reported once, and so that errors in checkEpochs
 * can have the affected file names attached, and so that the file system
 * is only read once.
 *
 * Also should summarize the errors instead of printing for every one
 * (e.g., XXX bad or unreachable blocks in /active/usr/rsc/foo).
 */
func (chk *Fsck) check(fs *Fs) {
	dprintf("check: starting\n")

	chk.init(fs)
	b, super, err := getSuper(chk.cache)
	if err != nil {
		chk.printf("could not load super block: %v", err)
		return
	}
	b.put()

	chk.hint = super.active
	checkEpochs(chk)

	chk.smap = make([]uint8, chk.nblocks/8+1)
	checkDirs(chk)

	dprintf("check: all done\n")
}

/*
 * Walk through all the blocks in the write buffer.
 * Then we can look for ones we missed -- those are leaks.
 */
func checkEpochs(chk *Fsck) {
	nb := uint(chk.nblocks)
	chk.amap = make([]uint8, nb/8+1)
	chk.emap = make([]uint8, nb/8+1)
	chk.xmap = make([]uint8, nb/8+1)
	chk.errmap = make([]uint8, nb/8+1)

	for e := chk.fs.ehi; e >= chk.fs.elo; e-- {
		for i := 0; i < chk.nblocks/8+1; i++ {
			chk.emap[i] = 0
		}
		for i := 0; i < chk.nblocks/8+1; i++ {
			chk.xmap[i] = 0
		}
		checkEpoch(chk, e)
	}

	checkLeak(chk)
}

func checkEpoch(chk *Fsck, epoch uint32) {
	var a uint32

	chk.printf("checking epoch %d...\n", epoch)

	for a = 0; a < uint32(chk.nblocks); a++ {
		l, err := chk.cache.readLabel((a + chk.hint) % uint32(chk.nblocks))
		if err != nil {
			chk.errorf("could not read label for addr %#0.8x", a)
			continue
		}

		if l.tag == RootTag && l.epoch == epoch {
			break
		}
	}

	if a == uint32(chk.nblocks) {
		chk.printf("could not find root block for epoch %d", epoch)
		return
	}

	a = (a + chk.hint) % uint32(chk.nblocks)
	b, err := chk.cache.localData(a, BtDir, RootTag, OReadOnly, 0)
	if err != nil {
		chk.errorf("could not read root block %#.8x: %v", a, err)
		return
	}
	defer b.put()

	/* no one should point at root blocks */
	setBit(chk.amap, a)
	setBit(chk.emap, a)
	setBit(chk.xmap, a)

	/*
	 * First entry is the rest of the file system.
	 * Second entry is link to previous epoch root,
	 * just a convenience to help the search.
	 */
	e, err := unpackEntry(b.data, 0)
	if err != nil {
		chk.errorf("could not unpack root block %#.8x: %v", a, err)
		return
	}

	walkEpoch(chk, b, &e.score, BtDir, e.tag, epoch)
	e, err = unpackEntry(b.data, 1)
	if err == nil {
		chk.hint = venti.GlobalToLocal(&e.score)
	}
}

/*
 * When b points at bb, need to check:
 *
 * (i) b.e in [bb.e, bb.eClose)
 * (ii) if b.e==bb.e,  then no other b' in e points at bb.
 * (iii) if !(b.state&Copied) and b.e==bb.e then no other b' points at bb.
 * (iv) if b is active then no other active b' points at bb.
 * (v) if b is a past life of b' then only one of b and b' is active
 *	(too hard to check)
 */
func walkEpoch(chk *Fsck, b *Block, score *venti.Score, typ BlockType, tag, epoch uint32) bool {
	if b != nil && chk.walkdepth == 0 && chk.printblocks {
		chk.printf("%v %d %#.8x %#.8x\n", &b.score, b.l.typ, b.l.tag, b.l.epoch)
	}

	if !chk.useventi && venti.GlobalToLocal(score) == NilBlock {
		return true
	}

	chk.walkdepth++

	bb, err := chk.cache.global(score, typ, tag, OReadOnly)
	if err != nil {
		chk.errorf("could not load block %v type=%d tag=%x: %v", score, typ, tag, err)
		chk.walkdepth--
		return false
	}

	if chk.printblocks {
		chk.printf("%*s%v %d %#.8x %#.8x\n", chk.walkdepth*2, "", score, typ, tag, bb.l.epoch)
	}

	ret := false
	addr := venti.GlobalToLocal(score)
	if addr == NilBlock {
		ret = true
		goto Exit
	}

	if b != nil {
		/* (i) */
		if b.l.epoch < bb.l.epoch || bb.l.epochClose <= b.l.epoch {
			chk.errorf("walk: block %#x [%d, %d) points at %#x [%d, %d)", b.addr, b.l.epoch, b.l.epochClose, bb.addr, bb.l.epoch, bb.l.epochClose)
			goto Exit
		}

		/* (ii) */
		if b.l.epoch == epoch && bb.l.epoch == epoch {
			if getBit(chk.emap, addr) != 0 {
				chk.errorf("walk: epoch join detected: addr %#x %v", bb.addr, &bb.l)
				goto Exit
			}

			setBit(chk.emap, addr)
		}

		/* (iii) */
		if b.l.state&BsCopied == 0 && b.l.epoch == bb.l.epoch {
			if getBit(chk.xmap, addr) != 0 {
				chk.errorf("walk: copy join detected; addr %#x %v", bb.addr, &bb.l)
				goto Exit
			}
			setBit(chk.xmap, addr)
		}
	}

	/* (iv) */
	if epoch == chk.fs.ehi {
		/*
		 * since epoch==fs->ehi is first, amap is same as
		 * ``have seen active''
		 */
		if getBit(chk.amap, addr) != 0 {
			chk.errorf("walk: active join detected: addr %#x %v", bb.addr, &bb.l)
			goto Exit
		}

		if bb.l.state&BsClosed != 0 {
			chk.errorf("walk: addr %#x: block is in active tree but is closed", addr)
		}
	} else if getBit(chk.amap, addr) == 0 {
		if bb.l.state&BsClosed == 0 {
			// chk.errorf("walk: addr %#x: block is not in active tree, not closed (%d)",
			// addr, bb->l.epochClose);
			if err := chk.close(chk, bb, epoch+1); err != nil {
				chk.errorf("%v", err)
			}
			chk.nclose++
		}
	}

	if getBit(chk.amap, addr) != 0 {
		ret = true
		goto Exit
	}

	setBit(chk.amap, addr)

	if chk.nseen%chk.quantum == 0 {
		chk.printf("check: visited %d/%d blocks (%.0f%%)\n",
			chk.nseen+1, chk.nblocks, float64(chk.nseen)*100/float64(chk.nblocks))
	}
	chk.nseen++

	b = nil /* make sure no more refs to parent */

	switch typ {
	/* pointer block */
	default:
		for i := int(0); i < chk.bsize/venti.ScoreSize; i++ {
			var score venti.Score
			copy(score[:], bb.data[i*venti.ScoreSize:])
			if !walkEpoch(chk, bb, &score, typ-1, tag, epoch) {
				setBit(chk.errmap, bb.addr)
				if err := chk.clrp(chk, bb, i); err != nil {
					chk.errorf("%v", err)
				}
				chk.nclrp++
			}
		}

	case BtData:
		break

	case BtDir:
		var ep uint32
		for i := int(0); i < chk.bsize/venti.EntrySize; i++ {
			e, err := unpackEntry(bb.data, i)
			if err != nil {
				//chk.errorf("walk: could not unpack entry: %ux[%d]: %v", addr, i, err);
				setBit(chk.errmap, bb.addr)

				if err := chk.clre(chk, bb, i); err != nil {
					chk.errorf("%v", err)
				}
				chk.nclre++
				continue
			}

			if e.flags&venti.EntryActive == 0 {
				continue
			}
			if false {
				dprintf("%x[%d] tag=%x snap=%d score=%v\n", addr, i, e.tag, e.snap, &e.score)
			}
			ep = epoch
			if e.snap != 0 {
				if e.snap >= epoch {
					// chk.errorf("bad snap in entry: %ux[%d] snap = %d: epoch = %d",
					//	addr, i, e.snap, epoch);
					setBit(chk.errmap, bb.addr)

					if err := chk.clre(chk, bb, i); err != nil {
						chk.errorf("%v", err)
					}
					chk.nclre++
					continue
				}

				continue
			}

			if e.flags&venti.EntryLocal != 0 {
				if e.tag < UserTag {
					if e.tag != RootTag || tag != RootTag || i != 1 {
						// chk.errorf("bad tag in entry: %ux[%d] tag = %ux",
						//	addr, i, e.tag);
						setBit(chk.errmap, bb.addr)

						if err := chk.clre(chk, bb, i); err != nil {
							chk.errorf("%v", err)
						}
						chk.nclre++
						continue
					}
				}
			} else if e.tag != 0 {
				// chk.errorf("bad tag in entry: %ux[%d] tag = %ux",
				//	addr, i, e.tag);
				setBit(chk.errmap, bb.addr)

				if err := chk.clre(chk, bb, i); err != nil {
					chk.errorf("%v", err)
				}
				chk.nclre++
				continue
			}

			if !walkEpoch(chk, bb, &e.score, EntryType(e), e.tag, ep) {
				setBit(chk.errmap, bb.addr)
				if err := chk.clre(chk, bb, i); err != nil {
					chk.errorf("%v", err)
				}
				chk.nclre++
			}
		}
	}

	ret = true

Exit:
	chk.walkdepth--
	bb.put()
	return ret
}

/*
 * We've just walked the whole write buffer.  Notice blocks that
 * aren't marked available but that we didn't visit.  They are lost.
 */
func checkLeak(chk *Fsck) {
	var nfree, nlost int64

	for a := uint32(0); a < uint32(chk.nblocks); a++ {
		l, err := chk.cache.readLabel(a)
		if err != nil {
			chk.errorf("could not read label: addr %#x %d %d: %v", a, l.typ, l.state, err)
			continue
		}

		if getBit(chk.amap, a) != 0 {
			continue
		}
		if l.state == BsFree || l.epochClose <= chk.fs.elo || l.epochClose == l.epoch {
			nfree++
			setBit(chk.amap, a)
			continue
		}

		if l.state&BsClosed != 0 {
			continue
		}
		nlost++

		//chk.warnf("unreachable block: addr %#x type %d tag %#x "
		//	"state %s epoch %d close %d", a, l.type, l.tag,
		//	l.state, l.epoch, l.epochClose);
		b, err := chk.cache.local(PartData, a, OReadOnly)
		if err != nil {
			chk.errorf("could not read block %#.8x", a)
			continue
		}

		if err := chk.close(chk, b, 0); err != nil {
			chk.errorf("%v", err)
		}
		chk.nclose++
		setBit(chk.amap, a)
		b.put()
	}

	chk.printf("fsys blocks: total=%d used=%d(%.1f%%) free=%d(%.1f%%) lost=%d(%.1f%%)\n",
		int64(chk.nblocks),
		int64(chk.nblocks)-nfree-nlost, 100*float64(int64(chk.nblocks)-nfree-nlost)/float64(chk.nblocks),
		nfree, 100*float64(nfree)/float64(chk.nblocks),
		nlost, 100*float64(nlost)/float64(chk.nblocks))
}

/*
 * Check that all sources in the tree are accessible.
 */
func openSource(chk *Fsck, s *Source, name string, bm []byte, offset, gen uint32, dir bool, mb *MetaBlock, i int, b *Block) (*Source, error) {
	var r *Source
	var err error

	if getBit(bm, offset) != 0 {
		chk.warnf("multiple references to source: %s -> %d", name, offset)
		err = fmt.Errorf("multiple references to source: %s -> %d", name, offset)
		goto Err
	}

	setBit(bm, offset)

	r, err = s.open(offset, OReadOnly, false)
	if err != nil {
		chk.warnf("could not open source: %s -> %d: %v", name, offset, err)
		goto Err
	}

	if r.gen != gen {
		chk.warnf("source has been removed: %s -> %d", name, offset)
		err = fmt.Errorf("source has been removed: %s -> %d", name, offset)
		goto Err
	}

	if r.dir != dir {
		chk.warnf("dir mismatch: %s -> %d", name, offset)
		err = fmt.Errorf("dir mismatch: %s -> %d", name, offset)
		goto Err
	}

	return r, nil

Err:
	if err := chk.clri(chk, name, mb, i, b); err != nil {
		chk.errorf("%v", err)
	}
	chk.nclri++
	if r != nil {
		r.close()
	}
	return nil, err
}

type MetaChunkSorter []MetaChunk

func (a MetaChunkSorter) Len() int           { return len(a) }
func (a MetaChunkSorter) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }
func (a MetaChunkSorter) Less(i, j int) bool { return a[i].offset < a[j].offset }

/*
 * Fsck that MetaBlock has reasonable header, sorted entries,
 */
func chkMetaBlock(mb *MetaBlock) bool {
	mc := make([]MetaChunk, mb.nindex)
	p := mb.buf[MetaHeaderSize:]
	for i := int(0); i < mb.nindex; i++ {
		mc[i].offset = uint16(p[0])<<8 | uint16(p[1])
		mc[i].size = uint16(p[2])<<8 | uint16(p[3])
		mc[i].index = uint16(i)
		p = p[MetaIndexSize:]
	}

	sort.Sort(MetaChunkSorter(mc))

	/* check block looks ok */
	oo := MetaHeaderSize + mb.maxindex*MetaIndexSize

	o := oo
	n := int(0)
	for i := int(0); i < mb.nindex; i++ {
		o = int(mc[i].offset)
		n = int(mc[i].size)
		if o < oo {
			goto Err
		}
		oo += n
	}

	if o+n > mb.size || mb.size-oo != mb.free {
		goto Err
	}

	return true

Err:
	if false {
		logf("metaChunks failed!\n")
		oo := MetaHeaderSize + mb.maxindex*MetaIndexSize
		for i := int(0); i < mb.nindex; i++ {
			logf("\t%d: %d %d\n", i, mc[i].offset, mc[i].offset+mc[i].size)
			oo += int(mc[i].size)
		}

		logf("\tused=%d size=%d free=%d free2=%d\n", oo, mb.size, mb.free, mb.size-oo)
	}

	return false
}

func scanSource(chk *Fsck, name string, r *Source) {
	if !chk.useventi && venti.GlobalToLocal(&r.score) == NilBlock {
		return
	}
	e, err := r.getEntry()
	if err != nil {
		chk.errorf("could not get entry for %s", name)
		return
	}

	a := venti.GlobalToLocal(&e.score)
	if !chk.useventi && a == NilBlock {
		return
	}
	if getBit(chk.smap, a) != 0 {
		return
	}
	setBit(chk.smap, a)

	nb := uint32((r.getSize() + uint64(r.dsize) - 1) / uint64(r.dsize))
	for o := uint32(0); o < nb; o++ {
		b, err := r.block(o, OReadOnly)
		if err != nil {
			chk.errorf("could not read block in data file %s", name)
			continue
		}

		if b.addr != NilBlock && getBit(chk.errmap, b.addr) != 0 {
			chk.warnf("previously reported error in block %x is in file %s", b.addr, name)
		}

		b.put()
	}
}

/*
 * Walk the source tree making sure that the BtData
 * sources containing directory entries are okay.
 */
func chkDir(chk *Fsck, name string, source, meta *Source) {
	if !chk.useventi &&
		venti.GlobalToLocal(&source.score) == NilBlock &&
		venti.GlobalToLocal(&meta.score) == NilBlock {
		return
	}

	if err := source.lock2(meta, OReadOnly); err != nil {
		chk.warnf("could not lock sources for %q: %v", name, err)
		return
	}
	e1, err := source.getEntry()
	if err != nil {
		chk.warnf("could not load source entry for %q: %v", name, err)
		return
	}
	e2, err := meta.getEntry()
	if err != nil {
		chk.warnf("could not load meta entry for %q: %v", name, err)
		return
	}

	a1 := venti.GlobalToLocal(&e1.score)
	a2 := venti.GlobalToLocal(&e2.score)
	if (!chk.useventi && a1 == NilBlock && a2 == NilBlock) ||
		(getBit(chk.smap, a1) != 0 && getBit(chk.smap, a2) != 0) {
		source.unlock()
		meta.unlock()
		return
	}
	setBit(chk.smap, a1)
	setBit(chk.smap, a2)

	var me MetaEntry
	bm := make([]uint8, int(source.getDirSize()/8+1))
	nb := uint32((meta.getSize() + uint64(meta.dsize) - 1) / uint64(meta.dsize))
	for o := uint32(0); o < nb; o++ {
		b, err := meta.block(o, OReadOnly)
		if err != nil {
			chk.errorf("could not read block in meta file: %s[%d]: %v", name, o, err)
			continue
		}
		if false {
			dprintf("source %v:%d block %d addr %d\n", &source.score, source.offset, o, b.addr)
		}
		if b.addr != NilBlock && getBit(chk.errmap, b.addr) != 0 {
			chk.warnf("previously reported error in block %x is in %s", b.addr, name)
		}
		mb, err := unpackMetaBlock(b.data, meta.dsize)
		if err != nil {
			chk.errorf("could not unpack meta block: %s[%d]: %v", name, o, err)
			b.put()
			continue
		}
		if !chkMetaBlock(mb) {
			chk.errorf("bad meta block: %s[%d]", name, o)
			b.put()
			continue
		}

		var s string
		for i := mb.nindex - 1; i >= 0; i-- {
			mb.unpackMetaEntry(&me, i)
			de, err := mb.unpackDirEntry(&me)
			if err != nil {
				chk.errorf("could not unpack dir entry: %s[%d][%d]: %v", name, o, i, err)
				continue
			}
			if s != "" && s <= de.elem {
				chk.errorf("dir entry out of order: %s[%d][%d] = %s last = %s", name, o, i, de.elem, s)
			}
			s = de.elem
			nn := fmt.Sprintf("%s/%s", name, de.elem)
			if chk.printdirs {
				if de.mode&ModeDir != 0 {
					chk.printf("%s/\n", nn)
				}
			}
			if chk.printfiles {
				if de.mode&ModeDir == 0 {
					chk.printf("%s\n", nn)
				}
			}
			if de.mode&ModeDir == 0 {
				if r, err := openSource(chk, source, nn, bm, de.entry, de.gen, false, mb, i, b); err == nil {
					if err = r.lock(OReadOnly); err == nil {
						scanSource(chk, nn, r)
						r.unlock()
					}
					r.close()
				}
				continue
			}

			r, err := openSource(chk, source, nn, bm, de.entry, de.gen, true, mb, i, b)
			if err != nil {
				continue
			}

			mr, err := openSource(chk, source, nn, bm, de.mentry, de.mgen, false, mb, i, b)
			if err != nil {
				r.close()
				continue
			}

			if de.mode&ModeSnapshot == 0 || chk.walksnapshots {
				chkDir(chk, nn, r, mr)
			}

			mr.close()
			r.close()
		}
		b.put()
	}

	nb = source.getDirSize()
	for o := uint32(0); o < nb; o++ {
		if getBit(bm, o) != 0 {
			continue
		}
		r, err := source.open(o, OReadOnly, false)
		if err != nil {
			continue
		}
		chk.warnf("non referenced entry in source %s[%d]", name, o)
		if b, err := source.block(o/(uint32(source.dsize)/venti.EntrySize), OReadOnly); err == nil {
			if b.addr != NilBlock {
				setBit(chk.errmap, b.addr)
				if err := chk.clre(chk, b, int(o%uint32(source.dsize/venti.EntrySize))); err != nil {
					chk.errorf("%v", err)
				}
				chk.nclre++
			}
			b.put()
		}
		r.close()
	}
	source.unlock()
	meta.unlock()
}

func checkDirs(chk *Fsck) {
	chk.fs.source.lock(OReadOnly)
	r, err := chk.fs.source.open(0, OReadOnly, false)
	if err != nil {
		panic(err) // should not occur
	}
	mr, err := chk.fs.source.open(1, OReadOnly, false)
	if err != nil {
		panic(err) // should not occur
	}
	chk.fs.source.unlock()
	chkDir(chk, "", r, mr)

	r.close()
	mr.close()
}

func setBit(bmap []byte, addr uint32) {
	if addr == NilBlock {
		return
	}

	bmap[addr>>3] |= 1 << (addr & 7)
}

func getBit(bmap []byte, addr uint32) int {
	if addr == NilBlock {
		return 0
	}
	return (int(bmap[addr>>3]) >> (addr & 7)) & 1
}

func (chk *Fsck) errorf(fmt_ string, args ...interface{}) {
	chk.printf("error: %s\n", fmt.Sprintf(fmt_, args...))
}

func (chk *Fsck) warnf(fmt_ string, args ...interface{}) {
	chk.printf("error: %s\n", fmt.Sprintf(fmt_, args...))
}

func clrenop(*Fsck, *Block, int) error { return nil }

func closenop(*Fsck, *Block, uint32) error { return nil }

func clrinop(*Fsck, string, *MetaBlock, int, *Block) error { return nil }

func printnop(_ string, args ...interface{}) (int, error) { return 0, nil }

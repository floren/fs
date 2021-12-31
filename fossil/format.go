package main

import (
	"bufio"
	"flag"
	"fmt"
	"log"
	"os"
	"syscall"
	"time"

	"github.com/floren/fs/venti"
)

const badSize = ^uint64(0)

var qid uint64 = 1

func format(argv []string) {
	flags := flag.NewFlagSet("format", flag.ExitOnError)
	flags.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s [-b blocksize] [-h host] [-l label] [-v score] [-y] file\n", argv0)
		flags.PrintDefaults()
		os.Exit(1)
	}
	var (
		bflag = flags.String("b", "8K", "Set the file system `blocksize`.")
		hflag = flags.String("h", "", "Use `host` as the Venti server.")
		lflag = flags.String("l", "", "Set the textual label on the file system to `label`.")
		vflag = flags.String("v", "", "Initialize the file system using the vac file system at `score`.")

		// This is -y instead of -f because flchk has a
		// (frequently used) -f option.  I type flfmt instead
		// of flchk all the time, and want to make it hard
		// to reformat my file system accidentally.
		yflag = flags.Bool("y", false, "Yes mode. If set, format will not prompt for confirmation.")
	)
	flags.Parse(argv)

	bsize := unittoull(*bflag)
	if bsize == badSize {
		flags.Usage()
	}
	buf := make([]byte, bsize)

	host := *hflag
	label := "vfs"
	if *lflag != "" {
		label = *lflag
	}
	score := *vflag
	force := *yflag

	if flags.NArg() != 1 {
		flags.Usage()
	}
	argv = flags.Args()

	fd, err := syscall.Open(argv[0], syscall.O_RDWR, 0)
	if err != nil {
		log.Fatal(err)
	}

	if _, err = syscall.Pread(fd, buf, HeaderOffset); err != nil {
		fatalf("could not read fs header block: %v", err)
	}

	dprintf("format: unpacking header\n")
	_, err = unpackHeader(buf)
	if err == nil && !force && !confirm("fs header block already exists; are you sure?") {
		return
	}

	// TODO(jnj)
	//d, err := dirfstat(f)
	//if err != nil {
	//	fatalf("dirfstat: %v", err)
	//}
	//if d.Type == 'M' && !force && !confirm("fs file is mounted via devmnt (is not a kernel device); are you sure?") {
	//	return
	//}

	dprintf("format: partitioning\n")
	h := partition(fd, int(bsize))
	h.pack(buf)
	if _, err := syscall.Pwrite(fd, buf, HeaderOffset); err != nil {
		fatalf("could not write fs header: %v", err)
	}

	dprintf("format: allocating disk structure\n")
	disk, err := allocDisk(fd)
	if err != nil {
		fatalf("could not open disk: %v", err)
	}

	dprintf("format: writing labels\n")
	// zero labels
	memset(buf, 0)
	for bn := uint32(0); bn < disk.size(PartLabel); bn++ {
		disk.blockWrite(PartLabel, bn, buf)
	}

	var z *venti.Session
	var root uint32
	if score != "" {
		dprintf("format: ventiRoot\n")
		z, root = disk.ventiRoot(host, score, buf)
	} else {
		dprintf("format: rootMetaInit\n")
		e := disk.rootMetaInit(buf)
		root = disk.rootInit(e, buf)
	}

	dprintf("format: initializing superblock\n")
	zscore := venti.ZeroScore()
	disk.superInit(label, root, &zscore, buf)

	dprintf("format: freeing disk structure\n")
	disk.free()

	if score == "" {
		dprintf("format: populating top-level fs entries\n")
		topLevel(argv[0], z)
	}
}

func confirm(msg string) bool {
	fmt.Fprintf(os.Stderr, "%s [y/n]: ", msg)
	line, _, err := bufio.NewReader(os.Stdin).ReadLine()
	if err != nil {
		return false
	}
	if line[0] == 'y' {
		return true
	}
	return false
}

func partition(fd, bsize int) *Header {
	if bsize%512 != 0 {
		fatalf("block size must be a multiple of 512 bytes")
	}
	if bsize > venti.MaxBlockSize {
		fatalf("block size must be less than %d", venti.MaxBlockSize)
	}

	h := Header{
		blockSize: uint16(bsize),
	}

	lpb := uint32(bsize) / LabelSize

	size, err := devsize(fd)
	if err != nil {
		fatalf("error getting file size: %v", err)
	}

	nblock := uint32(size / int64(bsize))

	/* sanity check */
	if nblock < uint32((HeaderOffset*10)/bsize) {
		fatalf("file too small: nblock=%d", nblock)
	}

	h.super = (HeaderOffset + 2*uint32(bsize)) / uint32(bsize)
	h.label = h.super + 1
	ndata := uint32((uint64(lpb)) * uint64(nblock-h.label) / uint64(lpb+1))
	nlabel := (ndata + lpb - 1) / lpb
	h.data = h.label + nlabel
	h.end = h.data + ndata

	return &h
}

func formatTagGen() uint32 {
	var tag uint32
	for {
		tag = uint32(lrand())
		if tag > RootTag {
			break
		}
	}
	return tag
}

func (d *Disk) entryInit() *Entry {
	bsize := d.blockSize()
	e := &Entry{
		dsize: uint16(bsize),
		psize: uint16(bsize / venti.EntrySize * venti.EntrySize),
		flags: venti.EntryActive,
		score: venti.ZeroScore(),
		tag:   formatTagGen(),
	}
	return e
}

func (d *Disk) rootMetaInit(buf []byte) *Entry {
	bsize := d.blockSize()
	de := DirEntry{
		elem:   "root",
		mentry: 1,
		qid:    qid,
		uid:    "adm",
		gid:    "adm",
		mid:    "adm",
		mtime:  uint32(time.Now().Unix()),
		ctime:  uint32(time.Now().Unix()),
		atime:  uint32(time.Now().Unix()),
		mode:   ModeDir | 0555,
	}
	qid++

	tag := formatTagGen()
	addr := d.blockAlloc(BtData, tag, buf)

	/* build up meta block */
	memset(buf, 0)
	mb := initMetaBlock(buf, bsize, bsize/100)
	var me MetaEntry
	me.size = uint16(de.getSize())
	o, err := mb.alloc(int(me.size))
	assert(err == nil)
	me.offset = o
	mb.packDirEntry(&de, &me)
	mb.insert(0, &me)
	mb.pack()
	d.blockWrite(PartData, addr, buf)

	/* build up entry for meta block */
	e := d.entryInit()
	e.flags |= venti.EntryLocal
	e.size = uint64(d.blockSize())
	e.tag = tag
	e.score = venti.LocalToGlobal(addr)

	return e
}

func (d *Disk) rootInit(e *Entry, buf []byte) uint32 {
	tag := formatTagGen()

	addr := d.blockAlloc(BtDir, tag, buf)
	memset(buf, 0)

	/* root meta data is in the third entry */
	e.pack(buf, 2)

	e = d.entryInit()
	e.flags |= venti.EntryDir
	e.pack(buf, 0)

	e = d.entryInit()
	e.pack(buf, 1)

	d.blockWrite(PartData, addr, buf)

	e = d.entryInit()
	e.flags |= venti.EntryLocal | venti.EntryDir
	e.size = venti.EntrySize * 3
	e.tag = tag
	e.score = venti.LocalToGlobal(addr)

	addr = d.blockAlloc(BtDir, RootTag, buf)
	memset(buf, 0)
	e.pack(buf, 0)

	d.blockWrite(PartData, addr, buf)

	return addr
}

// static
var blockAlloc_addr uint32

func (d *Disk) blockAlloc(typ BlockType, tag uint32, buf []byte) uint32 {
	bsize := d.blockSize()
	lpb := bsize / LabelSize
	d.blockRead(PartLabel, blockAlloc_addr/uint32(lpb), buf)

	l, err := unpackLabel(buf, int(blockAlloc_addr%uint32(lpb)))
	if err != nil {
		fatalf("bad label: %v", err)
	}
	if l.state != BsFree {
		fatalf("want to allocate block already in use")
	}
	l.epoch = 1
	l.epochClose = ^uint32(0)
	l.typ = typ
	l.state = BsAlloc
	l.tag = tag
	l.pack(buf, int(blockAlloc_addr%uint32(lpb)))
	d.blockWrite(PartLabel, blockAlloc_addr/uint32(lpb), buf)
	tmp1 := blockAlloc_addr
	blockAlloc_addr++
	return tmp1
}

func (d *Disk) superInit(label string, root uint32, score *venti.Score, buf []byte) {
	s := Super{
		version:   SuperVersion,
		epochLow:  1,
		epochHigh: 1,
		qid:       qid,
		active:    root,
		next:      NilBlock,
		current:   NilBlock,
		last:      *score,
	}
	copy(s.name[:], []byte(label))

	memset(buf, 0)
	s.pack(buf)
	d.blockWrite(PartSuper, 0, buf)
}

func (d *Disk) blockRead(part int, addr uint32, buf []byte) {
	if err := d.readRaw(part, addr, buf); err != nil {
		fatalf("read failed: %v", err)
	}
}

func (d *Disk) blockWrite(part int, addr uint32, buf []byte) {
	if err := d.writeRaw(part, addr, buf); err != nil {
		fatalf("write failed: %v", err)
	}
}

func addFile(root *File, name string, mode uint) {
	f, err := root.create(name, uint32(mode)|ModeDir, "adm")
	if err != nil {
		fatalf("format: create %q: %v", name, err)
	}
	f.decRef()
}

func topLevel(name string, z *venti.Session) {
	/* ok, now we can open as a fs */
	fs, err := openFs(name, "", z, false, 100, OReadWrite)
	if err != nil {
		fatalf("format: open fs: %v", err)
	}
	fs.elk.RLock()
	root := fs.getRoot()
	addFile(root, "active", 0555)
	addFile(root, "archive", 0555)
	addFile(root, "snapshot", 0555)
	root.decRef()
	fs.elk.RUnlock()
	fs.close()
}

func (d *Disk) ventiRead(z *venti.Session, score *venti.Score, typ venti.BlockType, buf []byte) int {
	n, err := z.Read(score, typ, buf)
	if err != nil {
		fatalf("ventiRead %v (%d) failed: %v", score, typ, err)
	}
	dprintf("retrieved %v block from venti; zero-extending from %d to %d bytes", typ, n, d.blockSize())
	venti.ZeroExtend(typ, buf, n, d.blockSize())
	return n
}

func (d *Disk) ventiRoot(host string, s string, buf []byte) (*venti.Session, uint32) {
	score, err := venti.ParseScore(s)
	if err != nil {
		fatalf("bad score %q: %v", s, err)
	}

	z, err := venti.Dial(host)
	if err != nil {
		fatalf("connect to venti: %v", err)
	}

	tag := formatTagGen()
	addr := d.blockAlloc(BtDir, tag, buf)

	d.ventiRead(z, score, venti.RootType, buf)
	root, err := venti.UnpackRoot(buf)
	if err != nil {
		fatalf("corrupted root: %v", err)
	}
	n := d.ventiRead(z, &root.Score, venti.DirType, buf)

	/*
	 * Fossil's vac archives start with an extra layer of source,
	 * but vac's don't.
	 */
	var e *Entry
	if n <= 2*venti.EntrySize {
		e, err := unpackEntry(buf, 0)
		if err != nil {
			fatalf("bad root: top entry: %v", err)
		}
		n = d.ventiRead(z, &e.score, venti.DirType, buf)
	} else {
		e = new(Entry)
	}

	/*
	 * There should be three root sources (and nothing else) here.
	 */
	for i := 0; i < 3; i++ {
		e, err = unpackEntry(buf, i)
		if err != nil || e.flags&venti.EntryActive == 0 || e.psize < 256 || e.dsize < 256 {
			fatalf("bad root: entry %d", i)
		}
		fmt.Fprintf(os.Stderr, "%v\n", &e.score)
	}

	if n > 3*venti.EntrySize {
		fatalf("bad root: entry count")
	}

	d.blockWrite(PartData, addr, buf)

	/*
	 * Maximum qid is recorded in root's msource, entry #2 (conveniently in e).
	 */
	d.ventiRead(z, &e.score, venti.DataType, buf)

	mb, err := unpackMetaBlock(buf, d.blockSize())
	if err != nil {
		fatalf("bad root: unpackMetaBlock: %v", err)
	}
	var me MetaEntry
	mb.unpackMetaEntry(&me, 0)
	de, err := mb.unpackDirEntry(&me)
	if err != nil {
		fatalf("bad root: dirUnpack: %v", err)
	}
	if de.qidSpace == 0 {
		fatalf("bad root: no qidSpace")
	}
	qid = de.qidMax

	/*
	 * Recreate the top layer of source.
	 */
	e = d.entryInit()

	e.flags |= venti.EntryLocal | venti.EntryDir
	e.size = venti.EntrySize * 3
	e.tag = tag
	e.score = venti.LocalToGlobal(addr)

	addr = d.blockAlloc(BtDir, RootTag, buf)
	memset(buf, 0)
	e.pack(buf, 0)
	d.blockWrite(PartData, addr, buf)

	return z, addr
}

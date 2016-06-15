package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"sigint.ca/fs/venti"
)

type Fsys struct {
	lock *sync.Mutex

	name  string // copy here & Fs to ease error reporting
	dev   string
	venti string

	fs      *Fs
	session *venti.Session
	ref     int

	noauth     bool
	noperm     bool
	wstatallow bool

	next *Fsys
}

var sbox struct {
	lock *sync.RWMutex
	head *Fsys
	tail *Fsys

	curfsys string
}

const FsysAll = "all"

var (
	EFsysBusy      = "fsys: %q busy"
	EFsysExists    = "fsys: %q already exists"
	EFsysNoCurrent = "fsys: no current fsys"
	EFsysNotFound  = "fsys: %q not found"
	EFsysNotOpen   = "fsys: %q not open"
)

var fsyscmd = []struct {
	cmd string
	f   func(*Fsys, []string) error
	f1  func(string, []string) error
}{
	{"close", fsysClose, nil},
	{"config", nil, fsysConfig},
	{"open", nil, fsysOpen},
	{"unconfig", nil, fsysUnconfig},
	{"venti", nil, fsysVenti},
	{"bfree", fsysBfree, nil},
	{"block", fsysBlock, nil},
	{"check", fsysCheck, nil},
	{"clre", fsysClre, nil},
	{"clri", fsysClri, nil},
	{"clrp", fsysClrp, nil},
	{"create", fsysCreate, nil},
	{"df", fsysDf, nil},
	{"epoch", fsysEpoch, nil},
	{"halt", fsysHalt, nil},
	{"label", fsysLabel, nil},
	{"remove", fsysRemove, nil},
	{"snap", fsysSnap, nil},
	{"snaptime", fsysSnapTime, nil},
	{"snapclean", fsysSnapClean, nil},
	{"stat", fsysStat, nil},
	{"sync", fsysSync, nil},
	{"unhalt", fsysUnhalt, nil},
	{"wstat", fsysWstat, nil},
	{"vac", fsysVac, nil},
	{"", nil, nil},
}

func ventihost(host string) string {
	if host != "" {
		return host
	}
	host = os.Getenv("venti")
	if host == "" {
		host = "$venti"
	}
	return host
}

func prventihost(host string) string {
	host = ventihost(host)
	fmt.Fprintf(os.Stderr, "%s: dialing venti at %v\n", argv0, host)
	return host
}

func vtDial(host string, canfail bool) (*venti.Session, error) {
	host = prventihost(host)
	return venti.Dial(host, canfail)
}

func cmdPrintConfig(argv []string) error {
	var usage string = "Usage: printconfig"

	flags := flag.NewFlagSet("printconfig", flag.ContinueOnError)
	flags.Usage = func() { fmt.Fprintln(os.Stderr, usage); flags.PrintDefaults() }
	if err := flags.Parse(argv[1:]); err != nil {
		return EUsage
	}
	if flags.NArg() != 0 {
		flags.Usage()
		return EUsage
	}

	sbox.lock.RLock()
	for fsys := sbox.head; fsys != nil; fsys = fsys.next {
		printf("\tfsys %s config %s\n", fsys.name, fsys.dev)
		if fsys.venti != "" {
			printf("\tfsys %s venti %q\n", fsys.name, fsys.venti)
		}
	}

	sbox.lock.RUnlock()
	return nil
}

func fsysGet(name string) (*Fsys, error) {
	fsys, err := _fsysGet(name)
	if err != nil {
		return nil, err
	}

	fsys.lock.Lock()
	if fsys.fs == nil {
		fsys.lock.Unlock()
		fsysPut(fsys)
		return nil, fmt.Errorf(EFsysNotOpen, fsys.name)
	}

	fsys.lock.Unlock()

	return fsys, nil
}

func _fsysGet(name string) (*Fsys, error) {
	if name == "" {
		name = "main"
	}

	sbox.lock.RLock()
	var fsys *Fsys
	for fsys = sbox.head; fsys != nil; fsys = fsys.next {
		if name == fsys.name {
			fsys.lock.Lock()
			fsys.ref++
			fsys.lock.Unlock()
			break
		}
	}
	sbox.lock.RUnlock()

	if fsys == nil {
		return nil, fmt.Errorf(EFsysNotFound, name)
	}
	return fsys, nil
}

func fsysGetName(fsys *Fsys) string {
	return fsys.name
}

func fsysIncRef(fsys *Fsys) *Fsys {
	sbox.lock.Lock()
	fsys.ref++
	sbox.lock.Unlock()

	return fsys
}

func fsysPut(fsys *Fsys) {
	sbox.lock.Lock()
	assert(fsys.ref > 0)
	fsys.ref--
	sbox.lock.Unlock()
}

func fsysGetFs(fsys *Fsys) *Fs {
	assert(fsys != nil && fsys.fs != nil)
	return fsys.fs
}

func fsysFsRlock(fsys *Fsys) {
	fsys.fs.elk.RLock()
}

func fsysFsRUnlock(fsys *Fsys) {
	fsys.fs.elk.RUnlock()
}

func fsysNoAuthCheck(fsys *Fsys) bool {
	return fsys.noauth
}

func fsysNoPermCheck(fsys *Fsys) bool {
	return fsys.noperm
}

func fsysWstatAllow(fsys *Fsys) bool {
	return fsys.wstatallow
}

var modechars string = "YUGalLdHSATs"

var modebits = []uint32{
	ModeSticky,
	ModeSetUid,
	ModeSetGid,
	ModeAppend,
	ModeExclusive,
	ModeLink,
	ModeDir,
	ModeHidden,
	ModeSystem,
	ModeArchive,
	ModeTemporary,
	ModeSnapshot,
}

// TODO(jnj): test
func fsysModeString(mode uint32) string {
	var buf []byte
	for i := range modebits {
		if mode&modebits[i] != 0 {
			buf = append(buf, modechars[i])
		}
	}
	return string(buf) + fmt.Sprintf("%o", mode&0777)
}

// TODO(jnj): test
func fsysParseMode(s string) (uint32, bool) {
	// get mode chars
	var x uint32
	for {
		if s == "" {
			return 0, false
		}
		if s[0] >= '0' && s[0] <= '9' {
			break
		}
		i := strings.IndexByte(modechars, s[0])
		if i < 0 {
			return 0, false
		}
		x |= modebits[i]
		s = s[1:]
	}

	// get mode bits
	y, err := strconv.ParseUint(s, 8, 32)
	if err != nil || y > 0777 {
		return 0, false
	}
	return x | uint32(y), true
}

func fsysGetRoot(fsys *Fsys, name string) *File {
	assert(fsys != nil && fsys.fs != nil)

	root := fsys.fs.getRoot()
	if name == "" {
		return root
	}

	var sub *File
	sub, _ = root.walk(name)
	root.decRef()

	return sub
}

func fsysAlloc(name string, dev string) (*Fsys, error) {
	sbox.lock.Lock()
	defer sbox.lock.Unlock()

	for fsys := sbox.head; fsys != nil; fsys = fsys.next {
		if fsys.name != name {
			continue
		}
		return nil, fmt.Errorf(EFsysExists, name)
	}

	fsys := &Fsys{
		lock: new(sync.Mutex),
		name: name,
		dev:  dev,
		ref:  1,
	}

	if sbox.tail != nil {
		sbox.tail.next = fsys
	} else {
		sbox.head = fsys
	}
	sbox.tail = fsys

	return fsys, nil
}

func fsysClose(fsys *Fsys, argv []string) error {
	var usage string = "Usage: [fsys name] close"

	flags := flag.NewFlagSet("close", flag.ContinueOnError)
	flags.Usage = func() { fmt.Fprintln(os.Stderr, usage); flags.PrintDefaults() }
	if err := flags.Parse(argv[1:]); err != nil {
		return EUsage
	}
	if flags.NArg() != 0 {
		flags.Usage()
		return EUsage
	}

	return fmt.Errorf("close isn't working yet; halt %s and then kill fossil", fsys.name)

	/*
		 * Oooh. This could be hard. What if fsys->ref != 1?
		 * Also, fsClose() either does the job or panics, can we
		 * gracefully detect it's still busy?
		 *
		 * More thought and care needed here.
		fsClose(fsys->fs);
		fsys->fs = nil;
		vtClose(fsys->session);
		fsys->session = nil;

		if(sbox.curfsys != nil && strcmp(fsys->name, sbox.curfsys) == 0){
			sbox.curfsys = nil;
			consPrompt(nil);
		}

		return nil;
	*/
}

func fsysVac(fsys *Fsys, argv []string) error {
	var usage string = "Usage: [fsys name] vac path"

	flags := flag.NewFlagSet("vac", flag.ContinueOnError)
	flags.Usage = func() { fmt.Fprintln(os.Stderr, usage); flags.PrintDefaults() }
	if err := flags.Parse(argv[1:]); err != nil {
		return EUsage
	}
	argv = flags.Args()
	argc := flags.NArg()
	if argc != 1 {
		flags.Usage()
		return EUsage
	}

	var score venti.Score
	if err := fsys.fs.vac(argv[0], &score); err != nil {
		return err
	}

	printf("vac:%v\n", &score)
	return nil
}

func fsysSnap(fsys *Fsys, argv []string) error {
	var usage string = "Usage: [fsys name] snap [-a] [-s /active] [-d /archive/yyyy/mmmm]"

	flags := flag.NewFlagSet("snap", flag.ContinueOnError)
	flags.Usage = func() { fmt.Fprintln(os.Stderr, usage); flags.PrintDefaults() }
	aflag := flags.Bool("a", false, "Take an archival snapshot.")
	sflag := flags.String("s", "", "Set the source path of the snapshot to `path`.")
	dflag := flags.String("d", "", "Set the destination path of the snapshot to `path`.")
	if err := flags.Parse(argv[1:]); err != nil {
		return EUsage
	}
	if flags.NArg() != 0 {
		flags.Usage()
		return EUsage
	}

	return fsys.fs.snapshot(*sflag, *dflag, *aflag)
}

func fsysSnapClean(fsys *Fsys, argv []string) error {
	var usage string = "Usage: [fsys name] snapclean [maxminutes]"

	flags := flag.NewFlagSet("snapclean", flag.ContinueOnError)
	flags.Usage = func() { fmt.Fprintln(os.Stderr, usage); flags.PrintDefaults() }
	if err := flags.Parse(argv[1:]); err != nil {
		return EUsage
	}
	if flags.NArg() > 1 {
		flags.Usage()
		return EUsage
	}

	var life time.Duration
	if flags.NArg() == 1 {
		min, err := strconv.ParseUint(flags.Arg(0), 0, 0)
		if err != nil {
			flags.Usage()
			return EUsage
		}
		life = time.Duration(min) * time.Minute
	} else {
		_, _, life = snapGetTimes(fsys.fs.snap)
	}

	fsys.fs.snapshotCleanup(life)
	return nil
}

func fsysSnapTime(fsys *Fsys, argv []string) error {
	var usage string = "Usage: [fsys name] snaptime [-a hhmm] [-s snapfreq] [-t snaplife]"

	flags := flag.NewFlagSet("snaptime", flag.ContinueOnError)
	flags.Usage = func() { fmt.Fprintln(os.Stderr, usage); flags.PrintDefaults() }
	var (
		// unfortunately, these cannot be flags.Duration, because we need to accept "none"
		aflag = flags.String("a", "", "Set the daily archival snapshot time to `hhmm`.")
		sflag = flags.String("s", "", "Set the ephemeral snapshot interval to `snapfreq` minutes.")
		tflag = flags.String("t", "", "Set the snapshot timeout to `snaplife` minutes.")
	)
	if err := flags.Parse(argv[1:]); err != nil {
		return EUsage
	}
	if flags.NArg() > 0 {
		flags.Usage()
		return EUsage
	}

	arch, snap, life := snapGetTimes(fsys.fs.snap)
	var err error

	// only consider flags that were explicitly set
	flags.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "a":
			arch = -1
			if *aflag != "none" {
				t, err1 := time.Parse("1504", *aflag)
				if err1 != nil {
					err = EUsage
					return
				}
				arch = t.Sub(t.Truncate(24 * time.Hour))
			}
		case "s":
			snap = -1
			if *sflag != "none" {
				d, err1 := strconv.ParseUint(*sflag, 10, 0)
				if err1 != nil {
					err = EUsage
					return
				}
				snap = time.Duration(d) * time.Minute
			}
		case "t":
			life = -1
			if *tflag != "none" {
				d, err1 := strconv.ParseUint(*tflag, 10, 0)
				if err1 != nil {
					err = EUsage
					return
				}
				life = time.Duration(d) * time.Minute
			}
		}
	})

	if err != nil {
		flags.Usage()
		return err
	}

	if flags.NFlag() > 0 {
		snapSetTimes(fsys.fs.snap, arch, snap, life)
		return nil
	}

	arch, snap, life = snapGetTimes(fsys.fs.snap)
	var buf string
	if arch >= 0 {
		buf = fmt.Sprintf("-a %s", time.Time{}.Add(arch).Format("1504"))
	} else {
		buf = fmt.Sprintf("-a none")
	}
	if snap >= 0 {
		buf += fmt.Sprintf(" -s %d", int(snap.Minutes()))
	} else {
		buf += fmt.Sprintf(" -s none")
	}
	if life >= 0 {
		buf += fmt.Sprintf(" -t %d", int(life.Minutes()))
	} else {
		buf += fmt.Sprintf(" -t none")
	}
	printf("\tsnaptime %s\n", buf)
	return nil
}

func fsysSync(fsys *Fsys, argv []string) error {
	var usage string = "Usage: [fsys name] sync"

	flags := flag.NewFlagSet("sync", flag.ContinueOnError)
	flags.Usage = func() { fmt.Fprintln(os.Stderr, usage); flags.PrintDefaults() }
	if err := flags.Parse(argv[1:]); err != nil {
		return EUsage
	}
	if flags.NArg() != 0 {
		flags.Usage()
		return EUsage
	}

	n := fsys.fs.cache.dirty()
	fsys.fs.sync()
	printf("\t%s sync: wrote %d blocks\n", fsys.name, n)
	return nil
}

func fsysHalt(fsys *Fsys, argv []string) error {
	var usage string = "Usage: [fsys name] halt"

	flags := flag.NewFlagSet("halt", flag.ContinueOnError)
	flags.Usage = func() { fmt.Fprintln(os.Stderr, usage); flags.PrintDefaults() }
	if err := flags.Parse(argv[1:]); err != nil {
		return EUsage
	}
	if flags.NArg() != 0 {
		flags.Usage()
		return EUsage
	}

	fsys.fs.halt()
	return nil
}

func fsysUnhalt(fsys *Fsys, argv []string) error {
	var usage string = "Usage: [fsys name] unhalt"

	flags := flag.NewFlagSet("unhalt", flag.ContinueOnError)
	flags.Usage = func() { fmt.Fprintln(os.Stderr, usage); flags.PrintDefaults() }
	if err := flags.Parse(argv[1:]); err != nil {
		return EUsage
	}
	if flags.NArg() != 0 {
		flags.Usage()
		return EUsage
	}

	if !fsys.fs.halted {
		return fmt.Errorf("file system %s not halted", fsys.name)
	}

	fsys.fs.unhalt()
	return nil
}

func fsysRemove(fsys *Fsys, argv []string) error {
	var usage string = "Usage: [fsys name] remove path ..."

	flags := flag.NewFlagSet("remove", flag.ContinueOnError)
	flags.Usage = func() { fmt.Fprintln(os.Stderr, usage); flags.PrintDefaults() }
	if err := flags.Parse(argv[1:]); err != nil {
		return EUsage
	}
	argv = flags.Args()
	argc := flags.NArg()
	if argc == 0 {
		flags.Usage()
		return EUsage
	}

	fsys.fs.elk.RLock()
	for argc > 0 {
		file, err := openFile(fsys.fs, argv[0])
		if err != nil {
			printf("%s: %v\n", argv[0], err)
		} else {
			if err := file.remove(uidadm); err != nil {
				printf("%s: %v\n", argv[0], err)
			}
			file.decRef()
		}
		argc--
		argv = argv[1:]
	}

	fsys.fs.elk.RUnlock()

	return nil
}

func fsysClri(fsys *Fsys, argv []string) error {
	var usage string = "Usage: [fsys name] clri path ..."

	flags := flag.NewFlagSet("clri", flag.ContinueOnError)
	flags.Usage = func() { fmt.Fprintln(os.Stderr, usage); flags.PrintDefaults() }
	if err := flags.Parse(argv[1:]); err != nil {
		return EUsage
	}
	argv = flags.Args()
	argc := flags.NArg()
	if argc == 0 {
		flags.Usage()
		return EUsage
	}

	fsys.fs.elk.RLock()
	for argc > 0 {
		if err := fileClriPath(fsys.fs, argv[0], uidadm); err != nil {
			printf("clri %s: %v\n", argv[0], err)
		}
		argc--
		argv = argv[1:]
	}

	fsys.fs.elk.RUnlock()

	return nil
}

/*
 * Inspect and edit the labels for blocks on disk.
 */
func fsysLabel(fsys *Fsys, argv []string) error {
	var usage string = "Usage: [fsys name] label addr [type state epoch epochClose tag]"

	flags := flag.NewFlagSet("label", flag.ContinueOnError)
	flags.Usage = func() { fmt.Fprintln(os.Stderr, usage); flags.PrintDefaults() }
	if err := flags.Parse(argv[1:]); err != nil {
		return EUsage
	}
	argv = flags.Args()
	argc := flags.NArg()
	if argc != 1 && argc != 6 {
		flags.Usage()
		return EUsage
	}

	fsys.fs.elk.RLock()
	defer fsys.fs.elk.RUnlock()

	fs := fsys.fs
	addr := strtoul(argv[0], 0)
	b, err := fs.cache.local(PartData, addr, OReadOnly)
	if err != nil {
		return err
	}
	defer b.put()

	l := b.l
	showOld := ""
	if argc == 6 {
		showOld = "old: "
	}
	printf("%slabel %x %d %d %d %d %x\n", showOld, addr, l.typ, l.state, l.epoch, l.epochClose, l.tag)

	if argc == 6 {
		if argv[1] != "-" {
			l.typ = uint8(atoi(argv[1]))
		}
		if argv[2] != "-" {
			l.state = uint8(atoi(argv[2]))
		}
		if argv[3] != "-" {
			l.epoch = strtoul(argv[3], 0)
		}
		if argv[4] != "-" {
			l.epochClose = strtoul(argv[4], 0)
		}
		if argv[5] != "-" {
			l.tag = strtoul(argv[5], 0)
		}

		printf("new: label %x %d %d %d %d %x\n", addr, l.typ, l.state, l.epoch, l.epochClose, l.tag)
		bb, err := b._setLabel(&l)
		if err != nil {
			return err
		}
		n := 0
		for {
			if bb.write(Waitlock) {
				for bb.iostate != BioClean {
					assert(bb.iostate == BioWriting)
					bb.ioready.Wait()
				}
				break
			}
			// TODO(jnj): better error
			printf("blockWrite failed\n")
			n++
			if n >= 6 {
				printf("giving up\n")
				break
			}
			time.Sleep(5 * time.Second)
		}
		bb.put()
	}

	return nil
}

/*
 * Inspect and edit the blocks on disk.
 */
func fsysBlock(fsys *Fsys, argv []string) error {
	var usage string = "Usage: [fsys name] block addr offset [count [data]]"

	flags := flag.NewFlagSet("block", flag.ContinueOnError)
	flags.Usage = func() { fmt.Fprintln(os.Stderr, usage); flags.PrintDefaults() }
	if err := flags.Parse(argv[1:]); err != nil {
		return EUsage
	}
	argv = flags.Args()
	argc := flags.NArg()
	if argc < 2 || argc > 4 {
		flags.Usage()
		return EUsage
	}

	fs := fsys.fs
	addr := strtoul(argv[0], 0)
	offset := int(strtoul(argv[1], 0))
	if offset < 0 || offset >= fs.blockSize {
		return errors.New("bad offset")
	}

	var count int
	if argc > 2 {
		count = int(strtoul(argv[2], 0))
	} else {
		count = 1e8
	}
	if offset+count > fs.blockSize {
		count = fs.blockSize - count
	}

	fs.elk.RLock()
	defer fs.elk.RUnlock()

	mode := OReadOnly
	if argc == 4 {
		mode = OReadWrite
	}
	b, err := fs.cache.local(PartData, addr, mode)
	if err != nil {
		return fmt.Errorf("(*Cache).local %x: %v", addr, err)
	}
	defer b.put()

	prefix := ""
	if argc == 4 {
		prefix = "old: "
	}
	printf("\t%sblock %x %d %d %.*X\n", prefix, addr, offset, count, count, b.data[offset:])

	if argc == 4 {
		s := argv[3]
		if len(s) != 2*count {
			return errors.New("bad data count")
		}

		buf := make([]byte, count)
		var c int
		for i := 0; i < count*2; i++ {
			if s[i] >= '0' && s[i] <= '9' {
				c = int(s[i]) - '0'
			} else if s[i] >= 'a' && s[i] <= 'f' {
				c = int(s[i]) - 'a' + 10
			} else if s[i] >= 'A' && s[i] <= 'F' {
				c = int(s[i]) - 'A' + 10
			} else {
				return errors.New("bad hex")
			}
			if i&1 == 0 {
				c <<= 4
			}
			buf[i>>1] |= byte(c)
		}

		copy(b.data[offset:], buf)
		printf("\tnew: block %x %d %d %.*X\n", addr, offset, count, count, b.data[offset:])
		b.dirty()
	}

	return nil
}

/*
 * Free a disk block.
 */
func fsysBfree(fsys *Fsys, argv []string) error {
	var usage string = "Usage: [fsys name] bfree addr ..."

	flags := flag.NewFlagSet("bfree", flag.ContinueOnError)
	flags.Usage = func() { fmt.Fprintln(os.Stderr, usage); flags.PrintDefaults() }
	if err := flags.Parse(argv[1:]); err != nil {
		return EUsage
	}
	argv = flags.Args()
	argc := flags.NArg()
	if argc == 0 {
		flags.Usage()
		return EUsage
	}

	fs := fsys.fs
	fs.elk.RLock()
	var l Label
	for argc > 0 {
		addr, err := strconv.ParseUint(argv[0], 0, 32)
		if err != nil {
			fs.elk.RUnlock()
			return fmt.Errorf("bad address: %v\n", err)
		}
		b, err := fs.cache.local(PartData, uint32(addr), OReadOnly)
		if err != nil {
			printf("loading %x: %v\n", addr, err)
			continue
		}
		l = b.l
		if l.state == BsFree {
			printf("%x is already free\n", addr)
		} else {
			printf("label %x %d %d %d %d %x\n", addr, l.typ, l.state, l.epoch, l.epochClose, l.tag)
			l.state = BsFree
			l.typ = BtMax
			l.tag = 0
			l.epoch = 0
			l.epochClose = 0
			if err := b.setLabel(&l, false); err != nil {
				printf("freeing %x: %v\n", addr, err)
			}
		}
		b.put()
		argc--
		argv = argv[1:]
	}

	fs.elk.RUnlock()

	return nil
}

func fsysDf(fsys *Fsys, argv []string) error {
	var usage string = "Usage: [fsys name] df"
	var used, tot, bsize uint32

	flags := flag.NewFlagSet("df", flag.ContinueOnError)
	flags.Usage = func() { fmt.Fprintln(os.Stderr, usage); flags.PrintDefaults() }
	if err := flags.Parse(argv[1:]); err != nil {
		return EUsage
	}
	if flags.NArg() != 0 {
		flags.Usage()
		return EUsage
	}

	fs := fsys.fs
	fs.elk.RLock()
	elo := fs.elo
	fs.elk.RUnlock()

	fs.cache.countUsed(elo, &used, &tot, &bsize)
	printf("\t%s: %s used + %s free = %s (%.1f%% used)\n",
		fsys.name,
		fmtComma(int64(used)*int64(bsize)),
		fmtComma(int64(tot-used)*int64(bsize)),
		fmtComma(int64(tot)*int64(bsize)),
		float64(used)*100/float64(tot))
	return nil
}

func fmtComma(n int64) string {
	in := strconv.FormatInt(n, 10)
	out := make([]byte, len(in)+(len(in)-2+int(in[0]/'0'))/3)
	if in[0] == '-' {
		in, out[0] = in[1:], '-'
	}

	for i, j, k := len(in)-1, len(out)-1, 0; ; i, j = i-1, j-1 {
		out[j] = in[i]
		if i == 0 {
			return string(out)
		}
		if k++; k == 3 {
			j, k = j-1, 0
			out[j] = ','
		}
	}
}

/*
 * Zero an entry or a pointer.
 */
func fsysClrep(fsys *Fsys, argv []string, ch rune) error {
	var usage = fmt.Sprintf("Usage: [fsys name] clr%c addr offset ...", ch)

	flags := flag.NewFlagSet("clrep", flag.ContinueOnError)
	flags.Usage = func() { fmt.Fprintln(os.Stderr, usage); flags.PrintDefaults() }
	if err := flags.Parse(argv[1:]); err != nil {
		return EUsage
	}
	argv = flags.Args()
	argc := flags.NArg()
	if argc < 2 {
		flags.Usage()
		return EUsage
	}

	fs := fsys.fs
	fsys.fs.elk.RLock()
	defer fsys.fs.elk.RUnlock()

	addr := strtoul(argv[0], 0)
	mode := OReadOnly
	if argc == 4 {
		mode = OReadWrite
	}
	b, err := fs.cache.local(PartData, addr, mode)
	if err != nil {
		return fmt.Errorf("(*Cache).local %x: %v", addr, err)
	}

	sz := venti.ScoreSize
	var zero [venti.EntrySize]uint8
	switch ch {
	default:
		return fmt.Errorf("clrep")
	case 'e':
		if b.l.typ != BtDir {
			return fmt.Errorf("wrong block type")
		}
		var e Entry
		entryPack(&e, zero[:], 0)
	case 'p':
		if b.l.typ == BtDir || b.l.typ == BtData {
			return fmt.Errorf("wrong block type")
		}
		copy(zero[:], venti.ZeroScore[:venti.ScoreSize])
	}
	max := fs.blockSize / sz
	for i := 1; i < argc; i++ {
		offset := atoi(argv[i])
		if offset >= max {
			printf("\toffset %d too large (>= %d)\n", i, max)
			continue
		}
		printf("\tblock %x %d %d %.*X\n", addr, offset*sz, sz, sz, b.data[offset*sz:])
		copy(b.data[offset*sz:], zero[:sz])
	}

	b.dirty()
	b.put()

	return nil
}

func fsysClre(fsys *Fsys, argv []string) error {
	return fsysClrep(fsys, argv, 'e')
}

func fsysClrp(fsys *Fsys, argv []string) error {
	return fsysClrep(fsys, argv, 'p')
}

// TODO(jnj): errors?
func fsysEsearch1(f *File, s string, elo uint32) int {
	dee, err := openDee(f)
	if err != nil {
		return 0
	}

	n := 0
	var de DirEntry
	var e, ee Entry
	for {
		r, err := dee.read(&de)
		if r < 0 {
			printf("\tdeeRead %s/%s: %v\n", s, de.elem, err)
			break
		}
		if r == 0 {
			break
		}
		if de.mode&ModeSnapshot != 0 {
			ff, err := f.walk(de.elem)
			if err != nil {
				printf("\tcannot walk %s/%s: %v\n", s, de.elem, err)
			} else {
				if err := ff.getSources(&e, &ee); err != nil {
					printf("\tcannot get sources for %s/%s: %v\n", s, de.elem, err)
				} else if e.snap != 0 && e.snap < elo {
					printf("\t%d\tclri %s/%s\n", e.snap, s, de.elem)
					n++
				}

				ff.decRef()
			}
		} else if de.mode&ModeDir != 0 {
			ff, err := f.walk(de.elem)
			if err != nil {
				printf("\tcannot walk %s/%s: %v\n", s, de.elem, err)
			} else {
				t := fmt.Sprintf("%s/%s", s, de.elem)
				n += fsysEsearch1(ff, t, elo)
				ff.decRef()
			}
		}

		deCleanup(&de)
		if r < 0 {
			break
		}
	}

	dee.close()

	return n
}

// TODO(jnj): errors?
func fsysEsearch(fs *Fs, path string, elo uint32) int {
	var f *File

	f, err := openFile(fs, path)
	if err != nil {
		return 0
	}
	defer f.decRef()
	var de DirEntry
	if err := f.getDir(&de); err != nil {
		printf("\tfileGetDir %s failed: %v\n", path, err)
		return 0
	}

	if de.mode&ModeDir == 0 {
		deCleanup(&de)
		return 0
	}

	deCleanup(&de)
	return fsysEsearch1(f, path, elo)
}

func fsysEpoch(fsys *Fsys, argv []string) error {
	var low, old uint32
	var usage string = "Usage: [fsys name] epoch [[-ry] low]"

	force := int(0)
	remove := int(0)
	flags := flag.NewFlagSet("epoch", flag.ContinueOnError)
	flags.Usage = func() { fmt.Fprintln(os.Stderr, usage); flags.PrintDefaults() }
	if err := flags.Parse(argv[1:]); err != nil {
		return EUsage
	}
	argv = flags.Args()
	argc := flags.NArg()
	if argc > 1 {
		flags.Usage()
		return EUsage
	}
	if argc > 0 {
		low = strtoul(argv[0], 0)
	} else {
		low = ^uint32(0)
	}

	if low == 0 {
		return fmt.Errorf("low epoch cannot be zero")
	}

	fs := fsys.fs

	fs.elk.RLock()
	printf("\tlow %d hi %d\n", fs.elo, fs.ehi)
	if low == ^uint32(0) {
		fs.elk.RUnlock()
		return nil
	}

	n := fsysEsearch(fsys.fs, "/archive", low)
	n += fsysEsearch(fsys.fs, "/snapshot", low)
	suff := ""
	if n > 1 {
		suff = "s"
	}
	printf("\t%d snapshot%s found with epoch < %d\n", n, suff, low)
	fs.elk.RUnlock()

	/*
	 * There's a small race here -- a new snapshot with epoch < low might
	 * get introduced now that we unlocked fs->elk.  Low has to
	 * be <= fs->ehi.  Of course, in order for this to happen low has
	 * to be equal to the current fs->ehi _and_ a snapshot has to
	 * run right now.  This is a small enough window that I don't care.
	 */
	if n != 0 && force == 0 {
		printf("\tnot setting low epoch\n")
		return nil
	}

	old = fs.elo
	if err := fs.epochLow(low); err != nil {
		printf("\tfsEpochLow: %v\n", err)
	} else {
		showForce := ""
		if force != 0 {
			showForce = " -y"
		}
		printf("\told: epoch%s %d\n", showForce, old)
		printf("\tnew: epoch%s %d\n", showForce, fs.elo)
		if fs.elo < low {
			printf("\twarning: new low epoch < old low epoch\n")
		}
		if force != 0 && remove != 0 {
			fs.snapshotRemove()
		}
	}

	return nil
}

func fsysCreate(fsys *Fsys, argv []string) error {
	var usage string = "Usage: [fsys name] create path uid gid perm"

	flags := flag.NewFlagSet("create", flag.ContinueOnError)
	flags.Usage = func() { fmt.Fprintln(os.Stderr, usage); flags.PrintDefaults() }
	if err := flags.Parse(argv[1:]); err != nil {
		return EUsage
	}
	argv = flags.Args()
	argc := flags.NArg()
	if argc != 4 {
		flags.Usage()
		return EUsage
	}

	mode, ok := fsysParseMode(argv[3])
	if !ok {
		return EUsage
	}
	if mode&ModeSnapshot != 0 {
		return fmt.Errorf("create - cannot create with snapshot bit set")
	}

	if argv[1] == uidnoworld {
		return fmt.Errorf("permission denied")
	}

	fsys.fs.elk.RLock()
	defer fsys.fs.elk.RUnlock()

	path := argv[0]
	var elem, parentPath string
	i := strings.LastIndexByte(path, '/')
	if i >= 0 {
		elem = path[i+1:]
		parentPath = path[:i]
		if len(parentPath) == 0 {
			parentPath = "/"
		}
	} else {
		parentPath = "/"
		elem = path
	}

	parent, err := openFile(fsys.fs, parentPath)
	if err != nil {
		return err
	}

	file, err := parent.create(elem, mode, argv[1])
	parent.decRef()
	if err != nil {
		return fmt.Errorf("create %s/%s: %v", parentPath, elem, err)
	}
	defer file.decRef()

	var de DirEntry
	if err := file.getDir(&de); err != nil {
		return fmt.Errorf("stat failed after create: %v", err)
	}

	defer deCleanup(&de)
	if de.gid != argv[2] {
		de.gid = argv[2]
		if err := file.setDir(&de, argv[1]); err != nil {
			return fmt.Errorf("wstat failed after create: %v", err)
		}
	}

	return nil
}

func fsysPrintStat(prefix string, file string, de *DirEntry) {
	printf("%sstat %q %q %q %q %s %d\n",
		prefix, file, de.elem, de.uid, de.gid, fsysModeString(de.mode), de.size)
}

func fsysStat(fsys *Fsys, argv []string) error {
	var usage string = "Usage: [fsys name] stat files..."

	flags := flag.NewFlagSet("stat", flag.ContinueOnError)
	flags.Usage = func() { fmt.Fprintln(os.Stderr, usage); flags.PrintDefaults() }
	if err := flags.Parse(argv[1:]); err != nil {
		return EUsage
	}
	argv = flags.Args()
	argc := flags.NArg()
	if argc == 0 {
		flags.Usage()
		return EUsage
	}

	fsys.fs.elk.RLock()
	for i := 0; i < argc; i++ {
		f, err := openFile(fsys.fs, argv[i])
		if err != nil {
			printf("%s: %v\n", argv[i], err)
			continue
		}

		var de DirEntry
		if err := f.getDir(&de); err != nil {
			printf("%s: %v\n", argv[i], err)
			f.decRef()
			continue
		}
		fsysPrintStat("\t", argv[i], &de)
		deCleanup(&de)
		f.decRef()
	}
	fsys.fs.elk.RUnlock()
	return nil
}

func fsysWstat(fsys *Fsys, argv []string) error {
	var usage string = `Usage: [fsys name] wstat file elem uid gid mode length
  -	Replace any field with - to mean "don't change".`

	flags := flag.NewFlagSet("wstat", flag.ContinueOnError)
	flags.Usage = func() { fmt.Fprintln(os.Stderr, usage); flags.PrintDefaults() }
	if err := flags.Parse(argv[1:]); err != nil {
		return EUsage
	}
	argv = flags.Args()
	argc := flags.NArg()
	if argc != 6 {
		flags.Usage()
		return EUsage
	}

	fsys.fs.elk.RLock()
	var err error
	var f *File
	f, err = openFile(fsys.fs, argv[0])
	if err != nil {
		fsys.fs.elk.RUnlock()
		return fmt.Errorf("console wstat - walk - %v", err)
	}

	var de DirEntry
	if err := f.getDir(&de); err != nil {
		f.decRef()
		fsys.fs.elk.RUnlock()
		return fmt.Errorf("console wstat - stat - %v", err)
	}

	fsysPrintStat("\told: w", argv[0], &de)

	if argv[1] != "-" {
		if err = checkValidFileName(argv[1]); err != nil {
			err = fmt.Errorf("console wstat - bad elem - %v", err)
			goto Err
		}

		de.elem = argv[1]
	}

	if argv[2] != "-" {
		if !validUserName(argv[2]) {
			err = fmt.Errorf("console wstat - bad uid - %v", err)
			goto Err
		}

		de.uid = argv[2]
	}

	if argv[3] != "-" {
		if !validUserName(argv[3]) {
			err = errors.New("console wstat - bad gid")
			goto Err
		}

		de.gid = argv[3]
	}

	if argv[4] != "-" {
		var ok bool
		if de.mode, ok = fsysParseMode(argv[4]); !ok {
			err = errors.New("console wstat - bad mode")
			goto Err
		}
	}

	if argv[5] != "-" {
		de.size, err = strconv.ParseUint(argv[5], 0, 64)
		if len(argv[5]) == 0 || err != nil || int64(de.size) < 0 {
			err = errors.New("console wstat - bad length")
			goto Err
		}
	}

	if err := f.setDir(&de, uidadm); err != nil {
		err = fmt.Errorf("console wstat - %v", err)
		goto Err
	}

	deCleanup(&de)

	if err := f.getDir(&de); err != nil {
		err = fmt.Errorf("console wstat - stat2 - %v", err)
		goto Err
	}

	fsysPrintStat("\tnew: w", argv[0], &de)
	deCleanup(&de)
	f.decRef()
	fsys.fs.elk.RUnlock()

	return nil

Err:
	deCleanup(&de) /* okay to do this twice */
	f.decRef()
	fsys.fs.elk.RUnlock()

	assert(err != nil)
	return err
}

const (
	doClose = 1 << iota
	doClre
	doClri
	doClrp
)

func fsckClri(fsck *Fsck, name string, mb *MetaBlock, i int, b *Block) {
	if fsck.flags&doClri == 0 {
		return
	}

	mb.delete(i)
	mb.pack()
	b.dirty()
}

func fsckClose(fsck *Fsck, b *Block, epoch uint32) {
	if fsck.flags&doClose == 0 {
		return
	}
	l := b.l
	if l.state == BsFree || (l.state&BsClosed != 0) {
		printf("%x is already closed\n", b.addr)
		return
	}

	if epoch != 0 {
		l.state |= BsClosed
		l.epochClose = epoch
	} else {
		l.state = BsFree
	}

	if err := b.setLabel(&l, false); err != nil {
		printf("%x setlabel: %v\n", b.addr, err)
	}
}

func fsckClre(fsck *Fsck, b *Block, offset int) {
	if fsck.flags&doClre == 0 {
		return
	}
	if offset < 0 || offset*venti.EntrySize >= fsck.bsize {
		printf("bad clre\n")
		return
	}

	e := Entry{score: new(venti.Score)}
	entryPack(&e, b.data, offset)
	b.dirty()
}

func fsckClrp(fsck *Fsck, b *Block, offset int) {
	if fsck.flags&doClrp == 0 {
		return
	}
	if offset < 0 || offset*venti.ScoreSize >= fsck.bsize {
		printf("bad clre\n")
		return
	}

	copy(b.data[offset*venti.ScoreSize:], venti.ZeroScore[:venti.ScoreSize])
	b.dirty()
}

func fsysCheck(fsys *Fsys, argv []string) error {
	var usage string = "Usage: [fsys name] check [options]"

	fsck := &Fsck{
		clri:   fsckClri,
		clre:   fsckClre,
		clrp:   fsckClrp,
		close:  fsckClose,
		printf: printf,
	}

	flags := flag.NewFlagSet("check", flag.ContinueOnError)
	flags.Usage = func() { fmt.Fprintln(os.Stderr, usage); flags.PrintDefaults() }
	if err := flags.Parse(argv[1:]); err != nil {
		return EUsage
	}
	for _, arg := range flags.Args() {
		switch arg {
		case "pblock":
			fsck.printblocks = true
		case "pdir":
			fsck.printdirs = true
		case "pfile":
			fsck.printfiles = true
		case "bclose":
			fsck.flags |= doClose
		case "clri":
			fsck.flags |= doClri
		case "clre":
			fsck.flags |= doClre
		case "clrp":
			fsck.flags |= doClrp
		case "fix":
			fsck.flags |= doClose | doClri | doClre | doClrp
		case "venti":
			fsck.useventi = true
		case "snapshot":
			fsck.walksnapshots = true
		default:
			printf("unknown option %q\n", arg)
			flags.Usage()
			return EUsage
		}
	}

	halting := fsys.fs.halted
	if halting {
		fsys.fs.halt()
	}
	if fsys.fs.arch != nil {
		var super Super
		b, err := superGet(fsys.fs.cache, &super)
		if err != nil {
			printf("could not load super block: %v\n", err)
			goto Out
		}

		b.put()
		if super.current != NilBlock {
			printf("cannot check fs while archiver is running; wait for it to finish\n")
			goto Out
		}
	}

	fsck.check(fsys.fs)
	printf("fsck: %d clri, %d clre, %d clrp, %d bclose\n", fsck.nclri, fsck.nclre, fsck.nclrp, fsck.nclose)

Out:
	if halting {
		fsys.fs.unhalt()
	}
	return nil
}

func fsysVenti(name string, argv []string) error {
	var usage string = "Usage: [fsys name] venti [address]"

	flags := flag.NewFlagSet("venti", flag.ContinueOnError)
	flags.Usage = func() { fmt.Fprintln(os.Stderr, usage); flags.PrintDefaults() }
	if err := flags.Parse(argv[1:]); err != nil {
		return EUsage
	}
	argv = flags.Args()
	argc := flags.NArg()

	var host string
	if argc == 0 {
		host = ""
	} else if argc == 1 {
		host = argv[0]
	} else {
		return EUsage
	}

	fsys, err := _fsysGet(name)
	if err != nil {
		return err
	}
	defer fsysPut(fsys)

	fsys.lock.Lock()
	defer fsys.lock.Unlock()

	if host == "" {
		host = fsys.venti
	} else {
		if host[0] != 0 {
			fsys.venti = host
		} else {
			host = ""
			fsys.venti = ""
		}
	}

	/* already open; redial */
	if fsys.fs != nil {
		if fsys.session == nil {
			return errors.New("file system was opened with -V")
		}
		fsys.session.Close()
		fsys.session, err = vtDial(host, false)
		if err != nil {
			return err
		}
	}

	/* not yet open: try to dial */
	if fsys.session != nil {
		fsys.session.Close()
	}
	fsys.session, err = vtDial(host, false)
	if err != nil {
		return err
	}

	return nil
}

func freemem() uint32 {
	var pgsize int = 0
	var userpgs uint64 = 0
	var userused uint64 = 0

	size := uint64(64 * 1024 * 1024)
	f, err := os.Open("#c/swap")
	if err == nil {
		bp := bufio.NewReader(f)
		for {
			ln, err := bp.ReadString('\n')
			if err != nil {
				break
			}
			ln = ln[:len(ln)-1]

			fields := strings.Fields(ln)
			if len(fields) != 2 {
				continue
			}
			if fields[1] == "pagesize" {
				pgsize = atoi(fields[0])
			} else if fields[1] == "user" {
				i := strings.IndexByte(fields[0], '/')
				if i < 0 {
					continue
				}
				userpgs = uint64(atoll(fields[0][i+1:]))
				userused = uint64(atoll(fields[0]))
			}
		}
		f.Close()

		if pgsize > 0 && userpgs > 0 {
			size = (userpgs - userused) * uint64(pgsize)
		}
	}

	/* cap it to keep the size within 32 bits */
	if size >= 3840*1024*1024 {
		size = 3840 * 1024 * 1024
	}
	return uint32(size)
}

func fsysOpen(name string, argv []string) error {
	argv = fixFlags(argv)

	var usage string = "Usage: fsys main open [-APVWr] [-c ncache]"

	flags := flag.NewFlagSet("open", flag.ContinueOnError)
	flags.Usage = func() { fmt.Fprintln(os.Stderr, usage); flags.PrintDefaults() }
	var (
		Aflag = flags.Bool("A", false, "run with no authentication")
		Pflag = flags.Bool("P", false, "run with no permission checking")
		Vflag = flags.Bool("V", false, "do not attempt to connect to a Venti server")
		Wflag = flags.Bool("W", false, "allow wstat to make arbitrary changes to the user and group fields")
		aflag = flags.Bool("a", false, "do not update file access times; primarily to avoid wear on flash memories")
		rflag = flags.Bool("r", false, "open the file system read-only")
		cflag = flags.Uint("c", 1000, "allocate an in-memory cache of `ncache` blocks")
	)
	if err := flags.Parse(argv[1:]); err != nil {
		return EUsage
	}

	noventi := *Vflag
	ncache := int(*cflag)
	mode := OReadWrite
	if *rflag {
		mode = OReadOnly
	}

	if flags.NArg() != 0 {
		return EUsage
	}

	fsys, err := _fsysGet(name)
	if err != nil {
		return err
	}

	/* automatic memory sizing? */
	if mempcnt > 0 {
		/* TODO: 8K is a hack; use the actual block size */
		ncache = int(((int64(freemem()) * int64(mempcnt)) / 100) / int64(8*1024))

		if ncache < 100 {
			ncache = 100
		}
	}

	fsys.lock.Lock()
	if fsys.fs != nil {
		fsys.lock.Unlock()
		fsysPut(fsys)
		return fmt.Errorf(EFsysBusy, fsys.name)
	}

	if noventi {
		if fsys.session != nil {
			fsys.session.Close()
			fsys.session = nil
		}
	} else if fsys.session == nil {
		var host string
		if fsys.venti != "" && fsys.venti[0] != 0 {
			host = fsys.venti
		} else {
			host = ""
		}
		fsys.session, err = vtDial(host, true)
		if err != nil && !noventi {
			fmt.Fprintf(os.Stderr, "warning: connecting to venti: %v\n", err)
		}
	}

	fsys.fs, err = openFs(fsys.dev, fsys.session, ncache, mode)
	if err != nil {
		fsys.lock.Unlock()
		fsysPut(fsys)
		return fmt.Errorf("fsOpen: %v", err)
	}

	fsys.fs.name = fsys.name /* for better error messages */
	fsys.noauth = *Aflag
	fsys.noperm = *Pflag
	fsys.wstatallow = *Wflag
	fsys.fs.noatimeupd = *aflag
	fsys.lock.Unlock()
	fsysPut(fsys)

	if name == "main" {
		usersFileRead("")
	}

	return nil
}

func fsysUnconfig(name string, argv []string) error {
	var usage string = "Usage: fsys name unconfig"

	flags := flag.NewFlagSet("unconfig", flag.ContinueOnError)
	flags.Usage = func() { fmt.Fprintln(os.Stderr, usage); flags.PrintDefaults() }
	if err := flags.Parse(argv[1:]); err != nil {
		return EUsage
	}
	if flags.NArg() != 0 {
		flags.Usage()
		return EUsage
	}

	sbox.lock.Lock()
	fp := &sbox.head
	var fsys *Fsys
	for fsys = *fp; fsys != nil; fsys = fsys.next {
		if fsys.name == name {
			break
		}
		fp = &fsys.next
	}

	if fsys == nil {
		sbox.lock.Unlock()
		return fmt.Errorf(EFsysNotFound, name)
	}

	if fsys.ref != 0 || fsys.fs != nil {
		sbox.lock.Unlock()
		return fmt.Errorf(EFsysBusy, fsys.name)
	}

	*fp = fsys.next
	sbox.lock.Unlock()

	if fsys.session != nil {
		fsys.session.Close()
	}

	return nil
}

func fsysConfig(name string, argv []string) error {
	var usage string = "Usage: fsys name config [dev]"

	flags := flag.NewFlagSet("config", flag.ContinueOnError)
	flags.Usage = func() { fmt.Fprintln(os.Stderr, usage); flags.PrintDefaults() }
	if err := flags.Parse(argv[1:]); err != nil {
		return EUsage
	}
	argv = flags.Args()
	argc := flags.NArg()
	if argc > 1 {
		flags.Usage()
		return EUsage
	}

	var part string
	if argc == 0 {
		part = foptname
	} else {
		part = argv[0]
	}

	fsys, err := _fsysGet(part)
	if err == nil {
		fsys.lock.Lock()
		if fsys.fs != nil {
			fsys.lock.Unlock()
			fsysPut(fsys)
			return fmt.Errorf(EFsysBusy, fsys.name)
		}
		fsys.dev = part
		fsys.lock.Unlock()
	} else {
		fsys, err = fsysAlloc(name, part)
		if err != nil {
			return err
		}
	}

	fsysPut(fsys)
	return nil
}

func fsysXXX1(fsys *Fsys, i int, argv []string) error {
	fsys.lock.Lock()
	defer fsys.lock.Unlock()

	if fsys.fs == nil {
		return fmt.Errorf(EFsysNotOpen, fsys.name)
	}

	if fsys.fs.halted && fsyscmd[i].cmd != "unhalt" && fsyscmd[i].cmd != "check" {
		return fmt.Errorf("file system %s is halted", fsys.name)
	}

	return fsyscmd[i].f(fsys, argv)
}

func fsysXXX(name string, argv []string) error {
	var i int
	for i = 0; fsyscmd[i].cmd != ""; i++ {
		if fsyscmd[i].cmd == argv[0] {
			break
		}
	}

	if fsyscmd[i].cmd == "" {
		return fmt.Errorf("unknown command - %q", argv[0])
	}

	/* some commands want the name... */
	if fsyscmd[i].f1 != nil {
		if name == FsysAll {
			return fmt.Errorf("cannot use fsys %#q with %#q command", FsysAll, argv[0])
		}
		return fsyscmd[i].f1(name, argv)
	}

	/* ... but most commands want the Fsys */
	var err error
	if name == FsysAll {
		sbox.lock.RLock()
		for fsys := sbox.head; fsys != nil; fsys = fsys.next {
			fsys.ref++
			err1 := fsysXXX1(fsys, i, argv)
			if err == nil && err1 != nil {
				err = err1 // preserve error through loop iterations
			}
			fsys.ref--
		}
		sbox.lock.RUnlock()
	} else {
		var fsys *Fsys
		fsys, err = _fsysGet(name)
		if err != nil {
			return err
		}
		err = fsysXXX1(fsys, i, argv)
		fsysPut(fsys)
	}
	return err
}

func cmdFsysXXX(argv []string) error {
	name := sbox.curfsys
	if name == "" {
		return errors.New(EFsysNoCurrent)
	}

	return fsysXXX(name, argv)
}

func cmdFsys(argv []string) error {
	var usage string = "Usage: fsys [name ...]"

	flags := flag.NewFlagSet("fsys", flag.ContinueOnError)
	flags.Usage = func() { fmt.Fprintln(os.Stderr, usage); flags.PrintDefaults() }
	if err := flags.Parse(argv[1:]); err != nil {
		return EUsage
	}
	argv = flags.Args()
	argc := flags.NArg()
	if argc == 0 {
		sbox.lock.RLock()
		if sbox.head == nil {
			return errors.New("no current fsys")
		}
		for fsys := sbox.head; fsys != nil; fsys = fsys.next {
			printf("\t%s\n", fsys.name)
		}
		sbox.lock.RUnlock()
		return nil
	}

	if argc == 1 {
		fsys := (*Fsys)(nil)
		if argv[0] != FsysAll {
			var err error
			fsys, err = fsysGet(argv[0])
			if err != nil {
				return err
			}
		}
		sbox.curfsys = argv[0]
		consPrompt(sbox.curfsys)
		if fsys != nil {
			fsysPut(fsys)
		}
		return nil
	}

	return fsysXXX(argv[0], argv[1:])
}

func fsysInit() error {
	sbox.lock = new(sync.RWMutex)

	cliAddCmd("fsys", cmdFsys)
	for _, cmd := range fsyscmd {
		if cmd.f != nil {
			cliAddCmd(cmd.cmd, cmdFsysXXX)
		}
	}

	/* the venti cmd is special: the fs can be either open or closed */
	cliAddCmd("venti", cmdFsysXXX)
	cliAddCmd("printconfig", cmdPrintConfig)

	return nil
}

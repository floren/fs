package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"strings"
	"sync"
	"syscall"

	"9fans.net/go/plan9"
)

const (
	NConInit     = 128
	NMsgInit     = 384
	NMsgProcInit = 64
	NMsizeInit   = 8192 + 24
)

var mbox struct {
	alock   *sync.Mutex // alloc
	ahead   *Msg
	arendez *sync.Cond

	maxmsg     int
	nmsg       int
	nmsgstarve int

	rlock   *sync.Mutex // read
	rhead   *Msg
	rtail   *Msg
	rrendez *sync.Cond

	maxproc     int
	nproc       int
	nprocstarve int

	msize uint32 // immutable
}

var cbox struct {
	alock   *sync.Mutex // alloc
	ahead   *Con
	arendez *sync.Cond

	clock *sync.RWMutex
	chead *Con
	ctail *Con

	maxcon     int
	ncon       int
	nconstarve int

	msize uint32
}

func conFree(con *Con) {
	assert(con.version == nil)
	assert(con.mhead == nil)
	assert(con.whead == nil)
	assert(con.nfid == 0)
	assert(con.state == ConMoribund)

	if con.fd >= 0 {
		syscall.Close(con.fd)
		con.fd = -1
	}

	con.state = ConDead
	con.aok = 0
	con.flags = 0
	con.isconsole = 0

	cbox.alock.Lock()
	if con.cprev != nil {
		con.cprev.cnext = con.cnext
	} else {

		cbox.chead = con.cnext
	}
	if con.cnext != nil {
		con.cnext.cprev = con.cprev
	} else {

		cbox.ctail = con.cprev
	}
	con.cnext = nil
	con.cprev = con.cnext

	if cbox.ncon > cbox.maxcon {
		cbox.ncon--
		cbox.alock.Unlock()
		return
	}

	con.anext = cbox.ahead
	cbox.ahead = con
	if con.anext == nil {
		cbox.arendez.Signal()
	}
	cbox.alock.Unlock()
}

func msgFree(m *Msg) {
	assert(m.rwnext == nil)
	assert(m.flush == nil)

	mbox.alock.Lock()
	if mbox.nmsg > mbox.maxmsg {
		mbox.nmsg--
		mbox.alock.Unlock()
		return
	}

	m.anext = mbox.ahead
	mbox.ahead = m
	if m.anext == nil {
		mbox.arendez.Signal()
	}
	mbox.alock.Unlock()
}

func msgAlloc(con *Con) *Msg {
	mbox.alock.Lock()
	for mbox.ahead == nil {
		if mbox.nmsg >= mbox.maxmsg {
			mbox.nmsgstarve++
			mbox.arendez.Wait()
			continue
		}

		m := &Msg{
			data:  make([]byte, mbox.msize),
			msize: mbox.msize,
		}
		mbox.nmsg++
		mbox.ahead = m
		break
	}

	m := mbox.ahead
	mbox.ahead = m.anext
	m.anext = nil
	mbox.alock.Unlock()

	m.con = con
	m.state = MsgR
	m.nowq = 0

	return m
}

func msgMunlink(m *Msg) {
	var con *Con

	con = m.con

	if m.mprev != nil {
		m.mprev.mnext = m.mnext
	} else {

		con.mhead = m.mnext
	}
	if m.mnext != nil {
		m.mnext.mprev = m.mprev
	} else {

		con.mtail = m.mprev
	}
	m.mnext = nil
	m.mprev = m.mnext
}

func msgFlush(m *Msg) {
	var con *Con
	var flush *Msg
	var old *Msg

	con = m.con

	if *Dflag {
		fmt.Fprintf(os.Stderr, "msgFlush %v\n", &m.t)
	}

	/*
	 * If this Tflush has been flushed, nothing to do.
	 * Look for the message to be flushed in the
	 * queue of all messages still on this connection.
	 * If it's not found must assume Elvis has already
	 * left the building and reply normally.
	 */
	con.mlock.Lock()

	if m.state == MsgF {
		con.mlock.Unlock()
		return
	}

	for old = con.mhead; old != nil; old = old.mnext {
		if old.t.Tag == m.t.Oldtag {
			break
		}
	}
	if old == nil {
		if *Dflag {
			fmt.Fprintf(os.Stderr, "msgFlush: cannot find %d\n", m.t.Oldtag)
		}
		con.mlock.Unlock()
		return
	}

	if *Dflag {
		fmt.Fprintf(os.Stderr, "\tmsgFlush found %v\n", &old.t)
	}

	/*
	 * Found it.
	 * There are two cases where the old message can be
	 * truly flushed and no reply to the original message given.
	 * The first is when the old message is in MsgR state; no
	 * processing has been done yet and it is still on the read
	 * queue. The second is if old is a Tflush, which doesn't
	 * affect the server state. In both cases, put the old
	 * message into MsgF state and let MsgWrite toss it after
	 * pulling it off the queue.
	 */
	if old.state == MsgR || old.t.Type == plan9.Tflush {

		old.state = MsgF
		if *Dflag {
			fmt.Fprintf(os.Stderr, "msgFlush: change %d from MsgR to MsgF\n", m.t.Oldtag)
		}
	}

	/*
	 * Link this flush message and the old message
	 * so multiple flushes can be coalesced (if there are
	 * multiple Tflush messages for a particular pending
	 * request, it is only necessary to respond to the last
	 * one, so any previous can be removed) and to be
	 * sure flushes wait for their corresponding old
	 * message to go out first.
	 * Waiting flush messages do not go on the write queue,
	 * they are processed after the old message is dealt
	 * with. There's no real need to protect the setting of
	 * Msg.nowq, the only code to check it runs in this
	 * process after this routine returns.
	 */
	flush = old.flush
	if flush != nil {

		if *Dflag {
			fmt.Fprintf(os.Stderr, "msgFlush: remove %d from %d list\n", old.flush.t.Tag, old.t.Tag)
		}
		m.flush = flush.flush
		flush.flush = nil
		msgMunlink(flush)
		msgFree(flush)
	}

	old.flush = m
	m.nowq = 1

	if *Dflag {
		fmt.Fprintf(os.Stderr, "msgFlush: add %d to %d queue\n", m.t.Tag, old.t.Tag)
	}
	con.mlock.Unlock()
}

func msgProc() {
	panic("TODO")
	/*
		var m *Msg
		var con *Con

		//vtThreadSetName("msgProc")

		for {
			// If surplus to requirements, exit.
			// If not, wait for and pull a message off
			// the read queue.
			mbox.rlock.Lock()

			if mbox.nproc > mbox.maxproc {
				mbox.nproc--
				mbox.rlock.Unlock()
				break
			}

			for mbox.rhead == nil {
				mbox.rrendez.Wait()
			}
			m = mbox.rhead
			mbox.rhead = m.rwnext
			m.rwnext = nil
			mbox.rlock.Unlock()

			con = m.con
			var e error

			// If the message has been flushed before
			// any 9P processing has started, mark it so
			// none will be attempted.
			con.mlock.Lock()
			if m.state == MsgF {
				e = errors.New("flushed")
			} else {
				m.state = Msg9
			}
			con.mlock.Unlock()

			if e == nil {
				// explain this
				con.lock.Lock()
				if m.t.Type == plan9.Tversion {
					con.version = m
					con.state = ConDown
					for con.mhead != m {
						con.rendez.Wait()
					}
					assert(con.state == ConDown)
					if con.version == m {
						con.version = nil
						con.state = ConInit
					} else {
						e = errors.New("Tversion aborted")
					}
				} else if con.state != ConUp {
					e = errors.New("connection not ready")
				}
				con.lock.Unlock()
			}

			// Dispatch if not error already.
			m.r.Tag = m.t.Tag
			if e == nil {
				var r *plan9.Fcall
				r, e = plan9.ReadFcall(m)
				m.r = *r
			}
			if e != nil {
				m.r.Type = plan9.Rerror
				m.r.Ename = e.Error()
			} else {
				m.r.Type = m.t.Type + 1
			}

			// Put the message (with reply) on the
			// write queue and wakeup the write process.
			if m.nowq == 0 {
				con.wlock.Lock()
				if con.whead == nil {
					con.whead = m
				} else {

					con.wtail.rwnext = m
				}
				con.wtail = m
				con.wrendez.Signal()
				con.wlock.Unlock()
			}
		}
	*/
}

func msgRead(con *Con) {
	panic("TODO")
	/*
		var m *Msg
		var eof int
		var fd int
		var n int

		//vtThreadSetName("msgRead")

		fd = con.fd
		eof = 0

		for eof == 0 {
			m = msgAlloc(con)

			for {
				n = read9pmsg(fd, m.data, con.msize)
				if n != 0 {
					break
				}
			}
			if n < 0 {
				m.t.Type = plan9.Tversion
				m.t.Fid = ^uint32(0)
				m.t.Tag = ^uint16(0)
				m.t.Msize = con.msize
				m.t.Version = "9PEoF"
				eof = 1
			} else if convM2S(m.data, uint(n), &m.t) != uint(n) {
				if *Dflag {
					fmt.Fprintf(os.Stderr, "msgRead: convM2S error: %s\n", con.name)
				}
				msgFree(m)
				continue
			}

			if *Dflag {
				fmt.Fprintf(os.Stderr, "msgRead %p: t %v\n", con, &m.t)
			}

			con.mlock.Lock()
			if con.mtail != nil {
				m.mprev = con.mtail
				con.mtail.mnext = m
			} else {

				con.mhead = m
				m.mprev = nil
			}

			con.mtail = m
			con.mlock.Unlock()

			mbox.rlock.Lock()
			if mbox.rhead == nil {
				mbox.rhead = m
				// TODO: sync.Cond.Signal() never fails (but vtWakeup does)
				//if mbox.rrendez.Signal() == 0 {
				//	if mbox.nproc < mbox.maxproc {
				//		go msgProc()
				//		mbox.nproc++
				//	} else {
				//		mbox.nprocstarve++
				//	}
				//}

				// don't need this surely?
				//mbox.rrendez.Signal()
			} else {
				mbox.rtail.rwnext = m
			}
			mbox.rtail = m
			mbox.rlock.Unlock()
		}
	*/
}

func msgWrite(con *Con) {
	panic("TODO")
	/*
		var eof int
		var n int
		var flush *Msg
		var m *Msg

		//vtThreadSetName("msgWrite")

		go msgRead(con)

		for {
			 // Wait for and pull a message off the write queue.
			con.wlock.Lock()

			for con.whead == nil {
				con.wrendez.Wait()
			}
			m = con.whead
			con.whead = m.rwnext
			m.rwnext = nil
			assert(m.nowq == 0)
			con.wlock.Unlock()

			eof = 0

			// Write each message (if it hasn't been flushed)
			// followed by any messages waiting for it to complete.
			con.mlock.Lock()

			for m != nil {
				msgMunlink(m)

				if *Dflag {
					fmt.Fprintf(os.Stderr, "msgWrite %d: r %v\n", m.state, &m.r)
				}

				if m.state != MsgF {
					m.state = MsgW
					con.mlock.Unlock()

					n = int(convS2M(&m.r, con.data, con.msize))
					if nn, err := syscall.Write(con.fd, con.data); nn != n || err != nil {
						eof = 1
					}

					con.mlock.Lock()
				}

				flush = m.flush
				if flush != nil {
					assert(flush.nowq != 0)
					m.flush = nil
				}

				msgFree(m)
				m = flush
			}

			con.mlock.Unlock()

			con.lock.Lock()
			if eof != 0 && con.fd >= 0 {
				syscall.Close(con.fd)
				con.fd = -1
			}

			if con.state == ConDown {
				con.rendez.Signal()
			}
			if con.state == ConMoribund && con.mhead == nil {
				con.lock.Unlock()
				conFree(con)
				break
			}

			con.lock.Unlock()
		}
	*/
}

func conAlloc(fd int, name string, flags int) *Con {
	cbox.alock.Lock()
	for cbox.ahead == nil {
		if cbox.ncon >= cbox.maxcon {
			cbox.nconstarve++
			cbox.arendez.Wait()
			continue
		}

		con := &Con{
			lock:    new(sync.Mutex),
			data:    make([]byte, cbox.msize),
			msize:   cbox.msize,
			alock:   new(sync.Mutex),
			mlock:   new(sync.Mutex),
			wlock:   new(sync.Mutex),
			fidlock: new(sync.Mutex),
		}
		con.rendez = sync.NewCond(con.lock)
		con.mrendez = sync.NewCond(con.mlock)
		con.wrendez = sync.NewCond(con.wlock)

		cbox.ncon++
		cbox.ahead = con
		break
	}

	con := cbox.ahead
	cbox.ahead = con.anext
	con.anext = nil

	if cbox.ctail != nil {
		con.cprev = cbox.ctail
		cbox.ctail.cnext = con
	} else {
		cbox.chead = con
		con.cprev = nil
	}

	cbox.ctail = con

	assert(con.mhead == nil)
	assert(con.whead == nil)
	assert(con.fhead == nil)
	assert(con.nfid == 0)

	con.state = ConNew
	con.fd = fd
	if con.name != "" {
		con.name = ""
	}

	if name != "" {
		con.name = name
	} else {
		con.name = "unknown"
	}
	con.remote = [128]byte{}
	rfd, err := syscall.Open(fmt.Sprintf("%s/remote", con.name), 0, 0)
	if err == nil {
		buf := make([]byte, 128)
		n, err := syscall.Read(rfd, buf)
		syscall.Close(rfd)
		if err == nil {
			i := bytes.IndexByte(buf[:n], '\n')
			if i >= 0 {
				buf = buf[:i]
			}
			copy(con.remote[:], buf)
		}
	}

	con.flags = flags
	con.isconsole = 0
	cbox.alock.Unlock()

	go msgWrite(con)

	return con
}

func cmdMsg(argv []string) error {
	var usage string = "usage: msg [-m nmsg] [-p nproc]"

	flags := flag.NewFlagSet("msg", flag.ContinueOnError)
	maxmsg := flags.Int("m", 0, "nmsg")
	maxproc := flags.Int("p", 0, "nproc")
	flags.Parse(argv[1:])
	if *maxmsg < 0 {
		return fmt.Errorf(usage)
	}
	if *maxproc < 0 {
		return fmt.Errorf(usage)
	}
	if flags.NArg() != 0 {
		return fmt.Errorf(usage)
	}

	mbox.alock.Lock()
	if *maxmsg > 0 {
		mbox.maxmsg = *maxmsg
	}
	*maxmsg = mbox.maxmsg
	nmsg := mbox.nmsg
	nmsgstarve := mbox.nmsgstarve
	mbox.alock.Unlock()

	mbox.rlock.Lock()
	if *maxproc > 0 {
		mbox.maxproc = *maxproc
	}
	*maxproc = mbox.maxproc
	nproc := mbox.nproc
	nprocstarve := mbox.nprocstarve
	mbox.rlock.Unlock()

	consPrintf("\tmsg -m %d -p %d\n", maxmsg, maxproc)
	consPrintf("\tnmsg %d nmsgstarve %d nproc %d nprocstarve %d\n", nmsg, nmsgstarve, nproc, nprocstarve)

	return nil
}

func scmp(a *Fid, b *Fid) int {
	if a == nil {
		return 0
	}
	if b == nil {
		return -1
	}
	return strings.Compare(a.uname, b.uname)
}

func fidMerge(a *Fid, b *Fid) *Fid {
	var s *Fid
	var l **Fid

	l = &s
	for a != nil || b != nil {
		if scmp(a, b) < 0 {
			*l = a
			l = &a.sort
			a = a.sort
		} else {

			*l = b
			l = &b.sort
			b = b.sort
		}
	}

	*l = nil
	return s
}

func fidMergeSort(f *Fid) *Fid {
	var delay int
	var a *Fid
	var b *Fid

	if f == nil {
		return nil
	}
	if f.sort == nil {
		return f
	}

	b = f
	a = b
	delay = 1
	for a != nil && b != nil {
		if delay != 0 { /* easy way to handle 2-element list */
			delay = 0
		} else {

			a = a.sort
		}
		b = b.sort
		if b != nil {
			b = b.sort
		}
	}

	b = a.sort
	a.sort = nil

	a = fidMergeSort(f)
	b = fidMergeSort(b)

	return fidMerge(a, b)
}

func cmdWho(argv []string) error {
	var usage string = "usage: who"

	flags := flag.NewFlagSet("who", flag.ContinueOnError)
	flags.Parse(argv[1:])
	if flags.NArg() != 0 {
		return fmt.Errorf(usage)
	}

	cbox.clock.RLock()
	l1 := 0
	l2 := 0
	for con := cbox.chead; con != nil; con = con.cnext {
		l := len(con.name)
		if l > l1 {
			l1 = l
		}
		l = len(con.remote)
		if l > l2 {
			l2 = l
		}
	}

	for con := cbox.chead; con != nil; con = con.cnext {
		consPrintf("\t%-*s %-*s", l1, con.name, l2, con.remote)
		con.fidlock.Lock()
		var last *Fid = nil
		for i := 0; i < NFidHash; i++ {
			for fid := con.fidhash[i]; fid != nil; fid = fid.hash {
				if fid.fidno != ^uint32(0) && fid.uname != "" {
					fid.sort = last
					last = fid
				}
			}
		}

		fid := fidMergeSort(last)
		last = nil
		for ; fid != nil; (func() { last = fid; fid = fid.sort })() {
			if last == nil || fid.uname != last.uname {
				consPrintf(" %q", fid.uname)
			}
		}
		con.fidlock.Unlock()
		consPrintf("\n")
	}

	cbox.clock.RUnlock()
	return nil
}

func msgInit() {
	mbox.alock = new(sync.Mutex)
	mbox.arendez = sync.NewCond(mbox.alock)

	mbox.rlock = new(sync.Mutex)
	mbox.rrendez = sync.NewCond(mbox.rlock)

	mbox.maxmsg = NMsgInit
	mbox.maxproc = NMsgProcInit
	mbox.msize = NMsizeInit

	cliAddCmd("msg", cmdMsg)
}

func cmdCon(argv []string) error {
	var usage string = "usage: con [-m ncon]"

	flags := flag.NewFlagSet("con", flag.ContinueOnError)
	maxcon := flags.Int("m", 0, "ncon")
	flags.Parse(argv[1:])
	if *maxcon < 0 {
		return fmt.Errorf(usage)
	}
	if flags.NArg() != 0 {
		return fmt.Errorf(usage)
	}

	cbox.clock.Lock()
	if *maxcon > 0 {
		cbox.maxcon = *maxcon
	}
	*maxcon = cbox.maxcon
	ncon := cbox.ncon
	nconstarve := cbox.nconstarve
	cbox.clock.Unlock()

	consPrintf("\tcon -m %d\n", maxcon)
	consPrintf("\tncon %d nconstarve %d\n", ncon, nconstarve)

	cbox.clock.RLock()
	for con := cbox.chead; con != nil; con = con.cnext {
		consPrintf("\t%s\n", con.name)
	}
	cbox.clock.RUnlock()

	return nil
}

func conInit() {
	cbox.alock = new(sync.Mutex)
	cbox.arendez = sync.NewCond(cbox.alock)

	cbox.clock = new(sync.RWMutex)

	cbox.maxcon = NConInit
	cbox.msize = NMsizeInit

	cliAddCmd("con", cmdCon)
	cliAddCmd("who", cmdWho)
}

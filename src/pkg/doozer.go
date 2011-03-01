package doozer

import (
	"doozer/ack"
	"doozer/client"
	"doozer/gc"
	"doozer/lock"
	"doozer/member"
	"doozer/consensus"
	"doozer/server"
	"doozer/session"
	"doozer/store"
	"doozer/util"
	"doozer/web"
	"goprotobuf.googlecode.com/hg/proto"
	"net"
	"os"
	"time"
)

const alpha = 50

const slot = "/doozer/slot"

var slots = store.MustCompileGlob("/doozer/slot/*")


type proposer struct {
	seqns chan int64
	props chan *consensus.Prop
	st    *store.Store
}


func (p *proposer) Propose(v []byte) (e store.Event) {
	for e.Mut != string(v) {
		n := <-p.seqns
		w := p.st.Wait(n)
		p.props <- &consensus.Prop{n, v}
		e = <-w
	}
	return
}


func Main(clusterName, attachAddr string, udpConn net.PacketConn, listener, webListener net.Listener, pulseInterval, fillDelay, kickTimeout int64) {
	logger := util.NewLogger("main")

	var err os.Error

	listenAddr := listener.Addr().String()

	var activateSeqn int64
	cal := make(chan bool, 1)
	useSelf := make(chan bool, 1)

	var cl *client.Client
	self := util.RandId()
	st := store.New()
	if attachAddr == "" { // we are the only node in a new cluster
		set(st, "/doozer/info/"+self+"/addr", listenAddr, store.Missing)
		set(st, "/doozer/info/"+self+"/public-addr", listenAddr, store.Missing)
		set(st, "/doozer/info/"+self+"/hostname", os.Getenv("HOSTNAME"), store.Missing)
		set(st, "/doozer/info/"+self+"/version", Version, store.Missing)
		set(st, "/doozer/members/"+self, listenAddr, store.Missing)
		set(st, "/doozer/slot/"+"1", self, store.Missing)
		set(st, "/ping", "pong", store.Missing)

		cal <- true
		useSelf <- true

		cl = client.New("local", listenAddr) // TODO use real cluster name
	} else {
		cl = client.New("local", attachAddr) // TODO use real cluster name

		path := "/doozer/info/" + self + "/addr"
		_, err = cl.Set(path, store.Clobber, []byte(listenAddr))
		if err != nil {
			panic(err)
		}

		path = "/doozer/info/" + self + "/public-addr"
		_, err = cl.Set(path, store.Clobber, []byte(listenAddr))
		if err != nil {
			panic(err)
		}

		path = "/doozer/info/" + self + "/hostname"
		_, err = cl.Set(path, store.Clobber, []byte(os.Getenv("HOSTNAME")))
		if err != nil {
			panic(err)
		}

		path = "/doozer/info/" + self + "/version"
		_, err = cl.Set(path, store.Clobber, []byte(Version))
		if err != nil {
			panic(err)
		}

		joinSeqn, snap, err := cl.Join(self, listenAddr)
		if err != nil {
			panic(err)
		}

		st.Ops <- store.Op{1, snap}

		go func() {
			advanceUntil(cl, st.Seqns, joinSeqn+alpha)
			activateSeqn = activate(st, self, cl)
			cal <- true
			advanceUntil(cl, st.Seqns, activateSeqn+alpha)
			useSelf <- true
		}()

		// TODO sink needs a way to pick up missing values if there are any
		// gaps in its sequence
	}

	acker := ack.Ackify(udpConn)

	pr := &proposer{
		seqns: make(chan int64, alpha),
		props: make(chan *consensus.Prop),
		st:    st,
	}

	start := <-st.Seqns
	<-st.Wait(start)
	cmw := st.Watch(store.Any)
	in := make(chan consensus.Packet, 50)
	out := make(chan consensus.Packet, 50)

	consensus.NewManager(self, start, alpha, in, out, st.Ops, pr.seqns, pr.props, cmw, fillDelay)

	if attachAddr == "" {
		// Skip ahead alpha steps so that the registrar can provide a
		// meaningful cluster.
		for i := start+1; i < start+alpha+1; i++ {
			st.Ops <- store.Op{i, store.Nop}
		}
	}

	times := make(map[string]int64)
	shun := make(chan string, 3) // sufficient for a cluster of 7

	go member.Clean(shun, st, pr)

	go func() {
		<-cal
		go lock.Clean(st, pr)
		go session.Clean(st, pr)
		go gc.Pulse(self, st.Seqns, pr, pulseInterval)
		go gc.Clean(st)
	}()

	sv := &server.Server{listenAddr, st, pr, self, alpha}

	go sv.Serve(listener, useSelf)

	if webListener != nil {
		web.Store = st
		web.ClusterName = clusterName
		go web.Serve(webListener)
	}

	go func() {
		for p := range out {
			acker.WriteTo(p.Data, p.Addr)
		}
	}()

	var pt int64
	pi := kickTimeout / 2
	for {
		t := time.Nanoseconds()

		data, addr, err := acker.ReadFrom()
		if err == os.EINVAL {
			break
		}
		if err != nil {
			logger.Println(err)
			continue
		}

		// Update liveness time stamp for this addr
		times[addr] = t

		if t > pt + pi {
			n := t - kickTimeout
			for addr, s := range times {
				if n > s && addr != self {
					times[addr] = 0, false
					shun <- addr
				}
			}
		}
		pt = t

		// TODO delete this hack when we have TCP following
		// begin hack
		// shortcut the consensus stuff just for this one value
		var m consensus.M
		err = proto.Unmarshal(data, &m)
		if err == nil && *m.Seqn < start+alpha+1 && *m.Cmd == 7 {
			st.Ops <- store.Op{*m.Seqn, string(m.Value)}
		}
		// end hack

		in <- consensus.Packet{addr, data}
	}
}

func activate(st *store.Store, self string, c *client.Client) int64 {
	logger := util.NewLogger("activate")
	w := store.NewWatch(st, slots)

	for _, base := range store.Getdir(st, slot) {
		p := slot + "/" + base
		v, cas := st.Get(p)
		if cas != store.Dir && v[0] == "" {
			seqn, err := c.Set(p, cas, []byte(self))
			if err != nil {
				logger.Println(err)
				continue
			}

			w.Stop()
			return seqn
		}
	}

	for ev := range w.C {
		// TODO ev.IsEmpty()
		if ev.IsSet() && ev.Body == "" {
			seqn, err := c.Set(ev.Path, ev.Cas, []byte(self))
			if err != nil {
				logger.Println(err)
				continue
			}
			w.Stop()
			return seqn
		}
	}

	return 0
}

func advanceUntil(cl *client.Client, ver <-chan int64, done int64) {
	for <-ver < done {
		cl.Noop()
	}
}

func set(st *store.Store, path, body string, cas int64) {
	mut := store.MustEncodeSet(path, body, cas)
	st.Ops <- store.Op{1 + <-st.Seqns, mut}
}

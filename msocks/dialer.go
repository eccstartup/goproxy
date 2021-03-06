package msocks

import (
	"errors"
	"fmt"
	"github.com/shell909090/goproxy/sutils"
	"net"
	"sync"
	"time"
)

type Dialer struct {
	sutils.Dialer
	serveraddr string
	username   string
	password   string
	lock       sync.Mutex
	sess       *Session
}

func NewDialer(dialer sutils.Dialer, serveraddr string,
	username, password string) (d *Dialer, err error) {
	d = &Dialer{
		Dialer:     dialer,
		serveraddr: serveraddr,
		username:   username,
		password:   password,
	}
	return
}

func (d *Dialer) Cutoff() {
	d.lock.Lock()
	defer d.lock.Unlock()
	if d.sess != nil {
		d.sess.Close()
	}
}

func (d *Dialer) createConn() (conn net.Conn, err error) {
	logger.Noticef("create connect, serveraddr: %s.",
		d.serveraddr)
	conn, err = d.Dialer.Dial("tcp", d.serveraddr)
	if err != nil {
		logger.Err(err)
		return
	}

	logger.Noticef("auth with username: %s, password: %s.",
		d.username, d.password)
	b, err := NewFrameAuth(0, d.username, d.password)
	if err != nil {
		logger.Err(err)
		return
	}
	_, err = conn.Write(b)
	if err != nil {
		return
	}

	f, err := ReadFrame(conn)
	if err != nil {
		return
	}

	switch ft := f.(type) {
	default:
		err = errors.New("unexpected package")
		logger.Err(err)
		return
	case *FrameOK:
		logger.Notice("auth ok.")
	case *FrameFAILED:
		conn.Close()
		err = fmt.Errorf("create connection failed with code: %d.",
			ft.Errno)
		logger.Err(err)
		return
	}

	return
}

func (d *Dialer) createSession() (err error) {
	d.lock.Lock()
	defer d.lock.Unlock()

	if d.sess != nil {
		return
	}

	// retry
	var conn net.Conn
	for i := uint(0); i < RETRY_TIMES; i++ {
		conn, err = d.createConn()
		if err != nil {
			logger.Err(err)
			time.Sleep((1 << i) * time.Second)
		} else {
			break
		}
	}
	if err != nil {
		logger.Crit("can't connect to host, quit.")
		return
	}

	logger.Noticef("create session.")
	d.sess = NewSession(conn)
	d.sess.Ping()

	go func() {
		d.sess.Run()
		// that's mean session is dead
		logger.Warning("session runtime quit, reboot from connect.")

		// remove from sess
		d.lock.Lock()
		d.sess = nil
		d.lock.Unlock()

		d.createSession()
	}()
	return
}

func (d *Dialer) GetSess(create bool) (sess *Session) {
	if d.sess == nil && create {
		err := d.createSession()
		if err != nil {
			return
		}
	}
	return d.sess
}

func (d *Dialer) Dial(network, address string) (conn net.Conn, err error) {
	sess := d.GetSess(true)
	logger.Infof("try dial: %s => %s.",
		sess.conn.RemoteAddr().String(), address)

	// lock streamid and put chan for it
	ch := NewChanFrameSender(1)
	streamid, err := sess.PutIntoNextId(ch)
	if err != nil {
		return
	}

	b, err := NewFrameOneString(MSG_SYN, streamid, address)
	if err != nil {
		return
	}
	_, err = sess.Write(b)
	if err != nil {
		return
	}

	fr := ch.RecvWithTimeout(DIAL_TIMEOUT)

	switch frt := fr.(type) {
	default:
		err = errors.New("unknown status")
	case nil: // close all or timeout
		err = fmt.Errorf("connection failed for timeout(%d) or all closed(%p).", streamid, sess)
	case *FrameFAILED: // FAILED
		err = fmt.Errorf("connection failed for remote failed(%d): %d.",
			streamid, frt.Errno)
	case *FrameOK: // OK
		logger.Info("connect ok.")
	}

	if err != nil {
		sess.RemovePorts(streamid)
		ch.Close()
		return
	}

	c := NewConn(streamid, sess, address)
	sess.PutIntoId(streamid, c)
	ch.Close()
	logger.Noticef("new conn: %p(%d) => %s.",
		sess, streamid, address)
	return c, nil
}

func (d *Dialer) LookupIP(hostname string) (ipaddr []net.IP, err error) {
	logger.Noticef("lookup ip: %s", hostname)
	sess := d.GetSess(true)

	// lock streamid and put chan for it
	ch := NewChanFrameSender(1)
	streamid, err := sess.PutIntoNextId(ch)
	if err != nil {
		return
	}

	b, err := NewFrameOneString(MSG_DNS, streamid, hostname)
	if err != nil {
		logger.Err(err)
		return
	}
	_, err = sess.Write(b)
	if err != nil {
		return
	}

	fr := ch.RecvWithTimeout(LOOKUP_TIMEOUT)
	sess.RemovePorts(streamid)
	ch.Close()

	switch frt := fr.(type) {
	default:
		err = errors.New("unknown status")
	case nil: // close all or timeout
		err = fmt.Errorf("lookup ip failed for timeout(%d) or all closed(%p).", streamid, sess)
	case *FrameFAILED: // FAILED
		err = fmt.Errorf("lookup ip failed for remote failed(%d): %d.",
			streamid, frt.Errno)
	case *FrameAddr: // OK
		logger.Infof("lookup ip ok.")
		ipaddr = frt.Ipaddr
	}

	if err != nil {
		logger.Err(err)
	}
	return
}

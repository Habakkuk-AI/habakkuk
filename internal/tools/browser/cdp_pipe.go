//go:build !windows

package browser

import (
	"bufio"
	"os"
	"sync"
)

// pipeTransport speaks the pipe protocol: JSON messages ended by a NUL byte.
type pipeTransport struct {
	wmu sync.Mutex
	w   *os.File // the browser reads commands from it (its fd 3)
	rf  *os.File // the browser writes replies to it (its fd 4)
	r   *bufio.Reader
}

func newPipeTransport(w, r *os.File) *pipeTransport {
	return &pipeTransport{w: w, rf: r, r: bufio.NewReaderSize(r, 1<<20)}
}

func (p *pipeTransport) send(msg []byte) error {
	p.wmu.Lock()
	defer p.wmu.Unlock()
	_, err := p.w.Write(append(msg, 0))
	return err
}

func (p *pipeTransport) recv() ([]byte, error) {
	b, err := p.r.ReadBytes(0)
	if err != nil {
		return nil, err
	}
	return b[:len(b)-1], nil
}

func (p *pipeTransport) close() error {
	err := p.w.Close()
	if e := p.rf.Close(); err == nil {
		err = e
	}
	return err
}

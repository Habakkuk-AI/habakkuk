package tui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	tea "github.com/charmbracelet/bubbletea"
)

// Run shows the UI on the terminal until the user quits or ctx ends. start
// runs the workers that run turns and returns a func that stops them; Run
// calls it once the session given in o is attached, so a turn that resumes
// there asks its approvals here. Turns still running when Run returns
// resume on the next start, since every turn is a durable job. A line
// naming the session is written to out on the way out.
func Run(ctx context.Context, be Backend, o Options, start func() (stop func()), out io.Writer) error {
	if !o.Theme.ok {
		o.Theme = DetectTheme()
	}
	o.Context = ctx
	br := newBridge()
	m := New(be, o, br)
	if o.Session != "" {
		info, err := be.Session(ctx, o.Session)
		if err != nil {
			return err
		}
		m.initCmd = m.open(info, "Resumed this session.")
	}
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion(), tea.WithContext(ctx))
	br.setSend(p.Send)

	var wg sync.WaitGroup
	var stop func()
	if start != nil {
		wg.Add(1)
		go func() { defer wg.Done(); stop = start() }() // MCP servers may take a while to connect
	}
	_, err := p.Run()
	close(br.done)
	busy, sid := m.busy, m.sess.ID
	m.closeSession()
	wg.Wait()
	if stop != nil {
		stop()
	}
	if sid != "" && out != nil {
		fmt.Fprintf(out, "Session %s. Continue it with: hk chat -session %s\n", sid, sid)
		if busy {
			fmt.Fprintln(out, "A turn was still running; it resumes the next time Habakkuk runs.")
		}
	}
	if errors.Is(err, tea.ErrProgramKilled) && ctx.Err() != nil {
		return nil // ended by a signal
	}
	return err
}

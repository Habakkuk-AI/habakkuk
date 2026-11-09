package app

import "github.com/habakkuk-ai/habakkuk/internal/work/board"

// openBoard sets up the shared task board: its store, the dispatcher that
// runs cards as jobs, and the board tools. It runs after the channels are
// open so chat notices for finished cards can be delivered.
func (a *App) openBoard() {
	a.Board = board.NewStore(a.Control, a.Queue)
	a.Dispatcher = &board.Dispatcher{Store: a.Board, Queue: a.Queue, Runner: a.Runner, Log: a.Log}
	if a.Gateway != nil {
		a.Dispatcher.Deliver = a.Gateway.Deliver
	}
	board.RegisterTools(a.Tools, a.Dispatcher, a.here)
}

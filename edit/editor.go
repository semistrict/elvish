// Package edit implements a command line editor.
package edit

import (
	"fmt"
	"os"
	"strings"
	"syscall"

	"github.com/elves/elvish/errutil"
	"github.com/elves/elvish/eval"
	"github.com/elves/elvish/logutil"
	"github.com/elves/elvish/parse"
	"github.com/elves/elvish/store"
	"github.com/elves/elvish/sys"
)

var Logger = logutil.Discard

const (
	lackEOLRune = '\u23ce'
	lackEOL     = "\033[7m" + string(lackEOLRune) + "\033[m"
)

type bufferMode int

const (
	modeInsert bufferMode = iota
	modeCommand
	modeCompletion
	modeNavigation
	modeHistory
)

type editorState struct {
	// States used during ReadLine. Reset at the beginning of ReadLine.
	active                bool
	savedTermios          *sys.Termios
	tokens                []Token
	prompt, rprompt, line string
	dot                   int
	tips                  []string
	mode                  bufferMode
	completion            *completion
	completionLines       int
	navigation            *navigation
	history               history
	isExternal            map[string]bool
	// Used for builtins.
	lastKey    Key
	nextAction action
}

type history struct {
	current int
	prefix  string
	line    string
}

// Editor keeps the status of the line editor.
type Editor struct {
	file      *os.File
	writer    *writer
	reader    *Reader
	sigs      chan os.Signal
	histories []string
	store     *store.Store
	evaler    *eval.Evaler
	cmdSeq    int
	editorState
}

// LineRead is the result of ReadLine. Exactly one member is non-zero, making
// it effectively a tagged union.
type LineRead struct {
	Line string
	EOF  bool
	Err  error
}

func (h *history) jump(i int, line string) {
	h.current = i
	h.line = line
}

func (ed *Editor) appendHistory(line string) {
	ed.histories = append(ed.histories, line)
	if ed.store != nil {
		ed.store.AddCmd(line)
		// TODO(xiaq): Report possible error
	}
}

func lastHistory(histories []string, upto int, prefix string) (int, string) {
	for i := upto - 1; i >= 0; i-- {
		if strings.HasPrefix(histories[i], prefix) {
			return i, histories[i]
		}
	}
	return -1, ""
}

func firstHistory(histories []string, from int, prefix string) (int, string) {
	for i := from; i < len(histories); i++ {
		if strings.HasPrefix(histories[i], prefix) {
			return i, histories[i]
		}
	}
	return -1, ""
}

func (ed *Editor) prevHistory() bool {
	if ed.history.current > 0 {
		// Session history
		i, line := lastHistory(ed.histories, ed.history.current, ed.history.prefix)
		if i >= 0 {
			ed.history.jump(i, line)
			return true
		}
	}

	if ed.store != nil {
		// Persistent history
		upto := ed.cmdSeq + min(0, ed.history.current)
		i, line, err := ed.store.LastCmd(upto, ed.history.prefix)
		if err == nil {
			ed.history.jump(i-ed.cmdSeq, line)
			return true
		}
	}
	// TODO(xiaq): Errors other than ErrNoMatchingCmd should be reported
	return false
}

func (ed *Editor) nextHistory() bool {
	if ed.store != nil {
		// Persistent history
		if ed.history.current < -1 {
			from := ed.cmdSeq + ed.history.current + 1
			i, line, err := ed.store.FirstCmd(from, ed.history.prefix)
			if err == nil {
				ed.history.jump(i-ed.cmdSeq, line)
				return true
			}
			// TODO(xiaq): Errors other than ErrNoMatchingCmd should be reported
		}
	}

	from := max(0, ed.history.current+1)
	i, line := firstHistory(ed.histories, from, ed.history.prefix)
	if i >= 0 {
		ed.history.jump(i, line)
		return true
	}
	return false
}

// NewEditor creates an Editor.
func NewEditor(file *os.File, sigs chan os.Signal, ev *eval.Evaler, st *store.Store) *Editor {
	seq := -1
	if st != nil {
		var err error
		seq, err = st.NextCmdSeq()
		if err != nil {
			// TODO(xiaq): Also report the error
			seq = -1
		}
	}

	ed := &Editor{
		file:   file,
		writer: newWriter(file),
		reader: NewReader(file),
		sigs:   sigs,
		store:  st,
		evaler: ev,
		cmdSeq: seq,
	}
	ev.Editor = ed
	return ed
}

func (ed *Editor) flash() {
	// TODO implement fish-like flash effect
}

func (ed *Editor) pushTip(more string) {
	ed.tips = append(ed.tips, more)
}

func (ed *Editor) refresh() error {
	// Re-lex the line, unless we are in modeCompletion
	name := "[interacitve]"
	src := ed.line
	if ed.mode != modeCompletion {
		n, _ /*err*/ := parse.Parse(src)
		if n == nil {
			ed.tokens = []Token{{ParserError, src, nil, ""}}
		} else {
			ed.tokens = tokenize(src, n)
			_, err := ed.evaler.Compile(name, src, n)
			if err != nil {
				if err, ok := err.(*errutil.ContextualError); ok {
					ed.pushTip("compiler error highlighted")
					p := err.Pos()
					for i, token := range ed.tokens {
						if token.Node.Begin() <= p && p < token.Node.End() {
							ed.tokens[i].MoreStyle += styleForCompilerError
							break
						}
					}
				}
			}
		}
		for i, t := range ed.tokens {
			for _, colorist := range colorists {
				ed.tokens[i].MoreStyle += colorist(t.Node, ed)
			}
		}
	}
	return ed.writer.refresh(&ed.editorState)
}

// acceptCompletion accepts currently selected completion candidate.
func (ed *Editor) acceptCompletion() {
	c := ed.completion
	if 0 <= c.current && c.current < len(c.candidates) {
		accepted := c.candidates[c.current].source.text
		ed.insertAtDot(accepted)
	}
	ed.completion = nil
	ed.mode = modeInsert
}

// insertAtDot inserts text at the dot and moves the dot after it.
func (ed *Editor) insertAtDot(text string) {
	ed.line = ed.line[:ed.dot] + text + ed.line[ed.dot:]
	ed.dot += len(text)
}

// acceptHistory accepts currently history.
func (ed *Editor) acceptHistory() {
	ed.line = ed.history.line
	ed.dot = len(ed.line)
}

func setupTerminal(file *os.File) (*sys.Termios, error) {
	fd := int(file.Fd())
	term, err := sys.NewTermiosFromFd(fd)
	if err != nil {
		return nil, fmt.Errorf("can't get terminal attribute: %s", err)
	}

	savedTermios := term.Copy()

	term.SetICanon(false)
	term.SetEcho(false)
	term.SetVMin(1)
	term.SetVTime(0)

	err = term.ApplyToFd(fd)
	if err != nil {
		return nil, fmt.Errorf("can't set up terminal attribute: %s", err)
	}

	/*
		err = sys.FlushInput(fd)
		if err != nil {
			return nil, fmt.Errorf("can't flush input: %s", err)
		}
	*/

	return savedTermios, nil
}

// startReadLine prepares the terminal for the editor.
func (ed *Editor) startReadLine() error {
	savedTermios, err := setupTerminal(ed.file)
	if err != nil {
		return err
	}
	ed.savedTermios = savedTermios

	_, width := sys.GetWinsize(int(ed.file.Fd()))
	// Turn on autowrap, write lackEOL along with enough padding to fill the
	// whole screen. If the cursor was in the first column, we end up in the
	// same line (just off the line boundary); otherwise we are now in the next
	// line. We now rewind to the first column and erase anything there. The
	// final effect is that a lackEOL gets written if and only if the cursor
	// was not in the first column.
	//
	// After that, we turn off autowrap. The editor has its own wrapping
	// mechanism.
	fmt.Fprintf(ed.file, "\033[?7h%s%*s\r \r\033[?7l", lackEOL, width-WcWidth(lackEOLRune), "")

	return nil
}

// finishReadLine puts the terminal in a state suitable for other programs to
// use.
func (ed *Editor) finishReadLine(addError func(error)) {
	ed.mode = modeInsert
	ed.tips = nil
	ed.completion = nil
	ed.navigation = nil
	ed.dot = len(ed.line)
	// TODO Perhaps make it optional to NOT clear the rprompt
	ed.rprompt = ""
	addError(ed.refresh())
	ed.file.WriteString("\n")

	// ed.reader.Stop()
	ed.reader.Quit()

	// turn on autowrap
	ed.file.WriteString("\033[?7h")

	// restore termios
	err := ed.savedTermios.ApplyToFd(int(ed.file.Fd()))

	if err != nil {
		addError(fmt.Errorf("can't restore terminal attribute: %s", err))
	}
	ed.savedTermios = nil
	ed.editorState = editorState{}
}

// ReadLine reads a line interactively.
// TODO(xiaq): ReadLine currently handles SIGINT and SIGWINCH and swallows all
// other signals.
func (ed *Editor) ReadLine(prompt, rprompt func() string) (lr LineRead) {
	ed.editorState = editorState{active: true}
	isExternalCh := make(chan map[string]bool, 1)
	go getIsExternal(ed.evaler, isExternalCh)

	ed.writer.resetOldBuf()
	ones := ed.reader.Chan()
	go ed.reader.Run()

	err := ed.startReadLine()
	if err != nil {
		return LineRead{Err: err}
	}
	defer ed.finishReadLine(func(err error) {
		if err != nil {
			lr.Err = errutil.CatError(lr.Err, err)
		}
	})

MainLoop:
	for {
		ed.prompt = prompt()
		ed.rprompt = rprompt()

		err := ed.refresh()
		if err != nil {
			return LineRead{Err: err}
		}

		ed.tips = nil

		select {
		case m := <-isExternalCh:
			ed.isExternal = m
		case sig := <-ed.sigs:
			// TODO(xiaq): Maybe support customizable handling of signals
			switch sig {
			case syscall.SIGINT:
				// Start over
				ed.editorState = editorState{
					savedTermios: ed.savedTermios,
					isExternal:   ed.isExternal,
				}
				goto MainLoop
			case syscall.SIGWINCH:
				continue MainLoop
			case syscall.SIGCHLD:
				// ignore
			default:
				ed.pushTip(fmt.Sprintf("ignored signal %s", sig))
			}
		case or := <-ones:
			// Alert about error
			err := or.Err
			if err != nil {
				ed.pushTip(err.Error())
				continue
			}

			// Ignore bogus CPR
			if or.CPR != invalidPos {
				continue
			}

			k := or.Key
		lookupKey:
			keyBinding, ok := keyBindings[ed.mode]
			if !ok {
				ed.pushTip("No binding for current mode")
				continue
			}

			fn, bound := keyBinding[k]
			if !bound {
				fn = keyBinding[DefaultBinding]
			}

			ed.lastKey = k
			fn.Call(ed)
			act := ed.nextAction
			ed.nextAction = action{}

			switch act.actionType {
			case noAction:
				continue
			case reprocessKey:
				err = ed.refresh()
				if err != nil {
					return LineRead{Err: err}
				}
				goto lookupKey
			case exitReadLine:
				lr = act.returnValue
				if lr.EOF == false && lr.Err == nil && lr.Line != "" {
					ed.appendHistory(lr.Line)
				}

				return lr
			}
		}
	}
}

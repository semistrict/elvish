package edit

import (
	"os"

	"github.com/elves/elvish/edit/highlight"
	"github.com/elves/elvish/eval"
	"github.com/elves/elvish/parse"
	"github.com/elves/elvish/util"
)

func doHighlight(n parse.Node, ed *Editor) {
	s := &highlight.Emitter{
		func(s string) bool { return goodFormHead(s, ed) },
		ed.styling.Add,
	}
	s.EmitAll(n)
}

func goodFormHead(head string, ed *Editor) bool {
	if eval.IsBuiltinSpecial[head] {
		return true
	} else if util.DontSearch(head) {
		// XXX don't stat twice
		return util.IsExecutable(head) || isDir(head)
	} else {
		ev := ed.evaler
		explode, ns, name := eval.ParseVariable(head)
		if !explode {
			switch ns {
			case "":
				if ev.Builtin.Names[eval.FnPrefix+name] != nil || ev.Global.Names[eval.FnPrefix+name] != nil {
					return true
				}
			case "e":
				if ed.isExternal[name] {
					return true
				}
			default:
				mod := ev.Global.Uses[ns]
				if mod == nil {
					mod = ev.Builtin.Uses[ns]
				}
				if mod != nil && mod[eval.FnPrefix+name] != nil {
					return true
				}
			}
		}
		return ed.isExternal[head]
	}
}

func isDir(fname string) bool {
	stat, err := os.Stat(fname)
	return err == nil && stat.IsDir()
}

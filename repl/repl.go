package repl

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"

	gosig "os/signal"

	gm "github.com/mitchellh/go-mruby"

	"github.com/pensando/box/builder/command"
	"github.com/pensando/box/builder/evaluator"
	"github.com/pensando/box/builder/evaluator/mruby"
	"github.com/pensando/box/builder/executor/docker"
	"github.com/pensando/box/logger"
	"github.com/pensando/box/signal"
	"github.com/pensando/box/types"
	"github.com/chzyer/readline"
	"github.com/moby/term"
	"github.com/fatih/color"
)

const (
	normalPrompt    = "box> "
	multilinePrompt = "box*> "
)

// Repl encapsulates a series of items used to create a read-evaluate-print
// loop so that end users can manually enter build instructions.
type Repl struct {
	readline  *readline.Instance
	evaluator evaluator.Evaluator
	globals   *types.Global
	vars      map[string]string
}

// NewRepl contypes a new Repl.
func NewRepl(omit []string, log *logger.Logger, vars map[string]string) (*Repl, error) {
	rl, err := readline.New(normalPrompt)
	if err != nil {
		return nil, err
	}

	signal.Handler.Exit = false
	signal.Handler.IgnoreRunners = true
	ctx, cancel := context.WithCancel(context.Background())
	globals := &types.Global{
		OmitFuncs: omit,
		TTY:       term.IsTerminal(1),
		Color:     true,
		Cache:     false,
		ShowRun:   true,
		Logger:    log,
		Context:   ctx,
	}

	color.NoColor = false // force color on

	exec, err := docker.NewDocker(globals)
	if err != nil {
		cancel()
		rl.Close()
		return nil, err
	}

	e, err := mruby.NewMRuby(&mruby.Config{
		Filename: "repl",
		Globals:  globals,
		Interp:   command.NewInterpreter(globals, exec, vars),
		Exec:     exec,
	})
	if err != nil {
		cancel()
		rl.Close()
		return nil, err
	}

	signal.Handler.AddFunc(cancel)

	return &Repl{readline: rl, evaluator: e, globals: globals, vars: vars}, nil
}

func (r *Repl) handleError(line string, err error) bool {
	if err == io.EOF {
		os.Exit(0)
	}

	if _, interrupted := err.(*readline.InterruptError); interrupted || err.Error() == "Interrupt" {
		if line != "" {
			r.readline.SetPrompt(normalPrompt)
		} else {
			fmt.Println("You can press ^D or type \"quit\" or \"exit\" to exit the shell")
		}

		return true
	} else if err != nil {
		fmt.Printf("+++ Error %#v\n", err)
		os.Exit(1)
	}
	return false
}

// Loop runs the loop. Returns nil on io.EOF, otherwise errors are forwarded.
func (r *Repl) Loop() {
	defer func() {
		if err := recover(); err != nil {
			fmt.Printf("Aborting due to interpreter error: %v\n", err)
			os.Exit(2)
		}
		r.readline.Close()
	}()

	signals := make(chan os.Signal, 2)
	// in no-tty mode, a literal ^C would be sent directly to the signal handler
	// and not the readline reader, causing a bug where the repl would get stuck.
	// So we install a signal handler just to trap interrupt.
	if !r.globals.TTY {
		gosig.Notify(signals, syscall.SIGINT)
		defer gosig.Stop(signals)
	}

	printHelp()

	lineChan := make(chan string, 1)
	errChan := make(chan error, 1)
	syncChan := make(chan struct{})

	go func() {
		for {
			tmp, err := r.readline.Readline()
			if err != nil {
				errChan <- err
			} else {
				lineChan <- tmp
			}
			<-syncChan
		}
	}()

	r.doLoop(lineChan, errChan, signals, syncChan)
}

func printHelp() {
	fmt.Println(`
Thanks for trying box! We don't have on-line help yet :(
If you want, try our documentation here: https://box-builder.github.io/box

* If you ever need to reset your repl, type "reset".
* If you need to cancel a ruby statement, press Control+C.
		`)
}

func (r *Repl) checkQuit(line string) (bool, error) {
	switch strings.TrimSpace(line) {
	case "quit":
		fallthrough
	case "exit":
		os.Exit(0)
	case "help":
		printHelp()
		return true, nil
	case "reset":
		exec, err := docker.NewDocker(r.globals)
		if err != nil {
			return false, err
		}

		e, err := mruby.NewMRuby(&mruby.Config{
			Filename: "repl",
			Globals:  r.globals,
			Interp:   command.NewInterpreter(r.globals, exec, r.vars),
			Exec:     exec,
		})
		if err != nil {
			return false, err
		}

		r.evaluator = e
		return true, nil
	}

	return false, nil
}

func (r *Repl) readChannels(line string, lineChan <-chan string, errChan <-chan error, signals <-chan os.Signal) (string, bool) {
	var (
		tmp string
		err error
	)

	select {
	case err = <-errChan:
		if r.handleError(line, err) {
			return "", true
		}
	case <-signals:
		fmt.Println("Statement canceled.")

		select {
		case err := <-errChan:
			r.handleError(line, err) // the return value isn't necessary here.
		default:
		}

		return "", true
	case tmp = <-lineChan:
	}

	return line + tmp + "\n", false
}

func (r *Repl) doLoop(lineChan <-chan string, errChan <-chan error, signals <-chan os.Signal, syncChan chan struct{}) {
	var (
		line      string
		stackKeep int
		cancel    context.CancelFunc
		cont      bool
	)

	for {
		if cancel != nil {
			cancel()
		}

		r.globals.Context, cancel = context.WithCancel(context.Background())
		signal.Handler.AddFunc(cancel)

		line, cont = r.readChannels(line, lineChan, errChan, signals)

		if cont {
			syncChan <- struct{}{}
			continue
		}

		if skip, err := r.checkQuit(line); err != nil {
			fmt.Printf("+++ Error: %v\n", err)
			os.Exit(1)
		} else if skip {
			line = ""
			syncChan <- struct{}{}
			continue
		}

		newKeep, err := r.evaluator.RunCode(line, stackKeep, false)
		if err != nil {
			switch err.(type) {
			case *gm.ParserError:
				if newKeep == stackKeep {
					r.readline.SetPrompt(multilinePrompt)
					syncChan <- struct{}{}
					continue
				}
			}
		}

		stackKeep = newKeep
		line = ""

		r.readline.SetPrompt(normalPrompt)
		if err != nil {
			fmt.Printf("+++ Error: %v\n", err)
			syncChan <- struct{}{}
			continue
		}

		if r.evaluator.Result().Value != "" {
			r.globals.Logger.EvalResponse(r.evaluator.Result().Value)
		} else {
			r.globals.Logger.EvalResponse("Executed!")
		}

		syncChan <- struct{}{}
	}
}

package os

import (
	"fmt"
	"io"
	realos "os"
	pathlib "path"
	"strings"

	log "github.com/sirupsen/logrus"
	"github.com/spf13/afero"
	"golang.org/x/crypto/ssh/terminal"
)

var (
	funcMap = make(map[string]Command)
)

type Shell struct {
	log        *log.Entry
	termSignal chan<- int
	terminal   *terminal.Terminal
	sys        *System
}

func NewShell(iostream io.ReadWriter, fsys afero.Fs, width, height int, user, ipSrc string, log *log.Entry, termSignal chan<- int) *Shell {
	sys := &System{
		io:      iostream,
		FSys:    fsys,
		cwd:     usernameMapping[user].Homedir,
		envVars: map[string]string{},
		Width:   width,
		Height:  height,
	}
	aferoFs := afero.Afero{Fs: fsys}
	if exists, _ := aferoFs.DirExists(usernameMapping[user].Homedir); !exists {
		aferoFs.MkdirAll(usernameMapping[user].Homedir, 0644)
	}
	return &Shell{
		log:        log,
		termSignal: termSignal,
		sys:        sys,
	}
}

func (sh *Shell) HandleRequest() {
	sh.terminal = terminal.NewTerminal(sh.sys.IOStream(), "$ ")
	defer func() {
		if r := recover(); r != nil {
			sh.log.Errorf("Recovered from crash %v, killing connection", r)
			sh.termSignal <- 1
		}
	}()
cmdLoop:
	for {
		cmd, err := sh.terminal.ReadLine()
		sh.log.WithField("cmd", cmd).Infof("User input command %v", cmd)
		cmd = strings.TrimSpace(cmd)
		switch {
		case err != nil:
			if err.Error() == "EOF" {
				sh.log.Info("EOF received from client")
				sh.termSignal <- 0
				return
			} else {
				sh.log.WithError(err).Error("Error when reading terminal")
			}
			break cmdLoop
		case strings.TrimSpace(cmd) == "":
			//Do nothing
		case cmd == "logout", cmd == "quit":
			sh.log.Infof("User logged out")
			sh.termSignal <- 0
			return
		case strings.HasPrefix(cmd, "cd"):
			args := strings.Split(cmd, " ")
			if len(args) > 1 {
				err := sh.sys.Chdir(args[1])
				if err != nil {
					sh.terminal.Write([]byte(fmt.Sprintf("-bash: cd: %v: No such file or directory\n", args[1])))
				}
			}
		case strings.HasPrefix(cmd, "export"):

		default:
			// Start parsing script

			args := strings.SplitN(cmd, " ", 2)
			n, err := sh.Exec(args[0], args[1:])
			if err != nil {
				sh.terminal.Write([]byte(fmt.Sprintf("%v: command not found\n", args[0])))
			} else {
				sh.sys.envVars["?"] = string(n)
			}
		}
	}
}

func (sh *Shell) input(line string) error {
	switch {
	case strings.HasPrefix(line, "cd "):
		err := sh.sys.Chdir(line[3:])
		if err != nil {
			return err
		}
	}
	return nil
}

func (sh *Shell) Exec(path string, args []string) (int, error) {
	cmd := pathlib.Base(path)
	if execFunc, ok := funcMap[cmd]; ok {
		res := execFunc.Exec(args, sh.sys)
		return res, nil
	}

	return -1, realos.ErrNotExist
}

func (sh *Shell) SetSize(width, height int) error {
	sh.sys.Width = width
	sh.sys.Height = height
	return sh.terminal.SetSize(width, height)
}

// RegisterCommand puts the command implementation into map so
// it can be invoked from command line
func RegisterCommand(name string, cmd Command) {
	funcMap[name] = cmd
}

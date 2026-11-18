package types

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"reflect"
	"runtime"
	"strings"

	v1 "github.com/baragoon/backrest/gen/go/v1"
	"github.com/baragoon/backrest/internal/hook/hookutil"
	"github.com/baragoon/backrest/internal/ioutil"
	"github.com/baragoon/backrest/internal/orchestrator/logging"
	"github.com/baragoon/backrest/internal/orchestrator/tasks"
	"github.com/baragoon/backrest/internal/platformutil"
	"github.com/kballard/go-shellquote"
)

// stripShebangComment removes trailing comments from a shebang line.
// A comment is # that appears at a word boundary (after whitespace or at start),
// but not inside quoted arguments. This preserves shebangs with quoted arguments
// that may contain # characters.
func stripShebangComment(shebang string) string {
	escaped := false
	doubleQuoted := false
	singleQuoted := false
	lastCharWasWhitespace := true
	
	for i, ch := range shebang {
		if escaped {
			escaped = false
			lastCharWasWhitespace = false
			continue
		}
		
		if ch == '\\' && !singleQuoted {
			escaped = true
			lastCharWasWhitespace = false
			continue
		}
		
		if ch == '"' && !singleQuoted {
			doubleQuoted = !doubleQuoted
			lastCharWasWhitespace = false
			continue
		}
		
		if ch == '\'' && !doubleQuoted {
			singleQuoted = !singleQuoted
			lastCharWasWhitespace = false
			continue
		}
		
		// Comment: unquoted # at word boundary (start or after whitespace)
		if ch == '#' && !doubleQuoted && !singleQuoted && lastCharWasWhitespace {
			return shebang[:i]
		}
		
		isWhitespace := !doubleQuoted && !singleQuoted && (ch == ' ' || ch == '\t')
		if isWhitespace {
			lastCharWasWhitespace = true
		} else {
			lastCharWasWhitespace = false
		}
	}
	
	return shebang
}

type commandHandler struct{}

func (commandHandler) Name() string {
	return "command"
}

func (commandHandler) Execute(ctx context.Context, h *v1.Hook, vars interface{}, runner tasks.TaskRunner, event v1.Hook_Condition) error {
	command, err := hookutil.RenderTemplate(h.GetActionCommand().GetCommand(), vars)
	if err != nil {
		return fmt.Errorf("template rendering: %w", err)
	}

	writer := logging.WriterFromContext(ctx)

	// Parse out the shell to use if a #! prefix is present
	shell := []string{"sh"}
	if runtime.GOOS == "windows" {
		shell = []string{"powershell", "-NoLogo", "-NoProfile", "-Command", "-"}
	}

	if len(command) > 2 && command[0:2] == "#!" {
		nextLine := strings.Index(command, "\n")
		if nextLine == -1 {
			nextLine = len(command)
		}
		shebang := strings.TrimRight(command[2:nextLine], " \r")
		
		// Strip trailing comments from shebang: recognize # only at word boundary
		shebang = stripShebangComment(shebang)
		
		shell, err = shellquote.Split(shebang)
		if err != nil {
			return fmt.Errorf("parsing shell for command: %w", err)
		} else if len(shell) == 0 {
			return errors.New("must specify shell for command")
		}
		command = command[nextLine+1:]
	}

	scriptWriter := &ioutil.LinePrefixer{W: writer, Prefix: []byte("[script] ")}
	fmt.Fprintf(scriptWriter, "%v\n%v\n", shell, command)
	scriptWriter.Close()
	outputWriter := &ioutil.LinePrefixer{W: writer, Prefix: []byte("[output] ")}
	defer outputWriter.Close()

	// Run the command in the specified shell
	execCmd := exec.Command(shell[0], shell[1:]...)
	platformutil.SetPlatformOptions(execCmd)
	execCmd.Stdin = strings.NewReader(command)

	stdout := &ioutil.SynchronizedWriter{W: outputWriter}
	execCmd.Stderr = stdout
	execCmd.Stdout = stdout

	return execCmd.Run()
}

func (commandHandler) ActionType() reflect.Type {
	return reflect.TypeOf(&v1.Hook_ActionCommand{})
}

func init() {
	DefaultRegistry().RegisterHandler(&commandHandler{})
}

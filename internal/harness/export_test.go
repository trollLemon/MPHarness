package harness

import (
	"bufio"
	"io"
)

var OutputRunDirCommand = outputRunDirCommand
var WriteCommandHistory = writeCommandHistory
var ErrInvalidInput = errInvalidInput
var ErrVMShouldNotBeUsed = errVMShouldNotBeUsed
var ErrPromptRead = errPromptRead

func PromptExistingVM(r *bufio.Reader, w io.Writer, name string) error {
	return promptExistingVM(r, w, name)
}

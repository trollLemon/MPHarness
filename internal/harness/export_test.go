package harness

import (
	"bufio"
	"io"
)

var WriteCommandHistory = writeCommandHistory
var ConfirmExistingVM = confirmExistingVM
var ErrVMShouldNotBeUsed = errVMShouldNotBeUsed
var ErrPromptRead = errPromptRead
var ErrPromptAttemptsExceeded = errPromptAttemptsExceeded

const (
	PromptAccept = promptAccept
	PromptReject = promptReject
	PromptRetry  = promptRetry
)

func PromptExistingVM(r *bufio.Reader, w io.Writer, name string) (promptDecision, error) {
	return promptExistingVM(r, w, name)
}

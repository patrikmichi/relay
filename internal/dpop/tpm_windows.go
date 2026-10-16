//go:build windows

package dpop

import (
	"errors"
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	tbs                  = windows.NewLazySystemDLL("tbs.dll")
	procContextCreate    = tbs.NewProc("Tbsi_Context_Create")
	procSubmitCommand    = tbs.NewProc("Tbsip_Submit_Command")
	procContextClose     = tbs.NewProc("Tbsip_Context_Close")
	tbsContextVersionTwo = uint32(2)
	tbsIncludeTPM20      = uint32(4)
	tbsPriorityNormal    = uintptr(200)
)

type tbsTransport struct{ ctx uintptr }

func openTPM() (tpmTransport, error) {
	if err := tbs.Load(); err != nil {
		return nil, fmt.Errorf("load tbs.dll: %w", err)
	}
	params := struct{ version, flags uint32 }{tbsContextVersionTwo, tbsIncludeTPM20}
	var ctx uintptr
	if r, _, _ := procContextCreate.Call(uintptr(unsafe.Pointer(&params)), uintptr(unsafe.Pointer(&ctx))); r != 0 {
		return nil, fmt.Errorf("open a TPM context: TBS result 0x%x", r)
	}
	return &tbsTransport{ctx: ctx}, nil
}

func (t *tbsTransport) transmit(command []byte) ([]byte, error) {
	resp := make([]byte, 4096)
	size := uint32(len(resp))
	r, _, _ := procSubmitCommand.Call(t.ctx, 0, tbsPriorityNormal,
		uintptr(unsafe.Pointer(&command[0])), uintptr(len(command)),
		uintptr(unsafe.Pointer(&resp[0])), uintptr(unsafe.Pointer(&size)))
	if r != 0 {
		return nil, fmt.Errorf("send a command to the TPM: TBS result 0x%x", r)
	}
	if int(size) > len(resp) {
		return nil, errors.New("the TPM answered with more than it reported room for")
	}
	return resp[:size], nil
}

func (t *tbsTransport) close() error {
	if r, _, _ := procContextClose.Call(t.ctx); r != 0 {
		return fmt.Errorf("close the TPM context: TBS result 0x%x", r)
	}
	return nil
}

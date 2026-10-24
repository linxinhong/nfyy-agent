//go:build windows

package cli

// Windows Credential Manager implementation. It deliberately uses the native
// CredRead/CredWrite/CredDelete APIs instead of command-line helpers: the CLI
// must never fall back to a plaintext file or a Linux secret-tool invocation
// when built for Windows.

import (
	"fmt"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	credTypeGeneric         = 1
	credPersistLocalMachine = 2
)

type winCredential struct {
	Flags              uint32
	Type               uint32
	TargetName         *uint16
	Comment            *uint16
	LastWritten        windows.Filetime
	CredentialBlobSize uint32
	CredentialBlob     *byte
	Persist            uint32
	AttributeCount     uint32
	Attributes         uintptr
	TargetAlias        *uint16
	UserName           *uint16
}

var (
	advapi32       = windows.NewLazySystemDLL("advapi32.dll")
	procCredRead   = advapi32.NewProc("CredReadW")
	procCredWrite  = advapi32.NewProc("CredWriteW")
	procCredDelete = advapi32.NewProc("CredDeleteW")
	procCredFree   = advapi32.NewProc("CredFree")
)

func NewOSKeyring() KeyringStore { return &windowsKeyring{} }

type windowsKeyring struct{}

func (k *windowsKeyring) Get(service, user string) (string, error) {
	target, err := windows.UTF16PtrFromString(service + ":" + user)
	if err != nil {
		return "", ErrNotFound
	}
	var credential *winCredential
	r1, _, callErr := procCredRead.Call(uintptr(unsafe.Pointer(target)), credTypeGeneric, 0, uintptr(unsafe.Pointer(&credential)))
	if r1 == 0 {
		if callErr == syscall.Errno(1168) {
			return "", ErrNotFound
		}
		return "", fmt.Errorf("windows credential read failed: %w", callErr)
	}
	defer procCredFree.Call(uintptr(unsafe.Pointer(credential)))
	if credential.CredentialBlob == nil || credential.CredentialBlobSize == 0 {
		return "", ErrNotFound
	}
	bytes := unsafe.Slice(credential.CredentialBlob, credential.CredentialBlobSize)
	return string(bytes), nil
}

func (k *windowsKeyring) Set(service, user, secret string) error {
	target, err := windows.UTF16PtrFromString(service + ":" + user)
	if err != nil {
		return fmt.Errorf("windows credential target: %w", err)
	}
	username, err := windows.UTF16PtrFromString(user)
	if err != nil {
		return fmt.Errorf("windows credential user: %w", err)
	}
	bytes := []byte(secret)
	credential := winCredential{Type: credTypeGeneric, TargetName: target, UserName: username, Persist: credPersistLocalMachine, CredentialBlobSize: uint32(len(bytes))}
	if len(bytes) > 0 {
		credential.CredentialBlob = &bytes[0]
	}
	r1, _, callErr := procCredWrite.Call(uintptr(unsafe.Pointer(&credential)), 0)
	if r1 == 0 {
		return fmt.Errorf("windows credential write failed: %v", redact(callErr.Error()))
	}
	return nil
}

func (k *windowsKeyring) Delete(service, user string) error {
	target, err := windows.UTF16PtrFromString(service + ":" + user)
	if err != nil {
		return ErrNotFound
	}
	r1, _, callErr := procCredDelete.Call(uintptr(unsafe.Pointer(target)), credTypeGeneric, 0)
	if r1 == 0 {
		if callErr == syscall.Errno(1168) {
			return ErrNotFound
		}
		return fmt.Errorf("windows credential delete failed: %w", callErr)
	}
	return nil
}

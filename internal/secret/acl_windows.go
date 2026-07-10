//go:build windows

package secret

import (
	"fmt"
	"os"
	"os/user"
	"runtime"
	"syscall"
	"unsafe"
)

const (
	daclSecurityInformation          = 0x00000004
	protectedDACLSecurityInformation = 0x80000000
)

const (
	securityDescriptorDACLProtected = 0x1000
	accessAllowedACEType            = 0
	objectInheritACE                = 0x1
	containerInheritACE             = 0x2
	fileAllAccess                   = 0x001f01ff
)

var (
	advapi32                                = syscall.NewLazyDLL("advapi32.dll")
	procConvertStringSecurityDescriptorToSD = advapi32.NewProc("ConvertStringSecurityDescriptorToSecurityDescriptorW")
	procSetFileSecurity                     = advapi32.NewProc("SetFileSecurityW")
	procGetFileSecurity                     = advapi32.NewProc("GetFileSecurityW")
	procGetSecurityDescriptorControl        = advapi32.NewProc("GetSecurityDescriptorControl")
	procGetSecurityDescriptorDACL           = advapi32.NewProc("GetSecurityDescriptorDacl")
	procGetACE                              = advapi32.NewProc("GetAce")
	procConvertStringSIDToSID               = advapi32.NewProc("ConvertStringSidToSidW")
	procEqualSID                            = advapi32.NewProc("EqualSid")
)

type WindowsAccessController struct{}

func NewAccessController() WindowsAccessController { return WindowsAccessController{} }

func (WindowsAccessController) Harden(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("inspect ACL target: %w", err)
	}
	current, err := user.Current()
	if err != nil {
		return fmt.Errorf("resolve current Windows identity: %w", err)
	}
	if current.Uid == "" {
		return fmt.Errorf("current Windows identity has no SID")
	}
	inheritance := ""
	if info.IsDir() {
		inheritance = "OICI"
	}
	sddl := fmt.Sprintf("D:P(A;%s;FA;;;SY)(A;%s;FA;;;%s)", inheritance, inheritance, current.Uid)
	if err := setFileDACL(path, sddl); err != nil {
		return err
	}
	return verifyFileDACL(path, current.Uid, info.IsDir())
}

func (WindowsAccessController) Verify(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("inspect ACL target: %w", err)
	}
	current, err := user.Current()
	if err != nil {
		return fmt.Errorf("resolve current Windows identity: %w", err)
	}
	return verifyFileDACL(path, current.Uid, info.IsDir())
}

func setFileDACL(path, sddl string) error {
	sddlPointer, err := syscall.UTF16PtrFromString(sddl)
	if err != nil {
		return fmt.Errorf("encode ACL descriptor: %w", err)
	}
	var descriptor uintptr
	result, _, callErr := procConvertStringSecurityDescriptorToSD.Call(
		uintptr(unsafe.Pointer(sddlPointer)),
		1,
		uintptr(unsafe.Pointer(&descriptor)),
		0,
	)
	if result == 0 {
		return fmt.Errorf("convert ACL descriptor: %w", callError(callErr))
	}
	defer procLocalFree.Call(descriptor)
	pathPointer, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return fmt.Errorf("encode ACL path: %w", err)
	}
	result, _, callErr = procSetFileSecurity.Call(
		uintptr(unsafe.Pointer(pathPointer)),
		daclSecurityInformation|protectedDACLSecurityInformation,
		descriptor,
	)
	if result == 0 {
		return fmt.Errorf("set ACL: %w", callError(callErr))
	}
	return nil
}

type windowsACL struct {
	Revision byte
	Padding  byte
	Size     uint16
	ACECount uint16
	Padding2 uint16
}

type windowsACEHeader struct {
	Type  byte
	Flags byte
	Size  uint16
}

type windowsAccessAllowedACE struct {
	Header   windowsACEHeader
	Mask     uint32
	SIDStart uint32
}

func verifyFileDACL(path, currentSID string, directory bool) error {
	descriptor, err := readFileSecurityDescriptor(path)
	if err != nil {
		return err
	}
	var control uint16
	var revision uint32
	result, _, callErr := procGetSecurityDescriptorControl.Call(
		uintptr(unsafe.Pointer(&descriptor[0])),
		uintptr(unsafe.Pointer(&control)),
		uintptr(unsafe.Pointer(&revision)),
	)
	if result == 0 {
		return fmt.Errorf("read security descriptor control: %w", callError(callErr))
	}
	if control&securityDescriptorDACLProtected == 0 {
		return fmt.Errorf("ACL inheritance is not protected")
	}

	var present, defaulted int32
	var aclPointer unsafe.Pointer
	result, _, callErr = procGetSecurityDescriptorDACL.Call(
		uintptr(unsafe.Pointer(&descriptor[0])),
		uintptr(unsafe.Pointer(&present)),
		uintptr(unsafe.Pointer(&aclPointer)),
		uintptr(unsafe.Pointer(&defaulted)),
	)
	if result == 0 {
		return fmt.Errorf("read security descriptor DACL: %w", callError(callErr))
	}
	if present == 0 || aclPointer == nil {
		return fmt.Errorf("protected ACL has no DACL")
	}
	acl := (*windowsACL)(aclPointer)
	if acl.ACECount != 2 {
		return fmt.Errorf("ACL has %d entries; expected exactly current user and SYSTEM", acl.ACECount)
	}
	currentPointer, err := sidFromString(currentSID)
	if err != nil {
		return err
	}
	defer procLocalFree.Call(uintptr(currentPointer))
	systemPointer, err := sidFromString("S-1-5-18")
	if err != nil {
		return err
	}
	defer procLocalFree.Call(uintptr(systemPointer))

	expectedFlags := byte(0)
	if directory {
		expectedFlags = objectInheritACE | containerInheritACE
	}
	currentCount, systemCount := 0, 0
	for index := uint16(0); index < acl.ACECount; index++ {
		var acePointer unsafe.Pointer
		result, _, callErr = procGetACE.Call(uintptr(aclPointer), uintptr(index), uintptr(unsafe.Pointer(&acePointer)))
		if result == 0 {
			return fmt.Errorf("read ACL entry %d: %w", index, callError(callErr))
		}
		ace := (*windowsAccessAllowedACE)(acePointer)
		if ace.Header.Type != accessAllowedACEType || ace.Header.Flags != expectedFlags || ace.Mask != fileAllAccess {
			return fmt.Errorf("ACL entry %d is not the expected inheritable full-control allow entry", index)
		}
		sidPointer := unsafe.Add(acePointer, unsafe.Offsetof(windowsAccessAllowedACE{}.SIDStart))
		switch {
		case equalSID(sidPointer, currentPointer):
			currentCount++
		case equalSID(sidPointer, systemPointer):
			systemCount++
		default:
			return fmt.Errorf("ACL entry %d grants an unexpected principal", index)
		}
	}
	if currentCount != 1 || systemCount != 1 {
		return fmt.Errorf("ACL must grant exactly one entry each to current user and SYSTEM")
	}
	runtime.KeepAlive(descriptor)
	return nil
}

func readFileSecurityDescriptor(path string) ([]byte, error) {
	pointer, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	var required uint32
	procGetFileSecurity.Call(
		uintptr(unsafe.Pointer(pointer)),
		daclSecurityInformation,
		0,
		0,
		uintptr(unsafe.Pointer(&required)),
	)
	if required == 0 {
		return nil, fmt.Errorf("query security descriptor size failed")
	}
	descriptor := make([]byte, required)
	result, _, callErr := procGetFileSecurity.Call(
		uintptr(unsafe.Pointer(pointer)),
		daclSecurityInformation,
		uintptr(unsafe.Pointer(&descriptor[0])),
		uintptr(required),
		uintptr(unsafe.Pointer(&required)),
	)
	if result == 0 {
		return nil, fmt.Errorf("read file security descriptor: %w", callError(callErr))
	}
	return descriptor, nil
}

func sidFromString(value string) (unsafe.Pointer, error) {
	pointer, err := syscall.UTF16PtrFromString(value)
	if err != nil {
		return nil, err
	}
	var sid unsafe.Pointer
	result, _, callErr := procConvertStringSIDToSID.Call(
		uintptr(unsafe.Pointer(pointer)),
		uintptr(unsafe.Pointer(&sid)),
	)
	if result == 0 {
		return nil, fmt.Errorf("convert SID %q: %w", value, callError(callErr))
	}
	return sid, nil
}

func equalSID(left, right unsafe.Pointer) bool {
	result, _, _ := procEqualSID.Call(uintptr(left), uintptr(right))
	return result != 0
}

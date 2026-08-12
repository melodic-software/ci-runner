//go:build windows

package statefs

import "golang.org/x/sys/windows"

// blockObservedOpen holds an exclusive read handle so a concurrent os.Open
// surfaces the same sharing-violation class of failure seen under concurrent
// state-directory writes on Windows hosts.
func blockObservedOpen(path string) (func(), error) {
	pathUTF16, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(
		pathUTF16,
		windows.GENERIC_READ,
		0,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		return nil, err
	}
	return func() { _ = windows.CloseHandle(handle) }, nil
}

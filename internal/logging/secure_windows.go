//go:build windows

package logging

import (
	"errors"
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// dataDirSDDL: owner Administrators; protected DACL (no inheritance from
// %ProgramData%) granting SYSTEM and Administrators full control and Users
// read/execute only (0x1200a9), inherited by files and subdirectories.
const dataDirSDDL = `O:BAD:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;0x1200a9;;;BU)`

// SecureDataDir creates dir if missing and gives it owner Administrators
// and a protected DACL, so non-admin users cannot create or replace files
// in it (no planted links, hard links or config.json). It refuses a dir
// that is a reparse point. Callers must fail closed on error.
func SecureDataDir(dir string) error {
	sd, err := windows.SecurityDescriptorFromString(dataDirSDDL)
	if err != nil {
		return fmt.Errorf("data dir: parse SDDL: %w", err)
	}
	owner, _, err := sd.Owner()
	if err != nil {
		return fmt.Errorf("data dir: owner: %w", err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return fmt.Errorf("data dir: DACL: %w", err)
	}
	p, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return err
	}
	// Create it with the final descriptor so there is no window in which
	// it carries %ProgramData%'s "users may create files" ACL.
	sa := &windows.SecurityAttributes{SecurityDescriptor: sd}
	sa.Length = uint32(unsafe.Sizeof(*sa))
	if err := windows.CreateDirectory(p, sa); err != nil && !errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		return fmt.Errorf("data dir: create %s: %w", dir, err)
	}
	// Open the directory itself (not a reparse target) and keep the handle
	// while we work: with share mode READ only, nobody can open it for
	// write or delete meanwhile, so it cannot be swapped for a junction
	// between the check and the security update.
	h, err := windows.CreateFile(p,
		windows.FILE_LIST_DIRECTORY|windows.FILE_READ_ATTRIBUTES|windows.READ_CONTROL|windows.WRITE_DAC|windows.WRITE_OWNER,
		windows.FILE_SHARE_READ, nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return fmt.Errorf("data dir: open %s: %w", dir, err)
	}
	defer windows.CloseHandle(h)
	var fi windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &fi); err != nil {
		return fmt.Errorf("data dir: stat %s: %w", dir, err)
	}
	if fi.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return fmt.Errorf("data dir: %s is a reparse point (link or junction); refusing", dir)
	}
	if fi.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
		return fmt.Errorf("data dir: %s is not a directory", dir)
	}
	const si = windows.OWNER_SECURITY_INFORMATION | windows.DACL_SECURITY_INFORMATION | windows.PROTECTED_DACL_SECURITY_INFORMATION
	// By handle first: from here on the previous owner (if a user
	// pre-created the dir) has lost control of it.
	if err := windows.SetSecurityInfo(h, windows.SE_FILE_OBJECT, si, owner, nil, dacl, nil); err != nil {
		return fmt.Errorf("data dir: set security on %s: %w", dir, err)
	}
	// By name, which also propagates the inheritable ACEs to existing
	// children.
	if err := windows.SetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, si, owner, nil, dacl, nil); err != nil {
		return fmt.Errorf("data dir: set security on %s: %w", dir, err)
	}
	return nil
}

// CheckFile removes path if it exists and is a reparse point (symlink,
// junction, mount point), a directory, or a file with more than one hard
// link, so that SYSTEM never opens, writes or truncates something a user
// redirected. A missing path is not an error; a failed removal is.
func CheckFile(path string) error {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	h, err := windows.CreateFile(p, windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("check %s: %w", path, err)
	}
	var fi windows.ByHandleFileInformation
	err = windows.GetFileInformationByHandle(h, &fi)
	windows.CloseHandle(h)
	if err != nil {
		return fmt.Errorf("check %s: %w", path, err)
	}
	if plainFile(&fi) {
		return nil
	}
	if err := removeEntry(p, fi.FileAttributes); err != nil {
		return fmt.Errorf("check %s: remove unsafe entry: %w", path, err)
	}
	return nil
}

func plainFile(fi *windows.ByHandleFileInformation) bool {
	return fi.FileAttributes&(windows.FILE_ATTRIBUTE_REPARSE_POINT|windows.FILE_ATTRIBUTE_DIRECTORY) == 0 &&
		fi.NumberOfLinks == 1
}

// removeEntry deletes the directory entry itself: DeleteFile and
// RemoveDirectory do not follow links. (os.Remove is avoided because it may
// clear the read-only attribute, which on a hard link would change the
// shared target.)
func removeEntry(p *uint16, attrs uint32) error {
	if attrs&windows.FILE_ATTRIBUTE_DIRECTORY != 0 {
		return windows.RemoveDirectory(p)
	}
	return windows.DeleteFile(p)
}

// openLog opens (creating if needed) path for writing without following a
// reparse point, and checks the open handle: it must be a plain file with
// exactly one link. If not, the entry is closed, removed and recreated once;
// a second failure is an error. FILE_SHARE_DELETE lets rotate rename it.
func openLog(path string) (*os.File, error) {
	if err := CheckFile(path); err != nil {
		return nil, err
	}
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	for attempt := 0; ; attempt++ {
		h, err := windows.CreateFile(p,
			windows.GENERIC_WRITE|windows.FILE_READ_ATTRIBUTES,
			windows.FILE_SHARE_READ|windows.FILE_SHARE_DELETE, nil,
			windows.OPEN_ALWAYS,
			windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
		if err != nil {
			return nil, fmt.Errorf("open %s: %w", path, err)
		}
		var fi windows.ByHandleFileInformation
		if err := windows.GetFileInformationByHandle(h, &fi); err != nil {
			windows.CloseHandle(h)
			return nil, fmt.Errorf("open %s: %w", path, err)
		}
		if plainFile(&fi) {
			return os.NewFile(uintptr(h), path), nil
		}
		windows.CloseHandle(h)
		if attempt > 0 {
			return nil, fmt.Errorf("open %s: not a plain file after recreating it", path)
		}
		if err := removeEntry(p, fi.FileAttributes); err != nil {
			return nil, fmt.Errorf("open %s: remove unsafe entry: %w", path, err)
		}
	}
}

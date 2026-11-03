package ustc

import (
	"os"
	"path/filepath"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

func TestWindowsPrivateFileDACL(t *testing.T) {
	directory := t.TempDir()
	if err := securePrivateDirectory(directory); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "private.json")
	for _, contents := range []string{"initial-secret", "replacement-secret"} {
		if err := WritePrivate(path, []byte(contents)); err != nil {
			t.Fatal(err)
		}
		user, err := windows.GetCurrentProcessToken().GetTokenUser()
		if err != nil {
			t.Fatal(err)
		}
		descriptor, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
		if err != nil {
			t.Fatal(err)
		}
		// Windows may serialize the same SID as a well-known alias and add
		// informational auto-inheritance flags. Verify the effective policy,
		// not its SDDL spelling: a protected DACL with exactly one explicit ACE.
		control, _, err := descriptor.Control()
		if err != nil || control&windows.SE_DACL_PROTECTED == 0 {
			t.Fatal("credential DACL permits inheritance", err)
		}
		acl, _, err := descriptor.DACL()
		if err != nil || acl == nil || acl.AceCount != 1 {
			t.Fatalf("credential DACL is not private: %s", descriptor.String())
		}
		var entry *windows.ACCESS_ALLOWED_ACE
		if err = windows.GetAce(acl, 0, &entry); err != nil {
			t.Fatal(err)
		}
		owner := (*windows.SID)(unsafe.Pointer(&entry.SidStart))
		const fileAllAccess windows.ACCESS_MASK = 0x001f01ff
		if entry.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || entry.Header.AceFlags != 0 || entry.Mask != fileAllAccess || !owner.Equals(user.User.Sid) {
			t.Fatalf("credential ACL grants unexpected access: %s", descriptor.String())
		}
		data, err := os.ReadFile(path)
		if err != nil || string(data) != contents {
			t.Fatal("private file replacement failed", err)
		}
	}
}

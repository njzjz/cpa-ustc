package ustc

import (
	"os"
	"path/filepath"
	"testing"

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
		expected := "D:P(A;;FA;;;" + user.User.Sid.String() + ")"
		if descriptor.String() != expected {
			t.Fatalf("credential DACL is not private: %s", descriptor.String())
		}
		data, err := os.ReadFile(path)
		if err != nil || string(data) != contents {
			t.Fatal("private file replacement failed", err)
		}
	}
}

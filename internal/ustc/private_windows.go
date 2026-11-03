package ustc

import "golang.org/x/sys/windows"

// Windows chmod cannot remove inherited read access. Install a protected DACL
// before writing any secrets; only the process identity receives full access.
func securePrivatePath(path string) error {
	return secureWindowsDACL(path, "")
}

func securePrivateDirectory(path string) error {
	return secureWindowsDACL(path, "OICI")
}

func secureWindowsDACL(path, inheritance string) error {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	descriptor, err := windows.SecurityDescriptorFromString("D:P(A;" + inheritance + ";FA;;;" + user.User.Sid.String() + ")")
	if err != nil {
		return err
	}
	acl, _, err := descriptor.DACL()
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil)
}

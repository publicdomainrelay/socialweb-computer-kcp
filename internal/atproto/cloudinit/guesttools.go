package cloudinit

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
)

const GuestToolsDir = AssociationRefDir + "/guest-tools"

var guestToolNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

func GuestToolPath(name string) string {
	return GuestToolsDir + "/" + name
}

func GuestToolOf(path string) (string, bool) {
	name, ok := strings.CutPrefix(path, GuestToolsDir+"/")
	if !ok || !guestToolNameRE.MatchString(name) {
		return "", false
	}
	return name, true
}

func GuestToolsModule(names []string) UserDataModule {
	return func(Context) (UserDataPatch, error) {
		var files []WriteFileEntry
		for _, name := range slices.Sorted(slices.Values(names)) {
			if !guestToolNameRE.MatchString(name) {
				return UserDataPatch{}, fmt.Errorf("cloudinit guest tools: %q is not a tool name", name)
			}
			files = append(files, WriteFileEntry{Path: GuestToolPath(name), Owner: "root:root", Permissions: "0644", Content: name + "\n"})
		}
		return UserDataPatch{WriteFiles: slices.CompactFunc(files, func(a, b WriteFileEntry) bool { return a.Path == b.Path })}, nil
	}
}

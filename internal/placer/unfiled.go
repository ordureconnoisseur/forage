package placer

import (
	"os"
	"path/filepath"
)

// Where a file goes when forage cannot attribute it to anyone.
//
// The name is "Unfiled" because filing is the verb forage uses everywhere
// else: the API route is /unfiled, the library query is FindUnfiledScenes,
// the UI view is Unfiled. "Unsorted" was the odd word out, and a user reading
// "Unfiled" in the interface should not have to learn that it means the
// folder called something else on disk.
const UnfiledFolder = "Unfiled"

// LegacyUnfiledFolder is the name earlier versions wrote, and it is still the
// live one on any library that has been running for a while. On the reference
// library it holds 4,884 files, 2.12 TB, and ten torrents seed from paths
// inside it.
const LegacyUnfiledFolder = "Unsorted"

// UnfiledFolders is every name that counts as the fallback bin, for the code
// that has to RECOGNISE one rather than pick one. Both are always accepted:
// a library never stops being readable because the preferred spelling moved.
func UnfiledFolders() []string { return []string{UnfiledFolder, LegacyUnfiledFolder} }

// unfiledDir picks the fallback folder to WRITE into: the legacy one when the
// library already has it, otherwise the current name.
//
// This is what makes the rename safe without a migration. Switching the
// constant alone would be worse than either name, because new unattributed
// files would start landing in <root>/Unfiled while thousands of existing
// ones stayed in <root>/Unsorted, splitting the one bin whose entire purpose
// is to be the single place to look. An established library keeps its folder
// until someone deliberately renames it; a fresh one gets the better name and
// never sees the old one.
func (p *Placer) unfiledDir() string {
	if p.libraryRoot == "" {
		return UnfiledFolder
	}
	if fi, err := os.Stat(filepath.Join(p.libraryRoot, LegacyUnfiledFolder)); err == nil && fi.IsDir() {
		return LegacyUnfiledFolder
	}
	return UnfiledFolder
}

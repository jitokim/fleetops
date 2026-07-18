// Package fsatomic writes a file so a reader never observes it half-written.
//
// Extracted because internal/sessions and internal/hidden had grown
// byte-identical copies of the same temp-file + rename dance (sessions'
// copy even documented itself as "mirroring hidden.write"). Both persist
// small JSON records under ~/.fleetops that are rewritten in place and then
// read back by a separate process, so both need the same guarantee and neither
// owns it more than the other: it belongs beside them, not inside one of them.
//
// The guarantee is rename(2)'s atomicity on a single filesystem, which the
// SIBLING temp file (created in the destination's own directory, never
// os.TempDir) is what makes safe -- a cross-device rename would fall back to a
// copy and lose exactly the property we came for. There is deliberately no
// fsync: this trades durability across a machine crash (a record may be lost)
// for integrity (a record is never torn), which is the right trade for
// best-effort registries that fail open and are rebuilt on the next scan.
package fsatomic

import (
	"os"
	"path/filepath"
)

// dirPerm is the mode for the ~/.fleetops tree WriteFile creates on demand.
// Owned here so the two registries that share this writer cannot drift apart
// on it.
const dirPerm = 0o755

// WriteFile writes data to path atomically, creating path's parent directory
// if it does not exist. tmpPrefix names the sibling temp file (an
// os.CreateTemp pattern, e.g. ".hidden-*.tmp") so a stray temp left by a
// hard kill is traceable to the registry that made it.
//
// On any error before the rename the temp file is removed, so a failed write
// leaves no ".tmp" litter behind. On success path is either the old bytes or
// the new bytes -- never a mix.
func WriteFile(path string, data []byte, tmpPrefix string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, tmpPrefix)
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}

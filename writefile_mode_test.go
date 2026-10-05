/*
 * Copyright (C) 2026 Franklin D. Amador
 *
 * This software is dual-licensed under the terms of the GPL v3.0 and
 * a commercial license. You may choose to use this software under either
 * license.
 *
 * See the LICENSE files in the project root for full license text.
 */

package honeycomb

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestWriteTagsToFileOwnerOnly: the file of retained values is readable and
// writable by its owner only, since ReadTagsFromFile restores it at start.
func TestWriteTagsToFileOwnerOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows files take their access from the directory's ACL")
	}
	path := filepath.Join(t.TempDir(), "retain.txt")
	if err := NewTagDatabase().WriteTagsToFile(path); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode&0o077 != 0 {
		t.Fatalf("mode %v: others may read or write retained values", mode)
	}
}

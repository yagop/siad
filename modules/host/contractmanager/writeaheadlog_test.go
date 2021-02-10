package contractmanager

import (
	"io/ioutil"
	"os"
	"path/filepath"
	"testing"

	"gitlab.com/NebulousLabs/Sia/build"
	"gitlab.com/NebulousLabs/Sia/modules"
	"gitlab.com/NebulousLabs/Sia/persist"
)

// TestLoadWAL tests loading an existing wal.
func TestLoadWAL(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
	}

	// Load legacy wal.
	wal, err := ioutil.ReadFile("../../../persist/testdata/154hostwal.wal")
	if err != nil {
		t.Fatal(err)
	}

	// Prepare a test dir.
	testdir := build.TempDir(modules.ContractManagerDir, t.Name())
	err = os.MkdirAll(testdir, persist.DefaultDiskPermissionsTest)
	if err != nil {
		t.Fatal(err)
	}

	// Store wal in persist dir.
	dstPath := filepath.Join(testdir, walFile)
	println("dst", dstPath)
	err = ioutil.WriteFile(dstPath, wal, persist.DefaultDiskPermissionsTest)
	if err != nil {
		t.Fatal(err)
	}

	// Start contract manager with existing wal.
	cm, err := New(testdir)
	if err != nil {
		t.Fatal(err)
	}
	err = cm.Close()
	if err != nil {
		t.Fatal(err)
	}
}

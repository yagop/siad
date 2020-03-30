package proto

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"testing"

	"gitlab.com/NebulousLabs/fastrand"

	"gitlab.com/NebulousLabs/writeaheadlog"

	"gitlab.com/NebulousLabs/Sia/modules"

	"gitlab.com/NebulousLabs/errors"

	"gitlab.com/NebulousLabs/Sia/build"
	"gitlab.com/NebulousLabs/Sia/crypto"
	"gitlab.com/NebulousLabs/Sia/types"
)

var testWAL = newTestWAL()

// TestRefCounter_Count tests that the Count method always returns the correct
// counter value, either from disk or from in-mem storage.
func TestRefCounter_Count(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
	}
	t.Parallel()

	// prepare a refcounter for the tests
	rc := testPrepareRefCounter(2+fastrand.Uint64n(10), t)
	sec := uint64(2)
	val := uint16(21)

	// set up the expected value on disk
	err := writeVal(rc.filepath, sec, val)
	if err != nil {
		t.Fatal("Failed to write a count to disk:", err)
	}

	// verify we can read it correctly
	rval, err := rc.Count(sec)
	if err != nil {
		t.Fatal("Failed to read count from disk:", err)
	}
	if rval != val {
		t.Fatal(fmt.Sprintf("read wrong value from disk: expected %d, got %d", val, rval))
	}

	// check behaviour on bad sector number
	_, err = rc.Count(math.MaxInt64)
	if !errors.Contains(err, ErrInvalidSectorNumber) {
		t.Fatal("Expected ErrInvalidSectorNumber, got:", err)
	}

	// set up a temporary override
	ov := uint16(12)
	rc.newSectorCounts[sec] = ov

	// verify we can read it correctly
	rov, err := rc.Count(sec)
	if err != nil {
		t.Fatal("Failed to read count from disk:", err)
	}
	if rov != ov {
		t.Fatal(fmt.Sprintf("read wrong override value from disk: expected %d, got %d", ov, rov))
	}
}

// TestRefCounter_Append tests that the Decrement method behaves correctly
func TestRefCounter_Append(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
	}
	t.Parallel()

	// prepare a refcounter for the tests
	numSec := fastrand.Uint64n(10)
	rc := testPrepareRefCounter(numSec, t)
	stats, err := os.Stat(rc.filepath)
	if err != nil {
		t.Fatal("RefCounter creation finished successfully but the file is not accessible:", err)
	}
	err = rc.StartUpdate()
	if err != nil {
		t.Fatal("Failed to start an update session", err)
	}

	// test Append
	u, err := rc.Append()
	if err != nil {
		t.Fatal("Failed to create an append update", err)
	}
	expectNumSec := numSec + 1
	if rc.numSectors != expectNumSec {
		t.Fatal(fmt.Errorf("append failed to properly increase the numSectors counter. Expected %d, got %d", expectNumSec, rc.numSectors))
	}

	// apply the update
	err = rc.CreateAndApplyTransaction(u)
	if err != nil {
		t.Fatal("Failed to apply append update:", err)
	}
	rc.UpdateApplied()

	// verify: we expect the file size to have grown by 2 bytes
	endStats, err := os.Stat(rc.filepath)
	if err != nil {
		t.Fatal("Failed to get file stats:", err)
	}
	expectSize := stats.Size() + 2
	actualSize := endStats.Size()
	if actualSize != expectSize {
		t.Fatal(fmt.Sprintf("File size did not grow as expected. Expected size: %d, actual size: %d", expectSize, actualSize))
	}
}

// TestRefCounter_Decrement tests that the Decrement method behaves correctly
func TestRefCounter_Decrement(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
	}
	t.Parallel()

	// prepare a refcounter for the tests
	rc := testPrepareRefCounter(2+fastrand.Uint64n(10), t)
	err := rc.StartUpdate()
	if err != nil {
		t.Fatal("Failed to start an update session", err)
	}

	// test Decrement
	u, err := rc.Decrement(rc.numSectors - 2)
	if err != nil {
		t.Fatal("Failed to create an decrement update:", err)
	}

	// verify: we expect the value to have increased from the base 1 to 0
	val, err := rc.readCount(rc.numSectors - 2)
	if err != nil {
		t.Fatal("Failed to read value after decrement:", err)
	}
	if val != 0 {
		t.Fatal(fmt.Errorf("read wrong value after decrement. Expected %d, got %d", 2, val))
	}

	// check behaviour on bad sector number
	_, err = rc.Decrement(math.MaxInt64)
	if !errors.Contains(err, ErrInvalidSectorNumber) {
		t.Fatal("Expected ErrInvalidSectorNumber, got:", err)
	}

	// apply the update
	err = rc.CreateAndApplyTransaction(u)
	if err != nil {
		t.Fatal("Failed to apply decrement update:", err)
	}
	rc.UpdateApplied()
}

// TestRefCounter_Delete tests that the Delete method behaves correctly
func TestRefCounter_Delete(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
	}
	t.Parallel()

	// prepare a refcounter for the tests
	rc := testPrepareRefCounter(fastrand.Uint64n(10), t)
	err := rc.StartUpdate()
	if err != nil {
		t.Fatal("Failed to start an update session", err)
	}

	// delete the ref counter
	u, err := rc.DeleteRefCounter()
	if err != nil {
		t.Fatal("Failed to create a delete update", err)
	}

	// apply the update
	err = rc.CreateAndApplyTransaction(u)
	if err != nil {
		t.Fatal("Failed to apply a delete update:", err)
	}
	rc.UpdateApplied()

	// verify
	_, err = os.Stat(rc.filepath)
	if err == nil {
		t.Fatal("RefCounter deletion finished successfully but the file is still on disk", err)
	}
}

// TestRefCounter_DropSectors tests that the DropSectors method behaves
// correctly and the file's size is properly adjusted
func TestRefCounter_DropSectors(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
	}
	t.Parallel()

	// prepare a refcounter for the tests
	numSec := 2 + fastrand.Uint64n(10)
	rc := testPrepareRefCounter(numSec, t)
	stats, err := os.Stat(rc.filepath)
	if err != nil {
		t.Fatal("RefCounter creation finished successfully but the file is not accessible:", err)
	}
	err = rc.StartUpdate()
	if err != nil {
		t.Fatal("Failed to start an update session", err)
	}

	// check behaviour on bad sector number
	// (trying to drop more sectors than we have)
	_, err = rc.DropSectors(math.MaxInt64)
	if !errors.Contains(err, ErrInvalidSectorNumber) {
		t.Fatal("Expected ErrInvalidSectorNumber, got:", err)
	}

	// test DropSectors by dropping two counters
	u, err := rc.DropSectors(2)
	if err != nil {
		t.Fatal("Failed to create truncate update:", err)
	}
	expectNumSec := numSec - 2
	if rc.numSectors != expectNumSec {
		t.Fatal(fmt.Errorf("wrong number of counters after Truncate. Expected %d, got %d", expectNumSec, rc.numSectors))
	}

	// apply the update
	err = rc.CreateAndApplyTransaction(u)
	if err != nil {
		t.Fatal("Failed to apply truncate update:", err)
	}
	rc.UpdateApplied()

	//verify:  we expect the file size to have shrunk with 2*2 bytes
	endStats, err := os.Stat(rc.filepath)
	if err != nil {
		t.Fatal("Failed to get file stats:", err)
	}
	expectSize := stats.Size() - 4
	actualSize := endStats.Size()
	if actualSize != expectSize {
		t.Fatal(fmt.Sprintf("File size did not shrink as expected. Expected size: %d, actual size: %d", expectSize, actualSize))
	}
}

// TestRefCounter_Increment tests that the Decrement method behaves correctly
func TestRefCounter_Increment(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
	}
	t.Parallel()

	// prepare a refcounter for the tests
	rc := testPrepareRefCounter(2+fastrand.Uint64n(10), t)
	err := rc.StartUpdate()
	if err != nil {
		t.Fatal("Failed to start an update session", err)
	}

	// test Increment
	secIdx := rc.numSectors - 2
	u, err := rc.Increment(secIdx)
	if err != nil {
		t.Fatal("Failed to create an increment update:", err)
	}

	// verify: we expect the value to have increased from the base 1 to 2
	readValAfterInc, err := rc.readCount(secIdx)
	if err != nil {
		t.Fatal("Failed to read value after increment:", err)
	}
	if readValAfterInc != 2 {
		t.Fatal(fmt.Errorf("read wrong value after increment. Expected %d, got %d", 2, readValAfterInc))
	}

	// check behaviour on bad sector number
	_, err = rc.Increment(math.MaxInt64)
	if !errors.Contains(err, ErrInvalidSectorNumber) {
		t.Fatal("Expected ErrInvalidSectorNumber, got:", err)
	}

	// apply the update
	err = rc.CreateAndApplyTransaction(u)
	if err != nil {
		t.Fatal("Failed to apply increment update:", err)
	}
	rc.UpdateApplied()
}

// TestRefCounter_Load specifically tests refcounter's Load method
func TestRefCounter_Load(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
	}
	t.Parallel()

	// prepare a refcounter to load
	rc := testPrepareRefCounter(fastrand.Uint64n(10), t)

	// happy case
	_, err := LoadRefCounter(rc.filepath, testWAL)
	if err != nil {
		t.Fatal("Failed to load refcounter:", err)
	}

	// fails with os.ErrNotExist for a non-existent file
	_, err = LoadRefCounter("there-is-no-such-file.rc", testWAL)
	if !errors.IsOSNotExist(err) {
		t.Fatal("Expected os.ErrNotExist, got something else:", err)
	}
}

// TestRefCounter_Load_InvalidHeader checks that loading a refcounters file with
// invalid header fails.
func TestRefCounter_Load_InvalidHeader(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
	}
	t.Parallel()

	// prepare
	cid := types.FileContractID(crypto.HashBytes([]byte("contractId")))
	d := build.TempDir(t.Name())
	err := os.MkdirAll(d, modules.DefaultDirPerm)
	if err != nil {
		t.Fatal("Failed to create test directory:", err)
	}
	path := filepath.Join(d, cid.String()+refCounterExtension)

	// Create a file that contains a corrupted header. This basically means
	// that the file is too short to have the entire header in there.
	f, err := os.Create(path)
	if err != nil {
		t.Fatal("Failed to create test file:", err)
	}
	defer f.Close()

	// The version number is 8 bytes. We'll only write 4.
	_, err = f.Write(fastrand.Bytes(4))
	if err != nil {
		t.Fatal("Failed to write to test file:", err)
	}
	_ = f.Sync()

	// Make sure we fail to load from that file and that we fail with the right
	// error
	_, err = LoadRefCounter(path, testWAL)
	if !errors.Contains(err, io.EOF) {
		t.Fatal(fmt.Sprintf("Should not be able to read file with bad header, expected `%s` error, got:", io.EOF.Error()), err)
	}
}

// TestRefCounter_Load_InvalidVersion checks that loading a refcounters file
// with invalid version fails.
func TestRefCounter_Load_InvalidVersion(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
	}
	t.Parallel()

	// prepare
	cid := types.FileContractID(crypto.HashBytes([]byte("contractId")))
	d := build.TempDir(t.Name())
	err := os.MkdirAll(d, modules.DefaultDirPerm)
	if err != nil {
		t.Fatal("Failed to create test directory:", err)
	}
	path := filepath.Join(d, cid.String()+refCounterExtension)

	// create a file with a header that encodes a bad version number
	f, err := os.Create(path)
	if err != nil {
		t.Fatal("Failed to create test file:", err)
	}
	defer f.Close()

	// The first 8 bytes are the version number. Write down an invalid one
	// followed 4 counters (another 8 bytes).
	_, err = f.Write(fastrand.Bytes(16))
	if err != nil {
		t.Fatal("Failed to write to test file:", err)
	}
	_ = f.Sync()

	// ensure that we cannot load it and we return the correct error
	_, err = LoadRefCounter(path, testWAL)
	if !errors.Contains(err, ErrInvalidVersion) {
		t.Fatal(fmt.Sprintf("Should not be able to read file with wrong version, expected `%s` error, got:", ErrInvalidVersion.Error()), err)
	}
}

// TestRefCounter_Swap tests that the Swap method results in correct values
func TestRefCounter_Swap(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
	}
	t.Parallel()

	// prepare a refcounter for the tests
	rc := testPrepareRefCounter(2+fastrand.Uint64n(10), t)
	updates := make([]writeaheadlog.Update, 0)
	err := rc.StartUpdate()
	if err != nil {
		t.Fatal("Failed to start an update session", err)
	}

	// increment one of the sectors, so we can tell the values apart
	u, err := rc.Increment(rc.numSectors - 1)
	if err != nil {
		t.Fatal("Failed to create increment update", err)
	}
	updates = append(updates, u)

	// test Swap
	us, err := rc.Swap(rc.numSectors-2, rc.numSectors-1)
	updates = append(updates, us...)
	if err != nil {
		t.Fatal("Failed to create swap update", err)
	}
	var v1, v2 uint16
	v1, err = rc.readCount(rc.numSectors - 2)
	if err != nil {
		t.Fatal("Failed to read value after swap", err)
	}
	v2, err = rc.readCount(rc.numSectors - 1)
	if err != nil {
		t.Fatal("Failed to read value after swap", err)
	}
	if v1 != 2 || v2 != 1 {
		t.Fatal(fmt.Errorf("read wrong value after swap. Expected %d and %d, got %d and %d", 2, 1, v1, v2))
	}

	// check behaviour on bad sector number
	_, err = rc.Swap(math.MaxInt64, 0)
	if !errors.Contains(err, ErrInvalidSectorNumber) {
		t.Fatal("Expected ErrInvalidSectorNumber, got:", err)
	}

	// apply the updates and check the values again
	err = rc.CreateAndApplyTransaction(updates...)
	if err != nil {
		t.Fatal("Failed to apply updates", err)
	}
	rc.UpdateApplied()
}

// TestRefCounter_UpdateSessionConstraints ensures that StartUpdate() and UpdateApplied()
// enforce all applicable restrictions to update creation and execution
func TestRefCounter_UpdateSessionConstraints(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
	}
	t.Parallel()

	// prepare a refcounter for the tests
	rc := testPrepareRefCounter(fastrand.Uint64n(10), t)

	var u writeaheadlog.Update
	// make sure we cannot create updates outside of an update session
	_, err1 := rc.Append()
	_, err2 := rc.Decrement(1)
	_, err3 := rc.DeleteRefCounter()
	_, err4 := rc.DropSectors(1)
	_, err5 := rc.Increment(1)
	_, err6 := rc.Swap(1, 2)
	err7 := rc.CreateAndApplyTransaction(u)
	for i, err := range []error{err1, err2, err3, err4, err5, err6, err7} {
		if !errors.Contains(err, ErrUpdateWithoutUpdateSession) {
			t.Fatalf("err%v: expected %v but was %v", i+1, ErrUpdateWithoutUpdateSession, err)
		}
	}

	// start an update session
	err := rc.StartUpdate()
	if err != nil {
		t.Fatal("Failed to start an update session", err)
	}
	// delete the ref counter
	u, err = rc.DeleteRefCounter()
	if err != nil {
		t.Fatal("Failed to create a delete update", err)
	}
	// make sure we cannot create any updates after a deletion has been triggered
	_, err1 = rc.Append()
	_, err2 = rc.Decrement(1)
	_, err3 = rc.DeleteRefCounter()
	_, err4 = rc.DropSectors(1)
	_, err5 = rc.Increment(1)
	_, err6 = rc.Swap(1, 2)
	for i, err := range []error{err1, err2, err3, err4, err5, err6} {
		if !errors.Contains(err, ErrUpdateAfterDelete) {
			t.Fatalf("err%v: expected %v but was %v", i+1, ErrUpdateAfterDelete, err)
		}
	}

	// apply the update
	err = rc.CreateAndApplyTransaction(u)
	if err != nil {
		t.Fatal("Failed to apply a delete update:", err)
	}
	rc.UpdateApplied()

	// verify: make sure we cannot start an update session on a deleted counter
	err = rc.StartUpdate()
	if !errors.Contains(err, ErrUpdateAfterDelete) {
		t.Fatal("Failed to prevent an update creation after a deletion", err)
	}
}

// TestRefCounter_WALFunctions tests RefCounter's functions for creating and
// reading WAL updates
func TestRefCounter_WALFunctions(t *testing.T) {
	t.Parallel()

	// test creating and reading updates
	wpath := "test/writtenPath"
	wsec := uint64(2)
	wval := uint16(12)
	u := createWriteAtUpdate(wpath, wsec, wval)
	rpath, rsec, rval, err := readWriteAtUpdate(u)
	if err != nil {
		t.Fatal("Failed to read writeAt update:", err)
	}
	if wpath != rpath || wsec != rsec || wval != rval {
		t.Fatal(fmt.Errorf("wrong values read from WriteAt update. Expected %s, %d, %d, found %s, %d, %d", wpath, wsec, wval, rpath, rsec, rval))
	}

	u = createTruncateUpdate(wpath, wsec)
	rpath, rsec, err = readTruncateUpdate(u)
	if err != nil {
		t.Fatal("Failed to read a truncate update:", err)
	}
	if wpath != rpath || wsec != rsec {
		t.Fatal(fmt.Errorf("wrong values read from Truncate update. Expected %s, %d found %s, %d", wpath, wsec, rpath, rsec))
	}
}

// newTestWal is a helper method to create a WAL for testing.
func newTestWAL() *writeaheadlog.WAL {
	// Create the wal.
	wd := filepath.Join(os.TempDir(), "rc-wals")
	if err := os.MkdirAll(wd, modules.DefaultDirPerm); err != nil {
		panic(err)
	}
	p := filepath.Join(wd, hex.EncodeToString(fastrand.Bytes(8)))
	_, wal, err := writeaheadlog.New(p)
	if err != nil {
		panic(err)
	}
	return wal
}

// testPrepareRefCounter is a helper that creates a refcounter and fails the
// test if that is not successful
func testPrepareRefCounter(numSec uint64, t *testing.T) *RefCounter {
	tcid := types.FileContractID(crypto.HashBytes([]byte("contractId")))
	td := build.TempDir(t.Name())
	err := os.MkdirAll(td, modules.DefaultDirPerm)
	if err != nil {
		t.Fatal("Failed to create test directory:", err)
	}
	path := filepath.Join(td, tcid.String()+refCounterExtension)
	// create a ref counter
	rc, err := NewRefCounter(path, numSec, testWAL)
	if err != nil {
		t.Fatal("Failed to create a reference counter:", err)
	}
	return rc
}

// writeVal is a helper method that writes a certain counter value to disk. This
// method does not do any validations or checks, the caller must make certain
// that the input parameters are valid.
func writeVal(path string, secIdx uint64, val uint16) error {
	f, err := os.OpenFile(path, os.O_RDWR, modules.DefaultFilePerm)
	if err != nil {
		return errors.AddContext(err, "failed to open refcounter file")
	}
	defer f.Close()
	var b u16
	binary.LittleEndian.PutUint16(b[:], val)
	if _, err = f.WriteAt(b[:], int64(offset(secIdx))); err != nil {
		return errors.AddContext(err, "failed to write to refcounter file")
	}
	return f.Sync()
}

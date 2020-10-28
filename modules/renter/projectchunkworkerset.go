package renter

// TODO: Currently, to optimize upload latency, we return the skylink to the
// user as soon as a single sector has finished uploading. This can cause
// problems if the user immediately attempts to download the file, resulting in
// the user creating a pcws that will be immediately out of date, and will
// remain out of date for the entire first 'pcwsWorkerStateResetTime'.
//
// We either need to change the upload streamer to delay returning the skylink
// until the upload is more complete, or we need the pcws to be able to reset
// relatively quickly the first time. Because skylinks are cross-portal, it's
// not sufficient to get a signal from elsewhere in siad that the upload is now
// complete, because the portal doing the download may not be the same as the
// portal doing the upload.

import (
	"context"
	"fmt"
	"sync"
	"time"

	"gitlab.com/NebulousLabs/Sia/build"
	"gitlab.com/NebulousLabs/Sia/crypto"
	"gitlab.com/NebulousLabs/Sia/modules"

	"gitlab.com/NebulousLabs/errors"
)

var (
	// pcwsWorkerStateResetTime defines the amount of time that the pcws will
	// wait before resetting / refreshing the worker state, meaning that all of
	// the workers will do another round of HasSector queries on the network.
	pcwsWorkerStateResetTime = build.Select(build.Var{
		Dev:      time.Minute * 10,
		Standard: time.Hour * 3 * 3,
		Testing:  time.Second * 15,
	}).(time.Duration)

	// pcwsHasSectorTimeout defines the amount of time that the pcws will wait
	// before giving up on receiving a HasSector response from a single worker.
	// This value is set as a global timeout because different download queries
	// that have different timeouts will use the same projectChunkWorkerSet.
	pcwsHasSectorTimeout = build.Select(build.Var{
		Dev:      time.Minute * 1,
		Standard: time.Minute * 3,
		Testing:  time.Second * 10,
	}).(time.Duration)
)

const (
	// pcwsGougingFractionDenom is used to identify what percentage of the
	// allowance is allowed to be spent on HasSector jobs before a worker is
	// flagged for being too expensive.
	//
	// For example, if the denom is 10, that means that if a worker's HasSector
	// cost multiplied by the total expected number of HasSector jobs to be
	// performed in a period exceeds 10% of the allowance, that worker will be
	// flagged for price gouging. If the denom is 100, the worker will be
	// flagged if the HasSector cost reaches 1% of the total cost of the
	// allowance.
	pcwsGougingFractionDenom = 25
)

// pcwsUnreseovledWorker tracks an unresolved worker that is associated with a
// specific projectChunkWorkerSet. The timestamp indicates when the unresolved
// worker is expected to have a resolution, and is an estimate based on historic
// performance from the worker.
type pcwsUnresolvedWorker struct {
	// The expected time that the HasSector job will finish, and the worker will
	// be able to resolve.
	staticExpectedCompleteTime time.Time

	// The worker that is performing the HasSector job.
	staticWorker *worker
}

// pcwsWorkerResponse contains a worker's response to a HasSector query. There
// is a list of piece indices where the worker responded that they had the piece
// at that index.
type pcwsWorkerResponse struct {
	worker       *worker
	pieceIndices []uint64
}

// pcwsWorkerState contains the worker state for a single thread that is
// resolving which workers have which pieces. When the projectChunkWorkerSet
// resets, it does so by spinning up a new pcwsWorkerState and then replacing
// the old worker state with the new worker state. The new worker state will
// send out a new round of HasSector queries to the network.
type pcwsWorkerState struct {
	// unresolvedWorkers is the set of workers that are currently running
	// HasSector programs and have not yet finished.
	//
	// A map is used so that workers can be removed from the set in constant
	// time as they complete their HasSector jobs.
	unresolvedWorkers map[string]*pcwsUnresolvedWorker

	// ResolvedWorkers is an array that tracks which workers have responded to
	// HasSector queries and which sectors are available. This array is only
	// appended to as workers come back, meaning that chunk downloads can track
	// internally which elements of the array they have already looked at,
	// saving computational time when updating.
	resolvedWorkers []*pcwsWorkerResponse

	// workerUpdateChans is used by download objects to block until more
	// information about the unresolved workers is available. All of the worker
	// update chans will be closed each time an unresolved worker returns a
	// response (regardless of whether the response is positive or negative).
	// The array will then be cleared.
	//
	// NOTE: Once 'unresolvedWorkers' has a length of zero, any attempt to add a
	// channel to the set of workerUpdateChans should fail, as there will be no
	// more updates. This is specific to this particular worker state, the
	// pcwsWorkerSet as a whole can be reset by replacing the worker state.
	workerUpdateChans []chan struct{}

	// Utilities.
	staticRenter *Renter
	mu           sync.Mutex
}

// projectChunkWorkerSet is an object that contains a set of workers that can be
// used to download a single chunk. The object can be initialized with either a
// set of roots (for Skynet downloads) or with a siafile where the host-root
// pairs are already known (for traditional renter downloads).
//
// If the pcws is initialized with only a set of roots, it will immediately spin
// up a bunch of worker jobs to locate those roots on the network using
// HasSector programs.
//
// Once the pcws has been initialized, it can be used repeatedly to download
// data from the chunk, and it will not need to repeat the network lookups.
// Every few hours (pcwsWorkerStateResetTime), it will re-do the lookups to
// ensure that it is up-to-date on the best way to download the file.
type projectChunkWorkerSet struct {
	// workerState is a pointer to a single pcwsWorkerState, specifically the
	// most recent worker state that has launched. The workerState is
	// responsible for querying the network with HasSector requests and
	// determining which workers are able to download which pieces of the chunk.
	//
	// workerStateLaunchTime indicates when the workerState was launched, which
	// is used to figure out when the worker state should be refreshed.
	//
	// updateInProgress and updateFinishedChan are used to ensure that only one
	// worker state is being refreshed at a time. Before a workerState refresh
	// begins, the projectChunkWorkerSet is locked and the updateInProgress
	// value is set to 'true'. At the same time, a new 'updateFinishedChan' is
	// created. Then the projectChunkWorkerSet is unlocked. New threads that try
	// to launch downloads will see that there is an update in progress and will
	// wait on the 'updateFinishedChan' to close before grabbing the new
	// workerState. When the new workerState is done being initialized, the
	// projectChunkWorkerSet is locked and the updateInProgress field is set to
	// false, the workerState is updated to the new state, and the
	// updateFinishedChan is closed.
	updateInProgress      bool
	updateFinishedChan    chan struct{}
	workerState           *pcwsWorkerState
	workerStateLaunchTime time.Time

	// Decoding and decryption information for the chunk.
	staticChunkIndex   uint64
	staticErasureCoder modules.ErasureCoder
	staticMasterKey    crypto.CipherKey
	staticPieceRoots   []crypto.Hash

	// Utilities
	staticCtx    context.Context
	staticRenter *Renter
	mu           sync.Mutex
}

// checkPCWSGouging verifies the cost of grabbing the HasSector information from
// a host is reasonble. The cost of completing the download is not checked.
//
// NOTE: The logic in this function assumes that every pcws results in just one
// download. The reality is that depending on the type of use case, there may be
// significantly less than 1 download per pcws (for single-user nodes that
// frequently open large movies without watching the full movie), or
// significantly more than one download per pcws (for multi-user nodes where
// users most commonly are using the same file over and over).
func checkPCWSGouging(pt modules.RPCPriceTable, allowance modules.Allowance, numWorkers int, numRoots int) error {
	// Check whether the download bandwidth price is too high.
	if !allowance.MaxDownloadBandwidthPrice.IsZero() && allowance.MaxDownloadBandwidthPrice.Cmp(pt.DownloadBandwidthCost) < 0 {
		return fmt.Errorf("download bandwidth price of host is %v, which is above the maximum allowed by the allowance: %v - price gouging protection enabled", pt.DownloadBandwidthCost, allowance.MaxDownloadBandwidthPrice)
	}
	// Check whether the upload bandwidth price is too high.
	if !allowance.MaxUploadBandwidthPrice.IsZero() && allowance.MaxUploadBandwidthPrice.Cmp(pt.UploadBandwidthCost) < 0 {
		return fmt.Errorf("upload bandwidth price of host is %v, which is above the maximum allowed by the allowance: %v - price gouging protection enabled", pt.UploadBandwidthCost, allowance.MaxUploadBandwidthPrice)
	}
	// If there is no allowance, price gouging checks have to be disabled,
	// because there is no baseline for understanding what might count as price
	// gouging.
	if allowance.Funds.IsZero() {
		return nil
	}

	// Calculate the cost of a has sector job.
	pb := modules.NewProgramBuilder(&pt, 0)
	for i := 0; i < numRoots; i++ {
		pb.AddHasSectorInstruction(crypto.Hash{})
	}
	programCost, _, _ := pb.Cost(true)
	ulbw, dlbw := hasSectorJobExpectedBandwidth(numRoots)
	bandwidthCost := modules.MDMBandwidthCost(pt, ulbw, dlbw)
	costHasSectorJob := programCost.Add(bandwidthCost)

	// Determine based on the allowance the number of HasSector jobs that would
	// need to be performed under normal conditions to reach the desired amount
	// of total data.
	requiredProjects := allowance.ExpectedDownload / modules.StreamDownloadSize
	requiredHasSectorQueries := requiredProjects * uint64(numWorkers)

	// Determine the total amount that we'd be willing to spend on all of those
	// queries before considering the host complicit in gouging.
	totalCost := costHasSectorJob.Mul64(requiredHasSectorQueries)
	reducedAllowance := allowance.Funds.Div64(pcwsGougingFractionDenom)

	// Check that we do not consider the host complicit in gouging.
	if totalCost.Cmp(reducedAllowance) > 0 {
		errStr := fmt.Sprintf("the cost of performing a HasSector job is too high - price gouging protection enabled")
		return errors.New(errStr)
	}
	return nil
}

// closeUpdateChans will close all of the update chans and clear out the slice.
// This will cause any threads waiting for more results from the unresolved
// workers to unblock.
//
// Typically there will be a small number of channels, often 0 and often just 1.
func (ws *pcwsWorkerState) closeUpdateChans() {
	for _, c := range ws.workerUpdateChans {
		close(c)
	}
	ws.workerUpdateChans = nil
}

// registerForWorkerUpdate will create a channel and append it to the list of
// update chans in the worker state. When there is more information available
// about which worker is the best worker to select, the channel will be closed.
func (ws *pcwsWorkerState) registerForWorkerUpdate() <-chan struct{} {
	// Return a nil channel if there are no more unresolved workers.
	if len(ws.unresolvedWorkers) == 0 {
		return nil
	}

	// Create the channel that will be closed when the set of unresolved workers
	// has been updated.
	c := make(chan struct{})
	ws.workerUpdateChans = append(ws.workerUpdateChans, c)
	return c
}

// managedHandleResponse will handle a HasSector response from a worker,
// updating the workerState accordingly.
//
// The worker's response will be included into the resolvedWorkers even if it is
// emptied or errored because the worker selection algorithms in the downloads
// may wish to be able to view which workers have failed. This is currently
// unused, but certain computational optimizations in the future depend on it.
func (ws *pcwsWorkerState) managedHandleResponse(resp *jobHasSectorResponse) {
	ws.mu.Lock()
	defer ws.mu.Unlock()

	// Delete the worker from the set of unresolved workers.
	w := resp.staticWorker
	if w == nil {
		ws.staticRenter.log.Critical("nil worker provided in resp")
	}
	delete(ws.unresolvedWorkers, w.staticHostPubKeyStr)
	ws.closeUpdateChans()

	// If the response contained an error, add this worker to the set of
	// resolved workers as supporting no indices.
	if resp.staticErr != nil {
		ws.resolvedWorkers = append(ws.resolvedWorkers, &pcwsWorkerResponse{
			worker: w,
		})
		return
	}

	// Create the list of pieces that the worker supports and add it to the
	// worker set.
	var indices []uint64
	for i, available := range resp.staticAvailables {
		if available {
			indices = append(indices, uint64(i))
		}
	}
	// Add this worker to the set of resolved workers (even if there are no
	// indices that the worker can fetch).
	ws.resolvedWorkers = append(ws.resolvedWorkers, &pcwsWorkerResponse{
		worker:       w,
		pieceIndices: indices,
	})
}

// managedLaunchWorker will launch a job to determine which sectors of a chunk
// are available through that worker. The resulting unresolved worker is
// returned so it can be added to the pending worker state.
func (pcws *projectChunkWorkerSet) managedLaunchWorker(ctx context.Context, w *worker, responseChan chan *jobHasSectorResponse, ws *pcwsWorkerState) error {
	// Check for gouging.
	cache := w.staticCache()
	pt := w.staticPriceTable().staticPriceTable
	numWorkers := pcws.staticRenter.staticWorkerPool.callNumWorkers()
	err := checkPCWSGouging(pt, cache.staticRenterAllowance, numWorkers, len(pcws.staticPieceRoots))
	if err != nil {
		pcws.staticRenter.log.Debugf("price gouging for chunk worker set detected in worker %v, err %v", w.staticHostPubKeyStr, err)
		return err
	}

	// Create and launch the job.
	jhs := w.newJobHasSector(ctx, responseChan, pcws.staticPieceRoots...)
	expectedCompleteTime, err := w.staticJobHasSectorQueue.callAddWithEstimate(jhs)
	if err != nil {
		pcws.staticRenter.log.Debugf("unable to add has sector job to %v, err %v", w.staticHostPubKeyStr, err)
		return err
	}

	// Create the unresolved worker for this job.
	uw := &pcwsUnresolvedWorker{
		staticWorker: w,

		staticExpectedCompleteTime: expectedCompleteTime,
	}

	// Add the unresolved worker to the worker state. Technically this doesn't
	// need to be wrapped in a lock, but that's not obvious from the function
	// context so we wrap it in a lock anyway. There will be no contention, so
	// there should be minimal performance overhead.
	ws.mu.Lock()
	ws.unresolvedWorkers[w.staticHostPubKeyStr] = uw
	ws.mu.Unlock()
	return nil
}

// threadedFindWorkers will spin up a bunch of jobs to determine which workers
// have what pieces for the pcws, and then update the input worker state with
// the results.
func (pcws *projectChunkWorkerSet) threadedFindWorkers(allWorkersLaunchedChan chan<- struct{}, ws *pcwsWorkerState) {
	err := pcws.staticRenter.tg.Add()
	if err != nil {
		return
	}
	defer pcws.staticRenter.tg.Done()

	// Create a context for finding jobs which has a timeout for waiting on
	// HasSector requests to return.
	ctx, cancel := context.WithTimeout(pcws.staticCtx, pcwsHasSectorTimeout)
	defer cancel()

	// Launch all of the HasSector jobs for each worker. A channel is needed to
	// receive the responses, and the channel needs to be buffered to be equal
	// in size to the number of queries so that none of the workers sending
	// reponses get blocked sending down the channel.
	workers := ws.staticRenter.staticWorkerPool.callWorkers()
	workersLaunched := 0
	responseChan := make(chan *jobHasSectorResponse, len(workers))
	for _, w := range workers {
		err := pcws.managedLaunchWorker(ctx, w, responseChan, ws)
		if err == nil {
			workersLaunched++
		}
	}

	// Signal that all of the workers have launched.
	close(allWorkersLaunchedChan)

	// Because there are timeouts on the HasSector programs, the longest that
	// this loop should be active is a little bit longer than the full timeout
	// for a single HasSector job.
	workersResponded := 0
	for workersResponded < workersLaunched {
		// Block until there is a worker response. Give up if the context times
		// out.
		var resp *jobHasSectorResponse
		select {
		case resp = <-responseChan:
			workersResponded++
		case <-ctx.Done():
			return
		case <-pcws.staticRenter.tg.StopChan():
			return
		}

		// Consistency check - should not be getting nil responses from the
		// workers.
		if resp == nil {
			ws.staticRenter.log.Critical("nil response received")
			continue
		}

		// Parse the response.
		ws.managedHandleResponse(resp)
	}
}

// managedWorkerState returns a pointer to the current worker state object
func (pcws *projectChunkWorkerSet) managedWorkerState() *pcwsWorkerState {
	pcws.mu.Lock()
	defer pcws.mu.Unlock()
	return pcws.workerState
}

// managedTryUpdateWorkerState will check whether the worker state needs to be
// refreshed. If so, it will refresh the worker state.
func (pcws *projectChunkWorkerSet) managedTryUpdateWorkerState() error {
	// The worker state does not need to be refreshed if it is recent or if
	// there is another refresh currently in progress.
	pcws.mu.Lock()
	if pcws.updateInProgress || time.Since(pcws.workerStateLaunchTime) < pcwsWorkerStateResetTime {
		c := pcws.updateFinishedChan
		pcws.mu.Unlock()
		// If there is no update in progress, the channel will already be
		// closed, and therefore listening on the channel will return
		// immediately.
		<-c
		return nil
	}
	// An update is needed. Set the flag that an update is in progress.
	pcws.updateInProgress = true
	pcws.updateFinishedChan = make(chan struct{})
	pcws.mu.Unlock()

	// Create the new worker state and launch the thread that will create worker
	// jobs and collect responses from the workers.
	//
	// The concurrency here is a bit awkward because jobs cannot be launched
	// while the pcws lock is held, the workerState of the pcws cannot be set
	// until all the jobs are launched, and the context for timing out the
	// worker jobs needs to be created in the same thread that listens for the
	// responses. Though there are a lot of concurrency patterns at play here,
	// it was the cleanest thing I could come up with.
	allWorkersLaunchedChan := make(chan struct{})
	ws := &pcwsWorkerState{
		unresolvedWorkers: make(map[string]*pcwsUnresolvedWorker),

		staticRenter: pcws.staticRenter,
	}

	// Launch the thread to find the workers for this launch state.
	err := pcws.staticRenter.tg.Launch(func() {
		pcws.threadedFindWorkers(allWorkersLaunchedChan, ws)
	})
	if err != nil {
		// If there is an error, need to reset the in-progress fields. This will
		// result in the worker set continuing to use the previous worker state.
		pcws.mu.Lock()
		pcws.updateInProgress = false
		pcws.mu.Unlock()
		close(pcws.updateFinishedChan)
		return errors.AddContext(err, "unable to launch worker set")
	}

	// Wait for the thread to indicate that all jobs are launched, the worker
	// state is not ready for use until all jobs have been launched. After that,
	// update the pcws so that the workerState in the pcws is the newest worker
	// state.
	<-allWorkersLaunchedChan
	pcws.mu.Lock()
	pcws.updateInProgress = false
	pcws.workerState = ws
	pcws.workerStateLaunchTime = time.Now()
	pcws.mu.Unlock()
	close(pcws.updateFinishedChan)
	return nil
}

// newPCWSByRoots will create a worker set to download a chunk given just the
// set of sector roots associated with the pieces. The hosts that correspond to
// the roots will be determined by scanning the network with a large number of
// HasSector queries. Once opened, the projectChunkWorkerSet can be used to
// initiate many downloads.
func (r *Renter) newPCWSByRoots(ctx context.Context, roots []crypto.Hash, ec modules.ErasureCoder, masterKey crypto.CipherKey, chunkIndex uint64) (*projectChunkWorkerSet, error) {
	// Check that the number of roots provided is consistent with the erasure
	// coder provided.
	//
	// NOTE: There's a legacy special case where 1-of-N only needs 1 root.
	if len(roots) != ec.NumPieces() && !(len(roots) == 1 && ec.MinPieces() == 1) {
		return nil, fmt.Errorf("%v roots provided, but erasure coder specifies %v pieces", len(roots), ec.NumPieces())
	}

	// Create the worker set.
	pcws := &projectChunkWorkerSet{
		staticChunkIndex:   chunkIndex,
		staticErasureCoder: ec,
		staticMasterKey:    masterKey,
		staticPieceRoots:   roots,

		staticCtx:    ctx,
		staticRenter: r,
	}

	// The worker state is blank, ensure that everything can get started.
	err := pcws.managedTryUpdateWorkerState()
	if err != nil {
		return nil, errors.AddContext(err, "cannot create a new PCWS")
	}

	// Return the worker set.
	return pcws, nil
}

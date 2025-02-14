package db

import (
	"context"
	"fmt"

	"github.com/couchbase/sync_gateway/base"
)

// ActivePullReplicator is a unidirectional pull active replicator.
type ActivePullReplicator struct {
	*activeReplicatorCommon
}

func NewPullReplicator(config *ActiveReplicatorConfig) *ActivePullReplicator {
	arc := newActiveReplicatorCommon(config, ActiveReplicatorTypePull)
	return &ActivePullReplicator{
		activeReplicatorCommon: arc,
	}
}

func (apr *ActivePullReplicator) Start() error {
	apr.lock.Lock()
	defer apr.lock.Unlock()

	if apr == nil {
		return fmt.Errorf("nil ActivePullReplicator, can't start")
	}

	if apr.ctx != nil && apr.ctx.Err() == nil {
		return fmt.Errorf("ActivePullReplicator already running")
	}

	logCtx := context.WithValue(context.Background(), base.LogContextKey{}, base.LogContext{CorrelationID: apr.config.ID + "-" + string(ActiveReplicatorTypePull)})
	apr.ctx, apr.ctxCancel = context.WithCancel(logCtx)

	err := apr._connect()
	if err != nil {
		_ = apr._setError(err)
		base.WarnfCtx(apr.ctx, "Couldn't connect. Attempting to reconnect in background: %v", err)
		go apr.reconnect(apr._connect)
	}
	return err
}

func (apr *ActivePullReplicator) _connect() error {
	var err error
	apr.blipSender, apr.blipSyncContext, err = connect(apr.activeReplicatorCommon, "-pull")
	if err != nil {
		return err
	}

	if apr.config.ConflictResolverFunc != nil {
		apr.blipSyncContext.conflictResolver = NewConflictResolver(apr.config.ConflictResolverFunc, apr.config.ReplicationStatsMap)
	}
	apr.blipSyncContext.purgeOnRemoval = apr.config.PurgeOnRemoval

	// wrap the replicator context with a cancelFunc that can be called to abort the checkpointer from _disconnect
	apr.checkpointerCtx, apr.checkpointerCtxCancel = context.WithCancel(apr.ctx)
	if err := apr._initCheckpointer(); err != nil {
		// clean up anything we've opened so far
		apr.checkpointerCtx = nil
		apr.blipSender.Close()
		apr.blipSyncContext.Close()
		return err
	}

	subChangesRequest := SubChangesRequest{
		Continuous:     apr.config.Continuous,
		Batch:          apr.config.ChangesBatchSize,
		Since:          apr.Checkpointer.lastCheckpointSeq,
		Filter:         apr.config.Filter,
		FilterChannels: apr.config.FilterChannels,
		DocIDs:         apr.config.DocIDs,
		ActiveOnly:     apr.config.ActiveOnly,
		clientType:     clientTypeSGR2,
	}

	if err := subChangesRequest.Send(apr.blipSender); err != nil {
		// clean up anything we've opened so far
		apr.checkpointerCtxCancel()
		apr.checkpointerCtx = nil
		apr.blipSender.Close()
		apr.blipSyncContext.Close()
		return err
	}

	apr._setState(ReplicationStateRunning)
	return nil
}

// Complete gracefully shuts down a replication, waiting for all in-flight revisions to be processed
// before stopping the replication
func (apr *ActivePullReplicator) Complete() {
	apr.lock.Lock()
	if apr == nil {
		apr.lock.Unlock()
		return
	}

	err := apr.Checkpointer.waitForExpectedSequences()
	if err != nil {
		base.InfofCtx(apr.ctx, base.KeyReplicate, "Timeout draining replication %s - stopping: %v", apr.config.ID, err)
	}

	stopErr := apr._disconnect()
	if stopErr != nil {
		base.InfofCtx(apr.ctx, base.KeyReplicate, "Error attempting to stop replication %s: %v", apr.config.ID, stopErr)
	}
	apr._setState(ReplicationStateStopped)

	// unlock the replication before triggering callback, in case callback attempts to access replication information
	// from the replicator
	onCompleteCallback := apr.onReplicatorComplete
	apr.lock.Unlock()

	if onCompleteCallback != nil {
		onCompleteCallback()
	}
}

func (apr *ActivePullReplicator) _initCheckpointer() error {

	checkpointHash, hashErr := apr.config.CheckpointHash()
	if hashErr != nil {
		return hashErr
	}

	apr.Checkpointer = NewCheckpointer(apr.checkpointerCtx, apr.CheckpointID(), checkpointHash, apr.blipSender, apr.config)
	err := apr.Checkpointer.fetchCheckpoints()
	if err != nil {
		return err
	}

	apr.registerCheckpointerCallbacks()
	apr.Checkpointer.Start()

	return nil
}

// CheckpointID returns a unique ID to be used for the checkpoint client (which is used as part of the checkpoint Doc ID on the recipient)
func (apr *ActivePullReplicator) CheckpointID() string {
	return "sgr2cp:pull:" + apr.config.ID
}

func (apr *ActivePullReplicator) reset() error {
	if apr.state != ReplicationStateStopped {
		return fmt.Errorf("reset invoked for replication %s when the replication was not stopped", apr.config.ID)
	}
	return resetLocalCheckpoint(apr.config.ActiveDB, apr.CheckpointID())
}

// registerCheckpointerCallbacks registers appropriate callback functions for checkpointing.
func (apr *ActivePullReplicator) registerCheckpointerCallbacks() {
	apr.blipSyncContext.sgr2PullAlreadyKnownSeqsCallback = apr.Checkpointer.AddAlreadyKnownSeq

	apr.blipSyncContext.sgr2PullAddExpectedSeqsCallback = apr.Checkpointer.AddExpectedSeqIDAndRevs

	apr.blipSyncContext.sgr2PullProcessedSeqCallback = apr.Checkpointer.AddProcessedSeqIDAndRev

	// Trigger complete for non-continuous replications when caught up
	if !apr.config.Continuous {
		apr.blipSyncContext.emptyChangesMessageCallback = func() {
			// Complete blocks waiting for pending rev messages, so needs
			// it's own goroutine
			go apr.Complete()
		}
	}

}

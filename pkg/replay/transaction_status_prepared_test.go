package replay

import (
	"errors"
	"testing"
)

func TestPreparedCommitRechecksAncestorAfterForkSwitch(t *testing.T) {
	cache := NewTransactionStatusCache()
	requireNoError := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}

	requireNoError(cache.CommitBlock(statusCacheTestBlock(10)))
	requireNoError(cache.CommitBlock(statusCacheTestBlock(
		11,
		statusCacheTestTransaction(6, 6, 1),
	)))

	retried := statusCacheTestTransaction(7, 8, 2)
	unique := statusCacheTestTransaction(7, 9, 3)
	candidate := statusCacheTestBlock(12, retried, unique)
	plan, err := planBlockTransactionExecution(candidate)
	requireNoError(err)
	requireNoError(cache.validateBlockWithPlan(candidate, plan))

	requireNoError(cache.Unwind(11))
	replacement := statusCacheTestBlock(
		11,
		statusCacheTestTransaction(7, 8, 4),
	)
	requireNoError(cache.CommitBlock(replacement))

	err = cache.commitBlockWithPlan(candidate, plan)
	var ancestorErr *AncestorAlreadyProcessedTransactionMessagesError
	if !errors.As(err, &ancestorErr) {
		t.Fatalf("prepared commit error = %v, want ancestor AlreadyProcessed", err)
	}
	if ancestorErr.AlreadyProcessedCount != 1 ||
		len(ancestorErr.Occurrences) != 1 ||
		ancestorErr.Occurrences[0].Index != 0 ||
		ancestorErr.Occurrences[0].ProcessedSlot != 11 {
		t.Fatalf("ancestor error = %+v, want candidate tx 0 processed at slot 11", ancestorErr)
	}

	cache.mu.RLock()
	tipSlot := cache.tip.slot
	cache.mu.RUnlock()
	if tipSlot != 11 {
		t.Fatalf("status-cache tip = %d after rejected commit, want 11", tipSlot)
	}
	view := cache.View()
	containsUnique, err := view.ContainsTransaction(unique)
	requireNoError(err)
	if containsUnique {
		t.Fatal("rejected prepared commit partially published its unique transaction")
	}
}

func TestConcurrentPreparedSiblingCommitsPublishExactlyOne(t *testing.T) {
	cache := NewTransactionStatusCache()
	if err := cache.CommitBlock(statusCacheTestBlock(10)); err != nil {
		t.Fatal(err)
	}

	left := statusCacheTestBlock(11, statusCacheTestTransaction(1, 2, 3))
	right := statusCacheTestBlock(11, statusCacheTestTransaction(4, 5, 6))
	leftPlan, err := planBlockTransactionExecution(left)
	if err != nil {
		t.Fatal(err)
	}
	rightPlan, err := planBlockTransactionExecution(right)
	if err != nil {
		t.Fatal(err)
	}
	if err := cache.validateBlockWithPlan(left, leftPlan); err != nil {
		t.Fatalf("prevalidate left sibling: %v", err)
	}
	if err := cache.validateBlockWithPlan(right, rightPlan); err != nil {
		t.Fatalf("prevalidate right sibling: %v", err)
	}

	start := make(chan struct{})
	results := make(chan error, 2)
	go func() {
		<-start
		results <- cache.commitBlockWithPlan(left, leftPlan)
	}()
	go func() {
		<-start
		results <- cache.commitBlockWithPlan(right, rightPlan)
	}()
	close(start)

	successes := 0
	lineageFailures := 0
	for range 2 {
		err := <-results
		if err == nil {
			successes++
			continue
		}
		var lineageErr *TransactionStatusLineageError
		if errors.As(err, &lineageErr) {
			lineageFailures++
			continue
		}
		t.Fatalf("unexpected sibling commit error: %v", err)
	}
	if successes != 1 || lineageFailures != 1 {
		t.Fatalf("sibling results: %d successes, %d lineage failures; want 1 and 1", successes, lineageFailures)
	}

	view := cache.View()
	leftVisible, err := view.ContainsTransaction(left.Transactions[0])
	if err != nil {
		t.Fatal(err)
	}
	rightVisible, err := view.ContainsTransaction(right.Transactions[0])
	if err != nil {
		t.Fatal(err)
	}
	if leftVisible == rightVisible {
		t.Fatalf("published sibling statuses: left=%t right=%t; want exactly one", leftVisible, rightVisible)
	}
}

func TestCommitRootedStatusAndEventsStopsBeforeEventsOnStatusFailure(t *testing.T) {
	cache := NewTransactionStatusCache()
	if err := cache.CommitBlock(statusCacheTestBlock(10)); err != nil {
		t.Fatal(err)
	}
	candidate := statusCacheTestBlock(12)
	candidate.ParentSlot = 11
	recorded := false

	err := commitRootedStatusAndEvents(
		cache,
		candidate,
		func() error { return cache.CommitBlock(candidate) },
		func() error {
			recorded = true
			return nil
		},
	)
	if err == nil {
		t.Fatal("rooted status commit unexpectedly succeeded")
	}
	if recorded {
		t.Fatal("rooted events were recorded after status commit failed")
	}
	if tip, ok := cache.TipSlot(); !ok || tip != 10 {
		t.Fatalf("status tip = %d (present=%t), want parent 10", tip, ok)
	}
}

func TestCommitRootedStatusAndEventsUnwindsRejectedEvents(t *testing.T) {
	cache := NewTransactionStatusCache()
	if err := cache.CommitBlock(statusCacheTestBlock(10)); err != nil {
		t.Fatal(err)
	}
	candidateTx := statusCacheTestTransaction(9, 8, 7)
	candidate := statusCacheTestBlock(11, candidateTx)
	recordErr := errors.New("rooted event limit")

	err := commitRootedStatusAndEvents(
		cache,
		candidate,
		func() error { return cache.CommitBlock(candidate) },
		func() error { return recordErr },
	)
	if !errors.Is(err, recordErr) {
		t.Fatalf("commit error = %v, want rooted event error", err)
	}
	if tip, ok := cache.TipSlot(); !ok || tip != 10 {
		t.Fatalf("status tip = %d (present=%t), want restored parent 10", tip, ok)
	}
	visible, visibleErr := cache.View().ContainsTransaction(candidateTx)
	if visibleErr != nil {
		t.Fatal(visibleErr)
	}
	if visible {
		t.Fatal("transaction from rejected rooted-event slot remains visible")
	}
}

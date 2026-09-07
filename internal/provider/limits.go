package provider

import (
	"fmt"
)

const (
	MaxRepositoryPathBytes       = 1 << 20
	maxJSONResponseSize          = 16 << 20
	maxPageItems                 = 100
	maxRefs                      = 100_000
	maxRefMetadataBytes    int64 = 32 << 20
	maxTreeItems                 = 1_000_000
	maxTreeMetadataBytes   int64 = 128 << 20
	maxPaginationRequests        = 1_000
	maxIndexedPages              = 10
	maxIndexedCandidates         = maxIndexedPages * maxPageItems
	maxCandidatePathBytes  int64 = 8 << 20
)

// ResourceLimitError marks provider input rejected before it can grow without bound.
type ResourceLimitError struct {
	Resource string
	Limit    int64
}

func (e *ResourceLimitError) Error() string {
	return fmt.Sprintf("%s exceeds resource limit of %d", e.Resource, e.Limit)
}

type collectionBudget struct {
	resource string
	items    int
	bytes    int64
	maxItems int
	maxBytes int64
}

type paginationBudget struct {
	resource string
	used     int
	max      int
}

func newCollectionBudget(resource string, maxItems int, maxBytes int64) *collectionBudget {
	return &collectionBudget{resource: resource, maxItems: maxItems, maxBytes: maxBytes}
}

func newPaginationBudget(resource string) *paginationBudget {
	return &paginationBudget{resource: resource, max: maxPaginationRequests}
}

func (b *paginationBudget) take() error {
	if b.used >= b.max {
		return &ResourceLimitError{Resource: b.resource + " pagination requests", Limit: int64(b.max)}
	}
	b.used++
	return nil
}

func (b *collectionBudget) add(values ...string) error {
	if b.items >= b.maxItems {
		return &ResourceLimitError{Resource: b.resource + " item count", Limit: int64(b.maxItems)}
	}
	var added int64
	for _, value := range values {
		if len(value) > MaxRepositoryPathBytes {
			return &ResourceLimitError{Resource: b.resource + " item value bytes", Limit: MaxRepositoryPathBytes}
		}
		added += int64(len(value))
	}
	if added > b.maxBytes-b.bytes {
		return &ResourceLimitError{Resource: b.resource + " metadata bytes", Limit: b.maxBytes}
	}
	b.items++
	b.bytes += added
	return nil
}

func enforcePageSize(resource string, items int) error {
	if items > maxPageItems {
		return &ResourceLimitError{Resource: resource + " page item count", Limit: maxPageItems}
	}
	return nil
}

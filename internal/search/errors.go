package search

import (
	"context"
	"errors"
	"fmt"

	"github.com/SamuelSupe/git-rg/internal/provider"
)

func IsContextError(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func incompleteReason(err error, fallback string) string {
	var budgetErr *provider.RequestBudgetError
	if errors.As(err, &budgetErr) {
		return "request_budget"
	}
	var httpErr *provider.HTTPError
	if errors.As(err, &httpErr) && httpErr.RateLimited {
		return "rate_limit"
	}
	if IsContextError(err) {
		return "cancelled"
	}
	if IsResourceLimit(err) {
		return "resource_limit"
	}
	return fallback
}

func IsResourceLimit(err error) bool {
	var providerLimit *provider.ResourceLimitError
	if errors.As(err, &providerLimit) {
		return true
	}
	return errors.Is(err, errLineTooLong) ||
		errors.Is(err, errContextWindowTooLarge) ||
		errors.Is(err, errSubmatchLimit) ||
		errors.Is(err, errFileTooLarge) ||
		errors.Is(err, errResultSpoolLimit) ||
		errors.Is(err, errArchiveCompressedLimit) ||
		errors.Is(err, errArchiveExpandedLimit) ||
		errors.Is(err, errArchiveMetadataLimit) ||
		errors.Is(err, errRepositoryPathLimit)
}

func ErrorCode(err error) string {
	if IsContextError(err) {
		return "search_cancelled"
	}
	var budgetErr *provider.RequestBudgetError
	if errors.As(err, &budgetErr) {
		return "request_budget_exceeded"
	}
	var httpErr *provider.HTTPError
	if errors.As(err, &httpErr) && httpErr.RateLimited {
		return "rate_limited"
	}
	if IsResourceLimit(err) {
		return "resource_limit"
	}
	var searchErr *Error
	if errors.As(err, &searchErr) {
		return searchErr.Code
	}
	return "search_failed"
}

func ErrorMessage(err error) string {
	var searchErr *Error
	if errors.As(err, &searchErr) {
		return searchErr.Err.Error()
	}
	return fmt.Sprint(err)
}
